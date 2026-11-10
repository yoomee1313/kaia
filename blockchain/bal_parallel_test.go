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
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
	"github.com/stretchr/testify/require"
)

// checkParallel executes every block of a chain with its list in parallel and
// checks the state root, the receipts, the gas and the logs against the block.
func checkParallel(t *testing.T, chain *BlockChain, blocks []*types.Block, lists []*access.BlockAccessList) {
	for i, block := range blocks {
		parent := chain.GetHeaderByHash(block.ParentHash())
		chain.PrefetchAccessList(context.Background(), parent.Root, lists[i], 4)
		final, receipts, logs, usedGas, _, err := chain.Processor().(*StateProcessor).ProcessWithAccessList(context.Background(), block, parent, lists[i], vm.Config{}, 4, true)
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
	}
}

// TestProcessWithAccessList runs the blocks of chains with transfers, storage
// writes, key updates, delegation, self-destruction and recreation (shanghai and
// prague), and of a chain whose txs all write the same contract, in parallel.
func TestProcessWithAccessList(t *testing.T) {
	for _, hardfork := range []string{"shanghai", "prague"} {
		c := newBALHistoryChain(t, hardfork)
		checkParallel(t, c.chain, c.blocks, c.lists)
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
	checkParallel(t, chain, blocks, lists)

	// A wrong value makes a phase start from another state.
	enc, err := lists[1].Encode()
	require.NoError(t, err)
	tampered, err := access.Decode(enc)
	require.NoError(t, err)
	store := findAccess(t, tampered, balStoreAddr)
	store.StorageWrites[0].Changes[0].Value[31] ^= 1
	_, _, _, _, _, err = chain.Processor().(*StateProcessor).ProcessWithAccessList(context.Background(), blocks[1], blocks[0].Header(), tampered, vm.Config{}, 4, true)
	require.ErrorIs(t, err, ErrAccessListMismatch)

	// A read that did not happen is caught with the reads, and ignored without.
	extra, err := access.Decode(enc)
	require.NoError(t, err)
	a := findAccess(t, extra, balStoreAddr)
	a.StorageReads = append(a.StorageReads, common.HexToHash("0xff"))
	_, _, _, _, _, err = chain.Processor().(*StateProcessor).ProcessWithAccessList(context.Background(), blocks[1], blocks[0].Header(), extra, vm.Config{}, 4, true)
	require.ErrorIs(t, err, ErrAccessListMismatch)
	_, _, _, _, _, err = chain.Processor().(*StateProcessor).ProcessWithAccessList(context.Background(), blocks[1], blocks[0].Header(), extra, vm.Config{}, 4, false)
	require.NoError(t, err)
}

// amberParallelChain makes a chain of 6 blocks from the Amber hardfork at block
// 2, whose txs write a shared contract, and the list of each block.
func amberParallelChain(t *testing.T) (*balTestEnv, *BlockChain, []*types.Block, []*access.BlockAccessList) {
	env := newAmberTestEnv(t)
	chain, blocks := env.generate(6, func(i int, b *BlockGen) {
		for j := range 4 {
			b.AddTx(env.legacyTx(b, j%2, &balStoreAddr, common.Big0, balSlotData(byte(j%3), byte(10*i+j))))
			b.AddTx(env.legacyTx(b, j%2, &env.addrs[(j+1)%2], big.NewInt(int64(j+1)), nil))
		}
	})
	return env, chain, blocks, checkBAL(t, chain, blocks)
}

// TestExecuteWithAccessListMatchesInOrder checks that executing a block in
// parallel with its list gives what executing it in order gives: the state
// root, the receipts, the gas, the computation cost and the list, reads
// included. Then a chain imports the blocks with their lists in parallel.
func TestExecuteWithAccessListMatchesInOrder(t *testing.T) {
	env, chain, blocks, lists := amberParallelChain(t)
	amber := 0
	for i, block := range blocks {
		parent := chain.GetHeaderByHash(block.ParentHash())
		if !env.config.IsAmberForkEnabled(block.Number()) {
			_, err := chain.ExecuteWithAccessList(context.Background(), block, parent, lists[i])
			require.Error(t, err, "block %d", block.NumberU64())
			continue
		}
		amber++

		inOrder, err := chain.StateAt(parent.Root)
		require.NoError(t, err)
		access.EnableRecorder(inOrder)
		receipts, _, usedGas, _, stats, err := chain.Processor().Process(context.Background(), block, inOrder, vm.Config{})
		require.NoError(t, err)
		root := inOrder.IntermediateRoot(true)
		wantList, err := access.Recorded(inOrder).Encode()
		require.NoError(t, err)

		res, err := chain.ExecuteWithAccessList(context.Background(), block, parent, lists[i])
		require.NoError(t, err, "block %d", block.NumberU64())
		require.NoError(t, chain.Validator().ValidateStateWithCache(block, res))
		require.Equal(t, root, res.State.IntermediateRoot(true))
		require.Equal(t, block.Root(), root)
		require.Equal(t, usedGas, res.UsedGas)
		require.Equal(t, stats.ComputationCost, res.ProcessStats.ComputationCost)
		require.NotZero(t, res.ProcessStats.ComputationCost)
		gotReceipts, err := rlp.EncodeToBytes(res.Receipts)
		require.NoError(t, err)
		wantReceipts, err := rlp.EncodeToBytes(receipts)
		require.NoError(t, err)
		require.Equal(t, wantReceipts, gotReceipts, "block %d", block.NumberU64())
		gotList, err := access.Recorded(res.State).Encode()
		require.NoError(t, err)
		require.Equal(t, wantList, gotList, "block %d", block.NumberU64())
	}
	require.Equal(t, 5, amber)

	// The list of a block before Amber is ignored.
	imported := newImportChain(t, env)
	for i, block := range blocks {
		imported.ProvideAccessList(block.Hash(), lists[i])
	}
	parallel, failed := parallelBlockCounter.Count(), parallelFailedCounter.Count()
	_, err := imported.InsertChain(blocks)
	require.NoError(t, err)
	require.Equal(t, int64(amber), parallelBlockCounter.Count()-parallel)
	require.Equal(t, failed, parallelFailedCounter.Count())

	head := blocks[len(blocks)-1]
	require.Equal(t, head.Hash(), imported.CurrentBlock().Hash())
	want, err := chain.StateAt(head.Root())
	require.NoError(t, err)
	got, err := imported.StateAt(head.Root())
	require.NoError(t, err)
	for slot := range 3 {
		key := common.BigToHash(big.NewInt(int64(slot)))
		require.Equal(t, want.GetState(balStoreAddr, key), got.GetState(balStoreAddr, key))
	}
	for i, block := range blocks {
		gotReceipts, err := rlp.EncodeToBytes(imported.GetReceiptsByBlockHash(block.Hash()))
		require.NoError(t, err)
		wantReceipts, err := rlp.EncodeToBytes(chain.GetReceiptsByBlockHash(block.Hash()))
		require.NoError(t, err)
		require.Equal(t, wantReceipts, gotReceipts, "block %d", i+1)
	}
}

// TestInsertChainAccessListFallback checks that a block whose list is missing
// or wrong is executed in order and inserted, and is not a bad block.
func TestInsertChainAccessListFallback(t *testing.T) {
	env, _, blocks, lists := amberParallelChain(t)
	tamper := func(i int, change func(*access.BlockAccessList)) *access.BlockAccessList {
		enc, err := lists[i].Encode()
		require.NoError(t, err)
		list, err := access.Decode(enc)
		require.NoError(t, err)
		change(list)
		return list
	}
	provided := map[int]*access.BlockAccessList{
		// a wrong write makes the phases after it start from another state
		2: tamper(2, func(l *access.BlockAccessList) {
			findAccess(t, l, balStoreAddr).StorageWrites[0].Changes[0].Value[31] ^= 1
		}),
		// a read that did not happen
		3: tamper(3, func(l *access.BlockAccessList) {
			a := findAccess(t, l, balStoreAddr)
			a.StorageReads = append(a.StorageReads, common.HexToHash("0xff"))
		}),
		// the list of another block
		4: lists[3],
		// block 6 (index 5) has no list
	}
	imported := newImportChain(t, env)
	for i, list := range provided {
		imported.ProvideAccessList(blocks[i].Hash(), list)
	}
	parallel, failed := parallelBlockCounter.Count(), parallelFailedCounter.Count()
	_, err := imported.InsertChain(blocks)
	require.NoError(t, err)
	require.Equal(t, blocks[len(blocks)-1].Hash(), imported.CurrentBlock().Hash())
	require.Equal(t, parallel, parallelBlockCounter.Count())
	require.Equal(t, int64(len(provided)), parallelFailedCounter.Count()-failed)
	for _, block := range blocks {
		require.False(t, imported.HasBadBlock(block.Hash()), "block %d", block.NumberU64())
	}

	// A config the phases cannot share runs in order.
	parent := imported.GetHeaderByHash(blocks[2].ParentHash())
	_, _, _, _, _, err = imported.Processor().(*StateProcessor).ProcessWithAccessList(context.Background(), blocks[2], parent, lists[2], vm.Config{EnableInternalTxTracing: true}, 4, true)
	require.ErrorIs(t, err, errParallelUnsupported)

	// A cancelled execution stops.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = imported.ExecuteWithAccessList(ctx, blocks[2], parent, lists[2])
	require.ErrorIs(t, err, context.Canceled)
}

// TestUseBlockAccessListBefore checks that a state that gets the list as it
// loads accounts reads, at the start of every phase, what a state with the
// list applied up front reads: every account, code and slot of the list.
func TestUseBlockAccessListBefore(t *testing.T) {
	check := func(chain *BlockChain, blocks []*types.Block, lists []*access.BlockAccessList) {
		for i, block := range blocks {
			parent := chain.GetHeaderByHash(block.ParentHash())
			list, index := lists[i], access.NewIndex(lists[i])
			for k := 1; k <= len(block.Transactions())+1; k++ {
				eager, err := chain.StateAt(parent.Root)
				require.NoError(t, err)
				require.NoError(t, access.ApplyBefore(eager, list, k))
				lazy, err := chain.StateAt(parent.Root)
				require.NoError(t, err)
				access.UseBefore(lazy, index, k)
				for j := range list.Accounts {
					a := &list.Accounts[j]
					where := fmt.Sprintf("block %d phase %d account %s", block.NumberU64(), k, a.Address.Hex())
					require.Equal(t, eager.Exist(a.Address), lazy.Exist(a.Address), where)
					require.Equal(t, accountFields(t, eager.GetAccount(a.Address)), accountFields(t, lazy.GetAccount(a.Address)), where)
					require.Equal(t, eager.GetCodeHash(a.Address), lazy.GetCodeHash(a.Address), where)
					require.Equal(t, eager.GetCode(a.Address), lazy.GetCode(a.Address), where)
					slots := append([]common.Hash(nil), a.StorageReads...)
					for _, w := range a.StorageWrites {
						slots = append(slots, w.Slot)
					}
					for _, slot := range slots {
						require.Equal(t, eager.GetState(a.Address, slot), lazy.GetState(a.Address, slot), "%s slot %x", where, slot)
						require.Equal(t, eager.GetCommittedState(a.Address, slot), lazy.GetCommittedState(a.Address, slot), "%s slot %x", where, slot)
					}
				}
				require.NoError(t, lazy.Error())
			}
		}
	}
	for _, hardfork := range []string{"shanghai", "prague"} {
		c := newBALHistoryChain(t, hardfork)
		check(c.chain, c.blocks, c.lists)
	}
	_, chain, blocks, lists := amberParallelChain(t)
	check(chain, blocks, lists)
}

// A block can be reported bad without being inserted.
func TestReportBadBlock(t *testing.T) {
	env := newAmberTestEnv(t)
	blocks, _ := GenerateChain(env.config, env.genesis, env.sealer, env.db, 1, amberTestTxs(env))
	chain := newImportChain(t, env)
	require.False(t, chain.HasBadBlock(blocks[0].Hash()))
	chain.ReportBadBlock(blocks[0], nil, errors.New("invalid by execution"))
	require.True(t, chain.HasBadBlock(blocks[0].Hash()))
	bad, err := chain.BadBlocks()
	require.NoError(t, err)
	require.Len(t, bad, 1)
	require.Equal(t, blocks[0].Hash(), bad[0].Hash)
}
