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

package work

import (
	"context"
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

// amberLimitResult is what a block producer kept from the txs it was given.
type amberLimitResult struct {
	txs     int
	cost    uint64 // computation cost of the kept txs
	entries uint64 // entries of the block access list before FinalizeState
	leftOut []*types.Transaction
}

// produceWithLimits builds block 1 with the worker from n txs of one sender,
// each storing a new slot of the store contract, under the given Amber block
// (nil: no Amber) and limits (0: default).
func produceWithLimits(t *testing.T, amber *big.Int, costLimit, entryLimit uint64, n int) amberLimitResult {
	var (
		config   = params.TestKaiaConfig("prague")
		gasPrice = big.NewInt(25 * params.Gkei)
		store    = common.HexToAddress("0x000000000000000000000000000000000000570e")
	)
	config.AmberCompatibleBlock = amber
	config.AmberBlockComputationCostLimit = costLimit
	config.AmberBlockAccessListEntryLimit = entryLimit
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	gspec := &blockchain.Genesis{Config: config, Alloc: blockchain.GenesisAlloc{
		store:  {Code: balStoreCode, Balance: common.Big0},
		sender: {Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.KAIA))},
	}}
	engine := faker.NewFaker()
	db := database.NewMemoryDBManager()
	gspec.MustCommit(db)
	chain, err := blockchain.NewBlockChain(db, nil, config, engine, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(chain.Stop)
	genesis := chain.Genesis()

	genDB := database.NewMemoryDBManager()
	gspec.MustCommit(genDB)
	template, _ := blockchain.GenerateChain(config, genesis, engine, genDB, 1, func(int, *blockchain.BlockGen) {})
	header := types.CopyHeader(template[0].Header())
	header.GasUsed, header.Root, header.BlockAccessListHash = 0, common.Hash{}, nil

	signer := types.LatestSignerForChainID(config.ChainID)
	var all types.Transactions
	for i := range n {
		data := make([]byte, 64)
		data[31], data[63] = byte(i+1), 0x22
		tx, err := types.SignTx(types.NewTransaction(uint64(i), store, common.Big0, 100_000, gasPrice, data), signer, key)
		require.NoError(t, err)
		all = append(all, tx)
	}
	txs := types.NewTransactionsByPriceAndNonce(signer, map[common.Address]types.Transactions{sender: all}, header.BaseFee)

	statedb, err := chain.StateAt(genesis.Root())
	require.NoError(t, err)
	startBlockAccessRecorder(config, statedb, header)
	require.NoError(t, chain.Processor().InitializeState(header, statedb))
	task := NewTask(config, signer, statedb, header)
	task.ApplyTransactions(txs, chain, sender, nil)

	entries, _ := access.Stats(statedb)
	res := amberLimitResult{txs: len(task.Transactions()), cost: task.computationCost, entries: entries}
	res.leftOut = all[res.txs:]

	// The block is valid: another node imports it.
	block, err := chain.Processor().FinalizeState(header, statedb, task.Transactions(), task.Receipts())
	require.NoError(t, err)
	importer := database.NewMemoryDBManager()
	gspec.MustCommit(importer)
	validator, err := blockchain.NewBlockChain(importer, nil, config, engine, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(validator.Stop)
	_, err = validator.InsertChain(types.Blocks{block})
	require.NoError(t, err)

	// Validators get the same computation cost from Process.
	replayed, err := validator.StateAt(genesis.Root())
	require.NoError(t, err)
	_, _, _, _, stats, err := validator.Processor().Process(context.Background(), block, replayed, vm.Config{})
	require.NoError(t, err)
	if amber != nil && config.IsAmberForkEnabled(block.Number()) {
		require.Equal(t, res.cost, stats.ComputationCost)
	}
	require.NotZero(t, stats.ComputationCost)
	return res
}

// TestProducerAmberLimits checks that a block producer from the Amber hardfork
// keeps a tx that makes the block reach a limit exactly, leaves out the tx
// that would cross it by one and closes the block, and applies no limit
// before Amber.
func TestProducerAmberLimits(t *testing.T) {
	amber := common.Big0

	// The cost and the entries of the first two txs.
	two := produceWithLimits(t, amber, 0, 0, 2)
	require.Equal(t, 2, two.txs)
	require.NotZero(t, two.cost)
	one := produceWithLimits(t, amber, 0, 0, 1)
	require.Less(t, one.cost, two.cost)
	require.Less(t, one.entries, two.entries)

	t.Run("computation cost", func(t *testing.T) {
		res := produceWithLimits(t, amber, two.cost, 0, 3)
		require.Equal(t, 2, res.txs, "exactly the limit")
		require.Equal(t, two.cost, res.cost)
		for _, tx := range res.leftOut {
			require.False(t, tx.IsMarkedUnexecutable(), "a tx left out by a block limit stays executable")
		}

		res = produceWithLimits(t, amber, two.cost-1, 0, 3)
		require.Equal(t, 1, res.txs, "the second tx crosses the limit by one")
		require.Equal(t, one.cost, res.cost)
	})

	t.Run("block access list entries", func(t *testing.T) {
		// The producer leaves room for FinalizeState.
		res := produceWithLimits(t, amber, 0, two.entries+amberFinalizeEntryReserve, 3)
		require.Equal(t, 2, res.txs, "exactly the limit")
		require.Equal(t, two.entries, res.entries)

		res = produceWithLimits(t, amber, 0, two.entries+amberFinalizeEntryReserve-1, 3)
		require.Equal(t, 1, res.txs, "the second tx crosses the limit by one")
	})

	t.Run("before Amber", func(t *testing.T) {
		res := produceWithLimits(t, big.NewInt(params.HeaderSegmentSize), one.cost-1, one.entries, 3)
		require.Equal(t, 3, res.txs)
		require.Zero(t, res.cost, "not counted before Amber")
		res = produceWithLimits(t, nil, one.cost-1, one.entries, 3)
		require.Equal(t, 3, res.txs)
	})
}
