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
	"bufio"
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

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/statedb"
)

// Sources and modes of a BAL regeneration.
const (
	BALRegenLocal  = "local"  // execute from the stored states of this node
	BALRegenWrite  = "write"  // append the lists to the BAL freezer
	BALRegenVerify = "verify" // compare the lists with the ones in the freezer
)

const (
	defaultBALRegenUnit     = 128     // blocks of a work interval
	minBALRegenWorkerMemory = 1 << 29 // 512 MiB: trie cache and the state changes of an interval
	balRegenHeadLag         = 30 * time.Second
)

var (
	ErrBALRegenRunning = errors.New("a BAL regeneration is running")
	errBALRegenStopped = errors.New("BAL regeneration stopped")
)

// BALRegenConfig configures a regeneration of the block access lists of blocks
// From to To.
type BALRegenConfig struct {
	From    uint64 `json:"from"` // 0: the next block of the freezer
	To      uint64 `json:"to"`   // 0: the head when it starts
	Source  string `json:"source"`
	Mode    string `json:"mode"`
	Memory  uint64 `json:"memory"`  // bytes for the workers; 0: 4 GiB
	Workers int    `json:"workers"` // 0: from the memory and the cores
	Step    uint64 `json:"step"`    // blocks between stored states; 0: the trie commit interval, 1 on an archive node

	ImportDir string   `json:"importDir"` // BAL freezer to import from, for the import source
	Genesis   *Genesis `json:"-"`         // genesis to start from; nil: the one of the chain ID
}

// BALRegenStatus reports a regeneration.
type BALRegenStatus struct {
	Running      bool    `json:"running"`
	Source       string  `json:"source"`
	Mode         string  `json:"mode"`
	From         uint64  `json:"from"`
	To           uint64  `json:"to"`
	Next         uint64  `json:"next"` // next block to commit
	Workers      int     `json:"workers"`
	Intervals    int     `json:"intervals"`
	Done         int     `json:"done"`
	BlocksPerSec float64 `json:"blocksPerSec"`
	Mismatches   uint64  `json:"mismatches"`
	Remaining    string  `json:"remaining"`
	HeapBytes    uint64  `json:"heapBytes"`
	Error        string  `json:"error,omitempty"`
}

type balRegen struct {
	bc   *BlockChain
	cfg  BALRegenConfig
	dir  string // interval files
	quit chan struct{}
	done chan struct{}

	prefix *bal.Freezer // lists of the blocks before the freezer, put in front of it at the end

	mu        sync.Mutex
	status    BALRegenStatus
	started   time.Time
	committed uint64 // blocks committed since started
}

type regenInterval struct {
	index      int
	from, to   uint64 // lists of blocks from+1 to to
	path       string
	mismatches uint64
	err        error
}

// StartBALRegen starts regenerating block access lists in the background.
func (bc *BlockChain) StartBALRegen(cfg BALRegenConfig) error {
	bc.balRegenMu.Lock()
	defer bc.balRegenMu.Unlock()
	if bc.balRegen != nil {
		select {
		case <-bc.balRegen.done:
		default:
			return ErrBALRegenRunning
		}
	}
	if bc.bal == nil {
		return errors.New("block access lists are not recorded (--bal.record-dir)")
	}
	if cfg.Source == "" {
		cfg.Source = BALRegenLocal
	}
	if cfg.Mode == "" {
		cfg.Mode = BALRegenWrite
	}
	fromGenesis := cfg.Source == BALRegenGenesis || cfg.Source == BALRegenImport
	if cfg.Source != BALRegenLocal && !fromGenesis {
		return fmt.Errorf("unknown source %q", cfg.Source)
	}
	if cfg.Source == BALRegenImport && cfg.ImportDir == "" {
		return errors.New("the import source needs importDir")
	}
	if cfg.Mode != BALRegenWrite && cfg.Mode != BALRegenVerify {
		return fmt.Errorf("unknown mode %q", cfg.Mode)
	}
	if cfg.Memory == 0 {
		cfg.Memory = 4 << 30
	}
	if cfg.Step == 0 {
		cfg.Step = uint64(bc.cacheConfig.BlockInterval)
		if bc.cacheConfig.ArchiveMode || cfg.Step == 0 {
			cfg.Step = 1
		}
	}
	if cfg.Workers <= 0 {
		cfg.Workers = max(1, min(runtime.NumCPU()/2, int(cfg.Memory/minBALRegenWorkerMemory)))
	}

	bc.bal.mu.Lock()
	next, first := bc.bal.freezer.Next(), bc.bal.freezer.First()
	bc.bal.mu.Unlock()
	empty := next == first || (first == 0 && next == 1)
	prepend := false
	if cfg.Mode == BALRegenWrite {
		if cfg.From == 0 {
			cfg.From = next
			if fromGenesis {
				cfg.From = 1
			}
		}
		if cfg.To == 0 {
			cfg.To = bc.CurrentBlock().NumberU64()
			if !empty && cfg.From < first {
				cfg.To = first - 1
			}
		}
		switch {
		case cfg.From == next:
		case !empty && cfg.From < first && cfg.To == first-1:
			// The lists of the blocks before the freezer are built apart and
			// put in front of it at the end.
			prepend = true
		default:
			// An empty freezer can start where the states start.
			if err := bc.bal.resetFirst(cfg.From); err != nil {
				return fmt.Errorf("write mode appends from block %d or fills blocks before %d: %w", next, first, err)
			}
		}
	} else if cfg.From, cfg.To = max(cfg.From, first, 1), cmpOr(cfg.To, next-1); cfg.From < max(first, 1) || cfg.To >= next {
		return fmt.Errorf("verify mode compares blocks %d-%d of the freezer", max(first, 1), next-1)
	}
	if cfg.From == 0 || cfg.From > cfg.To {
		return fmt.Errorf("nothing to regenerate in blocks %d-%d", cfg.From, cfg.To)
	}
	if cfg.To > bc.CurrentBlock().NumberU64() {
		return fmt.Errorf("block %d is after the head", cfg.To)
	}
	if !fromGenesis {
		if _, err := bc.FindStateAnchors(0, cfg.From-1, 1, 0); err != nil {
			return fmt.Errorf("no stored state before block %d, regenerate from genesis or import lists: %w", cfg.From, err)
		}
	}
	if cfg.Source == BALRegenImport {
		src, err := bal.OpenFreezer(cfg.ImportDir, true)
		if err != nil {
			return err
		}
		srcFirst, srcNext := src.First(), src.Next()
		src.Close()
		if srcFirst > 1 || srcNext <= cfg.To {
			return fmt.Errorf("the imported freezer holds blocks %d-%d, not 1-%d", srcFirst, srcNext-1, cfg.To)
		}
	}

	r := &balRegen{
		bc: bc, cfg: cfg, dir: filepath.Join(bc.bal.dir, "regen"),
		quit: make(chan struct{}), done: make(chan struct{}),
		started: time.Now(),
		status:  BALRegenStatus{Running: true, Source: cfg.Source, Mode: cfg.Mode, From: cfg.From, To: cfg.To, Next: cfg.From, Workers: cfg.Workers},
	}
	if err := os.RemoveAll(r.dir); err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	if prepend {
		start := cfg.From
		if start == 1 {
			start = 0 // a freezer from block 1 holds the genesis list too
		}
		f, err := bal.OpenFreezerAt(filepath.Join(r.dir, "prefix"), start)
		if err != nil {
			return err
		}
		r.prefix = f
	} else if cfg.Mode == BALRegenWrite {
		if err := bc.bal.hold(); err != nil {
			return err
		}
	}
	bc.balRegen = r
	logger.Info("Started a BAL regeneration", "from", cfg.From, "to", cfg.To, "source", cfg.Source, "mode", cfg.Mode, "workers", cfg.Workers, "memory", common.StorageSize(cfg.Memory), "prepend", prepend)
	if fromGenesis {
		bc.wg.Go(r.runFromGenesis)
	} else {
		bc.wg.Go(r.run)
	}
	return nil
}

// StopBALRegen stops a regeneration. Committed lists stay; a later one
// continues from the freezer.
func (bc *BlockChain) StopBALRegen() error {
	bc.balRegenMu.Lock()
	r := bc.balRegen
	bc.balRegenMu.Unlock()
	if r == nil {
		return errors.New("no BAL regeneration")
	}
	select {
	case <-r.quit:
	default:
		close(r.quit)
	}
	<-r.done
	return nil
}

// BALRegenStatus returns the status of the last regeneration.
func (bc *BlockChain) BALRegenStatus() BALRegenStatus {
	bc.balRegenMu.Lock()
	r := bc.balRegen
	bc.balRegenMu.Unlock()
	if r == nil {
		return BALRegenStatus{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	if elapsed := time.Since(r.started).Seconds(); elapsed > 0 && r.committed > 0 {
		s.BlocksPerSec = float64(r.committed) / elapsed
		if s.Running {
			s.Remaining = (time.Duration(float64(s.To+1-s.Next)/s.BlocksPerSec) * time.Second).Round(time.Second).String()
		}
	}
	s.HeapBytes = heapBytes()
	return s
}

func heapBytes() uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return sample[0].Value.Uint64()
}

func (r *balRegen) stopped() bool {
	select {
	case <-r.quit:
		return true
	case <-r.bc.quit:
		return true
	default:
		return false
	}
}

func (r *balRegen) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Error == "" {
		r.status.Error = err.Error()
	}
}

// run splits the blocks into intervals at multiples of the unit, executes them
// on the workers and commits their lists in block order.
func (r *balRegen) run() {
	defer close(r.done)
	defer r.finish()

	var intervals []*regenInterval
	for from := r.cfg.From - 1; from < r.cfg.To; {
		to := min((from/defaultBALRegenUnit+1)*defaultBALRegenUnit, r.cfg.To)
		intervals = append(intervals, &regenInterval{index: len(intervals), from: from, to: to,
			path: filepath.Join(r.dir, fmt.Sprintf("%d-%d", from+1, to))})
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
			db := state.NewDatabaseWithNewCache(r.bc.db, cache)
			for job := range jobs {
				job.err = r.execute(db, job)
				results <- job
			}
		})
	}

	// The dispatcher limits the intervals in flight by memory and by how far
	// the head lags, and the committer appends them in order.
	var (
		inFlight atomic.Int64
		allowed  atomic.Int64
	)
	allowed.Store(int64(r.cfg.Workers))
	go func() {
		defer close(jobs)
		for _, job := range intervals {
			for {
				if r.stopped() {
					return
				}
				if inFlight.Load() < allowed.Load() && heapBytes() < r.cfg.Memory+heapBase() {
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
	lastCheck := time.Now()
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
		// Fewer workers while the head falls behind.
		if time.Since(lastCheck) > time.Second {
			lastCheck = time.Now()
			head := r.bc.CurrentBlock().Header()
			if time.Since(time.Unix(head.Time.Int64(), 0)) > balRegenHeadLag && r.bc.CurrentBlock().NumberU64() > r.cfg.To {
				allowed.Store(max(1, allowed.Load()-1))
			} else if allowed.Load() < int64(r.cfg.Workers) {
				allowed.Add(1)
			}
		}
		if r.stopped() {
			r.fail(errBALRegenStopped)
			r.drain(results, &inFlight)
			return
		}
	}
}

// finish puts built lists in front of the freezer, appends the lists of the
// blocks inserted meanwhile and drops the work files.
func (r *balRegen) finish() {
	if r.prefix != nil {
		r.mu.Lock()
		failed := r.status.Error != ""
		r.mu.Unlock()
		if failed {
			r.prefix.Close()
		} else if err := r.bc.bal.prepend(r.prefix, filepath.Join(r.dir, "prefix")); err != nil {
			r.prefix.Close()
			r.fail(fmt.Errorf("put the lists in front of the freezer: %w", err))
		}
	}
	if r.cfg.Mode == BALRegenWrite {
		if n, err := r.bc.bal.release(); err != nil {
			logger.Error("Failed to append the pending block access lists", "err", err)
		} else if n > 0 {
			logger.Info("Appended the block access lists of the blocks inserted meanwhile", "count", n)
		}
	}
	os.RemoveAll(r.dir)
	r.mu.Lock()
	r.status.Running = false
	r.mu.Unlock()
	s := r.bc.BALRegenStatus()
	logger.Info("Finished a BAL regeneration", "next", s.Next, "mismatches", s.Mismatches, "err", s.Error)
}

// heapBase is the heap the node used before a regeneration counts, a fixed
// allowance since the node keeps running.
func heapBase() uint64 { return 2 << 30 }

func (r *balRegen) drain(results chan *regenInterval, inFlight *atomic.Int64) {
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
func (r *balRegen) execute(db state.Database, job *regenInterval) error {
	anchor, err := r.bc.stateAnchorAtOrBefore(job.from, 0, r.cfg.Step)
	if err != nil {
		// Stored states need not be at multiples of the step.
		if anchor, err = r.bc.stateAnchorAtOrBefore(job.from, 0, 1); err != nil {
			return err
		}
	}
	header := r.bc.GetHeaderByNumber(anchor)
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
			return errBALRegenStopped
		}
		block := r.bc.GetBlockByNumber(n)
		if block == nil {
			return fmt.Errorf("no block %d", n)
		}
		sdb.EnableBlockAccessRecorderAt(0)
		if _, _, _, _, _, err := r.bc.processor.Process(block, sdb, r.bc.vmConfig); err != nil {
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
		list := sdb.BlockAccessList()
		if err := list.CheckHash(block.Header().BlockAccessListHash); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		enc, err := list.Encode()
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

// commit appends or compares the lists of a finished interval, then drops its
// file.
func (r *balRegen) commit(job *regenInterval) error {
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
		if err := r.commitList(n, enc); err != nil {
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

// StartHistoryConversion turns a node that keeps its states, archive or full
// without live pruning, into a node with BAL history: it regenerates the lists
// of the blocks after its first stored state, or after its last state
// migration, up to the head, while new blocks keep being recorded. Blocks
// before stay without lists, for an operator to regenerate from genesis or to
// import. It does nothing on a live pruned node or when the lists are there.
func (bc *BlockChain) StartHistoryConversion(memory uint64, workers int) error {
	if bc.bal == nil || bc.db.ReadPruningEnabled() {
		return nil
	}
	head := bc.CurrentBlock().NumberU64()
	bc.bal.mu.Lock()
	next, first := bc.bal.freezer.Next(), bc.bal.freezer.First()
	bc.bal.mu.Unlock()
	if next > head {
		return nil
	}
	from := next
	if empty := next == first || (first == 0 && next == 1); empty {
		start, ok := bc.db.ReadLastMigratedBlockNumber()
		if !ok {
			step := uint64(bc.cacheConfig.BlockInterval)
			if bc.cacheConfig.ArchiveMode || step == 0 {
				step = 1
			}
			anchors, err := bc.FindStateAnchors(0, head, step, 100)
			if err != nil {
				return err
			}
			if len(anchors.Missing) > 0 {
				logger.Warn("Some stored states are missing, earlier ones are used instead", "sampled", anchors.Sampled, "missing", len(anchors.Missing))
			}
			start = anchors.First
		}
		from = start + 1
	}
	logger.Info("Converting to BAL history", "from", from, "to", head)
	return bc.StartBALRegen(BALRegenConfig{From: from, To: head, Memory: memory, Workers: workers})
}
