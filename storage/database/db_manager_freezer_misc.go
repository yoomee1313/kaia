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
	"bytes"
	"encoding/binary"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/params"
)

// Once blocks are frozen, the freeze loop also trims the misc database entries
// of those blocks that are not needed in their key-value form:
//
//   - The bloom bits of a frozen section are stored without the section head
//     hash in their keys. Kaia has no reorgs, so the hash only told apart the
//     sections a rewind abandoned, and those are deleted with it. The bloom
//     bits of a section that is not frozen keep the hash.
//   - The legacy istanbul snapshots and governance entries of frozen blocks are
//     deleted once the valset and header gov modules rebuilt their data from
//     the headers. Neither has a writer any more.
//
// The total blockscores of frozen blocks are deleted with the blocks.

var (
	// frozenBloomSectionsKey is the number of sections whose bloom bits are
	// stored without the section head hash.
	frozenBloomSectionsKey = []byte("FrozenBloomSections")

	// legacyKeysDeletedKey is set once no legacy key is left.
	legacyKeysDeletedKey = []byte("LegacyKeysDeleted")

	// The markers of the module migrations, owned by kaiax/valset and
	// kaiax/gov/headergov.
	lowestScannedValidatorVoteNumKey = []byte("lowestScannedValidatorVoteNum")
	govDataBlockNumsKey              = []byte("governanceDataBlockNums")
)

const (
	bloomBitsShortKeyLen   = 1 + 2 + 8
	bloomBitsLongKeyLen    = bloomBitsShortKeyLen + common.HashLength
	legacySnapshotKeyLen   = 8 + common.HashLength // snapshotKeyPrefix + hash
	legacyGovernanceKeyLen = 10 + 8                // governancePrefix + num (uint64 little endian)

	// frozenBloomBatchSections is the number of sections rewritten at once.
	frozenBloomBatchSections = 8
)

// bloomBitsSection is the number of blocks of a bloom bits section. It is a
// variable so that tests can lower it.
var bloomBitsSection = params.BloomBitsBlocks

// BloomBitsShortKey = bloomBitsPrefix + bit (uint16 big endian) + section (uint64 big endian)
func BloomBitsShortKey(bit uint, section uint64) []byte {
	key := make([]byte, bloomBitsShortKeyLen)
	copy(key, bloomBitsPrefix)
	binary.BigEndian.PutUint16(key[1:], uint16(bit))
	binary.BigEndian.PutUint64(key[3:], section)
	return key
}

// readBloomBits returns the bloom bits of the given key, which carries the
// section head hash, from the key without the hash first if the section may
// be frozen.
func (dbm *databaseManager) readBloomBits(key []byte) ([]byte, error) {
	db := dbm.getDatabase(MiscDB)
	if dbm.freezer != nil && len(key) == bloomBitsLongKeyLen && bytes.HasPrefix(key, bloomBitsPrefix) {
		if data, err := db.Get(key[:bloomBitsShortKeyLen]); err == nil && len(data) != 0 {
			return data, nil
		}
	}
	return db.Get(key)
}

// trimFrozenMisc trims the misc database entries of the frozen blocks.
func (dbm *databaseManager) trimFrozenMisc() {
	for dbm.shortenFrozenBloomBits(frozenBloomBatchSections) {
		select {
		case <-dbm.freezerQuit:
			return
		default:
		}
	}
	dbm.deleteFrozenLegacyKeys()
}

// frozenBloomSections returns the number of sections whose bloom bits are
// stored without the section head hash.
func (dbm *databaseManager) frozenBloomSections() uint64 {
	data, _ := dbm.getDatabase(MiscDB).Get(frozenBloomSectionsKey)
	if len(data) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(data)
}

// shortenFrozenBloomBits rewrites the bloom bits of up to n frozen sections
// without the section head hash, and deletes the bloom bits a rewind left in
// those sections. It stops at a section the bloom indexer has not stored yet,
// and returns whether it rewrote a section.
func (dbm *databaseManager) shortenFrozenBloomBits(n int) bool {
	dbm.freezeLock.Lock()
	defer dbm.freezeLock.Unlock()

	db := dbm.getDatabase(MiscDB)
	batch := db.NewBatch()
	defer batch.Release()

	var (
		first  = dbm.frozenBloomSections()
		next   = first
		frozen = dbm.FrozenBlocks()
	)
	for ; next < first+uint64(n) && (next+1)*bloomBitsSection <= frozen; next++ {
		head := common.BytesToHash(dbm.readAncientHash((next+1)*bloomBitsSection - 1))
		indexed, err := dbm.shortenBloomSection(batch, next, head)
		if err != nil {
			logger.Error("Failed to rewrite frozen bloom bits", "section", next, "err", err)
			return false
		}
		if !indexed {
			break
		}
	}
	if next == first {
		return false
	}
	var progress [8]byte
	binary.BigEndian.PutUint64(progress[:], next)
	if err := batch.Put(frozenBloomSectionsKey, progress[:]); err != nil {
		logger.Crit("Failed to rewrite frozen bloom bits", "err", err)
	}
	if err := batch.Write(); err != nil {
		logger.Crit("Failed to rewrite frozen bloom bits", "err", err)
	}
	return true
}

// shortenBloomSection adds to the batch the bloom bits of a section under the
// key without the section head hash, and the deletion of its keys with a
// hash. It reports false if the bloom indexer has not stored the section yet.
func (dbm *databaseManager) shortenBloomSection(batch Batch, section uint64, head common.Hash) (bool, error) {
	db := dbm.getDatabase(MiscDB)
	if has, _ := db.Has(BloomBitsKey(0, section, head)); !has {
		if has, _ := db.Has(BloomBitsShortKey(0, section)); !has {
			return false, nil
		}
	}
	for bit := uint(0); bit < types.BloomBitLength; bit++ {
		short := BloomBitsShortKey(bit, section)
		it := db.NewIterator(short, nil)
		for it.Next() {
			key := it.Key()
			if len(key) != bloomBitsLongKeyLen {
				continue
			}
			if bytes.Equal(key[bloomBitsShortKeyLen:], head[:]) {
				if err := batch.Put(short, common.CopyBytes(it.Value())); err != nil {
					it.Release()
					return false, err
				}
			}
			if err := batch.Delete(common.CopyBytes(key)); err != nil {
				it.Release()
				return false, err
			}
		}
		it.Release()
		if err := it.Error(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// unfreezeBloomBits deletes the bloom bits stored without the section head
// hash of the sections that are no longer frozen once the freezer keeps items
// blocks. The blocks after the rewind head are not indexed any more, so the
// bloom indexer stores those sections again, with the hash.
func (dbm *databaseManager) unfreezeBloomBits(items uint64) {
	first, next := items/bloomBitsSection, dbm.frozenBloomSections()
	if next <= first {
		return
	}
	batch := dbm.NewBatch(MiscDB)
	defer batch.Release()
	for section := first; section < next; section++ {
		for bit := uint(0); bit < types.BloomBitLength; bit++ {
			if err := batch.Delete(BloomBitsShortKey(bit, section)); err != nil {
				logger.Crit("Failed to delete frozen bloom bits", "err", err)
			}
		}
		if _, err := WriteBatchesOverThreshold(batch); err != nil {
			logger.Crit("Failed to delete frozen bloom bits", "err", err)
		}
	}
	var progress [8]byte
	binary.BigEndian.PutUint64(progress[:], first)
	if err := batch.Put(frozenBloomSectionsKey, progress[:]); err != nil {
		logger.Crit("Failed to delete frozen bloom bits", "err", err)
	}
	if err := batch.Write(); err != nil {
		logger.Crit("Failed to delete frozen bloom bits", "err", err)
	}
}

// legacyMigrated reports whether the valset module scanned the votes of every
// block and the header gov module indexed the governance blocks.
func (dbm *databaseManager) legacyMigrated() bool {
	db := dbm.getDatabase(MiscDB)
	lowest, _ := db.Get(lowestScannedValidatorVoteNumKey)
	if len(lowest) != 8 || binary.BigEndian.Uint64(lowest) != 0 {
		return false
	}
	has, _ := db.Has(govDataBlockNumsKey)
	return has
}

// deleteFrozenLegacyKeys deletes the legacy istanbul snapshots and governance
// entries of the frozen blocks once the module migrations are done.
func (dbm *databaseManager) deleteFrozenLegacyKeys() {
	db := dbm.getDatabase(MiscDB)
	if has, _ := db.Has(legacyKeysDeletedKey); has || !dbm.legacyMigrated() {
		return
	}
	dbm.freezeLock.Lock()
	defer dbm.freezeLock.Unlock()

	frozen := dbm.FrozenBlocks()
	batch := db.NewBatch()
	defer batch.Release()
	governance, left := dbm.deleteLegacyKeys(batch, governancePrefix, legacyGovernanceKeyLen, func(key []byte) bool {
		return binary.LittleEndian.Uint64(key[len(governancePrefix):]) < frozen
	})
	snapshots, snapshotsLeft := dbm.deleteLegacyKeys(batch, snapshotKeyPrefix, legacySnapshotKeyLen, func(key []byte) bool {
		number := dbm.ReadHeaderNumber(common.BytesToHash(key[len(snapshotKeyPrefix):]))
		return number == nil || *number < frozen
	})
	if left+snapshotsLeft == 0 {
		if err := batch.Delete(governanceHistoryKey); err != nil {
			logger.Crit("Failed to delete legacy keys", "err", err)
		}
		if err := batch.Put(legacyKeysDeletedKey, []byte{1}); err != nil {
			logger.Crit("Failed to delete legacy keys", "err", err)
		}
	}
	if err := batch.Write(); err != nil {
		logger.Crit("Failed to delete legacy keys", "err", err)
	}
	if snapshots+governance > 0 {
		logger.Info("Deleted legacy istanbul snapshots and governance entries of frozen blocks", "snapshots", snapshots, "governance", governance)
	}
}

// deleteLegacyKeys adds to the batch the deletion of the misc database keys of
// the given prefix and length that frozen accepts, and returns how many it
// deleted and how many it left.
func (dbm *databaseManager) deleteLegacyKeys(batch Batch, prefix []byte, keyLen int, frozen func(key []byte) bool) (int, int) {
	var deleted, left int
	it := dbm.getDatabase(MiscDB).NewIterator(prefix, nil)
	defer it.Release()
	for it.Next() {
		key := it.Key()
		if len(key) != keyLen {
			continue
		}
		if !frozen(key) {
			left++
			continue
		}
		if err := batch.Delete(common.CopyBytes(key)); err != nil {
			logger.Crit("Failed to delete legacy keys", "err", err)
		}
		if _, err := WriteBatchesOverThreshold(batch); err != nil {
			logger.Crit("Failed to delete legacy keys", "err", err)
		}
		deleted++
	}
	if err := it.Error(); err != nil {
		logger.Error("Failed to delete legacy keys", "err", err)
		left++
	}
	return deleted, left
}
