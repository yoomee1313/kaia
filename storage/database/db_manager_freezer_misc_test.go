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
	"encoding/binary"
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setBloomBitsBlocks shortens the bloom bits sections for a test.
func setBloomBitsBlocks(t *testing.T, n uint64) {
	old := bloomBitsSection
	bloomBitsSection = n
	t.Cleanup(func() { bloomBitsSection = old })
}

// writeBloomSection stores the bloom bits of a few bits of a section under
// the key with the given section head hash, as the bloom indexer does.
func writeBloomSection(t *testing.T, dbm *databaseManager, section uint64, head common.Hash) {
	for bit := uint(0); bit < 3; bit++ {
		dbm.WriteBloomBits(BloomBitsKey(bit, section, head), []byte{byte(section), byte(bit)})
	}
}

// TestFreezer_BloomBits checks that the bloom bits of frozen sections are
// rewritten without the section head hash, that sections are read under
// either key, and that a rewind into the freezer drops the rewritten bits of
// the sections no longer frozen.
func TestFreezer_BloomBits(t *testing.T) {
	setBloomBitsBlocks(t, 4)
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 2))
	defer dbm.Close()
	db := dbm.getDatabase(MiscDB)
	blocks := writeFreezerTestChain(t, dbm, 14) // blocks 0-11 are frozen: sections 0-2
	head := func(section uint64) common.Hash { return blocks[(section+1)*4-1].hash() }
	stale := common.Hash{0xff}

	writeBloomSection(t, dbm, 0, head(0))
	writeBloomSection(t, dbm, 1, head(1))
	writeBloomSection(t, dbm, 1, stale)
	writeBloomSection(t, dbm, 3, common.Hash{0x3})
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(12), dbm.FrozenBlocks())

	// Section 2 is not indexed yet, so the rewrite stops there.
	assert.Equal(t, uint64(2), dbm.frozenBloomSections())
	for section := uint64(0); section < 2; section++ {
		for bit := uint(0); bit < 3; bit++ {
			assert.False(t, hasKV(dbm, MiscDB, BloomBitsKey(bit, section, head(section))))
			assert.True(t, hasKV(dbm, MiscDB, BloomBitsShortKey(bit, section)))
			data, err := dbm.ReadBloomBits(BloomBitsKey(bit, section, head(section)))
			require.NoError(t, err)
			assert.Equal(t, []byte{byte(section), byte(bit)}, data)
		}
	}
	assert.False(t, hasKV(dbm, MiscDB, BloomBitsKey(0, 1, stale)), "a rewound section is deleted")

	writeBloomSection(t, dbm, 2, head(2))
	triggerFreeze(t, dbm)
	assert.Equal(t, uint64(3), dbm.frozenBloomSections())
	assert.True(t, hasKV(dbm, MiscDB, BloomBitsShortKey(1, 2)))
	// Section 3 is not frozen and keeps the hash.
	assert.True(t, hasKV(dbm, MiscDB, BloomBitsKey(1, 3, common.Hash{0x3})))
	assert.False(t, hasKV(dbm, MiscDB, BloomBitsShortKey(1, 3)))
	data, err := dbm.ReadBloomBits(BloomBitsKey(1, 3, common.Hash{0x3}))
	require.NoError(t, err)
	assert.Equal(t, []byte{3, 1}, data)

	// A rewind to block 5 keeps section 0 frozen.
	_, resume := dbm.PauseFreezer()
	require.NoError(t, dbm.TruncateFrozen(6))
	resume()
	assert.Equal(t, uint64(1), dbm.frozenBloomSections())
	assert.True(t, hasKV(dbm, MiscDB, BloomBitsShortKey(0, 0)))
	for section := uint64(1); section < 3; section++ {
		_, err := dbm.ReadBloomBits(BloomBitsKey(0, section, head(section)))
		assert.Error(t, err, "section %d", section)
	}
	has, _ := db.Has(frozenBloomSectionsKey)
	assert.True(t, has)
}

// TestFreezer_LegacyKeys checks that the legacy istanbul snapshots and
// governance entries of frozen blocks are deleted once the module migrations
// are done, and those of blocks not frozen are kept.
func TestFreezer_LegacyKeys(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 2))
	defer dbm.Close()
	db := dbm.getDatabase(MiscDB)
	blocks := writeFreezerTestChain(t, dbm, 14) // blocks 0-11 are frozen

	for _, num := range []uint64{0, 5, 13} {
		require.NoError(t, db.Put(makeKey(governancePrefix, num), []byte{1}))
	}
	unknown := common.Hash{0xee}
	for _, hash := range []common.Hash{blocks[0].hash(), blocks[8].hash(), blocks[12].hash(), unknown} {
		require.NoError(t, db.Put(snapshotKey(hash), []byte{1}))
	}
	require.NoError(t, db.Put(governanceHistoryKey, []byte{1}))
	has := func(key []byte) bool { return hasKV(dbm, MiscDB, key) }

	// Nothing is deleted before the migrations are done.
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(12), dbm.FrozenBlocks())
	assert.True(t, has(makeKey(governancePrefix, 0)))
	assert.True(t, has(snapshotKey(blocks[0].hash())))

	var zero [8]byte
	binary.BigEndian.PutUint64(zero[:], 0)
	require.NoError(t, db.Put(lowestScannedValidatorVoteNumKey, zero[:]))
	require.NoError(t, db.Put(govDataBlockNumsKey, []byte("[]")))
	triggerFreeze(t, dbm)
	assert.False(t, has(makeKey(governancePrefix, 0)))
	assert.False(t, has(makeKey(governancePrefix, 5)))
	assert.True(t, has(makeKey(governancePrefix, 13)))
	assert.False(t, has(snapshotKey(blocks[0].hash())))
	assert.False(t, has(snapshotKey(blocks[8].hash())))
	assert.False(t, has(snapshotKey(unknown)))
	assert.True(t, has(snapshotKey(blocks[12].hash())))
	assert.True(t, has(governanceHistoryKey))
	assert.False(t, has(legacyKeysDeletedKey))
	assert.True(t, has(govDataBlockNumsKey), "the module keys are kept")

	// Once no legacy key is left, the cleanup is done.
	require.NoError(t, db.Delete(makeKey(governancePrefix, 13)))
	require.NoError(t, db.Delete(snapshotKey(blocks[12].hash())))
	triggerFreeze(t, dbm)
	assert.False(t, has(governanceHistoryKey))
	assert.True(t, has(legacyKeysDeletedKey))
}
