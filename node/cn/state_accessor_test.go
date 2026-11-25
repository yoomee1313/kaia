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

package cn

import (
	"crypto/ecdsa"
	"math/big"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	stakingmock "github.com/kaiachain/kaia/kaiax/staking/mock"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

// TestStateAtBlockReexec regenerates a state whose trie is gone by executing
// blocks on an earlier state without committing them, so the trie database
// of the chain does not get the regenerated state.
func TestStateAtBlockReexec(t *testing.T) {
	config := params.TestChainConfig.Copy()
	key, _ := crypto.GenerateKey()
	from := crypto.PubkeyToAddress(key.PublicKey)
	gspec := &blockchain.Genesis{Config: config, Alloc: blockchain.GenesisAlloc{from: {Balance: new(big.Int).Mul(big.NewInt(1e6), big.NewInt(params.KAIA))}}}
	db := database.NewMemoryDBManager()
	gspec.MustCommit(db)
	chain, err := blockchain.NewBlockChain(db, &blockchain.CacheConfig{
		ArchiveMode:         true,
		CacheSize:           512,
		BlockInterval:       blockchain.DefaultBlockInterval,
		TriesInMemory:       blockchain.DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
	}, config, faker.NewFaker(), vm.Config{})
	require.NoError(t, err)
	t.Cleanup(chain.Stop)
	signer := types.LatestSignerForChainID(config.ChainID)
	blocks, _ := blockchain.GenerateChain(config, chain.Genesis(), faker.NewFaker(), db, 6, func(i int, b *blockchain.BlockGen) {
		tx, err := types.SignTx(types.NewTransaction(b.TxNonce(from), common.Address{0xaa, byte(i)}, big.NewInt(int64(i+1)), 21000, big.NewInt(params.Gkei*25), nil), signer, key)
		require.NoError(t, err)
		b.AddTx(tx)
	})
	_, err = chain.InsertChain(blocks)
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	staking := stakingmock.NewMockStakingModule(ctrl)
	staking.EXPECT().AllocPreloadRef().Return(uint64(1)).AnyTimes()
	staking.EXPECT().FreePreloadRef(gomock.Any()).AnyTimes()
	staking.EXPECT().PreloadFromState(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	cn := &CN{blockchain: chain, stakingModule: staking, config: &Config{}}

	for _, n := range []int{4, 5} {
		db.DeleteTrieNode(blocks[n-1].Root().ExtendZero())
		require.False(t, chain.States().HasTrie(blocks[n-1].Root()))
	}
	for _, readOnly := range []bool{true, false} {
		sdb, release, err := cn.stateAtBlock(blocks[4], 8, nil, readOnly, false)
		require.NoError(t, err)
		require.Equal(t, blocks[4].Root(), sdb.IntermediateRoot(true))
		require.Equal(t, big.NewInt(5), sdb.GetBalance(common.Address{0xaa, 4}))
		release()
		require.False(t, chain.States().HasTrie(blocks[4].Root()), "the regenerated state is not committed")
	}

	// Given the state of the parent, the block is executed on it and checked.
	base, release, err := cn.stateAtBlock(blocks[4], 8, nil, true, false)
	require.NoError(t, err)
	defer release()
	sdb, _, err := cn.stateAtBlock(blocks[5], 8, base, false, false)
	require.NoError(t, err)
	require.Equal(t, blocks[5].Root(), sdb.IntermediateRoot(true))
}

// TestStateAtTransactionFromBAL checks that the state at the start of a tx,
// built from the stored block access list without executing the earlier txs,
// is the state their execution gives, both on the state trie and on a
// read-only state of the BAL history.
func TestStateAtTransactionFromBAL(t *testing.T) {
	config := params.TestChainConfig.Copy()
	key, _ := crypto.GenerateKey()
	from := crypto.PubkeyToAddress(key.PublicKey)
	key2, _ := crypto.GenerateKey()
	from2 := crypto.PubkeyToAddress(key2.PublicKey)
	funds := new(big.Int).Mul(big.NewInt(1e6), big.NewInt(params.KAIA))
	gspec := &blockchain.Genesis{Config: config, Alloc: blockchain.GenesisAlloc{from: {Balance: funds}, from2: {Balance: funds}}}
	db := database.NewMemoryDBManager()
	gspec.MustCommit(db)
	chain, err := blockchain.NewBlockChain(db, &blockchain.CacheConfig{
		ArchiveMode:         true,
		CacheSize:           512,
		BlockInterval:       blockchain.DefaultBlockInterval,
		TriesInMemory:       blockchain.DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
		HistoryMode:         database.HistoryModeBAL,
	}, config, faker.NewFaker(), vm.Config{})
	require.NoError(t, err)
	t.Cleanup(chain.Stop)

	// A counter contract: each call adds one to slot 0.
	runtime := common.FromHex("600054600101600055" + "00")
	initCode := append(common.FromHex("600a600c600039600a6000f3"), runtime...)
	counter := crypto.CreateAddress(from, 0)
	gasPrice := big.NewInt(params.Gkei * 25)
	signer := types.LatestSignerForChainID(config.ChainID)
	sign := func(tx *types.Transaction, k *ecdsa.PrivateKey) *types.Transaction {
		tx, err := types.SignTx(tx, signer, k)
		require.NoError(t, err)
		return tx
	}
	blocks, _ := blockchain.GenerateChain(config, chain.Genesis(), faker.NewFaker(), db, 2, func(i int, b *blockchain.BlockGen) {
		if i == 0 {
			b.AddTx(sign(types.NewContractCreation(b.TxNonce(from), common.Big0, 200000, gasPrice, initCode), key))
			return
		}
		fresh := common.Address{0xaa}
		b.AddTx(sign(types.NewTransaction(b.TxNonce(from), fresh, big.NewInt(7), 21000, gasPrice, nil), key))
		b.AddTx(sign(types.NewTransaction(b.TxNonce(from), counter, common.Big0, 100000, gasPrice, nil), key))
		b.AddTx(sign(types.NewTransaction(b.TxNonce(from2), counter, common.Big0, 100000, gasPrice, nil), key2))
		b.AddTx(sign(types.NewTransaction(b.TxNonce(from2), from, big.NewInt(11), 21000, gasPrice, nil), key2))
		b.AddTx(sign(types.NewTransaction(b.TxNonce(from), counter, common.Big0, 100000, gasPrice, nil), key))
	})
	_, err = chain.InsertChain(blocks)
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	staking := stakingmock.NewMockStakingModule(ctrl)
	staking.EXPECT().AllocPreloadRef().Return(uint64(1)).AnyTimes()
	staking.EXPECT().FreePreloadRef(gomock.Any()).AnyTimes()
	staking.EXPECT().PreloadFromState(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	cn := &CN{blockchain: chain, chainDB: db, chainConfig: config, stakingModule: staking, config: &Config{}}

	block := blocks[1]
	enc := db.ReadBAL(block.Hash(), block.NumberU64())
	require.NotNil(t, enc, "the block access list is stored")
	list, err := types.DecodeBAL(enc)
	require.NoError(t, err)
	next := chain.CurrentBlock().NumberU64() + 1
	require.Eventually(t, func() bool {
		chain.States().BALs().SignalIndex()
		_, n, ok := chain.States().BALs().Indexed()
		return ok && n == next
	}, 5*time.Second, 10*time.Millisecond, "the BAL index covers the chain")

	for k := 1; k < len(block.Transactions()); k++ {
		// From the list.
		msg, _, _, fromBAL, release, err := cn.stateAtTransaction(block, k, 0, nil, true, false)
		require.NoError(t, err)

		// By executing the txs before k, without the list.
		db.DeleteBAL(block.Hash(), block.NumberU64())
		wantMsg, _, _, executed, releaseExec, err := cn.stateAtTransaction(block, k, 0, nil, true, false)
		require.NoError(t, db.WriteBAL(block.Hash(), block.NumberU64(), enc))
		require.NoError(t, err)

		require.Equal(t, wantMsg.ValidatedSender(), msg.ValidatedSender(), "tx %d", k)
		require.Equal(t, wantMsg.Nonce(), msg.Nonce(), "tx %d", k)
		require.Equal(t, executed.IntermediateRoot(true), fromBAL.IntermediateRoot(true), "state before tx %d", k)

		// On a read-only state of the BAL history at the parent.
		history, err := state.OpenBALState(chain.States().BALs(), blocks[0].Header(), chain.StateCache())
		require.NoError(t, err)
		require.True(t, history.ReadOnly())
		require.NoError(t, state.ApplyBALBefore(history, list, k+1))
		for _, a := range list.Accounts {
			addr := a.Address
			require.Equal(t, executed.Exist(addr), history.Exist(addr), "exist %s before tx %d", addr.Hex(), k)
			require.Equal(t, executed.GetBalance(addr), history.GetBalance(addr), "balance %s before tx %d", addr.Hex(), k)
			require.Equal(t, executed.GetNonce(addr), history.GetNonce(addr), "nonce %s before tx %d", addr.Hex(), k)
			require.Equal(t, executed.GetCode(addr), history.GetCode(addr), "code %s before tx %d", addr.Hex(), k)
			for _, sc := range a.StorageChanges {
				require.Equal(t, executed.GetState(addr, sc.Slot), history.GetState(addr, sc.Slot), "slot %s %s before tx %d", addr.Hex(), sc.Slot.Hex(), k)
			}
		}
		release()
		releaseExec()
	}
	// The counter was called three times, once before the last tx.
	_, _, _, sdb, release, err := cn.stateAtTransaction(block, 4, 0, nil, true, false)
	require.NoError(t, err)
	defer release()
	require.Equal(t, common.BigToHash(big.NewInt(2)), sdb.GetState(counter, common.Hash{}))
}
