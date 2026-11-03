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

func genSlimTestReceipts() types.Receipts {
	var receipts types.Receipts
	for i := 0; i < 4; i++ {
		r := &types.Receipt{
			Status:  types.ReceiptStatusSuccessful,
			TxHash:  common.BytesToHash([]byte{byte(i), 0xaa}),
			GasUsed: uint64(21000 + i),
			Logs:    []*types.Log{}, // decoded receipts have an empty slice
		}
		if i == 1 {
			r.ContractAddress = common.BytesToAddress([]byte{0xcc, byte(i)})
		}
		// Receipt i has i logs with up to three topics each.
		for j := 0; j < i; j++ {
			log := &types.Log{
				Address:     common.BytesToAddress([]byte{byte(j), 0xbb}),
				Data:        []byte{byte(i), byte(j)},
				BlockNumber: 7,
				TxHash:      r.TxHash,
				TxIndex:     uint(i),
				BlockHash:   common.BytesToHash([]byte{0xdd}),
				Index:       uint(i*10 + j),
			}
			for k := 0; k <= j && k < 3; k++ {
				log.Topics = append(log.Topics, common.BytesToHash([]byte{byte(i), byte(j), byte(k)}))
			}
			r.Logs = append(r.Logs, log)
		}
		r.Bloom = types.CreateBloom(types.Receipts{r})
		receipts = append(receipts, r)
	}
	return receipts
}

func encodeStorageReceipts(t *testing.T, receipts types.Receipts) []byte {
	storageReceipts := make([]*types.ReceiptForStorage, len(receipts))
	for i, r := range receipts {
		storageReceipts[i] = (*types.ReceiptForStorage)(r)
	}
	data, err := rlp.EncodeToBytes(storageReceipts)
	require.NoError(t, err)
	return data
}

// TestFrozenReceipts_Slim checks that the slim format drops the blooms and
// restores the receipts exactly.
func TestFrozenReceipts_Slim(t *testing.T) {
	receipts := genSlimTestReceipts()
	data := encodeStorageReceipts(t, receipts)

	slim, err := encodeFrozenReceipts(data)
	require.NoError(t, err)
	assert.Equal(t, byte(slimReceiptsPrefix), slim[0])
	// Each bloom is 256 bytes plus its RLP header; the prefix adds one byte.
	assert.LessOrEqual(t, len(slim), len(data)-len(receipts)*types.BloomByteLength+1)

	decoded, err := decodeReceipts(slim)
	require.NoError(t, err)
	assert.Equal(t, receipts, decoded)

	// The key-value format still decodes.
	decoded, err = decodeReceipts(data)
	require.NoError(t, err)
	assert.Equal(t, receipts, decoded)

	// A block without transactions.
	empty := encodeStorageReceipts(t, types.Receipts{})
	slim, err = encodeFrozenReceipts(empty)
	require.NoError(t, err)
	decoded, err = decodeReceipts(slim)
	require.NoError(t, err)
	assert.Empty(t, decoded)
}

// TestFrozenReceipts_BloomMismatch checks that receipts whose stored bloom
// does not match their logs are frozen unchanged.
func TestFrozenReceipts_BloomMismatch(t *testing.T) {
	receipts := genSlimTestReceipts()
	receipts[2].Bloom[0] ^= 0x80
	data := encodeStorageReceipts(t, receipts)

	_, err := encodeFrozenReceipts(data)
	assert.ErrorIs(t, err, errSlimReceiptsBloom)
	assert.Equal(t, data, frozenReceipts(1, data))

	decoded, err := decodeReceipts(frozenReceipts(1, data))
	require.NoError(t, err)
	assert.Equal(t, receipts, decoded)
}

// TestFreezer_SlimReceipts checks that the freeze loop stores receipts in the
// slim format and that they read back as they were written.
func TestFreezer_SlimReceipts(t *testing.T) {
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 2))
	defer dbm.Close()

	blocks := writeFreezerTestChain(t, dbm, 6)
	receipts := genSlimTestReceipts()
	b := blocks[2]
	dbm.WriteReceipts(b.hash(), 2, receipts)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(4), dbm.FrozenBlocks())

	for number := uint64(1); number < 4; number++ {
		item, err := dbm.freezer.Ancient(ancient.ChainFreezerReceiptTable, number)
		require.NoError(t, err)
		assert.Equal(t, byte(slimReceiptsPrefix), item[0], "receipts %d", number)
	}
	assert.Equal(t, receipts, dbm.ReadReceipts(b.hash(), 2))
	assert.Equal(t, receipts, dbm.ReadReceiptsByBlockHash(b.hash()))
}
