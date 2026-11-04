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
	"errors"
	"fmt"
	"maps"
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
)

var (
	errBALMissingAccount = errors.New("recreated or new account has no whole account record")
	errBALNoAccount      = errors.New("changed account does not exist and has no whole account record")
	errBALStorageRoot    = errors.New("storage root mismatch")
)

// accessRecorder records the block access list while a block is executed. It
// only observes the StateDB and never changes state or gas.
//
// Reads are recorded when an account is accessed and when a slot is read
// through GetState or GetCommittedState. Writes are recorded as post values
// whenever the journal is flushed: at every Finalise, which marks the end of a
// tx, and when the phase index changes, so that changes of InitializeState that
// were not finalised are not attributed to the first tx.
//
// Reads are held as pending until the phase they belong to is kept: they are
// committed at Finalise and at the end of InitializeState, and dropped when a
// tx starts and when a whole tx is reverted. So a block producer that reads
// state between txs, or runs a tx it then drops, records the same reads as a
// validator that runs only the included txs.
type accessRecorder struct {
	index   uint32
	builder *bal.Builder

	pendingAccounts map[common.Address]struct{}
	pendingSlots    map[common.Address]map[common.Hash]struct{}

	// seen holds, per account, the values as of the last recorded change, or
	// as loaded from the parent state. A change is recorded only if the value
	// differs from it.
	seen map[common.Address]*accessView

	// journalOffset is where the next flush starts reading journal entries.
	journalOffset int
}

type accessView struct {
	exists   bool
	balance  *big.Int
	nonce    uint64
	codeHash common.Hash
	fields   []byte // Kaia-specific fields, see kaiaFields
}

func newAccessView(obj *stateObject) *accessView {
	if obj == nil {
		return &accessView{balance: new(big.Int), codeHash: common.BytesToHash(emptyCodeHash)}
	}
	return &accessView{
		exists:   true,
		balance:  new(big.Int).Set(obj.Balance()),
		nonce:    obj.Nonce(),
		codeHash: common.BytesToHash(obj.CodeHash()),
		fields:   kaiaFields(obj.account),
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
		pa.SetCodeHash(emptyCodeHash)
	}
	enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(c))
	if err != nil {
		logger.Error("Failed to encode account fields for the block access list", "err", err)
	}
	return enc
}

// EnableBlockAccessRecorder starts recording the block access list. It must be
// called on a StateDB opened at the parent root, before the block is executed.
// Accounts already loaded hold parent values and are taken as the base of their
// changes. The phase index starts at 0 (InitializeState).
func (s *StateDB) EnableBlockAccessRecorder() {
	if len(s.journal.entries) > 0 || len(s.stateObjectsDirty) > 0 {
		logger.Error("Block access recorder enabled on a modified state, the list will be wrong")
	}
	r := &accessRecorder{
		builder:         bal.NewBuilder(),
		seen:            make(map[common.Address]*accessView, len(s.stateObjects)),
		pendingAccounts: make(map[common.Address]struct{}),
		pendingSlots:    make(map[common.Address]map[common.Hash]struct{}),
	}
	for addr, obj := range s.stateObjects {
		r.seen[addr] = newAccessView(obj)
	}
	s.blockAccess = r
}

// copy returns a deep copy of the recorder for a copy of the StateDB, which
// starts with an empty journal. Views in seen are replaced, never modified, so
// they are shared.
func (r *accessRecorder) copy() *accessRecorder {
	slots := make(map[common.Address]map[common.Hash]struct{}, len(r.pendingSlots))
	for addr, m := range r.pendingSlots {
		slots[addr] = maps.Clone(m)
	}
	return &accessRecorder{
		index:           r.index,
		builder:         r.builder.Copy(),
		seen:            maps.Clone(r.seen),
		pendingAccounts: maps.Clone(r.pendingAccounts),
		pendingSlots:    slots,
	}
}

// SetBlockAccessIndex starts a phase of the block access recorder: 0 for
// InitializeState, i+1 for the i-th tx, and len(txs)+1 for FinalizeState.
// Changes not yet finalised are recorded under the old index. Pending reads are
// dropped: they were made after the last Finalise, outside of any tx, as a
// block producer does. It is a no-op if the recorder is not enabled.
func (s *StateDB) SetBlockAccessIndex(idx int) {
	r := s.blockAccess
	if r == nil {
		return
	}
	r.dropReads()
	if r.index == uint32(idx) {
		return
	}
	r.capture(s, false, false)
	r.index = uint32(idx)
}

// EndBlockAccessInitialization keeps the changes and reads of InitializeState,
// which may not be finalised. It is a no-op if the recorder is not enabled.
func (s *StateDB) EndBlockAccessInitialization() {
	if r := s.blockAccess; r != nil {
		r.capture(s, false, false)
		r.commitReads()
	}
}

// BlockAccessList returns the recorded block access list, or nil if the recorder
// is not enabled. Changes are recorded at Finalise, so it is complete after
// IntermediateRoot of the block. Storage roots are not recorded.
func (s *StateDB) BlockAccessList() *bal.BlockAccessList {
	if s.blockAccess == nil {
		return nil
	}
	return s.blockAccess.builder.Build()
}

// accountLoaded takes the values of a newly loaded account as the base of its
// changes, and records the read.
func (r *accessRecorder) accountLoaded(addr common.Address, obj *stateObject) {
	if _, ok := r.seen[addr]; !ok {
		r.seen[addr] = newAccessView(obj)
	}
	r.accountRead(addr)
}

// accountRead records a read of an account, loaded or not. It is recorded on
// every access, not on load only: a tx that is dropped may be the one that
// loaded the account.
func (r *accessRecorder) accountRead(addr common.Address) {
	r.pendingAccounts[addr] = struct{}{}
}

func (r *accessRecorder) slotRead(addr common.Address, slot common.Hash) {
	m := r.pendingSlots[addr]
	if m == nil {
		m = make(map[common.Hash]struct{})
		r.pendingSlots[addr] = m
	}
	m[slot] = struct{}{}
}

func (r *accessRecorder) commitReads() {
	for addr := range r.pendingAccounts {
		r.builder.ReadAccount(addr)
	}
	for addr, slots := range r.pendingSlots {
		for slot := range slots {
			r.builder.ReadSlot(addr, slot)
		}
	}
	r.dropReads()
}

func (r *accessRecorder) dropReads() {
	if len(r.pendingAccounts) > 0 {
		r.pendingAccounts = make(map[common.Address]struct{})
	}
	if len(r.pendingSlots) > 0 {
		r.pendingSlots = make(map[common.Address]map[common.Hash]struct{})
	}
}

// capture records the post values of the accounts changed since the last
// flush. If final is set, it runs at Finalise and records the deletions that
// Finalise is about to apply.
func (r *accessRecorder) capture(s *StateDB, final, deleteEmptyObjects bool) {
	var entries []journalEntry
	if r.journalOffset < len(s.journal.entries) {
		entries = s.journal.entries[r.journalOffset:]
	}
	r.journalOffset = len(s.journal.entries)
	if final {
		// Finalise clears the journal and ends the tx, which keeps its reads.
		r.journalOffset = 0
		r.commitReads()
	}

	// Collect the written slots with their values before the first write, and
	// the accounts recreated, which drops their storage.
	written := make(map[common.Address]map[common.Hash]common.Hash)
	recreated := make(map[common.Address]bool)
	for _, entry := range entries {
		switch e := entry.(type) {
		case storageChange:
			slots := written[*e.account]
			if slots == nil {
				slots = make(map[common.Hash]common.Hash)
				written[*e.account] = slots
			}
			if _, ok := slots[e.key]; !ok {
				slots[e.key] = e.prevalue
			}
		case resetObjectChange:
			recreated[e.prev.address] = true
		}
	}

	// Same set of accounts as Finalise writes to the trie.
	for addr := range s.journal.dirties {
		obj, exist := s.stateObjects[addr]
		if !exist {
			continue
		}
		deleted := final && (obj.selfDestructed || (deleteEmptyObjects && obj.empty()))
		r.captureAccount(s, addr, obj, deleted, recreated[addr], written[addr])
	}
}

func (r *accessRecorder) captureAccount(s *StateDB, addr common.Address, obj *stateObject, deleted, recreated bool, slots map[common.Hash]common.Hash) {
	idx := r.index
	// prev is nil if the account was loaded before the recorder was enabled.
	// Then everything is recorded.
	prev := r.seen[addr]
	if deleted {
		if prev == nil || prev.exists {
			r.builder.Delete(addr, idx)
		}
		r.seen[addr] = newAccessView(nil)
		return
	}

	cur := newAccessView(obj)
	if recreated {
		r.builder.ClearStorage(addr, idx)
	}
	if prev == nil || !prev.exists || recreated || !bytes.Equal(prev.fields, cur.fields) {
		enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(obj.account))
		if err != nil {
			logger.Error("Failed to encode account for the block access list", "addr", addr, "err", err)
		}
		r.builder.SetAccount(addr, idx, enc)
	}
	if prev == nil || prev.balance.Cmp(cur.balance) != 0 {
		r.builder.SetBalance(addr, idx, cur.balance)
	}
	if prev == nil || prev.nonce != cur.nonce {
		r.builder.SetNonce(addr, idx, cur.nonce)
	}
	if prev == nil || prev.codeHash != cur.codeHash {
		r.builder.SetCode(addr, idx, obj.Code(s.db))
	}
	for slot, pre := range slots {
		// After a recreation the storage starts empty, so every written slot
		// is recorded even if it went back to the value before the first write.
		if value := obj.GetState(s.db, slot); value != pre || recreated {
			r.builder.WriteSlot(addr, idx, slot, value)
		} else {
			r.builder.ReadSlot(addr, slot)
		}
	}
	r.seen[addr] = cur
}

// SetAccount replaces the whole account at addr with acc. The storage root of
// acc is not trusted: the current storage is kept, or dropped if clearStorage
// is set. It is used to apply a block access list.
func (s *StateDB) SetAccount(addr common.Address, acc account.Account, clearStorage bool) {
	acc = acc.DeepCopy()
	pa := account.GetProgramAccount(acc)
	prev := s.getStateObject(addr)
	if prev == nil || clearStorage {
		if pa != nil {
			pa.SetStorageRoot(common.ExtHash{})
		}
		newobj, _ := s.createObjectWithAccount(addr, acc)
		newobj.touch() // sets the object to dirty
		return
	}

	if pa != nil {
		var root common.ExtHash
		if prevPa := account.GetProgramAccount(prev.account); prevPa != nil {
			root = prevPa.GetStorageRoot()
		}
		pa.SetStorageRoot(root)
	}
	s.journal.append(accountChange{
		account:       &prev.address,
		prev:          prev.account,
		prevcode:      prev.code,
		prevDirtyCode: prev.dirtyCode,
	})
	codeHash := prev.CodeHash()
	prev.account = acc
	if !bytes.Equal(codeHash, prev.CodeHash()) {
		prev.code, prev.dirtyCode = nil, false
	}
}

// applyCode sets code from a block access list. Unlike SetCode it does not
// load the previous code: a whole account record applied just before may
// already carry the new code hash, and that code is only in the list.
func (s *stateObject) applyCode(codeHash common.Hash, code []byte) error {
	s.db.journal.append(codeChange{
		account:  &s.address,
		prevhash: s.CodeHash(),
		prevcode: s.code,
	})
	return s.setCode(codeHash, code)
}

// ApplyBlockAccessList writes the final values of a block access list to the
// state, which must be opened at the parent root of the block, and checks the
// storage roots of the list. The caller then checks that IntermediateRoot(true)
// equals the state root of the header.
func (s *StateDB) ApplyBlockAccessList(list *bal.BlockAccessList) error {
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if err := s.applyAccountAccess(a); err != nil {
			return fmt.Errorf("account %s: %w", a.Address.Hex(), err)
		}
	}
	s.IntermediateRoot(true)
	// Lists recorded with storage roots are checked against them.
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if a.StorageRoot == nil {
			continue
		}
		if root := s.GetStorageRoot(a.Address); root != *a.StorageRoot {
			return fmt.Errorf("account %s: %w (recorded %x, applied %x)", a.Address.Hex(), errBALStorageRoot, *a.StorageRoot, root)
		}
	}
	return nil
}

// ApplyBlockAccessListUnchecked writes the final values of a block access list
// like ApplyBlockAccessList and finalises the block without hashing, so that
// several blocks can be applied in a row and checked once at the end: call
// HashStorages, then IntermediateRoot. The storage roots of the list are not
// checked.
func (s *StateDB) ApplyBlockAccessListUnchecked(list *bal.BlockAccessList) error {
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if err := s.applyAccountAccess(a); err != nil {
			return fmt.Errorf("account %s: %w", a.Address.Hex(), err)
		}
	}
	s.Finalise(true, false)
	return nil
}

// HashStorages computes the storage roots left out by Finalise with
// setStorageRoot unset, with up to workers storage tries hashed at a time, and
// writes the accounts to the account trie.
func (s *StateDB) HashStorages(workers int) {
	objs := make([]*stateObject, 0, len(s.stateObjectsDirtyStorage))
	for addr := range s.stateObjectsDirtyStorage {
		if so := s.getStateObject(addr); so != nil {
			objs = append(objs, so)
		}
	}
	s.stateObjectsDirtyStorage = make(map[common.Address]struct{})

	// Each object hashes its own storage trie, so they do not share state.
	var (
		wg   sync.WaitGroup
		next atomic.Int64
	)
	for range max(1, workers) {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i < len(objs); i = int(next.Add(1) - 1) {
				objs[i].updateStorageRoot(s.db)
			}
		})
	}
	wg.Wait()
	for _, so := range objs {
		s.updateStateObject(so)
	}
}

func (s *StateDB) applyAccountAccess(a *bal.AccountAccess) error {
	if !a.Changed() {
		return nil
	}
	addr := a.Address

	// Changes before from are superseded by a deletion or a recreation.
	from, cleared, deleted := a.Reset()
	if deleted {
		if s.getStateObject(addr) != nil {
			s.SelfDestruct(addr)
		}
		return nil
	}

	if n := len(a.AccountChanges); n > 0 && a.AccountChanges[n-1].Index >= from {
		ser := account.NewAccountSerializer()
		if err := rlp.DecodeBytes(a.AccountChanges[n-1].Account, ser); err != nil {
			return err
		}
		s.SetAccount(addr, ser.GetAccount(), cleared)
	} else if cleared {
		return errBALMissingAccount
	}
	obj := s.getStateObject(addr)
	if obj == nil {
		return errBALNoAccount
	}

	// Every change of a field is recorded, so the last one in range is the
	// final value, consistent with the whole account record if there is one.
	if n := len(a.BalanceChanges); n > 0 && a.BalanceChanges[n-1].Index >= from {
		obj.SetBalance(a.BalanceChanges[n-1].Balance)
	}
	if n := len(a.NonceChanges); n > 0 && a.NonceChanges[n-1].Index >= from {
		obj.SetNonce(a.NonceChanges[n-1].Nonce)
	}
	if n := len(a.CodeChanges); n > 0 && a.CodeChanges[n-1].Index >= from {
		code := a.CodeChanges[n-1].Code
		if err := obj.applyCode(crypto.Keccak256Hash(code), code); err != nil {
			return err
		}
	}
	for _, w := range a.StorageWrites {
		if last := w.Changes[len(w.Changes)-1]; last.Index >= from {
			obj.SetState(s.db, w.Slot, last.Value)
		}
	}
	return nil
}

// BALBase serves a state as the base of a BAL index, see bal.History.
type BALBase struct {
	s *StateDB
}

func NewBALBase(s *StateDB) *BALBase {
	return &BALBase{s: s}
}

func (b *BALBase) Account(addr common.Address) (account.Account, error) {
	acc := b.s.GetAccount(addr)
	if acc == nil {
		return nil, b.s.Error()
	}
	return acc.DeepCopy(), b.s.Error()
}

func (b *BALBase) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	return b.s.GetState(addr, slot), b.s.Error()
}

func (b *BALBase) Code(codeHash common.Hash) ([]byte, error) {
	return b.s.db.ContractCode(codeHash)
}
