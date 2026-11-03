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
	"errors"
	"fmt"
	"math/big"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database/ancient"
)

// derivedHeaderPrefix starts a frozen header without its parent hash and
// number, which are the frozen hash of the previous block and the item
// number. A header in the key-value format is an RLP list and starts with at
// least 0xc0.
const derivedHeaderPrefix = 0x02

// Positions of the derived fields in the header RLP list (types.Header).
const (
	headerParentHashField = 0
	headerNumberField     = 7
)

// splitRLPList returns the encoded elements of an RLP list.
func splitRLPList(data []byte) ([]rlp.RawValue, error) {
	content, rest, err := rlp.SplitList(data)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing bytes after list")
	}
	var elems []rlp.RawValue
	for len(content) > 0 {
		_, _, tail, err := rlp.Split(content)
		if err != nil {
			return nil, err
		}
		elems = append(elems, content[:len(content)-len(tail)])
		content = tail
	}
	return elems, nil
}

// encodeDerivedHeader converts a header from the key-value format into the
// derived format. It fails if the header's parent hash or number differ from
// the given ones, because the derived format could not restore them.
func encodeDerivedHeader(number uint64, parent common.Hash, header []byte) ([]byte, error) {
	elems, err := splitRLPList(header)
	if err != nil {
		return nil, err
	}
	if len(elems) <= headerNumberField {
		return nil, fmt.Errorf("header has %d fields", len(elems))
	}
	wantParent, _ := rlp.EncodeToBytes(parent)
	wantNumber, _ := rlp.EncodeToBytes(new(big.Int).SetUint64(number))
	if !bytes.Equal(elems[headerParentHashField], wantParent) || !bytes.Equal(elems[headerNumberField], wantNumber) {
		return nil, errors.New("header parent hash or number does not match the freezer")
	}
	rest := make([]rlp.RawValue, 0, len(elems)-2)
	rest = append(rest, elems[headerParentHashField+1:headerNumberField]...)
	rest = append(rest, elems[headerNumberField+1:]...)
	enc, err := rlp.EncodeToBytes(rest)
	if err != nil {
		return nil, err
	}
	return append([]byte{derivedHeaderPrefix}, enc...), nil
}

// decodeDerivedHeader restores a header in the derived format into the
// key-value format.
func decodeDerivedHeader(data []byte, number uint64, parent common.Hash) ([]byte, error) {
	rest, err := splitRLPList(data)
	if err != nil {
		return nil, err
	}
	if len(rest) < headerNumberField-1 {
		return nil, fmt.Errorf("derived header has %d fields", len(rest))
	}
	parentRLP, _ := rlp.EncodeToBytes(parent)
	numberRLP, _ := rlp.EncodeToBytes(new(big.Int).SetUint64(number))
	elems := make([]rlp.RawValue, 0, len(rest)+2)
	elems = append(elems, parentRLP)
	elems = append(elems, rest[:headerNumberField-1]...)
	elems = append(elems, numberRLP)
	elems = append(elems, rest[headerNumberField-1:]...)
	return rlp.EncodeToBytes(elems)
}

// frozenHeader returns the header item to append to the freezer for block
// number, whose parent is the frozen block parent: the derived format if it
// restores the header exactly, otherwise the header unchanged. The genesis is
// stored unchanged.
func frozenHeader(number uint64, parent common.Hash, header []byte) []byte {
	if number == 0 {
		return header
	}
	enc, err := encodeDerivedHeader(number, parent, header)
	if err == nil {
		var restored []byte
		if restored, err = decodeDerivedHeader(enc[1:], number, parent); err == nil && bytes.Equal(restored, header) {
			return enc
		}
		if err == nil {
			err = errors.New("restored header differs")
		}
	}
	logger.Warn("Freezing header without deriving its fields", "number", number, "err", err)
	return header
}

// readFrozenHeader returns the frozen header of a canonical block in the
// key-value format, or nil if the block is not frozen.
func (dbm *databaseManager) readFrozenHeader(hash common.Hash, number uint64) []byte {
	data := dbm.readAncient(ancient.ChainFreezerHeaderTable, hash, number)
	if len(data) == 0 || data[0] != derivedHeaderPrefix {
		return data
	}
	parent := dbm.readAncientHash(number - 1)
	if len(parent) == 0 {
		freezerReadErrorCounter.Inc(1)
		logger.Error("Parent hash of a frozen header missing", "number", number)
		return nil
	}
	header, err := decodeDerivedHeader(data[1:], number, common.BytesToHash(parent))
	if err != nil {
		freezerReadErrorCounter.Inc(1)
		logger.Error("Invalid frozen header", "number", number, "err", err)
		return nil
	}
	return header
}
