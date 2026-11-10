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

package blockchain

import (
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/params"
	"github.com/stretchr/testify/require"
)

// checkHint checks that the hint of every block rebuilds its list, and
// returns the encoded sizes of the lists and of their hints.
func checkHint(t *testing.T, chain *BlockChain, blocks []*types.Block, lists []*access.BlockAccessList) (full, hinted int) {
	for i, block := range blocks {
		parent := chain.GetHeaderByHash(block.ParentHash())
		require.NotNil(t, parent)
		hint, err := chain.AccessListHint(block, lists[i])
		require.NoError(t, err)
		rebuilt, err := chain.RebuildAccessList(block, parent, hint)
		require.NoError(t, err)
		want, err := lists[i].Encode()
		require.NoError(t, err)
		got, err := rebuilt.Encode()
		require.NoError(t, err)
		require.Equal(t, want, got, "block %d: the hint rebuilds the list", block.NumberU64())
		enc, err := hint.Encode()
		require.NoError(t, err)
		full, hinted = full+len(want), hinted+len(enc)
	}
	return full, hinted
}

// The hint of blocks of value transfers leaves out their changes and reads:
// it does not grow with the txs, while the list does.
func TestAccessListHintValueTransfers(t *testing.T) {
	sizes := func(n int) (full, hinted int, hint *access.BlockAccessList, txs int) {
		env := newBALTestEnv(t, "prague", 8)
		chain, blocks := env.generate(2, func(i int, b *BlockGen) {
			b.SetRewardbase(common.Address{0xbe})
			for j := range n {
				to := env.addrs[(j+1)%8]
				b.AddTx(env.legacyTx(b, j%8, &to, big.NewInt(int64(100*i+j+1)), nil))
			}
		})
		lists := checkBAL(t, chain, blocks)
		full, hinted = checkHint(t, chain, blocks[1:], lists[1:])
		hint, err := chain.AccessListHint(blocks[1], lists[1])
		require.NoError(t, err)
		return full, hinted, hint, len(blocks[1].Transactions())
	}
	smallFull, smallHint, _, _ := sizes(8)
	bigFull, bigHint, hint, txs := sizes(64)
	t.Logf("value transfers: 8 txs list %d B hint %d B, 64 txs list %d B hint %d B", smallFull, smallHint, bigFull, bigHint)
	require.Greater(t, bigFull, 3*smallFull, "the list grows with the txs")
	require.Less(t, bigHint, smallHint+64, "the hint does not")

	// What is left is what InitializeState and FinalizeState change.
	for _, a := range hint.Accounts {
		for _, c := range a.BalanceChanges {
			require.Contains(t, []uint32{0, uint32(txs + 1)}, c.Index, "account %s", a.Address.Hex())
		}
		require.Empty(t, a.NonceChanges, "account %s", a.Address.Hex())
	}
}

// A contract call, which the prediction leaves alone, before value transfers
// of the same accounts: the hint keeps the call and rebuilds the rest from it.
func TestAccessListHintMixed(t *testing.T) {
	c := newBALHistoryChain(t, "prague")
	checkHint(t, c.chain, c.blocks, c.lists)

	env := newAmberTestEnv(t)
	chain, blocks := env.generate(4, func(i int, b *BlockGen) {
		// addrs[1] calls the store contract, then sends value twice.
		b.AddTx(env.legacyTx(b, 1, &balStoreAddr, common.Big0, balSlotData(byte(i), 7)))
		b.AddTx(env.legacyTx(b, 1, &env.addrs[0], big.NewInt(params.KAIA), nil))
		b.AddTx(env.legacyTx(b, 0, &env.addrs[1], big.NewInt(3), nil))
		b.AddTx(env.legacyTx(b, 1, &env.addrs[0], big.NewInt(5), nil))
	})
	checkHint(t, chain, blocks, checkBAL(t, chain, blocks))
}

// InsertChain executes a block in parallel with the hint of its proposal,
// which it rebuilds into the list.
func TestInsertChainWithAccessListHint(t *testing.T) {
	env := newAmberTestEnv(t)
	src, blocks := env.generate(4, func(i int, b *BlockGen) {
		b.AddTx(env.legacyTx(b, 1, &balStoreAddr, common.Big0, balSlotData(byte(i), 7)))
		b.AddTx(env.legacyTx(b, 1, &env.addrs[0], big.NewInt(params.KAIA), nil))
		b.AddTx(env.legacyTx(b, 0, &env.addrs[1], big.NewInt(3), nil))
	})
	lists := checkBAL(t, src, blocks)
	imported := newImportChain(t, env)
	amber := 0
	for i, block := range blocks {
		if !env.config.IsAmberForkEnabled(block.Number()) {
			continue
		}
		hint, err := src.AccessListHint(block, lists[i])
		require.NoError(t, err)
		hintEnc, err := hint.Encode()
		require.NoError(t, err)
		listEnc, err := lists[i].Encode()
		require.NoError(t, err)
		require.Less(t, len(hintEnc), len(listEnc), "block %d: the hint leaves changes out", block.NumberU64())
		imported.ProvideAccessList(block.Hash(), hint)
		amber++
	}
	parallel, failed := parallelBlockCounter.Count(), parallelFailedCounter.Count()
	_, err := imported.InsertChain(blocks)
	require.NoError(t, err)
	require.Equal(t, int64(amber), parallelBlockCounter.Count()-parallel, "every Amber block runs in parallel")
	require.Equal(t, int64(0), parallelFailedCounter.Count()-failed)
	require.Equal(t, blocks[len(blocks)-1].Hash(), imported.CurrentBlock().Hash())
}
