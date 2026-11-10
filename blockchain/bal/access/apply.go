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

package access

import (
	"errors"
	"fmt"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
)

var (
	errBALMissingAccount = errors.New("recreated or new account has no whole account record")
	errBALNoAccount      = errors.New("changed account does not exist and has no whole account record")
	errBALStorageRoot    = errors.New("storage root mismatch")
)

// Apply writes the final values of a block access list to the state, which
// must be opened at the parent root of the block, and checks the storage
// roots of the list. The caller then checks that IntermediateRoot(true)
// equals the state root of the header.
func Apply(s *state.StateDB, list *BlockAccessList) error {
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if err := applyAccountAccess(s, a); err != nil {
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

// ApplyUnchecked writes the final values of a block access list like Apply
// and finalises the block without hashing, so that several blocks can be
// applied in a row and checked once at the end: call HashStorages, then
// IntermediateRoot. The storage roots of the list are not checked.
func ApplyUnchecked(s *state.StateDB, list *BlockAccessList) error {
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if err := applyAccountAccess(s, a); err != nil {
			return fmt.Errorf("account %s: %w", a.Address.Hex(), err)
		}
	}
	s.Finalise(true, false)
	return nil
}

func applyAccountAccess(s *state.StateDB, a *AccountAccess) error {
	if !a.Changed() {
		return nil
	}
	addr := a.Address

	// Changes before from are superseded by a deletion or a recreation.
	from, cleared, deleted := a.Reset()
	if deleted {
		if s.Exist(addr) {
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
	if !s.Exist(addr) {
		return errBALNoAccount
	}

	// Every change of a field is recorded, so the last one in range is the
	// final value, consistent with the whole account record if there is one.
	if n := len(a.BalanceChanges); n > 0 && a.BalanceChanges[n-1].Index >= from {
		s.SetBalance(addr, a.BalanceChanges[n-1].Balance)
	}
	if n := len(a.NonceChanges); n > 0 && a.NonceChanges[n-1].Index >= from {
		s.SetNonce(addr, a.NonceChanges[n-1].Nonce)
	}
	if n := len(a.CodeChanges); n > 0 && a.CodeChanges[n-1].Index >= from {
		if err := s.ApplyCode(addr, a.CodeChanges[n-1].Code); err != nil {
			return err
		}
	}
	for _, w := range a.StorageWrites {
		if last := w.Changes[len(w.Changes)-1]; last.Index >= from {
			s.SetState(addr, w.Slot, last.Value)
		}
	}
	return nil
}

// StateBase serves a state as the base of a BAL index, see History.
type StateBase struct {
	s *state.StateDB
}

func NewStateBase(s *state.StateDB) *StateBase {
	return &StateBase{s: s}
}

func (b *StateBase) Account(addr common.Address) (account.Account, error) {
	acc := b.s.GetAccount(addr)
	if acc == nil {
		return nil, b.s.Error()
	}
	return acc.DeepCopy(), b.s.Error()
}

func (b *StateBase) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	return b.s.GetState(addr, slot), b.s.Error()
}

func (b *StateBase) Code(codeHash common.Hash) ([]byte, error) {
	return b.s.Database().ContractCode(codeHash)
}

// Slots returns the slots of the storage of addr.
func (b *StateBase) Slots(addr common.Address) ([]common.Hash, error) {
	var slots []common.Hash
	b.s.ForEachStorage(addr, func(key, _ common.Hash) bool {
		slots = append(slots, key)
		return true
	})
	return slots, b.s.Error()
}
