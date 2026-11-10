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

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
)

// BALHooks is how the chain calls the code of block access lists, which
// package bal registers with RegisterBALHooks.
type BALHooks interface {
	// Executing is called with the parent state of a block before the
	// block is executed on it.
	Executing(statedb *state.StateDB)
	// Record is called with the state of a validated block before it is
	// written.
	Record(block *types.Block, parentRoot common.Hash, statedb *state.StateDB)
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
		if bc.cacheConfig.BALRecord {
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

// processBlock executes a block on its parent state.
func (bc *BlockChain) processBlock(block *types.Block, statedb *state.StateDB) (types.Receipts, []*types.Log, uint64, []*vm.InternalTxTrace, ProcessStats, error) {
	if bc.balHooks != nil {
		bc.balHooks.Executing(statedb)
	}
	return bc.processor.Process(block, statedb, bc.vmConfig)
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
