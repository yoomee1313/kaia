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
	"math/big"

	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
)

var (
	errOverlayMissingAccount = errors.New("recreated or new account has no whole account record")
	errOverlayNoAccount      = errors.New("changed account does not exist and has no whole account record")
)

// AccessOverlay gives a state opened at the parent root of a block the values
// of the phases of the block access list of the block before a phase, so that
// the state is the one at the start of the phase without the list being
// applied up front. Package blockchain/bal/access makes one from a list.
type AccessOverlay interface {
	// Account returns the changes of the account made before the phase, nil
	// if there are none.
	Account(addr common.Address) *OverlayAccount
}

// OverlayAccount is the state of an account at the start of a phase relative
// to the parent state, as applying the changes of the earlier phases and
// finalising them would leave it.
type OverlayAccount struct {
	// Deleted is set if the last of the changes deleted the account.
	Deleted bool
	// Cleared is set if the storage of the parent is gone: the account was
	// deleted or recreated and then changed again.
	Cleared bool
	// Record is the whole account, encoded by account.AccountSerializer, if
	// one was recorded after the last deletion or recreation. It is required
	// if Cleared is set.
	Record []byte
	// Balance, Nonce and Code, if set, are the last values of these fields.
	Balance *big.Int
	Nonce   *uint64
	Code    []byte
	HasCode bool
	// Slot, if set, returns the last value written to a slot, and false if
	// the slot keeps its value of the parent, or none after Cleared.
	Slot func(slot common.Hash) (common.Hash, bool)
}

// SetAccessOverlay makes the state give the accounts it loads the values of
// o. The values are not written to the tries, so the state root of such a
// state is wrong: it is only for a phase that executes and records its
// accesses. It must be called before the state loads any account. A list
// that cannot be applied is reported by Error.
func (s *StateDB) SetAccessOverlay(o AccessOverlay) {
	s.overlay = o
}

// overlayAccount gives the values of the overlay to an account just loaded
// from the parent state, obj, nil if it did not exist. Its slots get theirs
// when they are read, see stateObject.GetCommittedState.
func (s *StateDB) overlayAccount(addr common.Address, obj *stateObject) *stateObject {
	o := s.overlay.Account(addr)
	if o == nil {
		return obj
	}
	if o.Deleted {
		if s.snap != nil {
			s.snapDestructs[crypto.Keccak256Hash(addr[:])] = struct{}{}
		}
		if obj != nil {
			obj.deleted = true
		}
		return obj
	}
	if o.Cleared && s.snap != nil {
		// The storage of the parent is gone.
		s.snapDestructs[crypto.Keccak256Hash(addr[:])] = struct{}{}
	}

	if o.Record != nil {
		ser := account.NewAccountSerializer()
		if err := rlp.DecodeBytes(o.Record, ser); err != nil {
			s.setError(err)
			return obj
		}
		acc := ser.GetAccount()
		if pa := account.GetProgramAccount(acc); pa != nil {
			var root common.ExtHash
			if obj != nil && !o.Cleared {
				if prev := account.GetProgramAccount(obj.account); prev != nil {
					root = prev.GetStorageRoot()
				}
			}
			pa.SetStorageRoot(root)
		}
		replaced := newObject(s, addr, acc)
		if obj != nil && !o.Cleared && bytes.Equal(obj.CodeHash(), replaced.CodeHash()) {
			replaced.code = obj.code
		}
		obj = replaced
		s.setStateObject(obj)
	} else if o.Cleared {
		s.setError(errOverlayMissingAccount)
		return obj
	}
	if obj == nil {
		s.setError(errOverlayNoAccount)
		return nil
	}

	if o.Balance != nil {
		obj.setBalance(new(big.Int).Set(o.Balance))
	}
	if o.Nonce != nil {
		obj.setNonce(*o.Nonce)
	}
	if o.HasCode {
		if err := obj.setCode(crypto.Keccak256Hash(o.Code), o.Code); err != nil {
			s.setError(err)
		}
	}
	obj.overlay = o.Slot
	return obj
}
