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
	"slices"

	"github.com/kaiachain/kaia/common"
)

// Before returns the changes of the account made in phases below k: the state
// at the start of phase k is the parent state with these applied. Reads and
// the storage root are left out.
func (a AccountAccess) Before(k uint32) AccountAccess {
	out := AccountAccess{Address: a.Address}
	for _, w := range a.StorageWrites {
		var changes []SlotChange
		for _, c := range w.Changes {
			if c.Index < k {
				changes = append(changes, c)
			}
		}
		if len(changes) > 0 {
			out.StorageWrites = append(out.StorageWrites, SlotWrites{Slot: w.Slot, Changes: changes})
		}
	}
	out.BalanceChanges = before(a.BalanceChanges, k)
	out.NonceChanges = before(a.NonceChanges, k)
	out.CodeChanges = before(a.CodeChanges, k)
	out.AccountChanges = before(a.AccountChanges, k)
	for _, i := range a.Deleted {
		if i < k {
			out.Deleted = append(out.Deleted, i)
		}
	}
	for _, i := range a.StorageCleared {
		if i < k {
			out.StorageCleared = append(out.StorageCleared, i)
		}
	}
	return out
}

func before[T indexed](list []T, k uint32) []T {
	var out []T
	for _, v := range list {
		if v.index() < k {
			out = append(out, v)
		}
	}
	return out
}

// Merge combines the lists of the phases of one block, given in phase order
// with disjoint phase indices, into the list of the block. Slots read in one
// phase and written in another are writes, as the recorder of a whole block
// lists them.
func Merge(lists ...*BlockAccessList) *BlockAccessList {
	type merged struct {
		acc   AccountAccess
		slots map[common.Hash]int // index in acc.StorageWrites
		reads map[common.Hash]struct{}
	}
	accounts := make(map[common.Address]*merged)
	for _, list := range lists {
		for i := range list.Accounts {
			a := &list.Accounts[i]
			m := accounts[a.Address]
			if m == nil {
				m = &merged{acc: AccountAccess{Address: a.Address}, slots: make(map[common.Hash]int), reads: make(map[common.Hash]struct{})}
				accounts[a.Address] = m
			}
			for _, w := range a.StorageWrites {
				if j, ok := m.slots[w.Slot]; ok {
					m.acc.StorageWrites[j].Changes = append(m.acc.StorageWrites[j].Changes, w.Changes...)
				} else {
					m.slots[w.Slot] = len(m.acc.StorageWrites)
					m.acc.StorageWrites = append(m.acc.StorageWrites, SlotWrites{Slot: w.Slot, Changes: slices.Clone(w.Changes)})
				}
			}
			for _, r := range a.StorageReads {
				m.reads[r] = struct{}{}
			}
			m.acc.BalanceChanges = append(m.acc.BalanceChanges, a.BalanceChanges...)
			m.acc.NonceChanges = append(m.acc.NonceChanges, a.NonceChanges...)
			m.acc.CodeChanges = append(m.acc.CodeChanges, a.CodeChanges...)
			m.acc.AccountChanges = append(m.acc.AccountChanges, a.AccountChanges...)
			m.acc.Deleted = append(m.acc.Deleted, a.Deleted...)
			m.acc.StorageCleared = append(m.acc.StorageCleared, a.StorageCleared...)
		}
	}
	out := &BlockAccessList{Accounts: make([]AccountAccess, 0, len(accounts))}
	for _, m := range accounts {
		slices.SortFunc(m.acc.StorageWrites, func(x, y SlotWrites) int { return bytes.Compare(x.Slot[:], y.Slot[:]) })
		for r := range m.reads {
			if _, written := m.slots[r]; !written {
				m.acc.StorageReads = append(m.acc.StorageReads, r)
			}
		}
		slices.SortFunc(m.acc.StorageReads, func(x, y common.Hash) int { return bytes.Compare(x[:], y[:]) })
		out.Accounts = append(out.Accounts, m.acc)
	}
	slices.SortFunc(out.Accounts, func(x, y AccountAccess) int { return bytes.Compare(x.Address[:], y.Address[:]) })
	return out
}
