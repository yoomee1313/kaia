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
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kaiachain/kaia/storage/database/ancient"
)

const (
	// FreezerTable is the freezer table holding RLP-encoded block access
	// lists. Item N is the list of block N.
	FreezerTable = "accesslists"

	freezerNamespace = "klay/db/bal/"
)

var (
	freezerTables = map[string]ancient.TableConfig{FreezerTable: {NoSnappy: false}}

	// genesisList is stored as item 0: the genesis block is not executed.
	genesisList = mustEncode(&BlockAccessList{})

	ErrFreezerGap   = errors.New("block is not the next item of the BAL freezer")
	ErrBeforeFirst  = errors.New("block is before the first block of the BAL freezer")
	errFirstChanged = errors.New("BAL freezer starts at another block")
)

// firstFile holds the first block of a freezer that does not start at the
// genesis block, as a node whose earlier states are gone has no lists of them.
const firstFile = "FIRST"

func mustEncode(s *BlockAccessList) []byte {
	enc, err := s.Encode()
	if err != nil {
		panic(err)
	}
	return enc
}

// Freezer stores block access lists in a freezer instance of their own, so
// the files can later be served or moved as they are. Lists are appended in
// block order without gaps, from the genesis block or from the first block
// given when the freezer was created. Item k is the list of block first+k.
type Freezer struct {
	f     *ancient.Freezer
	first uint64
}

func readFirst(dir string) (uint64, bool, error) {
	enc, err := os.ReadFile(filepath.Join(dir, firstFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	first, err := strconv.ParseUint(strings.TrimSpace(string(enc)), 10, 64)
	return first, true, err
}

// OpenFreezer opens the BAL freezer in dir, which starts where it was created.
// A writable freezer created here starts at the genesis block, with an empty
// list for it, so the next item is block 1.
func OpenFreezer(dir string, readonly bool) (*Freezer, error) {
	first, _, err := readFirst(dir)
	if err != nil {
		return nil, err
	}
	return openFreezer(dir, readonly, first)
}

// OpenFreezerAt opens the BAL freezer in dir that starts at block first,
// creating it if it does not exist. An existing freezer must start there.
func OpenFreezerAt(dir string, first uint64) (*Freezer, error) {
	stored, ok, err := readFirst(dir)
	if err != nil {
		return nil, err
	}
	if !ok {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if first > 0 {
			if _, err := os.Stat(filepath.Join(dir, FreezerTable+".cidx")); err == nil {
				return nil, fmt.Errorf("%w: genesis, not %d", errFirstChanged, first)
			}
			if err := os.WriteFile(filepath.Join(dir, firstFile), []byte(strconv.FormatUint(first, 10)), 0o644); err != nil {
				return nil, err
			}
		}
		stored = first
	}
	if stored != first {
		return nil, fmt.Errorf("%w: %d, not %d", errFirstChanged, stored, first)
	}
	return openFreezer(dir, false, first)
}

func openFreezer(dir string, readonly bool, first uint64) (*Freezer, error) {
	f, err := ancient.NewFreezer(dir, freezerNamespace, readonly, ancient.DefaultTableSize, freezerTables)
	if err != nil {
		return nil, err
	}
	bf := &Freezer{f: f, first: first}
	if !readonly && first == 0 && bf.Next() == 0 {
		if err := bf.append(0, genesisList); err != nil {
			f.Close()
			return nil, err
		}
	}
	return bf, nil
}

// First returns the first block the freezer holds a list of.
func (bf *Freezer) First() uint64 { return bf.first }

// Next returns the number of the next block to append.
func (bf *Freezer) Next() uint64 {
	n, _ := bf.f.Ancients()
	return bf.first + n
}

// Append stores the RLP-encoded list of the given block, which must be Next().
func (bf *Freezer) Append(number uint64, enc []byte) error {
	if next := bf.Next(); number != next {
		return fmt.Errorf("%w: block %d, next %d", ErrFreezerGap, number, next)
	}
	return bf.append(number, enc)
}

func (bf *Freezer) append(number uint64, enc []byte) error {
	_, err := bf.f.ModifyAncients(func(op ancient.AncientWriteOp) error {
		return op.AppendRaw(FreezerTable, number-bf.first, enc)
	})
	return err
}

// ReadRaw returns the RLP-encoded list of the given block.
func (bf *Freezer) ReadRaw(number uint64) ([]byte, error) {
	if number < bf.first {
		return nil, fmt.Errorf("%w: block %d, first %d", ErrBeforeFirst, number, bf.first)
	}
	return bf.f.Ancient(FreezerTable, number-bf.first)
}

// ReadRawRange returns the RLP-encoded lists of up to count blocks from start,
// stopping early once maxBytes is reached (0 means no limit).
func (bf *Freezer) ReadRawRange(start, count, maxBytes uint64) ([][]byte, error) {
	if start < bf.first {
		return nil, fmt.Errorf("%w: block %d, first %d", ErrBeforeFirst, start, bf.first)
	}
	return bf.f.AncientRange(FreezerTable, start-bf.first, count, maxBytes)
}

// Read returns the decoded list of the given block.
func (bf *Freezer) Read(number uint64) (*BlockAccessList, error) {
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

// Sync flushes appended lists to disk.
func (bf *Freezer) Sync() error {
	return bf.f.SyncAncient()
}

func (bf *Freezer) Close() error {
	return bf.f.Close()
}
