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
	"sync/atomic"
	"time"

	"github.com/kaiachain/kaia/blockchain/checkpoint"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	storecp "github.com/kaiachain/kaia/storage/database/checkpoint"
)

// stateCheckpointer writes a state checkpoint, as an increment of the latest
// one, for every block the trigger picks (experimental).
type stateCheckpointer struct {
	store   *storecp.Store
	trigger *checkpoint.Trigger
	busy    atomic.Bool // an export is running, live pruning waits
}

func newStateCheckpointer(dir, yearly string, interval uint64) (*stateCheckpointer, error) {
	trigger, err := checkpoint.ParseTrigger(yearly, interval)
	if err != nil {
		return nil, err
	}
	store, err := storecp.Open(dir, nil)
	if err != nil {
		return nil, err
	}
	if latest, ok := store.Latest(); ok {
		logger.Info("Opened state checkpoints", "dir", dir, "latest", latest.BlockNumber, "id", latest.ID)
	}
	return &stateCheckpointer{store: store, trigger: trigger}, nil
}

// checkpointTrigger returns whether block gets a state checkpoint, and why.
func (bc *BlockChain) checkpointTrigger(block *types.Block) (storecp.Trigger, bool) {
	if bc.checkpoints == nil {
		return "", false
	}
	return bc.checkpoints.trigger.Match(bc.GetHeaderByHash(block.ParentHash()), block.Header())
}

// pruningPaused reports whether live pruning must wait for a checkpoint
// export, which reads trie nodes that pruning may delete.
func (bc *BlockChain) pruningPaused() bool {
	return bc.checkpoints != nil && bc.checkpoints.busy.Load()
}

// startCheckpoint exports the state of block, whose trie was just committed,
// in the background. It must be called before the block is sent to live
// pruning.
func (bc *BlockChain) startCheckpoint(block *types.Block, trigger storecp.Trigger) {
	c := bc.checkpoints
	if !c.busy.CompareAndSwap(false, true) {
		logger.Warn("Skipped a state checkpoint, the previous one is still running", "number", block.NumberU64())
		return
	}
	meta := storecp.Meta{ID: 1, BlockNumber: block.NumberU64(), BlockHash: block.Hash(), StateRoot: block.Root(), Trigger: trigger}
	if latest, ok := c.store.Latest(); ok {
		if latest.BlockNumber >= meta.BlockNumber {
			c.busy.Store(false)
			return
		}
		meta.ID, meta.Base = latest.ID+1, latest.ID
	}
	trieDB := bc.stateCache.TrieDB()
	bc.wg.Go(func() {
		defer c.busy.Store(false)
		start := time.Now()
		logger.Info("Exporting a state checkpoint", "number", meta.BlockNumber, "id", meta.ID, "base", meta.Base, "trigger", trigger)
		out, stats, err := checkpoint.Export(c.store, meta, trieDB.Node, bc.stateCache.ContractCode, bc.quit)
		if err != nil {
			logger.Error("Failed to export a state checkpoint", "number", meta.BlockNumber, "err", err)
			return
		}
		logger.Info("Exported a state checkpoint", "number", out.BlockNumber, "id", out.ID, "base", out.Base,
			"nodes", stats.Nodes, "tombstones", stats.Tombstones, "codes", stats.Codes,
			"size", common.StorageSize(stats.Bytes), "elapsed", time.Since(start))
	})
}
