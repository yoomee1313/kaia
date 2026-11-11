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

package blockchain

import (
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
)

// applyBatchFrozen is how many of the last unchecked states of a batch stay
// readable.
var applyBatchFrozen = 4

var errApplyBatchUnwritten = errors.New("the state holds the changes of an applied block that was not written")

// applyBatch applies the blocks one InsertChain call inserts without executing
// them onto one state, without hashing it per block, when their block apply
// module batches them. The state root is checked and the state committed at
// the blocks the trie is committed at, at the last block of the call, and
// before anything else is inserted; a wrong root rolls the chain back to the
// last checked block. The state of a block in between is read from a copy
// frozen after the block is applied, see StateAt, until the call returns.
type applyBatch struct {
	bc      *BlockChain
	base    *types.Block
	state   *state.StateDB
	pending []*types.Block
	dirty   bool
	commit  bool
	rewound bool

	mu     sync.Mutex
	frozen []frozenState
}

type frozenState struct {
	root  common.Hash
	state *state.StateDB
}

// newApplyBatch returns a batch for blocks that are all applied by modules
// that batch them, or nil.
func (bc *BlockChain) newApplyBatch(chain types.Blocks) *applyBatch {
	if bc.isArchiveMode() {
		return nil
	}
	for _, block := range chain {
		if module := bc.applyModule(block.Hash()); module == nil || !module.BatchesBlocks() {
			return nil
		}
	}
	return &applyBatch{bc: bc}
}

// stateFor returns the state to apply the child of parent on.
func (b *applyBatch) stateFor(parent *types.Block) (*state.StateDB, error) {
	if b.state != nil && !b.dirty && b.head().Hash() == parent.Hash() {
		return b.state, nil
	}
	if err := b.flush(); err != nil {
		return nil, err
	}
	sdb, err := b.bc.PrunableStateAt(parent.Root(), parent.NumberU64())
	if err != nil {
		return nil, err
	}
	b.base, b.state = parent, sdb
	return sdb, nil
}

func (b *applyBatch) head() *types.Block {
	if len(b.pending) > 0 {
		return b.pending[len(b.pending)-1]
	}
	return b.base
}

// validate checks the state of an applied block if it is committed, and
// otherwise freezes a copy of it to be read.
func (b *applyBatch) validate(block, parent *types.Block, receipts types.Receipts, usedGas uint64, last bool) error {
	if !last && !isCommitTrieRequired(b.bc, block.NumberU64()) {
		b.freeze(block.Root())
		return nil
	}
	b.commit = true
	b.state.HashStorages(runtime.NumCPU())
	if err := b.bc.validator.ValidateState(block, parent, b.state, receipts, usedGas); err != nil {
		return fmt.Errorf("applied blocks %d-%d: %w", b.base.NumberU64()+1, block.NumberU64(), err)
	}
	return b.state.Error()
}

// written records that an applied block is written.
func (b *applyBatch) written(block *types.Block) {
	b.dirty = false
	if b.commit {
		b.reset(block)
		return
	}
	b.pending = append(b.pending, block)
}

// defers reports whether the commit of state waits for a later block.
func (b *applyBatch) defers(state *state.StateDB) bool {
	return state == b.state && !b.commit
}

// flush checks and commits the state of the last written block.
func (b *applyBatch) flush() error {
	if b.dirty {
		return b.rollback(errApplyBatchUnwritten)
	}
	if len(b.pending) == 0 {
		return nil
	}
	last := b.pending[len(b.pending)-1]
	b.commit = true
	b.state.HashStorages(runtime.NumCPU())
	if root := b.state.IntermediateRoot(true); root != last.Root() {
		return b.rollback(fmt.Errorf("applied blocks %d-%d: invalid merkle root (remote: %x local: %x)",
			b.base.NumberU64()+1, last.NumberU64(), last.Root(), root))
	}
	if err := b.state.Error(); err != nil {
		return b.rollback(err)
	}
	if err := b.bc.writeStateTrie(last, b.state); err != nil {
		return b.rollback(err)
	}
	b.reset(last)
	return nil
}

func (b *applyBatch) reset(base *types.Block) {
	b.base, b.state, b.pending, b.dirty, b.commit = base, nil, nil, false, false
}

// rollback drops the states and rewinds the chain to the base if blocks after
// it were written.
func (b *applyBatch) rollback(err error) error {
	written := len(b.pending) > 0
	b.reset(b.base)
	b.mu.Lock()
	b.frozen = nil
	b.mu.Unlock()
	if !written {
		return err
	}
	b.rewound = true
	logger.Warn("Rolling back applied blocks", "to", b.base.NumberU64(), "err", err)
	if _, serr := b.bc.setHeadBeyondRootLocked(b.base.NumberU64(), common.Hash{}, false); serr != nil {
		return fmt.Errorf("%w (rolling back to block %d: %v)", err, b.base.NumberU64(), serr)
	}
	return err
}

// finish commits the state of the last written block, and drops the events
// of rolled back blocks.
func (b *applyBatch) finish(chain types.Blocks, n int, events []interface{}, logs []*types.Log, err error) (int, []interface{}, []*types.Log, error) {
	if ferr := b.flush(); err == nil {
		err = ferr
	}
	if !b.rewound {
		return n, events, logs, err
	}
	to := b.base.NumberU64()
	kept, logs := events[:0], logs[:0]
	for _, ev := range events {
		var block *types.Block
		switch ev := ev.(type) {
		case ChainEvent:
			block = ev.Block
		case ChainSideEvent:
			block = ev.Block
		case ChainHeadEvent:
			block = ev.Block
		}
		if block != nil && block.NumberU64() > to {
			continue
		}
		kept = append(kept, ev)
		if ev, ok := ev.(ChainEvent); ok {
			logs = append(logs, ev.Logs...)
		}
	}
	return int(to + 1 - chain[0].NumberU64()), kept, logs, err
}

func (b *applyBatch) freeze(root common.Hash) {
	frozen := b.state.Copy()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frozen = append(b.frozen, frozenState{root, frozen})
	if len(b.frozen) > applyBatchFrozen {
		b.frozen[0] = frozenState{}
		b.frozen = b.frozen[1:]
	}
}

// stateAt returns a copy of the frozen state with the given root, or nil.
func (b *applyBatch) stateAt(root common.Hash) *state.StateDB {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.frozen) - 1; i >= 0; i-- {
		if b.frozen[i].root == root {
			return b.frozen[i].state.Copy()
		}
	}
	return nil
}

func (b *applyBatch) has(root common.Hash) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, f := range b.frozen {
		if f.root == root {
			return true
		}
	}
	return false
}
