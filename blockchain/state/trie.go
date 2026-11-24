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
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/snapshot"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/rcrowley/go-metrics"
)

var (
	trieDBNodesSizeBytesGauge = metrics.NewRegisteredGauge("klay/triedb/nodessizebytes", nil)
	trieDBPreimagesSizeGauge  = metrics.NewRegisteredGauge("klay/triedb/preimagessizebytes", nil)
)

// TrieConfig is how a Manager keeps the trie nodes of the states of the blocks a chain
// writes.
type TrieConfig struct {
	// Archive flushes the state of every block to disk and never garbage
	// collects or prunes it.
	Archive bool
	// CacheSize is the memory, in MB, the trie nodes not yet on disk may take
	// before the oldest are flushed.
	CacheSize int
	// TriesInMemory is how many of the last block states stay in memory.
	TriesInMemory uint64
	// LivePruningRetention is how many blocks the trie nodes a block made
	// obsolete are kept, with live pruning. Zero keeps them.
	LivePruningRetention uint64
	// SnapshotCacheSize is the memory, in MB, of the snapshot. Zero disables
	// the snapshot.
	SnapshotCacheSize int
	// SnapshotAsyncGen generates the snapshot in the background.
	SnapshotAsyncGen bool

	// BlockInterval is how often the state of a block is flushed to disk,
	// unless the node is an archive, and also at the staking update interval of
	// ChainConfig with the weighted random proposer policy.
	BlockInterval uint
	ChainConfig   *params.ChainConfig
}

// FlushHook runs when the state of block number with root is flushed to disk,
// and reports whether the trie nodes in memory are flushed too.
type FlushHook func(number uint64, root common.Hash) (flushAll bool)

type gcBlock struct {
	root     common.Hash
	blockNum uint64
	rewound  bool // the chain is rewound to blockNum: the states after it go
}

// Snapshots returns the snapshot tree, nil if the snapshot is disabled.
func (s *Manager) Snapshots() *snapshot.Tree {
	return s.snaps
}

// OpenSnapshots loads the snapshot of the state of head, regenerating it if
// loading fails. If the chain was rewound past the snapshot on disk, which
// stored a recovery block number, the snapshot ahead of head is kept.
func (s *Manager) OpenSnapshots(head *types.Header) {
	if s.cfg.SnapshotCacheSize == 0 {
		return
	}
	var recovery bool
	if layer := s.diskDB.ReadSnapshotRecoveryNumber(); layer != nil && *layer > head.Number.Uint64() {
		logger.Warn("Enabling snapshot recovery", "chainhead", head.Number, "diskbase", *layer)
		recovery = true
	}
	s.snaps, _ = snapshot.New(s.diskDB, s.TrieDB(), s.cfg.SnapshotCacheSize, head.Root, s.cfg.SnapshotAsyncGen, true, recovery)
}

// RebuildSnapshots drops the snapshot and regenerates it for the state with
// root in the background.
func (s *Manager) RebuildSnapshots(root common.Hash) {
	if s.snaps != nil {
		s.snaps.Rebuild(root)
	}
}

// journalSnapshots writes the snapshot layers above the state with root to
// disk and releases the snapshot. It returns the root of the state the
// snapshot on disk is based on, if any.
func (s *Manager) journalSnapshots(root common.Hash) common.Hash {
	if s.snaps == nil {
		return common.Hash{}
	}
	base, err := s.snaps.Journal(root)
	if err != nil {
		logger.Error("Failed to journal state snapshot", "err", err)
	}
	s.snaps.Release()
	return base
}

// CommitRequired reports whether the state of the block number is flushed to
// disk, unless the node is an archive.
func (s *Manager) CommitRequired(number uint64) bool {
	if s.onFlush.Load() != nil {
		return true
	}
	// TODO-Klaytn-Issue1602 Introduce a simple and more concise way to determine commit trie requirements from governance
	if s.cfg.BlockInterval > 0 && number%uint64(s.cfg.BlockInterval) == 0 {
		return true
	}
	if c := s.cfg.ChainConfig; c != nil && c.Istanbul != nil {
		return c.Istanbul.ProposerPolicy == params.WeightedRandom &&
			number%c.Governance.Reward.StakingUpdateInterval == 0
	}
	return false
}

// CommitInterval returns the interval of the blocks whose states are flushed
// to disk: 1 for an archive.
func (s *Manager) CommitInterval() uint64 {
	if s.cfg.Archive || s.cfg.BlockInterval == 0 {
		return 1
	}
	return uint64(s.cfg.BlockInterval)
}

// FlushNext makes the state of the next committed block flushed to disk, and
// runs hook then. It fails if a hook waits already.
func (s *Manager) FlushNext(hook FlushHook) bool {
	return s.onFlush.CompareAndSwap(nil, &hook)
}

// runFlushHook runs the hook waiting for the flush of block number with root,
// if any, and reports whether it asks to flush the trie nodes in memory.
func (s *Manager) runFlushHook(number uint64, root common.Hash) bool {
	hook := s.onFlush.Swap(nil)
	return hook != nil && (*hook)(number, root)
}

// LastCommitted returns the last block whose state was flushed to disk.
func (s *Manager) LastCommitted() uint64 {
	return s.lastCommitted.Load()
}

// SetLastCommitted sets the last block whose state is on disk, like when the
// head is set.
func (s *Manager) SetLastCommitted(number uint64) {
	s.lastCommitted.Store(number)
}

// CheckRewind fails for a rewind to a block whose state live pruning deleted:
// looking for a state at or before it may loop until the genesis block. A
// history node builds the state of a block its BAL index covers.
func (s *Manager) CheckRewind(head uint64) error {
	if s.bals.Indexes() {
		if base, next, ok := s.bals.Indexed(); ok && base <= head && head < next {
			return nil
		}
	}
	if lastPruned, err := s.diskDB.ReadLastPrunedBlockNumber(); err == nil && head <= lastPruned {
		return fmt.Errorf("[SetHead] Cannot rewind to a state-pruned block number. lastPrunedBlock=%d targetHead=%d",
			lastPruned, head)
	}
	return nil
}

// HasTrie reports whether the state trie with the given root is stored, so
// that blocks can be executed on it.
func (s *Manager) HasTrie(root common.Hash) bool {
	_, err := s.db.OpenTrie(root, nil)
	return err == nil
}

// rewindTrie returns the highest header at or below target whose state trie
// is stored. With a limit root, it goes below the block with that root too and
// returns the number of that block.
func (s *Manager) rewindTrie(target *types.Header, limit common.Hash) (*types.Header, uint64, error) {
	var limitNumber uint64
	beyond := limit == (common.Hash{})
	h := target
	for {
		n := h.Number.Uint64()
		if !beyond && h.Root == limit {
			beyond, limitNumber = true, n
		}
		if n == 0 || beyond && s.HasTrie(h.Root) {
			break
		}
		parent := s.cfg.Header(h.ParentHash, n-1)
		if parent == nil {
			logger.Error("Missing block in the middle, aiming genesis", "number", n-1, "hash", h.ParentHash)
			parent = s.cfg.HeaderAt(0)
		}
		h = parent
	}
	if h.Number.Uint64() == 0 && !s.HasTrie(h.Root) {
		return nil, 0, errors.New("rewound to block number 0, but repair failed")
	}
	return h, limitNumber, nil
}

// LivePruning reports whether the trie nodes made obsolete are pruned.
func (s *Manager) LivePruning() bool {
	return s.diskDB.ReadPruningEnabled() && s.cfg.LivePruningRetention != 0
}

// commitRoot takes root, the state of block number committed to the trie
// database, and flushes it to disk if the node is an archive or the block
// requires it.
func (s *Manager) commitRoot(number uint64, root common.Hash) error {
	trieDB := s.TrieDB()
	trieDB.UpdateMetricNodes()

	// If we're running an archive node, always flush
	if s.cfg.Archive {
		if err := trieDB.Commit(root, false, number); err != nil {
			return err
		}

		s.runFlushHook(number, root)
		s.lastCommitted.Store(number)

		if s.LivePruning() {
			s.chPrune <- pruneRequest{number: number, gen: s.pruning.gen.Load()}
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

	if s.CommitRequired(number) {
		if err := trieDB.Commit(root, true, number); err != nil {
			return err
		}
		logger.Trace("Committed the state trie into the disk", "blocknum", number)

		if s.runFlushHook(number, root) {
			if err := trieDB.Cap(0); err != nil {
				logger.Error("Error from trieDB.Cap by state migration", "err", err)
			}
		}
		s.lastCommitted.Store(number)

		if s.LivePruning() {
			s.chPrune <- pruneRequest{number: number, gen: s.pruning.gen.Load()}
		}

		if dm := trieDB.DiskDB().GetDomainsManager(); dm != nil {
			if err := dm.CommitWrites(); err != nil {
				return err
			}
			logger.Info("Committed domains manager changes into the disk", "blocknum", number)
		}
	}

	s.chBlock <- gcBlock{root: root, blockNum: number}
	return nil
}

// flush flushes the state of the head block and the state the snapshot is
// based on, if any, to disk, and drops the states in memory.
func (s *Manager) flush(head *types.Block, snapBase common.Hash) {
	triedb := s.TrieDB()
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

// dropTriesAfter dereferences the states in memory of the blocks after number,
// which a rewind deleted. Blocks inserted again have the same roots, which
// would otherwise be dereferenced once more than referenced.
func (s *Manager) dropTriesAfter(number uint64) {
	var kept []gcBlock
	for !s.triegc.Empty() {
		root, n := s.triegc.Pop()
		if uint64(-n) > number {
			s.TrieDB().Dereference(root.(common.Hash))
		} else {
			kept = append(kept, gcBlock{root: root.(common.Hash), blockNum: uint64(-n)})
		}
	}
	for _, b := range kept {
		s.triegc.Push(b.root, -int64(b.blockNum))
	}
}

// gcLoop dereferences the states that leave memory.
func (s *Manager) gcLoop() {
	trieDB := s.TrieDB()

	s.wg.Go(func() {
		for {
			select {
			case block := <-s.chBlock:
				if block.rewound {
					s.dropTriesAfter(block.blockNum)
					continue
				}
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

// pruneRequest asks the pruning loop to prune after block number was flushed.
type pruneRequest struct {
	number uint64
	gen    uint64 // the generation of the pruning, see rewindPruning
}

// pruning is where the pruning loop is, guarded by mu.
type pruning struct {
	mu   sync.Mutex
	next uint64        // the first block whose marks are not pruned yet
	gen  atomic.Uint64 // counts the rewinds, which void the requests made before
	last atomic.Uint64 // the last pruned block, as stored
}

// lastPruned returns the last block whose marks were pruned, 0 if none.
func (s *Manager) lastPruned() uint64 {
	return s.pruning.last.Load()
}

func (s *Manager) setLastPruned(number uint64) {
	s.diskDB.WriteLastPrunedBlockNumber(number)
	s.pruning.last.Store(number)
}

// pruneLoop deletes the trie nodes marked obsolete by blocks older than the
// retention.
func (s *Manager) pruneLoop() {
	s.pruning.mu.Lock()
	s.pruning.next = 1 // ReadPruningMarks(1, limit) iterates over most of MiscDB, so the range is kept small
	s.pruning.mu.Unlock()

	s.wg.Go(func() {
		for {
			select {
			case req := <-s.chPrune:
				s.prune(req)
			case <-s.quit:
				return
			}
		}
	})
}

func (s *Manager) prune(req pruneRequest) {
	p := &s.pruning
	p.mu.Lock()
	defer p.mu.Unlock()
	if req.gen != p.gen.Load() || req.number <= s.cfg.LivePruningRetention {
		return
	}
	limit := req.number - s.cfg.LivePruningRetention // Prune [1, latest - retention]

	startTime := time.Now()
	count := s.diskDB.PruneMarkedTrieNodes(p.next, limit+1, func(marks []database.PruningMark) {
		s.pruneTrieNodes(marks, limit)
	})
	s.setLastPruned(limit)

	logger.Info("Pruned trie nodes", "number", req.number, "start", p.next, "limit", limit,
		"count", count, "elapsed", time.Since(startTime))

	p.next = limit + 1
}

// rewindPruning drops the pruning marks of the blocks from number on, which a
// rewind to block number deleted. The marks of block n are the nodes of its
// trie that block n+1 made obsolete, so those of the new head name nodes its
// trie still has, which other blocks after it may keep. The pruning goes on
// from number, and the requests made before the rewind are dropped.
func (s *Manager) rewindPruning(number uint64) {
	if !s.LivePruning() {
		return
	}
	p := &s.pruning
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen.Add(1)
	s.TrieDB().DropPruningMarksFrom(number)
	s.diskDB.DeletePruningMarksFrom(number)
	if s.lastPruned() >= number {
		s.setLastPruned(max(number, 1) - 1)
	}
	p.next = min(max(p.next, 1), max(number, 1))
}

// TrieNode returns the trie node with the given hash, from memory or disk.
// Trie nodes keyed with an ExtHash cannot be retrieved.
func (s *Manager) TrieNode(hash common.Hash) ([]byte, error) {
	return s.TrieDB().Node(hash.ExtendZero())
}

// SaveTrieNodeCacheToDisk saves the trie node cache to its file in the
// background.
func (s *Manager) SaveTrieNodeCacheToDisk() error {
	trieDB := s.TrieDB()
	if err := trieDB.CanSaveTrieNodeCacheToFile(); err != nil {
		return err
	}
	go trieDB.SaveTrieNodeCacheToFile(trieDB.GetTrieNodeCacheConfig().FastCacheFileDir, runtime.NumCPU()/2)
	return nil
}

// A history node keeps its state with live pruning: a block replacing a trie
// node marks it with the number of its parent block, and the node is deleted
// once the head is the retention period past the mark. Every node but the root
// of the account trie gets a fresh nonce from a counter that only grows, so a
// node is never written twice and the trie of a past block can be built back
// into the trie database next to the live one: it takes the stored trie of a
// later block and sets back what the lists in between changed. The built trie
// shares the nodes the lists left alone with the trie it was built from, and
// those are kept from pruning: a node marked at that block or later whose
// nonce is not above the counter when the build started was in its trie. The
// nodes without a nonce, of the genesis state, are kept too, but for the roots
// of later blocks.

var keptTriesKey = []byte("bal-kept-tries")

// KeptTrie is the state trie of block Number, which shares the nodes of the
// stored trie of block Height that have a nonce up to Counter.
type KeptTrie struct {
	Number  uint64
	Root    common.Hash
	Height  uint64
	Counter uint64
}

// keptTries keeps built state tries from live pruning.
type keptTries struct {
	mu     sync.Mutex // serialises pruning with keeping tries
	loaded bool
	kept   []KeptTrie
	pruned uint64 // last block whose marked nodes were deleted
}

func (t *keptTries) loadLocked(db database.DBManager) {
	if t.loaded {
		return
	}
	t.loaded = true
	t.pruned, _ = db.ReadLastPrunedBlockNumber()
	enc, err := db.GetMiscDB().Get(keptTriesKey)
	if err != nil || len(enc) == 0 {
		return
	}
	if err := rlp.DecodeBytes(enc, &t.kept); err != nil {
		logger.Error("Failed to read the kept state tries, live pruning may delete their nodes", "err", err)
	}
}

func (t *keptTries) storeLocked(db database.DBManager) error {
	enc, err := rlp.EncodeToBytes(t.kept)
	if err != nil {
		return err
	}
	return db.GetMiscDB().Put(keptTriesKey, enc)
}

// KeepTrie starts keeping k from live pruning, replacing a kept trie of the
// same block, and returns the last block whose marked nodes were deleted.
func (s *Manager) KeepTrie(k KeptTrie) (uint64, error) {
	t := &s.kept
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked(s.diskDB)
	kept := []KeptTrie{k}
	for _, o := range t.kept {
		if o.Number != k.Number {
			kept = append(kept, o)
		}
	}
	t.kept = kept
	return t.pruned, t.storeLocked(s.diskDB)
}

// DropKeptTrie stops keeping the trie of block number.
func (s *Manager) DropKeptTrie(number uint64) {
	t := &s.kept
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked(s.diskDB)
	kept := t.kept[:0:0]
	for _, o := range t.kept {
		if o.Number != number {
			kept = append(kept, o)
		}
	}
	t.kept = kept
	if err := t.storeLocked(s.diskDB); err != nil {
		logger.Error("Failed to store the kept state tries", "err", err)
	}
}

// KeptTries returns the kept state tries.
func (s *Manager) KeptTries() []KeptTrie {
	t := &s.kept
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked(s.diskDB)
	return append([]KeptTrie(nil), t.kept...)
}

// KeptTrie returns the kept state trie of block number.
func (s *Manager) KeptTrie(number uint64) (KeptTrie, bool) {
	for _, k := range s.KeptTries() {
		if k.Number == number {
			return k, true
		}
	}
	return KeptTrie{}, false
}

// ExtCounter returns the nonce counter of an extended hash.
func ExtCounter(h common.ExtHash) uint64 {
	var b [8]byte
	c := h.Counter()
	copy(b[1:], c[:])
	return binary.BigEndian.Uint64(b[:])
}

// pruneTrieNodes deletes the trie nodes of marks of blocks up to limit, but
// those of kept tries.
func (s *Manager) pruneTrieNodes(marks []database.PruningMark, limit uint64) {
	t := &s.kept
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked(s.diskDB)
	if len(t.kept) > 0 || s.bals != nil {
		marks = s.unkeptLocked(marks)
	}
	s.diskDB.PruneTrieNodes(marks)
	t.pruned = limit
}

// unkeptLocked returns the marks whose nodes no kept trie holds, nor the
// genesis state of a history node.
func (s *Manager) unkeptLocked(marks []database.PruningMark) []database.PruningMark {
	t := &s.kept
	roots := make(map[common.Hash]bool, len(t.kept))
	for _, k := range t.kept {
		roots[k.Root] = true
	}
	var (
		out        = make([]database.PruningMark, 0, len(marks))
		rootNumber = ^uint64(0)
		blockRoot  common.Hash
		keptNodes  int
	)
	// rootOf returns the state root of block number, the root of the trie its
	// marks are of.
	rootOf := func(number uint64) common.Hash {
		if rootNumber != number {
			rootNumber, blockRoot = number, common.Hash{}
			if h := s.diskDB.ReadHeader(s.diskDB.ReadCanonicalHash(number), number); h != nil {
				blockRoot = h.Root
			}
		}
		return blockRoot
	}
	for _, m := range marks {
		if m.Hash.IsZeroExtended() && roots[m.Hash.Unextend()] {
			keptNodes++
			continue
		}
		// A history node keeps the genesis state, which its BAL history may
		// start from: the nodes of the genesis trie and the nodes without a
		// nonce, which only the genesis state and the roots of the blocks
		// have.
		if s.bals != nil && (m.Number == 0 || m.Hash.IsZeroExtended() && m.Hash.Unextend() != rootOf(m.Number)) {
			keptNodes++
			continue
		}
		var (
			counter uint64
			shared  bool
		)
		for _, k := range t.kept {
			if m.Number >= k.Height {
				shared, counter = true, max(counter, k.Counter)
			}
		}
		keep := false
		switch {
		case !shared:
		case m.Hash.IsZeroExtended():
			keep = m.Hash.Unextend() != rootOf(m.Number)
		default:
			keep = ExtCounter(m.Hash) <= counter
		}
		if keep {
			keptNodes++
		} else {
			out = append(out, m)
		}
	}
	if keptNodes > 0 {
		logger.Debug("Kept trie nodes of built state tries from pruning", "nodes", keptNodes)
	}
	return out
}

// TrieProgress follows BuildTrieBack. Each field may be nil.
type TrieProgress struct {
	// Stopped stops the build when it returns an error.
	Stopped func() error
	// Stage is called as the build reads the lists, the values and applies them.
	Stage func(stage string)
	// Lists is called after the list of each later block was read.
	Lists func(blocks uint64, accounts, slots int)
	// Values is called as the values at the block are read.
	Values func(done int)
}

func (p *TrieProgress) stopped() error {
	if p == nil || p.Stopped == nil {
		return nil
	}
	return p.Stopped()
}

func (p *TrieProgress) stage(stage string) {
	if p != nil && p.Stage != nil {
		p.Stage(stage)
	}
}

// trieChange is what the lists after a block changed in an account.
type trieChange struct {
	reset bool // deleted or recreated
	slots map[common.Hash]struct{}
}

// BuildTrieBack builds the state trie of block number in trie from the trie at
// root of the later block height: it sets every account and slot the lists of
// the blocks in between changed back to its value at the block, read from the
// BAL history, and returns the root of the trie, which the caller checks and
// commits.
func (s *Manager) BuildTrieBack(trie Database, root common.Hash, height, number uint64, p *TrieProgress) (common.Hash, error) {
	p.stage("lists")
	changes := make(map[common.Address]*trieChange)
	slots := 0
	for n := number + 1; n <= height; n++ {
		if err := p.stopped(); err != nil {
			return common.Hash{}, err
		}
		list, err := s.diskDB.ReadCanonicalBALList(n)
		if err != nil {
			return common.Hash{}, fmt.Errorf("block %d: %w", n, err)
		}
		for i := range list.Accounts {
			a := &list.Accounts[i]
			c := changes[a.Address]
			if c == nil {
				c = &trieChange{slots: make(map[common.Hash]struct{})}
				changes[a.Address] = c
			}
			c.reset = c.reset || len(a.Deleted) > 0 || len(a.StorageCleared) > 0
			for _, w := range a.StorageChanges {
				if _, ok := c.slots[w.Slot]; !ok {
					c.slots[w.Slot] = struct{}{}
					slots++
				}
			}
		}
		if p != nil && p.Lists != nil {
			p.Lists(n-number, len(changes), slots)
		}
	}

	ix, valid, err := s.bals.Index()
	if err != nil {
		return common.Hash{}, err
	}
	if meta := ix.Meta(); number < meta.Base || height >= meta.Next {
		return common.Hash{}, fmt.Errorf("blocks %d-%d are not covered by the BAL index", number, height)
	}
	sdb, err := New(root, trie, nil, nil)
	if err != nil {
		return common.Hash{}, err
	}
	addrs := slices.SortedFunc(maps.Keys(changes), func(a, b common.Address) int { return bytes.Compare(a[:], b[:]) })

	p.stage("values")
	back := &types.BlockAccessList{Accounts: make([]types.AccountChanges, 0, len(addrs))}
	for i, addr := range addrs {
		if err := p.stopped(); err != nil {
			return common.Hash{}, err
		}
		a, err := backAccess(s.bals, sdb, addr, number, changes[addr])
		if err != nil {
			return common.Hash{}, fmt.Errorf("account %s: %w", addr.Hex(), err)
		}
		back.Accounts = append(back.Accounts, *a)
		if p != nil && p.Values != nil && i%1024 == 0 {
			p.Values(i)
		}
	}
	if !valid() {
		return common.Hash{}, ErrRebuilt
	}
	if p != nil && p.Values != nil {
		p.Values(len(addrs))
	}
	p.stage("apply")
	if err := sdb.ApplyBAL(back); err != nil {
		return common.Hash{}, err
	}
	return sdb.Commit(true)
}

// backAccess returns the changes that set addr in the state of a later block
// back to its value at the end of block number.
func backAccess(h *BALHistory, later *StateDB, addr common.Address, number uint64, c *trieChange) (*types.AccountChanges, error) {
	acc, err := h.Account(addr, number)
	if err != nil {
		return nil, err
	}
	a := &types.AccountChanges{Address: addr}
	if acc == nil {
		a.Deleted = []uint32{0}
		return a, nil
	}
	enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(acc))
	if err != nil {
		return nil, err
	}
	a.AccountRecords = []types.AccountRecord{{BlockAccessIndex: 0, Account: enc}}
	pa := account.GetProgramAccount(acc)
	laterProgram := later.IsProgramAccount(addr)
	if pa == nil {
		if laterProgram {
			a.StorageCleared = []uint32{0}
		}
		return a, nil
	}
	if !laterProgram || !bytes.Equal(later.GetCodeHash(addr).Bytes(), pa.GetCodeHash()) {
		code, err := h.Code(addr, number)
		if err != nil {
			return nil, err
		}
		a.CodeChanges = []types.CodeChange{{BlockAccessIndex: 0, NewCode: code}}
	}
	var slots []common.Hash
	if !laterProgram || c.reset {
		a.StorageCleared = []uint32{0}
		if slots, err = h.Slots(addr, number); err != nil {
			return nil, err
		}
	} else {
		for slot := range c.slots {
			slots = append(slots, slot)
		}
	}
	sort.Slice(slots, func(i, j int) bool { return bytes.Compare(slots[i][:], slots[j][:]) < 0 })
	for i, slot := range slots {
		if i > 0 && slots[i-1] == slot {
			continue
		}
		value, err := h.Storage(addr, slot, number)
		if err != nil {
			return nil, err
		}
		if value == (common.Hash{}) && len(a.StorageCleared) > 0 {
			continue
		}
		a.StorageChanges = append(a.StorageChanges, types.SlotChanges{Slot: slot, Changes: []types.StorageChange{{BlockAccessIndex: 0, NewValue: value}}})
	}
	return a, nil
}

// buildTrieForward builds the state trie of block number in trie from the
// trie at root of the earlier block from, applying the lists of the blocks in
// between, and returns the root of the trie, which the caller checks and
// commits.
func (s *Manager) buildTrieForward(trie Database, root common.Hash, from, number uint64) (common.Hash, error) {
	sdb, err := New(root, trie, nil, nil)
	if err != nil {
		return common.Hash{}, err
	}
	for n := from + 1; n <= number; n++ {
		list, err := s.diskDB.ReadCanonicalBALList(n)
		if err != nil {
			return common.Hash{}, fmt.Errorf("block %d: %w", n, err)
		}
		if err := sdb.ApplyBALUnchecked(list); err != nil {
			return common.Hash{}, fmt.Errorf("block %d: %w", n, err)
		}
	}
	sdb.HashStorages(runtime.NumCPU())
	if err := sdb.Error(); err != nil {
		return common.Hash{}, err
	}
	return sdb.Commit(true)
}

// buildTrie builds the state trie of header from the stored lists and the
// closest stored trie: forward from an earlier one, applying the lists in
// between, or back from a later one through the BAL index.
func (s *Manager) buildTrie(header *types.Header) error {
	number := header.Number.Uint64()
	// The trie to start from is whole, stored or in memory, see unpruned.
	pruned := s.lastPruned()
	whole := func(n uint64) *types.Header {
		if !s.unpruned(n) {
			return nil
		}
		if h := s.cfg.HeaderAt(n); h != nil && s.HasTrie(h.Root) {
			return h
		}
		return nil
	}
	var later *types.Header
	if base, next, ok := s.bals.Indexed(); ok && base <= number {
		for n := max(number+1, pruned+1); n < next && later == nil; n++ {
			later = whole(n)
		}
	}
	var earlier *types.Header
	if first, next := s.bals.Range(); first <= number && number < next {
		lowest := first - min(first, 1)
		if pruned > 0 {
			lowest = max(lowest, pruned+1)
		}
		if later != nil {
			lowest = max(lowest, number-min(number, later.Number.Uint64()-number))
		}
		for n := number; n > lowest && earlier == nil; n-- {
			earlier = whole(n - 1)
		}
	}

	trie := s.db
	var (
		built common.Hash
		err   error
	)
	switch {
	case earlier != nil:
		built, err = s.buildTrieForward(trie, earlier.Root, earlier.Number.Uint64(), number)
	case later != nil:
		built, err = s.BuildTrieBack(trie, later.Root, later.Number.Uint64(), number, nil)
	default:
		return fmt.Errorf("no whole state trie near block %d to build it from", number)
	}
	if err != nil {
		return err
	}
	if built != header.Root {
		return fmt.Errorf("built state root %x, block %d has %x", built, number, header.Root)
	}
	return trie.TrieDB().Commit(built, true, number)
}
