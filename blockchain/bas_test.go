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
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/holiman/uint256"
	"github.com/kaiachain/kaia/blockchain/bas"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/accountkey"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

var (
	basSystemAddr = common.HexToAddress("0x000000000000000000000000000000000000ba50")
	basFactory    = common.HexToAddress("0x000000000000000000000000000000000000fac7")
	basReward     = big.NewInt(params.KAIA)
	basGasPrice   = big.NewInt(25 * params.Gkei)
	basFunds      = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.KAIA))
)

// basTestSealer is a faker that also changes state in InitializeState and
// FinalizeState, standing in for system tx calls and block rewards.
type basTestSealer struct {
	*faker.Faker
	initAddr common.Address // gets 1 peb in InitializeState
}

func (s basTestSealer) InitializeState(header *types.Header, statedb *state.StateDB) error {
	// Not finalised on purpose: the recorder must keep these changes out of tx 1.
	statedb.SetState(basSystemAddr, common.BigToHash(header.Number), header.ParentHash)
	statedb.AddBalance(s.initAddr, common.Big1)
	return nil
}

func (s basTestSealer) FinalizeState(header *types.Header, statedb *state.StateDB, txs []*types.Transaction, receipts []*types.Receipt) error {
	statedb.AddBalance(header.Rewardbase, basReward)
	return nil
}

type basTestEnv struct {
	t       *testing.T
	config  *params.ChainConfig
	signer  types.Signer
	sealer  basTestSealer
	db      database.DBManager
	genesis *types.Block
	keys    []*ecdsa.PrivateKey
	addrs   []common.Address
}

func newBASTestEnv(t *testing.T, hardfork string, numKeys int) *basTestEnv {
	env := &basTestEnv{t: t, config: params.TestKaiaConfig(hardfork), db: database.NewMemoryDBManager()}
	env.signer = types.LatestSignerForChainID(env.config.ChainID)
	alloc := GenesisAlloc{
		basSystemAddr: {Code: []byte{byte(vm.STOP)}, Balance: common.Big0},
		basFactory:    {Code: basFactoryCode(), Balance: common.Big0},
		// The store contract is the delegation target of EIP-7702 tests.
		basStoreAddr: {Code: basStoreCode(), Balance: common.Big0},
	}
	for range numKeys {
		key, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(key.PublicKey)
		env.keys = append(env.keys, key)
		env.addrs = append(env.addrs, addr)
		alloc[addr] = GenesisAccount{Balance: basFunds}
	}
	env.sealer = basTestSealer{Faker: faker.NewFaker(), initAddr: env.addrs[0]}
	env.genesis = (&Genesis{Config: env.config, Alloc: alloc}).MustCommit(env.db)
	return env
}

var basStoreAddr = common.HexToAddress("0x000000000000000000000000000000000000570e")

// basStoreCode stores calldata[32:64] at slot calldata[0:32].
func basStoreCode() []byte {
	return []byte{
		byte(vm.PUSH1), 0x20, byte(vm.CALLDATALOAD),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD),
		byte(vm.SSTORE),
		byte(vm.STOP),
	}
}

// basDestructibleCode works like basStoreCode, and self-destructs on empty calldata.
func basDestructibleCode() []byte {
	return []byte{
		byte(vm.CALLDATASIZE), byte(vm.ISZERO), byte(vm.PUSH1), 13, byte(vm.JUMPI),
		byte(vm.PUSH1), 0x20, byte(vm.CALLDATALOAD),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD),
		byte(vm.SSTORE),
		byte(vm.STOP),
		byte(vm.JUMPDEST), byte(vm.CALLER), byte(vm.SELFDESTRUCT),
	}
}

// basFactoryCode deploys calldata as init code with CREATE2 and salt 0.
func basFactoryCode() []byte {
	return []byte{
		byte(vm.CALLDATASIZE), byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CALLDATACOPY),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATASIZE), byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CREATE2),
		byte(vm.POP), byte(vm.STOP),
	}
}

// basInitCode stores 0x11 at slot 1 and 0x22 at slot 2, then returns runtime.
func basInitCode(runtime []byte) []byte {
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

func basSlotData(slot, value byte) []byte {
	data := make([]byte, 64)
	data[31], data[63] = slot, value
	return data
}

func (env *basTestEnv) legacyTx(b *BlockGen, from int, to *common.Address, amount *big.Int, data []byte) *types.Transaction {
	nonce := b.TxNonce(env.addrs[from])
	var tx *types.Transaction
	if to == nil {
		tx = types.NewContractCreation(nonce, amount, 1_000_000, basGasPrice, data)
	} else {
		tx = types.NewTransaction(nonce, *to, amount, 1_000_000, basGasPrice, data)
	}
	tx, err := types.SignTx(tx, env.signer, env.keys[from])
	require.NoError(env.t, err)
	return tx
}

func (env *basTestEnv) kaiaTx(txType types.TxType, values map[types.TxValueKeyType]interface{}, keys, feePayerKeys []*ecdsa.PrivateKey) *types.Transaction {
	values[types.TxValueKeyGasLimit] = uint64(1_000_000)
	values[types.TxValueKeyGasPrice] = basGasPrice
	tx, err := types.NewTransactionWithMap(txType, values)
	require.NoError(env.t, err)
	require.NoError(env.t, tx.SignWithKeys(env.signer, keys))
	if feePayerKeys != nil {
		require.NoError(env.t, tx.SignFeePayerWithKeys(env.signer, feePayerKeys))
	}
	return tx
}

func (env *basTestEnv) setCodeTx(b *BlockGen, from int, target common.Address, data []byte) *types.Transaction {
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
	tx, err := types.SignTx(types.NewMessage(addr, &addr, nonce, nil, 1_000_000, nil, basGasPrice,
		basGasPrice, nil, data, false, intrinsicGas, nil, nil, nil, nil, authList), env.signer, env.keys[from])
	require.NoError(env.t, err)
	return tx
}

// generate makes the blocks and inserts them into a new chain.
func (env *basTestEnv) generate(n int, gen func(int, *BlockGen)) (*BlockChain, []*types.Block) {
	return env.generateWithCache(nil, n, gen)
}

func (env *basTestEnv) generateWithCache(cacheConfig *CacheConfig, n int, gen func(int, *BlockGen)) (*BlockChain, []*types.Block) {
	blocks, _ := GenerateChain(env.config, env.genesis, env.sealer, env.db, n, gen)
	chain, err := NewBlockChain(env.db, cacheConfig, env.config, env.sealer, vm.Config{})
	require.NoError(env.t, err)
	env.t.Cleanup(chain.Stop)
	chain.Processor().RegisterBlockStateModule(env.sealer)
	_, err = chain.InsertChain(blocks)
	require.NoError(env.t, err)
	return chain, blocks
}

// checkBAS re-executes every block with and without the recorder, then checks
// that the recorded set survives encoding and reproduces the state root when
// applied to the parent state.
func checkBAS(t *testing.T, chain *BlockChain, blocks []*types.Block) []*bas.BlockAccessSet {
	sets := make([]*bas.BlockAccessSet, len(blocks))
	for i, block := range blocks {
		parent := chain.GetHeaderByHash(block.ParentHash())
		require.NotNil(t, parent)

		process := func(record bool) (types.Receipts, uint64, common.Hash, *state.StateDB) {
			statedb, err := chain.StateAt(parent.Root)
			require.NoError(t, err)
			if record {
				statedb.EnableBlockAccessRecorder()
			}
			receipts, _, usedGas, _, _, err := chain.Processor().Process(block, statedb, vm.Config{})
			require.NoError(t, err)
			return receipts, usedGas, statedb.IntermediateRoot(true), statedb
		}
		receiptsOff, gasOff, rootOff := func() (types.Receipts, uint64, common.Hash) {
			r, g, root, statedb := process(false)
			require.Nil(t, statedb.BlockAccessSet())
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

		set := statedb.BlockAccessSet()
		enc, err := set.Encode()
		require.NoError(t, err)
		decoded, err := bas.Decode(enc)
		require.NoError(t, err)
		reenc, err := decoded.Encode()
		require.NoError(t, err)
		require.Equal(t, enc, reenc)

		applied, err := chain.StateAt(parent.Root)
		require.NoError(t, err)
		require.NoError(t, applied.ApplyBlockAccessSet(decoded))
		require.Equal(t, block.Root(), applied.IntermediateRoot(true), "block %d: applied root mismatch", block.NumberU64())
		sets[i] = decoded
	}
	return sets
}

func findAccess(t *testing.T, set *bas.BlockAccessSet, addr common.Address) *bas.AccountAccess {
	for i := range set.Accounts {
		if set.Accounts[i].Address == addr {
			return &set.Accounts[i]
		}
	}
	t.Fatalf("account %s is not in the access set", addr.Hex())
	return nil
}

func balanceIndices(a *bas.AccountAccess) []uint32 {
	var idx []uint32
	for _, c := range a.BalanceChanges {
		idx = append(idx, c.Index)
	}
	return idx
}

func TestBlockAccessSet(t *testing.T) {
	env := newBASTestEnv(t, "prague", 8)
	var (
		addrs     = env.addrs
		newEOA    = common.HexToAddress("0x00000000000000000000000000000000000e0a01")
		contract  = crypto.CreateAddress(addrs[0], 1) // second tx of addrs[0]
		ephemeral = crypto.CreateAddress2(basFactory, [32]byte{}, crypto.Keccak256([]byte{byte(vm.CALLER), byte(vm.SELFDESTRUCT)}))

		roleKeys     = []*ecdsa.PrivateKey{mustKey(t), mustKey(t), mustKey(t)}
		multisigKeys = []*ecdsa.PrivateKey{mustKey(t), mustKey(t), mustKey(t)}
	)
	roleBased := accountkey.NewAccountKeyRoleBasedWithValues(accountkey.AccountKeyRoleBased{
		accountkey.NewAccountKeyPublicWithValue(&roleKeys[0].PublicKey),
		accountkey.NewAccountKeyPublicWithValue(&roleKeys[1].PublicKey),
		accountkey.NewAccountKeyPublicWithValue(&roleKeys[2].PublicKey),
	})
	multisig := accountkey.NewAccountKeyWeightedMultiSigWithValues(2, accountkey.WeightedPublicKeys{
		accountkey.NewWeightedPublicKey(1, (*accountkey.PublicKeySerializable)(&multisigKeys[0].PublicKey)),
		accountkey.NewWeightedPublicKey(1, (*accountkey.PublicKeySerializable)(&multisigKeys[1].PublicKey)),
		accountkey.NewWeightedPublicKey(1, (*accountkey.PublicKeySerializable)(&multisigKeys[2].PublicKey)),
	})

	chain, blocks := env.generate(4, func(i int, b *BlockGen) {
		b.SetRewardbase(common.Address{0xbe, byte(i)})
		switch i {
		case 0:
			// Value transfer creating a new EOA, contract creation, fee delegation.
			b.AddTx(env.legacyTx(b, 0, &newEOA, big.NewInt(params.KAIA), nil))
			b.AddTx(env.legacyTx(b, 0, nil, common.Big0, basInitCode(basStoreCode())))
			b.AddTx(env.kaiaTx(types.TxTypeFeeDelegatedValueTransfer, map[types.TxValueKeyType]interface{}{
				types.TxValueKeyNonce:    b.TxNonce(addrs[1]),
				types.TxValueKeyFrom:     addrs[1],
				types.TxValueKeyTo:       addrs[2],
				types.TxValueKeyAmount:   big.NewInt(7),
				types.TxValueKeyFeePayer: addrs[3],
			}, env.keys[1:2], env.keys[3:4]))
		case 1:
			// Storage update, delete and no-op write.
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, basSlotData(1, 0)))
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, basSlotData(2, 0x22)))
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, basSlotData(3, 0x33)))
			// Key updates.
			b.AddTx(env.kaiaTx(types.TxTypeAccountUpdate, map[types.TxValueKeyType]interface{}{
				types.TxValueKeyNonce:      b.TxNonce(addrs[4]),
				types.TxValueKeyFrom:       addrs[4],
				types.TxValueKeyAccountKey: roleBased,
			}, env.keys[4:5], nil))
			b.AddTx(env.kaiaTx(types.TxTypeFeeDelegatedAccountUpdate, map[types.TxValueKeyType]interface{}{
				types.TxValueKeyNonce:      b.TxNonce(addrs[5]),
				types.TxValueKeyFrom:       addrs[5],
				types.TxValueKeyAccountKey: multisig,
				types.TxValueKeyFeePayer:   addrs[3],
			}, env.keys[5:6], env.keys[3:4]))
			// EIP-7702 delegation that writes the storage of the EOA.
			b.AddTx(env.setCodeTx(b, 6, basStoreAddr, basSlotData(5, 0x55)))
			// EIP-6780: created and self-destructed in the same tx.
			b.AddTx(env.legacyTx(b, 7, &basFactory, common.Big0, []byte{byte(vm.CALLER), byte(vm.SELFDESTRUCT)}))
		case 2:
			// Reward-only empty block.
		case 3:
			// Txs signed with the updated keys, and removal of the delegation.
			b.AddTx(env.kaiaTx(types.TxTypeValueTransfer, map[types.TxValueKeyType]interface{}{
				types.TxValueKeyNonce:  b.TxNonce(addrs[4]),
				types.TxValueKeyFrom:   addrs[4],
				types.TxValueKeyTo:     newEOA,
				types.TxValueKeyAmount: big.NewInt(11),
			}, roleKeys[:1], nil))
			b.AddTx(env.kaiaTx(types.TxTypeValueTransfer, map[types.TxValueKeyType]interface{}{
				types.TxValueKeyNonce:  b.TxNonce(addrs[5]),
				types.TxValueKeyFrom:   addrs[5],
				types.TxValueKeyTo:     newEOA,
				types.TxValueKeyAmount: big.NewInt(13),
			}, multisigKeys[:2], nil))
			b.AddTx(env.setCodeTx(b, 6, common.Address{}, nil))
		}
	})
	sets := checkBAS(t, chain, blocks)

	// Changes of InitializeState have index 0 and do not leak into tx 1.
	a := findAccess(t, sets[0], addrs[0])
	require.Equal(t, []uint32{0, 1, 2}, balanceIndices(a))
	require.Equal(t, uint32(0), findAccess(t, sets[0], basSystemAddr).StorageWrites[0].Changes[0].Index)

	// A new EOA and a new contract carry the whole account.
	require.Len(t, findAccess(t, sets[0], newEOA).AccountChanges, 1)
	c := findAccess(t, sets[0], contract)
	require.Len(t, c.AccountChanges, 1)
	require.Len(t, c.CodeChanges, 1)
	require.Len(t, c.StorageWrites, 2)

	// Slot 1 cleared, slot 2 unchanged (read only), slot 3 set.
	c = findAccess(t, sets[1], contract)
	require.Empty(t, c.AccountChanges)
	require.Len(t, c.StorageWrites, 2)
	require.Equal(t, common.Hash{}, c.StorageWrites[0].Changes[0].Value)
	require.Equal(t, []common.Hash{common.BigToHash(common.Big2)}, c.StorageReads)

	// Key updates store the whole account, fee payer balances are recorded.
	require.Equal(t, uint32(4), findAccess(t, sets[1], addrs[4]).AccountChanges[0].Index)
	require.Equal(t, uint32(5), findAccess(t, sets[1], addrs[5]).AccountChanges[0].Index)
	require.Equal(t, []uint32{3}, balanceIndices(findAccess(t, sets[0], addrs[3])))
	require.Equal(t, []uint32{5}, balanceIndices(findAccess(t, sets[1], addrs[3])))

	// Delegation changes code and code info, and writes EOA storage.
	d := findAccess(t, sets[1], addrs[6])
	require.Len(t, d.CodeChanges, 1)
	require.Len(t, d.AccountChanges, 1)
	require.Len(t, d.StorageWrites, 1)
	d = findAccess(t, sets[3], addrs[6])
	require.Empty(t, d.CodeChanges[0].Code)
	require.Len(t, d.AccountChanges, 1)

	// An account created and destroyed in the same tx is only accessed.
	require.False(t, findAccess(t, sets[1], ephemeral).Changed())

	// Reward-only block: the reward is in FinalizeState, index len(txs)+1 = 1.
	r := findAccess(t, sets[2], common.Address{0xbe, 2})
	require.Equal(t, []uint32{1}, balanceIndices(r))
}

// TestBlockAccessSetSelfDestruct uses pre-Cancun SELFDESTRUCT to delete and
// recreate a contract at the same address with CREATE2.
func TestBlockAccessSetSelfDestruct(t *testing.T) {
	env := newBASTestEnv(t, "shanghai", 2)
	initCode := basInitCode(basDestructibleCode())
	child := crypto.CreateAddress2(basFactory, [32]byte{}, crypto.Keccak256(initCode))

	chain, blocks := env.generate(4, func(i int, b *BlockGen) {
		b.SetRewardbase(common.Address{0xbe, byte(i)})
		switch i {
		case 0:
			b.AddTx(env.legacyTx(b, 1, &basFactory, common.Big0, initCode))
			b.AddTx(env.legacyTx(b, 1, &child, common.Big0, basSlotData(3, 0x33)))
		case 1:
			// Destroy, recreate with fresh storage, write again.
			b.AddTx(env.legacyTx(b, 1, &child, common.Big0, nil))
			b.AddTx(env.legacyTx(b, 1, &basFactory, common.Big0, initCode))
			b.AddTx(env.legacyTx(b, 1, &child, common.Big0, basSlotData(4, 0x44)))
		case 2:
			// Destroy and leave it deleted.
			b.AddTx(env.legacyTx(b, 1, &child, common.Big0, nil))
		}
	})
	sets := checkBAS(t, chain, blocks)

	c := findAccess(t, sets[1], child)
	require.Equal(t, []uint32{1}, c.Deleted)
	require.Equal(t, []uint32{2}, c.StorageCleared)
	require.Equal(t, uint32(2), c.AccountChanges[0].Index)

	c = findAccess(t, sets[2], child)
	require.Equal(t, []uint32{1}, c.Deleted)
	require.False(t, func() bool { _, ok := c.LastWriteIndex(); return ok }())

	state, err := chain.StateAt(blocks[2].Root())
	require.NoError(t, err)
	require.False(t, state.Exist(child))
}

// TestBlockAccessSetTampered checks that a modified set fails the root check.
func TestBlockAccessSetTampered(t *testing.T) {
	env := newBASTestEnv(t, "prague", 2)
	chain, blocks := env.generate(1, func(i int, b *BlockGen) {
		b.AddTx(env.legacyTx(b, 0, &env.addrs[1], big.NewInt(params.KAIA), nil))
	})
	set := checkBAS(t, chain, blocks)[0]

	a := findAccess(t, set, env.addrs[1])
	last := &a.BalanceChanges[len(a.BalanceChanges)-1]
	last.Balance = new(big.Int).Add(last.Balance, common.Big1)

	statedb, err := chain.StateAt(env.genesis.Root())
	require.NoError(t, err)
	require.NoError(t, statedb.ApplyBlockAccessSet(set))
	require.NotEqual(t, blocks[0].Root(), statedb.IntermediateRoot(true))
}

// TestBASRecorder records the sets while blocks are inserted and reads them back.
func TestBASRecorder(t *testing.T) {
	env := newBASTestEnv(t, "prague", 2)
	dir := t.TempDir()
	cacheConfig := &CacheConfig{
		CacheSize:           512,
		BlockInterval:       DefaultBlockInterval,
		TriesInMemory:       DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
		BASRecordDir:        dir,
	}
	chain, blocks := env.generateWithCache(cacheConfig, 5, func(i int, b *BlockGen) {
		if i%2 == 0 {
			b.AddTx(env.legacyTx(b, 1, &env.addrs[0], big.NewInt(params.KAIA), nil))
			b.AddTx(env.legacyTx(b, 1, nil, common.Big0, basInitCode(basStoreCode())))
		}
	})
	require.Zero(t, chain.bas.sinceLog.failures+chain.bas.stats.failures)
	chain.Stop()

	file, err := os.Open(filepath.Join(dir, "bas-000000.rlp"))
	require.NoError(t, err)
	defer file.Close()
	stream := rlp.NewStream(file, 0)
	for _, block := range blocks {
		var rec BASFileRecord
		require.NoError(t, stream.Decode(&rec))
		require.Equal(t, block.NumberU64(), rec.Number)
		require.Equal(t, block.Hash(), rec.Hash)
		require.NoError(t, verifyBAS(chain, block, chain.GetHeaderByHash(block.ParentHash()).Root, rec.Set))
	}
	var rec BASFileRecord
	require.ErrorIs(t, stream.Decode(&rec), io.EOF)
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	return key
}
