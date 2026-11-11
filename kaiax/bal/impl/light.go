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

package impl

import (
	"fmt"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
)

// History modes of a node. A history node keeps its state with live pruning
// and starts from genesis by applying the block access lists of its blocks.
const (
	HistoryModeFull    = "full"
	HistoryModeHistory = "history"
)

// CheckHistoryMode checks that a database is used in the mode it was made
// for, and records the mode of a new one. A database made before the modes is
// a full one.
func CheckHistoryMode(db database.DBManager, mode string) error {
	switch mode {
	case "":
		mode = HistoryModeFull
	case HistoryModeFull, HistoryModeHistory:
	default:
		return fmt.Errorf("unknown history mode %q, want full or history", mode)
	}
	stored := ReadHistoryMode(db)
	if stored == "" {
		if head := db.ReadHeaderNumber(db.ReadHeadBlockHash()); head != nil && *head > 0 {
			stored = HistoryModeFull
		} else {
			return WriteHistoryMode(db, mode)
		}
	}
	if stored != mode {
		return fmt.Errorf("the database is of a %s node; a %s node starts as a new node", stored, mode)
	}
	return nil
}

// writeHeaderSegmentRoot stores the root of the block hashes of a segment.
func (c *BALModule) writeHeaderSegmentRoot(k uint64) error {
	hashes := make([]common.Hash, 0, SegmentSize)
	for n := k * SegmentSize; n < (k+1)*SegmentSize; n++ {
		hash := c.db.ReadCanonicalHash(n)
		if hash == (common.Hash{}) {
			return fmt.Errorf("no canonical hash of block %d for the root of segment %d", n, k)
		}
		hashes = append(hashes, hash)
	}
	root, err := SegmentRoot(hashes)
	if err != nil {
		return err
	}
	return WriteHeaderSegmentRoot(c.db, k, root)
}

// HeaderSegmentRoot returns the stored root of the block hashes of a segment.
func HeaderSegmentRoot(bc *blockchain.BlockChain, k uint64) (common.Hash, bool) {
	return ReadHeaderSegmentRoot(of(bc).db, k)
}

// fillHeaderSegmentRoots stores the root of the block hashes of every
// finished segment, one the head is past the end of, that has none.
func (c *BALModule) fillHeaderSegmentRoots(head uint64, stop <-chan struct{}) error {
	finished := head / SegmentSize
	from, filled := c.hsegDone.Load(), 0
	for k := from; k < finished; k++ {
		select {
		case <-stop:
			return nil
		default:
		}
		if _, ok := ReadHeaderSegmentRoot(c.db, k); !ok {
			if err := c.writeHeaderSegmentRoot(k); err != nil {
				return err
			}
			filled++
			if filled%1024 == 0 {
				logger.Info("Storing the roots of the header segments", "segment", k, "finished", finished)
			}
		}
		c.hsegDone.Store(k + 1)
	}
	if filled > 0 {
		logger.Info("Stored the roots of the header segments", "stored", filled, "segments", finished)
	}
	return nil
}

// HeaderSegmentRootsFilled reports whether every finished segment has the
// root of its block hashes stored, which a history node needs to be synced.
func HeaderSegmentRootsFilled(bc *blockchain.BlockChain) bool {
	return of(bc).headerSegmentRootsFilled(bc.CurrentBlock().NumberU64())
}

func (c *BALModule) headerSegmentRootsFilled(head uint64) bool {
	return !c.cfg.HeaderSegmentRoots || c.hsegDone.Load() >= head/SegmentSize
}

// headerSegmentInterval is how often a history node stores the roots of the
// segments finished since.
var headerSegmentInterval = 2 * time.Second

func (c *BALModule) headerSegmentLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(headerSegmentInterval)
	defer ticker.Stop()
	for {
		if err := c.fillHeaderSegmentRoots(c.bc.CurrentBlock().NumberU64(), c.quit); err != nil {
			logger.Warn("Failed to store the roots of the header segments", "err", err)
		}
		select {
		case <-c.quit:
			return
		case <-ticker.C:
		}
	}
}
