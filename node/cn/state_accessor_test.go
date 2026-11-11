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
	"math/big"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/kaiachain/kaia/blockchain"
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
		require.False(t, chain.HasState(blocks[n-1].Root()))
	}
	for _, readOnly := range []bool{true, false} {
		sdb, release, err := cn.stateAtBlock(blocks[4], 8, nil, readOnly, false)
		require.NoError(t, err)
		require.Equal(t, blocks[4].Root(), sdb.IntermediateRoot(true))
		require.Equal(t, big.NewInt(5), sdb.GetBalance(common.Address{0xaa, 4}))
		release()
		require.False(t, chain.HasState(blocks[4].Root()), "the regenerated state is not committed")
	}

	// Given the state of the parent, the block is executed on it and checked.
	base, release, err := cn.stateAtBlock(blocks[4], 8, nil, true, false)
	require.NoError(t, err)
	defer release()
	sdb, _, err := cn.stateAtBlock(blocks[5], 8, base, false, false)
	require.NoError(t, err)
	require.Equal(t, blocks[5].Root(), sdb.IntermediateRoot(true))
}
