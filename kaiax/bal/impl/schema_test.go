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
	"testing"

	"github.com/kaiachain/kaia/blockchain/state/bal"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/assert"
)

func TestSchemaBAL(t *testing.T) {
	db := database.NewMemoryDBManager()
	hash := common.HexToHash("0x1234")
	enc := []byte{0xc1, 0xc0}

	assert.Nil(t, bal.ReadBAL(db, hash, 1))
	assert.False(t, bal.HasBAL(db, hash, 1))

	assert.NoError(t, bal.WriteBAL(db, hash, 1, enc))
	assert.Equal(t, enc, bal.ReadBAL(db, hash, 1))
	assert.True(t, bal.HasBAL(db, hash, 1))
	assert.Nil(t, bal.ReadBAL(db, hash, 2))

	reads := []byte{0xc1, 0xc1}
	assert.Nil(t, bal.ReadBALReads(db, hash, 1))
	assert.NoError(t, bal.WriteBALReads(db, hash, 1, reads))
	assert.Equal(t, reads, bal.ReadBALReads(db, hash, 1))
	assert.Equal(t, enc, bal.ReadBAL(db, hash, 1), "the writes are kept apart")

	bal.DeleteBAL(db, hash, 1)
	assert.Nil(t, bal.ReadBAL(db, hash, 1))
	assert.Equal(t, reads, bal.ReadBALReads(db, hash, 1))
	bal.DeleteBALReads(db, hash, 1)
	assert.Nil(t, bal.ReadBALReads(db, hash, 1))

	first, next := bal.ReadBALRange(db)
	assert.Equal(t, [2]uint64{0, 0}, [2]uint64{first, next})
	assert.NoError(t, bal.WriteBALRange(db, 3, 9))
	first, next = bal.ReadBALRange(db)
	assert.Equal(t, [2]uint64{3, 9}, [2]uint64{first, next})
}

func TestSchemaHistory(t *testing.T) {
	db := database.NewMemoryDBManager()

	assert.Equal(t, "", bal.ReadHistoryMode(db))
	assert.NoError(t, bal.WriteHistoryMode(db, HistoryModeLight))
	assert.Equal(t, HistoryModeLight, bal.ReadHistoryMode(db))

	assert.Equal(t, uint64(0), bal.ReadHistoryFirst(db))
	assert.NoError(t, bal.WriteHistoryFirst(db, 42))
	assert.Equal(t, uint64(42), bal.ReadHistoryFirst(db))

	_, ok := bal.ReadHeaderSegmentRoot(db, 3)
	assert.False(t, ok)
	root := common.HexToHash("0xabcd")
	assert.NoError(t, bal.WriteHeaderSegmentRoot(db, 3, root))
	got, ok := bal.ReadHeaderSegmentRoot(db, 3)
	assert.True(t, ok)
	assert.Equal(t, root, got)
}
