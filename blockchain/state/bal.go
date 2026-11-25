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

package state

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/hashchain"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
)

// BALConfig are the settings of a BALHistory.
type BALConfig struct {
	SkipReads bool // Stores the writes of the lists only, without their reads
}

// BALHistory is the BAL history of a history node, kept in its chain
// database. It stores the block access list of every written block, the one
// the block was inserted with or the one recorded while it was executed: the
// writes of a list, and its reads apart unless they are skipped. It keeps the
// range of blocks whose lists are all stored; a list that does not touch the
// range, like the one of a block written while a regeneration fills in
// earlier blocks, is stored and joins the range once the blocks in between
// are.
//
// It indexes the lists, the BAL index next to the chain database: a written
// block is added at once, and the index loop indexes the lists stored
// otherwise, as after a restart or by a regeneration, and rebuilds the index
// when its base changes. The index starts from the block before the first
// stored list, whose state, the base, comes from base.
//
// From the index and the lists it reads the state at the end of any indexed
// block without a state trie: a value is the one set by the last block at or
// before the requested block that changed it, or the base value. The rules
// mirror how ApplyBAL applies a list.
type BALHistory struct {
	db   database.DBManager
	cfg  BALConfig
	head func() *types.Header
	base baseFunc

	mu          sync.Mutex // guards first and next
	first, next uint64     // lists of blocks first to next-1 are stored; first == next if none

	lists   *lru.Cache // the last lists read, by block number
	signals chan struct{}
	ixMu    sync.Mutex                               // serialises the writers of the index
	builder atomic.Pointer[database.BALIndexBuilder] // nil until the index is opened or started
	gen     atomic.Uint64                            // counts the rebuilds and truncations of the index

	baseMu    sync.Mutex
	baseState BaseState // the base of the index of generation baseGen, nil until read
	baseGen   uint64

	loopMu sync.Mutex
	quit   chan struct{} // stops the index loop, nil while it is stopped
	wg     sync.WaitGroup
}

// baseFunc returns the state of block number, whose root is root, which the
// index starts from.
type baseFunc func(number uint64, root common.Hash) (BaseState, error)

// OpenBALHistory opens the BAL history in db and its index. head returns the
// head header, and base the state of a block, the base of the index.
func OpenBALHistory(db database.DBManager, cfg BALConfig, head func() *types.Header, base func(number uint64, root common.Hash) (BaseState, error)) (*BALHistory, error) {
	lists, _ := lru.New(256)
	s := &BALHistory{db: db, cfg: cfg, head: head, base: base, lists: lists, signals: make(chan struct{}, 1)}
	s.first, s.next = db.ReadBALRange()
	s.mu.Lock()
	s.extendLocked()
	s.mu.Unlock()
	b, err := database.OpenBALIndexBuilder(db)
	if err != nil {
		return nil, err
	}
	if b != nil {
		s.builder.Store(b)
	}
	logger.Info("Storing block access lists", "first", s.first, "next", s.next, "skipReads", cfg.SkipReads)
	return s, nil
}

// SkipsReads reports whether only the writes of the lists are stored.
func (s *BALHistory) SkipsReads() bool { return s.cfg.SkipReads }

// Range returns the blocks whose lists are all stored, first to next-1.
func (s *BALHistory) Range() (first, next uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.first, s.next
}

// firstStored returns the first block with a stored list, the block after the head
// if none.
func (s *BALHistory) firstStored() uint64 {
	if first, next := s.Range(); first < next {
		return first
	}
	return s.head().Number.Uint64() + 1
}

// Put stores the writes of the list of a block, and its reads unless they are
// nil, knowing that the lists of blocks from to number are now all stored,
// and joins them to the range if they touch it or if it is empty.
func (s *BALHistory) Put(number uint64, hash common.Hash, writes, reads []byte, from uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reads != nil {
		if err := s.db.WriteBALReads(hash, number, reads); err != nil {
			return err
		}
	}
	if err := s.db.WriteBAL(hash, number, writes); err != nil {
		return err
	}
	switch {
	case s.first == s.next:
		s.first, s.next = from, number+1
	case from <= s.next && number+1 >= s.first:
		s.first, s.next = min(s.first, from), max(s.next, number+1)
	}
	s.extendLocked()
	return nil
}

// extendLocked joins the stored lists around the range and saves it.
func (s *BALHistory) extendLocked() {
	if s.first == s.next {
		return
	}
	for s.db.HasBAL(s.db.ReadCanonicalHash(s.next), s.next) {
		s.next++
	}
	for s.first > 1 && s.db.HasBAL(s.db.ReadCanonicalHash(s.first-1), s.first-1) {
		s.first--
	}
	s.saveRangeLocked()
}

func (s *BALHistory) saveRangeLocked() {
	if first, next := s.db.ReadBALRange(); first != s.first || next != s.next {
		if err := s.db.WriteBALRange(s.first, s.next); err != nil {
			logger.Error("Failed to write the block access list range", "err", err)
		}
	}
}

// RewindTo does nothing: RewindDelete drops the lists of the deleted blocks.
func (s *BALHistory) RewindTo(newBlock *types.Block) {}

// RewindDelete deletes the list of a block deleted by a rewind and drops the
// block from the range and the index.
func (s *BALHistory) RewindDelete(hash common.Hash, number uint64) {
	if s == nil {
		return
	}
	s.db.DeleteBAL(hash, number)
	s.db.DeleteBALReads(hash, number)
	s.mu.Lock()
	if number < s.next {
		if number <= s.first {
			s.first, s.next = 0, 0
		} else {
			s.next = number
		}
		s.saveRangeLocked()
	}
	s.mu.Unlock()
	s.Truncate(number)
}

// PutInserted stores the writes of the list a block was inserted with, and
// indexes it.
func (s *BALHistory) PutInserted(block *types.Block, writes []byte) {
	number := block.NumberU64()
	list, err := types.DecodeBAL(writes)
	if err != nil {
		logger.Error("Invalid block access list", "number", number, "err", err)
		return
	}
	s.put(block, list, writes, nil)
}

func (s *BALHistory) put(block *types.Block, list *types.BlockAccessList, writes, reads []byte) {
	number := block.NumberU64()
	s.restartAfterGap(number)
	if err := s.Put(number, block.Hash(), writes, reads, number); err != nil {
		logger.Error("Failed to write block access list", "number", number, "err", err)
		return
	}
	s.add(block, list)
}

// restartAfterGap starts the range again at the list of the written block
// number if the lists before it are missing, as a node leaves them by running
// in another mode for a while. The earlier lists join the range again once a
// regeneration fills the blocks in between.
func (s *BALHistory) restartAfterGap(number uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first == s.next || number <= s.next {
		return
	}
	logger.Warn("Block access lists are missing before this block, as the node ran in another mode; regenerate them to join the earlier ones",
		"number", number, "missing", fmt.Sprintf("%d-%d", s.next, number-1), "earlier", fmt.Sprintf("%d-%d", s.first, s.next-1))
	s.first, s.next = 0, 0
}

// Stored reports whether the list of a block is stored.
func (s *BALHistory) Stored(number uint64, hash common.Hash) bool {
	first, next := s.Range()
	return (first <= number && number < next) || s.db.HasBAL(hash, number)
}

// PutExecuted stores the list recorded while block was executed, which the
// caller checked, unless it is stored already, and indexes it.
func (s *BALHistory) PutExecuted(block *types.Block, list *types.BlockAccessList) {
	number, hash := block.NumberU64(), block.Hash()
	if s.Stored(number, hash) {
		return // e.g. a block inserted again after a rewind
	}
	if list == nil {
		logger.Error("Block written without a recorded block access list", "number", number, "hash", hash)
		return
	}
	enc, err := list.Writes().Encode()
	var reads []byte
	if err == nil && !s.cfg.SkipReads {
		reads, err = list.Reads().Encode()
	}
	if err != nil {
		logger.Error("Failed to encode block access list", "number", number, "err", err)
		return
	}
	s.put(block, list, enc, reads)
}

// Indexes reports whether s indexes its lists, so that past states are read
// from them.
func (s *BALHistory) Indexes() bool {
	return s != nil
}

// SignalIndex asks the BAL index to catch up with the stored lists.
func (s *BALHistory) SignalIndex() {
	if s != nil {
		s.signal()
	}
}

// Start starts the loop that indexes the lists not added as their blocks were
// written. A rewind stops it and starts it again.
func (s *BALHistory) Start() error {
	if s == nil {
		return nil
	}
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	if s.quit == nil {
		s.quit = make(chan struct{})
		s.wg.Add(1)
		go func(quit chan struct{}) {
			defer s.wg.Done()
			s.loop(quit)
		}(s.quit)
	}
	return nil
}

// Stop stops the index loop, which writes the tail of the index.
func (s *BALHistory) Stop() {
	if s == nil {
		return
	}
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	if s.quit != nil {
		close(s.quit)
		s.quit = nil
		s.wg.Wait()
	}
}

// ErrNoHistory is returned for the state of a block before the BAL history.
var ErrNoHistory = errors.New("no history")

// HistoryError explains why the state of a block is missing: the history of
// the node starts later, and ken db regen can build the earlier part,
// or the BAL index has not reached the block yet.
func (s *BALHistory) HistoryError(number uint64, err error) error {
	if s == nil {
		return err
	}
	if first := s.firstStored(); number < first {
		return fmt.Errorf("%w before block %d, regenerate it with ken db regen: %v", ErrNoHistory, first, err)
	}
	if base, next, ok := s.Indexed(); !ok || number >= next || number < base {
		return fmt.Errorf("%w yet: the BAL index covers blocks %d to %d: %v", ErrNoHistory, base, int64(next)-1, err)
	}
	return err
}

// Recorder records the block access list while a block is executed, from what
// the state reports to it. It only observes the state and never changes state
// or gas.
//
// Reads are recorded when an account is accessed and when a slot is read
// through GetState or GetCommittedState. Writes are recorded as post values
// whenever the journal is flushed: at every Finalise, which marks the end of a
// tx, and when the index changes, so that changes of InitializeState that were
// not finalised are not attributed to the first tx.
//
// Reads are held as pending until the index they belong to is kept: they are
// committed at Finalise and at the end of InitializeState, and dropped when a
// tx starts and when a whole tx is reverted. So a block producer that reads
// state between txs, or runs a tx it then drops, records the same reads as a
// validator that runs only the included txs.
//
// A field keeps one change per index, its value at the end of the index: a
// later record with the same index replaces it.
type Recorder struct {
	index    uint32
	accounts map[common.Address]*accountEntry

	pendingAccounts map[common.Address]struct{}
	pendingSlots    map[common.Address]map[common.Hash]struct{}

	// seen holds, per account, the values as of the last recorded change, or
	// as loaded from the parent. A change is recorded only if the value
	// differs from it.
	seen map[common.Address]*accountView
}

// accountEntry is what the list has recorded of an account so far.
type accountEntry struct {
	reads    map[common.Hash]struct{}
	slots    map[common.Hash][]types.StorageChange
	balances []types.BalanceChange
	nonces   []types.NonceChange
	codes    []types.CodeChange
	accounts []types.AccountRecord
	deleted  []uint32
	cleared  []uint32
}

// Change is an account an index changed, as the state reports it to Record.
type Change struct {
	Address common.Address
	// Deleted tells that the account is about to be deleted, and Recreated
	// that it was created again over an existing one.
	Deleted, Recreated bool
	// Account is the account after the change, unless Deleted.
	Account account.Account
	// Code returns the code of the account after the change.
	Code func() []byte
	// Slots maps the written slots to their values before the first write and
	// after the change.
	Slots map[common.Hash]SlotWrite
}

// SlotWrite is the value of a written slot before its first write and after
// the change.
type SlotWrite struct {
	Pre, Post common.Hash
}

type accountView struct {
	exists   bool
	balance  *big.Int
	nonce    uint64
	codeHash common.Hash
	fields   []byte // Kaia-specific fields, see kaiaFields
}

func newAccountView(acc account.Account) *accountView {
	if acc == nil {
		return &accountView{balance: new(big.Int), codeHash: types.EmptyCodeHash}
	}
	codeHash := types.EmptyCodeHash
	if pa := account.GetProgramAccount(acc); pa != nil {
		codeHash = common.BytesToHash(pa.GetCodeHash())
	}
	return &accountView{
		exists:   true,
		balance:  new(big.Int).Set(acc.GetBalance()),
		nonce:    acc.GetNonce(),
		codeHash: codeHash,
		fields:   kaiaFields(acc),
	}
}

// kaiaFields encodes the account without balance, nonce, code hash and storage
// root, leaving the Kaia-specific fields: account type, humanReadable, key and
// code info.
func kaiaFields(acc account.Account) []byte {
	c := acc.DeepCopy()
	c.SetBalance(new(big.Int))
	c.SetNonce(0)
	if pa := account.GetProgramAccount(c); pa != nil {
		pa.SetStorageRoot(common.ExtHash{})
		pa.SetCodeHash(types.EmptyCodeHash.Bytes())
	}
	enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(c))
	if err != nil {
		logger.Error("Failed to encode account fields for the block access list", "err", err)
	}
	return enc
}

// NewRecorder returns a Recorder starting at index idx: 0 for
// InitializeState, i+1 for the i-th tx, and len(txs)+1 for FinalizeState.
func NewRecorder(idx int) *Recorder {
	return &Recorder{
		index:           uint32(idx),
		accounts:        make(map[common.Address]*accountEntry),
		seen:            make(map[common.Address]*accountView),
		pendingAccounts: make(map[common.Address]struct{}),
		pendingSlots:    make(map[common.Address]map[common.Hash]struct{}),
	}
}

// Copy returns a deep copy of the recorder. Views in seen are replaced, never
// modified, so they are shared.
func (r *Recorder) Copy() *Recorder {
	if r == nil {
		return nil
	}
	cpy := &Recorder{
		index:           r.index,
		accounts:        make(map[common.Address]*accountEntry, len(r.accounts)),
		seen:            maps.Clone(r.seen),
		pendingAccounts: maps.Clone(r.pendingAccounts),
		pendingSlots:    make(map[common.Address]map[common.Hash]struct{}, len(r.pendingSlots)),
	}
	for addr, m := range r.pendingSlots {
		cpy.pendingSlots[addr] = maps.Clone(m)
	}
	for addr, a := range r.accounts {
		c := &accountEntry{
			reads:    maps.Clone(a.reads),
			slots:    make(map[common.Hash][]types.StorageChange, len(a.slots)),
			balances: slices.Clone(a.balances),
			nonces:   slices.Clone(a.nonces),
			codes:    slices.Clone(a.codes),
			accounts: slices.Clone(a.accounts),
			deleted:  slices.Clone(a.deleted),
			cleared:  slices.Clone(a.cleared),
		}
		for slot, changes := range a.slots {
			c.slots[slot] = slices.Clone(changes)
		}
		cpy.accounts[addr] = c
	}
	return cpy
}

// StartIndex moves on to index idx. The changes not yet finalised, which
// changes returns, are recorded under the old index. Pending reads are
// dropped: they were made after the last Finalise, outside of any tx, as a
// block producer does.
func (r *Recorder) StartIndex(idx int, changes func() []Change) {
	if r == nil {
		return
	}
	r.DropReads()
	if r.index == uint32(idx) {
		return
	}
	r.record(changes())
	r.index = uint32(idx)
}

// EndInitialization keeps the changes, which changes returns, and the reads
// of InitializeState, which may not be finalised.
func (r *Recorder) EndInitialization(changes func() []Change) {
	if r == nil {
		return
	}
	r.record(changes())
	r.keepReads()
}

// Finalise records the changes of a finished tx, which changes returns, and
// keeps its reads.
func (r *Recorder) Finalise(changes func() []Change) {
	if r == nil {
		return
	}
	c := changes()
	r.keepReads()
	r.record(c)
}

func (r *Recorder) record(changes []Change) {
	for i := range changes {
		r.Record(&changes[i])
	}
}

// Base takes acc, nil if there is none, as the base of the changes of addr,
// for an account loaded before the recorder started.
func (r *Recorder) Base(addr common.Address, acc account.Account) {
	r.seen[addr] = newAccountView(acc)
}

// Loaded takes the values of a newly loaded account, nil if there is none, as
// the base of its changes, and records the read.
func (r *Recorder) Loaded(addr common.Address, acc account.Account) {
	if r == nil {
		return
	}
	if _, ok := r.seen[addr]; !ok {
		r.seen[addr] = newAccountView(acc)
	}
	r.ReadAccount(addr)
}

// ReadAccount records a read of an account, loaded or not. It is recorded on
// every access, not on load only: a tx that is dropped may be the one that
// loaded the account.
func (r *Recorder) ReadAccount(addr common.Address) {
	if r == nil {
		return
	}
	r.pendingAccounts[addr] = struct{}{}
}

// ReadSlot records a read of a slot.
func (r *Recorder) ReadSlot(addr common.Address, slot common.Hash) {
	if r == nil {
		return
	}
	m := r.pendingSlots[addr]
	if m == nil {
		m = make(map[common.Hash]struct{})
		r.pendingSlots[addr] = m
	}
	m[slot] = struct{}{}
}

func (r *Recorder) keepReads() {
	for addr := range r.pendingAccounts {
		r.entry(addr)
	}
	for addr, slots := range r.pendingSlots {
		a := r.entry(addr)
		for slot := range slots {
			a.reads[slot] = struct{}{}
		}
	}
	r.DropReads()
}

// DropReads drops the pending reads, as when a whole tx is dropped.
func (r *Recorder) DropReads() {
	if r == nil {
		return
	}
	if len(r.pendingAccounts) > 0 {
		r.pendingAccounts = make(map[common.Address]struct{})
	}
	if len(r.pendingSlots) > 0 {
		r.pendingSlots = make(map[common.Address]map[common.Hash]struct{})
	}
}

// Record records the post values of a changed account at the current index.
func (r *Recorder) Record(c *Change) {
	addr, idx := c.Address, r.index
	// prev is nil if the account was loaded before the recorder started. Then
	// everything is recorded.
	prev := r.seen[addr]
	if c.Deleted {
		if prev == nil || prev.exists {
			r.entry(addr).delete(idx)
		}
		r.seen[addr] = newAccountView(nil)
		return
	}

	acc := c.Account
	cur := newAccountView(acc)
	// Only an account that existed has a storage to drop. One that comes into
	// existence after being touched and removed as empty earlier in the block
	// has none, and the list must not depend on that history: an index executed
	// from the list alone does not see it.
	if c.Recreated && (prev == nil || prev.exists) {
		a := r.entry(addr)
		a.trimSlots(idx)
		a.cleared = appendIndex(a.cleared, idx)
	}
	if prev == nil || !prev.exists || c.Recreated || !bytes.Equal(prev.fields, cur.fields) {
		enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(acc))
		if err != nil {
			logger.Error("Failed to encode account for the block access list", "addr", addr, "err", err)
		}
		a := r.entry(addr)
		a.accounts = upsert(a.accounts, types.AccountRecord{BlockAccessIndex: idx, Account: enc}, recordIndex)
	}
	if prev == nil || prev.balance.Cmp(cur.balance) != 0 {
		a := r.entry(addr)
		a.balances = upsert(a.balances, types.BalanceChange{BlockAccessIndex: idx, PostBalance: new(big.Int).Set(cur.balance)}, balanceIndex)
	}
	if prev == nil || prev.nonce != cur.nonce {
		a := r.entry(addr)
		a.nonces = upsert(a.nonces, types.NonceChange{BlockAccessIndex: idx, NewNonce: cur.nonce}, nonceIndex)
	}
	if prev == nil || prev.codeHash != cur.codeHash {
		a := r.entry(addr)
		a.codes = upsert(a.codes, types.CodeChange{BlockAccessIndex: idx, NewCode: common.CopyBytes(c.Code())}, codeIndex)
	}
	for slot, w := range c.Slots {
		// After a recreation the storage starts empty, so every written slot
		// is recorded even if it went back to the value before the first write.
		a := r.entry(addr)
		if w.Post != w.Pre || c.Recreated {
			a.slots[slot] = upsert(a.slots[slot], types.StorageChange{BlockAccessIndex: idx, NewValue: w.Post}, storageIndex)
		} else {
			a.reads[slot] = struct{}{}
		}
	}
	r.seen[addr] = cur
}

func (r *Recorder) entry(addr common.Address) *accountEntry {
	a := r.accounts[addr]
	if a == nil {
		a = &accountEntry{
			reads: make(map[common.Hash]struct{}),
			slots: make(map[common.Hash][]types.StorageChange),
		}
		r.accounts[addr] = a
	}
	return a
}

// delete records that the account was deleted at the end of index idx. The
// changes recorded earlier at the same index are dropped. Changes recorded
// later with the same index belong to a recreation after the deletion.
func (a *accountEntry) delete(idx uint32) {
	a.trimSlots(idx)
	a.balances = trim(a.balances, idx, balanceIndex)
	a.nonces = trim(a.nonces, idx, nonceIndex)
	a.codes = trim(a.codes, idx, codeIndex)
	a.accounts = trim(a.accounts, idx, recordIndex)
	if n := len(a.cleared); n > 0 && a.cleared[n-1] == idx {
		a.cleared = a.cleared[:n-1]
	}
	a.deleted = appendIndex(a.deleted, idx)
}

// trimSlots drops the slot writes recorded at index idx, which a deletion or a
// recreation at the same index supersedes.
func (a *accountEntry) trimSlots(idx uint32) {
	for slot, changes := range a.slots {
		if changes = trim(changes, idx, storageIndex); len(changes) == 0 {
			delete(a.slots, slot)
		} else {
			a.slots[slot] = changes
		}
	}
}

// List returns the list recorded so far in canonical order, nil without a
// recorder.
func (r *Recorder) List() *types.BlockAccessList {
	if r == nil {
		return nil
	}
	addrs := slices.SortedFunc(maps.Keys(r.accounts), func(x, y common.Address) int { return bytes.Compare(x[:], y[:]) })
	list := &types.BlockAccessList{Accounts: make([]types.AccountChanges, 0, len(addrs))}
	for _, addr := range addrs {
		a := r.accounts[addr]
		acc := types.AccountChanges{
			Address:        addr,
			BalanceChanges: a.balances,
			NonceChanges:   a.nonces,
			CodeChanges:    a.codes,
			AccountRecords: a.accounts,
			Deleted:        a.deleted,
			StorageCleared: a.cleared,
		}
		for slot, changes := range a.slots {
			acc.StorageChanges = append(acc.StorageChanges, types.SlotChanges{Slot: slot, Changes: changes})
		}
		slices.SortFunc(acc.StorageChanges, func(x, y types.SlotChanges) int { return bytes.Compare(x.Slot[:], y.Slot[:]) })
		for slot := range a.reads {
			if _, written := a.slots[slot]; !written {
				acc.StorageReads = append(acc.StorageReads, slot)
			}
		}
		slices.SortFunc(acc.StorageReads, func(x, y common.Hash) int { return bytes.Compare(x[:], y[:]) })
		list.Accounts = append(list.Accounts, acc)
	}
	return list
}

// upsert appends v, or replaces the last element of list if it has the same
// index.
func upsert[T any](list []T, v T, index func(T) uint32) []T {
	if n := len(list); n > 0 && index(list[n-1]) == index(v) {
		list[n-1] = v
		return list
	}
	return append(list, v)
}

// trim drops the last element of list if index gives it index idx.
func trim[T any](list []T, idx uint32, index func(T) uint32) []T {
	if n := len(list); n > 0 && index(list[n-1]) == idx {
		return list[:n-1]
	}
	return list
}

func storageIndex(c types.StorageChange) uint32 { return c.BlockAccessIndex }

func balanceIndex(c types.BalanceChange) uint32 { return c.BlockAccessIndex }

func nonceIndex(c types.NonceChange) uint32 { return c.BlockAccessIndex }

func codeIndex(c types.CodeChange) uint32 { return c.BlockAccessIndex }

func recordIndex(c types.AccountRecord) uint32 { return c.BlockAccessIndex }

func appendIndex(list []uint32, idx uint32) []uint32 {
	if n := len(list); n > 0 && list[n-1] == idx {
		return list
	}
	return append(list, idx)
}

var (
	errBALMissingAccount = errors.New("recreated or new account has no whole account record")
	errBALNoAccount      = errors.New("changed account does not exist and has no whole account record")
	errBALStorageRoot    = errors.New("storage root mismatch")
)

// Writer is the state a block access list is applied to, see Apply.
type Writer interface {
	Exist(addr common.Address) bool
	SelfDestruct(addr common.Address)
	// SetAccount replaces the whole account at addr, keeping its storage
	// unless clearStorage is set.
	SetAccount(addr common.Address, acc account.Account, clearStorage bool)
	SetBalance(addr common.Address, amount *big.Int)
	SetNonce(addr common.Address, nonce uint64)
	// ApplyCode sets the code of addr without loading the previous one.
	ApplyCode(addr common.Address, code []byte) error
	SetState(addr common.Address, key, value common.Hash)
	GetStorageRoot(addr common.Address) common.Hash
}

// Apply writes the final values of list to w, which holds the parent state of
// the block.
func Apply(w Writer, list *types.BlockAccessList) error {
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if err := applyAccount(w, a); err != nil {
			return fmt.Errorf("account %s: %w", a.Address.Hex(), err)
		}
	}
	return nil
}

// ApplyBALBefore writes the values of the indices below k of a block access
// list to s, which holds the parent state of the block, and finalises them
// like the end of a tx: s then holds the state at the start of index k. Index
// 0 is InitializeState and index i+1 the i-th tx.
func ApplyBALBefore(s *StateDB, list *types.BlockAccessList, k int) error {
	for i := range list.Accounts {
		a := list.Accounts[i].Before(uint32(k))
		if err := applyAccount(s, &a); err != nil {
			return fmt.Errorf("account %s: %w", a.Address.Hex(), err)
		}
	}
	s.Finalise(true, true)
	return nil
}

// CheckStorageRoots checks the storage roots a list was recorded with against
// w, once its storage roots are computed.
func CheckStorageRoots(w Writer, list *types.BlockAccessList) error {
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if a.StorageRoot == nil {
			continue
		}
		if root := w.GetStorageRoot(a.Address); root != *a.StorageRoot {
			return fmt.Errorf("account %s: %w (recorded %x, applied %x)", a.Address.Hex(), errBALStorageRoot, *a.StorageRoot, root)
		}
	}
	return nil
}

func applyAccount(w Writer, a *types.AccountChanges) error {
	if !a.Changed() {
		return nil
	}
	addr := a.Address

	// Changes before from are superseded by a deletion or a recreation.
	from, cleared, deleted := a.Reset()
	if deleted {
		if w.Exist(addr) {
			w.SelfDestruct(addr)
		}
		return nil
	}

	if n := len(a.AccountRecords); n > 0 && a.AccountRecords[n-1].BlockAccessIndex >= from {
		ser := account.NewAccountSerializer()
		if err := rlp.DecodeBytes(a.AccountRecords[n-1].Account, ser); err != nil {
			return err
		}
		w.SetAccount(addr, ser.GetAccount(), cleared)
	} else if cleared {
		return errBALMissingAccount
	}
	if !w.Exist(addr) {
		return errBALNoAccount
	}

	// Every change of a field is recorded, so the last one in range is the
	// final value, consistent with the whole account record if there is one.
	if n := len(a.BalanceChanges); n > 0 && a.BalanceChanges[n-1].BlockAccessIndex >= from {
		w.SetBalance(addr, a.BalanceChanges[n-1].PostBalance)
	}
	if n := len(a.NonceChanges); n > 0 && a.NonceChanges[n-1].BlockAccessIndex >= from {
		w.SetNonce(addr, a.NonceChanges[n-1].NewNonce)
	}
	if n := len(a.CodeChanges); n > 0 && a.CodeChanges[n-1].BlockAccessIndex >= from {
		if err := w.ApplyCode(addr, a.CodeChanges[n-1].NewCode); err != nil {
			return err
		}
	}
	for _, sc := range a.StorageChanges {
		if last := sc.Changes[len(sc.Changes)-1]; last.BlockAccessIndex >= from {
			w.SetState(addr, sc.Slot, last.NewValue)
		}
	}
	return nil
}

// loop indexes the lists that were not added as they were written, and
// writes the tail of the index when it stops.
func (s *BALHistory) loop(quit <-chan struct{}) {
	for {
		if err := s.UpdateIndex(quit); err != nil {
			logger.Warn("Failed to update the BAL index", "err", err)
		}
		select {
		case <-quit:
			s.flush()
			return
		case <-s.signals:
		}
	}
}

func (s *BALHistory) flush() {
	s.ixMu.Lock()
	defer s.ixMu.Unlock()
	if b := s.builder.Load(); b != nil && b.Size() > 0 {
		if err := b.Flush(); err != nil {
			logger.Error("Failed to write the BAL index", "err", err)
		}
	}
}

// add indexes the list of a written canonical block if it is the next one,
// and leaves it to the loop otherwise.
func (s *BALHistory) add(block *types.Block, list *types.BlockAccessList) {
	if s == nil {
		return
	}
	s.ixMu.Lock()
	defer s.ixMu.Unlock()
	number := block.NumberU64()
	if s.builder.Load() == nil {
		// The first stored list starts the index.
		if first, _ := s.db.ReadBALRange(); number == max(first, 1) {
			if err := s.start(number - 1); err != nil {
				logger.Error("Failed to start the BAL index", "number", number, "err", err)
			}
		}
	}
	b := s.builder.Load()
	if b == nil || number != b.Next() || s.db.ReadCanonicalHash(number) != block.Hash() {
		s.signal()
		return
	}
	if err := s.addLocked(number, list); err != nil {
		logger.Error("Failed to index block access list", "number", number, "err", err)
	}
}

func (s *BALHistory) addLocked(number uint64, list *types.BlockAccessList) error {
	b := s.builder.Load()
	if err := b.Add(number, list); err != nil {
		return err
	}
	if b.Size() > flushBytes {
		return b.Flush()
	}
	return nil
}

func (s *BALHistory) signal() {
	select {
	case s.signals <- struct{}{}:
	default:
	}
}

// Truncate drops block number and the blocks after it, which are gone, from
// the index.
func (s *BALHistory) Truncate(number uint64) {
	if s == nil {
		return
	}
	s.ixMu.Lock()
	defer s.ixMu.Unlock()
	b := s.builder.Load()
	if b == nil || number >= b.Next() {
		return
	}
	s.gen.Add(1)
	s.lists.Purge()
	if number <= b.Meta().Base {
		s.builder.Store(nil)
		if err := s.db.DeleteBALIndex(); err != nil {
			logger.Error("Failed to delete the BAL index", "err", err)
		}
		return
	}
	if err := b.Truncate(number - 1); err != nil {
		logger.Error("Failed to truncate the BAL index", "number", number, "err", err)
		s.builder.Store(nil)
		s.signal()
	}
}

// Indexed returns the blocks whose state the BAL index serves, base to
// next-1.
func (s *BALHistory) Indexed() (base, next uint64, ok bool) {
	if s == nil {
		return 0, 0, false
	}
	b := s.builder.Load()
	if b == nil {
		return 0, 0, false
	}
	meta := b.Index().Meta()
	return meta.Base, meta.Next, true
}

// flushBytes is roughly how much the tail of the index holds before it is
// written.
var flushBytes = 64 << 20

// UpdateIndex indexes the stored lists after the end of the index. The index is
// rebuilt if the stored lists start at another block. It stops early, keeping what it indexed, when stop
// is closed.
func (s *BALHistory) UpdateIndex(stop <-chan struct{}) error {
	if s == nil {
		return nil
	}
	s.ixMu.Lock()
	defer s.ixMu.Unlock()

	first, next := s.db.ReadBALRange()
	if first == next {
		return nil
	}
	base := max(first, 1) - 1
	if b := s.builder.Load(); b != nil {
		// The lists start elsewhere, as after a regeneration filled in
		// earlier blocks.
		header := s.db.ReadHeader(s.db.ReadCanonicalHash(base), base)
		moved := header == nil || b.Meta().Base != base || b.Meta().BaseRoot != header.Root
		if moved || b.Next() > next {
			logger.Info("Rebuilding the BAL index", "base", base, "lists", next-first, "oldBase", b.Meta().Base, "oldNext", b.Next())
			s.gen.Add(1)
			s.lists.Purge()
			s.builder.Store(nil)
			if err := s.db.DeleteBALIndex(); err != nil {
				return err
			}
		}
	}
	if s.builder.Load() == nil {
		if err := s.start(base); err != nil {
			return err
		}
	}
	for n := s.builder.Load().Next(); n < next; n++ {
		select {
		case <-stop:
			return nil
		default:
		}
		if common.EmptyHash(s.db.ReadCanonicalHash(n)) {
			break // stored before the block is canonical: indexed with the next update
		}
		list, err := s.db.ReadCanonicalBALList(n)
		if err != nil {
			return err
		}
		if err := s.addLocked(n, list); err != nil {
			return err
		}
	}
	return nil
}

// start starts the index from the state of block base.
func (s *BALHistory) start(base uint64) error {
	header := s.db.ReadHeader(s.db.ReadCanonicalHash(base), base)
	if header == nil {
		return fmt.Errorf("no header of block %d", base)
	}
	b, err := database.NewBALIndexBuilder(s.db, base, header.Root)
	if err != nil {
		return err
	}
	s.builder.Store(b)
	return nil
}

// list returns the list of block number, from the last ones read if it can.
func (s *BALHistory) list(number uint64) (*types.BlockAccessList, error) {
	if v, ok := s.lists.Get(number); ok {
		return v.(*types.BlockAccessList), nil
	}
	list, err := s.db.ReadCanonicalBALList(number)
	if err != nil {
		return nil, err
	}
	s.lists.Add(number, list)
	return list, nil
}

// Index opens the index as it is now, and returns whether it is still valid,
// which it is not after it was rebuilt. It returns ErrNotCovered if nothing
// is indexed.
func (s *BALHistory) Index() (*database.BALIndex, func() bool, error) {
	if s == nil {
		return nil, nil, ErrNotCovered
	}
	gen := s.gen.Load()
	b := s.builder.Load()
	if b == nil {
		return nil, nil, ErrNotCovered
	}
	return b.Index(), func() bool { return s.gen.Load() == gen }, nil
}

// index returns the index as it is now, ErrNotCovered if nothing is indexed.
func (s *BALHistory) index() (*database.BALIndex, error) {
	b := s.builder.Load()
	if b == nil {
		return nil, ErrNotCovered
	}
	return b.Index(), nil
}

// baseOf returns the base of the index, opened once per generation of the
// index.
func (s *BALHistory) baseOf() (BaseState, error) {
	s.baseMu.Lock()
	defer s.baseMu.Unlock()
	gen := s.gen.Load()
	if s.baseState != nil && s.baseGen == gen {
		return s.baseState, nil
	}
	b := s.builder.Load()
	if b == nil {
		return nil, ErrNotCovered
	}
	meta := b.Meta()
	base, err := s.base(meta.Base, meta.BaseRoot)
	if err != nil {
		return nil, fmt.Errorf("base state of the BAL history at block %d: %w", meta.Base, err)
	}
	s.baseState, s.baseGen = base, gen
	return base, nil
}

var (
	errHistoryMissingAccount = errors.New("indexed block does not hold the account")
	errHistoryInconsistent   = errors.New("indexed block does not set the value")

	// ErrNotCovered is returned for a block whose state the BAL history does
	// not serve.
	ErrNotCovered = errors.New("state is not covered by the BAL history")
	// ErrRebuilt is returned by a state read from the BAL history after its
	// index was rebuilt.
	ErrRebuilt = errors.New("the BAL index was rebuilt, open the state again")
)

// BaseState is the state the index starts from.
type BaseState interface {
	// Account returns the account, or nil if it does not exist.
	Account(addr common.Address) (account.Account, error)
	Storage(addr common.Address, slot common.Hash) (common.Hash, error)
	Code(codeHash common.Hash) ([]byte, error)
}

// slotLister is a base state that can list the slots of an account.
type slotLister interface {
	Slots(addr common.Address) ([]common.Hash, error)
}

// Slots returns every slot of addr that may be set at the end of the given
// block: the slots written after the base, and the slots of the base if it
// can list them.
func (s *BALHistory) Slots(addr common.Address, number uint64) ([]common.Hash, error) {
	ix, err := s.index()
	if err != nil {
		return nil, err
	}
	slots, err := ix.SlotsBefore(addr, number)
	if err != nil {
		return nil, err
	}
	base, err := s.baseOf()
	if err != nil {
		return nil, err
	}
	if l, ok := base.(slotLister); ok {
		base, err := l.Slots(addr)
		if err != nil {
			return nil, err
		}
		slots = append(slots, base...)
	}
	return slots, nil
}

func (s *BALHistory) access(number uint64, addr common.Address) (*types.AccountChanges, error) {
	list, err := s.list(number)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", number, err)
	}
	i := sort.Search(len(list.Accounts), func(i int) bool {
		return bytes.Compare(list.Accounts[i].Address[:], addr[:]) >= 0
	})
	if i == len(list.Accounts) || list.Accounts[i].Address != addr {
		return nil, fmt.Errorf("%w: block %d, account %s", errHistoryMissingAccount, number, addr.Hex())
	}
	return &list.Accounts[i], nil
}

// latest returns the access of addr in the last block at or before number
// recorded under any of the given keys. a is nil if none is after the base.
func (s *BALHistory) latest(addr common.Address, number uint64, keys ...[]byte) (*types.AccountChanges, error) {
	ix, err := s.index()
	if err != nil {
		return nil, err
	}
	if err := ix.Check(number); err != nil {
		return nil, err
	}
	last, found, err := lastChange(ix, number, keys)
	if err != nil || !found {
		return nil, err
	}
	return s.access(last, addr)
}

// lastChange returns the last block at or before number recorded under any of
// keys.
func lastChange(ix *database.BALIndex, number uint64, keys [][]byte) (last uint64, found bool, err error) {
	for _, key := range keys {
		block, ok, err := ix.Last(key, number)
		if err != nil {
			return 0, false, err
		}
		if ok && (!found || block > last) {
			last, found = block, true
		}
	}
	return last, found, nil
}

// whole returns the last whole account record that is not superseded, or nil.
func whole(a *types.AccountChanges, from uint32) *types.AccountRecord {
	if n := len(a.AccountRecords); n > 0 && a.AccountRecords[n-1].BlockAccessIndex >= from {
		return &a.AccountRecords[n-1]
	}
	return nil
}

func decodeAccount(c *types.AccountRecord) (account.Account, error) {
	ser := account.NewAccountSerializer()
	if err := rlp.DecodeBytes(c.Account, ser); err != nil {
		return nil, err
	}
	return ser.GetAccount(), nil
}

// field returns where a balance, nonce or code hash of addr at the end of
// block number is read from: the changes of the block whose last change of the
// field, which last reports, sets it, or else the account that holds it, nil
// if there is none.
func (s *BALHistory) field(addr common.Address, number uint64, kind byte, last func(*types.AccountChanges) (uint32, bool)) (*types.AccountChanges, account.Account, error) {
	a, err := s.latest(addr, number, database.BALIndexKey(kind, addr), database.BALIndexKey(database.BALIndexAccount, addr), database.BALIndexKey(database.BALIndexReset, addr))
	if err != nil {
		return nil, nil, err
	}
	if a == nil {
		base, err := s.baseOf()
		if err != nil {
			return nil, nil, err
		}
		acc, err := base.Account(addr)
		return nil, acc, err
	}
	from, _, deleted := a.Reset()
	if deleted {
		return nil, nil, nil
	}
	w := whole(a, from)
	if idx, ok := last(a); ok && idx >= from && (w == nil || idx >= w.BlockAccessIndex) {
		return a, nil, nil
	}
	if w == nil {
		return nil, nil, fmt.Errorf("%w: account %s", errHistoryInconsistent, addr.Hex())
	}
	acc, err := decodeAccount(w)
	return nil, acc, err
}

// lastOf returns the index of the last change of list.
func lastOf[T any](list []T, index func(T) uint32) (uint32, bool) {
	if len(list) == 0 {
		return 0, false
	}
	return index(list[len(list)-1]), true
}

// Balance returns the balance of addr at the end of the given block.
func (s *BALHistory) Balance(addr common.Address, number uint64) (*big.Int, error) {
	a, acc, err := s.field(addr, number, database.BALIndexBalance, func(a *types.AccountChanges) (uint32, bool) {
		return lastOf(a.BalanceChanges, balanceIndex)
	})
	switch {
	case err != nil:
		return new(big.Int), err
	case a != nil:
		return new(big.Int).Set(a.BalanceChanges[len(a.BalanceChanges)-1].PostBalance), nil
	case acc != nil:
		return new(big.Int).Set(acc.GetBalance()), nil
	}
	return new(big.Int), nil
}

// Nonce returns the nonce of addr at the end of the given block.
func (s *BALHistory) Nonce(addr common.Address, number uint64) (uint64, error) {
	a, acc, err := s.field(addr, number, database.BALIndexNonce, func(a *types.AccountChanges) (uint32, bool) {
		return lastOf(a.NonceChanges, nonceIndex)
	})
	switch {
	case err != nil:
		return 0, err
	case a != nil:
		return a.NonceChanges[len(a.NonceChanges)-1].NewNonce, nil
	case acc != nil:
		return acc.GetNonce(), nil
	}
	return 0, nil
}

// Code returns the code of addr at the end of the given block.
func (s *BALHistory) Code(addr common.Address, number uint64) ([]byte, error) {
	a, acc, err := s.field(addr, number, database.BALIndexCode, func(a *types.AccountChanges) (uint32, bool) {
		return lastOf(a.CodeChanges, codeIndex)
	})
	if err != nil {
		return nil, err
	}
	if a != nil {
		return a.CodeChanges[len(a.CodeChanges)-1].NewCode, nil
	}
	hash := types.EmptyCodeHash
	if pa := account.GetProgramAccount(acc); pa != nil {
		hash = common.BytesToHash(pa.GetCodeHash())
	}
	return s.codeByHash(addr, number, hash)
}

// codeByHash finds the code with the given hash: in the last code change of
// addr, which a later whole account record leaves in place, or in the base.
func (s *BALHistory) codeByHash(addr common.Address, number uint64, hash common.Hash) ([]byte, error) {
	if hash == types.EmptyCodeHash || hash == (common.Hash{}) {
		return nil, nil
	}
	a, err := s.latest(addr, number, database.BALIndexKey(database.BALIndexCode, addr))
	if err != nil {
		return nil, err
	}
	if a != nil {
		if code := a.CodeChanges[len(a.CodeChanges)-1].NewCode; crypto.Keccak256Hash(code) == hash {
			return code, nil
		}
	}
	base, err := s.baseOf()
	if err != nil {
		return nil, err
	}
	return base.Code(hash)
}

// Storage returns the value of a storage slot of addr at the end of the given
// block.
func (s *BALHistory) Storage(addr common.Address, slot common.Hash, number uint64) (common.Hash, error) {
	a, err := s.latest(addr, number, database.BALIndexSlotKey(addr, slot), database.BALIndexKey(database.BALIndexReset, addr))
	if err != nil {
		return common.Hash{}, err
	}
	if a == nil {
		base, err := s.baseOf()
		if err != nil {
			return common.Hash{}, err
		}
		return base.Storage(addr, slot)
	}
	from, cleared, deleted := a.Reset()
	if deleted {
		return common.Hash{}, nil
	}
	i := sort.Search(len(a.StorageChanges), func(i int) bool {
		return bytes.Compare(a.StorageChanges[i].Slot[:], slot[:]) >= 0
	})
	if i < len(a.StorageChanges) && a.StorageChanges[i].Slot == slot {
		if c := a.StorageChanges[i].Changes; c[len(c)-1].BlockAccessIndex >= from {
			return c[len(c)-1].NewValue, nil
		}
	}
	if cleared {
		return common.Hash{}, nil
	}
	return common.Hash{}, fmt.Errorf("%w: account %s slot %x", errHistoryInconsistent, addr.Hex(), slot)
}

// Account returns the account at the end of the given block, or nil if it does
// not exist. Its storage root is not tracked and must not be used.
func (s *BALHistory) Account(addr common.Address, number uint64) (account.Account, error) {
	a, err := s.latest(addr, number, database.BALIndexKey(database.BALIndexAccount, addr), database.BALIndexKey(database.BALIndexReset, addr))
	if err != nil {
		return nil, err
	}
	var acc account.Account
	if a == nil {
		base, err := s.baseOf()
		if err != nil {
			return nil, err
		}
		if acc, err = base.Account(addr); err != nil {
			return nil, err
		}
	} else {
		from, _, deleted := a.Reset()
		if deleted {
			return nil, nil
		}
		w := whole(a, from)
		if w == nil {
			return nil, fmt.Errorf("%w: account %s", errHistoryInconsistent, addr.Hex())
		}
		if acc, err = decodeAccount(w); err != nil {
			return nil, err
		}
	}
	if acc == nil {
		return nil, nil
	}
	balance, err := s.Balance(addr, number)
	if err != nil {
		return nil, err
	}
	nonce, err := s.Nonce(addr, number)
	if err != nil {
		return nil, err
	}
	acc.SetBalance(balance)
	acc.SetNonce(nonce)
	if pa := account.GetProgramAccount(acc); pa != nil {
		code, err := s.Code(addr, number)
		if err != nil {
			return nil, err
		}
		pa.SetCodeHash(crypto.Keccak256(code))
	}
	return acc, nil
}

// balStorageRoot is set on the contract accounts of a state without a state
// trie: their storage is read by address from the history and has no root.
var balStorageRoot = common.HexToHash("0xbabababababababababababababababababababababababababababababababa").ExtendZero()

// historyReader reads the state at the end of one block from a BALHistory, as
// the reader of a read-only state.StateDB.
type historyReader struct {
	history *BALHistory
	number  uint64
	valid   func() bool // reports whether the history is still usable
}

func (r *historyReader) check() error {
	if r.valid != nil && !r.valid() {
		return ErrRebuilt
	}
	return nil
}

func (r *historyReader) Account(addr common.Address) (account.Account, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	acc, err := r.history.Account(addr, r.number)
	if err != nil || acc == nil {
		return nil, err
	}
	acc = acc.DeepCopy()
	if pa := account.GetProgramAccount(acc); pa != nil {
		pa.SetStorageRoot(balStorageRoot)
	}
	return acc, nil
}

func (r *historyReader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	if err := r.check(); err != nil {
		return common.Hash{}, err
	}
	return r.history.Storage(addr, slot, r.number)
}

// Slots lists the slots of addr that may be set at the block, see
// BALHistory.Slots.
func (r *historyReader) Slots(addr common.Address) ([]common.Hash, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	return r.history.Slots(addr, r.number)
}

func (r *historyReader) CodeSize(addr common.Address, codeHash common.Hash) (int, error) {
	code, err := r.Code(addr, codeHash)
	return len(code), err
}

func (r *historyReader) Code(addr common.Address, codeHash common.Hash) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	if codeHash == types.EmptyCodeHash {
		return nil, nil
	}
	code, err := r.history.Code(addr, r.number)
	if err != nil {
		return nil, err
	}
	if crypto.Keccak256Hash(code) != codeHash {
		return nil, errors.New("code not loaded from the BAL history: " + codeHash.Hex())
	}
	return code, nil
}

// OpenBALState opens the state of the block of header from the BAL history
// of a, read-only on db: a value is the one set by the last list at or before
// the block that changed it, and the rest is read from the state the history
// starts from. It returns ErrNotCovered if the block is not a canonical
// indexed one.
func OpenBALState(a *BALHistory, header *types.Header, db Database) (*StateDB, error) {
	if header == nil {
		return nil, ErrNotCovered
	}
	ix, valid, err := a.Index()
	if err != nil {
		return nil, err
	}
	number, meta := header.Number.Uint64(), ix.Meta()
	if number < meta.Base || number >= meta.Next || a.db.ReadCanonicalHash(number) != header.Hash() {
		return nil, ErrNotCovered
	}
	return NewReadOnly(header.Root, db, &historyReader{history: a, number: number, valid: valid}), nil
}

var (
	_ Reader     = (*historyReader)(nil)
	_ SlotLister = (*historyReader)(nil)
)

// BALBase serves a state as the base of a BALHistory.
type BALBase struct {
	mu sync.Mutex // serialises the reads of the state, which copies of a state share
	s  *StateDB
}

func NewBALBase(s *StateDB) *BALBase {
	return &BALBase{s: s}
}

func (b *BALBase) Account(addr common.Address) (account.Account, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	acc := b.s.GetAccount(addr)
	if acc == nil {
		return nil, b.s.Error()
	}
	return acc.DeepCopy(), b.s.Error()
}

func (b *BALBase) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.s.GetState(addr, slot), b.s.Error()
}

func (b *BALBase) Code(codeHash common.Hash) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.s.Database().ContractCode(codeHash)
}

// Slots returns the slots of the storage of addr.
func (b *BALBase) Slots(addr common.Address) ([]common.Hash, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var slots []common.Hash
	b.s.ForEachStorage(addr, func(key, _ common.Hash) bool {
		slots = append(slots, key)
		return true
	})
	return slots, b.s.Error()
}

// The pre-Amber BAL hash chain lets a node check block access lists of blocks
// whose headers do not commit one, like lists received from another node, see
// hashchain. The hash of block i is Hash() of its list; block 0, the genesis
// block, is not executed and has the empty list.

// HashChainPackage is the npm package of the BAL hash chains.
const HashChainPackage = "@kaiachain/bal-hashchain"

// BALHashChain is the kind of the BAL hash chains.
var BALHashChain = hashchain.Spec{
	Name:    "BAL",
	Magic:   "KAIABALH",
	Domain:  "kaia-bal-hashchain-v1",
	Package: HashChainPackage,
}

// HashChainPublicKey is the ed25519 key the BAL hash chain files are verified
// with.
//
// PLACEHOLDER: the Kaia Foundation signing key replaces this all-zero key at
// release. No chain verifies with the placeholder.
var HashChainPublicKey = ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))

// EmptyListHash is the hash of the empty list, the one of the genesis block.
var EmptyListHash = func() common.Hash {
	h, err := (&types.BlockAccessList{}).Hash()
	if err != nil {
		panic(err)
	}
	return h
}()

// HashOf returns the hash of a canonical block of db, the keccak256 of its
// stored writes, or EmptyListHash for the genesis block.
func HashOf(db database.DBManager, number uint64) (common.Hash, error) {
	if number == 0 {
		return EmptyListHash, nil
	}
	enc, err := db.ReadCanonicalBAL(number)
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash(enc), nil
}

// VerifyHashChainBlock checks the list of a block against the chain with a
// proof from hashchain.Proof.
func VerifyHashChainBlock(c *hashchain.Chain, number uint64, list *types.BlockAccessList, proof []common.Hash) error {
	h, err := list.Hash()
	if err != nil {
		return err
	}
	return c.VerifyBlock(number, h, proof)
}
