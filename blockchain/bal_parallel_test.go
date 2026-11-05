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

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
	"github.com/stretchr/testify/require"
)

// checkParallel executes every block of a chain with its list in parallel and
// checks the state root, the receipts, the gas and the logs against the block.
func checkParallel(t *testing.T, chain *BlockChain, blocks []*types.Block, lists []*bal.BlockAccessList, genesisRoot common.Hash) {
	parent := genesisRoot
	for i, block := range blocks {
		chain.PrefetchAccessList(parent, lists[i], 4)
		final, receipts, logs, usedGas, err := chain.Processor().(*StateProcessor).ProcessWithAccessList(block, parent, lists[i], vm.Config{}, 4, true)
		require.NoError(t, err, "block %d", block.NumberU64())
		require.Equal(t, block.Root(), final.IntermediateRoot(true), "block %d", block.NumberU64())
		require.Equal(t, block.GasUsed(), usedGas, "block %d", block.NumberU64())
		got, err := rlp.EncodeToBytes(receipts)
		require.NoError(t, err)
		want, err := rlp.EncodeToBytes(chain.GetReceiptsByBlockHash(block.Hash()))
		require.NoError(t, err)
		require.Equal(t, want, got, "block %d", block.NumberU64())
		for j, l := range logs {
			require.Equal(t, uint(j), l.Index)
		}
		parent = block.Root()
	}
}

// TestProcessWithAccessList runs the blocks of chains with transfers, storage
// writes, key updates, delegation, self-destruction and recreation (shanghai and
// prague), and of a chain whose txs all write the same contract, in parallel.
func TestProcessWithAccessList(t *testing.T) {
	for _, hardfork := range []string{"shanghai", "prague"} {
		c := newBALHistoryChain(t, hardfork)
		checkParallel(t, c.chain, c.blocks, c.lists, c.env.genesis.Root())
	}

	// Every tx reads and writes the slots the one before wrote.
	env := newAmberTestEnv(t)
	chain, blocks := env.generate(4, func(i int, b *BlockGen) {
		for j := range 6 {
			b.AddTx(env.legacyTx(b, j%2, &balStoreAddr, common.Big0, balSlotData(byte(j%3), byte(10*i+j))))
			b.AddTx(env.legacyTx(b, j%2, &env.addrs[(j+1)%2], big.NewInt(int64(j+1)), nil))
		}
	})
	lists := checkBAL(t, chain, blocks)
	checkParallel(t, chain, blocks, lists, env.genesis.Root())

	// A wrong value makes a phase start from another state.
	enc, err := lists[1].Encode()
	require.NoError(t, err)
	tampered, err := bal.Decode(enc)
	require.NoError(t, err)
	store := findAccess(t, tampered, balStoreAddr)
	store.StorageWrites[0].Changes[0].Value[31] ^= 1
	_, _, _, _, err = chain.Processor().(*StateProcessor).ProcessWithAccessList(blocks[1], blocks[0].Root(), tampered, vm.Config{}, 4, true)
	require.ErrorIs(t, err, ErrAccessListMismatch)

	// A read that did not happen is caught with the reads, and ignored without.
	extra, err := bal.Decode(enc)
	require.NoError(t, err)
	a := findAccess(t, extra, balStoreAddr)
	a.StorageReads = append(a.StorageReads, common.HexToHash("0xff"))
	_, _, _, _, err = chain.Processor().(*StateProcessor).ProcessWithAccessList(blocks[1], blocks[0].Root(), extra, vm.Config{}, 4, true)
	require.ErrorIs(t, err, ErrAccessListMismatch)
	_, _, _, _, err = chain.Processor().(*StateProcessor).ProcessWithAccessList(blocks[1], blocks[0].Root(), extra, vm.Config{}, 4, false)
	require.NoError(t, err)
}
