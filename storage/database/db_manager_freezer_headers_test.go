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

package database

import (
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database/ancient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFrozenHeader_Derived checks that the derived format restores headers
// with and without the optional fields, and that a header whose parent or
// number differ is stored unchanged.
func TestFrozenHeader_Derived(t *testing.T) {
	parent := common.HexToHash("0xabcd")
	blobGas := uint64(7)
	for name, h := range map[string]*types.Header{
		"legacy": {
			ParentHash: parent, Number: big.NewInt(300), BlockScore: big.NewInt(1), Time: big.NewInt(9),
			Extra: []byte("extra"), Governance: []byte{}, Vote: []byte{},
		},
		"randao": {
			ParentHash: parent, Number: big.NewInt(1 << 40), BlockScore: big.NewInt(1), Time: big.NewInt(9),
			Extra: []byte("extra"), Governance: []byte{1}, Vote: []byte{2}, BaseFee: big.NewInt(25),
			RandomReveal: make([]byte, 96), MixHash: make([]byte, 32), BlobGasUsed: &blobGas,
		},
	} {
		t.Run(name, func(t *testing.T) {
			number := h.Number.Uint64()
			data, err := rlp.EncodeToBytes(h)
			require.NoError(t, err)

			frozen := frozenHeader(number, parent, data)
			require.Equal(t, byte(derivedHeaderPrefix), frozen[0])
			assert.Less(t, len(frozen), len(data)-common.HashLength)
			restored, err := decodeDerivedHeader(frozen[1:], number, parent)
			require.NoError(t, err)
			assert.Equal(t, data, restored)

			// Another parent or number keeps the header.
			assert.Equal(t, data, frozenHeader(number, common.HexToHash("0x01"), data))
			assert.Equal(t, data, frozenHeader(number+1, parent, data))
		})
	}
	// The genesis is kept.
	genesis, _ := rlp.EncodeToBytes(&types.Header{Number: big.NewInt(0), BlockScore: big.NewInt(1), Time: big.NewInt(0)})
	assert.Equal(t, genesis, frozenHeader(0, common.Hash{}, genesis))
}

// TestFreezer_DerivedHeaders checks that the freeze loop stores headers in the
// derived format and reads them back.
func TestFreezer_DerivedHeaders(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 2))
	defer dbm.Close()
	blocks := writeFreezerTestChain(t, dbm, 8)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(6), dbm.FrozenBlocks())

	for _, b := range blocks[:6] {
		number := b.header.Number.Uint64()
		item, err := dbm.freezer.Ancient(ancient.ChainFreezerHeaderTable, number)
		require.NoError(t, err)
		if number == 0 {
			assert.NotEqual(t, byte(derivedHeaderPrefix), item[0])
			continue
		}
		assert.Equal(t, byte(derivedHeaderPrefix), item[0], "header %d", number)
		want, _ := rlp.EncodeToBytes(b.header)
		assert.Equal(t, want, []byte(dbm.ReadHeaderRLP(b.hash(), number)), "header %d", number)
		assert.Equal(t, b.hash(), dbm.ReadHeader(b.hash(), number).Hash(), "header %d", number)
	}
}

// TestFreezer_DerivedTd checks that the total blockscore of a frozen block is
// deleted from the key-value store only if it is number+1.
func TestFreezer_DerivedTd(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 2))
	defer dbm.Close()
	blocks := writeFreezerTestChain(t, dbm, 8)
	// Block 3 has another total blockscore.
	odd := blocks[3]
	dbm.WriteTd(odd.hash(), 3, big.NewInt(100))
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(6), dbm.FrozenBlocks())

	for _, b := range blocks[1:6] {
		hash, number := b.hash(), b.header.Number.Uint64()
		want := new(big.Int).SetUint64(number + 1)
		if number == 3 {
			want = big.NewInt(100)
		}
		assert.Equal(t, number == 3, hasKV(dbm, MiscDB, headerTDKey(number, hash)), "kv td %d", number)
		dbm.cm.deleteTdCache(hash)
		assert.Equal(t, want, dbm.ReadTd(hash, number), "td %d", number)
	}
	// Not for another hash at a frozen height.
	assert.Nil(t, dbm.ReadTd(common.HexToHash("0x01"), 4))
}

func readTxLookupRaw(t *testing.T, dbm *databaseManager, txHash common.Hash) (full bool, slim bool) {
	data, _ := dbm.getDatabase(TxLookUpEntryDB).Get(TxLookupKey(txHash))
	require.NotEmpty(t, data)
	var entry TxLookupEntry
	if rlp.DecodeBytes(data, &entry) == nil {
		return true, false
	}
	var s slimTxLookupEntry
	require.NoError(t, rlp.DecodeBytes(data, &s))
	return false, true
}

// TestFreezer_SlimTxLookups checks that a node with the freezer writes tx
// lookup entries without the block hash, that the freezer rewrites the full
// entries of the blocks it freezes, and that both kinds resolve.
func TestFreezer_SlimTxLookups(t *testing.T) {
	dir := t.TempDir()
	// Without the freezer: full entries.
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, false, 0))
	blocks := writeFreezerTestChain(t, dbm, 8)
	for _, b := range blocks {
		dbm.WriteTxLookupEntries(types.NewBlockWithHeader(b.header).WithBody(b.body.Transactions))
		full, _ := readTxLookupRaw(t, dbm, b.body.Transactions[0].Hash())
		require.True(t, full)
	}
	dbm.Close()

	// With the freezer: new entries are slim, frozen blocks' entries are rewritten.
	dbm = openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 2))
	defer dbm.Close()
	parent := blocks[len(blocks)-1].hash()
	for i := 8; i < 10; i++ {
		b := genFreezerTestBlock(t, uint64(i), parent, "canonical")
		writeFreezerTestBlock(dbm, b, true)
		dbm.WriteTxLookupEntries(types.NewBlockWithHeader(b.header).WithBody(b.body.Transactions))
		blocks = append(blocks, b)
		parent = b.hash()
	}
	dbm.WriteHeadBlockHash(parent)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(8), dbm.FrozenBlocks())

	for _, b := range blocks[1:] {
		hash, number := b.hash(), b.header.Number.Uint64()
		txHash := b.body.Transactions[0].Hash()
		_, slim := readTxLookupRaw(t, dbm, txHash)
		// Blocks 1-7 were frozen and rewritten; 8 and 9 were written slim.
		assert.True(t, slim, "slim entry %d", number)

		gotHash, gotNumber, index := dbm.ReadTxLookupEntry(txHash)
		assert.Equal(t, hash, gotHash, "block hash %d", number)
		assert.Equal(t, number, gotNumber)
		assert.Equal(t, uint64(0), index)
		tx, _, _, _ := dbm.ReadTxAndLookupInfo(txHash)
		if assert.NotNil(t, tx, "tx %d", number) {
			assert.Equal(t, txHash, tx.Hash())
		}
	}

	// A slim entry whose block does not hold the transaction resolves to nothing.
	stale := common.HexToHash("0x5a1e")
	data, _ := rlp.EncodeToBytes(slimTxLookupEntry{BlockIndex: 3, Index: 0})
	require.NoError(t, dbm.getDatabase(TxLookUpEntryDB).Put(TxLookupKey(stale), data))
	gotHash, _, _ := dbm.ReadTxLookupEntry(stale)
	assert.Equal(t, common.Hash{}, gotHash)
}
