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
	"errors"
	"fmt"
	"sync"

	"github.com/kaiachain/kaia/blockchain/state/bal"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
)

// ErrRebuilt is returned by a state read from the BAL history after its index
// was rebuilt.
var ErrRebuilt = errors.New("the BAL index was rebuilt, open the state again")

// balStorageRoot is set on the contract accounts of a state without a state
// trie: their storage is read by address from the history and has no root.
var balStorageRoot = common.HexToHash("0xbabababababababababababababababababababababababababababababababa").ExtendZero()

// OpenBALState opens the state with root from the BAL history of a. A value is
// the one set by the last list at or before the block that changed it; the rest
// is read from the state trie of the block in trie if it is there, and the
// state hashes, commits and proves on that trie. Otherwise the rest is read from
// the state the history starts from, and the state is read-only. It returns
// bal.ErrNotCovered if root is not the root of a canonical indexed block.
func OpenBALState(a *bal.Archive, root common.Hash, trie Database) (*StateDB, error) {
	ix, number, valid, err := a.Locate(root)
	if err != nil {
		return nil, err
	}
	if trie != nil {
		if sdb, err := New(root, trie, nil, nil); err == nil {
			accounts, err := trie.OpenTrie(root, nil)
			if err != nil {
				return nil, err
			}
			h := a.HistoryOn(ix, NewBALBase(sdb))
			return NewWithReader(root, trie, newHistoryReader(h, number, valid, trie, accounts))
		}
	}
	h, err := a.HistoryFromBase(ix)
	if err != nil {
		return nil, err
	}
	return NewWithReader(root, NewReadOnlyDatabase(root), newHistoryReader(h, number, valid, nil, nil))
}

// historyReader reads the state at the end of one block from a BAL history.
type historyReader struct {
	history *bal.History
	number  uint64
	valid   func() bool // reports whether the history is still usable

	trie     Database // the state trie of the block, nil without it
	mu       sync.Mutex
	accounts Trie // the account trie of the block, for storage roots
}

func newHistoryReader(h *bal.History, number uint64, valid func() bool, trie Database, accounts Trie) *historyReader {
	return &historyReader{history: h, number: number, valid: valid, trie: trie, accounts: accounts}
}

func (r *historyReader) check() error {
	if r.valid != nil && !r.valid() {
		return ErrRebuilt
	}
	return nil
}

func (r *historyReader) Account(addr common.Address) (account.Account, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	acc, err := r.history.Account(addr, r.number)
	if err != nil || acc == nil {
		return nil, err
	}
	acc = acc.DeepCopy()
	if pa := account.GetProgramAccount(acc); pa != nil {
		root, err := r.storageRoot(addr)
		if err != nil {
			return nil, err
		}
		pa.SetStorageRoot(root)
	}
	return acc, nil
}

// storageRoot returns the storage root to set on a contract account: the one
// in the state trie of the block, or a placeholder without the trie.
func (r *historyReader) storageRoot(addr common.Address) (common.ExtHash, error) {
	if r.trie == nil {
		return balStorageRoot, nil
	}
	r.mu.Lock()
	enc, err := r.accounts.TryGet(addr[:])
	r.mu.Unlock()
	if err != nil {
		return common.ExtHash{}, err
	}
	if enc == nil {
		return common.ExtHash{}, fmt.Errorf("the BAL history has account %s, which the state trie of block %d has not", addr.Hex(), r.number)
	}
	ser := account.NewAccountSerializer()
	if err := rlp.DecodeBytes(enc, ser); err != nil {
		return common.ExtHash{}, err
	}
	pa := account.GetProgramAccount(ser.GetAccount())
	if pa == nil {
		return common.ExtHash{}, fmt.Errorf("account %s is a contract in the BAL history, not in the state trie of block %d", addr.Hex(), r.number)
	}
	return pa.GetStorageRoot(), nil
}

func (r *historyReader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	if err := r.check(); err != nil {
		return common.Hash{}, err
	}
	return r.history.Storage(addr, slot, r.number)
}

func (r *historyReader) Code(addr common.Address, codeHash common.Hash) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	if codeHash == types.EmptyCodeHash {
		return nil, nil
	}
	code, err := r.history.Code(addr, r.number)
	if err == nil && crypto.Keccak256Hash(code) == codeHash {
		return code, nil
	}
	if r.trie != nil {
		return r.trie.ContractCode(codeHash)
	}
	if err != nil {
		return nil, err
	}
	return nil, errors.New("code not loaded from the BAL history: " + codeHash.Hex())
}
