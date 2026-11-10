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

package bal

import (
	"errors"
	"fmt"

	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/storage/database"
)

// A block access list is stored as its writes, the RLP encoding of Writes(),
// whose keccak256 is its Hash. Its reads are stored apart: writing them is
// optional, and deleting them changes nothing but ReadFull.
var (
	ErrNotStored      = errors.New("block access list not stored")
	ErrReadsNotStored = errors.New("block access list reads not stored")
)

// ReadRaw returns the RLP-encoded writes of the list of a canonical block of
// db.
func ReadRaw(db database.DBManager, number uint64) ([]byte, error) {
	enc := db.ReadBAL(db.ReadCanonicalHash(number), number)
	if enc == nil {
		return nil, fmt.Errorf("%w: block %d", ErrNotStored, number)
	}
	return enc, nil
}

// Read returns the writes of the list of a canonical block of db.
func Read(db database.DBManager, number uint64) (*access.BlockAccessList, error) {
	enc, err := ReadRaw(db, number)
	if err != nil {
		return nil, err
	}
	return access.Decode(enc)
}

// ReadReads returns the reads of the list of a canonical block of db.
func ReadReads(db database.DBManager, number uint64) (*access.Reads, error) {
	enc := db.ReadBALReads(db.ReadCanonicalHash(number), number)
	if enc == nil {
		return nil, fmt.Errorf("%w: block %d", ErrReadsNotStored, number)
	}
	return access.DecodeReads(enc)
}

// ReadFull returns the whole list of a canonical block of db, as recorded: its
// writes with its reads.
func ReadFull(db database.DBManager, number uint64) (*access.BlockAccessList, error) {
	writes, err := Read(db, number)
	if err != nil {
		return nil, err
	}
	reads, err := ReadReads(db, number)
	if err != nil {
		return nil, err
	}
	return access.Combine(writes, reads)
}

// Lists reads the writes of the lists of the canonical chain of a database.
type Lists struct{ DB database.DBManager }

func (l Lists) Read(number uint64) (*access.BlockAccessList, error) { return Read(l.DB, number) }
