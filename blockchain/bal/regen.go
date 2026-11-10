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

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/statedb"
)

// Sources and modes of a BAL regeneration.
const (
	RegenLocal  = "local"  // execute from the stored states of this node
	RegenWrite  = "write"  // store the lists: their writes, and their reads unless skipped
	RegenVerify = "verify" // compare the writes of the lists with the stored ones
	RegenReads  = "reads"  // store the reads of blocks whose writes are stored, executing them again
)

const (
	defaultRegenUnit     = 128     // blocks of a work interval
	minRegenWorkerMemory = 1 << 29 // 512 MiB: trie cache and the state changes of an interval
	regenHeadLag         = 30 * time.Second
)

var (
	ErrRegenRunning = errors.New("a BAL regeneration is running")
	errRegenStopped = errors.New("BAL regeneration stopped")
)

// RegenConfig configures a regeneration of the block access lists of blocks
// From to To.
type RegenConfig struct {
	From    uint64 `json:"from"` // 0: the block after the stored lists
	To      uint64 `json:"to"`   // 0: the head when it starts
	Source  string `json:"source"`
	Mode    string `json:"mode"`
	Memory  uint64 `json:"memory"`  // bytes for the workers; 0: 4 GiB
	Workers int    `json:"workers"` // 0: from the memory and the cores
	Step    uint64 `json:"step"`    // blocks between stored states; 0: the trie commit interval, 1 on an archive node

	SkipReads bool `json:"skipReads"` // write mode stores the writes only; set with --bal.skip-reads

	Genesis *blockchain.Genesis `json:"-"` // genesis to start from; nil: the one of the chain ID
}

// RegenStatus reports a regeneration.
type RegenStatus struct {
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

type regen struct {
	c    *chain
	cfg  RegenConfig
	dir  string // interval files
	quit chan struct{}
	done chan struct{}

	mu        sync.Mutex
	status    RegenStatus
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

// StartRegen starts regenerating block access lists in the background.
func StartRegen(bc *blockchain.BlockChain, cfg RegenConfig) error { return of(bc).startRegen(cfg) }

func (c *chain) startRegen(cfg RegenConfig) error {
	c.regenMu.Lock()
	defer c.regenMu.Unlock()
	if c.regen != nil {
		select {
		case <-c.regen.done:
		default:
			return ErrRegenRunning
		}
	}
	if c.rec == nil {
		return errors.New("block access lists are not recorded (--bal.record)")
	}
	if cfg.Source == "" {
		cfg.Source = RegenLocal
	}
	if cfg.Mode == "" {
		cfg.Mode = RegenWrite
	}
	fromGenesis := cfg.Source == RegenGenesis
	if cfg.Source != RegenLocal && !fromGenesis {
		return fmt.Errorf("unknown source %q", cfg.Source)
	}
	if cfg.Mode != RegenWrite && cfg.Mode != RegenVerify && cfg.Mode != RegenReads {
		return fmt.Errorf("unknown mode %q", cfg.Mode)
	}
	cfg.SkipReads = cfg.SkipReads || c.rec.skipReads
	if cfg.Memory == 0 {
		cfg.Memory = 4 << 30
	}
	if cfg.Step == 0 {
		cfg.Step = uint64(c.cfg.BlockInterval)
		if c.cfg.ArchiveMode || cfg.Step == 0 {
			cfg.Step = 1
		}
	}
	if cfg.Workers <= 0 {
		cfg.Workers = max(1, min(runtime.NumCPU()/2, int(cfg.Memory/minRegenWorkerMemory)))
	}

	first, next := c.rec.span()
	empty := first == next
	if cfg.Mode == RegenWrite {
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
		return fmt.Errorf("%s mode needs stored lists", cfg.Mode)
	} else if cfg.From, cfg.To = max(cfg.From, first), cmpOr(cfg.To, next-1); cfg.To >= next {
		return fmt.Errorf("%s mode works on blocks %d-%d of the stored lists", cfg.Mode, first, next-1)
	}
	// Lists that do not touch the stored ones would not join them.
	if cfg.Mode == RegenWrite && !empty && (cfg.From > next || cfg.To+1 < first) {
		return fmt.Errorf("write mode fills blocks that touch the stored lists of blocks %d-%d", first, next-1)
	}
	if cfg.From == 0 || cfg.From > cfg.To {
		return fmt.Errorf("nothing to regenerate in blocks %d-%d", cfg.From, cfg.To)
	}
	if cfg.To > c.bc.CurrentBlock().NumberU64() {
		return fmt.Errorf("block %d is after the head", cfg.To)
	}
	if !fromGenesis {
		if _, err := c.findStateAnchors(0, cfg.From-1, 1, 0); err != nil {
			return fmt.Errorf("no stored state before block %d, regenerate from genesis: %w", cfg.From, err)
		}
	}

	dir, err := c.regenDir()
	if err != nil {
		return err
	}
	r := &regen{
		c: c, cfg: cfg, dir: dir,
		quit: make(chan struct{}), done: make(chan struct{}),
		started: time.Now(),
		status:  RegenStatus{Running: true, Source: cfg.Source, Mode: cfg.Mode, From: cfg.From, To: cfg.To, Next: cfg.From, Workers: cfg.Workers},
	}
	if err := os.RemoveAll(r.dir); err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	c.regen = r
	logger.Info("Started a BAL regeneration", "from", cfg.From, "to", cfg.To, "source", cfg.Source, "mode", cfg.Mode, "workers", cfg.Workers, "memory", common.StorageSize(cfg.Memory))
	if fromGenesis {
		c.wg.Go(r.runFromGenesis)
	} else {
		c.wg.Go(r.run)
	}
	return nil
}

// StopRegen stops a regeneration. Committed lists stay; a later one
// continues after the stored lists.
func StopRegen(bc *blockchain.BlockChain) error { return of(bc).stopRegen() }

func (c *chain) stopRegen() error {
	c.regenMu.Lock()
	r := c.regen
	c.regenMu.Unlock()
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

// GetRegenStatus returns the status of the last regeneration.
func GetRegenStatus(bc *blockchain.BlockChain) RegenStatus { return of(bc).regenStatus() }

func (c *chain) regenStatus() RegenStatus {
	c.regenMu.Lock()
	r := c.regen
	c.regenMu.Unlock()
	if r == nil {
		return RegenStatus{}
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

// run splits the blocks into intervals at multiples of the unit, executes them
// on the workers and commits their lists in block order.
func (r *regen) run() {
	defer r.finish()

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
			head := r.c.bc.CurrentBlock().Header()
			if time.Since(time.Unix(head.Time.Int64(), 0)) > regenHeadLag && r.c.bc.CurrentBlock().NumberU64() > r.cfg.To {
				allowed.Store(max(1, allowed.Load()-1))
			} else if allowed.Load() < int64(r.cfg.Workers) {
				allowed.Add(1)
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
	r.c.signalIndex()
	close(r.done)
	r.mu.Lock()
	r.status.Running = false
	r.mu.Unlock()
	s := r.c.regenStatus()
	first, next := r.c.rec.span()
	logger.Info("Finished a BAL regeneration", "next", s.Next, "mismatches", s.Mismatches, "err", s.Error, "storedFirst", first, "storedNext", next)
}

// heapBase is the heap the node used before a regeneration counts, a fixed
// allowance since the node keeps running.
func heapBase() uint64 { return 2 << 30 }

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
		access.EnableRecorderAt(sdb, 0)
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
		enc, err := access.Recorded(sdb).Encode()
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
		list, err := access.Decode(enc)
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

// StartHistoryConversion turns a node that keeps its states, archive or full
// without live pruning, into a node with BAL history: it regenerates the lists
// of the blocks after its first stored state, or after its last state
// migration, up to the stored lists or the head, while new blocks keep being
// recorded. Blocks before stay without lists, for an operator to regenerate
// from genesis. It does nothing on a live pruned node or when the lists are
// there.
func StartHistoryConversion(bc *blockchain.BlockChain, memory uint64, workers int) error {
	return of(bc).startHistoryConversion(memory, workers)
}

func (c *chain) startHistoryConversion(memory uint64, workers int) error {
	if c.rec == nil || c.db.ReadPruningEnabled() {
		return nil
	}
	head := c.bc.CurrentBlock().NumberU64()
	first, next := c.rec.span()
	empty := first == next
	if !empty && next <= head {
		logger.Info("Converting to BAL history", "from", next, "to", head)
		return c.startRegen(RegenConfig{From: next, To: head, Memory: memory, Workers: workers})
	}
	start, ok := c.db.ReadLastMigratedBlockNumber()
	if !ok {
		step := uint64(c.cfg.BlockInterval)
		if c.cfg.ArchiveMode || step == 0 {
			step = 1
		}
		anchors, err := c.findStateAnchors(0, head, step, 100)
		if err != nil {
			return err
		}
		if len(anchors.Missing) > 0 {
			logger.Warn("Some stored states are missing, earlier ones are used instead", "sampled", anchors.Sampled, "missing", len(anchors.Missing))
		}
		start = anchors.First
	}
	from, to := start+1, head
	if !empty {
		to = first - 1
	}
	if from > to {
		return nil
	}
	logger.Info("Converting to BAL history", "from", from, "to", to)
	return c.startRegen(RegenConfig{From: from, To: to, Memory: memory, Workers: workers})
}
