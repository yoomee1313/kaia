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

package database

import (
	"path/filepath"

	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database/ancient"
)

// With the block freezer enabled, the KIP-111 pruning marks are appended to a
// freezer of their own, one item per flush of the trie database, instead of
// being put to and deleted from the misc database one by one. The prune loop
// consumes the items from the tail and truncates it. Marks in the misc
// database, written before, are still consumed.

// pruningMarkDirKey names the pruning mark freezer once a mark was written to it.
var pruningMarkDirKey = []byte("PruningMarkDir")

// The prune loop consumes at most pruningMarkBatchItems items or
// pruningMarkBatchBytes bytes of pruning marks at once.
const (
	pruningMarkBatchItems = 1024
	pruningMarkBatchBytes = 64 * 1024 * 1024
)

// openPruningMarkFreezer opens the pruning mark freezer next to the block
// freezer if the block freezer is writable.
func (dbm *databaseManager) openPruningMarkFreezer() error {
	marker, _ := dbm.getDatabase(MiscDB).Get(pruningMarkDirKey)
	if dbm.freezer == nil || dbm.freezerQuit == nil {
		if len(marker) != 0 {
			logger.Warn("Pruning marks are in a freezer that is not opened; their trie nodes are not pruned", "dir", string(marker))
		}
		return nil
	}
	dir := filepath.Join(filepath.Dir(dbm.freezerDir()), ancient.PruningMarkFreezerName)
	if len(marker) != 0 && string(marker) != dir {
		dir = string(marker)
	}
	f, err := ancient.NewPruningMarkFreezer(dir, dbMetricPrefix+"pruning/", false)
	if err != nil {
		return err
	}
	dbm.pruningMarks = f
	return nil
}

// closePruningMarkFreezer closes the pruning mark freezer.
func (dbm *databaseManager) closePruningMarkFreezer() {
	if dbm.pruningMarks == nil {
		return
	}
	if err := dbm.pruningMarks.Close(); err != nil {
		logger.Error("Failed to close pruning mark freezer", "err", err)
	}
}

// appendPruningMarks appends the marks of a flush as one item.
func (dbm *databaseManager) appendPruningMarks(marks []PruningMark) {
	if len(marks) == 0 {
		return
	}
	data, err := rlp.EncodeToBytes(marks)
	if err != nil {
		logger.Crit("Failed to encode trie pruning marks", "err", err)
	}
	dbm.pruningMarkDirOnce.Do(func() {
		dir, _ := dbm.pruningMarks.AncientDatadir()
		if err := dbm.getDatabase(MiscDB).Put(pruningMarkDirKey, []byte(dir)); err != nil {
			logger.Crit("Failed to store the pruning mark freezer directory", "err", err)
		}
	})
	if _, err := dbm.pruningMarks.ModifyAncients(func(op ancient.AncientWriteOp) error {
		next, _ := dbm.pruningMarks.Ancients()
		return op.AppendRaw(ancient.PruningMarkTable, next, data)
	}); err != nil {
		logger.Crit("Failed to store trie pruning marks", "err", err)
	}
}

// PruneMarkedTrieNodes hands the marks of the blocks [startNumber, endNumber)
// to prune, which deletes their trie nodes, then deletes the marks, and
// returns the number of marks. A nil prune deletes the nodes of every mark.
// The marks in the pruning mark freezer are consumed from its tail, an item
// at a time, as long as all marks of the item are below endNumber.
func (dbm *databaseManager) PruneMarkedTrieNodes(startNumber, endNumber uint64, prune func([]PruningMark)) int {
	if prune == nil {
		prune = dbm.PruneTrieNodes
	}
	marks := dbm.ReadPruningMarks(startNumber, endNumber)
	if len(marks) != 0 {
		prune(marks)
		dbm.DeletePruningMarks(marks)
	}
	count := len(marks)
	if dbm.pruningMarks != nil {
		count += dbm.pruneFrozenMarks(endNumber, prune)
	}
	return count
}

// pruneFrozenMarks consumes the items of the pruning mark freezer whose marks
// are all below endNumber.
func (dbm *databaseManager) pruneFrozenMarks(endNumber uint64, prune func([]PruningMark)) int {
	count := 0
	for {
		tail, _ := dbm.pruningMarks.Tail(ancient.PruningMarkGroup)
		head, _ := dbm.pruningMarks.Ancients()
		if tail >= head {
			return count
		}
		items, err := dbm.pruningMarks.AncientRange(ancient.PruningMarkTable, tail, min(head-tail, pruningMarkBatchItems), pruningMarkBatchBytes)
		if err != nil {
			logger.Error("Failed to read trie pruning marks", "from", tail, "err", err)
			return count
		}
		var marks []PruningMark
		next := tail
		for _, item := range items {
			var itemMarks []PruningMark
			if err := rlp.DecodeBytes(item, &itemMarks); err != nil {
				logger.Error("Invalid trie pruning marks", "item", next, "err", err)
				return count
			}
			if !marksBelow(itemMarks, endNumber) {
				break
			}
			marks = append(marks, itemMarks...)
			next++
		}
		if next == tail {
			return count
		}
		prune(marks)
		if _, err := dbm.pruningMarks.TruncateTail(ancient.PruningMarkGroup, next); err != nil {
			logger.Error("Failed to truncate trie pruning marks", "to", next, "err", err)
			return count
		}
		if err := dbm.pruningMarks.SyncAncient(); err != nil {
			logger.Error("Failed to flush trie pruning marks", "err", err)
		}
		count += len(marks)
		if next < tail+uint64(len(items)) {
			return count
		}
	}
}

// marksBelow reports whether all marks are of blocks below endNumber. An
// endNumber of 0 takes every mark.
func marksBelow(marks []PruningMark, endNumber uint64) bool {
	if endNumber == 0 {
		return true
	}
	for _, mark := range marks {
		if mark.Number >= endNumber {
			return false
		}
	}
	return true
}
