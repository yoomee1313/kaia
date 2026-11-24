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

package state

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/prque"
	"github.com/kaiachain/kaia/snapshot"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
)

// ManagerConfig is how a Manager keeps the states of the blocks a chain writes.
type ManagerConfig struct {
	TrieConfig
	// HistoryMode is how much of the chain the node keeps, full if empty. A
	// history node keeps its BAL history.
	HistoryMode database.HistoryMode
	// BAL is how a history node keeps its BAL history.
	BAL BALConfig
	// Head returns the header of the head block.
	Head func() *types.Header
	// HeadBlock returns the head block, whose state the next block runs on.
	HeadBlock func() *types.Block
	// HeaderAt returns the canonical header of block number, nil if none.
	HeaderAt func(number uint64) *types.Header
	// Header returns the header with hash and number, nil if none.
	Header func(hash common.Hash, number uint64) *types.Header
	// GenesisState returns the genesis state rebuilt, nil if it is not known.
	GenesisState func() Database
	// ChainLock is held while the migrated state database replaces the old
	// one, so that no block is written meanwhile.
	ChainLock sync.Locker
}

// Manager keeps the states of the blocks a chain writes across blocks, in the
// state trie and, for a history node, in the BAL history: it commits the state
// of each written block and stores its block access list, flushes the trie
// nodes to disk, garbage collects and prunes them, keeps the snapshot, opens
// the states of blocks to write and to read them, and migrates and warms up
// the state trie.
type Manager struct {
	db     Database
	diskDB database.DBManager
	cfg    ManagerConfig
	snaps  *snapshot.Tree

	lastCommitted atomic.Uint64     // the last block whose state was flushed to disk
	triegc        *prque.Prque      // block states in memory, by block number, to garbage collect
	chBlock       chan gcBlock      // written block states, for the garbage collection loop
	chPrune       chan pruneRequest // flushed block numbers, for the pruning loop
	pruning       pruning
	onFlush       atomic.Pointer[FlushHook] // runs at the next flush, which it forces
	kept          keptTries

	bals      *BALHistory // nil for an archive or full node
	batch     *balBatch   // nil unless a history node
	migration migration
	warmUp    chan struct{}   // stops the warm-up, nil if none runs
	genesis   func() Database // the genesis state rebuilt once, nil if it is not known
	quit      chan struct{}   // stops the loops, the migration and the warm-up
	wg        sync.WaitGroup
}

// NewManager returns a Manager that keeps the block states in db, which reads and
// writes diskDB made for cfg.HistoryMode, and opens the BAL history of a history
// node. Start runs its loops.
func NewManager(db Database, diskDB database.DBManager, cfg ManagerConfig) (*Manager, error) {
	mode, err := database.ParseHistoryMode(string(cfg.HistoryMode))
	if err != nil {
		return nil, err
	}
	cfg.HistoryMode = mode
	s := &Manager{
		db:        db,
		diskDB:    diskDB,
		cfg:       cfg,
		triegc:    prque.New(),
		chBlock:   make(chan gcBlock, 2048),      // downloader.maxResultsProcess
		chPrune:   make(chan pruneRequest, 2048), // downloader.maxResultsProcess
		quit:      make(chan struct{}),
		migration: migration{stop: make(chan struct{})},
	}
	if cfg.GenesisState != nil {
		s.genesis = sync.OnceValue(cfg.GenesisState)
	}
	if pruned, err := diskDB.ReadLastPrunedBlockNumber(); err == nil {
		s.pruning.last.Store(pruned)
	}
	if mode == database.HistoryModeBAL {
		bals, err := OpenBALHistory(diskDB, cfg.BAL, cfg.Head, s.balBase)
		if err != nil {
			return nil, err
		}
		s.bals, s.batch = bals, &balBatch{}
	}
	return s, nil
}

// HistoryMode returns how much of the chain the node keeps.
func (s *Manager) HistoryMode() database.HistoryMode {
	return s.cfg.HistoryMode
}

// Database returns the database the states are kept in.
func (s *Manager) Database() Database {
	return s.db
}

// TrieDB returns the trie database the states are kept in.
func (s *Manager) TrieDB() *statedb.Database {
	return s.db.TrieDB()
}

// BALs returns the BAL history of a history node, nil for an
// archive or full node.
func (s *Manager) BALs() *BALHistory {
	return s.bals
}

// Start opens the snapshot of the state of the head, runs the loops that
// garbage collect the states leaving memory, prune the trie nodes made
// obsolete, save the trie node cache periodically if configured and index the
// BAL history, and resumes a migration the node stopped in.
func (s *Manager) Start() {
	if s.cfg.Head != nil {
		s.OpenSnapshots(s.cfg.Head())
	}
	s.gcLoop()
	s.pruneLoop()
	trieDB := s.TrieDB()
	if c := trieDB.GetTrieNodeCacheConfig(); c != nil && c.DumpPeriodically() {
		logger.Info("LocalCache is used for trie node cache, start saving cache to file periodically",
			"dir", c.FastCacheFileDir, "period", c.FastCacheSavePeriod)
		s.wg.Go(func() {
			trieDB.SaveCachePeriodically(c, s.quit)
		})
	}
	s.bals.Start()
	s.ResumeMigration()
}

// Stop stops the loops, the migration and the warm-up, journals the snapshot,
// flushes the state of head and the state the snapshot is based on to disk,
// unless the node is an archive, which has every state on disk already, and
// closes the trie node cache.
func (s *Manager) Stop(head *types.Block) {
	close(s.quit)
	s.wg.Wait()
	s.bals.Stop()
	snapBase := s.journalSnapshots(head.Root())
	if !s.cfg.Archive {
		s.flush(head, snapBase)
	}
	if c := s.TrieDB().TrieNodeCache(); c != nil {
		_ = c.Close()
	}
}

// ResetHead moves the head of the chain to head, whose state must be on disk
// already, and rebuilds the snapshot for it in the background.
func (s *Manager) ResetHead(head *types.Block) error {
	if _, err := s.db.OpenTrie(head.Root(), nil); err != nil {
		return err
	}
	s.SetLastCommitted(head.NumberU64())
	s.RebuildSnapshots(head.Root())
	return nil
}

// Commit commits sdb, the state of block, and flushes it to disk if the node
// is an archive or the block requires it. The state of the batch is committed
// only once it is checked, see Check.
func (s *Manager) Commit(block *types.Block, sdb *StateDB) error {
	if b := s.batch; b.owns(sdb) && !b.commit {
		return nil
	}
	return s.commit(block.NumberU64(), sdb)
}

func (s *Manager) commit(number uint64, sdb *StateDB) error {
	sdb.LockGCCachedNode()
	defer sdb.UnlockGCCachedNode()

	root, err := sdb.Commit(true)
	if err != nil {
		return err
	}
	return s.commitRoot(number, root)
}

// Written tells that block was written with sdb: it stores the list of the
// block in the BAL history, the list it was inserted with or the one sdb
// recorded while it was executed, and moves the batch on.
func (s *Manager) Written(block *types.Block, sdb *StateDB) {
	s.batch.written(block.Header(), sdb)
	if s.bals == nil {
		return
	}
	if inserted := block.BAL(); inserted != nil {
		s.bals.PutInserted(block, inserted)
		return
	}
	if s.bals.Stored(block.NumberU64(), block.Hash()) {
		return
	}
	s.bals.PutExecuted(block, sdb.BAL())
}

// balBase opens the state the BAL index starts from: for the genesis block
// its state rebuilt from the genesis of the chain, otherwise the stored state.
func (s *Manager) balBase(number uint64, root common.Hash) (BaseState, error) {
	db := s.db
	if number == 0 {
		if s.genesis != nil {
			if g := s.genesis(); g != nil {
				db = g
			}
		}
	}
	sdb, err := New(root, db, nil, nil)
	if err != nil {
		return nil, err
	}
	return NewBALBase(sdb), nil
}

var ErrNotExistNode = errors.New("the node does not exist in cached node")

// Use is what a state is opened for, see Open.
type Use int

const (
	// ForRead opens a state to read it: from its trie if it is stored, and
	// otherwise, on a history node, from the BAL history, read-only. The BAL
	// history also serves the blocks a sync wrote before their states are
	// checked.
	ForRead Use = iota
	// ForTrie opens a state from its trie only, to execute a block on it
	// without writing it.
	ForTrie
	// ForBuild opens a state from its trie to build the next block on and
	// write it: with live pruning, the trie nodes it makes obsolete are
	// marked at the head, and the block access list of the block is recorded
	// if the BAL history stores it.
	ForBuild
	// ForExec opens a state like ForBuild to insert the next block by
	// executing it. The batch is checked and committed first.
	ForExec
	// ForApply opens a state like ForExec to insert the next block by
	// applying its block access list, which is stored as it is, so none is
	// recorded. On a history node it is the state of the batch, which goes
	// on from the last block written with it.
	ForApply
	// ForPrefetch opens a throwaway state from the trie to prefetch trie
	// nodes with: without the snapshot, unless the trie node cache is set to
	// prefetch with it, so that its reads load the nodes into the cache.
	ForPrefetch
)

// Open returns a new mutable state of the block of header, opened for use.
func (s *Manager) Open(header *types.Header, use Use) (*StateDB, error) {
	root := header.Root
	switch use {
	case ForPrefetch:
		var snaps *snapshot.Tree
		if c := s.TrieDB().GetTrieNodeCacheConfig(); c != nil && c.UseSnapshotForPrefetch {
			snaps = s.Snapshots()
		}
		return New(root, s.db, snaps, &statedb.TrieOpts{Prefetching: true})
	case ForBuild, ForExec, ForApply:
		b := s.batch
		if use == ForApply && b.continues(header) {
			return b.state, nil
		}
		if use != ForBuild {
			if err := s.Settle(); err != nil {
				return nil, err
			}
		}
		sdb, err := New(root, s.db, s.Snapshots(), s.headTrieOpts(root))
		if err != nil {
			return nil, err
		}
		if use != ForApply && s.bals != nil {
			sdb.StartBAL(0)
		}
		if use == ForApply && b != nil {
			b.start(header, sdb)
		}
		return sdb, nil
	}
	// The trie of a block live pruning went over may keep its root and lose
	// nodes below it, so a history node reads such a state from the BAL
	// history unless the trie is kept.
	if use == ForRead && s.bals.Indexes() && !s.unpruned(header.Number.Uint64()) {
		if k, ok := s.KeptTrie(header.Number.Uint64()); !ok || k.Root != root {
			if bal, err := OpenBALState(s.bals, header, s.db); !errors.Is(err, ErrNotCovered) {
				return bal, err
			}
		}
	}
	sdb, err := New(root, s.db, s.Snapshots(), nil)
	if err == nil || use != ForRead || !s.bals.Indexes() {
		return sdb, err
	}
	bal, balErr := OpenBALState(s.bals, header, s.db)
	if balErr == nil {
		return bal, nil
	}
	if !errors.Is(balErr, ErrNotCovered) {
		return nil, balErr
	}
	return nil, err
}

// headTrieOpts returns the trie options of the state with root, whose obsolete
// trie nodes are marked at the head with live pruning if it is the state of
// the head.
func (s *Manager) headTrieOpts(root common.Hash) *statedb.TrieOpts {
	if s.cfg.HeadBlock == nil || !s.LivePruning() {
		return nil
	}
	if head := s.cfg.HeadBlock(); head != nil && head.Root() == root {
		return &statedb.TrieOpts{PruningBlockNumber: head.NumberU64()}
	}
	return nil
}

// Rewind makes the state of target writable and returns its header: a
// history node builds its trie from the BAL history, and otherwise it is the
// highest block at or below target whose state trie is stored. With a limit
// root, the block is the one with that root or below it, and its number is
// returned too.
func (s *Manager) Rewind(target *types.Header, limit common.Hash) (*types.Header, uint64, error) {
	h, number, err := s.rewind(target, limit)
	if err == nil {
		s.chBlock <- gcBlock{blockNum: h.Number.Uint64(), rewound: true}
		s.rewindPruning(h.Number.Uint64())
	}
	return h, number, err
}

func (s *Manager) rewind(target *types.Header, limit common.Hash) (*types.Header, uint64, error) {
	if s.bals.Indexes() {
		h, number := target, uint64(0)
		for limit != (common.Hash{}) && h != nil && h.Root != limit {
			h = s.cfg.Header(h.ParentHash, h.Number.Uint64()-1)
		}
		if h != nil && limit != (common.Hash{}) {
			number = h.Number.Uint64()
		}
		if h != nil {
			if s.wholeTrie(h) {
				return h, number, nil
			}
			err := s.buildTrie(h)
			if err == nil {
				logger.Info("Built the state trie of the new head from the BAL history", "number", h.Number, "root", h.Root)
				return h, number, nil
			}
			logger.Warn("Failed to build the state trie of the new head, rewinding to a stored one", "number", h.Number, "err", err)
		}
	}
	h, number, err := s.rewindTrie(target, limit)
	if err == nil && s.bals != nil {
		// The lists above the new head go, which a repair, keeping the
		// blocks, would not drop: they may be of blocks a sync wrote
		// before their states were checked.
		for n := target.Number.Uint64(); n > h.Number.Uint64(); n-- {
			if dropped := s.cfg.HeaderAt(n); dropped != nil {
				s.bals.RewindDelete(dropped.Hash(), n)
			}
		}
	}
	return h, number, err
}

// wholeTrie reports whether the state trie of the block of header is whole,
// see unpruned; one live pruning went over is whole only if it is kept.
func (s *Manager) wholeTrie(header *types.Header) bool {
	if !s.HasTrie(header.Root) {
		return false
	}
	number := header.Number.Uint64()
	if s.unpruned(number) {
		return true
	}
	k, ok := s.KeptTrie(number)
	return ok && k.Root == header.Root
}

// unpruned reports whether live pruning left the state trie of block number
// whole. The marks of block n are the nodes of its trie that block n+1 made
// obsolete, so the pruning of the blocks up to the last pruned one deletes
// nodes of their tries, which may keep their roots. The genesis state, whose
// nodes have no nonce, is kept.
func (s *Manager) unpruned(number uint64) bool {
	return number == 0 || number > s.lastPruned()
}

// StateAtWithPersistent returns a new mutable state of the block of header
// from its trie, if its root node is on disk.
func (s *Manager) StateAtWithPersistent(header *types.Header) (*StateDB, error) {
	if !s.db.TrieDB().DoesExistNodeInPersistent(header.Root.ExtendZero()) {
		return nil, ErrNotExistNode
	}
	return s.Open(header, ForTrie)
}

// A history node applies the block access lists of the blocks it syncs onto
// one state, the batch, without hashing it per block: the state root is
// checked and the state committed at the blocks the trie is committed at and
// when the chain settles after an insertion. The blocks in between are
// written with unchecked states, which the BAL history serves. A wrong root
// rolls the chain back to the last checked block. The batch is used under the
// chain lock: by ForExec and ForApply, Check, ApplyBAL, Commit, Written and
// Settle.

var errBatchUnwritten = errors.New("the state holds the changes of an applied block that was not written")

// RollbackError is a wrong state root found after blocks were written with
// the unchecked state of the batch: the chain is rolled back to block To, the
// last checked one.
type RollbackError struct {
	To  uint64
	Err error
}

func (e *RollbackError) Error() string { return e.Err.Error() }

func (e *RollbackError) Unwrap() error { return e.Err }

// balBatch holds the blocks written after the last checked one.
type balBatch struct {
	base    *types.Header   // last checked block
	state   *StateDB        // base with the applied lists, nil when idle
	applied bool            // a list was applied for a block not written yet
	commit  bool            // the applied block is checked
	pending []*types.Header // written blocks after base
}

// HasState reports whether the state of the block of header is there: its
// trie is stored, or the BAL history holds the list of the block.
func (s *Manager) HasState(header *types.Header) bool {
	return s.Executable(header.Root) || s.bals != nil && s.bals.Stored(header.Number.Uint64(), header.Hash())
}

// Executable reports whether blocks can be executed on the state with root:
// its trie is stored, or it is the state of the batch, which the next block
// of the insertion runs on.
func (s *Manager) Executable(root common.Hash) bool {
	return s.batch.holds(root) || s.HasTrie(root)
}

// Check checks sdb, the state of block, with validate. The state of the batch
// is checked only where the trie is committed, see Settle; a wrong root there
// is a RollbackError.
func (s *Manager) Check(block *types.Block, sdb *StateDB, validate func(*StateDB) error) error {
	b := s.batch
	if !b.owns(sdb) {
		return validate(sdb)
	}
	if !s.CommitRequired(block.NumberU64()) {
		return nil
	}
	b.commit = true
	sdb.HashStorages(runtime.NumCPU())
	if err := validate(sdb); err != nil {
		return s.rollback(fmt.Errorf("applied blocks %d-%d: %w", b.base.Number.Uint64()+1, block.NumberU64(), err))
	}
	if err := sdb.Error(); err != nil {
		return s.rollback(err)
	}
	return nil
}

// ApplyBAL applies list, the block access list of a block, to sdb, its parent
// state. On the state of the batch, check runs first and the list is applied
// without hashing.
func (s *Manager) ApplyBAL(sdb *StateDB, list *types.BlockAccessList, check func() error) error {
	b := s.batch
	if !b.owns(sdb) {
		return sdb.ApplyBAL(list)
	}
	if err := check(); err != nil {
		return err
	}
	b.applied = true
	return sdb.ApplyBALUnchecked(list)
}

// Settle checks and commits the state of the last block written with the
// batch, which the chain does after each insertion. A wrong root is a
// RollbackError.
func (s *Manager) Settle() error {
	b := s.batch
	if b == nil || b.state == nil {
		return nil
	}
	if b.applied {
		return s.rollback(errBatchUnwritten)
	}
	if len(b.pending) == 0 {
		b.stop()
		return nil
	}
	last := b.pending[len(b.pending)-1]
	b.commit = true
	b.state.HashStorages(runtime.NumCPU())
	if root := b.state.IntermediateRoot(true); root != last.Root {
		return s.rollback(fmt.Errorf("applied blocks %d-%d: invalid merkle root (remote: %x local: %x)",
			b.base.Number.Uint64()+1, last.Number.Uint64(), last.Root, root))
	}
	if err := b.state.Error(); err != nil {
		return s.rollback(err)
	}
	if err := s.commit(last.Number.Uint64(), b.state); err != nil {
		return s.rollback(err)
	}
	b.stop()
	b.base = last
	return nil
}

// rollback drops the state of the batch, and returns a RollbackError to its
// base if blocks after it were written.
func (s *Manager) rollback(err error) error {
	b := s.batch
	base, written := b.base, len(b.pending) > 0
	b.stop()
	if !written {
		return err
	}
	return &RollbackError{To: base.Number.Uint64(), Err: err}
}

func (b *balBatch) start(base *types.Header, sdb *StateDB) {
	b.base, b.state, b.applied, b.commit, b.pending = base, sdb, false, false, nil
}

// continues reports whether the batch goes on from the block of header, the
// last one written with it.
func (b *balBatch) continues(header *types.Header) bool {
	return b != nil && b.state != nil && !b.applied && b.head().Hash() == header.Hash()
}

func (b *balBatch) head() *types.Header {
	if len(b.pending) > 0 {
		return b.pending[len(b.pending)-1]
	}
	return b.base
}

// holds reports whether root is the state of the last block the batch wrote.
func (b *balBatch) holds(root common.Hash) bool {
	return b != nil && b.state != nil && len(b.pending) > 0 && b.pending[len(b.pending)-1].Root == root
}

// owns reports whether sdb is the state of the batch.
func (b *balBatch) owns(sdb *StateDB) bool {
	return b != nil && sdb != nil && sdb == b.state
}

// written moves on to the block of header written with sdb: a checked one,
// whose state is committed, becomes the base of the next blocks.
func (b *balBatch) written(header *types.Header, sdb *StateDB) {
	if !b.owns(sdb) {
		return
	}
	b.applied = false
	if b.commit {
		b.stop()
		b.base = header
		return
	}
	b.pending = append(b.pending, header)
}

// stop drops the state.
func (b *balBatch) stop() {
	b.state, b.applied, b.commit, b.pending = nil, false, false, nil
}
