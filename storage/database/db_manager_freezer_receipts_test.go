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
	"github.com/kaiachain/kaia/storage/database/ancient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestFreezer_MovesSlimReceipts checks that freezing moves receipts written in
// a slim format as they are.
func TestFreezer_MovesSlimReceipts(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 2))
	defer dbm.Close()

	blocks := writeFreezerTestChain(t, dbm, 6)
	wantPrefix := map[uint64]byte{}
	for _, fb := range blocks[1:4] {
		number := fb.header.Number.Uint64()
		block := types.NewBlockWithHeader(fb.header).WithBody(fb.body.Transactions)
		wantPrefix[number] = slimReceiptsPrefix // the logs carry another block number
		if number == 2 {
			fb.receipts = genDerivableReceipts(fb.hash(), number, fb.body.Transactions)
			wantPrefix[number] = derivedReceiptsPrefix
		}
		dbm.WriteBlockReceipts(block, fb.receipts)
	}
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(4), dbm.FrozenBlocks())
	for _, fb := range blocks[1:4] {
		number := fb.header.Number.Uint64()
		item, err := dbm.freezer.Ancient(ancient.ChainFreezerReceiptTable, number)
		require.NoError(t, err)
		assert.Equal(t, wantPrefix[number], item[0], "frozen receipts %d", number)
		assert.Equal(t, fb.receipts, dbm.ReadReceipts(fb.hash(), number), "frozen receipts %d", number)
	}
}
