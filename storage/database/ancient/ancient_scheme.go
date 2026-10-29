// Modifications Copyright 2026 The Kaia Authors
// Copyright 2022 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
//
// This file is derived from core/rawdb/ancient_scheme.go and ethdb/database.go (2026/10/21).
// Modified for the Kaia development.

// Package ancient implements an append-only, file based store ("freezer") for
// immutable chain segments. It is a port of go-ethereum's core/rawdb freezer.
package ancient

import (
	"github.com/kaiachain/kaia/log"
)

var logger = log.NewModuleLogger(log.StorageDatabase)

// The list of table names of chain freezer.
const (
	// ChainFreezerHeaderTable indicates the name of the freezer header table.
	ChainFreezerHeaderTable = "headers"

	// ChainFreezerHashTable indicates the name of the freezer canonical hash table.
	ChainFreezerHashTable = "hashes"

	// ChainFreezerBodiesTable indicates the name of the freezer block body table.
	ChainFreezerBodiesTable = "bodies"

	// ChainFreezerReceiptTable indicates the name of the freezer receipts table.
	ChainFreezerReceiptTable = "receipts"
)

// ChainFreezerName is the sub-directory of the ancient root holding the chain freezer.
const ChainFreezerName = "chain"

// chainFreezerTableConfigs configures the settings for tables in the chain freezer.
// Hashes are random and do not compress, so snappy is disabled for them. Total
// difficulty is not frozen; it stays in the key-value store.
var chainFreezerTableConfigs = map[string]TableConfig{
	ChainFreezerHeaderTable:  {NoSnappy: false},
	ChainFreezerHashTable:    {NoSnappy: true},
	ChainFreezerBodiesTable:  {NoSnappy: false},
	ChainFreezerReceiptTable: {NoSnappy: false},
}

// TableConfig contains the settings for a freezer table.
type TableConfig struct {
	// NoSnappy disables item compression when true.
	NoSnappy bool

	// TailGroup names a logical group of tables that share the same tail
	// position. Tables in the same group are pruned together and must agree
	// on their tail. An empty value means the table is not prunable; its
	// tail is always 0.
	TailGroup string
}

// AncientReaderOp contains the methods required to read from immutable ancient data.
type AncientReaderOp interface {
	// Ancient retrieves an ancient binary blob from the append-only immutable files.
	Ancient(kind string, number uint64) ([]byte, error)

	// AncientRange retrieves multiple items in sequence, starting from the index 'start'.
	AncientRange(kind string, start, count, maxBytes uint64) ([][]byte, error)

	// Ancients returns the ancient item numbers in the ancient store.
	Ancients() (uint64, error)

	// Tail returns the number of first stored item in the given tail group.
	Tail(group string) (uint64, error)

	// AncientSize returns the ancient size of the specified category.
	AncientSize(kind string) (uint64, error)
}

// AncientWriteOp is given to the function argument of ModifyAncients.
type AncientWriteOp interface {
	// Append adds an RLP-encoded item.
	Append(kind string, number uint64, item interface{}) error

	// AppendRaw adds an item without RLP-encoding it.
	AppendRaw(kind string, number uint64, item []byte) error
}

// DefaultTableSize is the maximum size of a freezer data file, for callers of
// NewFreezer that define their own tables.
const DefaultTableSize = freezerTableSize

// NewChainFreezer opens (or creates) the block data freezer at the given
// directory. The metrics are registered under the given namespace.
func NewChainFreezer(datadir string, namespace string, readonly bool) (*Freezer, error) {
	return NewFreezer(datadir, namespace, readonly, freezerTableSize, chainFreezerTableConfigs)
}
