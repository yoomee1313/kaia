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
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
)

// The index maps every changed field of an account, and every written slot,
// to the blocks that changed it. With it the value at any block is read from
// one list instead of a state trie, see History.
//
// A key is kind || address [|| slot] || ^first, where first is the first block
// of a chunk of blocks. The value is the uvarint gaps from first to the next
// blocks of the chunk. The inverted first block sorts the newest chunk first,
// so a forward seek to ^N finds the chunk holding the last change at or
// before N.
const (
	kindBalance byte = iota + 1 // BalanceChanges
	kindNonce                   // NonceChanges
	kindCode                    // CodeChanges
	kindAccount                 // AccountChanges, the whole account
	kindReset                   // Deleted or StorageCleared
	kindSlot                    // StorageWrites of one slot
)

var (
	indexMetaKey = []byte("bal-index-meta")

	ErrNotIndexed = errors.New("block is not covered by the BAL index")
	errIndexBase  = errors.New("BAL index has another base")
)

// IndexMeta describes what the index covers: the values at block Base come
// from the base state, and blocks Base+1 to Next-1 are indexed.
type IndexMeta struct {
	Base     uint64
	BaseRoot common.Hash
	Next     uint64
}

func accountKey(kind byte, addr common.Address) []byte {
	key := make([]byte, 0, 1+common.AddressLength+8)
	return append(append(key, kind), addr[:]...)
}

func slotKey(addr common.Address, slot common.Hash) []byte {
	key := make([]byte, 0, 1+common.AddressLength+common.HashLength+8)
	return append(append(append(key, kindSlot), addr[:]...), slot[:]...)
}

func invertedBlock(number uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], ^number)
	return b[:]
}

func readIndexMeta(db database.Database) (*IndexMeta, error) {
	if ok, err := db.Has(indexMetaKey); err != nil || !ok {
		return nil, err
	}
	enc, err := db.Get(indexMetaKey)
	if err != nil {
		return nil, err
	}
	meta := new(IndexMeta)
	if err := rlp.DecodeBytes(enc, meta); err != nil {
		return nil, err
	}
	return meta, nil
}

// IndexBuilder adds block access lists to the index in block order. Blocks are
// buffered and written as chunks by Flush, together with the new end of the
// index, so a crash loses at most the blocks since the last flush.
type IndexBuilder struct {
	db      database.Database
	meta    IndexMeta
	pending map[string][]uint64
	size    int // rough bytes held in pending
}

// NewIndexBuilder opens the index in db, or starts one from the state of block
// base with the given root. An existing index must have the same base.
func NewIndexBuilder(db database.Database, base uint64, baseRoot common.Hash) (*IndexBuilder, error) {
	meta, err := readIndexMeta(db)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		meta = &IndexMeta{Base: base, BaseRoot: baseRoot, Next: base + 1}
	} else if meta.Base != base || meta.BaseRoot != baseRoot {
		return nil, fmt.Errorf("%w: block %d root %x", errIndexBase, meta.Base, meta.BaseRoot)
	}
	return &IndexBuilder{db: db, meta: *meta, pending: make(map[string][]uint64)}, nil
}

// Next returns the number of the next block to add.
func (b *IndexBuilder) Next() uint64 { return b.meta.Next }

// Size returns roughly how many bytes are buffered.
func (b *IndexBuilder) Size() int { return b.size }

// Add indexes the list of the given block, which must be Next().
func (b *IndexBuilder) Add(number uint64, list *BlockAccessList) error {
	if number != b.meta.Next {
		return fmt.Errorf("%w: block %d, next %d", ErrFreezerGap, number, b.meta.Next)
	}
	for i := range list.Accounts {
		a := &list.Accounts[i]
		add := func(key []byte, changed bool) {
			if changed {
				k := string(key)
				if _, ok := b.pending[k]; !ok {
					b.size += len(k) + 48
				}
				b.pending[k] = append(b.pending[k], number)
				b.size += 8
			}
		}
		add(accountKey(kindBalance, a.Address), len(a.BalanceChanges) > 0)
		add(accountKey(kindNonce, a.Address), len(a.NonceChanges) > 0)
		add(accountKey(kindCode, a.Address), len(a.CodeChanges) > 0)
		add(accountKey(kindAccount, a.Address), len(a.AccountChanges) > 0)
		add(accountKey(kindReset, a.Address), len(a.Deleted) > 0 || len(a.StorageCleared) > 0)
		for _, w := range a.StorageWrites {
			add(slotKey(a.Address, w.Slot), true)
		}
	}
	b.meta.Next++
	return nil
}

// Flush writes the buffered chunks and the new end of the index.
func (b *IndexBuilder) Flush() error {
	batch := b.db.NewBatch()
	defer batch.Release()
	var buf []byte
	for k, blocks := range b.pending {
		buf = buf[:0]
		for i := 1; i < len(blocks); i++ {
			buf = binary.AppendUvarint(buf, blocks[i]-blocks[i-1])
		}
		key := append([]byte(k), invertedBlock(blocks[0])...)
		if err := batch.Put(key, common.CopyBytes(buf)); err != nil {
			return err
		}
		if batch.ValueSize() > database.IdealBatchSize {
			if err := batch.Write(); err != nil {
				return err
			}
			batch.Reset()
		}
	}
	meta, err := rlp.EncodeToBytes(&b.meta)
	if err != nil {
		return err
	}
	if err := batch.Put(indexMetaKey, meta); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return err
	}
	b.pending, b.size = make(map[string][]uint64), 0
	return nil
}

// Index looks up the blocks that changed a field or a slot.
type Index struct {
	db   database.Database
	meta IndexMeta
}

// OpenIndex opens the index in db. It returns nil if db holds no index.
func OpenIndex(db database.Database) (*Index, error) {
	meta, err := readIndexMeta(db)
	if err != nil || meta == nil {
		return nil, err
	}
	return &Index{db: db, meta: *meta}, nil
}

// Meta returns what the index covered when it was opened.
func (ix *Index) Meta() IndexMeta { return ix.meta }

func (ix *Index) check(number uint64) error {
	if number < ix.meta.Base || number >= ix.meta.Next {
		return fmt.Errorf("%w: block %d, indexed %d-%d", ErrNotIndexed, number, ix.meta.Base, ix.meta.Next-1)
	}
	return nil
}

// last returns the last block at or before number recorded under prefix.
// ok is false if there is none after the base.
func (ix *Index) last(prefix []byte, number uint64) (block uint64, ok bool, err error) {
	it := ix.db.NewIterator(prefix, invertedBlock(number))
	defer it.Release()
	if !it.Next() {
		return 0, false, it.Error()
	}
	key := it.Key()
	if len(key) != len(prefix)+8 {
		return 0, false, fmt.Errorf("malformed BAL index key %x", key)
	}
	block = ^binary.BigEndian.Uint64(key[len(prefix):])
	for value := it.Value(); len(value) > 0; {
		gap, n := binary.Uvarint(value)
		if n <= 0 {
			return 0, false, fmt.Errorf("malformed BAL index value at key %x", key)
		}
		if block+gap > number {
			break
		}
		block += gap
		value = value[n:]
	}
	return block, true, nil
}

// blocks returns every block after the base and at or before number recorded
// under prefix, oldest first. It is meant for tests and tools.
func (ix *Index) blocks(prefix []byte, number uint64) ([]uint64, error) {
	var chunks [][]uint64
	it := ix.db.NewIterator(prefix, invertedBlock(number))
	defer it.Release()
	for it.Next() {
		key, value := it.Key(), it.Value()
		block := ^binary.BigEndian.Uint64(key[len(prefix):])
		chunk := []uint64{block}
		for len(value) > 0 {
			gap, n := binary.Uvarint(value)
			if n <= 0 {
				return nil, fmt.Errorf("malformed BAL index value at key %x", key)
			}
			block += gap
			value = value[n:]
			if block <= number {
				chunk = append(chunk, block)
			}
		}
		chunks = append(chunks, chunk)
	}
	var out []uint64
	for i := len(chunks) - 1; i >= 0; i-- {
		out = append(out, chunks[i]...)
	}
	return out, it.Error()
}
