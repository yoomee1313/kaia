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
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

// buildBlock builds the block like a proposer: it executes the transactions of
// the given block on the head state with the recorder on, drops a transaction
// that fails as the worker does, and writes the result with
// WriteBlockWithState.
func buildBlock(t *testing.T, env *balTestEnv, chain *blockchain.BlockChain, like *types.Block, failing *types.Transaction) *types.Block {
	parent := chain.CurrentBlock()
	require.Equal(t, like.ParentHash(), parent.Hash())
	sdb, err := chain.StateAt(parent.Root())
	require.NoError(t, err)
	chain.StartBALRecording(sdb)

	header := types.CopyHeader(like.Header())
	header.Root, header.GasUsed = common.Hash{}, 0
	require.NoError(t, chain.Processor().InitializeState(header, sdb))
	author, err := env.sealer.Author(header)
	require.NoError(t, err)

	var (
		txs      types.Transactions
		receipts types.Receipts
		usedGas  uint64
	)
	pending := append(types.Transactions{failing}, like.Transactions()...)
	for _, tx := range pending {
		snap := sdb.Snapshot()
		sdb.SetTxContext(tx.Hash(), common.Hash{}, len(txs))
		receipt, _, err := chain.ApplyTransaction(env.config, &author, sdb, header, tx, &usedGas, &vm.Config{})
		if err != nil {
			require.Equal(t, failing, tx, "only the failing tx fails")
			sdb.RevertToSnapshot(snap)
			continue
		}
		txs = append(txs, tx)
		receipts = append(receipts, receipt)
	}
	header.GasUsed = usedGas
	block, err := chain.Processor().FinalizeState(header, sdb, txs, receipts)
	require.NoError(t, err)
	require.Equal(t, like.Root(), block.Root())

	_, err = chain.WriteBlockWithState(block, receipts, sdb)
	require.NoError(t, err)
	return block
}

// TestBALRecorderOwnBlocks stores the lists of the blocks a node builds
// itself, between inserted ones, so that the stored range has no gap, and
// keeps a transaction the builder dropped out of the list.
func TestBALRecorderOwnBlocks(t *testing.T) {
	env := newBALTestEnv(t, "prague", 3)
	blocks, _ := blockchain.GenerateChain(env.config, env.genesis, env.sealer, env.db, 6, func(i int, b *blockchain.BlockGen) {
		b.AddTx(env.legacyTx(b, 1, &env.addrs[0], big.NewInt(params.KAIA), nil))
		b.AddTx(env.legacyTx(b, 0, &balStoreAddr, common.Big0, balSlotData(byte(i), byte(i+1))))
	})

	// A recording node that inserts the even blocks and builds the odd ones.
	db := database.NewMemoryDBManager()
	env.gspec.MustCommit(db)
	cacheConfig := &blockchain.CacheConfig{
		CacheSize:           512,
		BlockInterval:       blockchain.DefaultBlockInterval,
		TriesInMemory:       blockchain.DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
		BALRecord:           true,
	}
	chain, err := blockchain.NewBlockChain(db, cacheConfig, env.config, env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(chain.Stop)
	chain.Processor().RegisterBlockStateModule(env.sealer)

	// A tx from an account the blocks do not touch otherwise, with a nonce
	// too high, fails and is dropped.
	failing, err := types.SignTx(types.NewTransaction(5, env.addrs[0], common.Big1, 21000, balGasPrice, nil), env.signer, env.keys[2])
	require.NoError(t, err)

	for i, like := range blocks {
		if i%2 == 1 {
			block := buildBlock(t, env, chain, like, failing)
			require.Equal(t, like.Hash(), block.Hash())
		} else {
			_, err := chain.InsertChain(types.Blocks{like})
			require.NoError(t, err)
		}
	}
	rec := of(chain).rec
	require.Zero(t, rec.sinceLog.failures+rec.stats.failures)
	first, next := db.ReadBALRange()
	require.Equal(t, [2]uint64{1, uint64(len(blocks) + 1)}, [2]uint64{first, next})

	lists := checkBAL(t, chain, blocks)
	for i, block := range blocks {
		full, err := ReadFull(db, block.NumberU64())
		require.NoError(t, err, "block %d", block.NumberU64())
		requireSameList(t, lists[i], full)
		for _, a := range full.Accounts {
			require.NotEqual(t, env.addrs[2], a.Address, "block %d holds the dropped tx", block.NumberU64())
		}
	}
}
