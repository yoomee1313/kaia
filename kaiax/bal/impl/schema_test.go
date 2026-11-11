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

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/assert"
)

func TestSchemaBAL(t *testing.T) {
	db := database.NewMemoryDBManager()
	hash := common.HexToHash("0x1234")
	enc := []byte{0xc1, 0xc0}

	assert.Nil(t, ReadBAL(db, hash, 1))
	assert.False(t, HasBAL(db, hash, 1))

	assert.NoError(t, WriteBAL(db, hash, 1, enc))
	assert.Equal(t, enc, ReadBAL(db, hash, 1))
	assert.True(t, HasBAL(db, hash, 1))
	assert.Nil(t, ReadBAL(db, hash, 2))

	reads := []byte{0xc1, 0xc1}
	assert.Nil(t, ReadBALReads(db, hash, 1))
	assert.NoError(t, WriteBALReads(db, hash, 1, reads))
	assert.Equal(t, reads, ReadBALReads(db, hash, 1))
	assert.Equal(t, enc, ReadBAL(db, hash, 1), "the writes are kept apart")

	DeleteBAL(db, hash, 1)
	assert.Nil(t, ReadBAL(db, hash, 1))
	assert.Equal(t, reads, ReadBALReads(db, hash, 1))
	DeleteBALReads(db, hash, 1)
	assert.Nil(t, ReadBALReads(db, hash, 1))

	first, next := ReadBALRange(db)
	assert.Equal(t, [2]uint64{0, 0}, [2]uint64{first, next})
	assert.NoError(t, WriteBALRange(db, 3, 9))
	first, next = ReadBALRange(db)
	assert.Equal(t, [2]uint64{3, 9}, [2]uint64{first, next})
}
