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
	"testing"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database/ancient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genDerivableReceipts makes receipts for the transactions of block (hash,
// number) whose derived fields are set as block processing sets them. Receipt
// i has i logs with up to three topics each, and receipt 1 created a contract.
func genDerivableReceipts(hash common.Hash, number uint64, txs types.Transactions) types.Receipts {
	var receipts types.Receipts
	logIndex := uint(0)
	for i, tx := range txs {
		r := &types.Receipt{
			Status:  types.ReceiptStatusSuccessful,
			TxHash:  tx.Hash(),
			GasUsed: uint64(21000 + i),
			Logs:    []*types.Log{}, // decoded receipts have an empty slice
		}
		if i == 1 {
			r.ContractAddress = common.BytesToAddress([]byte{0xcc, byte(i)})
		}
		for j := 0; j < i; j++ {
			log := &types.Log{
				Address:     common.BytesToAddress([]byte{byte(j), 0xbb}),
				Topics:      []common.Hash{},
				Data:        []byte{byte(i), byte(j)},
				BlockNumber: number,
				TxHash:      r.TxHash,
				TxIndex:     uint(i),
				BlockHash:   hash,
				Index:       logIndex,
			}
			for k := 0; k <= j && k < 3; k++ {
				log.Topics = append(log.Topics, common.BytesToHash([]byte{byte(i), byte(j), byte(k)}))
			}
			r.Logs = append(r.Logs, log)
			logIndex++
		}
		r.Bloom = types.CreateBloom(types.Receipts{r})
		receipts = append(receipts, r)
	}
	return receipts
}

func genReceiptTestTxs(t *testing.T, n int) types.Transactions {
	var txs types.Transactions
	for i := 0; i < n; i++ {
		tx, err := genTransaction(uint64(i + 1))
		require.NoError(t, err)
		txs = append(txs, tx)
	}
	return txs
}

func bodyOf(txs types.Transactions) func() *types.Body {
	return func() *types.Body { return &types.Body{Transactions: txs} }
}

// TestFrozenReceipts_Derived checks that the derived format leaves out the
// blooms and every field derived from the block, and restores the receipts
// exactly.
func TestFrozenReceipts_Derived(t *testing.T) {
	var (
		hash   = common.HexToHash("0xb10c")
		number = uint64(7)
		txs    = genReceiptTestTxs(t, 4)
	)
	receipts := genDerivableReceipts(hash, number, txs)
	data := encodeStorageReceipts(receipts)

	frozen := frozenReceipts(hash, number, txs, data)
	require.Equal(t, byte(derivedReceiptsPrefix), frozen[0])
	var stored []*types.ReceiptForStorage
	require.NoError(t, rlp.DecodeBytes(data, &stored))
	slim, err := encodeSlimReceipts(stored)
	require.NoError(t, err)
	t.Logf("4 receipts, 6 logs: key-value %d bytes, slim %d, derived %d", len(data), len(slim), len(frozen))
	assert.Less(t, len(frozen), len(slim))
	assert.Less(t, len(slim), len(data))

	decoded, err := decodeReceipts(frozen, hash, number, bodyOf(txs))
	require.NoError(t, err)
	assert.Equal(t, receipts, decoded)

	// The slim and key-value formats still decode, without the body.
	noBody := func() *types.Body { t.Fatal("body read"); return nil }
	for _, item := range [][]byte{slim, data} {
		decoded, err = decodeReceipts(item, hash, number, noBody)
		require.NoError(t, err)
		assert.Equal(t, receipts, decoded)
	}

	// The derived format needs the body.
	_, err = decodeReceipts(frozen, hash, number, func() *types.Body { return nil })
	assert.Error(t, err)

	// A block without transactions.
	empty := frozenReceipts(hash, number, nil, encodeStorageReceipts(types.Receipts{}))
	assert.Equal(t, byte(derivedReceiptsPrefix), empty[0])
	decoded, err = decodeReceipts(empty, hash, number, bodyOf(nil))
	require.NoError(t, err)
	assert.Empty(t, decoded)
}

// TestFrozenReceipts_Fallback checks that receipts whose stored fields differ
// from the derived ones are frozen in a format that keeps them.
func TestFrozenReceipts_Fallback(t *testing.T) {
	var (
		hash   = common.HexToHash("0xb10c")
		number = uint64(7)
		txs    = genReceiptTestTxs(t, 4)
	)
	for name, tc := range map[string]struct {
		modify func(types.Receipts)
		txs    types.Transactions
		prefix byte
	}{
		"log block hash": {func(r types.Receipts) { r[2].Logs[0].BlockHash = common.Hash{} }, txs, slimReceiptsPrefix},
		"log index":      {func(r types.Receipts) { r[3].Logs[1].Index = 9 }, txs, slimReceiptsPrefix},
		"tx hash":        {func(r types.Receipts) { r[0].TxHash = common.Hash{1} }, txs, slimReceiptsPrefix},
		"tx count":       {func(types.Receipts) {}, txs[:3], slimReceiptsPrefix},
		"bloom":          {func(r types.Receipts) { r[2].Bloom[0] ^= 0x80 }, txs, 0}, // unchanged
	} {
		t.Run(name, func(t *testing.T) {
			receipts := genDerivableReceipts(hash, number, txs)
			tc.modify(receipts)
			data := encodeStorageReceipts(receipts)
			frozen := frozenReceipts(hash, number, tc.txs, data)
			if tc.prefix == 0 {
				assert.Equal(t, data, frozen)
			} else {
				assert.Equal(t, tc.prefix, frozen[0])
			}
			decoded, err := decodeReceipts(frozen, hash, number, bodyOf(tc.txs))
			require.NoError(t, err)
			assert.Equal(t, receipts, decoded)
		})
	}
}

// TestFreezer_SlimReceipts checks that the freeze loop stores receipts in the
// derived format when they match their block, in the slim format otherwise,
// and that both read back as they were written.
func TestFreezer_SlimReceipts(t *testing.T) {
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 2))
	defer dbm.Close()

	blocks := writeFreezerTestChain(t, dbm, 6)
	// The logs of the test chain carry another block number, so they are frozen
	// in the slim format. Block 2 gets receipts that match it.
	b := blocks[2]
	receipts := genDerivableReceipts(b.hash(), 2, b.body.Transactions)
	dbm.WriteReceipts(b.hash(), 2, receipts)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(4), dbm.FrozenBlocks())

	for _, fb := range blocks[1:4] {
		number := fb.header.Number.Uint64()
		item, err := dbm.freezer.Ancient(ancient.ChainFreezerReceiptTable, number)
		require.NoError(t, err)
		want, wantReceipts := byte(slimReceiptsPrefix), fb.receipts
		if number == 2 {
			want, wantReceipts = derivedReceiptsPrefix, receipts
		}
		assert.Equal(t, want, item[0], "receipts %d", number)
		assert.Equal(t, wantReceipts, dbm.ReadReceipts(fb.hash(), number), "receipts %d", number)
	}
	assert.Equal(t, receipts, dbm.ReadReceiptsByBlockHash(b.hash()))
}
