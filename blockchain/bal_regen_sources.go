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
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
)

// Sources that start from the genesis block instead of a stored state.
const (
	BALRegenGenesis = "genesis" // execute every block from the genesis state
	BALRegenImport  = "import"  // apply the lists of another freezer, checking every root
)

// blocks between commits of the temporary state of a source from genesis
var balRegenCommitInterval uint64 = 2048

// genesisSpec returns the genesis of the chain, to rebuild its state.
func (bc *BlockChain) genesisSpec(cfg *BALRegenConfig) (*Genesis, error) {
	if cfg.Genesis != nil {
		return cfg.Genesis, nil
	}
	switch bc.chainConfig.ChainID.Uint64() {
	case DefaultGenesisBlock().Config.ChainID.Uint64():
		return DefaultGenesisBlock(), nil
	case DefaultKairosGenesisBlock().Config.ChainID.Uint64():
		return DefaultKairosGenesisBlock(), nil
	}
	return nil, fmt.Errorf("no genesis of chain %d to start from", bc.chainConfig.ChainID)
}

// runFromGenesis rebuilds the state from the genesis block in a temporary
// database, executing every block (genesis source) or applying the lists of
// another freezer (import source), checks the state root of every block, and
// commits the lists of the blocks from From on.
func (r *balRegen) runFromGenesis() {
	defer close(r.done)
	defer r.finish()

	var source *bal.Freezer
	if r.cfg.Source == BALRegenImport {
		f, err := bal.OpenFreezer(r.cfg.ImportDir, true)
		if err != nil {
			r.fail(err)
			return
		}
		defer f.Close()
		source = f
	}
	genesis, err := r.bc.genesisSpec(&r.cfg)
	if err != nil {
		r.fail(err)
		return
	}
	tempDir := filepath.Join(r.dir, "state")
	dbm := database.NewDBManager(&database.DBConfig{Dir: tempDir, DBType: database.LevelDB, OpenFilesLimit: 256, LevelDBCacheSize: 64, NumStateTrieShards: 1})
	defer os.RemoveAll(tempDir)
	defer dbm.Close()
	if root := genesis.ToBlock(common.Hash{}, dbm).Root(); root != r.bc.Genesis().Root() {
		r.fail(fmt.Errorf("genesis state root %x, chain has %x", root, r.bc.Genesis().Root()))
		return
	}

	cache := statedb.GetEmptyTrieNodeCacheConfig()
	cache.LocalCacheSizeMiB = int(r.cfg.Memory / 2 >> 20)
	sdbDB := state.NewDatabaseWithNewCache(dbm, cache)
	root := r.bc.Genesis().Root()
	sdb, err := state.New(root, sdbDB, nil, nil)
	if err != nil {
		r.fail(err)
		return
	}
	r.mu.Lock()
	r.status.Intervals = 1
	r.mu.Unlock()
	for n := uint64(1); n <= r.cfg.To; n++ {
		if r.stopped() {
			r.fail(errBALRegenStopped)
			return
		}
		block := r.bc.GetBlockByNumber(n)
		if block == nil {
			r.fail(fmt.Errorf("no block %d", n))
			return
		}
		var list *bal.BlockAccessList
		if source == nil {
			sdb.EnableBlockAccessRecorderAt(0)
			if _, _, _, _, _, err := r.bc.processor.Process(block, sdb, r.bc.vmConfig); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
			if err := sdb.Error(); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
			list = sdb.BlockAccessList()
		} else {
			if list, err = source.Read(n); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
			if err := sdb.ApplyBlockAccessList(list); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
		}
		if err := list.CheckHash(block.Header().BlockAccessListHash); err != nil {
			r.fail(fmt.Errorf("block %d: %w", n, err))
			return
		}
		if got := sdb.IntermediateRoot(true); got != block.Root() {
			r.mu.Lock()
			r.status.Mismatches++
			r.mu.Unlock()
			r.fail(fmt.Errorf("block %d: state root %x, header has %x", n, got, block.Root()))
			return
		}
		if n >= r.cfg.From {
			enc, err := list.Encode()
			if err != nil {
				r.fail(err)
				return
			}
			if err := r.commitList(n, enc); err != nil {
				r.fail(err)
				return
			}
		}
		// The temporary state is written out now and then to bound memory.
		if n%balRegenCommitInterval == 0 || heapBytes() > r.cfg.Memory+heapBase() {
			if root, err = sdb.Commit(true); err != nil {
				r.fail(err)
				return
			}
			if err := sdbDB.TrieDB().Commit(root, false, n); err != nil {
				r.fail(err)
				return
			}
			if sdb, err = state.New(root, sdbDB, nil, nil); err != nil {
				r.fail(err)
				return
			}
		}
	}
	r.mu.Lock()
	r.status.Done = 1
	r.mu.Unlock()
	logger.Info("Rebuilt the state from genesis", "number", r.cfg.To)
}

// commitList appends the list of a block, or compares it with the stored one.
func (r *balRegen) commitList(n uint64, enc []byte) error {
	if r.prefix != nil {
		if err := r.prefix.Append(n, enc); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
	} else if r.cfg.Mode == BALRegenWrite {
		if err := r.bc.bal.appendRegenerated(n, enc); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
	} else {
		r.bc.bal.mu.Lock()
		stored, err := r.bc.bal.freezer.ReadRaw(n)
		r.bc.bal.mu.Unlock()
		if err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		if !bytes.Equal(stored, enc) {
			r.mu.Lock()
			r.status.Mismatches++
			r.mu.Unlock()
			logger.Warn("Regenerated block access list differs from the stored one", "number", n)
		}
	}
	r.mu.Lock()
	r.status.Next = n + 1
	r.committed++
	r.mu.Unlock()
	return nil
}

// BALHistoryFirst returns the first block with a block access list, and
// whether the node records them.
func (bc *BlockChain) BALHistoryFirst() (uint64, bool) {
	if bc.bal == nil {
		return 0, false
	}
	bc.bal.mu.Lock()
	defer bc.bal.mu.Unlock()
	return max(bc.bal.freezer.First(), 1), true
}

// ErrNoHistory is returned for the state of a block before the BAL history.
var ErrNoHistory = errors.New("no history")

// HistoryError explains why the state of a block is missing: the history of
// the node starts later, and admin_startBALRegen can build the earlier part.
func (bc *BlockChain) HistoryError(number uint64, err error) error {
	if first, ok := bc.BALHistoryFirst(); ok && number < first {
		return fmt.Errorf("%w before block %d, regenerate it with admin_startBALRegen: %v", ErrNoHistory, first, err)
	}
	return err
}
