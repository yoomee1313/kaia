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
	"time"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/kaiax"
	"github.com/kaiachain/kaia/storage/database"
)

// RegisterExecutionStateModule registers modules that watch the states blocks
// are executed on.
func (bc *BlockChain) RegisterExecutionStateModule(modules ...kaiax.ExecutionStateModule) {
	bc.executionStateModules = append(bc.executionStateModules, modules...)
}

// RegisterBlockApplyModule registers modules that insert blocks without
// executing them.
func (bc *BlockChain) RegisterBlockApplyModule(modules ...kaiax.BlockApplyModule) {
	bc.blockApplyModules = append(bc.blockApplyModules, modules...)
}

// RegisterHistoricalStateModule registers modules that serve the states of
// past blocks.
func (bc *BlockChain) RegisterHistoricalStateModule(modules ...kaiax.HistoricalStateModule) {
	bc.historicalStateModules = append(bc.historicalStateModules, modules...)
}

// RegisterTriePruningModule registers a module that deletes the trie nodes
// live pruning marked obsolete, instead of the chain.
func (bc *BlockChain) RegisterTriePruningModule(modules ...kaiax.TriePruningModule) {
	bc.triePruningModules = append(bc.triePruningModules, modules...)
}

// RegisterRewindableModule registers modules that erase their data of the
// blocks a rewind deletes.
func (bc *BlockChain) RegisterRewindableModule(modules ...kaiax.RewindableModule) {
	bc.rewindableModules = append(bc.rewindableModules, modules...)
}

// applyModule returns the module that applies the block with the given hash
// instead of executing it, or nil.
func (bc *BlockChain) applyModule(hash common.Hash) kaiax.BlockApplyModule {
	for _, module := range bc.blockApplyModules {
		if module.AppliesBlock(hash) {
			return module
		}
	}
	return nil
}

// processBlock executes a block on its parent state, or applies it if a
// module inserts it without executing it. A block of a batch is applied after
// its receipts are checked, without hashing.
func (bc *BlockChain) processBlock(block *types.Block, statedb *state.StateDB, batch *applyBatch) (types.Receipts, []*types.Log, uint64, []*vm.InternalTxTrace, ProcessStats, error) {
	module := bc.applyModule(block.Hash())
	if module == nil {
		bc.PreExecuteBlock(statedb)
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
	receipts, logs, usedGas, err := module.ApplyBlock(block, statedb, check)
	if err != nil {
		return nil, nil, 0, nil, stats, err
	}
	stats.AfterApplyTxs, stats.AfterFinalize = time.Now(), time.Now()
	return receipts, logs, usedGas, nil, stats, nil
}

// PreExecuteBlock hands the parent state of a block to the execution state
// modules before the block is executed on it: processBlock calls it for the
// blocks the chain inserts, and the worker for the blocks the node builds.
func (bc *BlockChain) PreExecuteBlock(statedb *state.StateDB) {
	for _, module := range bc.executionStateModules {
		module.PreExecuteBlock(statedb)
	}
}

// postExecuteBlock hands the state of a validated block to the execution state
// modules before the block is written.
func (bc *BlockChain) postExecuteBlock(block *types.Block, parentRoot common.Hash, statedb *state.StateDB) {
	for _, module := range bc.executionStateModules {
		module.PostExecuteBlock(block, parentRoot, statedb)
	}
}

// historicalStateAt opens the state with the given root from a batch or a
// historical state module, and reports whether it did.
func (bc *BlockChain) historicalStateAt(root common.Hash) (*state.StateDB, bool, error) {
	if b := bc.applyBatch.Load(); b != nil {
		if sdb := b.stateAt(root); sdb != nil {
			return sdb, true, nil
		}
	}
	if root == bc.CurrentBlock().Root() {
		return nil, false, nil
	}
	for _, module := range bc.historicalStateModules {
		if sdb, ok, err := module.StateAt(root); ok {
			return sdb, true, err
		}
	}
	return nil, false, nil
}

// pruneTrieNodes deletes the trie nodes of marks of blocks up to limit, through
// a trie pruning module if there is one.
func (bc *BlockChain) pruneTrieNodes(marks []database.PruningMark, limit uint64) {
	if len(bc.triePruningModules) > 0 {
		bc.triePruningModules[0].PruneTrieNodes(marks, limit)
		return
	}
	bc.db.PruneTrieNodes(marks)
}
