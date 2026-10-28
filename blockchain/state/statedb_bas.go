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
	"math/big"

	"github.com/kaiachain/kaia/blockchain/bas"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
)

var (
	errBASMissingAccount = errors.New("recreated or new account has no whole account record")
	errBASNoAccount      = errors.New("changed account does not exist and has no whole account record")
)

// accessRecorder records the block access set while a block is executed. It
// only observes the StateDB and never changes state or gas.
//
// Reads are recorded when an account is loaded and when a slot is read through
// GetState or GetCommittedState. Writes are recorded as post values whenever
// the journal is flushed: at every Finalise, which marks the end of a tx, and
// when the phase index changes, so that changes of InitializeState that were
// not finalised are not attributed to the first tx.
type accessRecorder struct {
	index   uint32
	builder *bas.Builder

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
		logger.Error("Failed to encode account fields for the block access set", "err", err)
	}
	return enc
}

// EnableBlockAccessRecorder starts recording the block access set. It must be
// called on a StateDB freshly opened at the parent root, before the block is
// executed. The phase index starts at 0 (InitializeState).
func (s *StateDB) EnableBlockAccessRecorder() {
	s.blockAccess = &accessRecorder{
		builder: bas.NewBuilder(),
		seen:    make(map[common.Address]*accessView),
	}
}

// SetBlockAccessIndex switches the phase index of the block access recorder:
// 0 for InitializeState, i+1 for the i-th tx, and len(txs)+1 for
// FinalizeState. Changes not yet finalised are recorded under the old index.
// It is a no-op if the recorder is not enabled.
func (s *StateDB) SetBlockAccessIndex(idx int) {
	r := s.blockAccess
	if r == nil || r.index == uint32(idx) {
		return
	}
	r.capture(s, false, false)
	r.index = uint32(idx)
}

// BlockAccessSet returns the recorded block access set, or nil if the recorder
// is not enabled. Changes are recorded at Finalise, so it is complete after
// IntermediateRoot of the block.
func (s *StateDB) BlockAccessSet() *bas.BlockAccessSet {
	if s.blockAccess == nil {
		return nil
	}
	return s.blockAccess.builder.Build()
}

func (r *accessRecorder) accountLoaded(addr common.Address, obj *stateObject) {
	if _, ok := r.seen[addr]; ok {
		return
	}
	r.builder.ReadAccount(addr)
	r.seen[addr] = newAccessView(obj)
}

func (r *accessRecorder) slotRead(addr common.Address, slot common.Hash) {
	r.builder.ReadSlot(addr, slot)
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
		// Finalise clears the journal.
		r.journalOffset = 0
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
			logger.Error("Failed to encode account for the block access set", "addr", addr, "err", err)
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
// is set. It is used to apply a block access set.
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

// ApplyBlockAccessSet writes the final values of a block access set to the
// state, which must be opened at the parent root of the block. The caller then
// checks that IntermediateRoot(true) equals the state root of the header.
func (s *StateDB) ApplyBlockAccessSet(set *bas.BlockAccessSet) error {
	for i := range set.Accounts {
		a := &set.Accounts[i]
		if err := s.applyAccountAccess(a); err != nil {
			return fmt.Errorf("account %s: %w", a.Address.Hex(), err)
		}
	}
	return nil
}

func (s *StateDB) applyAccountAccess(a *bas.AccountAccess) error {
	if !a.Changed() {
		return nil
	}
	addr := a.Address

	// Changes before from are superseded by a deletion or a recreation.
	var (
		from    uint32
		cleared bool
	)
	if n := len(a.Deleted); n > 0 {
		// Changes with the same index as a deletion happened after it.
		if last, ok := a.LastWriteIndex(); !ok || last < a.Deleted[n-1] {
			if s.getStateObject(addr) != nil {
				s.SelfDestruct(addr)
			}
			return nil
		}
		from, cleared = a.Deleted[n-1], true
	}
	if n := len(a.StorageCleared); n > 0 && a.StorageCleared[n-1] >= from {
		from, cleared = a.StorageCleared[n-1], true
	}

	if n := len(a.AccountChanges); n > 0 && a.AccountChanges[n-1].Index >= from {
		ser := account.NewAccountSerializer()
		if err := rlp.DecodeBytes(a.AccountChanges[n-1].Account, ser); err != nil {
			return err
		}
		s.SetAccount(addr, ser.GetAccount(), cleared)
	} else if cleared {
		return errBASMissingAccount
	}
	obj := s.getStateObject(addr)
	if obj == nil {
		return errBASNoAccount
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
		if err := obj.SetCode(crypto.Keccak256Hash(code), code); err != nil {
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
