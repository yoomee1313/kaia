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

package balsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/hashchain"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/networks/p2p"
	"github.com/kaiachain/kaia/storage/database"
)

// kaia/69 exchanges block access lists (BALs) and slim receipts. A node
// announces the oldest block whose header, body, receipts and BAL it holds in
// the handshake, and again while connected whenever one of them changes or its
// BALs grew by balAnnounceStep blocks. Data is asked only of peers whose
// announced range of its kind holds it.
// The downloader fetches the BALs of old blocks by their hashes, with their
// receipts, see Applies; before they are applied, each is checked against the
// hashes of the BALs of its segment of the BAL hash chain, which are fetched
// from a peer and checked against the chain once per segment. A peer whose
// data does not match is dropped.
// New blocks are followed by their BAL when they are propagated.

// SyncVersion is the first protocol version, kaia/69, that exchanges BALs.
const SyncVersion = 69

// Protocol messages belonging to kaia/69
const (
	HistoryRangeMsg         = 0x19
	BALsRequestMsg          = 0x1a
	BALsMsg                 = 0x1b
	SegmentHashesRequestMsg = 0x1c
	SegmentHashesMsg        = 0x1d
	NewBlockBALMsg          = 0x1e
	BlocksRequestMsg        = 0x1f
	BlocksMsg               = 0x20
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

// announces reports whether a node holding r announces it to a peer it last
// announced sent to.
func (r HistoryRange) announces(sent HistoryRange) bool {
	return r.Header != sent.Header || r.Body != sent.Body || r.Receipt != sent.Receipt || r.BAL != sent.BAL ||
		r.BALNext < sent.BALNext || r.BALNext >= sent.BALNext+balAnnounceStep
}

// A BALsRequestMsg asks for the BALs of the blocks with a list of hashes. Its
// BALsMsg holds them in order, as stored: the RLP encoding of their writes. It
// stops at the first one the node does not hold, or at the response size
// limit.

// maxBALsPerRequest is how many BALs a request may ask for.
const maxBALsPerRequest = 256

// segmentHashesRequest asks for the hashes of the BALs of the blocks of a
// segment in a BAL hash chain whose last block is Last.
type segmentHashesRequest struct {
	ID, Segment, Last uint64
}

// segmentHashesResponse holds the hashes of the BALs of the blocks of the
// segment in order, or none if the node does not hold them all.
type segmentHashesResponse struct {
	ID     uint64
	Hashes []common.Hash
}

// slimReceipt is a receipt without its bloom, which is derived from its logs.
type slimReceipt struct {
	Status  uint
	GasUsed uint64
	Logs    []*types.Log
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
	verified  *lru.Cache // segment -> hashes of its BALs, checked against hashChain

	applyBALs   bool             // sync by applying BALs, see Applies
	tip         *tipState        // blocks at the head to apply, nil unless headers commit BALs
	hashChain   *hashchain.Chain // signed chain that checks fetched BALs, nil if none
	quit        <-chan struct{}
	deliverBALs func(id string, lists [][]byte) error // hands fetched BALs to the downloader
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
	verified, _ := lru.New(4)
	return &Sync{db: db, chain: chain, drop: drop, peers: make(map[string]*balPeer), received: received, segHashes: segHashes, verified: verified}
}

// errNoHashChain tells how to give a node applying BALs the hash chain.
var errNoHashChain = fmt.Errorf("pre-Amber block access lists are applied only once checked against the BAL hash chain signed by the Kaia Foundation: "+
	"install it with `npm install %s` and set --bal.hash-chain to the directory it was installed in, or set --sync.execute to execute the blocks", state.HashChainPackage)

// Configure sets how the node syncs: apply tells whether it applies BALs,
// which needs the signed hash chain of the network of the genesis block, in
// the file or package directory hashChain.
func (s *Sync) Configure(hashChain string, apply bool, genesis func() common.Hash, quit <-chan struct{}) error {
	s.quit = quit
	s.applyBALs = apply
	if hashChain == "" {
		if apply {
			return errNoHashChain
		}
		return nil
	}
	chain, err := state.BALHashChain.LoadSigned(hashChain, genesis(), state.HashChainPublicKey)
	if err != nil {
		return fmt.Errorf("BAL hash chain %s: %w; %w", hashChain, err, errNoHashChain)
	}
	syncLogger.Info("Loaded the signed BAL hash chain", "last", chain.Last, "root", chain.Root())
	s.hashChain = chain
	return nil
}

// LocalRange returns the data this node holds: every header, body and
// receipt, and the stored lists.
func (s *Sync) LocalRange() HistoryRange {
	r := HistoryRange{BAL: NoHistory}
	if first, next := s.db.ReadBALRange(); first < next {
		r.BAL, r.BALNext = first, next
	}
	return r
}

// ApplyBALs reports whether the node syncs by applying BALs.
func (s *Sync) ApplyBALs() bool { return s.applyBALs }

// hashchain.Chain returns the signed chain that checks fetched BALs, nil if none.
func (s *Sync) HashChain() *hashchain.Chain { return s.hashChain }

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
		var hashes []common.Hash
		if err := msg.Decode(&hashes); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		if len(hashes) > maxBALsPerRequest {
			return msgError(errTooManyItems, "%d BALs requested", len(hashes))
		}
		return conn.Send(BALsMsg, s.serveBALs(hashes))
	case SegmentHashesRequestMsg:
		var req segmentHashesRequest
		if err := msg.Decode(&req); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		return conn.Send(SegmentHashesMsg, s.serveSegmentHashes(&req))
	case BALsMsg:
		var lists [][]byte
		if err := msg.Decode(&lists); err != nil {
			return msgError(errDecode, "%v: %v", msg, err)
		}
		if s.deliverBALs != nil {
			if err := s.deliverBALs(conn.GetID(), lists); err != nil {
				syncLogger.Debug("Failed to deliver block access lists", "peer", conn.GetID(), "err", err)
			}
		}
	case SegmentHashesMsg:
		var res segmentHashesResponse
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

// serveBALs returns the stored BALs of the blocks with hashes, up to the first
// one the node does not hold.
func (s *Sync) serveBALs(hashes []common.Hash) [][]byte {
	var lists [][]byte
	size := 0
	for _, hash := range hashes {
		header := s.chain.GetHeaderByHash(hash)
		if header == nil {
			break
		}
		enc := s.db.ReadBAL(hash, header.Number.Uint64())
		if enc == nil {
			break
		}
		lists = append(lists, enc)
		if size += len(enc); size >= softResponseLimit {
			break
		}
	}
	return lists
}

// localSegmentHashes returns the hashes of the stored BALs of segment k in a
// BAL hash chain whose last block is last, and the first block of the
// segment, or false if the node does not hold them all.
func (s *Sync) localSegmentHashes(k, last uint64) ([]common.Hash, uint64, bool) {
	from := k * hashchain.SegmentSize
	if from > last {
		return nil, 0, false
	}
	to := min(from+hashchain.SegmentSize-1, last)
	if rng := s.LocalRange(); !rng.holdsBALs(max(from, 1), to) {
		return nil, 0, false
	}
	key := [2]uint64{k, to}
	if v, ok := s.segHashes.Get(key); ok {
		return v.([]common.Hash), from, true
	}
	hashes := make([]common.Hash, 0, to-from+1)
	for n := from; n <= to; n++ {
		h, err := state.HashOf(s.db, n)
		if err != nil {
			return nil, 0, false
		}
		hashes = append(hashes, h)
	}
	s.segHashes.Add(key, hashes)
	return hashes, from, true
}

func (s *Sync) serveSegmentHashes(req *segmentHashesRequest) *segmentHashesResponse {
	res := &segmentHashesResponse{ID: req.ID}
	if hashes, _, ok := s.localSegmentHashes(req.Segment, req.Last); ok {
		res.Hashes = hashes
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

// BroadcastBAL sends the stored BAL and the receipts of an inserted block to
// the kaia/69 peers among peers that did not send or get them yet.
func BroadcastBAL[P Conn](s *Sync, block *types.Block, peers []P) {
	if s == nil {
		return
	}
	hash, n := block.Hash(), block.NumberU64()
	enc := s.db.ReadBAL(hash, n)
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
	if stored := s.db.ReadBAL(m.Hash, m.Number); stored != nil {
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
