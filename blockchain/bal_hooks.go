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
	"time"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
)

// BALHooks is how the chain calls the code of block access lists, which
// package bal registers with RegisterBALHooks.
type BALHooks interface {
	// Inserted reports whether a block is inserted with its block access
	// list instead of being executed.
	Inserted(hash common.Hash) bool
	// Executing is called with the parent state of a block before the
	// block is executed on it.
	Executing(statedb *state.StateDB)
	// ApplyBlock returns the receipts, logs and gas of a block inserted with
	// its list, and applies the list to statedb. With check the list is
	// applied without hashing, after check accepts the receipts.
	ApplyBlock(block *types.Block, statedb *state.StateDB, check func(types.Receipts, uint64) error) (types.Receipts, []*types.Log, uint64, error)
	// Record is called with the state of a validated block before it is
	// written.
	Record(block *types.Block, parentRoot common.Hash, statedb *state.StateDB)
	// StateAt opens the state with the given root of a past block, and
	// reports whether it handled the lookup.
	StateAt(root common.Hash) (*state.StateDB, bool, error)
	// PruneTrieNodes deletes the trie nodes of the marks of blocks up to
	// limit.
	PruneTrieNodes(marks []database.PruningMark, limit uint64)
	// Truncate is called for every block deleted by a rewind.
	Truncate(number uint64)
	// Stop is called once the chain stopped.
	Stop()
}

// BALContext is what the chain hands to its BAL hooks besides itself.
type BALContext struct {
	DB          database.DBManager
	CacheConfig *CacheConfig
	VMConfig    vm.Config
}

var newBALHooks func(bc *BlockChain, ctx BALContext) (BALHooks, error)

// RegisterBALHooks sets how every new chain makes its BAL hooks.
func RegisterBALHooks(f func(bc *BlockChain, ctx BALContext) (BALHooks, error)) {
	newBALHooks = f
}

// BALHooks returns the BAL hooks of the chain, nil if package bal is not
// linked.
func (bc *BlockChain) BALHooks() BALHooks { return bc.balHooks }

func (bc *BlockChain) attachBALHooks() error {
	if newBALHooks == nil {
		if c := bc.cacheConfig; c.BALRecord || c.HistoryRetention > 0 || c.HeaderSegmentRoots {
			return errors.New("block access lists need package blockchain/bal")
		}
		return nil
	}
	h, err := newBALHooks(bc, BALContext{DB: bc.db, CacheConfig: bc.cacheConfig, VMConfig: bc.vmConfig})
	if err != nil {
		return err
	}
	bc.balHooks = h
	return nil
}

// processBlock executes a block on its parent state, or applies its block
// access list if it is inserted with it. The list of a block in a batch is
// applied after its receipts are checked, without hashing.
func (bc *BlockChain) processBlock(block *types.Block, statedb *state.StateDB, batch *balBatch) (types.Receipts, []*types.Log, uint64, []*vm.InternalTxTrace, ProcessStats, error) {
	h := bc.balHooks
	if h == nil || !h.Inserted(block.Hash()) {
		if h != nil {
			h.Executing(statedb)
		}
		return bc.processor.Process(block, statedb, bc.vmConfig)
	}
	stats := ProcessStats{BeforeApplyTxs: time.Now()}
	var check func(types.Receipts, uint64) error
	if batch != nil {
		check = func(receipts types.Receipts, usedGas uint64) error {
			if err := validateReceipts(block, receipts, usedGas); err != nil {
				return err
			}
			batch.dirty = true
			return nil
		}
	}
	receipts, logs, usedGas, err := h.ApplyBlock(block, statedb, check)
	if err != nil {
		return nil, nil, 0, nil, stats, err
	}
	stats.AfterApplyTxs, stats.AfterFinalize = time.Now(), time.Now()
	return receipts, logs, usedGas, nil, stats, nil
}

// StartBALRecording turns the block access list recorder on for a block this
// node builds, on the parent state before the block is executed, as
// processBlock does for inserted blocks. WriteBlockWithState then stores the
// list.
func (bc *BlockChain) StartBALRecording(statedb *state.StateDB) {
	if bc.balHooks != nil {
		bc.balHooks.Executing(statedb)
	}
}

func (bc *BlockChain) recordBAL(block *types.Block, parentRoot common.Hash, statedb *state.StateDB) {
	if bc.balHooks != nil {
		bc.balHooks.Record(block, parentRoot, statedb)
	}
}

// balStateAt opens the state with the given root from a batch or the BAL
// history, and reports whether it did.
func (bc *BlockChain) balStateAt(root common.Hash) (*state.StateDB, bool, error) {
	if b := bc.balBatch.Load(); b != nil {
		if sdb := b.stateAt(root); sdb != nil {
			return sdb, true, nil
		}
	}
	if bc.balHooks != nil && root != bc.CurrentBlock().Root() {
		return bc.balHooks.StateAt(root)
	}
	return nil, false, nil
}

// pruneTrieNodes deletes the trie nodes of marks of blocks up to limit, but
// those the BAL hooks keep.
func (bc *BlockChain) pruneTrieNodes(marks []database.PruningMark, limit uint64) {
	if bc.balHooks != nil {
		bc.balHooks.PruneTrieNodes(marks, limit)
		return
	}
	bc.db.PruneTrieNodes(marks)
}
