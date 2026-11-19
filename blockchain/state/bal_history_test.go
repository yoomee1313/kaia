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
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/state/bal"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

type listMap map[uint64]*bal.BlockAccessList

func (m listMap) Read(number uint64) (*bal.BlockAccessList, error) { return m[number], nil }

// TestBALStateOnTrie checks that a state read from the BAL history over the
// state trie of its block reads a value a list changed from the list even
// where the trie disagrees, reads the rest from the trie, and hashes, commits
// and proves on the trie.
func TestBALStateOnTrie(t *testing.T) {
	changed, untouched := common.Address{0x01}, common.Address{0x02}
	trie := NewDatabase(database.NewMemoryDBManager())
	sdb, err := New(common.Hash{}, trie, nil, nil)
	require.NoError(t, err)
	sdb.SetBalance(changed, big.NewInt(7))
	sdb.SetBalance(untouched, big.NewInt(3))
	root, err := sdb.Commit(true)
	require.NoError(t, err)
	require.NoError(t, trie.TrieDB().Commit(root, false, 0))

	ixdb := database.NewMemDB()
	builder, err := bal.NewIndexBuilder(ixdb, 0, common.Hash{})
	require.NoError(t, err)
	lists := listMap{1: {Accounts: []bal.AccountAccess{{Address: changed, BalanceChanges: []bal.BalanceChange{{Index: 1, Balance: big.NewInt(5)}}}}}}
	require.NoError(t, builder.Add(1, lists[1]))
	builder.AddRoot(1, root)
	require.NoError(t, builder.Flush())
	ix, err := bal.OpenIndex(ixdb)
	require.NoError(t, err)

	open := func() *StateDB {
		base, err := New(root, trie, nil, nil)
		require.NoError(t, err)
		accounts, err := trie.OpenTrie(root, nil)
		require.NoError(t, err)
		h := bal.NewHistory(ix, lists, NewBALBase(base))
		s, err := NewWithReader(root, trie, newHistoryReader(h, 1, nil, trie, accounts))
		require.NoError(t, err)
		require.False(t, ReadOnly(s.Database()))
		return s
	}
	s := open()
	require.Equal(t, big.NewInt(5), s.GetBalance(changed), "the list first")
	require.Equal(t, big.NewInt(3), s.GetBalance(untouched), "the trie for what no list changed")
	require.NoError(t, s.Error())
	require.Equal(t, root, s.IntermediateRoot(true))

	want, err := New(root, trie, nil, nil)
	require.NoError(t, err)
	want.SetBalance(untouched, big.NewInt(4))
	s.SetBalance(untouched, big.NewInt(4))
	require.Equal(t, want.IntermediateRoot(true), s.IntermediateRoot(true))
	got, err := s.Commit(true)
	require.NoError(t, err)
	require.Equal(t, want.IntermediateRoot(true), got)

	accounts, err := open().Database().OpenTrie(root, nil)
	require.NoError(t, err)
	require.NoError(t, accounts.Prove(untouched[:], 0, database.NewMemoryDBManager()))
}
