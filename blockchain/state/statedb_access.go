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

	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
)

var errNoAccessAccount = errors.New("no account to apply code to")

// AccessRecorder observes the accesses and the changes of a StateDB while a
// block is executed, see SetAccessRecorder. It never changes state.
type AccessRecorder interface {
	// AccountLoaded is called when an account is loaded, with nil if there is
	// none, and AccountRead when a loaded one is accessed again.
	AccountLoaded(addr common.Address, acc account.Account)
	AccountRead(addr common.Address)
	// SlotRead is called when a slot is read through GetState or
	// GetCommittedState.
	SlotRead(addr common.Address, slot common.Hash)
	// SetIndex starts a phase of the block, see SetBlockAccessIndex.
	SetIndex(s *StateDB, idx int)
	// EndInitialization ends the phase of InitializeState.
	EndInitialization(s *StateDB)
	// Finalise is called at the start of Finalise.
	Finalise(s *StateDB, deleteEmptyObjects bool)
	// RevertedTx is called when the state is reverted to the start of the
	// journal, dropping a whole tx.
	RevertedTx()
	// Copy returns a copy for a copy of the StateDB.
	Copy() AccessRecorder
}

// SetAccessRecorder sets the recorder of the accesses, or none with nil.
func (s *StateDB) SetAccessRecorder(r AccessRecorder) {
	s.access = r
}

// AccessRecorder returns the recorder of the accesses, nil if none.
func (s *StateDB) AccessRecorder() AccessRecorder {
	return s.access
}

// SetBlockAccessIndex starts a phase of the access recorder: 0 for
// InitializeState, i+1 for the i-th tx, and len(txs)+1 for FinalizeState. It
// is a no-op if no recorder is set.
func (s *StateDB) SetBlockAccessIndex(idx int) {
	if s.access != nil {
		s.access.SetIndex(s, idx)
	}
}

// EndBlockAccessInitialization ends the phase of InitializeState, which may
// not be finalised. It is a no-op if no recorder is set.
func (s *StateDB) EndBlockAccessInitialization() {
	if s.access != nil {
		s.access.EndInitialization(s)
	}
}

func (s *StateDB) accessLoaded(addr common.Address, obj *stateObject) {
	var acc account.Account
	if obj != nil {
		acc = obj.account
	}
	s.access.AccountLoaded(addr, acc)
}

// Modified reports whether the state has changes since it was opened.
func (s *StateDB) Modified() bool {
	return len(s.journal.entries) > 0 || len(s.stateObjectsDirty) > 0
}

// LoadedAccounts calls fn with every loaded account.
func (s *StateDB) LoadedAccounts(fn func(addr common.Address, acc account.Account, deleted bool)) {
	for addr, obj := range s.stateObjects {
		fn(addr, obj.account, obj.deleted)
	}
}

// AccessChange is an account changed by the journal entries an access
// recorder flushes, see AccessChanges.
type AccessChange struct {
	Address common.Address
	// Deleted tells that Finalise is about to delete the account, and
	// Recreated that it was created again over an existing one.
	Deleted, Recreated bool
	// Slots maps the written slots to their values before the first write.
	Slots map[common.Hash]common.Hash

	s   *StateDB
	obj *stateObject
}

// Account returns the account, which must not be changed.
func (c *AccessChange) Account() account.Account { return c.obj.account }

func (c *AccessChange) Code() []byte { return c.obj.Code(c.s.db) }

func (c *AccessChange) State(slot common.Hash) common.Hash { return c.obj.GetState(c.s.db, slot) }

// AccessChanges returns the accounts changed by the journal entries from
// offset on, the same set Finalise writes to the trie, and the offset after
// them. With final it is called at Finalise, and reports the deletions that
// Finalise is about to apply.
func (s *StateDB) AccessChanges(offset int, final, deleteEmptyObjects bool) ([]AccessChange, int) {
	var entries []journalEntry
	if offset < len(s.journal.entries) {
		entries = s.journal.entries[offset:]
	}
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
	changes := make([]AccessChange, 0, len(s.journal.dirties))
	for addr := range s.journal.dirties {
		obj, exist := s.stateObjects[addr]
		if !exist {
			continue
		}
		changes = append(changes, AccessChange{
			Address:   addr,
			Deleted:   final && (obj.selfDestructed || (deleteEmptyObjects && obj.empty())),
			Recreated: recreated[addr],
			Slots:     written[addr],
			s:         s,
			obj:       obj,
		})
	}
	return changes, len(s.journal.entries)
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

// ApplyCode sets the code of the account at addr from a block access list.
// Unlike SetCode it does not load the previous code: a whole account record
// applied just before may already carry the new code hash, and that code is
// only in the list.
func (s *StateDB) ApplyCode(addr common.Address, code []byte) error {
	obj := s.getStateObject(addr)
	if obj == nil {
		return errNoAccessAccount
	}
	s.journal.append(codeChange{
		account:  &obj.address,
		prevhash: obj.CodeHash(),
		prevcode: obj.code,
	})
	return obj.setCode(crypto.Keccak256Hash(code), code)
}
