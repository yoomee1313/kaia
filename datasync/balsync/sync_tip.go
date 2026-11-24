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
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus"
	"github.com/kaiachain/kaia/rlp"
)

// A node applying BALs follows the head by applying the BAL of a new block
// only when its header commits the BAL (from Amber, see trustHeader): the
// fetcher hands it such blocks, which are inserted with the receipts and BAL
// propagated after them, or fetched from a peer. A block whose header does
// not commit its BAL, or whose BAL cannot be had, is executed.

var (
	tipBALWait      = 3 * time.Second // how long a block waits for its BAL before it is executed
	tipFetchTimeout = 2 * time.Second
)

type tipBlock struct {
	block   *types.Block
	addedAt time.Time
}

// TipChain is the chain a node applying BALs inserts blocks into.
type TipChain interface {
	CurrentBlock() *types.Block
	InsertChain(blocks types.Blocks) (int, error)
	InsertChainWithBALs(blocks types.Blocks, receipts []types.Receipts) (int, error)
	InsertChainContext(ctx context.Context, blocks types.Blocks, receipts []types.Receipts) (int, error)
}

// ChainOf returns chain as one that inserts blocks with their BALs.
func ChainOf(chain interface{}) (TipChain, bool) {
	c, ok := chain.(TipChain)
	return c, ok
}

// blocksRequest asks for the blocks From to To with their receipts and BALs.
type blocksRequest struct {
	ID, From, To uint64
}

type rangeBlock struct {
	Header   *types.Header
	Body     rlp.RawValue
	Receipts []*slimReceipt
	BAL      []byte
}

// blocksResponse holds the requested blocks in order, up to the first one the
// node does not hold whole, or the response size limit.
type blocksResponse struct {
	ID     uint64
	Blocks []*rangeBlock
}

// maxRangeBlocks is how many blocks a request asks for.
const maxRangeBlocks = 1024

func (s *Sync) serveBlocks(req *blocksRequest) *blocksResponse {
	res := &blocksResponse{ID: req.ID}
	size := 0
	for n := max(req.From, 1); n <= req.To && n < max(req.From, 1)+maxRangeBlocks && size < softResponseLimit; n++ {
		hash := s.db.ReadCanonicalHash(n)
		header := s.db.ReadHeader(hash, n)
		body := s.db.ReadBodyRLP(hash, n)
		list := s.db.ReadBAL(hash, n)
		receipts := s.db.ReadReceipts(hash, n)
		if header == nil || body == nil || list == nil || (receipts == nil && header.ReceiptHash != types.GetEmptyRootHash(header.Number)) {
			break
		}
		res.Blocks = append(res.Blocks, &rangeBlock{Header: header, Body: body, Receipts: slimReceipts(receipts), BAL: list})
		size += len(body) + len(list) + 600
		for _, r := range receipts {
			size += 16 + len(r.Logs)*128
		}
	}
	return res
}

func (r *HistoryRange) holdsBlocks(from, to uint64) bool {
	return r.Header <= from && r.Body <= from && r.Receipt <= from && r.holdsBALs(from, to)
}

// checkBlock checks a fetched block against its header and returns its
// receipts.
func checkBlock(n uint64, b *rangeBlock) (types.Receipts, error) {
	if b.Header == nil || b.Header.Number == nil || b.Header.Number.Uint64() != n {
		return nil, fmt.Errorf("%w: not block %d", errBALSyncMismatch, n)
	}
	var body types.Body
	if err := rlp.DecodeBytes(b.Body, &body); err != nil {
		return nil, fmt.Errorf("%w: body of block %d: %v", errBALSyncMismatch, n, err)
	}
	if root := types.DeriveTransactionsRoot(types.Transactions(body.Transactions), b.Header.Number); root != b.Header.TxHash {
		return nil, fmt.Errorf("%w: block %d transactions root", errBALSyncMismatch, n)
	}
	return fullReceipts(b.Header, b.Receipts)
}

type tipState struct {
	mu      sync.Mutex
	blocks  map[common.Hash]*tipBlock
	signal  chan struct{}
	chain   TipChain
	onWrite func(block *types.Block) // announces an inserted block

	// trustHeader reports whether a header alone commits the BAL of its
	// block. From Amber the header hash of the BAL and the committed seals do.
	trustHeader func(header *types.Header) bool
}

// StartTip makes the node follow the head by applying the BALs of the blocks
// whose headers trustHeader trusts. Without trustHeader the fetcher executes
// the new blocks.
func (s *Sync) StartTip(chain TipChain, onWrite func(*types.Block), trustHeader func(*types.Header) bool) {
	if trustHeader == nil {
		return
	}
	s.tip = &tipState{blocks: make(map[common.Hash]*tipBlock), signal: make(chan struct{}, 1), chain: chain, onWrite: onWrite, trustHeader: trustHeader}
	go s.tipLoop()
}

// HasTip reports whether the node follows the head by applying BALs, see
// StartTip.
func (s *Sync) HasTip() bool { return s != nil && s.tip != nil }

func (s *Sync) signalTip() {
	if s.tip == nil {
		return
	}
	select {
	case s.tip.signal <- struct{}{}:
	default:
	}
}

// InsertTipBlocks takes the blocks the fetcher imports: the blocks whose
// headers commit their BALs, and the blocks after them, are inserted by
// applyTip; the blocks before are executed.
func (s *Sync) InsertTipBlocks(blocks types.Blocks) (int, error) {
	t := s.tip
	i := 0
	for ; i < len(blocks); i++ {
		if t.trustHeader(blocks[i].Header()) || s.TipBlock(blocks[i].ParentHash()) != nil {
			break
		}
	}
	if i > 0 {
		if n, err := t.chain.InsertChain(blocks[:i]); err != nil {
			return n, err
		}
	}
	if i == len(blocks) {
		return 0, nil
	}
	t.mu.Lock()
	for _, b := range blocks[i:] {
		if _, ok := t.blocks[b.Hash()]; !ok {
			t.blocks[b.Hash()] = &tipBlock{block: b, addedAt: time.Now()}
		}
	}
	t.mu.Unlock()
	s.signalTip()
	return 0, nil
}

// TipBlock returns a block the node took and did not insert yet.
func (s *Sync) TipBlock(hash common.Hash) *types.Block {
	t := s.tip
	t.mu.Lock()
	defer t.mu.Unlock()
	if b := t.blocks[hash]; b != nil {
		return b.block
	}
	return nil
}

// TipHeight returns the height of the chain with the blocks taken on top of
// its head, which the fetcher imports the next block after.
func (s *Sync) TipHeight() uint64 {
	t := s.tip
	head := t.chain.CurrentBlock()
	n, hash := head.NumberU64(), head.Hash()
	t.mu.Lock()
	defer t.mu.Unlock()
	for found := true; found; {
		found = false
		for _, b := range t.blocks {
			if b.block.ParentHash() == hash {
				n, hash, found = b.block.NumberU64(), b.block.Hash(), true
				break
			}
		}
	}
	return n
}

// VerifyTipHeader defers the check of a header whose parent is not inserted
// yet to its insertion, without relaying the block.
func (s *Sync) VerifyTipHeader(header *types.Header, verify func(*types.Header) error) error {
	if s.TipBlock(header.ParentHash) != nil {
		return consensus.ErrFutureBlock
	}
	return verify(header)
}

func (s *Sync) tipLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-s.tip.signal:
		case <-ticker.C:
		}
		s.applyTip()
	}
}

// readyTip returns a copy of the block on the head, or nil, and forgets the
// blocks already inserted.
func (s *Sync) readyTip() *tipBlock {
	t := s.tip
	head := t.chain.CurrentBlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	for hash, b := range t.blocks {
		if b.block.NumberU64() <= head.NumberU64() {
			delete(t.blocks, hash)
		}
	}
	for _, b := range t.blocks {
		if b.block.ParentHash() == head.Hash() {
			cpy := *b
			return &cpy
		}
	}
	return nil
}

// applyTip inserts the blocks on the head that are ready.
func (s *Sync) applyTip() {
	t := s.tip
	for {
		b := s.readyTip()
		if b == nil {
			return
		}
		block := b.block
		trusted := t.trustHeader(block.Header())
		var (
			insert   *types.Block
			receipts types.Receipts
			ok       bool
		)
		if trusted {
			insert, receipts, ok = s.tipData(block)
		}
		if !ok {
			if trusted {
				if time.Since(b.addedAt) < tipBALWait {
					return
				}
				syncLogger.Warn("No block access list for a new block, executing it", "number", block.NumberU64(), "hash", block.Hash())
			}
			if _, err := t.chain.InsertChain(types.Blocks{block}); err != nil {
				syncLogger.Warn("Failed to insert a new block", "number", block.NumberU64(), "err", err)
				s.forgetTip(block.Hash())
				return
			}
		} else if _, err := t.chain.InsertChainWithBALs(types.Blocks{insert}, []types.Receipts{receipts}); err != nil {
			syncLogger.Warn("Failed to insert a new block with its block access list, executing it", "number", block.NumberU64(), "err", err)
			s.received.Remove(block.Hash())
			if _, err := t.chain.InsertChain(types.Blocks{block}); err != nil {
				s.forgetTip(block.Hash())
				return
			}
		}
		s.forgetTip(block.Hash())
		if t.onWrite != nil {
			t.onWrite(block)
		}
	}
}

func (s *Sync) forgetTip(hash common.Hash) {
	s.tip.mu.Lock()
	delete(s.tip.blocks, hash)
	s.tip.mu.Unlock()
}

// tipData returns block with the BAL propagated after it, or fetched from a
// peer holding it, and its receipts.
func (s *Sync) tipData(block *types.Block) (*types.Block, types.Receipts, bool) {
	if list, slim, ok := s.PropagatedBAL(block.Hash()); ok {
		if receipts, err := fullReceipts(block.Header(), slim); err == nil {
			return block.WithBAL(list), receipts, true
		}
		s.received.Remove(block.Hash())
	}
	n := block.NumberU64()
	ctx, cancel := context.WithTimeout(context.Background(), tipFetchTimeout)
	defer cancel()
	p := s.pick(func(r *HistoryRange) bool { return r.holdsBlocks(n, n) }, nil)
	if p == nil {
		return nil, nil, false
	}
	defer s.release(p)
	res, err := s.request(ctx, p, BlocksRequestMsg, func(id uint64) interface{} { return &blocksRequest{ID: id, From: n, To: n} })
	if err != nil {
		return nil, nil, false
	}
	blocks := res.(*blocksResponse).Blocks
	if len(blocks) != 1 || blocks[0].Header == nil || blocks[0].Header.Hash() != block.Hash() {
		return nil, nil, false
	}
	receipts, err := checkBlock(n, blocks[0])
	if err != nil {
		return nil, nil, false
	}
	return block.WithBAL(blocks[0].BAL), receipts, true
}
