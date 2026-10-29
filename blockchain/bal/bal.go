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

// Package bal defines the block access list (BAL): the accounts and storage
// slots a block touched, with the post value of every change, in the style of
// EIP-7928 block-level access lists.
//
// Every change carries the index of the execution phase that made it:
//   - 0 is InitializeState (EIP-2935, system tx calls),
//   - 1..n are the transactions of the block,
//   - n+1 is FinalizeState (rewards).
//
// A BAL is not trusted: applying it to the parent state must reproduce the
// state root of the block header, so a wrong list cannot pass that check.
package bal

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
)

var (
	errUnsortedAccounts = errors.New("accounts are not strictly sorted by address")
	errUnsortedSlots    = errors.New("slots are not strictly sorted")
	errUnsortedIndices  = errors.New("change indices are not strictly increasing")
	errEmptySlotWrites  = errors.New("slot has no write")
)

// BlockAccessList is the access list of one block. Accounts are sorted by address.
type BlockAccessList struct {
	Accounts []AccountAccess
}

// AccountAccess holds what a block did to one account. An account with no
// change at all was only read.
//
// Kaia-specific fields (account type, AccountKey, humanReadable, codeInfo)
// are not listed one by one. Whenever they change, or the account comes into
// existence, the whole account is stored in AccountChanges. Its storage root
// is not trusted: the applier keeps or clears the storage itself.
type AccountAccess struct {
	Address        common.Address
	StorageWrites  []SlotWrites    // sorted by slot
	StorageReads   []common.Hash   // sorted, slots that were read but not changed
	BalanceChanges []BalanceChange // sorted by index
	NonceChanges   []NonceChange   // sorted by index
	CodeChanges    []CodeChange    // sorted by index
	AccountChanges []AccountChange // sorted by index
	Deleted        []uint32        // phases in which the account was deleted
	StorageCleared []uint32        // phases in which the account was recreated, dropping its storage
}

// SlotWrites holds the post values of one storage slot.
type SlotWrites struct {
	Slot    common.Hash
	Changes []SlotChange // sorted by index
}

type SlotChange struct {
	Index uint32
	Value common.Hash
}

type BalanceChange struct {
	Index   uint32
	Balance *big.Int
}

type NonceChange struct {
	Index uint32
	Nonce uint64
}

type CodeChange struct {
	Index uint32
	Code  []byte
}

// AccountChange is the whole account encoded by account.AccountSerializer.
type AccountChange struct {
	Index   uint32
	Account []byte
}

// Encode returns the RLP encoding of the list.
func (s *BlockAccessList) Encode() ([]byte, error) {
	return rlp.EncodeToBytes(s)
}

// Decode parses an RLP-encoded list and checks that it is in canonical order.
func Decode(b []byte) (*BlockAccessList, error) {
	s := new(BlockAccessList)
	if err := rlp.DecodeBytes(b, s); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Validate checks that the list is in the canonical order the Builder produces,
// so that one block has exactly one encoding.
func (s *BlockAccessList) Validate() error {
	for i := range s.Accounts {
		a := &s.Accounts[i]
		if i > 0 && bytes.Compare(s.Accounts[i-1].Address[:], a.Address[:]) >= 0 {
			return errUnsortedAccounts
		}
		if err := a.validate(); err != nil {
			return fmt.Errorf("account %s: %w", a.Address.Hex(), err)
		}
	}
	return nil
}

func (a *AccountAccess) validate() error {
	for i := range a.StorageWrites {
		if i > 0 && bytes.Compare(a.StorageWrites[i-1].Slot[:], a.StorageWrites[i].Slot[:]) >= 0 {
			return errUnsortedSlots
		}
		changes := a.StorageWrites[i].Changes
		if len(changes) == 0 {
			return errEmptySlotWrites
		}
		if !increasing(len(changes), func(j int) uint32 { return changes[j].Index }) {
			return errUnsortedIndices
		}
	}
	for i := 1; i < len(a.StorageReads); i++ {
		if bytes.Compare(a.StorageReads[i-1][:], a.StorageReads[i][:]) >= 0 {
			return errUnsortedSlots
		}
	}
	ok := increasing(len(a.BalanceChanges), func(i int) uint32 { return a.BalanceChanges[i].Index }) &&
		increasing(len(a.NonceChanges), func(i int) uint32 { return a.NonceChanges[i].Index }) &&
		increasing(len(a.CodeChanges), func(i int) uint32 { return a.CodeChanges[i].Index }) &&
		increasing(len(a.AccountChanges), func(i int) uint32 { return a.AccountChanges[i].Index }) &&
		increasing(len(a.Deleted), func(i int) uint32 { return a.Deleted[i] }) &&
		increasing(len(a.StorageCleared), func(i int) uint32 { return a.StorageCleared[i] })
	if !ok {
		return errUnsortedIndices
	}
	return nil
}

func increasing(n int, index func(int) uint32) bool {
	for i := 1; i < n; i++ {
		if index(i-1) >= index(i) {
			return false
		}
	}
	return true
}

// Changed reports whether the account was changed, as opposed to only read.
func (a *AccountAccess) Changed() bool {
	return len(a.StorageWrites) > 0 || len(a.BalanceChanges) > 0 || len(a.NonceChanges) > 0 ||
		len(a.CodeChanges) > 0 || len(a.AccountChanges) > 0 || len(a.Deleted) > 0 || len(a.StorageCleared) > 0
}

// LastWriteIndex returns the highest index of any recorded write to the account
// except deletion. ok is false if the account has no such write.
func (a *AccountAccess) LastWriteIndex() (idx uint32, ok bool) {
	update := func(i uint32) {
		if !ok || i > idx {
			idx, ok = i, true
		}
	}
	for _, w := range a.StorageWrites {
		update(w.Changes[len(w.Changes)-1].Index)
	}
	if n := len(a.BalanceChanges); n > 0 {
		update(a.BalanceChanges[n-1].Index)
	}
	if n := len(a.NonceChanges); n > 0 {
		update(a.NonceChanges[n-1].Index)
	}
	if n := len(a.CodeChanges); n > 0 {
		update(a.CodeChanges[n-1].Index)
	}
	if n := len(a.AccountChanges); n > 0 {
		update(a.AccountChanges[n-1].Index)
	}
	if n := len(a.StorageCleared); n > 0 {
		update(a.StorageCleared[n-1])
	}
	return idx, ok
}
