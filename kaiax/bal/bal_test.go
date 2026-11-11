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

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/stretchr/testify/require"
)

func TestBuilder(t *testing.T) {
	var (
		a     = common.Address{0xa}
		b     = common.Address{0xb}
		slot1 = common.Hash{1}
		slot2 = common.Hash{2}
	)
	builder := NewBuilder()
	builder.ReadAccount(b)
	builder.ReadSlot(b, slot2)
	builder.ReadSlot(b, slot1)

	builder.SetBalance(a, 0, big.NewInt(1))
	builder.SetBalance(a, 1, big.NewInt(2))
	builder.SetBalance(a, 1, big.NewInt(3)) // same phase: replaces
	builder.WriteSlot(a, 1, slot1, common.Hash{0x11})
	builder.ReadSlot(a, slot1) // written slots are not listed as reads

	// Deletion drops what the same phase wrote before it.
	builder.WriteSlot(a, 2, slot2, common.Hash{0x22})
	builder.SetNonce(a, 2, 5)
	builder.Delete(a, 2)
	// Recreation later in the same phase is kept.
	builder.ClearStorage(a, 2)
	builder.SetAccount(a, 2, []byte{0x01})
	builder.WriteSlot(a, 2, slot2, common.Hash{0x23})

	list := builder.Build()
	require.NoError(t, list.Validate())
	require.Len(t, list.Accounts, 2)

	acc := list.Accounts[0]
	require.Equal(t, a, acc.Address)
	require.Equal(t, []BalanceChange{{0, big.NewInt(1)}, {1, big.NewInt(3)}}, acc.BalanceChanges)
	require.Empty(t, acc.NonceChanges)
	require.Equal(t, []uint32{2}, acc.Deleted)
	require.Equal(t, []uint32{2}, acc.StorageCleared)
	require.Equal(t, []SlotWrites{
		{slot1, []SlotChange{{1, common.Hash{0x11}}}},
		{slot2, []SlotChange{{2, common.Hash{0x23}}}},
	}, acc.StorageWrites)
	require.Empty(t, acc.StorageReads)
	last, ok := acc.LastWriteIndex()
	require.True(t, ok)
	require.Equal(t, uint32(2), last)

	acc = list.Accounts[1]
	require.False(t, acc.Changed())
	require.Equal(t, []common.Hash{slot1, slot2}, acc.StorageReads)

	enc, err := list.Encode()
	require.NoError(t, err)
	dec, err := Decode(enc)
	require.NoError(t, err)
	reenc, err := dec.Encode()
	require.NoError(t, err)
	require.Equal(t, enc, reenc)
}

func TestDecodeRejectsNonCanonical(t *testing.T) {
	for name, list := range map[string]*BlockAccessList{
		"accounts": {Accounts: []AccountAccess{{Address: common.Address{2}}, {Address: common.Address{1}}}},
		"slots":    {Accounts: []AccountAccess{{StorageReads: []common.Hash{{2}, {1}}}}},
		"indices":  {Accounts: []AccountAccess{{NonceChanges: []NonceChange{{1, 1}, {1, 2}}}}},
		"empty":    {Accounts: []AccountAccess{{StorageWrites: []SlotWrites{{Slot: common.Hash{1}}}}}},
	} {
		enc, err := list.Encode()
		require.NoError(t, err)
		_, err = Decode(enc)
		require.Error(t, err, name)
	}
}

// TestStorageRootOptional checks that a list without storage roots, as recorded
// before they were added, still decodes, and that a storage root without a
// storage change is rejected.
func TestStorageRootOptional(t *testing.T) {
	addr := common.HexToAddress("0x1")
	list := &BlockAccessList{Accounts: []AccountAccess{{
		Address:       addr,
		StorageWrites: []SlotWrites{{Slot: common.Hash{1}, Changes: []SlotChange{{Index: 1, Value: common.Hash{2}}}}},
	}}}
	enc, err := list.Encode()
	require.NoError(t, err)
	decoded, err := Decode(enc)
	require.NoError(t, err)
	require.Nil(t, decoded.Accounts[0].StorageRoot)

	root := common.Hash{3}
	list.Accounts[0].StorageRoot = &root
	enc, err = list.Encode()
	require.NoError(t, err)
	decoded, err = Decode(enc)
	require.NoError(t, err)
	require.Equal(t, root, *decoded.Accounts[0].StorageRoot)

	list.Accounts[0].StorageWrites = nil
	list.Accounts[0].BalanceChanges = []BalanceChange{{Index: 1, Balance: common.Big1}}
	enc, err = list.Encode()
	require.NoError(t, err)
	_, err = Decode(enc)
	require.ErrorIs(t, err, errNoStorageChange)
}

// TestWritesHash checks that Writes drops the reads and the storage roots,
// and that the hash covers the writes only: lists that differ in reads have
// the same hash, while a different write changes it.
func TestWritesHash(t *testing.T) {
	root := common.Hash{9}
	list := func() *BlockAccessList {
		return &BlockAccessList{Accounts: []AccountAccess{
			{
				Address:        common.HexToAddress("0x1"),
				StorageWrites:  []SlotWrites{{Slot: common.Hash{1}, Changes: []SlotChange{{Index: 1, Value: common.Hash{2}}}}},
				StorageReads:   []common.Hash{{3}},
				BalanceChanges: []BalanceChange{{Index: 1, Balance: big.NewInt(5)}},
				StorageRoot:    &root,
			},
			{Address: common.HexToAddress("0x2"), StorageReads: []common.Hash{{4}}}, // only read
		}}
	}
	full := list()
	writes := full.Writes()
	require.Len(t, writes.Accounts, 1)
	require.Equal(t, common.HexToAddress("0x1"), writes.Accounts[0].Address)
	require.Nil(t, writes.Accounts[0].StorageReads)
	require.Nil(t, writes.Accounts[0].StorageRoot)
	require.NotNil(t, full.Accounts[0].StorageRoot, "Writes leaves the list as it is")
	require.Len(t, full.Accounts, 2)

	hash, err := full.Hash()
	require.NoError(t, err)
	enc, err := writes.Encode()
	require.NoError(t, err)
	require.Equal(t, crypto.Keccak256Hash(enc), hash)
	decoded, err := Decode(enc)
	require.NoError(t, err)
	writesHash, err := decoded.Hash()
	require.NoError(t, err)
	require.Equal(t, hash, writesHash, "the writes alone have the same hash")

	otherReads := list()
	otherReads.Accounts[0].StorageReads = []common.Hash{{7}, {8}}
	otherReads.Accounts[0].StorageRoot = nil
	otherReads.Accounts = append(otherReads.Accounts, AccountAccess{Address: common.HexToAddress("0x3")})
	require.NoError(t, otherReads.Validate())
	otherHash, err := otherReads.Hash()
	require.NoError(t, err)
	require.Equal(t, hash, otherHash, "reads and storage roots are not hashed")

	otherWrite := list()
	otherWrite.Accounts[0].StorageWrites[0].Changes[0].Value = common.Hash{6}
	otherHash, err = otherWrite.Hash()
	require.NoError(t, err)
	require.NotEqual(t, hash, otherHash)
	otherBalance := list()
	otherBalance.Accounts[0].BalanceChanges[0].Balance = big.NewInt(6)
	otherHash, err = otherBalance.Hash()
	require.NoError(t, err)
	require.NotEqual(t, hash, otherHash)
}

// TestReadsCombine splits a list into its writes and its reads and puts them
// back together, and checks that the encoded writes hash to Hash.
func TestReadsCombine(t *testing.T) {
	builder := NewBuilder()
	builder.ReadAccount(common.Address{1}) // only read
	builder.ReadSlot(common.Address{2}, common.Hash{5})
	builder.WriteSlot(common.Address{2}, 1, common.Hash{6}, common.Hash{7})
	builder.SetBalance(common.Address{3}, 1, big.NewInt(3)) // written, no reads
	builder.ReadSlot(common.Address{4}, common.Hash{8})     // only read, with a slot
	full := builder.Build()
	require.Len(t, full.Accounts, 4)
	fullEnc, err := full.Encode()
	require.NoError(t, err)

	writes, reads := full.Writes(), full.Reads()
	require.Len(t, writes.Accounts, 2)
	require.Equal(t, []AccountReads{
		{Address: common.Address{1}},
		{Address: common.Address{2}, Slots: []common.Hash{{5}}},
		{Address: common.Address{4}, Slots: []common.Hash{{8}}},
	}, reads.Accounts)

	writesEnc, err := writes.Encode()
	require.NoError(t, err)
	hash, err := full.Hash()
	require.NoError(t, err)
	require.Equal(t, hash, crypto.Keccak256Hash(writesEnc), "the stored writes hash to the BAL hash")

	readsEnc, err := reads.Encode()
	require.NoError(t, err)
	decodedWrites, err := Decode(writesEnc)
	require.NoError(t, err)
	decodedReads, err := DecodeReads(readsEnc)
	require.NoError(t, err)
	combined, err := Combine(decodedWrites, decodedReads)
	require.NoError(t, err)
	combinedEnc, err := combined.Encode()
	require.NoError(t, err)
	require.Equal(t, fullEnc, combinedEnc)

	none, err := Combine(writes, &Reads{})
	require.NoError(t, err)
	noneEnc, err := none.Encode()
	require.NoError(t, err)
	require.Equal(t, writesEnc, noneEnc)

	_, err = Combine(full, reads)
	require.ErrorIs(t, err, errReadsInWrites)
	unsorted := &Reads{Accounts: []AccountReads{{Address: common.Address{2}}, {Address: common.Address{1}}}}
	enc, err := unsorted.Encode()
	require.NoError(t, err)
	_, err = DecodeReads(enc)
	require.ErrorIs(t, err, errUnsortedAccounts)
}
