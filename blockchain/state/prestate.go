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
	"context"
	"sync"
	"sync/atomic"

	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
)

// Prestate holds the accounts and the storage slots of a state, decoded, for
// the states of the phases of a block that run in parallel: each phase opens
// its own state at the parent root, and without a Prestate each one walks the
// tries and decodes their nodes again for every key it reads. It is read only
// once built, so the phases share it.
//
// A state with a Prestate takes an account from it instead of the account
// trie, and a slot instead of the storage trie as long as the storage root of
// the account is the one of the Prestate. Keys it does not hold are read from
// the tries as usual.
type Prestate struct {
	accounts map[common.Address]*prestateAccount
}

type prestateAccount struct {
	acc   account.Account // nil if the account does not exist
	root  common.ExtHash  // its storage root, empty if it has none
	slots map[common.Hash]common.Hash
}

// PrestateKeys are the keys a Prestate holds: accounts with the slots read
// from their storage.
type PrestateKeys map[common.Address][]common.Hash

// NewPrestate reads keys from the state at root in db with the given number
// of goroutines. It returns nil if ctx is done first or a read fails: the
// phases then read the tries.
func NewPrestate(ctx context.Context, db Database, root common.Hash, keys PrestateKeys, workers int) *Prestate {
	p := &Prestate{accounts: make(map[common.Address]*prestateAccount, len(keys))}
	addrs := make([]common.Address, 0, len(keys))
	for addr, slots := range keys {
		p.accounts[addr] = &prestateAccount{slots: make(map[common.Hash]common.Hash, len(slots))}
		addrs = append(addrs, addr)
	}
	var (
		wg     sync.WaitGroup
		next   atomic.Int64
		failed atomic.Bool
	)
	for range max(1, workers) {
		wg.Go(func() {
			sdb, err := New(root, db, nil, nil)
			if err != nil {
				failed.Store(true)
				return
			}
			for i := int(next.Add(1) - 1); i < len(addrs) && ctx.Err() == nil; i = int(next.Add(1) - 1) {
				addr := addrs[i]
				pa := p.accounts[addr]
				obj := sdb.loadStateObject(addr)
				if obj == nil {
					continue
				}
				pa.acc = obj.account.DeepCopy()
				pa.root = obj.storageRoot()
				for _, slot := range keys[addr] {
					pa.slots[slot] = obj.GetCommittedState(db, slot)
				}
			}
			if sdb.Error() != nil {
				failed.Store(true)
			}
		})
	}
	wg.Wait()
	if failed.Load() || ctx.Err() != nil {
		return nil
	}
	return p
}

// account returns a copy of the account at addr, nil if it does not exist,
// and whether the Prestate holds it.
func (p *Prestate) account(addr common.Address) (account.Account, bool) {
	pa, ok := p.accounts[addr]
	if !ok {
		return nil, false
	}
	if pa.acc == nil {
		return nil, true
	}
	return pa.acc.DeepCopy(), true
}

// slot returns the value of a slot of addr and whether the Prestate holds it
// for an account whose storage root is root.
func (p *Prestate) slot(addr common.Address, root common.ExtHash, key common.Hash) (common.Hash, bool) {
	pa, ok := p.accounts[addr]
	if !ok || pa.acc == nil || pa.root != root {
		return common.Hash{}, false
	}
	v, ok := pa.slots[key]
	return v, ok
}

// SetPrestate makes the state read the accounts and the slots of p before the
// tries, see Prestate.
func (s *StateDB) SetPrestate(p *Prestate) {
	s.prestate = p
}

// SkipTrieWrites makes Finalise keep the changes of the state in its objects,
// where reads find them, without writing them to the tries: the state of a
// phase that runs in parallel records its changes, and its root is never
// computed. IntermediateRoot and Commit of such a state are wrong.
func (s *StateDB) SkipTrieWrites() {
	s.skipTrieWrites = true
}

// storageRoot returns the storage root of the account, empty if it has no
// storage.
func (s *stateObject) storageRoot() common.ExtHash {
	if acc := account.GetProgramAccount(s.account); acc != nil {
		return acc.GetStorageRoot()
	}
	return common.ExtHash{}
}
