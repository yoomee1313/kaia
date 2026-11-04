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
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/hashicorp/golang-lru"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
)

var (
	emptyCodeHash = crypto.Keccak256Hash(nil)

	errHistoryMissingAccount = errors.New("indexed block does not hold the account")
	errHistoryInconsistent   = errors.New("indexed block does not set the value")
)

// BaseState is the state the index starts from.
type BaseState interface {
	// Account returns the account, or nil if it does not exist.
	Account(addr common.Address) (account.Account, error)
	Storage(addr common.Address, slot common.Hash) (common.Hash, error)
	Code(codeHash common.Hash) ([]byte, error)
}

// ListReader reads the block access list of a block, see Freezer.
type ListReader interface {
	Read(number uint64) (*BlockAccessList, error)
}

// History reads the state at any indexed block from the index and the block
// access lists, without a state trie: a value is the one set by the last
// block at or before the requested block that changed it, or the base value.
// The rules mirror how StateDB.ApplyBlockAccessList applies a list.
type History struct {
	index *Index
	lists ListReader
	base  BaseState
	cache *lru.Cache // block number -> *BlockAccessList
}

func NewHistory(index *Index, lists ListReader, base BaseState) *History {
	return NewHistoryWithCache(index, lists, base, 16)
}

// NewHistoryWithCache is NewHistory keeping up to lists decoded lists in memory.
func NewHistoryWithCache(index *Index, lists ListReader, base BaseState, cached int) *History {
	cache, _ := lru.New(cached)
	return &History{index: index, lists: lists, base: base, cache: cache}
}

// Index returns the index the history reads.
func (h *History) Index() *Index { return h.index }

// slotLister is a base state that can list the slots of an account.
type slotLister interface {
	Slots(addr common.Address) ([]common.Hash, error)
}

// Slots returns every slot of addr that may be set at the end of the given
// block: the slots written after the base, and the slots of the base if it
// can list them.
func (h *History) Slots(addr common.Address, number uint64) ([]common.Hash, error) {
	slots, err := h.index.SlotsBefore(addr, number)
	if err != nil {
		return nil, err
	}
	if l, ok := h.base.(slotLister); ok {
		base, err := l.Slots(addr)
		if err != nil {
			return nil, err
		}
		slots = append(slots, base...)
	}
	return slots, nil
}

func (h *History) access(number uint64, addr common.Address) (*AccountAccess, error) {
	var list *BlockAccessList
	if v, ok := h.cache.Get(number); ok {
		list = v.(*BlockAccessList)
	} else {
		var err error
		if list, err = h.lists.Read(number); err != nil {
			return nil, fmt.Errorf("block %d: %w", number, err)
		}
		h.cache.Add(number, list)
	}
	i := sort.Search(len(list.Accounts), func(i int) bool {
		return bytes.Compare(list.Accounts[i].Address[:], addr[:]) >= 0
	})
	if i == len(list.Accounts) || list.Accounts[i].Address != addr {
		return nil, fmt.Errorf("%w: block %d, account %s", errHistoryMissingAccount, number, addr.Hex())
	}
	return &list.Accounts[i], nil
}

// latest returns the access of addr in the last block at or before number
// recorded under any of the given keys. a is nil if none is after the base.
func (h *History) latest(addr common.Address, number uint64, keys ...[]byte) (*AccountAccess, error) {
	if err := h.index.check(number); err != nil {
		return nil, err
	}
	var (
		last  uint64
		found bool
	)
	for _, key := range keys {
		block, ok, err := h.index.last(key, number)
		if err != nil {
			return nil, err
		}
		if ok && (!found || block > last) {
			last, found = block, true
		}
	}
	if !found {
		return nil, nil
	}
	return h.access(last, addr)
}

// whole returns the last whole account record that is not superseded, or nil.
func whole(a *AccountAccess, from uint32) (*AccountChange, error) {
	if n := len(a.AccountChanges); n > 0 && a.AccountChanges[n-1].Index >= from {
		return &a.AccountChanges[n-1], nil
	}
	return nil, nil
}

func decodeAccount(c *AccountChange) (account.Account, error) {
	ser := account.NewAccountSerializer()
	if err := rlp.DecodeBytes(c.Account, ser); err != nil {
		return nil, err
	}
	return ser.GetAccount(), nil
}

// field resolves a balance, nonce or code hash. last returns the index of the
// last change of the field in an access, and get reads the field from a whole
// account record or from the base account.
func (h *History) field(addr common.Address, number uint64, kind byte,
	last func(a *AccountAccess) (uint32, bool), fromChange func(a *AccountAccess), fromAccount func(acc account.Account),
) error {
	a, err := h.latest(addr, number, accountKey(kind, addr), accountKey(kindAccount, addr), accountKey(kindReset, addr))
	if err != nil {
		return err
	}
	if a == nil {
		acc, err := h.base.Account(addr)
		if err != nil {
			return err
		}
		fromAccount(acc)
		return nil
	}
	from, _, deleted := a.Reset()
	if deleted {
		fromAccount(nil)
		return nil
	}
	w, err := whole(a, from)
	if err != nil {
		return err
	}
	if idx, ok := last(a); ok && idx >= from && (w == nil || idx >= w.Index) {
		fromChange(a)
		return nil
	}
	if w == nil {
		return fmt.Errorf("%w: account %s", errHistoryInconsistent, addr.Hex())
	}
	acc, err := decodeAccount(w)
	if err != nil {
		return err
	}
	fromAccount(acc)
	return nil
}

// Balance returns the balance of addr at the end of the given block.
func (h *History) Balance(addr common.Address, number uint64) (*big.Int, error) {
	balance := new(big.Int)
	err := h.field(addr, number, kindBalance,
		func(a *AccountAccess) (uint32, bool) {
			n := len(a.BalanceChanges)
			return lastIndex(n, func() uint32 { return a.BalanceChanges[n-1].Index })
		},
		func(a *AccountAccess) { balance.Set(a.BalanceChanges[len(a.BalanceChanges)-1].Balance) },
		func(acc account.Account) {
			if acc != nil {
				balance.Set(acc.GetBalance())
			}
		})
	return balance, err
}

// Nonce returns the nonce of addr at the end of the given block.
func (h *History) Nonce(addr common.Address, number uint64) (uint64, error) {
	var nonce uint64
	err := h.field(addr, number, kindNonce,
		func(a *AccountAccess) (uint32, bool) {
			n := len(a.NonceChanges)
			return lastIndex(n, func() uint32 { return a.NonceChanges[n-1].Index })
		},
		func(a *AccountAccess) { nonce = a.NonceChanges[len(a.NonceChanges)-1].Nonce },
		func(acc account.Account) {
			if acc != nil {
				nonce = acc.GetNonce()
			}
		})
	return nonce, err
}

// Code returns the code of addr at the end of the given block.
func (h *History) Code(addr common.Address, number uint64) ([]byte, error) {
	var (
		code  []byte
		found bool
		hash  = emptyCodeHash
	)
	err := h.field(addr, number, kindCode,
		func(a *AccountAccess) (uint32, bool) {
			n := len(a.CodeChanges)
			return lastIndex(n, func() uint32 { return a.CodeChanges[n-1].Index })
		},
		func(a *AccountAccess) { code, found = a.CodeChanges[len(a.CodeChanges)-1].Code, true },
		func(acc account.Account) {
			if pa := account.GetProgramAccount(acc); pa != nil {
				hash = common.BytesToHash(pa.GetCodeHash())
			}
		})
	if err != nil || found {
		return code, err
	}
	return h.codeByHash(addr, number, hash)
}

// codeByHash finds the code with the given hash: in the last code change of
// addr, which a later whole account record leaves in place, or in the base.
func (h *History) codeByHash(addr common.Address, number uint64, hash common.Hash) ([]byte, error) {
	if hash == emptyCodeHash || hash == (common.Hash{}) {
		return nil, nil
	}
	a, err := h.latest(addr, number, accountKey(kindCode, addr))
	if err != nil {
		return nil, err
	}
	if a != nil {
		if code := a.CodeChanges[len(a.CodeChanges)-1].Code; crypto.Keccak256Hash(code) == hash {
			return code, nil
		}
	}
	return h.base.Code(hash)
}

// Storage returns the value of a storage slot of addr at the end of the given
// block.
func (h *History) Storage(addr common.Address, slot common.Hash, number uint64) (common.Hash, error) {
	a, err := h.latest(addr, number, slotKey(addr, slot), accountKey(kindReset, addr))
	if err != nil {
		return common.Hash{}, err
	}
	if a == nil {
		return h.base.Storage(addr, slot)
	}
	from, cleared, deleted := a.Reset()
	if deleted {
		return common.Hash{}, nil
	}
	i := sort.Search(len(a.StorageWrites), func(i int) bool {
		return bytes.Compare(a.StorageWrites[i].Slot[:], slot[:]) >= 0
	})
	if i < len(a.StorageWrites) && a.StorageWrites[i].Slot == slot {
		if c := a.StorageWrites[i].Changes; c[len(c)-1].Index >= from {
			return c[len(c)-1].Value, nil
		}
	}
	if cleared {
		return common.Hash{}, nil
	}
	return common.Hash{}, fmt.Errorf("%w: account %s slot %x", errHistoryInconsistent, addr.Hex(), slot)
}

// Account returns the account at the end of the given block, or nil if it does
// not exist. Its storage root is not tracked and must not be used.
func (h *History) Account(addr common.Address, number uint64) (account.Account, error) {
	a, err := h.latest(addr, number, accountKey(kindAccount, addr), accountKey(kindReset, addr))
	if err != nil {
		return nil, err
	}
	var acc account.Account
	if a == nil {
		if acc, err = h.base.Account(addr); err != nil {
			return nil, err
		}
	} else {
		from, _, deleted := a.Reset()
		if deleted {
			return nil, nil
		}
		w, err := whole(a, from)
		if err != nil {
			return nil, err
		}
		if w == nil {
			return nil, fmt.Errorf("%w: account %s", errHistoryInconsistent, addr.Hex())
		}
		if acc, err = decodeAccount(w); err != nil {
			return nil, err
		}
	}
	if acc == nil {
		return nil, nil
	}
	balance, err := h.Balance(addr, number)
	if err != nil {
		return nil, err
	}
	nonce, err := h.Nonce(addr, number)
	if err != nil {
		return nil, err
	}
	acc.SetBalance(balance)
	acc.SetNonce(nonce)
	if pa := account.GetProgramAccount(acc); pa != nil {
		code, err := h.Code(addr, number)
		if err != nil {
			return nil, err
		}
		pa.SetCodeHash(crypto.Keccak256(code))
	}
	return acc, nil
}

func lastIndex(n int, last func() uint32) (uint32, bool) {
	if n == 0 {
		return 0, false
	}
	return last(), true
}
