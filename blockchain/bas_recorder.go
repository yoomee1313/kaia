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
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kaiachain/kaia/blockchain/bas"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
)

const (
	basFileBlocks    = 100_000 // blocks per BAS file
	basStatsInterval = 10_000  // blocks between stats logs
)

// BASFileRecord is one entry of a BAS file. A file is a plain sequence of
// RLP-encoded records.
type BASFileRecord struct {
	Number uint64
	Hash   common.Hash
	Set    []byte // RLP-encoded bas.BlockAccessSet
}

// basRecorder writes the block access set of every inserted block to files
// under dir, after checking that the set reproduces the state root. It is an
// experimental tool to measure BAS on real chains.
type basRecorder struct {
	dir     string
	fileNum uint64
	file    *os.File
	w       *bufio.Writer

	stats    basStats
	sinceLog basStats
}

type basStats struct {
	blocks, txs, bytes, failures uint64
	verifyTime                   time.Duration
}

func newBASRecorder(dir string) (*basRecorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	logger.Warn("Recording block access sets (experimental)", "dir", dir)
	return &basRecorder{dir: dir}, nil
}

// record encodes the set recorded by statedb, applies it to the parent state
// and writes it out only if the resulting root matches the block.
func (r *basRecorder) record(bc *BlockChain, block *types.Block, parentRoot common.Hash, statedb *state.StateDB) {
	enc, err := statedb.BlockAccessSet().Encode()
	if err != nil {
		logger.Error("Failed to encode block access set", "number", block.NumberU64(), "err", err)
		r.sinceLog.failures++
		return
	}

	start := time.Now()
	err = verifyBAS(bc, block, parentRoot, enc)
	r.sinceLog.verifyTime += time.Since(start)
	if err != nil {
		logger.Error("Block access set failed the self-check", "number", block.NumberU64(), "hash", block.Hash(), "err", err)
		r.sinceLog.failures++
		return
	}

	if err := r.write(&BASFileRecord{Number: block.NumberU64(), Hash: block.Hash(), Set: enc}); err != nil {
		logger.Error("Failed to write block access set", "number", block.NumberU64(), "err", err)
		r.sinceLog.failures++
		return
	}
	r.sinceLog.blocks++
	r.sinceLog.txs += uint64(len(block.Transactions()))
	r.sinceLog.bytes += uint64(len(enc))
	if r.sinceLog.blocks >= basStatsInterval {
		r.logStats(block.NumberU64())
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

func (r *basRecorder) write(rec *BASFileRecord) error {
	if num := rec.Number / basFileBlocks; r.file == nil || num != r.fileNum {
		if err := r.closeFile(); err != nil {
			return err
		}
		path := filepath.Join(r.dir, fmt.Sprintf("bas-%06d.rlp", num))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		r.fileNum, r.file, r.w = num, file, bufio.NewWriter(file)
	}
	return rlp.Encode(r.w, rec)
}

func (r *basRecorder) closeFile() error {
	if r.file == nil {
		return nil
	}
	if err := r.w.Flush(); err != nil {
		return err
	}
	err := r.file.Close()
	r.file, r.w = nil, nil
	return err
}

func (r *basRecorder) logStats(number uint64) {
	s := &r.sinceLog
	r.stats.blocks += s.blocks
	r.stats.txs += s.txs
	r.stats.bytes += s.bytes
	r.stats.failures += s.failures
	r.stats.verifyTime += s.verifyTime

	perBlock, perTx := uint64(0), uint64(0)
	if s.blocks > 0 {
		perBlock = s.bytes / s.blocks
	}
	if s.txs > 0 {
		perTx = s.bytes / s.txs
	}
	logger.Info("Block access set stats", "number", number,
		"blocks", s.blocks, "txs", s.txs, "bytes", s.bytes, "bytesPerBlock", perBlock, "bytesPerTx", perTx,
		"failures", s.failures, "verify", common.PrettyDuration(s.verifyTime),
		"totalBlocks", r.stats.blocks, "totalBytes", r.stats.bytes, "totalFailures", r.stats.failures)
	*s = basStats{}
}

func (r *basRecorder) close(number uint64) {
	if r.sinceLog.blocks > 0 || r.sinceLog.failures > 0 {
		r.logStats(number)
	}
	if err := r.closeFile(); err != nil {
		logger.Error("Failed to close block access set file", "err", err)
	}
}
