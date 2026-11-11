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
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPruningMarks_Freezer checks that with the block freezer, pruning marks
// are appended to the pruning mark freezer and consumed from its tail, and that
// marks in the misc database are still consumed.
func TestPruningMarks_Freezer(t *testing.T) {
	dir := t.TempDir()
	dbc := newFreezerTestConfig(dir, true, 10)
	dbm := openFreezerTestDBM(t, dbc)
	require.NotNil(t, dbm.pruningMarks)

	node := func(i byte) common.ExtHash { return common.Hash{i}.ExtendZero() }
	trieDB := dbm.getDatabase(StateTrieDB)
	for i := byte(1); i <= 6; i++ {
		require.NoError(t, trieDB.Put(TrieNodeKey(node(i)), []byte{i}))
	}
	hasNode := func(i byte) bool {
		has, _ := trieDB.Has(TrieNodeKey(node(i)))
		return has
	}
	require.NoError(t, dbm.getDatabase(MiscDB).Put(pruningMarkKey(PruningMark{2, node(6)}), pruningMarkValue))
	dbm.WritePruningMarks([]PruningMark{{1, node(1)}, {2, node(2)}})
	dbm.WritePruningMarks(nil)
	dbm.WritePruningMarks([]PruningMark{{3, node(3)}})
	dbm.WritePruningMarks([]PruningMark{{5, node(5)}, {4, node(4)}})
	items, _ := dbm.pruningMarks.Ancients()
	assert.Equal(t, uint64(3), items)
	assert.Empty(t, dbm.ReadPruningMarks(3, 0), "marks are not put to the misc database")

	assert.Equal(t, 4, dbm.PruneMarkedTrieNodes(1, 4, nil))
	for i := byte(1); i <= 6; i++ {
		assert.Equal(t, i == 4 || i == 5, hasNode(i), "node %d", i)
	}
	assert.Empty(t, dbm.ReadPruningMarks(1, 0))
	dbm.Close()

	dbm = openFreezerTestDBM(t, dbc)
	defer dbm.Close()
	tail, _ := dbm.pruningMarks.Tail("marks")
	assert.Equal(t, uint64(2), tail)
	assert.Equal(t, 0, dbm.PruneMarkedTrieNodes(4, 5, nil))
	// The marks are handed to the given prune function, as the BAL hooks do.
	var pruned []PruningMark
	assert.Equal(t, 2, dbm.PruneMarkedTrieNodes(4, 6, func(marks []PruningMark) {
		pruned = append(pruned, marks...)
		dbm.PruneTrieNodes(marks)
	}))
	assert.ElementsMatch(t, []PruningMark{{5, node(5)}, {4, node(4)}}, pruned)
	assert.False(t, hasNode(4))
	assert.False(t, hasNode(5))
	assert.Equal(t, 0, dbm.PruneMarkedTrieNodes(6, 10, nil))

	// Without the freezer, the marks stay in the misc database.
	mem := NewMemoryDBManager()
	mem.WritePruningMarks([]PruningMark{{1, node(1)}})
	assert.Len(t, mem.ReadPruningMarks(1, 0), 1)
	assert.Equal(t, 1, mem.PruneMarkedTrieNodes(1, 2, nil))
	assert.Empty(t, mem.ReadPruningMarks(1, 0))
}
