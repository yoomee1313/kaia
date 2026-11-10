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
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/holiman/uint256"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/blockchain/types/accountkey"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

// Test helpers of the Amber block access list tests of package blockchain.
// The block access list tests themselves are in package bal.

var (
	balSystemAddr = common.HexToAddress("0x000000000000000000000000000000000000ba50")
	balFactory    = common.HexToAddress("0x000000000000000000000000000000000000fac7")
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
		balFactory:    {Code: balFactoryCode(), Balance: common.Big0},
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

// balDestructibleCode works like balStoreCode, and self-destructs on empty calldata.
func balDestructibleCode() []byte {
	return []byte{
		byte(vm.CALLDATASIZE), byte(vm.ISZERO), byte(vm.PUSH1), 13, byte(vm.JUMPI),
		byte(vm.PUSH1), 0x20, byte(vm.CALLDATALOAD),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD),
		byte(vm.SSTORE),
		byte(vm.STOP),
		byte(vm.JUMPDEST), byte(vm.CALLER), byte(vm.SELFDESTRUCT),
	}
}

// balFactoryCode deploys calldata as init code with CREATE2 and salt 0.
func balFactoryCode() []byte {
	return []byte{
		byte(vm.CALLDATASIZE), byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CALLDATACOPY),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATASIZE), byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CREATE2),
		byte(vm.POP), byte(vm.STOP),
	}
}

// balInitCode stores 0x11 at slot 1 and 0x22 at slot 2, then returns runtime.
func balInitCode(runtime []byte) []byte {
	ctor := []byte{
		byte(vm.PUSH1), 0x11, byte(vm.PUSH1), 0x01, byte(vm.SSTORE),
		byte(vm.PUSH1), 0x22, byte(vm.PUSH1), 0x02, byte(vm.SSTORE),
	}
	// CODECOPY(0, offset, size) and RETURN(0, size), 12 bytes in total.
	offset := len(ctor) + 12
	ctor = append(ctor,
		byte(vm.PUSH1), byte(len(runtime)), byte(vm.PUSH1), byte(offset), byte(vm.PUSH1), 0x00, byte(vm.CODECOPY),
		byte(vm.PUSH1), byte(len(runtime)), byte(vm.PUSH1), 0x00, byte(vm.RETURN),
	)
	return append(ctor, runtime...)
}

func (env *balTestEnv) kaiaTx(txType types.TxType, values map[types.TxValueKeyType]interface{}, keys, feePayerKeys []*ecdsa.PrivateKey) *types.Transaction {
	values[types.TxValueKeyGasLimit] = uint64(1_000_000)
	values[types.TxValueKeyGasPrice] = balGasPrice
	tx, err := types.NewTransactionWithMap(txType, values)
	require.NoError(env.t, err)
	require.NoError(env.t, tx.SignWithKeys(env.signer, keys))
	if feePayerKeys != nil {
		require.NoError(env.t, tx.SignFeePayerWithKeys(env.signer, feePayerKeys))
	}
	return tx
}

func (env *balTestEnv) setCodeTx(b *BlockGen, from int, target common.Address, data []byte) *types.Transaction {
	addr := env.addrs[from]
	nonce := b.TxNonce(addr)
	auth, err := types.SignSetCode(env.keys[from], types.SetCodeAuthorization{
		ChainID: *uint256.MustFromBig(env.config.ChainID),
		Address: target,
		Nonce:   nonce + 1, // the sender is the authority, so its nonce is already bumped
	})
	require.NoError(env.t, err)
	authList := []types.SetCodeAuthorization{auth}
	intrinsicGas, err := types.IntrinsicGas(data, nil, authList, false, env.config.Rules(b.Number()))
	require.NoError(env.t, err)
	tx, err := types.SignTx(types.NewMessage(addr, &addr, nonce, nil, 1_000_000, nil, balGasPrice,
		balGasPrice, nil, data, false, intrinsicGas, nil, nil, nil, nil, authList), env.signer, env.keys[from])
	require.NoError(env.t, err)
	return tx
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	return key
}

// checkBAL re-executes every block with and without the recorder, then checks
// that the recorded list survives encoding and reproduces the state root when
// applied to the parent state.
func checkBAL(t *testing.T, chain *BlockChain, blocks []*types.Block) []*access.BlockAccessList {
	lists := make([]*access.BlockAccessList, len(blocks))
	for i, block := range blocks {
		parent := chain.GetHeaderByHash(block.ParentHash())
		require.NotNil(t, parent)

		process := func(record bool) (types.Receipts, uint64, common.Hash, *state.StateDB) {
			statedb, err := chain.StateAt(parent.Root)
			require.NoError(t, err)
			if record {
				access.EnableRecorder(statedb)
			}
			receipts, _, usedGas, _, _, err := chain.Processor().Process(context.Background(), block, statedb, vm.Config{})
			require.NoError(t, err)
			return receipts, usedGas, statedb.IntermediateRoot(true), statedb
		}
		receiptsOff, gasOff, rootOff := func() (types.Receipts, uint64, common.Hash) {
			r, g, root, statedb := process(false)
			require.Nil(t, access.Recorded(statedb))
			return r, g, root
		}()
		receiptsOn, gasOn, rootOn, statedb := process(true)

		require.Equal(t, block.Root(), rootOff, "block %d", block.NumberU64())
		require.Equal(t, rootOff, rootOn, "block %d: recording changed the root", block.NumberU64())
		require.Equal(t, gasOff, gasOn, "block %d: recording changed the gas", block.NumberU64())
		encOff, err := rlp.EncodeToBytes(receiptsOff)
		require.NoError(t, err)
		encOn, err := rlp.EncodeToBytes(receiptsOn)
		require.NoError(t, err)
		require.Equal(t, encOff, encOn, "block %d: recording changed the receipts", block.NumberU64())

		list := access.Recorded(statedb)
		enc, err := list.Encode()
		require.NoError(t, err)
		decoded, err := access.Decode(enc)
		require.NoError(t, err)
		reenc, err := decoded.Encode()
		require.NoError(t, err)
		require.Equal(t, enc, reenc)

		applied, err := chain.StateAt(parent.Root)
		require.NoError(t, err)
		require.NoError(t, access.Apply(applied, decoded))
		require.Equal(t, block.Root(), applied.IntermediateRoot(true), "block %d: applied root mismatch", block.NumberU64())
		lists[i] = decoded
	}
	return lists
}

func findAccess(t *testing.T, list *access.BlockAccessList, addr common.Address) *access.AccountAccess {
	for i := range list.Accounts {
		if list.Accounts[i].Address == addr {
			return &list.Accounts[i]
		}
	}
	t.Fatalf("account %s is not in the access list", addr.Hex())
	return nil
}

// balHistoryChain is a chain whose lists exercise every kind of change.
type balHistoryChain struct {
	env                      *balTestEnv
	chain                    *BlockChain
	blocks                   []*types.Block
	lists                    []*access.BlockAccessList
	contract, child, untouch common.Address
}

func newBALHistoryChain(t *testing.T, hardfork string) *balHistoryChain {
	env := newBALTestEnv(t, hardfork, 6)
	addrs := env.addrs
	var (
		newEOA   = common.HexToAddress("0x00000000000000000000000000000000000e0a02")
		untouch  = common.HexToAddress("0x00000000000000000000000000000000000e0a03")
		contract = crypto.CreateAddress(addrs[0], 0)
		initCode = balInitCode(balDestructibleCode())
		child    = crypto.CreateAddress2(balFactory, [32]byte{}, crypto.Keccak256(initCode))
		roleKeys = []*ecdsa.PrivateKey{mustKey(t), mustKey(t), mustKey(t)}
	)
	roleBased := accountkey.NewAccountKeyRoleBasedWithValues(accountkey.AccountKeyRoleBased{
		accountkey.NewAccountKeyPublicWithValue(&roleKeys[0].PublicKey),
		accountkey.NewAccountKeyPublicWithValue(&roleKeys[1].PublicKey),
		accountkey.NewAccountKeyPublicWithValue(&roleKeys[2].PublicKey),
	})

	chain, blocks := env.generate(9, func(i int, b *BlockGen) {
		b.SetRewardbase(common.Address{0xbe, byte(i % 3)})
		switch i {
		case 0:
			b.AddTx(env.legacyTx(b, 0, nil, common.Big0, balInitCode(balStoreCode())))
			b.AddTx(env.legacyTx(b, 1, &newEOA, big.NewInt(params.KAIA), nil))
		case 1:
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, balSlotData(1, 0)))
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, balSlotData(5, 0x55)))
			b.AddTx(env.legacyTx(b, 2, &balFactory, common.Big0, initCode))
		case 2:
			b.AddTx(env.kaiaTx(types.TxTypeAccountUpdate, map[types.TxValueKeyType]interface{}{
				types.TxValueKeyNonce:      b.TxNonce(addrs[3]),
				types.TxValueKeyFrom:       addrs[3],
				types.TxValueKeyAccountKey: roleBased,
			}, env.keys[3:4], nil))
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, balSlotData(3, 0x33)))
		case 3:
			// Destroy, recreate with fresh storage, write again.
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, nil))
			b.AddTx(env.legacyTx(b, 2, &balFactory, common.Big0, initCode))
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, balSlotData(4, 0x44)))
		case 4:
			if hardfork == "prague" {
				b.AddTx(env.setCodeTx(b, 4, balStoreAddr, nil))
			}
		case 5:
			// Destroy and leave it deleted.
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, nil))
			if hardfork == "prague" {
				b.AddTx(env.legacyTx(b, 4, &addrs[4], common.Big0, balSlotData(7, 0x77)))
			}
		case 7:
			// Recreate in a later block.
			b.AddTx(env.legacyTx(b, 2, &balFactory, common.Big0, initCode))
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, balSlotData(5, 0)))
		}
		b.AddTx(env.legacyTx(b, 5, &addrs[1], big.NewInt(int64(i+1)), nil))
	})
	return &balHistoryChain{
		env: env, chain: chain, blocks: blocks, lists: checkBAL(t, chain, blocks),
		contract: contract, child: child, untouch: untouch,
	}
}

// accountFields encodes an account without its storage root, which History
// does not track.
func accountFields(t *testing.T, acc account.Account) []byte {
	if acc == nil {
		return nil
	}
	acc = acc.DeepCopy()
	if pa := account.GetProgramAccount(acc); pa != nil {
		pa.SetStorageRoot(common.ExtHash{})
	}
	enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(acc))
	require.NoError(t, err)
	return enc
}
