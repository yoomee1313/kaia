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
	"sync"
	"sync/atomic"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus"
	"github.com/kaiachain/kaia/kaiax"
	"github.com/kaiachain/kaia/kaiax/bal"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
)

var logger = log.NewModuleLogger(log.Blockchain)

var (
	_ kaiax.BaseModule            = (*BALModule)(nil)
	_ kaiax.ExecutionStateModule  = (*BALModule)(nil)
	_ kaiax.BlockApplyModule      = (*BALModule)(nil)
	_ kaiax.HistoricalStateModule = (*BALModule)(nil)
	_ kaiax.TriePruningModule     = (*BALModule)(nil)
	_ kaiax.RewindableModule      = (*BALModule)(nil)
	_ kaiax.JsonRpcModule         = (*BALModule)(nil)

	errInitNil = errors.New("chain and chain database are required")

	// modules maps a chain to its module, for the functions of this package
	// that take the chain.
	modules sync.Map // *blockchain.BlockChain -> *BALModule
)

// BALModule keeps the block access lists of a chain: it records them, serves
// past states from them, regenerates lists and builds past state tries.
type BALModule struct {
	bc       *blockchain.BlockChain
	db       database.DBManager
	cfg      *Config
	vmConfig vm.Config

	lifeMu    sync.Mutex
	running   bool
	closeOnce sync.Once
	quit      chan struct{} // closed by Stop, nil before Start
	wg        sync.WaitGroup

	rec      *recorder     // nil unless recording
	sync     *Sync         // kaia/69 syncer of the node, nil without one
	archive  *chainArchive // serves past states from the lists, nil unless indexed
	hsegDone atomic.Uint64 // segments from the first one whose header segment roots are stored
	inserted sync.Map      // block hash -> *Block, blocks inserted by InsertChain

	genesis atomic.Pointer[blockchain.Genesis]
	regenMu sync.Mutex
	regen   *regen // last regeneration, nil if none
	tries   tries
}

// Config are the settings of a BAL module.
type Config struct {
	Record             bool // Records the block access lists of inserted blocks in the database (experimental)
	SkipReads          bool // Records the writes of the lists only, without their reads
	NoIndex            bool // Does not index the recorded lists, so past states are not read from them
	HeaderSegmentRoots bool // Stores the root of the block hashes of every finished segment
}

// InitOpts are the chain and the settings of a BAL module.
type InitOpts struct {
	Chain    *blockchain.BlockChain
	ChainDB  database.DBManager
	Config   *Config // nil records nothing
	VMConfig vm.Config
}

func NewBALModule() *BALModule {
	return &BALModule{}
}

func (c *BALModule) Init(opts *InitOpts) error {
	if opts == nil || opts.Chain == nil || opts.ChainDB == nil {
		return errInitNil
	}
	c.bc, c.db, c.cfg, c.vmConfig = opts.Chain, opts.ChainDB, opts.Config, opts.VMConfig
	if c.cfg == nil {
		c.cfg = &Config{}
	}
	if c.cfg.Record {
		c.rec = newRecorder(c.db, c.cfg.SkipReads)
		if !c.cfg.NoIndex {
			if err := c.openArchive(); err != nil {
				return err
			}
		}
	}
	modules.Store(c.bc, c)
	return nil
}

// RegisterTo registers the module with its chain, before the chain inserts a
// block.
func (c *BALModule) RegisterTo() {
	c.bc.RegisterExecutionStateModule(c)
	c.bc.RegisterBlockApplyModule(c)
	c.bc.RegisterHistoricalStateModule(c)
	c.bc.RegisterTriePruningModule(c)
	c.bc.RegisterRewindableModule(c)
}

// Start starts the loops that index the lists and store the header segment
// roots. A rewind stops the module and starts it again.
func (c *BALModule) Start() error {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.running {
		return nil
	}
	c.running = true
	c.quit = make(chan struct{})
	if c.archive != nil {
		c.wg.Add(1)
		go c.indexLoop()
	}
	if c.cfg.HeaderSegmentRoots {
		c.wg.Add(1)
		go c.headerSegmentLoop()
	}
	return nil
}

// Close closes the index database, once the module and its chain stopped.
func (c *BALModule) Close() {
	c.Stop()
	if c.archive != nil {
		c.closeOnce.Do(func() { c.archive.db.Close() })
	}
}

// Stop stops the loops, a regeneration and a trie build. A rewind stops the
// module and starts it again, so the index database stays open until Close.
func (c *BALModule) Stop() {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if !c.running {
		return
	}
	c.running = false
	close(c.quit)
	c.wg.Wait()
	if c.rec != nil {
		c.rec.close(c.bc.CurrentBlock().NumberU64())
	}
}

// of returns the BAL module of a chain.
func of(bc *blockchain.BlockChain) *BALModule {
	c, _ := modules.Load(bc)
	m, _ := c.(*BALModule)
	return m
}

func (c *BALModule) AppliesBlock(hash common.Hash) bool {
	_, ok := c.inserted.Load(hash)
	return ok
}

// BatchesBlocks reports whether the lists are not indexed, so the states
// between the commits of a light node need not be kept.
func (c *BALModule) BatchesBlocks() bool {
	return c.cfg.NoIndex
}

func (c *BALModule) PreExecuteBlock(statedb *state.StateDB) {
	if c.rec != nil {
		bal.EnableRecorder(statedb)
	}
}

func (c *BALModule) StateAt(root common.Hash) (*state.StateDB, bool, error) {
	if c.archive == nil {
		return nil, false, nil
	}
	sdb, err := c.stateFromBAL(root, c.bc.StateCache(), c.bc.Snapshots())
	return sdb, true, err
}

// RewindTo does nothing: RewindDelete drops the lists of the deleted blocks.
func (c *BALModule) RewindTo(newBlock *types.Block) {}

// RewindDelete deletes the list of a block deleted by a rewind and drops the
// block from the stored range and the index.
func (c *BALModule) RewindDelete(hash common.Hash, number uint64) {
	DeleteBAL(c.db, hash, number)
	DeleteBALReads(c.db, hash, number)
	if c.rec != nil {
		c.rec.truncate(number)
	}
	if c.archive != nil {
		c.archive.Truncate(number)
	}
}

// NewBlockChain makes a chain with its BAL module, registered with it and
// started. Tools and tests use it; a node makes the module itself, see
// node/cn. StopChain stops both.
func NewBlockChain(db database.DBManager, cacheConfig *blockchain.CacheConfig, balConfig *Config, chainConfig *params.ChainConfig, engine consensus.Sealer, vmConfig vm.Config) (*blockchain.BlockChain, error) {
	bc, err := blockchain.NewBlockChain(db, cacheConfig, chainConfig, engine, vmConfig)
	if err != nil {
		return nil, err
	}
	m := NewBALModule()
	if err := m.Init(&InitOpts{Chain: bc, ChainDB: db, Config: balConfig, VMConfig: vmConfig}); err != nil {
		bc.Stop()
		return nil, err
	}
	m.RegisterTo()
	return bc, m.Start()
}

// StopChain stops the BAL module of a chain, if it has one, and the chain,
// then closes the module.
func StopChain(bc *blockchain.BlockChain) {
	m := of(bc)
	if m != nil {
		m.Stop()
	}
	bc.Stop()
	if m != nil {
		m.Close()
	}
}
