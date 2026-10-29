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
	"fmt"
	"time"

	"github.com/kaiachain/kaia/blockchain/bas"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
)

const basStatsInterval = 10_000 // blocks between stats logs

// basRecorder appends the block access set of every inserted block to a BAS
// freezer under dir, after checking that the set reproduces the state root.
// Item N of the freezer is the set of block N. It is an experimental tool to
// measure BAS on real chains.
type basRecorder struct {
	freezer *bas.Freezer

	stats    basStats
	sinceLog basStats
}

type basStats struct {
	blocks, txs, bytes, failures, gaps uint64
	verifyTime                         time.Duration
}

func newBASRecorder(dir string) (*basRecorder, error) {
	freezer, err := bas.OpenFreezer(dir, false)
	if err != nil {
		return nil, err
	}
	logger.Warn("Recording block access sets (experimental)", "dir", dir, "next", freezer.Next())
	return &basRecorder{freezer: freezer}, nil
}

// record encodes the set recorded by statedb, applies it to the parent state
// and appends it only if the resulting root matches the block.
func (r *basRecorder) record(bc *BlockChain, block *types.Block, parentRoot common.Hash, statedb *state.StateDB) {
	number := block.NumberU64()
	if next := r.freezer.Next(); number < next {
		return // already stored, e.g. a block inserted again after a rewind
	} else if number > next {
		// The freezer has no gaps, so nothing can be recorded until the
		// blocks before this one are.
		if r.sinceLog.gaps == 0 {
			logger.Warn("Block access set not recorded: missing earlier blocks", "number", number, "next", next)
		}
		r.sinceLog.gaps++
		return
	}

	enc, err := statedb.BlockAccessSet().Encode()
	if err != nil {
		logger.Error("Failed to encode block access set", "number", number, "err", err)
		r.sinceLog.failures++
		return
	}

	start := time.Now()
	err = verifyBAS(bc, block, parentRoot, enc)
	r.sinceLog.verifyTime += time.Since(start)
	if err != nil {
		logger.Error("Block access set failed the self-check", "number", number, "hash", block.Hash(), "err", err)
		r.sinceLog.failures++
		return
	}

	if err := r.freezer.Append(number, enc); err != nil {
		logger.Error("Failed to write block access set", "number", number, "err", err)
		r.sinceLog.failures++
		return
	}
	r.sinceLog.blocks++
	r.sinceLog.txs += uint64(len(block.Transactions()))
	r.sinceLog.bytes += uint64(len(enc))
	if r.sinceLog.blocks >= basStatsInterval {
		r.logStats(number)
	}
}

func verifyBAS(bc *BlockChain, block *types.Block, parentRoot common.Hash, enc []byte) error {
	set, err := bas.Decode(enc)
	if err != nil {
		return err
	}
	statedb, err := bc.StateAt(parentRoot)
	if err != nil {
		return err
	}
	if err := statedb.ApplyBlockAccessSet(set); err != nil {
		return err
	}
	if root := statedb.IntermediateRoot(true); root != block.Root() {
		return fmt.Errorf("root mismatch: have %s, want %s", root.Hex(), block.Root().Hex())
	}
	return nil
}

func (r *basRecorder) logStats(number uint64) {
	if err := r.freezer.Sync(); err != nil {
		logger.Error("Failed to sync the BAS freezer", "err", err)
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
	logger.Info("Block access set stats", "number", number,
		"blocks", s.blocks, "txs", s.txs, "bytes", s.bytes, "bytesPerBlock", perBlock, "bytesPerTx", perTx,
		"failures", s.failures, "gaps", s.gaps, "verify", common.PrettyDuration(s.verifyTime),
		"totalBlocks", r.stats.blocks, "totalBytes", r.stats.bytes, "totalFailures", r.stats.failures,
		"freezerSize", size)
	*s = basStats{}
}

func (r *basRecorder) close(number uint64) {
	if r.sinceLog.blocks > 0 || r.sinceLog.failures > 0 || r.sinceLog.gaps > 0 {
		r.logStats(number)
	}
	if err := r.freezer.Close(); err != nil {
		logger.Error("Failed to close the BAS freezer", "err", err)
	}
}
