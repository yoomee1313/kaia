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

package types

import (
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
	"github.com/stretchr/testify/require"
)

func TestDecodeRejectsNonCanonical(t *testing.T) {
	for name, list := range map[string]*BlockAccessList{
		"accounts": {Accounts: []AccountChanges{{Address: common.Address{2}}, {Address: common.Address{1}}}},
		"slots":    {Accounts: []AccountChanges{{StorageReads: []common.Hash{{2}, {1}}}}},
		"indices":  {Accounts: []AccountChanges{{NonceChanges: []NonceChange{{1, 1}, {1, 2}}}}},
		"empty":    {Accounts: []AccountChanges{{StorageChanges: []SlotChanges{{Slot: common.Hash{1}}}}}},
	} {
		enc, err := list.Encode()
		require.NoError(t, err)
		_, err = DecodeBAL(enc)
		require.Error(t, err, name)
	}
}

// TestStorageRootOptional checks that a list without storage roots, as recorded
// before they were added, still decodes, and that a storage root without a
// storage change is rejected.
func TestStorageRootOptional(t *testing.T) {
	addr := common.HexToAddress("0x1")
	list := &BlockAccessList{Accounts: []AccountChanges{{
		Address:        addr,
		StorageChanges: []SlotChanges{{Slot: common.Hash{1}, Changes: []StorageChange{{BlockAccessIndex: 1, NewValue: common.Hash{2}}}}},
	}}}
	enc, err := list.Encode()
	require.NoError(t, err)
	decoded, err := DecodeBAL(enc)
	require.NoError(t, err)
	require.Nil(t, decoded.Accounts[0].StorageRoot)

	root := common.Hash{3}
	list.Accounts[0].StorageRoot = &root
	enc, err = list.Encode()
	require.NoError(t, err)
	decoded, err = DecodeBAL(enc)
	require.NoError(t, err)
	require.Equal(t, root, *decoded.Accounts[0].StorageRoot)

	list.Accounts[0].StorageChanges = nil
	list.Accounts[0].BalanceChanges = []BalanceChange{{BlockAccessIndex: 1, PostBalance: common.Big1}}
	enc, err = list.Encode()
	require.NoError(t, err)
	_, err = DecodeBAL(enc)
	require.ErrorIs(t, err, errNoStorageChange)
}

// TestWritesAndReads splits a list into its writes and its reads and puts them
// back together, and checks that the hash covers the encoded writes only.
func TestWritesAndReads(t *testing.T) {
	list := func() *BlockAccessList {
		return &BlockAccessList{Accounts: []AccountChanges{
			{Address: common.Address{1}}, // only read
			{
				Address:        common.Address{2},
				StorageChanges: []SlotChanges{{Slot: common.Hash{6}, Changes: []StorageChange{{BlockAccessIndex: 1, NewValue: common.Hash{7}}}}},
				StorageReads:   []common.Hash{{5}},
			},
			{Address: common.Address{3}, BalanceChanges: []BalanceChange{{BlockAccessIndex: 1, PostBalance: big.NewInt(3)}}}, // written, no reads
			{Address: common.Address{4}, StorageReads: []common.Hash{{8}}},                                                   // only read, with a slot
		}}
	}
	full := list()
	require.NoError(t, full.Validate())
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
	decodedWrites, err := DecodeBAL(writesEnc)
	require.NoError(t, err)
	decodedReads, err := DecodeBALReads(readsEnc)
	require.NoError(t, err)
	combined, err := CombineBAL(decodedWrites, decodedReads)
	require.NoError(t, err)
	combinedEnc, err := combined.Encode()
	require.NoError(t, err)
	require.Equal(t, fullEnc, combinedEnc)

	none, err := CombineBAL(writes, &BALReads{})
	require.NoError(t, err)
	noneEnc, err := none.Encode()
	require.NoError(t, err)
	require.Equal(t, writesEnc, noneEnc)

	_, err = CombineBAL(full, reads)
	require.ErrorIs(t, err, errReadsInWrites)
	unsorted := &BALReads{Accounts: []AccountReads{{Address: common.Address{2}}, {Address: common.Address{1}}}}
	enc, err := unsorted.Encode()
	require.NoError(t, err)
	_, err = DecodeBALReads(enc)
	require.ErrorIs(t, err, errUnsortedAccounts)

	// Writes drops the storage roots too, and the hash covers the writes only:
	// the decoded writes, and lists that differ in reads or storage roots,
	// have the same hash, while a different write changes it.
	writesHash, err := decodedWrites.Hash()
	require.NoError(t, err)
	require.Equal(t, hash, writesHash)
	root := common.Hash{9}
	withRoot := list()
	withRoot.Accounts[1].StorageRoot = &root
	require.Nil(t, withRoot.Writes().Accounts[0].StorageRoot)
	require.NotNil(t, withRoot.Accounts[1].StorageRoot, "Writes leaves the list as it is")
	otherReads := list()
	otherReads.Accounts[1].StorageReads = []common.Hash{{7}, {8}}
	otherReads.Accounts = append(otherReads.Accounts, AccountChanges{Address: common.Address{5}})
	require.NoError(t, otherReads.Validate())
	for _, other := range []*BlockAccessList{withRoot, otherReads} {
		otherHash, err := other.Hash()
		require.NoError(t, err)
		require.Equal(t, hash, otherHash, "reads and storage roots are not hashed")
	}
	otherWrite := list()
	otherWrite.Accounts[1].StorageChanges[0].Changes[0].NewValue = common.Hash{6}
	otherBalance := list()
	otherBalance.Accounts[2].BalanceChanges[0].PostBalance = big.NewInt(6)
	for _, other := range []*BlockAccessList{otherWrite, otherBalance} {
		otherHash, err := other.Hash()
		require.NoError(t, err)
		require.NotEqual(t, hash, otherHash)
	}
}

// TestBlockWithBAL attaches a list to a block without changing its hash or
// encoding.
func TestBlockWithBAL(t *testing.T) {
	block := NewBlockWithHeader(&Header{Number: big.NewInt(7), Time: big.NewInt(1), BlockScore: big.NewInt(1)})
	require.Nil(t, block.BAL())
	enc := []byte{0xc1, 0xc0}
	withBAL := block.WithBAL(enc)
	require.Equal(t, enc, withBAL.BAL())
	require.Nil(t, block.BAL(), "the block itself is not changed")
	require.Equal(t, block.Hash(), withBAL.Hash())
	want, err := rlp.EncodeToBytes(block)
	require.NoError(t, err)
	got, err := rlp.EncodeToBytes(withBAL)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Nil(t, withBAL.WithBAL(nil).BAL())
}
