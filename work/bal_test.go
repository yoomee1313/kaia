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
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

// balStoreCode stores calldata[32:64] at slot calldata[0:32] after reading it.
var balStoreCode = []byte{
	byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD), byte(vm.SLOAD), byte(vm.POP),
	byte(vm.PUSH1), 0x20, byte(vm.CALLDATALOAD),
	byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD),
	byte(vm.SSTORE),
	byte(vm.STOP),
}

// TestProducerBlockAccessList builds an Amber block with the worker, from txs
// among which one cannot be included, and inserts it into another chain, which
// re-executes it and checks the hash of the whole list, reads included.
func TestProducerBlockAccessList(t *testing.T) {
	var (
		config   = params.TestKaiaConfig("prague")
		gasPrice = big.NewInt(25 * params.Gkei)
		funds    = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.KAIA))
		store    = common.HexToAddress("0x000000000000000000000000000000000000570e")
		keys     []*ecdsa.PrivateKey
		addrs    []common.Address
	)
	config.AmberCompatibleBlock = big.NewInt(1)
	alloc := blockchain.GenesisAlloc{store: {Code: balStoreCode, Balance: common.Big0}}
	for i := range 3 {
		key, _ := crypto.GenerateKey()
		keys, addrs = append(keys, key), append(addrs, crypto.PubkeyToAddress(key.PublicKey))
		balance := funds
		if i == 2 {
			balance = big.NewInt(1) // cannot pay for gas
		}
		alloc[addrs[i]] = blockchain.GenesisAccount{Balance: balance}
	}
	gspec := &blockchain.Genesis{Config: config, Alloc: alloc}
	engine := faker.NewFaker()
	newChain := func() *blockchain.BlockChain {
		db := database.NewMemoryDBManager()
		gspec.MustCommit(db)
		chain, err := blockchain.NewBlockChain(db, nil, config, engine, vm.Config{})
		require.NoError(t, err)
		t.Cleanup(chain.Stop)
		return chain
	}
	producerChain, validatorChain := newChain(), newChain()
	genesis := producerChain.Genesis()

	// The header of block 1 as the chain maker builds it, before execution.
	genDB := database.NewMemoryDBManager()
	gspec.MustCommit(genDB)
	template, _ := blockchain.GenerateChain(config, genesis, engine, genDB, 1, func(int, *blockchain.BlockGen) {})
	header := types.CopyHeader(template[0].Header())
	header.GasUsed, header.Root, header.BlockAccessListHash = 0, common.Hash{}, nil

	signer := types.LatestSignerForChainID(config.ChainID)
	slotData := func(slot, value byte) []byte {
		data := make([]byte, 64)
		data[31], data[63] = slot, value
		return data
	}
	sign := func(from int, nonce uint64, to common.Address, data []byte) *types.Transaction {
		tx, err := types.SignTx(types.NewTransaction(nonce, to, common.Big1, 100_000, gasPrice, data), signer, keys[from])
		require.NoError(t, err)
		return tx
	}
	// The tx of addrs[2] fails for lack of funds and is left out. It is the
	// highest priced, so it runs first and loads the store contract and its slot.
	failing, err := types.SignTx(types.NewTransaction(0, store, common.Big0, 100_000, new(big.Int).Mul(gasPrice, big.NewInt(2)), slotData(1, 0x11)), signer, keys[2])
	require.NoError(t, err)
	txs := types.NewTransactionsByPriceAndNonce(signer, map[common.Address]types.Transactions{
		addrs[0]: {sign(0, 0, store, slotData(1, 0x22)), sign(0, 1, addrs[1], nil)},
		addrs[1]: {sign(1, 0, store, slotData(2, 0x33))},
		addrs[2]: {failing},
	}, header.BaseFee)

	statedb, err := producerChain.StateAt(genesis.Root())
	require.NoError(t, err)
	exec := NewDefaultExecutor(config, producerChain, addrs[0], vm.Config{})
	// In the order of the worker: the recorder, InitializeState (EIP-2935
	// writes the parent hash), then the executor of the consensus engine.
	startBlockAccessRecorder(config, statedb, header)
	producerChain.Processor().InitializeState(header, statedb)
	require.NoError(t, exec.ResetWithState(statedb, header))
	task := NewTask(config, signer, statedb, header)
	task.ApplyTransactions(txs, producerChain, addrs[0], nil)
	require.Len(t, task.Transactions(), 3, "the failing tx is left out")
	block, err := producerChain.Processor().FinalizeState(header, statedb, task.Transactions(), task.Receipts())
	require.NoError(t, err)
	require.NotNil(t, block.Header().BlockAccessListHash)
	produced, err := statedb.BlockAccessList().Encode()
	require.NoError(t, err)
	for _, a := range statedb.BlockAccessList().Accounts {
		require.NotEqual(t, addrs[2], a.Address, "the sender of the dropped tx is recorded")
	}

	// The validator re-executes the block and checks its list hash.
	_, err = validatorChain.InsertChain(types.Blocks{block})
	require.NoError(t, err)
	validated, err := validatorChain.StateAt(genesis.Root())
	require.NoError(t, err)
	validated.EnableBlockAccessRecorder()
	_, _, _, _, _, err = validatorChain.Processor().Process(context.Background(), block, validated, vm.Config{})
	require.NoError(t, err)
	validated.IntermediateRoot(true)
	replayed, err := validated.BlockAccessList().Encode()
	require.NoError(t, err)
	require.Equal(t, replayed, produced)
}
