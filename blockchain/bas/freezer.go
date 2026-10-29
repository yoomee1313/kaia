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

package bas

import (
	"errors"
	"fmt"

	"github.com/kaiachain/kaia/storage/database/ancient"
)

const (
	// FreezerTable is the freezer table holding RLP-encoded block access
	// sets. Item N is the set of block N.
	FreezerTable = "accesssets"

	freezerNamespace = "klay/db/bas/"
)

var (
	freezerTables = map[string]ancient.TableConfig{FreezerTable: {NoSnappy: false}}

	// genesisSet is stored as item 0: the genesis block is not executed.
	genesisSet = mustEncode(&BlockAccessSet{})

	ErrFreezerGap = errors.New("block is not the next item of the BAS freezer")
)

func mustEncode(s *BlockAccessSet) []byte {
	enc, err := s.Encode()
	if err != nil {
		panic(err)
	}
	return enc
}

// Freezer stores block access sets in a freezer instance of their own, so
// the files can later be served or moved as they are. Sets are appended in
// block order without gaps, starting from the genesis block.
type Freezer struct {
	f *ancient.Freezer
}

// OpenFreezer opens the BAS freezer in dir. A writable freezer that is empty
// gets an empty set for the genesis block, so the next item is block 1.
func OpenFreezer(dir string, readonly bool) (*Freezer, error) {
	f, err := ancient.NewFreezer(dir, freezerNamespace, readonly, ancient.DefaultTableSize, freezerTables)
	if err != nil {
		return nil, err
	}
	bf := &Freezer{f: f}
	if !readonly && bf.Next() == 0 {
		if err := bf.append(0, genesisSet); err != nil {
			f.Close()
			return nil, err
		}
	}
	return bf, nil
}

// Next returns the number of the next block to append, which is also the
// number of stored sets including the genesis one.
func (bf *Freezer) Next() uint64 {
	n, _ := bf.f.Ancients()
	return n
}

// Append stores the RLP-encoded set of the given block, which must be Next().
func (bf *Freezer) Append(number uint64, enc []byte) error {
	if next := bf.Next(); number != next {
		return fmt.Errorf("%w: block %d, next %d", ErrFreezerGap, number, next)
	}
	return bf.append(number, enc)
}

func (bf *Freezer) append(number uint64, enc []byte) error {
	_, err := bf.f.ModifyAncients(func(op ancient.AncientWriteOp) error {
		return op.AppendRaw(FreezerTable, number, enc)
	})
	return err
}

// ReadRaw returns the RLP-encoded set of the given block.
func (bf *Freezer) ReadRaw(number uint64) ([]byte, error) {
	return bf.f.Ancient(FreezerTable, number)
}

// ReadRawRange returns the RLP-encoded sets of up to count blocks from start,
// stopping early once maxBytes is reached (0 means no limit).
func (bf *Freezer) ReadRawRange(start, count, maxBytes uint64) ([][]byte, error) {
	return bf.f.AncientRange(FreezerTable, start, count, maxBytes)
}

// Read returns the decoded set of the given block.
func (bf *Freezer) Read(number uint64) (*BlockAccessSet, error) {
	enc, err := bf.ReadRaw(number)
	if err != nil {
		return nil, err
	}
	return Decode(enc)
}

// Size returns the bytes the table takes on disk, after compression.
func (bf *Freezer) Size() (uint64, error) {
	return bf.f.AncientSize(FreezerTable)
}

// Sync flushes appended sets to disk.
func (bf *Freezer) Sync() error {
	return bf.f.SyncAncient()
}

func (bf *Freezer) Close() error {
	return bf.f.Close()
}
