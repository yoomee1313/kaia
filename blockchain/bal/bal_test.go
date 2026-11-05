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

func TestFreezer(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenFreezer(dir, false)
	require.NoError(t, err)
	require.Equal(t, uint64(1), f.Next()) // the genesis list

	builder := NewBuilder()
	builder.SetBalance(common.Address{1}, 1, big.NewInt(7))
	enc, err := builder.Build().Encode()
	require.NoError(t, err)
	require.NoError(t, f.Append(1, enc))
	require.ErrorIs(t, f.Append(3, enc), ErrFreezerGap)
	require.ErrorIs(t, f.Append(1, enc), ErrFreezerGap)
	require.NoError(t, f.Close())

	f, err = OpenFreezer(dir, true)
	require.NoError(t, err)
	defer f.Close()
	require.Equal(t, uint64(2), f.Next())
	list, err := f.Read(1)
	require.NoError(t, err)
	require.Equal(t, big.NewInt(7), list.Accounts[0].BalanceChanges[0].Balance)
	genesis, err := f.Read(0)
	require.NoError(t, err)
	require.Empty(t, genesis.Accounts)
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

// TestCheckHash checks that the hash covers the writes only: lists that differ
// in reads or storage roots have the same hash, and so has the list without
// its reads, while a different write is rejected.
func TestCheckHash(t *testing.T) {
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
	hash, err := full.Hash()
	require.NoError(t, err)
	require.NoError(t, full.CheckHash(nil))
	require.NoError(t, full.CheckHash(&hash))

	otherReads := list()
	otherReads.Accounts[0].StorageReads = []common.Hash{{7}, {8}}
	otherReads.Accounts[0].StorageRoot = nil
	otherReads.Accounts = append(otherReads.Accounts, AccountAccess{Address: common.HexToAddress("0x3")})
	require.NoError(t, otherReads.Validate())
	require.NoError(t, otherReads.CheckHash(&hash), "reads and storage roots are not hashed")

	writes := full.Writes()
	require.Len(t, writes.Accounts, 1)
	require.Nil(t, writes.Accounts[0].StorageReads)
	require.Nil(t, writes.Accounts[0].StorageRoot)
	require.NotNil(t, full.Accounts[0].StorageRoot, "Writes leaves the list as it is")
	enc, err := writes.Encode()
	require.NoError(t, err)
	decoded, err := Decode(enc)
	require.NoError(t, err)
	require.NoError(t, decoded.CheckHash(&hash), "the writes alone pass the header check")

	otherWrite := list()
	otherWrite.Accounts[0].StorageWrites[0].Changes[0].Value = common.Hash{6}
	require.ErrorIs(t, otherWrite.CheckHash(&hash), ErrHashMismatch)
	otherBalance := list()
	otherBalance.Accounts[0].BalanceChanges[0].Balance = big.NewInt(6)
	require.ErrorIs(t, otherBalance.CheckHash(&hash), ErrHashMismatch)
}

// TestEntryCountAndStats checks that the builder counts the entries as the list
// does, reads and writes of a slot once, and bounds the encoded size.
func TestEntryCountAndStats(t *testing.T) {
	var (
		a1    = common.HexToAddress("0x01")
		a2    = common.HexToAddress("0x02")
		a3    = common.HexToAddress("0x03")
		s1    = common.HexToHash("0x11")
		s2    = common.HexToHash("0x12")
		s3    = common.HexToHash("0x13")
		value = common.HexToHash("0xff")
	)
	b := NewBuilder()
	b.ReadAccount(a1)
	b.ReadSlot(a1, s1)
	b.ReadSlot(a1, s2)
	b.WriteSlot(a1, 1, s2, value) // read and written: one entry
	b.WriteSlot(a1, 2, s2, value)
	b.WriteSlot(a2, 1, s3, value)
	b.SetBalance(a2, 1, new(big.Int).Lsh(common.Big1, 255))
	b.SetNonce(a3, 2, 7)
	b.SetCode(a3, 2, make([]byte, 1000))
	b.SetAccount(a3, 2, make([]byte, 100))
	b.Delete(a2, 3)
	b.ClearStorage(a3, 3)

	list := b.Build()
	require.Equal(t, uint64(3+3), list.EntryCount(), "3 accounts, slots s1 and s2 of a1, s3 of a2")
	entries, size := b.Stats()
	require.Equal(t, list.EntryCount(), entries)
	enc, err := list.Encode()
	require.NoError(t, err)
	require.GreaterOrEqual(t, size, uint64(len(enc)))
	require.Less(t, size, uint64(2*len(enc)), "the bound is not far off")

	has, hasSlot := b.Has(a1, &s1)
	require.True(t, has)
	require.True(t, hasSlot)
	_, hasSlot = b.Has(a1, &s3)
	require.False(t, hasSlot)
	has, _ = b.Has(common.HexToAddress("0x04"), nil)
	require.False(t, has)
}
