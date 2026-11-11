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
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

func TestCheckHistoryMode(t *testing.T) {
	db := database.NewMemoryDBManager()
	require.ErrorContains(t, CheckHistoryMode(db, "archive"), "unknown history mode")
	require.NoError(t, CheckHistoryMode(db, HistoryModeHistory))
	require.NoError(t, CheckHistoryMode(db, HistoryModeHistory))
	require.ErrorContains(t, CheckHistoryMode(db, HistoryModeFull), "the database is of a history node")
	require.ErrorContains(t, CheckHistoryMode(db, ""), "the database is of a history node")

	// A database made before the modes, with blocks, is a full one.
	env := newBALTestEnv(t, "prague", 1)
	env.generate(2, func(int, *blockchain.BlockGen) {})
	require.ErrorContains(t, CheckHistoryMode(env.db, HistoryModeHistory), "the database is of a full node")
	require.NoError(t, CheckHistoryMode(env.db, HistoryModeFull))
}

// TestFillHeaderSegmentRoots stores the root of the block hashes of every
// finished segment that has none, from the canonical hashes.
func TestFillHeaderSegmentRoots(t *testing.T) {
	db := database.NewMemoryDBManager()
	head := uint64(3*SegmentSize - 1)
	for n := uint64(0); n <= head; n++ {
		db.WriteCanonicalHash(common.BigToHash(new(big.Int).SetUint64(n+1)), n)
	}
	kept := common.Hash{0xaa}
	require.NoError(t, WriteHeaderSegmentRoot(db, 1, kept))
	c := &BALModule{db: db, cfg: &Config{HeaderSegmentRoots: true}}

	require.True(t, c.headerSegmentRootsFilled(SegmentSize-1), "no segment is finished")
	require.NoError(t, c.fillHeaderSegmentRoots(SegmentSize-1, nil))
	_, ok := ReadHeaderSegmentRoot(db, 0)
	require.False(t, ok, "the head is not past segment 0")

	require.False(t, c.headerSegmentRootsFilled(head))
	require.NoError(t, c.fillHeaderSegmentRoots(head, nil))
	require.True(t, c.headerSegmentRootsFilled(head))
	hashes := make([]common.Hash, SegmentSize)
	for n := range hashes {
		hashes[n] = db.ReadCanonicalHash(uint64(n))
	}
	want, err := SegmentRoot(hashes)
	require.NoError(t, err)
	got, ok := ReadHeaderSegmentRoot(db, 0)
	require.True(t, ok)
	require.Equal(t, want, got)
	got, _ = ReadHeaderSegmentRoot(db, 1)
	require.Equal(t, kept, got, "a stored root is kept")
	_, ok = ReadHeaderSegmentRoot(db, 2)
	require.False(t, ok, "the head is not past segment 2")

	// A missing canonical hash stops the fill.
	db.DeleteCanonicalHash(2*SegmentSize + 5)
	require.ErrorContains(t, c.fillHeaderSegmentRoots(head+1, nil), "no canonical hash")
	require.False(t, c.headerSegmentRootsFilled(head+1))

	full := &BALModule{db: db, cfg: &Config{}}
	require.True(t, full.headerSegmentRootsFilled(head+1), "a full node does not fill")
}
