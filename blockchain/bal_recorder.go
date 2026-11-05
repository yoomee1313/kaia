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
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

const balStatsInterval = 10_000 // blocks between stats logs

// balRecorder appends the block access list of every inserted block to a BAL
// freezer under dir, after checking that the list reproduces the state root.
// Item N of the freezer is the list of block N. It is an experimental tool to
// measure BAL on real chains.
//
// While a regeneration fills in earlier blocks (see StartBALRegen), the lists
// of inserted blocks wait in a pending database and are appended when the
// regeneration reaches them.
type balRecorder struct {
	dir string

	mu      sync.Mutex // guards the freezer and pending
	freezer *bal.Freezer
	pending database.Database // lists of inserted blocks held for a regeneration, nil if none
	holding bool

	stats    balStats
	sinceLog balStats
}

type balStats struct {
	blocks, txs, bytes, failures, gaps uint64
	verifyTime                         time.Duration
}

func newBALRecorder(dir string) (*balRecorder, error) {
	freezer, err := bal.OpenFreezer(dir, false)
	if err != nil {
		return nil, err
	}
	logger.Warn("Recording block access lists (experimental)", "dir", dir, "first", freezer.First(), "next", freezer.Next())
	return &balRecorder{dir: dir, freezer: freezer}, nil
}

func pendingKey(number uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte("p"), number)
}

// hold makes inserted blocks wait in the pending database, so that a
// regeneration appends the earlier blocks first.
func (r *balRecorder) hold() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		db, err := database.NewLevelDBWithOption(filepath.Join(r.dir, "pending"), &opt.Options{})
		if err != nil {
			return err
		}
		r.pending = db
	}
	r.holding = true
	return nil
}

// appendRegenerated appends the list of a block a regeneration produced.
func (r *balRecorder) appendRegenerated(number uint64, enc []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.freezer.Append(number, enc)
}

// release appends the pending lists that follow the freezer and goes back to
// appending inserted blocks directly. It returns how many were appended.
func (r *balRecorder) release() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.holding = false
	if r.pending == nil {
		return 0, nil
	}
	appended := 0
	for {
		next := r.freezer.Next()
		key := pendingKey(next)
		ok, err := r.pending.Has(key)
		if err != nil || !ok {
			return appended, err
		}
		enc, err := r.pending.Get(key)
		if err != nil {
			return appended, err
		}
		if err := r.freezer.Append(next, enc); err != nil {
			return appended, err
		}
		if err := r.pending.Delete(key); err != nil {
			return appended, err
		}
		appended++
	}
}

// resetFirst makes an empty freezer start at block first, as a node whose
// states before first are gone cannot regenerate earlier lists.
func (r *balRecorder) resetFirst(first uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.freezer.First() == first {
		return nil
	}
	if empty := r.freezer.Next() == r.freezer.First() || (r.freezer.First() == 0 && r.freezer.Next() == 1); !empty {
		return fmt.Errorf("BAL freezer holds blocks %d-%d", r.freezer.First(), r.freezer.Next()-1)
	}
	if err := r.freezer.Close(); err != nil {
		return err
	}
	if err := removeFreezerFiles(r.dir); err != nil {
		return err
	}
	freezer, err := bal.OpenFreezerAt(r.dir, first)
	if err != nil {
		return err
	}
	r.freezer = freezer
	return nil
}

// prepend puts the lists of the freezer built in dir, which ends right before
// the first block of this freezer, in front of it: the lists of this freezer
// are copied after them and the built freezer takes its place. Inserted blocks
// keep being appended meanwhile; the last ones are copied under the lock.
func (r *balRecorder) prepend(built *bal.Freezer, dir string) error {
	r.mu.Lock()
	first, next := r.freezer.First(), r.freezer.Next()
	r.mu.Unlock()
	if built.Next() != max(first, 1) {
		return fmt.Errorf("built lists end at block %d, the freezer starts at %d", built.Next()-1, first)
	}
	copyTo := func(to uint64) error {
		for n := built.Next(); n < to; n++ {
			enc, err := r.freezer.ReadRaw(n)
			if err != nil {
				return err
			}
			if err := built.Append(n, enc); err != nil {
				return err
			}
		}
		return nil
	}
	if err := copyTo(next); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := copyTo(r.freezer.Next()); err != nil {
		return err
	}
	if err := built.Close(); err != nil {
		return err
	}
	if err := r.freezer.Close(); err != nil {
		return err
	}
	if err := removeFreezerFiles(r.dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), bal.FreezerTable+".") || e.Name() == "FIRST" {
			if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(r.dir, e.Name())); err != nil {
				return err
			}
		}
	}
	freezer, err := bal.OpenFreezer(r.dir, false)
	if err != nil {
		return err
	}
	r.freezer = freezer
	logger.Info("Put regenerated block access lists in front of the freezer", "first", freezer.First(), "next", freezer.Next())
	return nil
}

func removeFreezerFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), bal.FreezerTable+".") || e.Name() == "FIRST" {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// record encodes the list recorded by statedb, applies it to the parent state
// and appends it only if the resulting root matches the block.
func (r *balRecorder) record(bc *BlockChain, block *types.Block, parentRoot common.Hash, statedb *state.StateDB) {
	number := block.NumberU64()
	r.mu.Lock()
	next, holding := r.freezer.Next(), r.holding
	r.mu.Unlock()
	if number < next {
		return // already stored, e.g. a block inserted again after a rewind
	} else if number > next && !holding {
		// The freezer has no gaps, so nothing can be recorded until the
		// blocks before this one are.
		if r.sinceLog.gaps == 0 {
			logger.Warn("Block access list not recorded: missing earlier blocks", "number", number, "next", next)
		}
		r.sinceLog.gaps++
		return
	}

	enc, err := statedb.BlockAccessList().Encode()
	if err != nil {
		logger.Error("Failed to encode block access list", "number", number, "err", err)
		r.sinceLog.failures++
		return
	}

	start := time.Now()
	err = verifyBAL(bc, block, parentRoot, enc)
	r.sinceLog.verifyTime += time.Since(start)
	if err != nil {
		logger.Error("Block access list failed the self-check", "number", number, "hash", block.Hash(), "err", err)
		r.sinceLog.failures++
		return
	}

	r.mu.Lock()
	if r.holding {
		err = r.pending.Put(pendingKey(number), enc)
	} else {
		err = r.freezer.Append(number, enc)
	}
	r.mu.Unlock()
	if err != nil {
		logger.Error("Failed to write block access list", "number", number, "err", err)
		r.sinceLog.failures++
		return
	}
	r.sinceLog.blocks++
	r.sinceLog.txs += uint64(len(block.Transactions()))
	r.sinceLog.bytes += uint64(len(enc))
	if r.sinceLog.blocks >= balStatsInterval {
		r.logStats(number)
	}
}

func verifyBAL(bc *BlockChain, block *types.Block, parentRoot common.Hash, enc []byte) error {
	list, err := bal.Decode(enc)
	if err != nil {
		return err
	}
	statedb, err := bc.StateAt(parentRoot)
	if err != nil {
		return err
	}
	if err := statedb.ApplyBlockAccessList(list); err != nil {
		return err
	}
	if root := statedb.IntermediateRoot(true); root != block.Root() {
		return fmt.Errorf("root mismatch: have %s, want %s", root.Hex(), block.Root().Hex())
	}
	return nil
}

func (r *balRecorder) logStats(number uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.freezer.Sync(); err != nil {
		logger.Error("Failed to sync the BAL freezer", "err", err)
	}
	s := &r.sinceLog
	r.stats.blocks += s.blocks
	r.stats.txs += s.txs
	r.stats.bytes += s.bytes
	r.stats.failures += s.failures
	r.stats.gaps += s.gaps
	r.stats.verifyTime += s.verifyTime

	perBlock, perTx := uint64(0), uint64(0)
	if s.blocks > 0 {
		perBlock = s.bytes / s.blocks
	}
	if s.txs > 0 {
		perTx = s.bytes / s.txs
	}
	size, _ := r.freezer.Size()
	logger.Info("Block access list stats", "number", number,
		"blocks", s.blocks, "txs", s.txs, "bytes", s.bytes, "bytesPerBlock", perBlock, "bytesPerTx", perTx,
		"failures", s.failures, "gaps", s.gaps, "verify", common.PrettyDuration(s.verifyTime),
		"totalBlocks", r.stats.blocks, "totalBytes", r.stats.bytes, "totalFailures", r.stats.failures,
		"freezerSize", size)
	*s = balStats{}
}

func (r *balRecorder) close(number uint64) {
	if r.sinceLog.blocks > 0 || r.sinceLog.failures > 0 || r.sinceLog.gaps > 0 {
		r.logStats(number)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending != nil {
		r.pending.Close()
	}
	if err := r.freezer.Close(); err != nil {
		logger.Error("Failed to close the BAL freezer", "err", err)
	}
}
