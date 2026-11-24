// Modifications Copyright 2024 The Kaia Authors
// Modifications Copyright 2018 The klaytn Authors
// Copyright 2017 The go-ethereum Authors
// This file is part of go-ethereum.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
//
// This file is derived from eth/api_test.go (2018/06/04).
// Modified and improved for the klaytn development.
// Modified and improved for the Kaia development.
package cn

import (
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

var dumper = spew.ConfigState{Indent: "    "}

func TestStorageRangeAt(t *testing.T) {
	// Create a state where account 0x010000... has a few storage entries.
	var (
		state, _ = state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil, nil)
		addr     = common.Address{0x01}
		keys     = []common.Hash{ // hashes of Keys of storage
			common.HexToHash("340dd630ad21bf010b4e676dbfa9ba9a02175262d1fa356232cfde6cb5b47ef2"),
			common.HexToHash("426fcb404ab2d5d8e61a3d918108006bbb0a9be65e92235bb10eefbdb6dcd053"),
			common.HexToHash("48078cfed56339ea54962e72c37c7f588fc4f8e5bc173827ba75cb10a63a96a5"),
			common.HexToHash("5723d2c3a83af9b735e3b7f21531e5623d183a9095a56604ead41f3582fdfb75"),
		}
		storage = storageMap{
			keys[0]: {Key: &common.Hash{0x02}, Value: common.Hash{0x01}},
			keys[1]: {Key: &common.Hash{0x04}, Value: common.Hash{0x02}},
			keys[2]: {Key: &common.Hash{0x01}, Value: common.Hash{0x03}},
			keys[3]: {Key: &common.Hash{0x03}, Value: common.Hash{0x04}},
		}
	)
	for _, entry := range storage {
		state.SetState(addr, *entry.Key, entry.Value)
	}

	// Check a few combinations of limit and start/end.
	tests := []struct {
		start []byte
		limit int
		want  StorageRangeResult
	}{
		{
			start: []byte{}, limit: 0,
			want: StorageRangeResult{storageMap{}, &keys[0]},
		},
		{
			start: []byte{}, limit: 100,
			want: StorageRangeResult{storage, nil},
		},
		{
			start: []byte{}, limit: 2,
			want: StorageRangeResult{storageMap{keys[0]: storage[keys[0]], keys[1]: storage[keys[1]]}, &keys[2]},
		},
		{
			start: []byte{0x00}, limit: 4,
			want: StorageRangeResult{storage, nil},
		},
		{
			start: []byte{0x40}, limit: 2,
			want: StorageRangeResult{storageMap{keys[1]: storage[keys[1]], keys[2]: storage[keys[2]]}, &keys[3]},
		},
	}
	for _, test := range tests {
		result, err := storageRangeAt(state.StorageTrie(addr), test.start, test.limit)
		if err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(result, test.want) {
			t.Fatalf("wrong result for range 0x%x.., limit %d:\ngot %s\nwant %s",
				test.start, test.limit, dumper.Sdump(result), dumper.Sdump(&test.want))
		}
	}
}

// slotReader serves the account and the storage of one contract, read from
// a state, and lists its slots if lists is set, like the BAL history.
type slotReader struct {
	addr  common.Address
	acc   account.Account
	slots map[common.Hash]common.Hash
	lists bool
}

func (r slotReader) Account(addr common.Address) (account.Account, error) {
	if addr != r.addr {
		return nil, nil
	}
	return r.acc.DeepCopy(), nil
}

func (r slotReader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	return r.slots[slot], nil
}

func (r slotReader) Code(common.Address, common.Hash) ([]byte, error)  { return nil, nil }
func (r slotReader) CodeSize(common.Address, common.Hash) (int, error) { return 0, nil }

type listingReader struct{ slotReader }

func (r listingReader) Slots(addr common.Address) ([]common.Hash, error) {
	var slots []common.Hash
	for slot := range r.slots {
		slots = append(slots, slot)
	}
	return slots, nil
}

// TestStorageRangeAtReadOnly pages through the storage of a contract of a
// read-only state, like one read from the BAL history, and gets what the
// storage trie of the same storage gives, with every key: the slots the
// reader lists that are set, and the slots written in memory.
func TestStorageRangeAtReadOnly(t *testing.T) {
	db := state.NewDatabase(database.NewMemoryDBManager())
	addr := common.Address{0x01}
	slots := map[common.Hash]common.Hash{}
	base, err := state.New(common.Hash{}, db, nil, nil)
	require.NoError(t, err)
	require.NoError(t, base.SetCode(addr, []byte{1}))
	for i := byte(1); i <= 6; i++ {
		slots[common.Hash{i}] = common.Hash{0x10 + i}
		base.SetState(addr, common.Hash{i}, common.Hash{0x10 + i})
	}
	slots[common.Hash{7}] = common.Hash{} // listed but not set
	root, err := base.Commit(true)
	require.NoError(t, err)
	acc := base.GetAccount(addr)

	// The same writes in memory, on the trie and on a read-only state.
	write := func(sdb *state.StateDB) {
		sdb.SetState(addr, common.Hash{1}, common.Hash{})
		sdb.SetState(addr, common.Hash{8}, common.Hash{0x18})
	}
	onTrie, err := state.New(root, db, nil, nil)
	require.NoError(t, err)
	write(onTrie)
	onTrie.IntermediateRoot(true)
	ro := state.NewReadOnly(root, db, listingReader{slotReader{addr: addr, acc: acc, slots: slots}})
	write(ro)

	var want, got []StorageRangeResult
	for _, r := range []struct {
		pages *[]StorageRangeResult
		next  func(start []byte) (StorageRangeResult, error)
	}{
		{&want, func(start []byte) (StorageRangeResult, error) {
			return storageRangeAt(onTrie.StorageTrie(addr), start, 2)
		}},
		{&got, func(start []byte) (StorageRangeResult, error) { return readOnlyStorageRangeAt(ro, addr, start, 2) }},
	} {
		var start []byte
		for {
			page, err := r.next(start)
			require.NoError(t, err)
			*r.pages = append(*r.pages, page)
			if page.NextKey == nil {
				break
			}
			start = page.NextKey[:]
		}
	}
	require.Len(t, got, len(want))
	count := 0
	for i := range want {
		require.Len(t, got[i].Storage, len(want[i].Storage), "page %d", i)
		for hash, e := range want[i].Storage {
			g, ok := got[i].Storage[hash]
			require.True(t, ok, "page %d slot %x", i, hash)
			require.Equal(t, e.Value, g.Value)
			require.NotNil(t, g.Key)
			require.Equal(t, hash, crypto.Keccak256Hash(g.Key[:]))
			count++
		}
		require.Equal(t, want[i].NextKey, got[i].NextKey, "page %d", i)
	}
	require.Equal(t, 6, count, "slots 2-6 and 8")

	// A reader that cannot list slots cannot be iterated.
	plain := state.NewReadOnly(root, db, slotReader{addr: addr, acc: acc, slots: slots})
	_, err = readOnlyStorageRangeAt(plain, addr, nil, 10)
	require.ErrorIs(t, err, state.ErrNoIteration)
	_, err = readOnlyStorageRangeAt(ro, common.Address{0x02}, nil, 10)
	require.ErrorContains(t, err, "doesn't exist")
}
