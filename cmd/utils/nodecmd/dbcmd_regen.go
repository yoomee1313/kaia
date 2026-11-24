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

package nodecmd

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/hashchain"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
)

// Sources and modes of a BAL regeneration.
const (
	regenLocal   = "local"   // execute from the stored states of this node
	regenGenesis = "genesis" // execute every block from the genesis state
	regenImport  = "import"  // apply the lists stored by another node, checking every root

	regenWrite  = "write"  // store the lists: their writes, and their reads unless skipped
	regenVerify = "verify" // compare the writes of the lists with the stored ones
	regenReads  = "reads"  // store the reads of blocks whose writes are stored, executing them again
)

const (
	defaultRegenUnit     = 128     // blocks of a work interval
	minRegenWorkerMemory = 1 << 29 // 512 MiB: trie cache and the state changes of an interval
)

var (
	errNoBALHistory = errors.New("the chain keeps no BAL history")
	errRegenStopped = errors.New("BAL regeneration stopped")
)

// blocks between commits of the temporary state of a source from genesis
var regenCommitInterval uint64 = 2048

// regenConfig configures a regeneration of the block access lists of blocks
// From to To.
type regenConfig struct {
	From    uint64 // 0: the block after the stored lists
	To      uint64 // 0: the head
	Source  string
	Mode    string
	Memory  uint64 // bytes for the workers; 0: 4 GiB
	Workers int    // 0: from the memory and the cores
	Step    uint64 // blocks between stored states; 0: the trie commit interval, 1 on an archive node

	SkipReads bool // write mode stores the writes only

	ImportDir string              // chaindata of another node to import the lists from, for the import source
	ImportDB  database.DBManager  // database to import from instead of ImportDir
	Genesis   *blockchain.Genesis // genesis to start from; nil: the one of the chain

	// The signed pre-Amber BAL hash chain, a file or the directory of its
	// package, checks every segment of imported lists before they are
	// applied; an import stops at its last block.
	HashChainFile string
	HashChain     *hashchain.Chain // chain to check against instead of HashChainFile
}

// regenStatus reports a regeneration.
type regenStatus struct {
	Source       string
	Mode         string
	From         uint64
	To           uint64
	Next         uint64 // next block to commit
	Workers      int
	Intervals    int
	Done         int
	BlocksPerSec float64
	Mismatches   uint64
	Remaining    string
	HeapBytes    uint64
	Error        string
}

type regen struct {
	c        *balTools
	cfg      regenConfig
	dir      string // interval files
	quit     chan struct{}
	heapBase uint64 // heap in use before the regeneration

	mu        sync.Mutex
	status    regenStatus
	started   time.Time
	committed uint64 // blocks committed since started
}

type regenInterval struct {
	index    int
	from, to uint64 // lists of blocks from+1 to to
	path     string
	err      error
}

// regen regenerates block access lists, logging its progress, and returns
// how it ended. The error is that of a configuration that does not run;
// the status holds the error the regeneration stopped with.
func (c *balTools) regen(cfg regenConfig) (regenStatus, error) {
	if c.bc.States().BALs() == nil {
		return regenStatus{}, errNoBALHistory
	}
	if cfg.Source == "" {
		cfg.Source = regenLocal
	}
	if cfg.Mode == "" {
		cfg.Mode = regenWrite
	}
	imported := cfg.Source == regenImport
	fromGenesis := cfg.Source == regenGenesis || imported
	if cfg.Source != regenLocal && !fromGenesis {
		return regenStatus{}, fmt.Errorf("unknown source %q", cfg.Source)
	}
	if cfg.Source == regenImport && cfg.ImportDir == "" && cfg.ImportDB == nil {
		return regenStatus{}, errors.New("the import source needs the chaindata to import from")
	}
	if imported && cfg.HashChainFile == "" && cfg.HashChain == nil {
		return regenStatus{}, fmt.Errorf("the %s source checks the lists of another node against the signed BAL hash chain", cfg.Source)
	}
	if cfg.HashChainFile != "" || cfg.HashChain != nil {
		if !imported {
			return regenStatus{}, errors.New("a BAL hash chain checks imported lists, for the import source")
		}
		if cfg.HashChain == nil {
			chain, err := state.BALHashChain.LoadSigned(cfg.HashChainFile, c.bc.Genesis().Hash(), state.HashChainPublicKey)
			if err != nil {
				return regenStatus{}, err
			}
			cfg.HashChain = chain
		}
	}
	if cfg.Mode != regenWrite && cfg.Mode != regenVerify && cfg.Mode != regenReads {
		return regenStatus{}, fmt.Errorf("unknown mode %q", cfg.Mode)
	}
	if cfg.Mode == regenReads && imported {
		return regenStatus{}, errors.New("reads mode executes the blocks again, from stored states or genesis")
	}
	cfg.SkipReads = cfg.SkipReads || c.bc.States().BALs().SkipsReads()
	if cfg.Memory == 0 {
		cfg.Memory = 4 << 30
	}
	if cfg.Step == 0 {
		cfg.Step = c.states.CommitInterval()
	}
	if cfg.Workers <= 0 {
		cfg.Workers = max(1, min(runtime.NumCPU()/2, int(cfg.Memory/minRegenWorkerMemory)))
	}

	first, next := c.bc.States().BALs().Range()
	empty := first == next
	if cfg.Mode == regenWrite {
		if cfg.From == 0 {
			cfg.From = max(next, 1)
			if fromGenesis {
				cfg.From = 1
			}
		}
		if cfg.To == 0 {
			cfg.To = c.bc.CurrentBlock().NumberU64()
			if !empty && cfg.From < first {
				cfg.To = first - 1
			}
		}
	} else if empty {
		return regenStatus{}, fmt.Errorf("%s mode needs stored lists", cfg.Mode)
	} else if cfg.From, cfg.To = max(cfg.From, first), cmpOr(cfg.To, next-1); cfg.To >= next {
		return regenStatus{}, fmt.Errorf("%s mode works on blocks %d-%d of the stored lists", cfg.Mode, first, next-1)
	}
	if imported && cfg.To > cfg.HashChain.Last {
		logger.Info("Importing block access lists up to the last block of the BAL hash chain", "to", cfg.To, "last", cfg.HashChain.Last)
		cfg.To = cfg.HashChain.Last
	}
	// Lists that do not touch the stored ones would not join them.
	if cfg.Mode == regenWrite && !empty && (cfg.From > next || cfg.To+1 < first) {
		return regenStatus{}, fmt.Errorf("write mode fills blocks that touch the stored lists of blocks %d-%d", first, next-1)
	}
	if cfg.From == 0 || cfg.From > cfg.To {
		return regenStatus{}, fmt.Errorf("nothing to regenerate in blocks %d-%d", cfg.From, cfg.To)
	}
	if cfg.To > c.bc.CurrentBlock().NumberU64() {
		return regenStatus{}, fmt.Errorf("block %d is after the head", cfg.To)
	}
	if !fromGenesis {
		if _, err := c.findStateAnchors(0, cfg.From-1, 1, 0); err != nil {
			return regenStatus{}, fmt.Errorf("no stored state before block %d, regenerate from genesis or import lists: %w", cfg.From, err)
		}
	}
	if cfg.Source == regenImport {
		src, closeSrc, err := c.openImport(&cfg)
		if err != nil {
			return regenStatus{}, err
		}
		srcFirst, srcNext := src.ReadBALRange()
		closeSrc()
		if srcFirst > 1 || srcNext <= cfg.To {
			return regenStatus{}, fmt.Errorf("the imported chaindata holds the lists of blocks %d-%d, not 1-%d", srcFirst, srcNext-1, cfg.To)
		}
	}

	dir, err := c.regenDir()
	if err != nil {
		return regenStatus{}, err
	}
	r := &regen{
		c: c, cfg: cfg, dir: dir,
		quit:     make(chan struct{}),
		heapBase: heapBytes(),
		started:  time.Now(),
		status:   regenStatus{Source: cfg.Source, Mode: cfg.Mode, From: cfg.From, To: cfg.To, Next: cfg.From, Workers: cfg.Workers},
	}
	if err := os.RemoveAll(r.dir); err != nil {
		return regenStatus{}, err
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return regenStatus{}, err
	}
	logger.Info("Started a BAL regeneration", "from", cfg.From, "to", cfg.To, "source", cfg.Source, "mode", cfg.Mode, "workers", cfg.Workers, "memory", common.StorageSize(cfg.Memory))
	done := make(chan struct{})
	go r.logProgress(done)
	if fromGenesis {
		r.runFromGenesis()
	} else {
		r.run()
	}
	close(done)
	r.finish()
	return r.snapshot(), nil
}

// snapshot returns the status of the regeneration.
func (r *regen) snapshot() regenStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	if elapsed := time.Since(r.started).Seconds(); elapsed > 0 && r.committed > 0 {
		s.BlocksPerSec = float64(r.committed) / elapsed
		if s.Next <= s.To {
			s.Remaining = (time.Duration(float64(s.To+1-s.Next)/s.BlocksPerSec) * time.Second).Round(time.Second).String()
		}
	}
	s.HeapBytes = heapBytes()
	return s
}

// logProgress logs the progress of the regeneration until done is closed.
func (r *regen) logProgress(done chan struct{}) {
	ticker := time.NewTicker(8 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			s := r.snapshot()
			logger.Info("Regenerating block access lists", "next", s.Next, "to", s.To, "intervals", fmt.Sprintf("%d/%d", s.Done, s.Intervals),
				"blocksPerSec", fmt.Sprintf("%.1f", s.BlocksPerSec), "remaining", s.Remaining, "mismatches", s.Mismatches, "heap", common.StorageSize(s.HeapBytes))
		}
	}
}

func heapBytes() uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return sample[0].Value.Uint64()
}

func (r *regen) stopped() bool {
	select {
	case <-r.quit:
		return true
	case <-r.c.quit:
		return true
	default:
		return false
	}
}

func (r *regen) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Error == "" {
		r.status.Error = err.Error()
	}
}

// overMemory reports whether the heap grew by more than the memory of the
// regeneration since it started.
func (r *regen) overMemory() bool {
	return heapBytes() > r.cfg.Memory+r.heapBase
}

// run splits the blocks into intervals at multiples of the unit, executes them
// on the workers and commits their lists in block order.
func (r *regen) run() {
	var intervals []*regenInterval
	for from := r.cfg.From - 1; from < r.cfg.To; {
		to := min((from/defaultRegenUnit+1)*defaultRegenUnit, r.cfg.To)
		intervals = append(intervals, &regenInterval{
			index: len(intervals), from: from, to: to,
			path: filepath.Join(r.dir, fmt.Sprintf("%d-%d", from+1, to)),
		})
		from = to
	}
	r.mu.Lock()
	r.status.Intervals = len(intervals)
	r.mu.Unlock()

	jobs := make(chan *regenInterval)
	results := make(chan *regenInterval, r.cfg.Workers)
	var workers sync.WaitGroup
	for range r.cfg.Workers {
		workers.Go(func() {
			cache := statedb.GetEmptyTrieNodeCacheConfig()
			cache.LocalCacheSizeMiB = int(r.cfg.Memory / uint64(r.cfg.Workers) / 2 >> 20)
			db := state.NewDatabaseWithNewCache(r.c.db, cache)
			for job := range jobs {
				job.err = r.execute(db, job)
				results <- job
			}
		})
	}

	// The dispatcher limits the intervals in flight by memory, and the
	// committer appends them in order.
	var inFlight atomic.Int64
	go func() {
		defer close(jobs)
		for _, job := range intervals {
			for {
				if r.stopped() {
					return
				}
				if inFlight.Load() < int64(r.cfg.Workers) && !r.overMemory() {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			inFlight.Add(1)
			select {
			case jobs <- job:
			case <-r.quit:
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	finished := make(map[int]*regenInterval)
	next := 0
	for job := range results {
		inFlight.Add(-1)
		finished[job.index] = job
		for job, ok := finished[next]; ok; job, ok = finished[next] {
			delete(finished, next)
			next++
			if job.err != nil {
				r.fail(fmt.Errorf("blocks %d-%d: %w", job.from+1, job.to, job.err))
				r.drain(results, &inFlight)
				return
			}
			if err := r.commit(job); err != nil {
				r.fail(err)
				r.drain(results, &inFlight)
				return
			}
		}
		if r.stopped() {
			r.fail(errRegenStopped)
			r.drain(results, &inFlight)
			return
		}
	}
}

// finish drops the work files.
func (r *regen) finish() {
	os.RemoveAll(r.dir)
	r.c.bc.States().BALs().SignalIndex()
	s := r.snapshot()
	first, next := r.c.bc.States().BALs().Range()
	logger.Info("Finished a BAL regeneration", "next", s.Next, "mismatches", s.Mismatches, "err", s.Error, "storedFirst", first, "storedNext", next)
}

func (r *regen) drain(results chan *regenInterval, inFlight *atomic.Int64) {
	select {
	case <-r.quit:
	default:
		close(r.quit)
	}
	for range results {
		inFlight.Add(-1)
	}
}

// execute runs the blocks of an interval from the stored state at its start,
// or at the last stored state before it, and writes the lists of its blocks to
// its file, each prefixed by its length.
func (r *regen) execute(db state.Database, job *regenInterval) error {
	anchor, err := r.c.stateAnchorAtOrBefore(job.from, 0, r.cfg.Step)
	if err != nil {
		// Stored states need not be at multiples of the step.
		if anchor, err = r.c.stateAnchorAtOrBefore(job.from, 0, 1); err != nil {
			return err
		}
	}
	header := r.c.bc.GetHeaderByNumber(anchor)
	sdb, err := state.New(header.Root, db, nil, nil)
	if err != nil {
		return err
	}
	f, err := os.Create(job.path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for n := anchor + 1; n <= job.to; n++ {
		if r.stopped() {
			return errRegenStopped
		}
		block := r.c.bc.GetBlockByNumber(n)
		if block == nil {
			return fmt.Errorf("no block %d", n)
		}
		sdb.StartBAL(0)
		if _, _, _, _, _, err := r.c.bc.Processor().Process(block, sdb, r.c.vmConfig); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		if root := sdb.IntermediateRoot(true); root != block.Root() {
			return fmt.Errorf("block %d: state root %x, header has %x", n, root, block.Root())
		}
		if err := sdb.Error(); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		if n <= job.from {
			continue // executed to reach the interval from an earlier state
		}
		enc, err := sdb.BAL().Encode()
		if err != nil {
			return err
		}
		if err := binary.Write(w, binary.BigEndian, uint32(len(enc))); err != nil {
			return err
		}
		if _, err := w.Write(enc); err != nil {
			return err
		}
	}
	return w.Flush()
}

// commit stores or compares the lists of a finished interval, then drops its
// file.
func (r *regen) commit(job *regenInterval) error {
	f, err := os.Open(job.path)
	if err != nil {
		return err
	}
	defer os.Remove(job.path)
	defer f.Close()
	rd := bufio.NewReader(f)
	for n := job.from + 1; n <= job.to; n++ {
		var size uint32
		if err := binary.Read(rd, binary.BigEndian, &size); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		enc := make([]byte, size)
		if _, err := io.ReadFull(rd, enc); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		list, err := types.DecodeBAL(enc)
		if err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		if err := r.commitList(n, list, true); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.status.Done++
	r.mu.Unlock()
	return nil
}

func cmpOr(v, def uint64) uint64 {
	if v == 0 {
		return def
	}
	return v
}

// genesisSpec returns the genesis of the chain, to rebuild its state. It does
// not build the default genesis of a network, which would reset DeriveSha.
func (c *balTools) genesisSpec(cfg *regenConfig) (*blockchain.Genesis, error) {
	if cfg.Genesis != nil {
		return cfg.Genesis, nil
	}
	if g := c.bc.GenesisSpec(); g != nil {
		return g, nil
	}
	return nil, fmt.Errorf("no genesis of chain %d to start from", c.bc.Config().ChainID)
}

// runFromGenesis rebuilds the state from the genesis block in a temporary
// database, executing every block (genesis source) or applying the lists in
// the chaindata of another node (import source), checks the state root of
// every block, and commits the lists of the blocks from From on.
func (r *regen) runFromGenesis() {
	var source *importLists
	switch r.cfg.Source {
	case regenImport:
		src, closeSrc, err := r.c.openImport(&r.cfg)
		if err != nil {
			r.fail(err)
			return
		}
		defer closeSrc()
		source = &importLists{db: src, chain: r.cfg.HashChain, to: r.cfg.To}
	}
	if c := r.cfg.HashChain; c != nil {
		logger.Info("Checking imported block access lists against the BAL hash chain", "last", c.Last, "root", c.Root())
	}
	genesis, err := r.c.genesisSpec(&r.cfg)
	if err != nil {
		r.fail(err)
		return
	}
	tempDir := filepath.Join(r.dir, "state")
	dbm := database.NewDBManager(&database.DBConfig{Dir: tempDir, DBType: database.LevelDB, OpenFilesLimit: 256, LevelDBCacheSize: 64, NumStateTrieShards: 1})
	defer os.RemoveAll(tempDir)
	defer dbm.Close()
	if root := genesis.ToBlock(common.Hash{}, dbm).Root(); root != r.c.bc.Genesis().Root() {
		r.fail(fmt.Errorf("genesis state root %x, chain has %x", root, r.c.bc.Genesis().Root()))
		return
	}

	cache := statedb.GetEmptyTrieNodeCacheConfig()
	cache.LocalCacheSizeMiB = int(r.cfg.Memory / 2 >> 20)
	sdbDB := state.NewDatabaseWithNewCache(dbm, cache)
	root := r.c.bc.Genesis().Root()
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
			r.fail(errRegenStopped)
			return
		}
		block := r.c.bc.GetBlockByNumber(n)
		if block == nil {
			r.fail(fmt.Errorf("no block %d", n))
			return
		}
		var list *types.BlockAccessList
		if source == nil {
			sdb.StartBAL(0)
			if _, _, _, _, _, err := r.c.bc.Processor().Process(block, sdb, r.c.vmConfig); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
			if err := sdb.Error(); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
			list = sdb.BAL()
		} else {
			if list, err = source.read(n); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
			if err := sdb.ApplyBAL(list); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
		}
		if got := sdb.IntermediateRoot(true); got != block.Root() {
			r.mu.Lock()
			r.status.Mismatches++
			r.mu.Unlock()
			r.fail(fmt.Errorf("block %d: state root %x, header has %x", n, got, block.Root()))
			return
		}
		// Imported lists hold the writes only; executed ones have the reads.
		if n >= r.cfg.From {
			if err := r.commitList(n, list, source == nil); err != nil {
				r.fail(err)
				return
			}
		}
		// The temporary state is written out now and then to bound memory.
		if n%regenCommitInterval == 0 || r.overMemory() {
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

// importLists reads the writes of the lists of another node from its
// chaindata, up to the last block of the hash chain. The writes of a segment,
// whose keccak256 are the hashes of their blocks, are all read and checked
// against the chain before any list of it is decoded and returned.
type importLists struct {
	db    database.DBManager
	chain *hashchain.Chain
	to    uint64 // last block to import, at most the last of the chain

	from, next uint64                   // blocks of lists
	lists      []*types.BlockAccessList // lists of blocks from to next-1
}

func (l *importLists) read(n uint64) (*types.BlockAccessList, error) {
	if !l.chain.Covers(n) {
		return nil, fmt.Errorf("%w: block %d is after the last block %d", hashchain.ErrMismatch, n, l.chain.Last)
	}
	if n < l.from || n >= l.next {
		from, to := l.chain.SegmentBlocks(hashchain.Segment(n))
		encs, err := l.segment(from, to)
		if err != nil {
			return nil, err
		}
		lists := make([]*types.BlockAccessList, len(encs))
		for i, enc := range encs {
			if from+uint64(i) == 0 {
				lists[i] = &types.BlockAccessList{}
				continue
			}
			list, err := types.DecodeBAL(enc)
			if err != nil {
				return nil, fmt.Errorf("block %d: %w", from+uint64(i), err)
			}
			lists[i] = list
		}
		l.from, l.next, l.lists = from, to+1, lists
	}
	return l.lists[n-l.from], nil
}

// segment returns the writes of the segment of blocks from to to, checked
// against the chain.
func (l *importLists) segment(from, to uint64) ([][]byte, error) {
	encs := make([][]byte, 0, to-from+1)
	hashes := make([]common.Hash, 0, to-from+1)
	for m := from; m <= to; m++ {
		if m == 0 {
			encs, hashes = append(encs, nil), append(hashes, state.EmptyListHash)
			continue
		}
		enc, err := l.db.ReadCanonicalBAL(m)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", m, err)
		}
		encs, hashes = append(encs, enc), append(hashes, crypto.Keccak256Hash(enc))
	}
	if err := l.chain.VerifySegment(hashchain.Segment(from), hashes); err != nil {
		return nil, fmt.Errorf("blocks %d-%d: %w", from, to, err)
	}
	return encs, nil
}

// commitList stores the list of a block, or compares its writes with the
// stored ones and, in reads mode, stores its reads if they match. hasReads
// tells whether the list holds the reads of the block, as an executed one
// does. Lists are committed in block order from From on.
func (r *regen) commitList(n uint64, list *types.BlockAccessList, hasReads bool) error {
	hash := r.c.db.ReadCanonicalHash(n)
	writes, err := list.Writes().Encode()
	if err != nil {
		return fmt.Errorf("block %d: %w", n, err)
	}
	var reads []byte
	if hasReads && (r.cfg.Mode == regenReads || (r.cfg.Mode == regenWrite && !r.cfg.SkipReads)) {
		if reads, err = list.Reads().Encode(); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
	}
	if r.cfg.Mode == regenWrite {
		if err := r.c.bc.States().BALs().Put(n, hash, writes, reads, r.cfg.From); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
	} else {
		stored := r.c.db.ReadBAL(hash, n)
		if stored == nil {
			return fmt.Errorf("block %d: %w", n, database.ErrBALNotStored)
		}
		if !bytes.Equal(stored, writes) {
			r.mu.Lock()
			r.status.Mismatches++
			r.mu.Unlock()
			logger.Warn("Regenerated block access list differs from the stored one", "number", n)
		} else if r.cfg.Mode == regenReads {
			if err := r.c.db.WriteBALReads(hash, n, reads); err != nil {
				return fmt.Errorf("block %d: %w", n, err)
			}
		}
	}
	r.mu.Lock()
	r.status.Next = n + 1
	r.committed++
	r.mu.Unlock()
	return nil
}

// openImport opens the chaindata of another node to import its lists, and
// returns how to close it.
func (c *balTools) openImport(cfg *regenConfig) (database.DBManager, func(), error) {
	if cfg.ImportDB != nil {
		return cfg.ImportDB, func() {}, nil
	}
	if _, err := os.Stat(cfg.ImportDir); err != nil {
		return nil, nil, err
	}
	dbc := *c.db.GetDBConfig()
	dbc.Dir = cfg.ImportDir
	db := database.NewDBManager(&dbc)
	return db, db.Close, nil
}

// regenDir returns the directory of the work files of a regeneration.
func (c *balTools) regenDir() (string, error) {
	if dir := c.db.GetDBConfig().Dir; dir != "" {
		return filepath.Join(dir, "balregen"), nil
	}
	return os.MkdirTemp("", "balregen")
}

var errNoStateAnchor = errors.New("no stored state in the range")

// hasStateAt reports whether the state of block number is stored: the root
// node of its trie is in the database. State migration and live pruning drop
// the oldest states first, so a stored root means a whole trie.
func (c *balTools) hasStateAt(number uint64) bool {
	header := c.bc.GetHeaderByNumber(number)
	if header == nil {
		return false
	}
	if header.Root == types.EmptyRootHash {
		return true
	}
	node, err := c.states.TrieNode(header.Root)
	return err == nil && len(node) > 0
}

// stateAnchors describes where the stored states of a range are: the first
// one, found by binary search, and a sample of the later candidates, which are
// the multiples of Step after it.
type stateAnchors struct {
	First   uint64
	Step    uint64
	Sampled int
	Missing []uint64 // sampled candidates without a state
}

// findStateAnchors finds the first block in [from, to] with a stored state,
// looking at the multiples of step, and checks samples of the later multiples.
// Blocks with a stored state are anchors: blocks after them can be executed
// again from there. Missing later anchors are allowed; an execution then
// starts from an earlier one.
func (c *balTools) findStateAnchors(from, to, step uint64, samples int) (*stateAnchors, error) {
	if step == 0 {
		return nil, errors.New("zero step")
	}
	lo := (from + step - 1) / step * step
	hi := to / step * step
	if lo > hi || !c.hasStateAt(hi) {
		return nil, fmt.Errorf("%w: [%d, %d] at multiples of %d", errNoStateAnchor, from, to, step)
	}
	// States are dropped from the oldest, so the multiples with a state are a
	// suffix of the range.
	for lo < hi {
		mid := lo + (hi-lo)/step/2*step
		if c.hasStateAt(mid) {
			hi = mid
		} else {
			lo = mid + step
		}
	}
	anchors := &stateAnchors{First: lo, Step: step}
	if count := (to/step*step-lo)/step + 1; samples > 0 && count > 1 {
		every := max(1, count/uint64(samples))
		for k := every; k < count; k += every {
			n := lo + k*step
			anchors.Sampled++
			if !c.hasStateAt(n) {
				anchors.Missing = append(anchors.Missing, n)
			}
		}
	}
	return anchors, nil
}

// stateAnchorAtOrBefore returns the last block with a stored state at or before
// number, among the multiples of step from first.
func (c *balTools) stateAnchorAtOrBefore(number, first, step uint64) (uint64, error) {
	for n := (number-first)/step*step + first; ; n -= step {
		if c.hasStateAt(n) {
			return n, nil
		}
		if n < first+step {
			return 0, fmt.Errorf("%w: at or before block %d", errNoStateAnchor, number)
		}
	}
}
