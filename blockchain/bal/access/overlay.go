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
	"bytes"
	"fmt"
	"slices"
	"sort"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
)

// ApplyBefore writes the values of the phases below k of a block access list,
// which gives the state at the start of phase k when the state is opened at
// the parent root, and finalises them like the end of a tx.
func ApplyBefore(s *state.StateDB, list *BlockAccessList, k int) error {
	for i := range list.Accounts {
		a := list.Accounts[i].Before(uint32(k))
		if err := applyAccountAccess(s, &a); err != nil {
			return fmt.Errorf("account %s: %w", a.Address.Hex(), err)
		}
	}
	s.Finalise(true, false)
	return nil
}

// Index holds the accounts of a block access list by address, for UseBefore.
// It is read only, so the phases of a block share it.
type Index map[common.Address]*indexedAccount

type indexedAccount struct {
	access *AccountAccess
	// storageWrites holds the phases that wrote a slot of the account, sorted
	// and unique, so the last one before a phase is found without the slots.
	storageWrites []uint32
}

func NewIndex(list *BlockAccessList) Index {
	index := make(Index, len(list.Accounts))
	for i := range list.Accounts {
		a := &list.Accounts[i]
		var writes []uint32
		for _, w := range a.StorageWrites {
			for _, c := range w.Changes {
				writes = append(writes, c.Index)
			}
		}
		slices.Sort(writes)
		index[a.Address] = &indexedAccount{access: a, storageWrites: slices.Compact(writes)}
	}
	return index
}

// UseBefore makes a state opened at the parent root see the values of the
// phases below k of a block access list, as after ApplyBefore, without
// applying the list up front: an account gets them when the state first loads
// it, and a slot when it is first read. So preparing a phase costs the
// accounts and slots it touches, found by binary search, not the whole list.
// See state.StateDB.SetAccessOverlay for the use of such a state. It must be
// called before EnableRecorderAt.
func UseBefore(s *state.StateDB, index Index, k int) {
	s.SetAccessOverlay(overlay{index: index, k: uint32(k)})
}

type overlay struct {
	index Index
	k     uint32
}

// countBefore returns how many of the entries, sorted by phase, are below k.
func countBefore(n int, phase func(int) uint32, k uint32) int {
	return sort.Search(n, func(i int) bool { return phase(i) >= k })
}

// Account returns the changes of the account before the phase, as
// applyAccountAccess and Finalise would leave them.
func (o overlay) Account(addr common.Address) *state.OverlayAccount {
	ia := o.index[addr]
	if ia == nil {
		return nil
	}
	return accountBefore(ia.access, o.k, func() (uint32, bool) {
		n := countBefore(len(ia.storageWrites), func(i int) uint32 { return ia.storageWrites[i] }, o.k)
		if n == 0 {
			return 0, false
		}
		return ia.storageWrites[n-1], true
	})
}

// AccountBefore returns the account at the start of phase k relative to the
// parent state, from its changes alone, as a state given the list with
// UseBefore sees it; nil if no change of the account is before k. It finds the
// changes by binary search.
func AccountBefore(a *AccountAccess, k uint32) *state.OverlayAccount {
	return accountBefore(a, k, func() (uint32, bool) {
		var (
			last    uint32
			written bool
		)
		for _, w := range a.StorageWrites {
			if n := countBefore(len(w.Changes), func(i int) uint32 { return w.Changes[i].Index }, k); n > 0 {
				if idx := w.Changes[n-1].Index; !written || idx > last {
					last, written = idx, true
				}
			}
		}
		return last, written
	})
}

// accountBefore is AccountBefore given the last phase below k that wrote a
// slot of the account.
func accountBefore(a *AccountAccess, k uint32, lastSlotWrite func() (uint32, bool)) *state.OverlayAccount {
	var (
		balances = a.BalanceChanges[:countBefore(len(a.BalanceChanges), func(i int) uint32 { return a.BalanceChanges[i].Index }, k)]
		nonces   = a.NonceChanges[:countBefore(len(a.NonceChanges), func(i int) uint32 { return a.NonceChanges[i].Index }, k)]
		codes    = a.CodeChanges[:countBefore(len(a.CodeChanges), func(i int) uint32 { return a.CodeChanges[i].Index }, k)]
		records  = a.AccountChanges[:countBefore(len(a.AccountChanges), func(i int) uint32 { return a.AccountChanges[i].Index }, k)]
		deleted  = a.Deleted[:countBefore(len(a.Deleted), func(i int) uint32 { return a.Deleted[i] }, k)]
		cleared  = a.StorageCleared[:countBefore(len(a.StorageCleared), func(i int) uint32 { return a.StorageCleared[i] }, k)]
	)
	slotWrite, slotsWritten := lastSlotWrite()
	if len(balances)+len(nonces)+len(codes)+len(records)+len(deleted)+len(cleared) == 0 && !slotsWritten {
		return nil
	}

	// As AccountAccess.Reset, on the changes before k.
	var (
		lastWrite uint32
		written   bool
	)
	note := func(n int, idx func(int) uint32) {
		if n > 0 && (!written || idx(n-1) > lastWrite) {
			lastWrite, written = idx(n-1), true
		}
	}
	note(len(balances), func(i int) uint32 { return balances[i].Index })
	note(len(nonces), func(i int) uint32 { return nonces[i].Index })
	note(len(codes), func(i int) uint32 { return codes[i].Index })
	note(len(records), func(i int) uint32 { return records[i].Index })
	if slotsWritten {
		note(1, func(int) uint32 { return slotWrite })
	}
	out := &state.OverlayAccount{}
	var from uint32
	if n := len(deleted); n > 0 {
		if !written || lastWrite < deleted[n-1] {
			out.Deleted = true
			return out
		}
		from, out.Cleared = deleted[n-1], true
	}
	if n := len(cleared); n > 0 && cleared[n-1] >= from {
		from, out.Cleared = cleared[n-1], true
	}
	if n := len(records); n > 0 && records[n-1].Index >= from {
		out.Record = records[n-1].Account
	}
	if n := len(balances); n > 0 && balances[n-1].Index >= from {
		out.Balance = balances[n-1].Balance
	}
	if n := len(nonces); n > 0 && nonces[n-1].Index >= from {
		nonce := nonces[n-1].Nonce
		out.Nonce = &nonce
	}
	if n := len(codes); n > 0 && codes[n-1].Index >= from {
		out.Code, out.HasCode = codes[n-1].Code, true
	}
	if slotsWritten && slotWrite >= from {
		out.Slot = slotValue(a, k, from)
	}
	return out
}

// slotValue returns the last value written to a slot of a from phase from and
// below phase k.
func slotValue(a *AccountAccess, k, from uint32) func(common.Hash) (common.Hash, bool) {
	return func(slot common.Hash) (common.Hash, bool) {
		writes := a.StorageWrites
		i := sort.Search(len(writes), func(i int) bool { return bytes.Compare(writes[i].Slot[:], slot[:]) >= 0 })
		if i == len(writes) || writes[i].Slot != slot {
			return common.Hash{}, false
		}
		changes := writes[i].Changes
		n := countBefore(len(changes), func(j int) uint32 { return changes[j].Index }, k)
		if n == 0 || changes[n-1].Index < from {
			return common.Hash{}, false
		}
		return changes[n-1].Value, true
	}
}
