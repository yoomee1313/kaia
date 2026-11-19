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
	"sync"
	"sync/atomic"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/prque"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/rcrowley/go-metrics"
)

var (
	trieDBNodesSizeBytesGauge = metrics.NewRegisteredGauge("klay/triedb/nodessizebytes", nil)
	trieDBPreimagesSizeGauge  = metrics.NewRegisteredGauge("klay/triedb/preimagessizebytes", nil)
)

// Config is how a State keeps the states of the blocks a chain writes.
type Config struct {
	// Archive flushes the state of every block to disk.
	Archive bool
	// CacheSize is the memory, in MB, the trie nodes not yet on disk may take
	// before the oldest are flushed.
	CacheSize int
	// TriesInMemory is how many of the last block states stay in memory.
	TriesInMemory uint64
	// LivePruningRetention is how many blocks the trie nodes a block made
	// obsolete are kept, with live pruning. Zero keeps them.
	LivePruningRetention uint64

	// CommitRequired reports whether the state of a block is flushed to
	// disk, unless Archive is set.
	CommitRequired func(number uint64) bool
	// Flushed is called after the state of a block is flushed to disk. If it
	// returns true, the rest of the trie nodes in memory are flushed too,
	// unless Archive is set.
	Flushed func(number uint64, root common.Hash) bool
	// Prune deletes the trie nodes of marks, of blocks up to limit.
	Prune func(marks []database.PruningMark, limit uint64)
}

// State keeps the states of the blocks a chain writes: it commits the state
// of each written block to the trie database, flushes it to disk when
// required, garbage collects the states that leave memory, and prunes the
// trie nodes they made obsolete with live pruning.
type State struct {
	db     Database
	diskDB database.DBManager
	cfg    Config

	lastCommitted atomic.Uint64 // the last block whose state was flushed to disk

	triegc  *prque.Prque  // block states in memory, by block number, to garbage collect
	chBlock chan gcBlock  // written block states, for the garbage collection loop
	chPrune chan uint64   // flushed block numbers, for the pruning loop
	quit    chan struct{} // stops the loops
	wg      sync.WaitGroup
}

type gcBlock struct {
	root     common.Hash
	blockNum uint64
}

// NewState returns a State that keeps the block states in db, which reads
// and writes diskDB, with lastCommitted the last block whose state is on
// disk. Start runs its loops.
func NewState(db Database, diskDB database.DBManager, cfg Config, lastCommitted uint64) *State {
	s := &State{
		db:      db,
		diskDB:  diskDB,
		cfg:     cfg,
		triegc:  prque.New(),
		chBlock: make(chan gcBlock, 2048), // downloader.maxResultsProcess
		chPrune: make(chan uint64, 2048),  // downloader.maxResultsProcess
		quit:    make(chan struct{}),
	}
	s.lastCommitted.Store(lastCommitted)
	return s
}

// Database returns the database the states are kept in.
func (s *State) Database() Database {
	return s.db
}

// LastCommitted returns the last block whose state was flushed to disk.
func (s *State) LastCommitted() uint64 {
	return s.lastCommitted.Load()
}

// SetLastCommitted sets the last block whose state is on disk, like when the
// head is set.
func (s *State) SetLastCommitted(number uint64) {
	s.lastCommitted.Store(number)
}

// LivePruning reports whether the trie nodes made obsolete are pruned.
func (s *State) LivePruning() bool {
	return s.diskDB.ReadPruningEnabled() && s.cfg.LivePruningRetention != 0
}

// Commit commits sdb, the state of block, to the trie database and flushes
// it to disk if the node is an archive or the block requires it.
func (s *State) Commit(block *types.Block, sdb *StateDB) error {
	sdb.LockGCCachedNode()
	defer sdb.UnlockGCCachedNode()

	root, err := sdb.Commit(true)
	if err != nil {
		return err
	}
	trieDB := s.db.TrieDB()
	trieDB.UpdateMetricNodes()

	// If we're running an archive node, always flush
	if s.cfg.Archive {
		if err := trieDB.Commit(root, false, block.NumberU64()); err != nil {
			return err
		}

		s.cfg.Flushed(block.NumberU64(), root)
		s.lastCommitted.Store(block.NumberU64())

		if s.LivePruning() {
			s.chPrune <- block.NumberU64()
		}
		return nil
	}

	// Full but not archive node, do proper garbage collection
	trieDB.ReferenceRoot(root) // metadata reference to keep trie alive

	// If we exceeded our memory allowance, flush matured singleton nodes to disk
	var (
		nodesSize, _, preimagesSize = trieDB.Size()
		nodesSizeLimit              = common.StorageSize(s.cfg.CacheSize) * 1024 * 1024
	)

	trieDBNodesSizeBytesGauge.Update(int64(nodesSize))
	trieDBPreimagesSizeGauge.Update(int64(preimagesSize))

	if nodesSize > nodesSizeLimit || preimagesSize > 4*1024*1024 {
		// NOTE-Kaia Not to change the original behavior, error is not returned.
		// Error should be returned if it is thought to be safe in the future.
		if err := trieDB.Cap(nodesSizeLimit - database.IdealBatchSize); err != nil {
			logger.Error("Error from trieDB.Cap", "err", err, "limit", nodesSizeLimit-database.IdealBatchSize)
		}
	}

	if s.cfg.CommitRequired(block.NumberU64()) {
		if err := trieDB.Commit(block.Header().Root, true, block.NumberU64()); err != nil {
			return err
		}
		logger.Trace("Committed the state trie into the disk", "blocknum", block.NumberU64())

		if s.cfg.Flushed(block.NumberU64(), root) {
			// flush referenced trie nodes out to new stateTrieDB
			if err := trieDB.Cap(0); err != nil {
				logger.Error("Error from trieDB.Cap by state migration", "err", err)
			}
		}
		s.lastCommitted.Store(block.NumberU64())

		if s.LivePruning() {
			s.chPrune <- block.NumberU64()
		}

		if dm := trieDB.DiskDB().GetDomainsManager(); dm != nil {
			if err := dm.CommitWrites(); err != nil {
				return err
			}
			logger.Info("Committed domains manager changes into the disk", "blocknum", block.NumberU64())
		}
	}

	s.chBlock <- gcBlock{root, block.NumberU64()}
	return nil
}

// Start runs the loops that garbage collect the states leaving memory and
// prune the trie nodes made obsolete.
func (s *State) Start() {
	s.gcLoop()
	s.pruneLoop()
}

// Stop stops the loops.
func (s *State) Stop() {
	close(s.quit)
	s.wg.Wait()
}

// FlushOnStop flushes the state of the head block and the state the snapshot
// is based on, if any, to disk, and drops the states in memory. The archive
// node has every state on disk already.
func (s *State) FlushOnStop(head *types.Block, snapBase common.Hash) {
	triedb := s.db.TrieDB()
	logger.Info("Writing cached state to disk", "block", head.Number(), "hash", head.Hash(), "root", head.Root())
	if err := triedb.Commit(head.Root(), true, head.NumberU64()); err != nil {
		logger.Error("Failed to commit recent state trie", "err", err)
	}
	if snapBase != (common.Hash{}) {
		logger.Info("Writing snapshot state to disk", "root", snapBase)
		if err := triedb.Commit(snapBase, true, head.NumberU64()); err != nil {
			logger.Error("Failed to commit recent state trie", "err", err)
		}
	}
	for !s.triegc.Empty() {
		triedb.Dereference(s.triegc.PopItem().(common.Hash))
	}
	if size, _, _ := triedb.Size(); size != 0 {
		logger.Error("Dangling trie nodes after full cleanup")
	}
}

// Close closes the trie node cache.
func (s *State) Close() {
	if c := s.db.TrieDB().TrieNodeCache(); c != nil {
		_ = c.Close()
	}
}

// gcLoop dereferences the states that leave memory.
func (s *State) gcLoop() {
	trieDB := s.db.TrieDB()

	s.wg.Go(func() {
		for {
			select {
			case block := <-s.chBlock:
				s.triegc.Push(block.root, -int64(block.blockNum))
				logger.Trace("Push GC block", "blkNum", block.blockNum, "hash", block.root.String())

				blkNum := block.blockNum
				if blkNum <= s.cfg.TriesInMemory {
					continue
				}

				// Garbage collect anything below our required write retention
				chosen := blkNum - s.cfg.TriesInMemory
				cnt := 0
				for !s.triegc.Empty() {
					root, number := s.triegc.Pop()
					if uint64(-number) > chosen {
						s.triegc.Push(root, number)
						break
					}
					trieDB.Dereference(root.(common.Hash))
					cnt++
				}
				logger.Debug("GC cached node", "currentBlk", blkNum, "chosenBlk", chosen, "deferenceCnt", cnt)
			case <-s.quit:
				return
			}
		}
	})
}

// pruneLoop deletes the trie nodes marked obsolete by blocks older than the
// retention.
func (s *State) pruneLoop() {
	// ReadPruningMarks(1, limit) is very slow because it iterates over the most of MiscDB.
	// ReadPruningMarks(start, limit) is much faster because it only iterates a small range.
	startNum := uint64(1)

	s.wg.Go(func() {
		for {
			select {
			case num := <-s.chPrune:
				if num <= s.cfg.LivePruningRetention {
					continue
				}
				limit := num - s.cfg.LivePruningRetention // Prune [1, latest - retention]

				startTime := time.Now()
				count := s.diskDB.PruneMarkedTrieNodes(startNum, limit+1, func(marks []database.PruningMark) {
					s.cfg.Prune(marks, limit)
				})
				s.diskDB.WriteLastPrunedBlockNumber(limit)

				logger.Info("Pruned trie nodes", "number", num, "start", startNum, "limit", limit,
					"count", count, "elapsed", time.Since(startTime))

				startNum = limit + 1
			case <-s.quit:
				return
			}
		}
	})
}
