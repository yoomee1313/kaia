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
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

type listMap map[uint64]*access.BlockAccessList

func (m listMap) Read(number uint64) (*access.BlockAccessList, error) { return m[number], nil }

// TestDatabaseOnTrie reads a value a list changed from the list even where the
// state trie of the block disagrees, reads the rest from the trie, and passes
// writes, hashes, commits and proofs on to the trie.
func TestDatabaseOnTrie(t *testing.T) {
	changed, untouched := common.Address{0x01}, common.Address{0x02}
	trie := state.NewDatabase(database.NewMemoryDBManager())
	sdb, err := state.New(common.Hash{}, trie, nil, nil)
	require.NoError(t, err)
	sdb.SetBalance(changed, big.NewInt(7))
	sdb.SetBalance(untouched, big.NewInt(3))
	root, err := sdb.Commit(true)
	require.NoError(t, err)
	require.NoError(t, trie.TrieDB().Commit(root, false, 0))

	ixdb := database.NewMemDB()
	builder, err := NewIndexBuilder(ixdb, 0, common.Hash{})
	require.NoError(t, err)
	lists := listMap{1: {Accounts: []access.AccountAccess{{Address: changed, BalanceChanges: []access.BalanceChange{{Index: 1, Balance: big.NewInt(5)}}}}}}
	require.NoError(t, builder.Add(1, lists[1]))
	builder.AddRoot(1, root)
	require.NoError(t, builder.Flush())
	ix, err := OpenIndex(ixdb)
	require.NoError(t, err)

	open := func() (*state.StateDB, *Database) {
		base, err := state.New(root, trie, nil, nil)
		require.NoError(t, err)
		db, err := NewDatabaseOnTrie(NewHistory(ix, lists, &lockedBase{base: access.NewStateBase(base)}), 1, root, trie, nil)
		require.NoError(t, err)
		require.False(t, ReadOnly(db))
		s, err := state.New(root, db, nil, nil)
		require.NoError(t, err)
		return s, db
	}
	s, _ := open()
	require.Equal(t, big.NewInt(5), s.GetBalance(changed), "the list first")
	require.Equal(t, big.NewInt(3), s.GetBalance(untouched), "the trie for what no list changed")
	require.NoError(t, s.Error())
	require.Equal(t, root, s.IntermediateRoot(true))

	want, err := state.New(root, trie, nil, nil)
	require.NoError(t, err)
	want.SetBalance(untouched, big.NewInt(4))
	s.SetBalance(untouched, big.NewInt(4))
	require.Equal(t, want.IntermediateRoot(true), s.IntermediateRoot(true))
	got, err := s.Commit(true)
	require.NoError(t, err)
	require.Equal(t, want.IntermediateRoot(true), got)

	_, db := open()
	accounts, err := db.OpenTrie(root, nil)
	require.NoError(t, err)
	require.NoError(t, accounts.Prove(untouched[:], 0, database.NewMemoryDBManager()))
}
