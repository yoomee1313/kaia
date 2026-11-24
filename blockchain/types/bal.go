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

package types

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
)

// The block access list (BAL) holds the accounts and storage slots a block
// touched, with the post value of every change, in the style of EIP-7928
// block-level access lists. The StateDB records it while a block is
// executed to be written, see package state.
//
// Every change carries the BlockAccessIndex of the part of the block that made
// it:
//   - 0 is InitializeState (EIP-2935, system tx calls),
//   - 1..n are the transactions of the block,
//   - n+1 is FinalizeState (rewards).
//
// A BAL is not trusted: applying it to the parent state must reproduce the
// state root of the block header, so a wrong list cannot pass that check.
//
// Package blockchain/state stores the lists, and the chain keeps its BAL
// history, see state.Store.

var (
	errUnsortedAccounts = errors.New("accounts are not strictly sorted by address")
	errUnsortedSlots    = errors.New("slots are not strictly sorted")
	errUnsortedIndices  = errors.New("change indices are not strictly increasing")
	errEmptySlotChanges = errors.New("slot has no write")
	errNoStorageChange  = errors.New("storage root without a storage change")
	errReadsInWrites    = errors.New("writes hold storage reads")
)

// BlockAccessList is the access list of one block. Accounts are sorted by address.
type BlockAccessList struct {
	Accounts []AccountChanges
}

// AccountChanges holds what a block did to one account. An account with no
// change at all was only read.
//
// Kaia-specific fields (account type, AccountKey, humanReadable, codeInfo)
// are not listed one by one. Whenever they change, or the account comes into
// existence, the whole account is stored in AccountRecords. Its storage root
// is not trusted: the applier keeps or clears the storage itself.
type AccountChanges struct {
	Address        common.Address
	StorageChanges []SlotChanges   // sorted by slot
	StorageReads   []common.Hash   // sorted, slots that were read but not changed
	BalanceChanges []BalanceChange // sorted by index
	NonceChanges   []NonceChange   // sorted by index
	CodeChanges    []CodeChange    // sorted by index
	AccountRecords []AccountRecord // sorted by index
	Deleted        []uint32        // indexes at which the account was deleted
	StorageCleared []uint32        // indexes at which the account was recreated, dropping its storage

	// StorageRoot is the storage root at the end of the block, if the storage
	// changed and the account exists at the end of the block. It is not
	// recorded, but a list that has it still decodes, and the root is checked
	// when it is applied.
	StorageRoot *common.Hash `rlp:"optional"`
}

// SlotChanges holds the post values of one storage slot.
type SlotChanges struct {
	Slot    common.Hash
	Changes []StorageChange // sorted by index
}

type StorageChange struct {
	BlockAccessIndex uint32
	NewValue         common.Hash
}

type BalanceChange struct {
	BlockAccessIndex uint32
	PostBalance      *big.Int
}

type NonceChange struct {
	BlockAccessIndex uint32
	NewNonce         uint64
}

type CodeChange struct {
	BlockAccessIndex uint32
	NewCode          []byte
}

// AccountRecord is the whole account encoded by account.AccountSerializer.
type AccountRecord struct {
	BlockAccessIndex uint32
	Account          []byte
}

// Encode returns the RLP encoding of the list.
func (s *BlockAccessList) Encode() ([]byte, error) {
	return rlp.EncodeToBytes(s)
}

// Writes returns the list without its reads: the accounts that were only read
// are left out, and so are the storage reads and storage roots of the others.
// Accounts are shared with s, so neither may be modified afterwards.
func (s *BlockAccessList) Writes() *BlockAccessList {
	w := &BlockAccessList{Accounts: make([]AccountChanges, 0, len(s.Accounts))}
	for _, a := range s.Accounts {
		if !a.Changed() {
			continue
		}
		a.StorageReads, a.StorageRoot = nil, nil
		w.Accounts = append(w.Accounts, a)
	}
	return w
}

// Hash returns the BAL hash of the block: the keccak256 of the RLP encoding of
// Writes. It is the per-block value of the pre-Amber BAL hash chain, and from
// the Amber hardfork the header commits the same value.
func (s *BlockAccessList) Hash() (common.Hash, error) {
	enc, err := s.Writes().Encode()
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash(enc), nil
}

// BALReads holds what a block read without changing it: the accounts it only
// read, and the storage slots of any account that it read but did not change.
// With Writes it makes up the whole list, see CombineBAL. BALReads are not part of
// the hash, and executing the block again on its parent state gives them back.
type BALReads struct {
	Accounts []AccountReads // sorted by address
}

// AccountReads holds the reads of one account. An account the block only read
// has one even without slots.
type AccountReads struct {
	Address common.Address
	Slots   []common.Hash // sorted
}

// BALReads returns the reads of the list.
func (s *BlockAccessList) Reads() *BALReads {
	r := &BALReads{}
	for _, a := range s.Accounts {
		if !a.Changed() || len(a.StorageReads) > 0 {
			r.Accounts = append(r.Accounts, AccountReads{Address: a.Address, Slots: a.StorageReads})
		}
	}
	return r
}

// Encode returns the RLP encoding of the reads.
func (r *BALReads) Encode() ([]byte, error) {
	return rlp.EncodeToBytes(r)
}

// DecodeBALReads parses RLP-encoded reads and checks that they are sorted.
func DecodeBALReads(b []byte) (*BALReads, error) {
	r := new(BALReads)
	if err := rlp.DecodeBytes(b, r); err != nil {
		return nil, err
	}
	for i := range r.Accounts {
		a := &r.Accounts[i]
		if i > 0 && bytes.Compare(r.Accounts[i-1].Address[:], a.Address[:]) >= 0 {
			return nil, errUnsortedAccounts
		}
		for j := 1; j < len(a.Slots); j++ {
			if bytes.Compare(a.Slots[j-1][:], a.Slots[j][:]) >= 0 {
				return nil, fmt.Errorf("account %s: %w", a.Address.Hex(), errUnsortedSlots)
			}
		}
	}
	return r, nil
}

// CombineBAL returns the whole list, as recorded, from its writes and its reads.
// Accounts are shared with writes, so neither may be modified afterwards.
func CombineBAL(writes *BlockAccessList, reads *BALReads) (*BlockAccessList, error) {
	out := &BlockAccessList{Accounts: make([]AccountChanges, 0, len(writes.Accounts)+len(reads.Accounts))}
	slots := func(r *AccountReads) []common.Hash {
		if len(r.Slots) == 0 {
			return nil
		}
		return r.Slots
	}
	for i, j := 0, 0; i < len(writes.Accounts) || j < len(reads.Accounts); {
		cmp := -1
		if i == len(writes.Accounts) {
			cmp = 1
		} else if j < len(reads.Accounts) {
			cmp = bytes.Compare(writes.Accounts[i].Address[:], reads.Accounts[j].Address[:])
		}
		switch {
		case cmp < 0:
			out.Accounts = append(out.Accounts, writes.Accounts[i])
			i++
		case cmp > 0:
			out.Accounts = append(out.Accounts, AccountChanges{Address: reads.Accounts[j].Address, StorageReads: slots(&reads.Accounts[j])})
			j++
		default:
			a := writes.Accounts[i]
			if len(a.StorageReads) > 0 {
				return nil, fmt.Errorf("account %s: %w", a.Address.Hex(), errReadsInWrites)
			}
			a.StorageReads = slots(&reads.Accounts[j])
			out.Accounts = append(out.Accounts, a)
			i, j = i+1, j+1
		}
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeBAL parses an RLP-encoded list and checks that it is in canonical order.
func DecodeBAL(b []byte) (*BlockAccessList, error) {
	s := new(BlockAccessList)
	if err := rlp.DecodeBytes(b, s); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Validate checks that the list is in the canonical order state.Recorder produces,
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

func (a *AccountChanges) validate() error {
	for i := range a.StorageChanges {
		if i > 0 && bytes.Compare(a.StorageChanges[i-1].Slot[:], a.StorageChanges[i].Slot[:]) >= 0 {
			return errUnsortedSlots
		}
		changes := a.StorageChanges[i].Changes
		if len(changes) == 0 {
			return errEmptySlotChanges
		}
		if !increasing(len(changes), func(j int) uint32 { return changes[j].BlockAccessIndex }) {
			return errUnsortedIndices
		}
	}
	for i := 1; i < len(a.StorageReads); i++ {
		if bytes.Compare(a.StorageReads[i-1][:], a.StorageReads[i][:]) >= 0 {
			return errUnsortedSlots
		}
	}
	ok := increasing(len(a.BalanceChanges), func(i int) uint32 { return a.BalanceChanges[i].BlockAccessIndex }) &&
		increasing(len(a.NonceChanges), func(i int) uint32 { return a.NonceChanges[i].BlockAccessIndex }) &&
		increasing(len(a.CodeChanges), func(i int) uint32 { return a.CodeChanges[i].BlockAccessIndex }) &&
		increasing(len(a.AccountRecords), func(i int) uint32 { return a.AccountRecords[i].BlockAccessIndex }) &&
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
func (a *AccountChanges) StorageChanged() bool {
	return len(a.StorageChanges) > 0 || len(a.StorageCleared) > 0
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
func (a *AccountChanges) Changed() bool {
	return len(a.StorageChanges) > 0 || len(a.BalanceChanges) > 0 || len(a.NonceChanges) > 0 ||
		len(a.CodeChanges) > 0 || len(a.AccountRecords) > 0 || len(a.Deleted) > 0 || len(a.StorageCleared) > 0
}

// Reset returns how a deletion or a recreation in the block supersedes the
// changes of the account. If deleted is set, the account does not exist at the
// end of the block. Otherwise changes with an index below from are superseded,
// and cleared tells that the account was recreated from scratch at from, with
// an empty storage. Changes with the same index as a deletion happened after it.
func (a *AccountChanges) Reset() (from uint32, cleared, deleted bool) {
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
func (a *AccountChanges) LastWriteIndex() (idx uint32, ok bool) {
	update := func(i uint32) {
		if !ok || i > idx {
			idx, ok = i, true
		}
	}
	for _, w := range a.StorageChanges {
		update(w.Changes[len(w.Changes)-1].BlockAccessIndex)
	}
	if n := len(a.BalanceChanges); n > 0 {
		update(a.BalanceChanges[n-1].BlockAccessIndex)
	}
	if n := len(a.NonceChanges); n > 0 {
		update(a.NonceChanges[n-1].BlockAccessIndex)
	}
	if n := len(a.CodeChanges); n > 0 {
		update(a.CodeChanges[n-1].BlockAccessIndex)
	}
	if n := len(a.AccountRecords); n > 0 {
		update(a.AccountRecords[n-1].BlockAccessIndex)
	}
	if n := len(a.StorageCleared); n > 0 {
		update(a.StorageCleared[n-1])
	}
	return idx, ok
}
