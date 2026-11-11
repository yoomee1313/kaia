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
	"context"
	"sync"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus"
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

// TipChain is the chain a node applying BALs follows the head of.
type TipChain interface {
	RangeChain
	InsertChain(blocks types.Blocks) (int, error)
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
			insert *Block
			ok     bool
		)
		if trusted {
			insert, ok = s.tipData(block)
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
		} else if _, err := t.chain.InsertChainWithBALs([]*Block{insert}); err != nil {
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

// tipData returns a block with the receipts and BAL propagated after it, or
// fetched from a peer holding them.
func (s *Sync) tipData(block *types.Block) (*Block, bool) {
	if list, slim, ok := s.PropagatedBAL(block.Hash()); ok {
		if receipts, err := fullReceipts(block.Header(), slim); err == nil {
			return &Block{Block: block, Receipts: receipts, BAL: list}, true
		}
		s.received.Remove(block.Hash())
	}
	n := block.NumberU64()
	ctx, cancel := context.WithTimeout(context.Background(), tipFetchTimeout)
	defer cancel()
	p := s.pick(func(r *HistoryRange) bool { return r.holdsBlocks(n, n) }, nil)
	if p == nil {
		return nil, false
	}
	defer s.release(p)
	res, err := s.request(ctx, p, BlocksRequestMsg, func(id uint64) interface{} { return &blocksRequest{ID: id, From: n, To: n} })
	if err != nil {
		return nil, false
	}
	blocks := res.(*blocksResponse).Blocks
	if len(blocks) != 1 || blocks[0].Header == nil || blocks[0].Header.Hash() != block.Hash() {
		return nil, false
	}
	b, err := checkBlock(n, blocks[0])
	if err != nil {
		return nil, false
	}
	b.Block = block
	return b, true
}
