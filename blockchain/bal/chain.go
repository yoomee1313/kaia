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

package bal

import (
	"sync"
	"sync/atomic"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
)

func init() {
	blockchain.RegisterBALHooks(newChain)
}

// chain keeps the block access lists of a BlockChain, see
// blockchain.BALHooks: it records them, serves past states from them and
// regenerates lists.
type chain struct {
	bc       *blockchain.BlockChain
	db       database.DBManager
	cfg      *blockchain.CacheConfig
	vmConfig vm.Config
	quit     chan struct{}
	wg       sync.WaitGroup

	rec     *recorder     // nil unless recording
	archive *chainArchive // serves past states from the lists, nil unless indexed

	genesis atomic.Pointer[blockchain.Genesis]
	regenMu sync.Mutex
	regen   *regen // last regeneration, nil if none
}

func newChain(bc *blockchain.BlockChain, ctx blockchain.BALContext) (blockchain.BALHooks, error) {
	c := &chain{bc: bc, db: ctx.DB, cfg: ctx.CacheConfig, vmConfig: ctx.VMConfig, quit: make(chan struct{})}
	if c.cfg.BALRecord {
		c.rec = newRecorder(c.db, c.cfg.BALSkipReads)
		if err := c.openArchive(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// of returns the block access lists of a chain.
func of(bc *blockchain.BlockChain) *chain {
	c, _ := bc.BALHooks().(*chain)
	return c
}

func (c *chain) Executing(statedb *state.StateDB) {
	if c.rec != nil {
		access.EnableRecorder(statedb)
	}
}

// Record stores the list recorded while a block was executed.
func (c *chain) Record(block *types.Block, parentRoot common.Hash, statedb *state.StateDB) {
	if c.rec != nil {
		c.rec.record(c, block, parentRoot, statedb)
	}
}

func (c *chain) StateAt(root common.Hash) (*state.StateDB, bool, error) {
	if c.archive == nil {
		return nil, false, nil
	}
	sdb, err := c.stateFromBAL(root, c.bc.StateCache(), c.bc.Snapshots())
	return sdb, true, err
}

func (c *chain) Truncate(number uint64) {
	if c.rec != nil {
		c.rec.truncate(number)
	}
	if c.archive != nil {
		c.archive.Truncate(number)
	}
}

func (c *chain) Stop() {
	close(c.quit)
	c.wg.Wait()
	if c.rec != nil {
		c.rec.close(c.bc.CurrentBlock().NumberU64())
	}
}
