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

package checkpoint

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
)

// RevertStats counts what Revert set back.
type RevertStats struct {
	Accounts uint64 // distinct accounts changed after the target
	Slots    uint64 // distinct slots changed after the target
	Rebuilt  uint64 // accounts whose storage was dropped after the target, rebuilt slot by slot
	Removed  uint64 // accounts that did not exist at the target
	Written  uint64 // slots written to rebuild storages
}

// Revert sets the state of block head back to block target: every account and
// slot changed in blocks target+1 to head gets its value at the end of target,
// read from the history. The changes stay in sdb, which must be opened at the
// state root of head and must not be committed, so the database is left as it
// is. An account deleted or recreated after target has its storage rebuilt
// from every slot written up to target. header, if set, returns the header of
// a block, and the root is checked against the one of target.
func Revert(sdb *state.StateDB, h *bal.History, lists bal.ListReader, target, head uint64, header func(uint64) (*types.Header, error)) (RevertStats, error) {
	var stats RevertStats
	if target > head {
		return stats, fmt.Errorf("block %d is after the head %d", target, head)
	}
	type changed struct {
		slots map[common.Hash]struct{}
		reset bool
	}
	accounts := make(map[common.Address]*changed)
	for n := target + 1; n <= head; n++ {
		list, err := lists.Read(n)
		if err != nil {
			return stats, fmt.Errorf("block %d: %w", n, err)
		}
		for i := range list.Accounts {
			a := &list.Accounts[i]
			if !a.Changed() {
				continue
			}
			c := accounts[a.Address]
			if c == nil {
				c = &changed{slots: make(map[common.Hash]struct{})}
				accounts[a.Address] = c
			}
			for _, w := range a.StorageWrites {
				c.slots[w.Slot] = struct{}{}
			}
			if len(a.Deleted) > 0 || len(a.StorageCleared) > 0 {
				c.reset = true
			}
		}
	}

	addrs := make([]common.Address, 0, len(accounts))
	for addr, c := range accounts {
		addrs = append(addrs, addr)
		stats.Slots += uint64(len(c.slots))
	}
	sort.Slice(addrs, func(i, j int) bool { return bytes.Compare(addrs[i][:], addrs[j][:]) < 0 })
	stats.Accounts = uint64(len(addrs))

	for _, addr := range addrs {
		c := accounts[addr]
		acc, err := h.Account(addr, target)
		if err != nil {
			return stats, fmt.Errorf("account %s: %w", addr.Hex(), err)
		}
		if acc == nil {
			if sdb.Exist(addr) {
				sdb.SelfDestruct(addr)
			}
			stats.Removed++
			continue
		}
		// A storage dropped after target, or an account gone at head, has no
		// storage of target left to change: it is rebuilt.
		rebuild := c.reset || !sdb.Exist(addr)
		sdb.SetAccount(addr, acc, rebuild)

		var slots []common.Hash
		if rebuild {
			stats.Rebuilt++
			if slots, err = h.Slots(addr, target); err != nil {
				return stats, fmt.Errorf("account %s: %w", addr.Hex(), err)
			}
		} else {
			for slot := range c.slots {
				slots = append(slots, slot)
			}
		}
		for _, slot := range slots {
			value, err := h.Storage(addr, slot, target)
			if err != nil {
				return stats, fmt.Errorf("account %s slot %x: %w", addr.Hex(), slot, err)
			}
			if rebuild {
				if value == (common.Hash{}) {
					continue
				}
				stats.Written++
			}
			sdb.SetState(addr, slot, value)
		}
	}
	if err := sdb.Error(); err != nil {
		return stats, err
	}
	if header != nil {
		want, err := header(target)
		if err != nil {
			return stats, err
		}
		if root := sdb.IntermediateRoot(false); root != want.Root {
			return stats, fmt.Errorf("block %d: reverted state root %x, header has %x", target, root, want.Root)
		}
	}
	return stats, sdb.Error()
}
