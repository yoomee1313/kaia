// Modifications Copyright 2024 The Kaia Authors
// Copyright 2020 The klaytn Authors
// This file is part of the klaytn library.
//
// The klaytn library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The klaytn library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the klaytn library. If not, see <http://www.gnu.org/licenses/>.
// Modified and improved for the Kaia development.

package state

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/alecthomas/units"
	lru "github.com/hashicorp/golang-lru"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/mclock"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
)

const (
	DefaultWarmUpMinLoad        = 90
	DefaultWarmUpReportInterval = time.Second * 10
)

var (
	ErrQuitBySignal = errors.New("quit by signal")
	ErrNotInWarmUp  = errors.New("not in warm up")

	stateStoppedErr = errors.New("warm-up terminate as blockchain stopped")
)

// migration is the state of the state trie migration.
type migration struct {
	stop chan struct{} // stops the running migration

	readCnt      int
	committedCnt int
	pendingCnt   int
	progress     float64
	err          error

	hook func() // called before the migration copies the trie nodes, for tests
}

// SetMigrationHook sets f to be called before a migration copies the trie
// nodes, for tests.
func (s *Manager) SetMigrationHook(f func()) {
	s.migration.hook = f
}

func migrationCommit(sync *statedb.TrieSync, batch database.Batch) (int, error) {
	written, err := sync.Commit(batch)
	if written == 0 || err != nil {
		return written, err
	}

	if batch.ValueSize() > database.IdealBatchSize {
		if err := batch.Write(); err != nil {
			return 0, fmt.Errorf("DB write error: %v", err)
		}
		batch.Reset()
	}

	return written, nil
}

func concurrentRead(db Database, quitCh chan struct{}, hashCh chan common.Hash, resultCh chan statedb.SyncResult) {
	for {
		select {
		case <-quitCh:
			return
		case hash := <-hashCh:
			data, err := db.TrieDB().NodeFromOld(hash.ExtendZero())
			if err != nil {
				data, err = db.ContractCode(hash)
			}
			if err != nil {
				resultCh <- statedb.SyncResult{Hash: hash, Err: err}
				continue
			}
			resultCh <- statedb.SyncResult{Hash: hash, Data: data}
		}
	}
}

// migrate is the core implementation of state trie migration.
// This migrates a trie from StateTrieDB to StateTrieMigrationDB.
// Reading StateTrieDB happens in parallel and writing StateTrieMigrationDB happens in batch write.
//
// Before this function is called, StateTrieMigrationDB should be set.
// After the migration finish, the original StateTrieDB is removed and StateTrieMigrationDB becomes a new StateTrieDB.
func (s *Manager) migrate(rootHash common.Hash) (returnErr error) {
	m := &s.migration
	m.err = nil
	defer func() {
		m.err = returnErr
		// If migration stops by quit signal, it doesn't finish migration and it will restart again.
		if returnErr != ErrQuitBySignal {
			// lock to prevent from a conflict of state DB close and state DB write
			s.cfg.ChainLock.Lock()
			s.diskDB.FinishStateMigration(returnErr == nil)
			s.cfg.ChainLock.Unlock()
		}
	}()

	start := time.Now()

	srcState := s.db
	dstState := NewDatabase(s.diskDB)

	// NOTE: lruCache is mandatory when state migration and block processing are executed simultaneously
	lruCache, _ := lru.New(int(2 * units.Giga / common.HashLength)) // 2GB for 62,500,000 common.Hash key values
	trieSync := NewStateSync(rootHash, dstState.TrieDB().DiskDB(), nil, lruCache, nil)
	var queue []common.Hash

	quitCh := make(chan struct{})
	defer close(quitCh)

	// Prepare concurrent read goroutines
	threads := runtime.NumCPU()
	hashCh := make(chan common.Hash, threads)
	resultCh := make(chan statedb.SyncResult, threads)

	for range threads {
		go concurrentRead(srcState, quitCh, hashCh, resultCh)
	}

	stateTrieBatch := dstState.TrieDB().DiskDB().NewBatch(database.StateTrieDB)
	defer stateTrieBatch.Release()
	stats := migrationStats{initialStartTime: start, startTime: mclock.Now()}

	if m.hook != nil {
		m.hook()
	}

	// Migration main loop
	for trieSync.Pending() > 0 {
		nodes, _, codes := trieSync.Missing(1024)
		queue = append(queue[:0], append(nodes, codes...)...)
		results := make([]statedb.SyncResult, len(queue))

		// Read the trie nodes
		startRead := time.Now()
		go func() {
			for _, hash := range queue {
				hashCh <- hash
			}
		}()

		for i := 0; i < len(queue); i++ {
			result := <-resultCh
			if result.Err != nil {
				logger.Error("State migration is failed by resultCh",
					"result.hash", result.Hash.String(), "result.Err", result.Err)
				return fmt.Errorf("failed to retrieve node data for %x: %v", result.Hash, result.Err)
			}
			results[i] = result
		}
		stats.read += len(queue)
		stats.readElapsed += time.Since(startRead)

		// Process trie nodes
		startProcess := time.Now()
		for index, result := range results {
			if err := trieSync.Process(result); err != nil {
				logger.Error("State migration is failed by process error", "err", err)
				return fmt.Errorf("failed to process result #%d: %v", index, err)
			}
		}
		stats.processElapsed += time.Since(startProcess)

		// Commit trie nodes
		startWrite := time.Now()
		written, err := migrationCommit(trieSync, stateTrieBatch)
		if err != nil {
			logger.Error("State migration is failed by commit error", "err", err)
			return fmt.Errorf("failed to commit data #%d: %v", written, err)
		}
		stats.committed += written
		stats.writeElapsed += time.Since(startWrite)

		// Report progress
		stats.stateMigrationReport(false, trieSync.Pending(), trieSync.CalcProgressPercentage())

		select {
		case <-m.stop:
			logger.Info("State migration terminated by request")
			return errors.New("stop state migration")
		case <-s.quit:
			logger.Info("State migration stopped by quit signal; should continue on node restart")
			return ErrQuitBySignal
		default:
		}

		m.readCnt, m.committedCnt, m.pendingCnt, m.progress = stats.totalRead, stats.totalCommitted, trieSync.Pending(), stats.progress
	}

	// Flush trie nodes which is not written yet.
	if err := stateTrieBatch.Write(); err != nil {
		logger.Error("State migration is failed by commit error", "err", err)
		return fmt.Errorf("DB write error: %v", err)
	}

	stats.stateMigrationReport(true, trieSync.Pending(), trieSync.CalcProgressPercentage())
	m.readCnt, m.committedCnt, m.pendingCnt, m.progress = stats.totalRead, stats.totalCommitted, trieSync.Pending(), stats.progress

	// Clear memory of trieSync
	trieSync = nil

	elapsed := time.Since(start)
	speed := float64(stats.totalCommitted) / elapsed.Seconds()
	logger.Info("State migration : Copy is done",
		"totalRead", stats.totalRead, "totalCommitted", stats.totalCommitted,
		"totalElapsed", elapsed, "committed per second", speed)

	startCheck := time.Now()
	if err := CheckStateConsistencyParallel(srcState, dstState, rootHash, s.quit); err != nil {
		logger.Error("State migration : copied stateDB is invalid", "err", err)
		return err
	}
	checkElapsed := time.Since(startCheck)
	logger.Info("State migration is completed", "copyElapsed", elapsed, "checkElapsed", checkElapsed)
	return nil
}

// migrationStats tracks and reports on state migration.
type migrationStats struct {
	read, committed, totalRead, totalCommitted, pending int
	progress                                            float64
	initialStartTime                                    time.Time
	startTime                                           mclock.AbsTime
	readElapsed                                         time.Duration
	processElapsed                                      time.Duration
	writeElapsed                                        time.Duration
}

func (st *migrationStats) stateMigrationReport(force bool, pending int, progress float64) {
	var (
		now     = mclock.Now()
		elapsed = time.Duration(now) - time.Duration(st.startTime)
	)

	if force || elapsed >= log.StatsReportLimit {
		st.totalRead += st.read
		st.totalCommitted += st.committed
		st.pending, st.progress = pending, progress

		progressStr := strconv.FormatFloat(st.progress, 'f', 4, 64)
		progressStr = strings.TrimRight(progressStr, "0")
		progressStr = strings.TrimRight(progressStr, ".") + "%"

		logger.Info("State migration progress",
			"progress", progressStr,
			"totalRead", st.totalRead, "totalCommitted", st.totalCommitted, "pending", st.pending,
			"read", st.read, "readElapsed", st.readElapsed, "processElapsed", st.processElapsed,
			"written", st.committed, "writeElapsed", st.writeElapsed,
			"elapsed", common.PrettyDuration(elapsed),
			"totalElapsed", time.Since(st.initialStartTime))

		st.read, st.committed = 0, 0
		st.startTime = now
	}
}

// ResumeMigration continues the migration of the state of a block, which was
// running when the node stopped, or removes the old state database left by
// a finished migration.
func (s *Manager) ResumeMigration() {
	if s.diskDB.InMigration() {
		number := s.diskDB.MigrationBlockNumber()

		header := s.cfg.HeaderAt(number)
		if header == nil {
			logger.Error("failed to get migration block number", "blockNumber", number)
			return
		}
		r := header.Root

		logger.Warn("State migration : Restarted", "blockNumber", number, "root", r.String())

		s.wg.Go(func() {
			s.migrate(r)
		})
	} else if dbPath := s.diskDB.MigrationOldDBPath(); dbPath != "" {
		// Remove the old database if it exists.
		// This can happen when the node is stopped while removing the old database.
		go s.diskDB.RemoveOldDB(dbPath, nil)
	}
}

// PrepareMigration starts the migration at the next block whose state is
// flushed to disk, after the head.
func (s *Manager) PrepareMigration() error {
	next := s.cfg.Head().Number.Uint64() + 1
	if s.diskDB.ReadPruningEnabled() {
		return errors.New("state migration not supported with live pruning enabled")
	}

	if s.diskDB.InMigration() || !s.FlushNext(s.startPreparedMigration) {
		return errors.New("migration already started")
	}

	logger.Info("State migration is prepared", "expectedMigrationStartingBlockNumber", next)

	return nil
}

// startPreparedMigration starts the prepared migration of the state of the
// block number with root, flushed to disk, and asks to flush the trie nodes
// in memory to the new database.
func (s *Manager) startPreparedMigration(number uint64, root common.Hash) bool {
	logger.Info("State migration is started", "block", number, "root", root)

	if err := s.StartMigration(number, root); err != nil {
		logger.Error("Failed to start state migration", "err", err)
	}
	return true
}

// StartMigration configures the database and starts the migration of the state of the block number with root.
func (s *Manager) StartMigration(number uint64, root common.Hash) error {
	if s.diskDB.InMigration() {
		return errors.New("migration already started")
	}

	if err := s.diskDB.CreateMigrationDBAndSetStatus(number); err != nil {
		return err
	}

	s.wg.Go(func() {
		s.migrate(root)
	})

	return nil
}

// StopMigration stops the running migration.
func (s *Manager) StopMigration() error {
	if !s.diskDB.InMigration() {
		return errors.New("not in migration")
	}

	s.migration.stop <- struct{}{}

	return nil
}

// MigrationStatus returns if it is in migration, the block number of in migration,
// number of committed blocks and number of pending blocks
func (s *Manager) MigrationStatus() (bool, uint64, int, int, int, float64, error) {
	m := &s.migration
	return s.diskDB.InMigration(), s.diskDB.MigrationBlockNumber(), m.readCnt, m.committedCnt, m.pendingCnt, m.progress, m.err
}

// trieWarmUp runs Iterator, generated from the given state or storage trie node hash,
// until it reaches end. If it reaches end, it will send a nil error to errCh to indicate that
// it has been finished.
func (s *Manager) trieWarmUp(next func() bool, resultCh chan int, errCh chan error) {
	var (
		resultErr    error
		reportTicker = time.NewTicker(DefaultWarmUpReportInterval)
		nReads       = 0
	)

	defer func() {
		resultCh <- nReads
		errCh <- resultErr
		reportTicker.Stop()
	}()

	for next() {
		select {
		case <-s.warmUp:
			return
		case <-s.quit:
			resultErr = stateStoppedErr
			return
		case <-reportTicker.C:
			resultCh <- nReads
			nReads = 1
		default:
			nReads++
		}
	}
}

// warmUpTrieCache receives errors from each warm-up goroutine.
// If it receives a nil error, it means a child goroutine is successfully terminated.
// It also periodically checks and logs warm-up progress.
func (s *Manager) warmUpTrieCache(mainTrieDB *statedb.Database, minLoad uint, nChildren int,
	resultCh chan int, errCh chan error,
) {
	defer func() { s.warmUp = nil }()

	cache := mainTrieDB.TrieNodeCache()
	mainTrieCacheLimit := mainTrieDB.GetTrieNodeLocalCacheByteLimit()
	if minLoad == 0 {
		minLoad = DefaultWarmUpMinLoad
	}

	var (
		resultErr   error
		started     = time.Now()
		logged      = time.Now()
		context     []interface{}
		progress    uint64
		trieReadCnt int
	)

	updateContext := func() {
		switch c := cache.(type) {
		case *statedb.FastCache:
			stats := c.UpdateStats().(fastcache.Stats)
			progress = stats.BytesSize * 100 / mainTrieCacheLimit
			context = []interface{}{
				"warmUpCnt", trieReadCnt,
				"cacheLimit", units.Base2Bytes(mainTrieCacheLimit).String(),
				"cachedSize", units.Base2Bytes(stats.BytesSize).String(),
				"progress", progress,
				"elapsed", time.Since(started),
			}
		default:
			context = []interface{}{
				"warmUpCnt", trieReadCnt,
				"cacheLimit", units.Base2Bytes(mainTrieCacheLimit).String(),
			}
		}
	}

	for childCnt := 0; childCnt < nChildren; {
		select {
		case nReadNodes := <-resultCh:
			trieReadCnt += nReadNodes

			if time.Since(logged) < log.StatsReportLimit {
				continue
			}
			logged = time.Now()
			updateContext()
			if progress > uint64(minLoad) { // more than 90%
				close(s.warmUp)
				logger.Info("Warm up is completed", context...)
				return
			}
			logger.Info("Warm up progress", context...)
		case err := <-errCh:
			// if errCh returns nil, it means success.
			if err != nil {
				resultErr = err
				logger.Warn("Warm up got an error", "err", err)
			}

			logger.Debug("Warm up a child trie is finished", "childCnt", childCnt, "err", err)
			childCnt++
		}
	}

	updateContext()
	context = append(context, "resultErr", resultErr)
	logger.Info("Warm up is completed", context...)
}

// StartWarmUp retrieves all state/storage tries of the last state flushed to
// disk and caches the tries.
func (s *Manager) StartWarmUp(minLoad uint) error {
	header, err := s.lastCommittedHeader()
	if err != nil {
		return err
	}
	number, root := header.Number.Uint64(), header.Root
	db, mainTrieDB, err := s.prepareWarmUp()
	if err != nil {
		return err
	}
	// retrieve children nodes of state trie root node
	children, err := db.TrieDB().NodeChildren(root.ExtendZero())
	if err != nil {
		return err
	}
	// run goroutine for each child node
	resultCh := make(chan int, len(children))
	errCh := make(chan error)
	s.warmUp = make(chan struct{})
	for idx, child := range children {
		childHash := child.Unextend()
		stateDB, err := New(childHash, db, nil, nil)
		if err != nil {
			logger.Warn("[WarmUp] Failed to get state",
				"rootHash", children, "childIdx", idx, "childHash", childHash.Hex())
			continue
		}
		it := NewNodeIterator(stateDB)
		go s.trieWarmUp(it.Next, resultCh, errCh)
	}
	// run a warm-up checker routine
	go s.warmUpTrieCache(mainTrieDB, minLoad, len(children), resultCh, errCh)
	logger.Info("State trie warm-up is started", "blockNum", number,
		"root", root.String(), "len(children)", len(children))
	return nil
}

// StopWarmUp stops the warming up process.
func (s *Manager) StopWarmUp() error {
	if s.warmUp == nil {
		return ErrNotInWarmUp
	}

	close(s.warmUp)

	return nil
}

// StartCollectingTrieStats collects the statistics of the last state trie
// flushed to disk, or of the storage trie of contractAddr in it.
func (s *Manager) StartCollectingTrieStats(contractAddr common.Address) error {
	header, err := s.lastCommittedHeader()
	if err != nil {
		return err
	}
	number, root := header.Number.Uint64(), header.Root
	cache := s.db.TrieDB().TrieNodeCache()
	if cache == nil {
		return fmt.Errorf("target cache is nil")
	}
	db := NewDatabaseWithExistingCache(s.diskDB, cache)

	startNode := root.ExtendZero()
	// If the contractAddr is given, start collecting stats from the root of storage trie
	if !common.EmptyAddress(contractAddr) {
		var err error
		startNode, err = ContractStorageRoot(root, db, contractAddr)
		if err != nil {
			logger.Error("Failed to get the contract storage root",
				"contractAddr", contractAddr.String(), "rootHash", root.String(),
				"err", err)
			return err
		}
	}

	children, err := db.TrieDB().NodeChildren(startNode)
	if err != nil {
		logger.Error("Failed to retrieve the children of start node", "err", err)
		return err
	}

	logger.Info("Started collecting trie statistics",
		"blockNum", number, "root", root.String(), "len(children)", len(children))
	go collectTrieStats(db, startNode)

	return nil
}

// collectChildrenStats wraps CollectChildrenStats, in order to send finish signal to resultCh.
func collectChildrenStats(db Database, child common.ExtHash, resultCh chan<- statedb.NodeInfo) {
	db.TrieDB().CollectChildrenStats(child, 2, resultCh)
	resultCh <- statedb.NodeInfo{Finished: true}
}

// collectTrieStats is the main function of collecting trie statistics.
// It spawns goroutines for the upper-most children and iterates each sub-trie.
func collectTrieStats(db Database, startNode common.ExtHash) {
	children, err := db.TrieDB().NodeChildren(startNode)
	if err != nil {
		logger.Error("Failed to retrieve the children of start node", "err", err)
	}

	// collecting statistics by running individual goroutines for each child
	resultCh := make(chan statedb.NodeInfo, 10000)
	for _, child := range children {
		go collectChildrenStats(db, child, resultCh)
	}

	numGoRoutines := len(children)
	ticker := time.NewTicker(1 * time.Minute)

	numNodes, numLeafNodes, maxDepth := 0, 0, 0
	depthCounter := make(map[int]int)
	begin := time.Now()
	for {
		select {
		case result := <-resultCh:
			if result.Finished {
				numGoRoutines--
				if numGoRoutines == 0 {
					logger.Info("Finished collecting trie statistics", "elapsed", time.Since(begin),
						"numNodes", numNodes, "numLeafNodes", numLeafNodes, "maxDepth", maxDepth)
					printDepthStats(depthCounter)
					return
				}
				continue
			}
			numNodes++
			// if a leaf node, collect the depth data
			if result.Depth != 0 {
				numLeafNodes++
				depthCounter[result.Depth]++
				if result.Depth > maxDepth {
					maxDepth = result.Depth
				}
			}
		case <-ticker.C:
			// leave a periodic log
			logger.Info("Collecting trie statistics is in progress...", "elapsed", time.Since(begin),
				"numGoRoutines", numGoRoutines, "numNodes", numNodes, "numLeafNodes", numLeafNodes, "maxDepth", maxDepth)
			printDepthStats(depthCounter)
		}
	}
}

// printDepthStats leaves logs containing the depth and the number of nodes in the depth.
func printDepthStats(depthCounter map[int]int) {
	// max depth 20 is set by heuristically
	for depth := 2; depth < 20; depth++ {
		if depthCounter[depth] == 0 {
			continue
		}
		logger.Info("number of leaf nodes in a depth",
			"depth", depth, "numNodes", depthCounter[depth])
	}
}

// ContractStorageRoot returns the storage root of contractAddr in the state
// with root.
func ContractStorageRoot(root common.Hash, db Database, contractAddr common.Address) (common.ExtHash, error) {
	stateDB, err := New(root, db, nil, nil)
	if err != nil {
		return common.ExtHash{}, fmt.Errorf("failed to get StateDB - %w", err)
	}
	return stateDB.GetContractStorageRoot(contractAddr)
}

// prepareWarmUp creates and returns resources needed for state warm-up.
func (s *Manager) prepareWarmUp() (Database, *statedb.Database, error) {
	// There is a chance of concurrent access to warmUp, though not likely to happen.
	if s.warmUp != nil {
		return nil, nil, fmt.Errorf("already warming up")
	}

	mainTrieDB := s.db.TrieDB()
	cache := mainTrieDB.TrieNodeCache()
	if cache == nil {
		return nil, nil, fmt.Errorf("target cache is nil")
	}
	db := NewDatabaseWithExistingCache(s.diskDB, cache)
	return db, mainTrieDB, nil
}

func prepareContractWarmUp(root common.Hash, db Database, contractAddr common.Address) (common.ExtHash, Trie, error) {
	stateDB, err := New(root, db, nil, nil)
	if err != nil {
		return common.ExtHash{}, nil, fmt.Errorf("failed to get StateDB, err: %w", err)
	}
	storageTrieRoot, err := stateDB.GetContractStorageRoot(contractAddr)
	if err != nil {
		return common.ExtHash{}, nil, err
	}
	storageTrie, err := db.OpenStorageTrie(contractAddr, storageTrieRoot, nil)
	if err != nil {
		return common.ExtHash{}, nil, err
	}
	return storageTrieRoot, storageTrie, nil
}

// StartContractWarmUp retrieves the storage trie of contractAddr in the last
// state flushed to disk and caches the trie.
func (s *Manager) StartContractWarmUp(contractAddr common.Address, minLoad uint) error {
	header, err := s.lastCommittedHeader()
	if err != nil {
		return err
	}
	number, root := header.Number.Uint64(), header.Root
	db, mainTrieDB, err := s.prepareWarmUp()
	if err != nil {
		return err
	}
	// prepare contract storage trie specific resources - storageTrieRoot and storageTrie
	storageTrieRoot, storageTrie, err := prepareContractWarmUp(root, db, contractAddr)
	if err != nil {
		return fmt.Errorf("failed to prepare contract warm-up, err: %w", err)
	}
	// retrieve children nodes of contract storage trie root node
	children, err := db.TrieDB().NodeChildren(storageTrieRoot)
	if err != nil {
		return err
	}
	// run goroutine for each child node
	resultCh := make(chan int, len(children))
	errCh := make(chan error)
	s.warmUp = make(chan struct{})
	for _, child := range children {
		it := statedb.NewIterator(storageTrie.NodeIterator(child[:]))
		go s.trieWarmUp(it.Next, resultCh, errCh)
	}
	// run a warm-up checker routine
	go s.warmUpTrieCache(mainTrieDB, minLoad, len(children), resultCh, errCh)
	logger.Info("Contract storage trie warm-up is started",
		"blockNum", number, "root", root.String(), "contractAddr", contractAddr.String(),
		"contractStorageRoot", storageTrieRoot.String(), "len(children)", len(children))
	return nil
}

// lastCommittedHeader returns the header of the last block whose state was
// flushed to disk.
func (s *Manager) lastCommittedHeader() (*types.Header, error) {
	number := s.LastCommitted()
	header := s.cfg.HeaderAt(number)
	if header == nil {
		return nil, fmt.Errorf("block #%d not found", number)
	}
	return header, nil
}
