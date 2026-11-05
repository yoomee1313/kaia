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
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
)

var (
	errUnsortedAccounts = errors.New("accounts are not strictly sorted by address")
	errUnsortedSlots    = errors.New("slots are not strictly sorted")
	errUnsortedIndices  = errors.New("change indices are not strictly increasing")
	errEmptySlotWrites  = errors.New("slot has no write")
	errNoStorageChange  = errors.New("storage root without a storage change")

	ErrHashMismatch = errors.New("block access list hash mismatch")
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

	// StorageRoot is the storage root at the end of the block, if the storage
	// changed and the account exists at the end of the block. It is no longer
	// recorded, and is not part of the hash, but lists that have it are still
	// decoded and the root is checked when they are applied.
	StorageRoot *common.Hash `rlp:"optional"`
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

// Writes returns the list without its reads: the accounts that were only read
// are left out, and so are the storage reads and storage roots of the others.
// Accounts are shared with s, so neither may be modified afterwards.
func (s *BlockAccessList) Writes() *BlockAccessList {
	w := &BlockAccessList{Accounts: make([]AccountAccess, 0, len(s.Accounts))}
	for _, a := range s.Accounts {
		if !a.Changed() {
			continue
		}
		a.StorageReads, a.StorageRoot = nil, nil
		w.Accounts = append(w.Accounts, a)
	}
	return w
}

// Hash returns the hash committed in the header from the Amber hardfork: the
// keccak256 of the RLP encoding of Writes. Blocks and headers carry the writes
// only, so a node that receives or keeps the writes only checks them against
// the header as well; the reads are meant to be exchanged in consensus
// messages.
func (s *BlockAccessList) Hash() (common.Hash, error) {
	enc, err := s.Writes().Encode()
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash(enc), nil
}

// CheckHash checks the writes of the list against the hash committed in a
// header, before the list is applied. A header without one, before the Amber hardfork, passes:
// only the state root after applying the list can check it then.
func (s *BlockAccessList) CheckHash(want *common.Hash) error {
	if want == nil {
		return nil
	}
	got, err := s.Hash()
	if err != nil {
		return err
	}
	if got != *want {
		return fmt.Errorf("%w: header %x, list %x", ErrHashMismatch, *want, got)
	}
	return nil
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
	if a.StorageRoot != nil && !a.StorageChanged() {
		return errNoStorageChange
	}
	return nil
}

// StorageChanged returns whether the block wrote or dropped the storage of the
// account.
func (a *AccountAccess) StorageChanged() bool {
	return len(a.StorageWrites) > 0 || len(a.StorageCleared) > 0
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

// Reset returns how a deletion or a recreation in the block supersedes the
// changes of the account. If deleted is set, the account does not exist at the
// end of the block. Otherwise changes with an index below from are superseded,
// and cleared tells that the account was recreated from scratch at from, with
// an empty storage. Changes with the same index as a deletion happened after it.
func (a *AccountAccess) Reset() (from uint32, cleared, deleted bool) {
	if n := len(a.Deleted); n > 0 {
		if last, ok := a.LastWriteIndex(); !ok || last < a.Deleted[n-1] {
			return 0, false, true
		}
		from, cleared = a.Deleted[n-1], true
	}
	if n := len(a.StorageCleared); n > 0 && a.StorageCleared[n-1] >= from {
		from, cleared = a.StorageCleared[n-1], true
	}
	return from, cleared, false
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

// EntryCount returns the accounts plus the storage slots of the list, read or
// written. It is what the Amber entry limit counts.
func (s *BlockAccessList) EntryCount() uint64 {
	n := uint64(len(s.Accounts))
	for i := range s.Accounts {
		n += uint64(len(s.Accounts[i].StorageWrites) + len(s.Accounts[i].StorageReads))
	}
	return n
}
