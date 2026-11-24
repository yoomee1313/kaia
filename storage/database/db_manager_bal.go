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
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
	"github.com/pkg/errors"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// The block access lists (BALs) of a history node are stored in the BAL
// database: the writes of the list of a block, the RLP encoding of Writes()
// whose keccak256 is its hash, apart from its reads, which may be missing.
var (
	balPrefix      = []byte("j")               // balPrefix + num (uint64 big endian) + hash -> writes of the block access list
	balReadsPrefix = []byte("J")               // balReadsPrefix + num (uint64 big endian) + hash -> reads of the block access list
	balRangeKey    = []byte("AccessListRange") // first and next block of the stored block access lists
)

func balKey(prefix []byte, number uint64, hash common.Hash) []byte {
	return append(append(append([]byte{}, prefix...), common.Int64ToByteBigEndian(number)...), hash.Bytes()...)
}

// ReadBAL returns the RLP-encoded writes of the block access list of a block,
// or nil.
func (dbm *databaseManager) ReadBAL(hash common.Hash, number uint64) []byte {
	enc, _ := dbm.GetBALDB().Get(balKey(balPrefix, number, hash))
	return enc
}

// HasBAL reports whether the writes of the block access list of a block are
// stored.
func (dbm *databaseManager) HasBAL(hash common.Hash, number uint64) bool {
	ok, _ := dbm.GetBALDB().Has(balKey(balPrefix, number, hash))
	return ok
}

// WriteBAL stores the RLP-encoded writes of the block access list of a block.
func (dbm *databaseManager) WriteBAL(hash common.Hash, number uint64, enc []byte) error {
	return dbm.GetBALDB().Put(balKey(balPrefix, number, hash), enc)
}

// DeleteBAL removes the block access list of a block.
func (dbm *databaseManager) DeleteBAL(hash common.Hash, number uint64) {
	if err := dbm.GetBALDB().Delete(balKey(balPrefix, number, hash)); err != nil {
		logger.Error("Failed to delete block access list", "number", number, "err", err)
	}
}

// ReadBALReads returns the RLP-encoded reads of the block access list of a
// block, or nil. They are kept apart from the writes and may be missing.
func (dbm *databaseManager) ReadBALReads(hash common.Hash, number uint64) []byte {
	enc, _ := dbm.GetBALDB().Get(balKey(balReadsPrefix, number, hash))
	return enc
}

// WriteBALReads stores the RLP-encoded reads of the block access list of a
// block.
func (dbm *databaseManager) WriteBALReads(hash common.Hash, number uint64, enc []byte) error {
	return dbm.GetBALDB().Put(balKey(balReadsPrefix, number, hash), enc)
}

// DeleteBALReads removes the reads of the block access list of a block.
func (dbm *databaseManager) DeleteBALReads(hash common.Hash, number uint64) {
	if err := dbm.GetBALDB().Delete(balKey(balReadsPrefix, number, hash)); err != nil {
		logger.Error("Failed to delete block access list reads", "number", number, "err", err)
	}
}

// ErrBALNotStored is returned for a block whose list is not stored.
var ErrBALNotStored = errors.New("block access list not stored")

// ReadCanonicalBAL returns the RLP-encoded writes of the list of canonical
// block number.
func (dbm *databaseManager) ReadCanonicalBAL(number uint64) ([]byte, error) {
	enc := dbm.ReadBAL(dbm.ReadCanonicalHash(number), number)
	if enc == nil {
		return nil, fmt.Errorf("%w: block %d", ErrBALNotStored, number)
	}
	return enc, nil
}

// ReadCanonicalBALList returns the writes of the list of canonical block
// number.
func (dbm *databaseManager) ReadCanonicalBALList(number uint64) (*types.BlockAccessList, error) {
	enc, err := dbm.ReadCanonicalBAL(number)
	if err != nil {
		return nil, err
	}
	return types.DecodeBAL(enc)
}

// ReadBALRange returns the blocks first to next-1 whose block access lists
// are all stored; first == next if there are none.
func (dbm *databaseManager) ReadBALRange() (first, next uint64) {
	enc, err := dbm.GetBALDB().Get(balRangeKey)
	if err != nil || len(enc) != 16 {
		return 0, 0
	}
	return binary.BigEndian.Uint64(enc), binary.BigEndian.Uint64(enc[8:])
}

// WriteBALRange stores the range returned by ReadBALRange.
func (dbm *databaseManager) WriteBALRange(first, next uint64) error {
	enc := binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, first), next)
	return dbm.GetBALDB().Put(balRangeKey, enc)
}

// The BAL index maps every changed field of an account, and every written
// slot, to the blocks that changed it. It is kept in a LevelDB of its own,
// balindex, next to the chain database, which is opened on first use.
//
// A key is kind || address [|| slot] || ^first, where first is the first block
// of a chunk of blocks. The value is the uvarint gaps from first to the next
// blocks of the chunk. The inverted first block sorts the newest chunk first,
// so a forward seek to ^N finds the chunk holding the last change at or
// before N.
const (
	BALIndexBalance byte = iota + 1 // BalanceChanges
	BALIndexNonce                   // NonceChanges
	BALIndexCode                    // CodeChanges
	BALIndexAccount                 // AccountRecords, the whole account
	BALIndexReset                   // Deleted or StorageCleared
	BALIndexSlot                    // StorageChanges of one slot
	// 7 mapped state roots to blocks in older indexes, whose keys are left.
)

var balIndexMetaKey = []byte("bal-index-meta")

// BALIndexMeta describes what the BAL index covers: the values at block Base
// come from the base state, and blocks Base+1 to Next-1 are indexed.
type BALIndexMeta struct {
	Base     uint64
	BaseRoot common.Hash
	Next     uint64
}

func BALIndexKey(kind byte, addr common.Address) []byte {
	key := make([]byte, 0, 1+common.AddressLength+8)
	return append(append(key, kind), addr[:]...)
}

func BALIndexSlotKey(addr common.Address, slot common.Hash) []byte {
	key := make([]byte, 0, 1+common.AddressLength+common.HashLength+8)
	return append(append(append(key, BALIndexSlot), addr[:]...), slot[:]...)
}

func invertedBlock(number uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, ^number)
}

func (dbm *databaseManager) balIndexDB() (Database, error) {
	dbm.balIndexMu.Lock()
	defer dbm.balIndexMu.Unlock()
	if dbm.balIndex != nil {
		return dbm.balIndex, nil
	}
	if dir := dbm.config.Dir; dir != "" {
		ldb, err := NewLevelDBWithOption(filepath.Join(dir, "balindex"), &opt.Options{
			OpenFilesCacheCapacity: 256, BlockCacheCapacity: 64 * opt.MiB, WriteBuffer: 32 * opt.MiB,
		})
		if err != nil {
			return nil, err
		}
		dbm.balIndex = ldb
	} else {
		dbm.balIndex = NewMemDB()
	}
	return dbm.balIndex, nil
}

func (dbm *databaseManager) closeBALIndex() {
	dbm.balIndexMu.Lock()
	defer dbm.balIndexMu.Unlock()
	if dbm.balIndex != nil {
		dbm.balIndex.Close()
		dbm.balIndex = nil
	}
}

// readBALIndexMeta returns what the BAL index covers, or nil if there is no
// index.
func (dbm *databaseManager) readBALIndexMeta() (*BALIndexMeta, error) {
	db, err := dbm.balIndexDB()
	if err != nil {
		return nil, err
	}
	if ok, err := db.Has(balIndexMetaKey); err != nil || !ok {
		return nil, err
	}
	enc, err := db.Get(balIndexMetaKey)
	if err != nil {
		return nil, err
	}
	meta := new(BALIndexMeta)
	if err := rlp.DecodeBytes(enc, meta); err != nil {
		return nil, err
	}
	return meta, nil
}

// writeBALIndex writes chunks, the blocks that changed each key in block
// order, and then the new meta.
func (dbm *databaseManager) writeBALIndex(chunks map[string][]uint64, meta *BALIndexMeta) error {
	db, err := dbm.balIndexDB()
	if err != nil {
		return err
	}
	batch := db.NewBatch()
	defer batch.Release()
	put := func(key, value []byte) error {
		if err := batch.Put(key, value); err != nil {
			return err
		}
		_, err := WriteBatchesOverThreshold(batch)
		return err
	}
	for k, blocks := range chunks {
		gaps := []byte{}
		for i := 1; i < len(blocks); i++ {
			gaps = binary.AppendUvarint(gaps, blocks[i]-blocks[i-1])
		}
		if err := put(append([]byte(k), invertedBlock(blocks[0])...), gaps); err != nil {
			return err
		}
	}
	enc, err := rlp.EncodeToBytes(meta)
	if err != nil {
		return err
	}
	if err := batch.Put(balIndexMetaKey, enc); err != nil {
		return err
	}
	return batch.Write()
}

// DeleteBALIndex deletes the BAL index.
func (dbm *databaseManager) DeleteBALIndex() error {
	db, err := dbm.balIndexDB()
	if err != nil {
		return err
	}
	it := db.NewIterator(nil, nil)
	defer it.Release()
	batch := db.NewBatch()
	defer batch.Release()
	for it.Next() {
		if err := batch.Delete(common.CopyBytes(it.Key())); err != nil {
			return err
		}
		if _, err := WriteBatchesOverThreshold(batch); err != nil {
			return err
		}
	}
	if err := it.Error(); err != nil {
		return err
	}
	return batch.Write()
}

// truncateBALIndex drops the blocks after number from the BAL index and writes
// meta, the index left.
func (dbm *databaseManager) truncateBALIndex(number uint64, meta *BALIndexMeta) error {
	db, err := dbm.balIndexDB()
	if err != nil {
		return err
	}
	it := db.NewIterator(nil, nil)
	defer it.Release()
	batch := db.NewBatch()
	defer batch.Release()
	for it.Next() {
		key, value := it.Key(), it.Value()
		switch {
		case len(key) > 8 && key[0] >= BALIndexBalance && key[0] <= BALIndexSlot:
			first := ^binary.BigEndian.Uint64(key[len(key)-8:])
			if first > number {
				err = batch.Delete(common.CopyBytes(key))
				break
			}
			gaps, block := []byte{}, first
			for v := value; len(v) > 0; {
				gap, n := binary.Uvarint(v)
				if n <= 0 || block+gap > number {
					break
				}
				gaps, block, v = binary.AppendUvarint(gaps, gap), block+gap, v[n:]
			}
			if len(gaps) != len(value) {
				err = batch.Put(common.CopyBytes(key), gaps)
			}
		}
		if err != nil {
			return err
		}
		if _, err := WriteBatchesOverThreshold(batch); err != nil {
			return err
		}
	}
	if err := it.Error(); err != nil {
		return err
	}
	enc, err := rlp.EncodeToBytes(meta)
	if err != nil {
		return err
	}
	if err := batch.Put(balIndexMetaKey, enc); err != nil {
		return err
	}
	return batch.Write()
}

// readBALIndexLast returns the last block at or before number recorded under
// key, a BALIndexKey or BALIndexSlotKey. ok is false if there is none.
func (dbm *databaseManager) readBALIndexLast(key []byte, number uint64) (block uint64, ok bool, err error) {
	db, err := dbm.balIndexDB()
	if err != nil {
		return 0, false, err
	}
	it := db.NewIterator(key, invertedBlock(number))
	defer it.Release()
	if !it.Next() {
		return 0, false, it.Error()
	}
	k := it.Key()
	if len(k) != len(key)+8 {
		return 0, false, fmt.Errorf("malformed BAL index key %x", k)
	}
	block = ^binary.BigEndian.Uint64(k[len(key):])
	for value := it.Value(); len(value) > 0; {
		gap, n := binary.Uvarint(value)
		if n <= 0 {
			return 0, false, fmt.Errorf("malformed BAL index value at key %x", k)
		}
		if block+gap > number {
			break
		}
		block += gap
		value = value[n:]
	}
	return block, true, nil
}

// readBALIndexSlots returns the slots of addr in the BAL index first written
// at or before number, in slot order.
func (dbm *databaseManager) readBALIndexSlots(addr common.Address, number uint64) ([]common.Hash, error) {
	db, err := dbm.balIndexDB()
	if err != nil {
		return nil, err
	}
	prefix := BALIndexKey(BALIndexSlot, addr)
	it := db.NewIterator(prefix, nil)
	defer it.Release()
	var slots []common.Hash
	for it.Next() {
		key := it.Key()
		if len(key) != len(prefix)+common.HashLength+8 {
			return nil, fmt.Errorf("malformed BAL index key %x", key)
		}
		if first := ^binary.BigEndian.Uint64(key[len(key)-8:]); first > number {
			continue
		}
		slot := common.BytesToHash(key[len(prefix) : len(prefix)+common.HashLength])
		if n := len(slots); n == 0 || slots[n-1] != slot {
			slots = append(slots, slot)
		}
	}
	return slots, it.Error()
}

var (
	ErrBALNotIndexed = errors.New("block is not covered by the BAL index")
	errBALIndexGap   = errors.New("block is not the next block of the BAL index")
	errBALIndexBase  = errors.New("BAL index has another base")
)

// BALIndexBuilder adds block access lists to the index in block order. The blocks
// not yet written to the database, the tail of the index, are held in memory
// and read with the database; Flush writes them as chunks, together with the
// new end of the index. A crash loses the tail, which is indexed again from
// the stored lists.
type BALIndexBuilder struct {
	db      DBManager
	meta    BALIndexMeta // Next counts the tail
	flushed uint64       // the end of the index in the database

	mu      sync.RWMutex // guards the tail against its readers
	pending map[string][]uint64
	slots   map[common.Address]map[common.Hash]uint64 // written slot -> first block of the tail
	size    int                                       // rough bytes held in the tail
}

// OpenBALIndexBuilder opens the builder of the index in db, nil if there is no
// index.
func OpenBALIndexBuilder(db DBManager) (*BALIndexBuilder, error) {
	meta, err := db.readBALIndexMeta()
	if err != nil || meta == nil {
		return nil, err
	}
	return NewBALIndexBuilder(db, meta.Base, meta.BaseRoot)
}

// NewBALIndexBuilder opens the index in db, or starts one from the state of block
// base with the given root. An existing index must have the same base.
func NewBALIndexBuilder(db DBManager, base uint64, baseRoot common.Hash) (*BALIndexBuilder, error) {
	meta, err := db.readBALIndexMeta()
	if err != nil {
		return nil, err
	}
	if meta == nil {
		meta = &BALIndexMeta{Base: base, BaseRoot: baseRoot, Next: base + 1}
	} else if meta.Base != base || meta.BaseRoot != baseRoot {
		return nil, fmt.Errorf("%w: block %d root %x", errBALIndexBase, meta.Base, meta.BaseRoot)
	}
	b := &BALIndexBuilder{db: db, meta: *meta, flushed: meta.Next}
	b.resetTail()
	return b, nil
}

func (b *BALIndexBuilder) resetTail() {
	b.pending, b.slots, b.size = make(map[string][]uint64), make(map[common.Address]map[common.Hash]uint64), 0
}

// Next returns the number of the next block to add.
func (b *BALIndexBuilder) Next() uint64 { return b.meta.Next }

// Size returns roughly how many bytes are held in the tail.
func (b *BALIndexBuilder) Size() int { return b.size }

// Add indexes the list of the given block, which must be Next().
func (b *BALIndexBuilder) Add(number uint64, list *types.BlockAccessList) error {
	if number != b.meta.Next {
		return fmt.Errorf("%w: block %d, next %d", errBALIndexGap, number, b.meta.Next)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	balIndexKeys(list, func(key []byte) {
		k := string(key)
		if _, ok := b.pending[k]; !ok {
			b.size += len(k) + 48
		}
		b.pending[k] = append(b.pending[k], number)
		b.size += 8
	})
	for i := range list.Accounts {
		a := &list.Accounts[i]
		for _, w := range a.StorageChanges {
			m := b.slots[a.Address]
			if m == nil {
				m = make(map[common.Hash]uint64)
				b.slots[a.Address] = m
			}
			if _, ok := m[w.Slot]; !ok {
				m[w.Slot] = number
			}
		}
	}
	b.meta.Next++
	return nil
}

// balIndexKeys calls add with every index key list changes.
func balIndexKeys(list *types.BlockAccessList, add func(key []byte)) {
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if len(a.BalanceChanges) > 0 {
			add(BALIndexKey(BALIndexBalance, a.Address))
		}
		if len(a.NonceChanges) > 0 {
			add(BALIndexKey(BALIndexNonce, a.Address))
		}
		if len(a.CodeChanges) > 0 {
			add(BALIndexKey(BALIndexCode, a.Address))
		}
		if len(a.AccountRecords) > 0 {
			add(BALIndexKey(BALIndexAccount, a.Address))
		}
		if len(a.Deleted) > 0 || len(a.StorageCleared) > 0 {
			add(BALIndexKey(BALIndexReset, a.Address))
		}
		for _, w := range a.StorageChanges {
			add(BALIndexSlotKey(a.Address, w.Slot))
		}
	}
}

// Flush writes the tail and the new end of the index.
func (b *BALIndexBuilder) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.db.writeBALIndex(b.pending, &b.meta); err != nil {
		return err
	}
	b.flushed = b.meta.Next
	b.resetTail()
	return nil
}

// Truncate drops the blocks after number, which must be after the base.
func (b *BALIndexBuilder) Truncate(number uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.meta.Next = number + 1
	if number+1 < b.flushed {
		b.resetTail()
		b.flushed = b.meta.Next
		return b.db.truncateBALIndex(number, &b.meta)
	}
	for k, blocks := range b.pending {
		i := sort.Search(len(blocks), func(i int) bool { return blocks[i] > number })
		if i == 0 {
			delete(b.pending, k)
		} else {
			b.pending[k] = blocks[:i]
		}
	}
	for addr, m := range b.slots {
		for slot, first := range m {
			if first > number {
				delete(m, slot)
			}
		}
		if len(m) == 0 {
			delete(b.slots, addr)
		}
	}
	return nil
}

func (b *BALIndexBuilder) last(key []byte, number uint64) (uint64, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	blocks := b.pending[string(key)]
	i := sort.Search(len(blocks), func(i int) bool { return blocks[i] > number })
	if i == 0 {
		return 0, false
	}
	return blocks[i-1], true
}

func (b *BALIndexBuilder) slotsBefore(addr common.Address, number uint64) []common.Hash {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var slots []common.Hash
	for slot, first := range b.slots[addr] {
		if first <= number {
			slots = append(slots, slot)
		}
	}
	return slots
}

// BALIndex looks up the blocks that changed a field or a slot.
type BALIndex struct {
	db   DBManager
	meta BALIndexMeta
	tail *BALIndexBuilder // the blocks not in the database, nil if none
}

// OpenBALIndex opens the BAL index of db. It returns nil if there is no index.
func OpenBALIndex(db DBManager) (*BALIndex, error) {
	meta, err := db.readBALIndexMeta()
	if err != nil || meta == nil {
		return nil, err
	}
	return &BALIndex{db: db, meta: *meta}, nil
}

// Meta returns what the index covered when it was opened.
func (ix *BALIndex) Meta() BALIndexMeta { return ix.meta }

// Check returns ErrBALNotIndexed if block number is not indexed.
func (ix *BALIndex) Check(number uint64) error {
	if number < ix.meta.Base || number >= ix.meta.Next {
		return fmt.Errorf("%w: block %d, indexed %d-%d", ErrBALNotIndexed, number, ix.meta.Base, ix.meta.Next-1)
	}
	return nil
}

// Last returns the last block at or before number that changed what key
// indexes.
func (ix *BALIndex) Last(key []byte, number uint64) (uint64, bool, error) {
	if ix.tail != nil {
		if block, ok := ix.tail.last(key, number); ok {
			return block, true, nil
		}
	}
	return ix.db.readBALIndexLast(key, number)
}

// SlotsBefore returns the slots of addr written in a block after the base and at
// or before number, in slot order.
func (ix *BALIndex) SlotsBefore(addr common.Address, number uint64) ([]common.Hash, error) {
	slots, err := ix.db.readBALIndexSlots(addr, number)
	if err != nil || ix.tail == nil {
		return slots, err
	}
	for _, slot := range ix.tail.slotsBefore(addr, number) {
		if i, found := slices.BinarySearchFunc(slots, slot, func(a, b common.Hash) int { return bytes.Compare(a[:], b[:]) }); !found {
			slots = slices.Insert(slots, i, slot)
		}
	}
	return slots, nil
}

// Meta returns what the builder covers now.
func (b *BALIndexBuilder) Meta() BALIndexMeta {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.meta
}

// Index returns the index as the builder holds it now, its tail included.
func (b *BALIndexBuilder) Index() *BALIndex {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return &BALIndex{db: b.db, meta: b.meta, tail: b}
}
