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
	"fmt"
	"sync"
	"time"

	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
)

const statsInterval = 10_000 // blocks between stats logs

// recorder stores the block access list of every inserted block in the
// chain database, after checking that the list reproduces the state root: its
// writes, and its reads apart unless they are skipped. It is an experimental
// tool to measure BAL on real chains.
//
// It also keeps the range of blocks whose lists are all stored. A list that
// does not touch the range, like the one of a block inserted while a
// regeneration fills in earlier blocks, is stored and joins the range once the
// blocks in between are.
type recorder struct {
	db        database.DBManager
	skipReads bool // only the writes are stored

	mu          sync.Mutex // guards first and next
	first, next uint64     // lists of blocks first to next-1 are stored; first == next if none

	stats    recorderStats
	sinceLog recorderStats
}

type recorderStats struct {
	blocks, txs, bytes, readBytes, failures uint64
	verifyTime                              time.Duration
}

func newRecorder(db database.DBManager, skipReads bool) *recorder {
	r := &recorder{db: db, skipReads: skipReads}
	r.first, r.next = db.ReadBALRange()
	r.mu.Lock()
	r.extendLocked()
	r.mu.Unlock()
	logger.Warn("Recording block access lists (experimental)", "first", r.first, "next", r.next, "skipReads", skipReads)
	return r
}

// span returns the range of blocks whose lists are all stored.
func (r *recorder) span() (first, next uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.first, r.next
}

// store stores the writes of the list of a block, and its reads unless they
// are nil, knowing that the lists of blocks from to number are now all stored,
// and joins them to the range if they touch it or if it is empty.
func (r *recorder) store(number uint64, hash common.Hash, writes, reads []byte, from uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if reads != nil {
		if err := r.db.WriteBALReads(hash, number, reads); err != nil {
			return err
		}
	}
	if err := r.db.WriteBAL(hash, number, writes); err != nil {
		return err
	}
	switch {
	case r.first == r.next:
		r.first, r.next = from, number+1
	case from <= r.next && number+1 >= r.first:
		r.first, r.next = min(r.first, from), max(r.next, number+1)
	}
	r.extendLocked()
	return nil
}

// extendLocked joins the stored lists that follow the range and saves it.
func (r *recorder) extendLocked() {
	if r.first == r.next {
		return
	}
	for r.db.HasBAL(r.db.ReadCanonicalHash(r.next), r.next) {
		r.next++
	}
	if first, next := r.db.ReadBALRange(); first != r.first || next != r.next {
		if err := r.db.WriteBALRange(r.first, r.next); err != nil {
			logger.Error("Failed to write the block access list range", "err", err)
		}
	}
}

// truncate drops the blocks from number on from the range, for a rewind.
func (r *recorder) truncate(number uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if number >= r.next {
		return
	}
	if number <= r.first {
		r.first, r.next = 0, 0
	} else {
		r.next = number
	}
	if err := r.db.WriteBALRange(r.first, r.next); err != nil {
		logger.Error("Failed to write the block access list range", "err", err)
	}
}

// record encodes the list recorded by statedb, applies it to the parent state
// and stores it only if the resulting root matches the block.
func (r *recorder) record(c *chain, block *types.Block, parentRoot common.Hash, statedb *state.StateDB) {
	number, hash := block.NumberU64(), block.Hash()
	if first, next := r.span(); (first <= number && number < next) || r.db.HasBAL(hash, number) {
		return // already stored, e.g. a block inserted again after a rewind
	}

	list := access.Recorded(statedb)
	enc, err := list.Writes().Encode()
	var reads []byte
	if err == nil && !r.skipReads {
		reads, err = list.Reads().Encode()
	}
	if err != nil {
		logger.Error("Failed to encode block access list", "number", number, "err", err)
		r.sinceLog.failures++
		return
	}

	start := time.Now()
	err = verifyBAL(c, block, parentRoot, enc)
	r.sinceLog.verifyTime += time.Since(start)
	if err != nil {
		logger.Error("Block access list failed the self-check", "number", number, "hash", hash, "err", err)
		r.sinceLog.failures++
		return
	}

	if err := r.store(number, hash, enc, reads, number); err != nil {
		logger.Error("Failed to write block access list", "number", number, "err", err)
		r.sinceLog.failures++
		return
	}
	c.signalIndex()
	r.sinceLog.blocks++
	r.sinceLog.txs += uint64(len(block.Transactions()))
	r.sinceLog.bytes += uint64(len(enc))
	r.sinceLog.readBytes += uint64(len(reads))
	if r.sinceLog.blocks >= statsInterval {
		r.logStats(number)
	}
}

func verifyBAL(c *chain, block *types.Block, parentRoot common.Hash, enc []byte) error {
	list, err := access.Decode(enc)
	if err != nil {
		return err
	}
	statedb, err := c.bc.StateAt(parentRoot)
	if err != nil {
		return err
	}
	if err := access.Apply(statedb, list); err != nil {
		return err
	}
	if root := statedb.IntermediateRoot(true); root != block.Root() {
		return fmt.Errorf("root mismatch: have %s, want %s", root.Hex(), block.Root().Hex())
	}
	return nil
}

func (r *recorder) logStats(number uint64) {
	s := &r.sinceLog
	r.stats.blocks += s.blocks
	r.stats.txs += s.txs
	r.stats.bytes += s.bytes
	r.stats.readBytes += s.readBytes
	r.stats.failures += s.failures
	r.stats.verifyTime += s.verifyTime

	perBlock, perTx := uint64(0), uint64(0)
	if s.blocks > 0 {
		perBlock = s.bytes / s.blocks
	}
	if s.txs > 0 {
		perTx = s.bytes / s.txs
	}
	first, next := r.span()
	logger.Info("Block access list stats", "number", number,
		"blocks", s.blocks, "txs", s.txs, "bytes", s.bytes, "bytesPerBlock", perBlock, "bytesPerTx", perTx, "readBytes", s.readBytes,
		"failures", s.failures, "verify", common.PrettyDuration(s.verifyTime),
		"totalBlocks", r.stats.blocks, "totalBytes", r.stats.bytes, "totalFailures", r.stats.failures,
		"first", first, "next", next)
	*s = recorderStats{}
}

func (r *recorder) close(number uint64) {
	if r.sinceLog.blocks > 0 || r.sinceLog.failures > 0 {
		r.logStats(number)
	}
}

// trimFront drops the blocks before number from the range, whose lists were
// deleted.
func (r *recorder) trimFront(number uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if number <= r.first || r.first == r.next {
		return
	}
	r.first = min(number, r.next)
	if r.first == r.next {
		r.first, r.next = 0, 0
	}
	if err := r.db.WriteBALRange(r.first, r.next); err != nil {
		logger.Error("Failed to write the block access list range", "err", err)
	}
}
