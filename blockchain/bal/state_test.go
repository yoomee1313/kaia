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
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

// waitBALIndex waits until the BAL index of chain covers blocks before next.
func waitBALIndex(t *testing.T, chain *blockchain.BlockChain, next uint64) {
	t.Helper()
	for i := 0; i < 500; i++ {
		of(chain).signalIndex()
		if _, n, ok := Indexed(chain); ok && n == next {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, n, _ := Indexed(chain)
	t.Fatalf("BAL index reached block %d, want %d", n, next)
}

// TestBALStateAt opens the state of every block from the BAL history, over
// the state trie of the block and without it, and compares it with the trie;
// then drops state tries and checks that StateAt reads the BAL history alone,
// which re-executes the next block, refuses proofs and iteration, and follows
// a rewind and a reorg.
func TestBALStateAt(t *testing.T) {
	t.Run("shanghai", func(t *testing.T) { testBALStateAt(t, "shanghai") })
	t.Run("prague", func(t *testing.T) { testBALStateAt(t, "prague") })
}

func compareStates(t *testing.T, n int, want, got *state.StateDB, accounts map[common.Address]bool, slots map[common.Address]map[common.Hash]bool) {
	t.Helper()
	for addr := range accounts {
		msg := fmt.Sprintf("block %d account %s", n, addr.Hex())
		require.Equal(t, want.Exist(addr), got.Exist(addr), msg)
		require.Equal(t, want.GetBalance(addr), got.GetBalance(addr), msg)
		require.Equal(t, want.GetNonce(addr), got.GetNonce(addr), msg)
		require.Equal(t, want.GetCodeHash(addr), got.GetCodeHash(addr), msg)
		require.Equal(t, common.CopyBytes(want.GetCode(addr)), common.CopyBytes(got.GetCode(addr)), msg)
		require.Equal(t, want.IsProgramAccount(addr), got.IsProgramAccount(addr), msg)
		require.Equal(t, accountFields(t, want.GetAccount(addr)), accountFields(t, got.GetAccount(addr)), msg)
		for slot := range slots[addr] {
			require.Equal(t, want.GetState(addr, slot), got.GetState(addr, slot), "%s slot %x", msg, slot)
		}
	}
	require.NoError(t, got.Error(), "block %d", n)
}

func testBALStateAt(t *testing.T, hardfork string) {
	c := newBALHistoryChainWith(t, hardfork, regenCacheConfig(true))
	env, chain, blocks, lists := c.env, c.chain, c.blocks, c.lists
	waitBALIndex(t, chain, uint64(len(blocks)+1))

	// Every block executed on the state of its parent from the BAL history
	// reaches its root, a recreated contract included.
	for _, block := range blocks[1:] {
		parent := chain.GetHeaderByHash(block.ParentHash())
		sdb, err := chain.StateAt(parent.Root)
		require.NoError(t, err)
		_, _, _, _, _, err = chain.Processor().Process(context.Background(), block, sdb, vm.Config{})
		require.NoError(t, err)
		require.Equal(t, block.Root(), sdb.IntermediateRoot(true), "block %d", block.NumberU64())
	}

	accounts := map[common.Address]bool{c.untouch: true, balSystemAddr: true}
	slots := map[common.Address]map[common.Hash]bool{}
	for _, list := range lists {
		for _, a := range list.Accounts {
			accounts[a.Address] = true
			for _, w := range a.StorageWrites {
				if slots[a.Address] == nil {
					slots[a.Address] = make(map[common.Hash]bool)
				}
				slots[a.Address][w.Slot] = true
			}
		}
	}
	headers := []*types.Header{env.genesis.Header()}
	for _, block := range blocks {
		headers = append(headers, block.Header())
	}
	for n, header := range headers {
		want, err := state.New(header.Root, chain.StateCache(), nil, nil)
		require.NoError(t, err)
		readOnly, err := of(chain).archive.StateAt(header.Root, nil)
		require.NoError(t, err, "block %d", n)
		require.True(t, ReadOnly(readOnly.Database()))
		onTrie, err := chain.StateAt(header.Root)
		require.NoError(t, err, "block %d", n)
		if n == len(headers)-1 {
			require.Same(t, chain.StateCache(), onTrie.Database(), "the head is read from the trie")
		} else {
			require.IsType(t, &Database{}, onTrie.Database())
			require.False(t, ReadOnly(onTrie.Database()))
		}
		for _, got := range []*state.StateDB{readOnly, onTrie} {
			compareStates(t, n, want, got, accounts, slots)
		}
		require.Equal(t, header.Root, onTrie.IntermediateRoot(true), "block %d", n)
		if onTrie.IsProgramAccount(c.contract) {
			require.NoError(t, onTrie.StorageTrie(c.contract).Prove(common.Hash{}.Bytes(), 0, database.NewMemoryDBManager()), "block %d", n)
		}
	}

	// Without the trie of block 4, StateAt reads it from the BAL history, and
	// block 5 executes on it as on the trie.
	env.db.DeleteTrieNode(headers[4].Root.ExtendZero())
	_, err := state.New(headers[4].Root, state.NewDatabase(env.db), nil, nil)
	require.Error(t, err)
	sdb, err := chain.StateAt(headers[4].Root)
	require.NoError(t, err)
	require.True(t, ReadOnly(sdb.Database()))
	receipts, _, usedGas, _, _, err := chain.Processor().Process(context.Background(), blocks[4], sdb, vm.Config{})
	require.NoError(t, err)
	require.Equal(t, blocks[4].GasUsed(), usedGas)
	wantReceipts, err := rlp.EncodeToBytes(chain.GetReceiptsByBlockHash(blocks[4].Hash()))
	require.NoError(t, err)
	gotReceipts, err := rlp.EncodeToBytes(receipts)
	require.NoError(t, err)
	require.Equal(t, wantReceipts, gotReceipts)
	_, err = sdb.Commit(true)
	require.ErrorIs(t, err, ErrReadOnly)

	// Proofs and iteration need the trie.
	sdb, err = chain.StateAt(headers[4].Root)
	require.NoError(t, err)
	require.ErrorIs(t, sdb.StorageTrie(c.contract).Prove(common.Hash{}.Bytes(), 0, nil), ErrNoProof)
	it := sdb.StorageTrie(c.contract).NodeIterator(nil)
	require.False(t, it.Next(true))
	require.ErrorIs(t, it.Error(), ErrNoIteration)

	// A root the history does not have keeps the trie error.
	_, err = chain.StateAt(common.Hash{0x01})
	require.Error(t, err)
	var missing *statedb.MissingNodeError
	require.ErrorAs(t, err, &missing)

	// A rewind rebuilds the index up to the new head, and a state opened
	// before keeps failing instead of reading the rebuilt index.
	old, err := chain.StateAt(headers[4].Root)
	require.NoError(t, err)
	require.NoError(t, chain.SetHead(6))
	waitBALIndex(t, chain, 7)
	old.GetBalance(env.addrs[0])
	require.ErrorIs(t, old.Error(), ErrRebuilt)
	sdb, err = chain.StateAt(headers[4].Root)
	require.NoError(t, err)
	require.Equal(t, headers[4].Root, sdb.IntermediateRoot(true))
	_, err = of(chain).archive.StateAt(headers[8].Root, nil)
	require.ErrorIs(t, err, ErrNotCovered)

	// A reorg that replaced the last indexed block rebuilds the index.
	old, err = chain.StateAt(headers[4].Root)
	require.NoError(t, err)
	fork := types.CopyHeader(headers[6])
	fork.Root = common.Hash{0xf0}
	env.db.WriteHeader(fork)
	env.db.WriteBAL(fork.Hash(), 6, env.db.ReadBAL(headers[6].Hash(), 6))
	env.db.WriteCanonicalHash(fork.Hash(), 6)
	require.NoError(t, of(chain).archive.Update(nil))
	old.GetBalance(env.addrs[0])
	require.ErrorIs(t, old.Error(), ErrRebuilt)
	_, err = of(chain).archive.StateAt(headers[6].Root, nil)
	require.ErrorIs(t, err, ErrNotCovered)

	// A list stored before its block is canonical waits for the next update.
	first, _ := env.db.ReadBALRange()
	env.db.WriteBAL(common.Hash{0x07}, 7, env.db.ReadBAL(fork.Hash(), 6))
	require.NoError(t, env.db.WriteBALRange(first, 8))
	require.NoError(t, of(chain).archive.Update(nil))
	_, next, _ := Indexed(chain)
	require.Equal(t, uint64(7), next)
}
