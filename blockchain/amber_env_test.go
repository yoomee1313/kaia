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
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

// Test helpers of the Amber block access list tests of package blockchain.
// The block access list tests themselves are in package bal.

var (
	balSystemAddr = common.HexToAddress("0x000000000000000000000000000000000000ba50")
	balReward     = big.NewInt(params.KAIA)
	balGasPrice   = big.NewInt(25 * params.Gkei)
	balFunds      = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.KAIA))
)

// balTestSealer is a faker that also changes state in InitializeState and
// FinalizeState, standing in for system tx calls and block rewards.
type balTestSealer struct {
	*faker.Faker
	initAddr common.Address // gets 1 peb in InitializeState
}

func (s balTestSealer) InitializeState(header *types.Header, statedb *state.StateDB) error {
	// Not finalised on purpose: the recorder must keep these changes out of tx 1.
	statedb.SetState(balSystemAddr, common.BigToHash(header.Number), header.ParentHash)
	statedb.AddBalance(s.initAddr, common.Big1)
	return nil
}

func (s balTestSealer) FinalizeState(header *types.Header, statedb *state.StateDB, txs []*types.Transaction, receipts []*types.Receipt) error {
	statedb.AddBalance(header.Rewardbase, balReward)
	return nil
}

type balTestEnv struct {
	t       *testing.T
	config  *params.ChainConfig
	signer  types.Signer
	sealer  balTestSealer
	db      database.DBManager
	gspec   *Genesis
	genesis *types.Block
	keys    []*ecdsa.PrivateKey
	addrs   []common.Address
}

func newBALTestEnv(t *testing.T, hardfork string, numKeys int) *balTestEnv {
	env := &balTestEnv{t: t, config: params.TestKaiaConfig(hardfork), db: database.NewMemoryDBManager()}
	env.signer = types.LatestSignerForChainID(env.config.ChainID)
	alloc := GenesisAlloc{
		balSystemAddr: {Code: []byte{byte(vm.STOP)}, Balance: common.Big0},
		// The store contract is the delegation target of EIP-7702 tests.
		balStoreAddr: {Code: balStoreCode(), Balance: common.Big0},
	}
	for range numKeys {
		key, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(key.PublicKey)
		env.keys = append(env.keys, key)
		env.addrs = append(env.addrs, addr)
		alloc[addr] = GenesisAccount{Balance: balFunds}
	}
	env.sealer = balTestSealer{Faker: faker.NewFaker(), initAddr: env.addrs[0]}
	env.gspec = &Genesis{Config: env.config, Alloc: alloc}
	env.genesis = env.gspec.MustCommit(env.db)
	return env
}

var balStoreAddr = common.HexToAddress("0x000000000000000000000000000000000000570e")

// balStoreCode stores calldata[32:64] at slot calldata[0:32].
func balStoreCode() []byte {
	return []byte{
		byte(vm.PUSH1), 0x20, byte(vm.CALLDATALOAD),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD),
		byte(vm.SSTORE),
		byte(vm.STOP),
	}
}

func balSlotData(slot, value byte) []byte {
	data := make([]byte, 64)
	data[31], data[63] = slot, value
	return data
}

func (env *balTestEnv) legacyTx(b *BlockGen, from int, to *common.Address, amount *big.Int, data []byte) *types.Transaction {
	nonce := b.TxNonce(env.addrs[from])
	var tx *types.Transaction
	if to == nil {
		tx = types.NewContractCreation(nonce, amount, 1_000_000, balGasPrice, data)
	} else {
		tx = types.NewTransaction(nonce, *to, amount, 1_000_000, balGasPrice, data)
	}
	tx, err := types.SignTx(tx, env.signer, env.keys[from])
	require.NoError(env.t, err)
	return tx
}

// generate makes the blocks and inserts them into a new chain.
func (env *balTestEnv) generate(n int, gen func(int, *BlockGen)) (*BlockChain, []*types.Block) {
	return env.generateWithCache(nil, n, gen)
}

func (env *balTestEnv) generateWithCache(cacheConfig *CacheConfig, n int, gen func(int, *BlockGen)) (*BlockChain, []*types.Block) {
	blocks, _ := GenerateChain(env.config, env.genesis, env.sealer, env.db, n, gen)
	chain, err := NewBlockChain(env.db, cacheConfig, env.config, env.sealer, vm.Config{})
	require.NoError(env.t, err)
	env.t.Cleanup(chain.Stop)
	chain.Processor().RegisterBlockStateModule(env.sealer)
	_, err = chain.InsertChain(blocks)
	require.NoError(env.t, err)
	return chain, blocks
}

// newAmberTestEnv returns an env whose Amber hardfork starts at block 2. The
// genesis is committed before, as a chain config refuses an Amber block that
// does not start a header segment.
func newAmberTestEnv(t *testing.T) *balTestEnv {
	env := newBALTestEnv(t, "prague", 2)
	env.config.AmberCompatibleBlock = big.NewInt(2)
	return env
}

func amberTestTxs(env *balTestEnv) func(int, *BlockGen) {
	return func(i int, b *BlockGen) {
		b.AddTx(env.legacyTx(b, 0, &env.addrs[1], big.NewInt(params.KAIA), nil))
		b.AddTx(env.legacyTx(b, 1, &balStoreAddr, common.Big0, balSlotData(byte(i), byte(i+1))))
	}
}

// newImportChain opens an empty chain with the genesis of env.
func newImportChain(t *testing.T, env *balTestEnv) *BlockChain {
	db := database.NewMemoryDBManager()
	commitBeforeAmber(env.gspec, db)
	chain, err := NewBlockChain(db, nil, env.config, env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(chain.Stop)
	chain.Processor().RegisterBlockStateModule(env.sealer)
	return chain
}

// commitBeforeAmber commits g to db as newAmberTestEnv does.
func commitBeforeAmber(g *Genesis, db database.DBManager) *types.Block {
	amber := g.Config.AmberCompatibleBlock
	g.Config.AmberCompatibleBlock = nil
	defer func() { g.Config.AmberCompatibleBlock = amber }()
	return g.MustCommit(db)
}
