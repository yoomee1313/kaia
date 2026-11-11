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
	"errors"
	"fmt"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
)

// History modes of a node. A history or light node keeps its state with live
// pruning and starts from genesis by applying the block access lists of its
// blocks. A light node keeps the blocks, receipts and lists of a retention
// period only: of an older block it keeps the canonical hash, and the header
// if the gov or valset module needs it, and of an older segment the root of
// its block hashes.
const (
	HistoryModeFull    = "full"
	HistoryModeHistory = "history"
	HistoryModeLight   = "light"
)

// ErrPrunedHistory is returned for the data of a block a light node deleted.
var ErrPrunedHistory = errors.New("block data is pruned")

// CheckHistoryMode checks that a database is used in the mode it was made
// for, and records the mode of a new one. A database made before the modes is
// a full one.
func CheckHistoryMode(db database.DBManager, mode string) error {
	switch mode {
	case "":
		mode = HistoryModeFull
	case HistoryModeFull, HistoryModeHistory, HistoryModeLight:
	default:
		return fmt.Errorf("unknown history mode %q, want full, history or light", mode)
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

// HistoryFirst returns the first block whose header, body and receipts are
// held. The data of earlier blocks, but the genesis block, was deleted.
func HistoryFirst(bc *blockchain.BlockChain) uint64 { return of(bc).historyFirst() }

func (c *BALModule) historyFirst() uint64 { return ReadHistoryFirst(c.db) }

// PrunedError returns ErrPrunedHistory for a block before the held ones.
func PrunedError(bc *blockchain.BlockChain, number uint64) error { return of(bc).prunedError(number) }

func (c *BALModule) prunedError(number uint64) error {
	if first := ReadHistoryFirst(c.db); number > 0 && number < first {
		return fmt.Errorf("%w: this node holds blocks from %d", ErrPrunedHistory, first)
	}
	return nil
}

// MinHistoryBlocks is the least number of blocks before the head a light node
// keeps.
var MinHistoryBlocks uint64 = 128

// PruneHistory deletes the blocks of a light node older than its retention
// period, as it does every minute.
func PruneHistory(bc *blockchain.BlockChain) error {
	c := of(bc)
	return c.pruneHistory(c.quit)
}

// pruneHistory deletes the blocks older than the retention period of a light
// node: their headers, bodies, receipts, block access lists, total
// difficulties, transaction lookups and bloom bits sections. It keeps their
// canonical hashes, and stores the root of the block hashes of every segment
// before the first kept block.
func (c *BALModule) pruneHistory(stop <-chan struct{}) error {
	retention := c.cfg.HistoryRetention
	head := c.bc.CurrentBlock().Header()
	if retention <= 0 || head.Number.Uint64() <= MinHistoryBlocks {
		return nil
	}
	first := max(ReadHistoryFirst(c.db), 1)
	cutoffTime := head.Time.Uint64() - min(head.Time.Uint64(), uint64(retention/time.Second))
	// The first block at or after the cutoff time; the headers from first on
	// are held, and their times do not decrease.
	lo, hi := first, head.Number.Uint64()-MinHistoryBlocks
	for lo < hi {
		mid := lo + (hi-lo)/2
		if h := c.bc.GetHeaderByNumber(mid); h != nil && h.Time.Uint64() >= cutoffTime {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	cutoff := lo
	if cutoff <= first {
		return nil
	}
	for n := first; n < cutoff; n++ {
		select {
		case <-stop:
			return WriteHistoryFirst(c.db, n)
		default:
		}
		hash := c.db.ReadCanonicalHash(n)
		if body := c.db.ReadBody(hash, n); body != nil {
			for _, tx := range body.Transactions {
				c.db.DeleteTxLookupEntry(tx.Hash())
			}
		}
		c.db.DeleteBody(hash, n)
		c.db.DeleteReceipts(hash, n)
		DeleteBAL(c.db, hash, n)
		DeleteBALReads(c.db, hash, n)
		if header := c.db.ReadHeader(hash, n); header == nil || !keepsHeader(header) {
			c.db.DeleteTd(hash, n)
			c.db.DeleteHeader(hash, n)
		}
		if (n+1)%SegmentSize == 0 {
			if err := c.writeHeaderSegmentRoot(Segment(n)); err != nil {
				return err
			}
		}
		if (n+1)%params.BloomBitsBlocks == 0 {
			section := n / params.BloomBitsBlocks
			for bit := uint(0); bit < bloomBits; bit++ {
				c.db.DeleteBloomBits(database.BloomBitsKey(bit, section, hash))
			}
		}
		if n%1024 == 0 {
			if err := WriteHistoryFirst(c.db, n+1); err != nil {
				return err
			}
		}
	}
	if err := WriteHistoryFirst(c.db, cutoff); err != nil {
		return err
	}
	if c.rec != nil {
		c.rec.trimFront(cutoff)
	}
	c.purgeCaches()
	logger.Info("Deleted old blocks of a light node", "from", first, "to", cutoff-1, "retention", retention)
	return nil
}

// keepsHeader reports whether a light node keeps the header of a deleted
// block: the gov and valset modules read the headers of the blocks with votes
// or governance changes, which they index, and of the Istanbul checkpoints,
// also after a restart.
func keepsHeader(header *types.Header) bool {
	return len(header.Vote) > 0 || len(header.Governance) > 0 || params.IsCheckpointInterval(header.Number.Uint64())
}

// bloomBits is the number of bits of a bloom filter, each with a vector.
const bloomBits = 2048

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

// HeaderSegmentRoot returns the root of the block hashes of a segment whose
// blocks a light node deleted.
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
// root of its block hashes stored, which a history or light node needs to be
// synced.
func HeaderSegmentRootsFilled(bc *blockchain.BlockChain) bool {
	return of(bc).headerSegmentRootsFilled(bc.CurrentBlock().NumberU64())
}

func (c *BALModule) headerSegmentRootsFilled(head uint64) bool {
	return !c.cfg.HeaderSegmentRoots || c.hsegDone.Load() >= head/SegmentSize
}

// headerSegmentInterval is how often a history or light node stores the roots
// of the segments finished since.
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

// purgeCaches drops the cached blocks, which may be deleted ones.
func (c *BALModule) purgeCaches() {
	c.db.ClearHeaderChainCache()
	c.db.ClearBlockChainCache()
}

// historyPruneInterval is how often a light node deletes old blocks.
var historyPruneInterval = time.Minute

func (c *BALModule) historyPruneLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(historyPruneInterval)
	defer ticker.Stop()
	for {
		if err := c.pruneHistory(c.quit); err != nil {
			logger.Warn("Failed to delete old blocks", "err", err)
		}
		select {
		case <-c.quit:
			return
		case <-ticker.C:
		}
	}
}
