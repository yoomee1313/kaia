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

package bal

import (
	"bytes"
	"math/big"
	"slices"

	"github.com/kaiachain/kaia/common"
)

// Builder collects accesses during block execution and produces the canonical
// BlockAccessList. Changes must be added in non-decreasing index order; a change
// with the same index as the previous one of the same field replaces it, so
// only the post value of each phase is kept.
type Builder struct {
	accounts map[common.Address]*accountBuilder
}

type accountBuilder struct {
	reads    map[common.Hash]struct{}
	slots    map[common.Hash][]SlotChange
	balances []BalanceChange
	nonces   []NonceChange
	codes    []CodeChange
	accounts []AccountChange
	deleted  []uint32
	cleared  []uint32
}

func NewBuilder() *Builder {
	return &Builder{accounts: make(map[common.Address]*accountBuilder)}
}

func (b *Builder) account(addr common.Address) *accountBuilder {
	a := b.accounts[addr]
	if a == nil {
		a = &accountBuilder{
			reads: make(map[common.Hash]struct{}),
			slots: make(map[common.Hash][]SlotChange),
		}
		b.accounts[addr] = a
	}
	return a
}

// ReadAccount records that the account was accessed.
func (b *Builder) ReadAccount(addr common.Address) {
	b.account(addr)
}

// ReadSlot records that the slot was read.
func (b *Builder) ReadSlot(addr common.Address, slot common.Hash) {
	b.account(addr).reads[slot] = struct{}{}
}

func (b *Builder) WriteSlot(addr common.Address, idx uint32, slot, value common.Hash) {
	a := b.account(addr)
	a.slots[slot] = upsert(a.slots[slot], SlotChange{Index: idx, Value: value})
}

func (b *Builder) SetBalance(addr common.Address, idx uint32, balance *big.Int) {
	a := b.account(addr)
	a.balances = upsert(a.balances, BalanceChange{Index: idx, Balance: new(big.Int).Set(balance)})
}

func (b *Builder) SetNonce(addr common.Address, idx uint32, nonce uint64) {
	a := b.account(addr)
	a.nonces = upsert(a.nonces, NonceChange{Index: idx, Nonce: nonce})
}

func (b *Builder) SetCode(addr common.Address, idx uint32, code []byte) {
	a := b.account(addr)
	a.codes = upsert(a.codes, CodeChange{Index: idx, Code: common.CopyBytes(code)})
}

// SetAccount records the whole account, encoded by account.AccountSerializer.
func (b *Builder) SetAccount(addr common.Address, idx uint32, enc []byte) {
	a := b.account(addr)
	a.accounts = upsert(a.accounts, AccountChange{Index: idx, Account: common.CopyBytes(enc)})
}

// ClearStorage records that the account was recreated in phase idx. Slot
// writes recorded earlier in the same phase are dropped, as they happened
// before the storage was cleared.
func (b *Builder) ClearStorage(addr common.Address, idx uint32) {
	a := b.account(addr)
	a.trimSlots(idx)
	a.cleared = appendIndex(a.cleared, idx)
}

// Delete records that the account was deleted at the end of phase idx. All
// changes recorded earlier in the same phase are dropped. Changes recorded
// later with the same index belong to a recreation after the deletion.
func (b *Builder) Delete(addr common.Address, idx uint32) {
	a := b.account(addr)
	a.trimSlots(idx)
	a.balances = trim(a.balances, idx)
	a.nonces = trim(a.nonces, idx)
	a.codes = trim(a.codes, idx)
	a.accounts = trim(a.accounts, idx)
	if n := len(a.cleared); n > 0 && a.cleared[n-1] == idx {
		a.cleared = a.cleared[:n-1]
	}
	a.deleted = appendIndex(a.deleted, idx)
}

func (a *accountBuilder) trimSlots(idx uint32) {
	for slot, changes := range a.slots {
		if changes = trim(changes, idx); len(changes) == 0 {
			delete(a.slots, slot)
		} else {
			a.slots[slot] = changes
		}
	}
}

// Build returns the list in canonical order.
func (b *Builder) Build() *BlockAccessList {
	addrs := make([]common.Address, 0, len(b.accounts))
	for addr := range b.accounts {
		addrs = append(addrs, addr)
	}
	slices.SortFunc(addrs, func(x, y common.Address) int { return bytes.Compare(x[:], y[:]) })

	list := &BlockAccessList{Accounts: make([]AccountAccess, 0, len(addrs))}
	for _, addr := range addrs {
		a := b.accounts[addr]
		acc := AccountAccess{
			Address:        addr,
			BalanceChanges: a.balances,
			NonceChanges:   a.nonces,
			CodeChanges:    a.codes,
			AccountChanges: a.accounts,
			Deleted:        a.deleted,
			StorageCleared: a.cleared,
		}
		for slot, changes := range a.slots {
			acc.StorageWrites = append(acc.StorageWrites, SlotWrites{Slot: slot, Changes: changes})
		}
		slices.SortFunc(acc.StorageWrites, func(x, y SlotWrites) int { return bytes.Compare(x.Slot[:], y.Slot[:]) })
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

type indexed interface {
	index() uint32
}

func (c SlotChange) index() uint32    { return c.Index }
func (c BalanceChange) index() uint32 { return c.Index }
func (c NonceChange) index() uint32   { return c.Index }
func (c CodeChange) index() uint32    { return c.Index }
func (c AccountChange) index() uint32 { return c.Index }

// upsert appends v, or replaces the last element if it has the same index.
func upsert[T indexed](list []T, v T) []T {
	if n := len(list); n > 0 && list[n-1].index() == v.index() {
		list[n-1] = v
		return list
	}
	return append(list, v)
}

// trim drops the last element if it has the given index.
func trim[T indexed](list []T, idx uint32) []T {
	if n := len(list); n > 0 && list[n-1].index() == idx {
		return list[:n-1]
	}
	return list
}

func appendIndex(list []uint32, idx uint32) []uint32 {
	if n := len(list); n > 0 && list[n-1] == idx {
		return list
	}
	return append(list, idx)
}
