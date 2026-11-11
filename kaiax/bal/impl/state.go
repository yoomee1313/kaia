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
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/kaiax/bal"
	"github.com/kaiachain/kaia/snapshot"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// indexInterval is how often the BAL index catches up without a signal.
var indexInterval = 30 * time.Second

// chainArchive serves the state of past blocks from the stored lists, see
// BlockChain.StateAt.
type chainArchive struct {
	*Archive
	db     database.Database
	signal chan struct{}

	genesisMu sync.Mutex
	genesis   state.Database
}

// openArchive opens the BAL index next to the chain database and starts
// keeping it up to date.
func (c *BALModule) openArchive() error {
	var db database.Database
	if dir := c.db.GetDBConfig().Dir; dir != "" {
		ldb, err := database.NewLevelDBWithOption(filepath.Join(dir, "balindex"), &opt.Options{
			OpenFilesCacheCapacity: 256, BlockCacheCapacity: 64 * opt.MiB, WriteBuffer: 32 * opt.MiB,
		})
		if err != nil {
			return err
		}
		db = ldb
	} else {
		db = database.NewMemDB()
	}
	a := &chainArchive{db: db, signal: make(chan struct{}, 1)}
	a.Archive = NewArchive(c.db, db, c.baseState(a))
	c.archive = a
	return nil
}

// baseState returns the state the index starts from: for the genesis block
// its state rebuilt from the genesis of the chain, otherwise the stored state.
func (c *BALModule) baseState(a *chainArchive) BaseFunc {
	return func(number uint64, root common.Hash) (BaseState, error) {
		db := state.Database(c.bc.StateCache())
		if number == 0 {
			if g := a.genesisState(c); g != nil {
				db = g
			}
		}
		sdb, err := state.New(root, db, nil, nil)
		if err != nil {
			return nil, err
		}
		return bal.NewStateBase(sdb), nil
	}
}

// genesisState returns the genesis state rebuilt in memory, or nil if the
// genesis of the chain is not known.
func (a *chainArchive) genesisState(c *BALModule) state.Database {
	a.genesisMu.Lock()
	defer a.genesisMu.Unlock()
	if a.genesis == nil {
		genesis, err := c.genesisSpec(&RegenConfig{})
		if err != nil {
			return nil
		}
		dbm := database.NewMemoryDBManager()
		if genesis.ToBlock(common.Hash{}, dbm).Root() != c.bc.Genesis().Root() {
			return nil
		}
		a.genesis = state.NewDatabase(dbm)
	}
	return a.genesis
}

// signalIndex asks the BAL index to catch up with the stored lists.
func (c *BALModule) signalIndex() {
	if c.archive == nil {
		return
	}
	select {
	case c.archive.signal <- struct{}{}:
	default:
	}
}

func (c *BALModule) indexLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(indexInterval)
	defer ticker.Stop()
	for {
		if err := c.archive.Update(c.quit); err != nil {
			logger.Warn("Failed to update the BAL index", "err", err)
		}
		select {
		case <-c.quit:
			return
		case <-c.archive.signal:
		case <-ticker.C:
		}
	}
}

// stateFromBAL opens the state with the given root from the BAL history over
// the state trie in db, or from the trie alone if the history does not cover
// it or fails.
func (c *BALModule) stateFromBAL(root common.Hash, db state.Database, snaps *snapshot.Tree) (*state.StateDB, error) {
	sdb, err := c.archive.StateAt(root, db)
	if err == nil {
		return sdb, nil
	}
	trie, trieErr := state.New(root, db, snaps, nil)
	if trieErr == nil && !errors.Is(err, ErrNotCovered) {
		logger.Warn("Failed to open a state from the BAL history, reading the state trie", "root", root, "err", err)
	}
	if trieErr == nil || errors.Is(err, ErrNotCovered) {
		return trie, trieErr
	}
	return nil, err
}

// Indexed returns the blocks whose state the BAL history serves, base to
// next-1.
func Indexed(bc *blockchain.BlockChain) (base, next uint64, ok bool) { return of(bc).indexed() }

func (c *BALModule) indexed() (base, next uint64, ok bool) {
	if c.archive == nil {
		return 0, 0, false
	}
	return c.archive.Covered()
}
