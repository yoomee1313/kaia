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

package bas

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

	set := builder.Build()
	require.NoError(t, set.Validate())
	require.Len(t, set.Accounts, 2)

	acc := set.Accounts[0]
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

	acc = set.Accounts[1]
	require.False(t, acc.Changed())
	require.Equal(t, []common.Hash{slot1, slot2}, acc.StorageReads)

	enc, err := set.Encode()
	require.NoError(t, err)
	dec, err := Decode(enc)
	require.NoError(t, err)
	reenc, err := dec.Encode()
	require.NoError(t, err)
	require.Equal(t, enc, reenc)
}

func TestDecodeRejectsNonCanonical(t *testing.T) {
	for name, set := range map[string]*BlockAccessSet{
		"accounts": {Accounts: []AccountAccess{{Address: common.Address{2}}, {Address: common.Address{1}}}},
		"slots":    {Accounts: []AccountAccess{{StorageReads: []common.Hash{{2}, {1}}}}},
		"indices":  {Accounts: []AccountAccess{{NonceChanges: []NonceChange{{1, 1}, {1, 2}}}}},
		"empty":    {Accounts: []AccountAccess{{StorageWrites: []SlotWrites{{Slot: common.Hash{1}}}}}},
	} {
		enc, err := set.Encode()
		require.NoError(t, err)
		_, err = Decode(enc)
		require.Error(t, err, name)
	}
}

func TestFreezer(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenFreezer(dir, false)
	require.NoError(t, err)
	require.Equal(t, uint64(1), f.Next()) // the genesis set

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
	set, err := f.Read(1)
	require.NoError(t, err)
	require.Equal(t, big.NewInt(7), set.Accounts[0].BalanceChanges[0].Balance)
	genesis, err := f.Read(0)
	require.NoError(t, err)
	require.Empty(t, genesis.Accounts)
}
