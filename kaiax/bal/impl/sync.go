// Copyright 2026 The Kaia Authors
// This file is part of the Kaia library.
//
// The Kaia library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The Kaia library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Kaia library. If not, see <http://www.gnu.org/licenses/>.

package impl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/kaiax/bal"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/networks/p2p"
	"github.com/kaiachain/kaia/storage/database"
)

// kaia/69 exchanges block access lists (BALs) and slim receipts. A node
// announces the oldest block whose header, body, receipts and BAL it holds in
// the handshake, and again while connected whenever one of them changes or its
// BALs grew by balAnnounceStep blocks. Data is asked only of peers whose
// announced range of its kind holds it.
// Old BALs are fetched by segments of the BAL hash chain, each one from one
// peer and checked against the chain on arrival; a single BAL comes with its
// Merkle proof. A peer whose data does not match is dropped. New blocks are
// followed by their BAL when they are propagated.

// SyncVersion is the first protocol version, kaia/69, that exchanges BALs.
const SyncVersion = 69

// Protocol messages belonging to kaia/69
const (
	HistoryRangeMsg        = 0x19
	BALsRequestMsg         = 0x1a
	BALsMsg                = 0x1b
	BALProofRequestMsg     = 0x1c
	BALProofMsg            = 0x1d
	SlimReceiptsRequestMsg = 0x1e
	SlimReceiptsMsg        = 0x1f
	NewBlockBALMsg         = 0x20
	BlocksRequestMsg       = 0x21
	BlocksMsg              = 0x22
)

// softResponseLimit is the target maximum size of a response.
const softResponseLimit = 2 * 1024 * 1024

var syncLogger = log.NewModuleLogger(log.NodeCN)

const (
	errDecode       = "Invalid message"
	errTooManyItems = "Too many items"
)

func msgError(kind string, format string, v ...interface{}) error {
	return fmt.Errorf("%v - %v", kind, fmt.Sprintf(format, v...))
}

// NoHistory is the oldest held block of data a node does not hold.
const NoHistory = math.MaxUint64

// HistoryRange is the data a node holds: the oldest block of each kind, and
// for BALs also the block after the last one.
type HistoryRange struct {
	Header  uint64 `json:"header"`
	Body    uint64 `json:"body"`
	Receipt uint64 `json:"receipt"`
	BAL     uint64 `json:"bal"`
	BALNext uint64 `json:"balNext"`
}

func (r *HistoryRange) holdsBALs(from, to uint64) bool {
	return r.BAL != NoHistory && r.BAL <= from && to < r.BALNext
}

// HoldsBlocksFrom reports whether the headers, bodies and receipts from block
// from on are held, which a light node deletes.
func (r *HistoryRange) HoldsBlocksFrom(from uint64) bool {
	return r.Header <= from && r.Body <= from && r.Receipt <= from
}

// announces reports whether a node holding r announces it to a peer it last
// announced sent to.
func (r HistoryRange) announces(sent HistoryRange) bool {
	return r.Header != sent.Header || r.Body != sent.Body || r.Receipt != sent.Receipt || r.BAL != sent.BAL ||
		r.BALNext < sent.BALNext || r.BALNext >= sent.BALNext+balAnnounceStep
}

// balsRequest asks for the BALs of blocks From to To, which are in one segment.
type balsRequest struct {
	ID, From, To uint64
}

// balsResponse holds the BALs of blocks from the requested one in order, as
// stored: the RLP encoding of their writes. It stops at the first one the node
// does not hold, or at the response size limit.
type balsResponse struct {
	ID    uint64
	Lists [][]byte
}

// balProofRequest asks for the BAL of block Number with its proof in a BAL
// hash chain whose last block is Last.
type balProofRequest struct {
	ID, Number, Last uint64
}

type balProofResponse struct {
	ID    uint64
	List  []byte // nil: not held
	Proof []common.Hash
}

type slimReceiptsRequest struct {
	ID     uint64
	Hashes []common.Hash
}

// slimReceipt is a receipt without its bloom, which is derived from its logs.
type slimReceipt struct {
	Status  uint
	GasUsed uint64
	Logs    []*types.Log
}

// slimReceiptsResponse holds the receipts of the requested blocks in order,
// up to the first one the node does not hold.
type slimReceiptsResponse struct {
	ID       uint64
	Receipts [][]*slimReceipt
}

// newBlockBAL follows a block a node inserted: its slim receipts and the
// stored writes of its BAL, which a node applying BALs inserts it with.
type newBlockBAL struct {
	Hash     common.Hash
	Number   uint64
	List     []byte
	Receipts []*slimReceipt
}

// IsSyncMsg reports whether a message code is one of the messages of Sync.
func IsSyncMsg(code uint64) bool {
	return code >= HistoryRangeMsg && code <= BlocksMsg
}

func slimReceipts(receipts types.Receipts) []*slimReceipt {
	out := make([]*slimReceipt, len(receipts))
	for i, r := range receipts {
		out[i] = &slimReceipt{Status: r.Status, GasUsed: r.GasUsed, Logs: r.Logs}
	}
	return out
}

// fullReceipts rebuilds the receipts of a block from slim ones and checks them
// against its header.
func fullReceipts(header *types.Header, slim []*slimReceipt) (types.Receipts, error) {
	receipts := make(types.Receipts, len(slim))
	for i, s := range slim {
		receipts[i] = &types.Receipt{Status: s.Status, GasUsed: s.GasUsed, Logs: s.Logs}
		receipts[i].Bloom = types.BytesToBloom(types.LogsBloom(s.Logs).Bytes())
	}
	if root := types.DeriveReceiptsRoot(receipts, header.Number); root != header.ReceiptHash {
		return nil, fmt.Errorf("%w: block %d receipts root %x, header has %x", errBALSyncMismatch, header.Number, root, header.ReceiptHash)
	}
	if bloom := types.CreateBloom(receipts); bloom != header.Bloom {
		return nil, fmt.Errorf("%w: block %d bloom", errBALSyncMismatch, header.Number)
	}
	return receipts, nil
}

var (
	errBALSyncMismatch = errors.New("data of peer does not match")
	errBALSyncNoPeer   = errors.New("no peer holds the data")
	errBALSyncTimeout  = errors.New("peer did not answer in time")
	errBALSyncGone     = errors.New("peer is gone")
)

var (
	balSyncTimeout  = 15 * time.Second
	balSyncNoPeer   = 2 * time.Minute // a fetch gives up after this long without a peer
	balSyncRetry    = 500 * time.Millisecond
	balAnnounceTick = 10 * time.Second
	balAnnounceStep = uint64(1024) // blocks the BALs grow by before they are announced again
)

// Conn is the part of a peer the BAL protocol uses.
type Conn interface {
	GetID() string
	GetVersion() int
	Send(msgcode uint64, data interface{}) error
}

type balPeer struct {
	conn    Conn
	rng     *HistoryRange // nil until announced
	sent    *HistoryRange // the range last announced to the peer
	busy    bool
	pending map[uint64]chan interface{}
	known   *lru.Cache // hashes of blocks whose BAL the peer has
}

// Sync serves and fetches BALs and slim receipts over kaia/69.
type Sync struct {
	db    database.DBManager
	chain SyncChain
	drop  func(id string) // disconnects a peer whose data does not match

	mu     sync.Mutex
	peers  map[string]*balPeer
	nextID uint64

	received  *lru.Cache // block hash -> BAL propagated with it
	segHashes *lru.Cache // [2]uint64{segment, last block} -> hashes of the stored BALs

	applyBALs    bool       // sync by applying BALs, see SyncRanges
	tip          *tipState  // blocks at the head to apply, nil unless headers commit BALs
	hashChain    *HashChain // signed chain that checks fetched BALs, nil if none
	quit         <-chan struct{}
	rangeRunning atomic.Bool
}

// SyncChain is the chain whose blocks a Sync serves.
type SyncChain interface {
	GetHeaderByHash(hash common.Hash) *types.Header
	GetReceiptsByBlockHash(hash common.Hash) types.Receipts
}

// NewSync returns a Sync of the BALs in db and the blocks of chain; drop
// disconnects a peer.
func NewSync(db database.DBManager, chain SyncChain, drop func(id string)) *Sync {
	received, _ := lru.New(128)
	segHashes, _ := lru.New(8)
	return &Sync{db: db, chain: chain, drop: drop, peers: make(map[string]*balPeer), received: received, segHashes: segHashes}
}

// LocalRange returns the data this node holds: a light node deletes the
// headers, bodies and receipts of old blocks.
func (s *Sync) LocalRange() HistoryRange {
	first := ReadHistoryFirst(s.db)
	r := HistoryRange{Header: first, Body: first, Receipt: first, BAL: NoHistory}
	if first, next := ReadBALRange(s.db); first < next {
		r.BAL, r.BALNext = first, next
	}
	return r
}

// ApplyBALs reports whether the node syncs by applying BALs.
func (s *Sync) ApplyBALs() bool { return s.applyBALs }

// HashChain returns the signed chain that checks fetched BALs, nil if none.
func (s *Sync) HashChain() *HashChain { return s.hashChain }

// RangeRunning reports whether Synchronise is applying blocks.
func (s *Sync) RangeRunning() bool { return s.rangeRunning.Load() }

// AddPeer adds a kaia/69 peer, which was sent the history range sent and
// sent rng in the handshake. A nil sent range is announced now.
func (s *Sync) AddPeer(conn Conn, sent, rng *HistoryRange) {
	if s == nil || conn.GetVersion() < SyncVersion {
		return
	}
	p := &balPeer{conn: conn, pending: make(map[uint64]chan interface{})}
	if rng != nil {
		cpy := *rng
		p.rng = &cpy
	}
	if sent != nil {
		cpy := *sent
		p.sent = &cpy
	}
	s.mu.Lock()
	s.peers[conn.GetID()] = p
	s.mu.Unlock()
	s.announce(s.LocalRange())
}

func (s *Sync) RemovePeer(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.peers[id]; p != nil {
		for _, ch := range p.pending {
			close(ch)
		}
		delete(s.peers, id)
	}
}

// AnnounceLoop announces the history range again when it changed.
func (s *Sync) AnnounceLoop(quit <-chan struct{}) {
	ticker := time.NewTicker(balAnnounceTick)
	defer ticker.Stop()
	for {
		select {
		case <-quit:
			return
		case <-ticker.C:
		}
		s.announce(s.LocalRange())
	}
}

// announce sends the history range rng to the peers it is news to.
func (s *Sync) announce(rng HistoryRange) {
	var conns []Conn
	s.mu.Lock()
	for _, p := range s.peers {
		if p.sent == nil || rng.announces(*p.sent) {
			cpy := rng
			p.sent = &cpy
			conns = append(conns, p.conn)
		}
	}
	s.mu.Unlock()
	for _, c := range conns {
		if err := c.Send(HistoryRangeMsg, &rng); err != nil {
			syncLogger.Debug("Failed to announce the history range", "peer", c.GetID(), "err", err)
		}
	}
}

func (s *Sync) HandleMsg(conn Conn, msg p2p.Msg) error {
	if s == nil {
		return nil
	}
	switch msg.Code {
	case HistoryRangeMsg:
		var rng HistoryRange
		if err := msg.Decode(&rng); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		s.mu.Lock()
		if p := s.peers[conn.GetID()]; p != nil {
			p.rng = &rng
		}
		s.mu.Unlock()
	case BALsRequestMsg:
		var req balsRequest
		if err := msg.Decode(&req); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		return conn.Send(BALsMsg, s.serveBALs(&req))
	case BALProofRequestMsg:
		var req balProofRequest
		if err := msg.Decode(&req); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		return conn.Send(BALProofMsg, s.serveProof(&req))
	case SlimReceiptsRequestMsg:
		var req slimReceiptsRequest
		if err := msg.Decode(&req); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		if len(req.Hashes) > maxReceiptsPerRequest {
			return msgError(errTooManyItems, "%d receipts requested", len(req.Hashes))
		}
		return conn.Send(SlimReceiptsMsg, s.serveReceipts(&req))
	case BALsMsg:
		var res balsResponse
		if err := msg.Decode(&res); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		s.deliver(conn, res.ID, &res)
	case BALProofMsg:
		var res balProofResponse
		if err := msg.Decode(&res); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		s.deliver(conn, res.ID, &res)
	case SlimReceiptsMsg:
		var res slimReceiptsResponse
		if err := msg.Decode(&res); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		s.deliver(conn, res.ID, &res)
	case BlocksRequestMsg:
		var req blocksRequest
		if err := msg.Decode(&req); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		return conn.Send(BlocksMsg, s.serveBlocks(&req))
	case BlocksMsg:
		var res blocksResponse
		if err := msg.Decode(&res); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		s.deliver(conn, res.ID, &res)
	case NewBlockBALMsg:
		var m newBlockBAL
		if err := msg.Decode(&m); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		s.receive(conn, &m)
	}
	return nil
}

var emptyBALWrites = func() []byte {
	enc, err := (&bal.BlockAccessList{}).Writes().Encode()
	if err != nil {
		panic(err)
	}
	return enc
}()

// rawBAL returns the stored writes of a canonical block, or nil.
func (s *Sync) rawBAL(n uint64) []byte {
	if n == 0 {
		return emptyBALWrites
	}
	enc, _ := ReadRaw(s.db, n)
	return enc
}

func (s *Sync) serveBALs(req *balsRequest) *balsResponse {
	res := &balsResponse{ID: req.ID}
	if req.To < req.From || Segment(req.From) != Segment(req.To) {
		return res
	}
	size := 0
	for n := req.From; n <= req.To && size < softResponseLimit; n++ {
		enc := s.rawBAL(n)
		if enc == nil {
			break
		}
		res.Lists = append(res.Lists, enc)
		size += len(enc)
	}
	return res
}

func (s *Sync) serveProof(req *balProofRequest) *balProofResponse {
	res := &balProofResponse{ID: req.ID}
	if req.Number > req.Last {
		return res
	}
	k := Segment(req.Number)
	from := k * SegmentSize
	to := min(from+SegmentSize-1, req.Last)
	if rng := s.LocalRange(); !rng.holdsBALs(max(from, 1), to) {
		return res
	}
	key := [2]uint64{k, to}
	var hashes []common.Hash
	if v, ok := s.segHashes.Get(key); ok {
		hashes = v.([]common.Hash)
	} else {
		for n := from; n <= to; n++ {
			h, err := HashOf(s.db, n)
			if err != nil {
				return res
			}
			hashes = append(hashes, h)
		}
		s.segHashes.Add(key, hashes)
	}
	proof, err := SegmentProof(hashes, int(req.Number-from))
	if err != nil {
		return res
	}
	res.List, res.Proof = s.rawBAL(req.Number), proof
	return res
}

func (s *Sync) serveReceipts(req *slimReceiptsRequest) *slimReceiptsResponse {
	res := &slimReceiptsResponse{ID: req.ID}
	size := 0
	for _, hash := range req.Hashes {
		receipts := s.chain.GetReceiptsByBlockHash(hash)
		if receipts == nil {
			if header := s.chain.GetHeaderByHash(hash); header == nil || header.ReceiptHash != types.GetEmptyRootHash(header.Number) {
				break
			}
		}
		res.Receipts = append(res.Receipts, slimReceipts(receipts))
		for _, r := range receipts {
			size += 16 + len(r.Logs)*128
		}
		if size >= softResponseLimit {
			break
		}
	}
	return res
}

func (s *Sync) deliver(conn Conn, id uint64, res interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.peers[conn.GetID()]; p != nil {
		if ch, ok := p.pending[id]; ok {
			delete(p.pending, id)
			ch <- res
		}
	}
}

// request sends a request to a peer and waits for its response.
func (s *Sync) request(ctx context.Context, p *balPeer, code uint64, build func(id uint64) interface{}) (interface{}, error) {
	ch := make(chan interface{}, 1)
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	p.pending[id] = ch
	s.mu.Unlock()
	cleanup := func() {
		s.mu.Lock()
		delete(p.pending, id)
		s.mu.Unlock()
	}
	if err := p.conn.Send(code, build(id)); err != nil {
		cleanup()
		return nil, err
	}
	timer := time.NewTimer(balSyncTimeout)
	defer timer.Stop()
	select {
	case res, ok := <-ch:
		if !ok {
			return nil, errBALSyncGone
		}
		return res, nil
	case <-timer.C:
		cleanup()
		return nil, errBALSyncTimeout
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	}
}

// pick takes an idle peer, not yet tried, whose history range holds what
// holds accepts.
func (s *Sync) pick(holds func(*HistoryRange) bool, tried map[string]bool) *balPeer {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.peers))
	for id := range s.peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if p := s.peers[id]; !p.busy && !tried[id] && p.rng != nil && holds(p.rng) {
			p.busy = true
			return p
		}
	}
	return nil
}

func (s *Sync) release(p *balPeer) {
	s.mu.Lock()
	p.busy = false
	s.mu.Unlock()
}

// fetch runs get on peers whose range holds what holds accepts until one
// succeeds. A peer that fails is not asked again; one whose data does not
// match is dropped.
func (s *Sync) fetch(ctx context.Context, what string, holds func(*HistoryRange) bool, get func(p *balPeer) error) error {
	tried := make(map[string]bool)
	since := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := s.pick(holds, tried)
		if p == nil {
			if time.Since(since) > balSyncNoPeer {
				return fmt.Errorf("%w: %s", errBALSyncNoPeer, what)
			}
			select {
			case <-ctx.Done():
			case <-time.After(balSyncRetry):
			}
			continue
		}
		id := p.conn.GetID()
		err := get(p)
		s.release(p)
		if err == nil {
			return nil
		}
		tried[id] = true
		if errors.Is(err, errBALSyncMismatch) {
			syncLogger.Warn("Dropping a peer whose data does not match", "peer", id, "data", what, "err", err)
			s.drop(id)
		} else {
			syncLogger.Debug("Failed to fetch from a peer", "peer", id, "data", what, "err", err)
		}
		since = time.Now()
	}
}

// FetchBALs returns the stored writes of the BALs of blocks from to to, in one
// segment, from one peer. With a chain they are the blocks of one of its
// segments and are checked against it.
func (s *Sync) FetchBALs(ctx context.Context, chain *HashChain, from, to uint64) ([][]byte, error) {
	k := Segment(from)
	if to < from || Segment(to) != k {
		return nil, fmt.Errorf("blocks %d-%d are not in one segment", from, to)
	}
	if chain != nil {
		if f, t := chain.SegmentBlocks(k); f != from || t != to {
			return nil, fmt.Errorf("blocks %d-%d are not segment %d of the BAL hash chain", from, to, k)
		}
	}
	var encs [][]byte
	err := s.fetch(ctx, fmt.Sprintf("BALs of blocks %d-%d", from, to),
		func(r *HistoryRange) bool { return r.holdsBALs(max(from, 1), to) },
		func(p *balPeer) error {
			encs = encs[:0]
			for next := from; next <= to; {
				res, err := s.request(ctx, p, BALsRequestMsg, func(id uint64) interface{} {
					return &balsRequest{ID: id, From: next, To: to}
				})
				if err != nil {
					return err
				}
				lists := res.(*balsResponse).Lists
				if len(lists) == 0 {
					return fmt.Errorf("%w: block %d", errBALSyncNoPeer, next)
				}
				if uint64(len(lists)) > to-next+1 {
					return fmt.Errorf("%w: %d BALs for blocks %d-%d", errBALSyncMismatch, len(lists), next, to)
				}
				encs = append(encs, lists...)
				next += uint64(len(lists))
			}
			if chain == nil {
				return nil
			}
			hashes := make([]common.Hash, len(encs))
			for i, enc := range encs {
				hashes[i] = crypto.Keccak256Hash(enc)
			}
			if err := chain.VerifySegment(k, hashes); err != nil {
				return fmt.Errorf("%w: %v", errBALSyncMismatch, err)
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return encs, nil
}

// FetchBAL returns the writes of the BAL of a block from a peer, checked
// against the chain with its proof.
func (s *Sync) FetchBAL(ctx context.Context, chain *HashChain, number uint64) (*bal.BlockAccessList, error) {
	if !chain.Covers(number) {
		return nil, fmt.Errorf("block %d is after the BAL hash chain, which ends at block %d", number, chain.Last)
	}
	from, to := chain.SegmentBlocks(Segment(number))
	var list *bal.BlockAccessList
	err := s.fetch(ctx, fmt.Sprintf("BAL of block %d", number),
		func(r *HistoryRange) bool { return r.holdsBALs(max(from, 1), to) },
		func(p *balPeer) error {
			res, err := s.request(ctx, p, BALProofRequestMsg, func(id uint64) interface{} {
				return &balProofRequest{ID: id, Number: number, Last: chain.Last}
			})
			if err != nil {
				return err
			}
			r := res.(*balProofResponse)
			if r.List == nil {
				return fmt.Errorf("%w: block %d", errBALSyncNoPeer, number)
			}
			if list, err = bal.Decode(r.List); err != nil {
				return fmt.Errorf("%w: %v", errBALSyncMismatch, err)
			}
			if err := chain.VerifyBlock(number, list, r.Proof); err != nil {
				return fmt.Errorf("%w: %v", errBALSyncMismatch, err)
			}
			return nil
		})
	return list, err
}

// maxReceiptsPerRequest is how many blocks a slim receipts request asks for.
const maxReceiptsPerRequest = 256

// FetchReceipts returns the receipts of blocks from peers, checked against
// their headers.
func (s *Sync) FetchReceipts(ctx context.Context, headers []*types.Header) ([]types.Receipts, error) {
	out := make([]types.Receipts, 0, len(headers))
	for len(out) < len(headers) {
		batch := headers[len(out):min(len(headers), len(out)+maxReceiptsPerRequest)]
		oldest := batch[0].Number.Uint64()
		for _, h := range batch {
			oldest = min(oldest, h.Number.Uint64())
		}
		var got []types.Receipts
		err := s.fetch(ctx, fmt.Sprintf("receipts of block %d", batch[0].Number),
			func(r *HistoryRange) bool { return r.Receipt <= oldest },
			func(p *balPeer) error {
				hashes := make([]common.Hash, len(batch))
				for i, h := range batch {
					hashes[i] = h.Hash()
				}
				res, err := s.request(ctx, p, SlimReceiptsRequestMsg, func(id uint64) interface{} {
					return &slimReceiptsRequest{ID: id, Hashes: hashes}
				})
				if err != nil {
					return err
				}
				slim := res.(*slimReceiptsResponse).Receipts
				if len(slim) == 0 {
					return fmt.Errorf("%w: block %d", errBALSyncNoPeer, batch[0].Number)
				}
				if len(slim) > len(batch) {
					return fmt.Errorf("%w: %d receipts for %d blocks", errBALSyncMismatch, len(slim), len(batch))
				}
				got = got[:0]
				for i, r := range slim {
					receipts, err := fullReceipts(batch[i], r)
					if err != nil {
						return err
					}
					got = append(got, receipts)
				}
				return nil
			})
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// BroadcastBAL sends the stored BAL and the receipts of an inserted block to
// the kaia/69 peers among peers that did not send or get them yet.
func BroadcastBAL[P Conn](s *Sync, block *types.Block, peers []P) {
	if s == nil {
		return
	}
	hash, n := block.Hash(), block.NumberU64()
	enc := ReadBAL(s.db, hash, n)
	receipts := s.db.ReadReceipts(hash, n)
	if enc == nil || (receipts == nil && block.Header().ReceiptHash != types.GetEmptyRootHash(block.Number())) {
		return
	}
	m := &newBlockBAL{Hash: hash, Number: n, List: enc, Receipts: slimReceipts(receipts)}
	for _, p := range peers {
		if p.GetVersion() < SyncVersion || !s.markKnown(p.GetID(), hash) {
			continue
		}
		if err := p.Send(NewBlockBALMsg, m); err != nil {
			syncLogger.Debug("Failed to send a block access list", "peer", p.GetID(), "err", err)
		}
	}
}

// markKnown records that a peer has the BAL of a block, and reports whether
// it did not before.
func (s *Sync) markKnown(id string, hash common.Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[id]
	if p == nil {
		return false
	}
	if p.known == nil {
		p.known, _ = lru.New(256)
	}
	if p.known.Contains(hash) {
		return false
	}
	p.known.Add(hash, struct{}{})
	return true
}

// propagated is a BAL and receipts received with a block.
type propagated struct {
	list     []byte
	receipts []*slimReceipt
}

// receive keeps a propagated BAL with its receipts, or compares it with the
// stored one of the block. The peer holds the BALs up to the block.
func (s *Sync) receive(conn Conn, m *newBlockBAL) {
	s.markKnown(conn.GetID(), m.Hash)
	s.mu.Lock()
	if p := s.peers[conn.GetID()]; p != nil && p.rng != nil && p.rng.BAL <= m.Number && p.rng.BALNext <= m.Number {
		p.rng.BALNext = m.Number + 1
	}
	s.mu.Unlock()
	if stored := ReadBAL(s.db, m.Hash, m.Number); stored != nil {
		if !bytes.Equal(stored, m.List) {
			syncLogger.Warn("Propagated block access list differs from the recorded one", "peer", conn.GetID(), "number", m.Number, "hash", m.Hash)
		}
		return
	}
	s.received.Add(m.Hash, &propagated{list: m.List, receipts: m.Receipts})
	s.signalTip()
}

// PropagatedBAL returns the BAL and receipts propagated with a block this node
// has not inserted. They are checked when the block is inserted with them.
func (s *Sync) PropagatedBAL(hash common.Hash) ([]byte, []*slimReceipt, bool) {
	if v, ok := s.received.Get(hash); ok {
		p := v.(*propagated)
		return p.list, p.receipts, true
	}
	return nil, nil, false
}

// PeerRange returns the announced history range of a kaia/69 peer.
func (s *Sync) PeerRange(id string) (HistoryRange, bool) {
	if s == nil {
		return HistoryRange{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.peers[id]; p != nil && p.rng != nil {
		return *p.rng, true
	}
	return HistoryRange{}, false
}

// PeersHighestBAL returns the last block whose BAL a peer holds, 0 if none.
func (s *Sync) PeersHighestBAL() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n uint64
	for _, p := range s.peers {
		if p.rng != nil && p.rng.BAL != NoHistory && p.rng.BALNext > 0 {
			n = max(n, p.rng.BALNext-1)
		}
	}
	return n
}

// PeerRanges returns the announced history ranges of the kaia/69 peers.
func (s *Sync) PeerRanges() map[string]HistoryRange {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]HistoryRange)
	for id, p := range s.peers {
		if p.rng != nil {
			out[id] = *p.rng
		}
	}
	return out
}
