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
	"encoding/binary"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
)

var (
	balPrefix      = []byte("j")               // balPrefix + num (uint64 big endian) + hash -> writes of the block access list
	balReadsPrefix = []byte("J")               // balReadsPrefix + num (uint64 big endian) + hash -> reads of the block access list
	balRangeKey    = []byte("AccessListRange") // first and next block of the stored block access lists

	historyModeKey          = []byte("HistoryMode")  // full, history or light
	historyFirstKey         = []byte("HistoryFirst") // first block whose header, body and receipts are held
	headerSegmentRootPrefix = []byte("hseg")         // headerSegmentRootPrefix + segment (uint64 big endian) -> root of its block hashes
)

func balKey(prefix []byte, number uint64, hash common.Hash) []byte {
	return append(append(append([]byte{}, prefix...), common.Int64ToByteBigEndian(number)...), hash.Bytes()...)
}

func headerSegmentRootKey(segment uint64) []byte {
	return binary.BigEndian.AppendUint64(append([]byte{}, headerSegmentRootPrefix...), segment)
}

// ReadBAL returns the RLP-encoded writes of the block access list of a block,
// or nil.
func ReadBAL(db database.DBManager, hash common.Hash, number uint64) []byte {
	enc, _ := db.GetBALDB().Get(balKey(balPrefix, number, hash))
	return enc
}

// HasBAL reports whether the writes of the block access list of a block are
// stored.
func HasBAL(db database.DBManager, hash common.Hash, number uint64) bool {
	ok, _ := db.GetBALDB().Has(balKey(balPrefix, number, hash))
	return ok
}

// WriteBAL stores the RLP-encoded writes of the block access list of a block.
func WriteBAL(db database.DBManager, hash common.Hash, number uint64, enc []byte) error {
	return db.GetBALDB().Put(balKey(balPrefix, number, hash), enc)
}

// DeleteBAL removes the block access list of a block.
func DeleteBAL(db database.DBManager, hash common.Hash, number uint64) {
	if err := db.GetBALDB().Delete(balKey(balPrefix, number, hash)); err != nil {
		logger.Error("Failed to delete block access list", "number", number, "err", err)
	}
}

// ReadBALReads returns the RLP-encoded reads of the block access list of a
// block, or nil. They are kept apart from the writes and may be missing.
func ReadBALReads(db database.DBManager, hash common.Hash, number uint64) []byte {
	enc, _ := db.GetBALDB().Get(balKey(balReadsPrefix, number, hash))
	return enc
}

// WriteBALReads stores the RLP-encoded reads of the block access list of a
// block.
func WriteBALReads(db database.DBManager, hash common.Hash, number uint64, enc []byte) error {
	return db.GetBALDB().Put(balKey(balReadsPrefix, number, hash), enc)
}

// DeleteBALReads removes the reads of the block access list of a block.
func DeleteBALReads(db database.DBManager, hash common.Hash, number uint64) {
	if err := db.GetBALDB().Delete(balKey(balReadsPrefix, number, hash)); err != nil {
		logger.Error("Failed to delete block access list reads", "number", number, "err", err)
	}
}

// ReadBALRange returns the blocks first to next-1 whose block access lists
// are all stored; first == next if there are none.
func ReadBALRange(db database.DBManager) (first, next uint64) {
	enc, err := db.GetBALDB().Get(balRangeKey)
	if err != nil || len(enc) != 16 {
		return 0, 0
	}
	return binary.BigEndian.Uint64(enc), binary.BigEndian.Uint64(enc[8:])
}

// WriteBALRange stores the range returned by ReadBALRange.
func WriteBALRange(db database.DBManager, first, next uint64) error {
	enc := binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, first), next)
	return db.GetBALDB().Put(balRangeKey, enc)
}

// ReadHistoryMode returns the history mode the database was made for, or ""
// for a database made before the modes.
func ReadHistoryMode(db database.DBManager) string {
	enc, _ := db.GetMiscDB().Get(historyModeKey)
	return string(enc)
}

// WriteHistoryMode stores the history mode of the database.
func WriteHistoryMode(db database.DBManager, mode string) error {
	return db.GetMiscDB().Put(historyModeKey, []byte(mode))
}

// ReadHistoryFirst returns the first block whose header, body and receipts
// are held; the data of earlier blocks but the genesis block was deleted.
func ReadHistoryFirst(db database.DBManager) uint64 {
	enc, err := db.GetMiscDB().Get(historyFirstKey)
	if err != nil || len(enc) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(enc)
}

// WriteHistoryFirst stores the first block whose data is held.
func WriteHistoryFirst(db database.DBManager, number uint64) error {
	return db.GetMiscDB().Put(historyFirstKey, binary.BigEndian.AppendUint64(nil, number))
}

// ReadHeaderSegmentRoot returns the root of the block hashes of a segment
// whose blocks were deleted.
func ReadHeaderSegmentRoot(db database.DBManager, segment uint64) (common.Hash, bool) {
	enc, err := db.GetMiscDB().Get(headerSegmentRootKey(segment))
	if err != nil || len(enc) != common.HashLength {
		return common.Hash{}, false
	}
	return common.BytesToHash(enc), true
}

// WriteHeaderSegmentRoot stores the root of the block hashes of a segment.
func WriteHeaderSegmentRoot(db database.DBManager, segment uint64, root common.Hash) error {
	return db.GetMiscDB().Put(headerSegmentRootKey(segment), root[:])
}
