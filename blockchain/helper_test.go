// Modifications Copyright 2024 The Kaia Authors
// Modifications Copyright 2018 The klaytn Authors
// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
//
// This file is derived from core/helper_test.go (2018/06/04).
// Modified and improved for the klaytn development.
// Modified and improved for the Kaia development.

package blockchain

import (
	"container/list"

	"crypto/ecdsa"
	"math/big"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/event"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

// Implement our EthTest Manager
type TestManager struct {
	// stateManager *StateManager
	eventMux *event.TypeMux

	db         database.Database
	txPool     *TxPool
	blockChain *BlockChain
	Blocks     []*types.Block
}

func (tm *TestManager) IsListening() bool {
	return false
}

func (tm *TestManager) IsMining() bool {
	return false
}

func (tm *TestManager) PeerCount() int {
	return 0
}

func (tm *TestManager) Peers() *list.List {
	return list.New()
}

func (tm *TestManager) BlockChain() *BlockChain {
	return tm.blockChain
}

func (tm *TestManager) TxPool() *TxPool {
	return tm.txPool
}

// func (tm *TestManager) StateManager() *StateManager {
// 	return tm.stateManager
// }

func (tm *TestManager) EventMux() *event.TypeMux {
	return tm.eventMux
}

// func (tm *TestManager) KeyManager() *crypto.KeyManager {
// 	return nil
// }

func (tm *TestManager) Db() database.Database {
	return tm.db
}

func NewTestManager() *TestManager {
	testManager := &TestManager{}
	testManager.eventMux = new(event.TypeMux)
	testManager.db = database.NewMemDB()
	// testManager.txPool = NewTxPool(testManager)
	// testManager.blockChain = NewBlockChain(testManager)
	// testManager.stateManager = NewStateManager(testManager)
	return testManager
}

var (
	SystemAddr  = common.HexToAddress("0x000000000000000000000000000000000000ba50")
	FactoryAddr = common.HexToAddress("0x000000000000000000000000000000000000fac7")
	Reward      = big.NewInt(params.KAIA)
	GasPrice    = big.NewInt(25 * params.Gkei)
	Funds       = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.KAIA))
)

// Sealer is a faker that also changes state in InitializeState and
// FinalizeState, standing in for system tx calls and block rewards.
type Sealer struct {
	*faker.Faker
	initAddr common.Address // gets 1 peb in InitializeState
}

func (s Sealer) InitializeState(header *types.Header, statedb *state.StateDB) error {
	// Not finalised on purpose: the recorder must keep these changes out of tx 1.
	statedb.SetState(SystemAddr, common.BigToHash(header.Number), header.ParentHash)
	statedb.AddBalance(s.initAddr, common.Big1)
	return nil
}

func (s Sealer) FinalizeState(header *types.Header, statedb *state.StateDB, txs []*types.Transaction, receipts []*types.Receipt) error {
	statedb.AddBalance(header.Rewardbase, Reward)
	return nil
}

type Env struct {
	T       *testing.T
	Config  *params.ChainConfig
	Signer  types.Signer
	Sealer  Sealer
	DB      database.DBManager
	Gspec   *Genesis
	Genesis *types.Block
	Keys    []*ecdsa.PrivateKey
	Addrs   []common.Address
}

func NewEnv(t *testing.T, hardfork string, numKeys int) *Env {
	env := &Env{T: t, Config: params.TestKaiaConfig(hardfork), DB: database.NewMemoryDBManager()}
	env.Signer = types.LatestSignerForChainID(env.Config.ChainID)
	alloc := GenesisAlloc{
		SystemAddr:  {Code: []byte{byte(vm.STOP)}, Balance: common.Big0},
		FactoryAddr: {Code: FactoryCode(), Balance: common.Big0},
		// The store contract is the delegation target of EIP-7702 tests.
		StoreAddr: {Code: StoreCode(), Balance: common.Big0},
	}
	for range numKeys {
		key, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(key.PublicKey)
		env.Keys = append(env.Keys, key)
		env.Addrs = append(env.Addrs, addr)
		alloc[addr] = GenesisAccount{Balance: Funds}
	}
	env.Sealer = Sealer{Faker: faker.NewFaker(), initAddr: env.Addrs[0]}
	env.Gspec = &Genesis{Config: env.Config, Alloc: alloc}
	env.Genesis = env.Gspec.MustCommit(env.DB)
	return env
}

var StoreAddr = common.HexToAddress("0x000000000000000000000000000000000000570e")

// StoreCode stores calldata[32:64] at slot calldata[0:32].
func StoreCode() []byte {
	return []byte{
		byte(vm.PUSH1), 0x20, byte(vm.CALLDATALOAD),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD),
		byte(vm.SSTORE),
		byte(vm.STOP),
	}
}

// DestructibleCode works like StoreCode, and self-destructs on empty calldata.
func DestructibleCode() []byte {
	return []byte{
		byte(vm.CALLDATASIZE), byte(vm.ISZERO), byte(vm.PUSH1), 13, byte(vm.JUMPI),
		byte(vm.PUSH1), 0x20, byte(vm.CALLDATALOAD),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD),
		byte(vm.SSTORE),
		byte(vm.STOP),
		byte(vm.JUMPDEST), byte(vm.CALLER), byte(vm.SELFDESTRUCT),
	}
}

// FactoryCode deploys calldata as init code with CREATE2 and salt 0.
func FactoryCode() []byte {
	return []byte{
		byte(vm.CALLDATASIZE), byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CALLDATACOPY),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATASIZE), byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CREATE2),
		byte(vm.POP), byte(vm.STOP),
	}
}

// InitCode stores 0x11 at slot 1 and 0x22 at slot 2, then returns runtime.
func InitCode(runtime []byte) []byte {
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

func SlotData(slot, value byte) []byte {
	data := make([]byte, 64)
	data[31], data[63] = slot, value
	return data
}

func (env *Env) LegacyTx(b *BlockGen, from int, to *common.Address, amount *big.Int, data []byte) *types.Transaction {
	nonce := b.TxNonce(env.Addrs[from])
	var tx *types.Transaction
	if to == nil {
		tx = types.NewContractCreation(nonce, amount, 1_000_000, GasPrice, data)
	} else {
		tx = types.NewTransaction(nonce, *to, amount, 1_000_000, GasPrice, data)
	}
	tx, err := types.SignTx(tx, env.Signer, env.Keys[from])
	require.NoError(env.T, err)
	return tx
}

func (env *Env) KaiaTx(txType types.TxType, values map[types.TxValueKeyType]interface{}, keys, feePayerKeys []*ecdsa.PrivateKey) *types.Transaction {
	values[types.TxValueKeyGasLimit] = uint64(1_000_000)
	values[types.TxValueKeyGasPrice] = GasPrice
	tx, err := types.NewTransactionWithMap(txType, values)
	require.NoError(env.T, err)
	require.NoError(env.T, tx.SignWithKeys(env.Signer, keys))
	if feePayerKeys != nil {
		require.NoError(env.T, tx.SignFeePayerWithKeys(env.Signer, feePayerKeys))
	}
	return tx
}

func (env *Env) SetCodeTx(b *BlockGen, from int, target common.Address, data []byte) *types.Transaction {
	addr := env.Addrs[from]
	nonce := b.TxNonce(addr)
	auth, err := types.SignSetCode(env.Keys[from], types.SetCodeAuthorization{
		ChainID: *uint256.MustFromBig(env.Config.ChainID),
		Address: target,
		Nonce:   nonce + 1, // the sender is the authority, so its nonce is already bumped
	})
	require.NoError(env.T, err)
	authList := []types.SetCodeAuthorization{auth}
	intrinsicGas, err := types.IntrinsicGas(data, nil, authList, false, env.Config.Rules(b.Number()))
	require.NoError(env.T, err)
	tx, err := types.SignTx(types.NewMessage(addr, &addr, nonce, nil, 1_000_000, nil, GasPrice,
		GasPrice, nil, data, false, intrinsicGas, nil, nil, nil, nil, authList), env.Signer, env.Keys[from])
	require.NoError(env.T, err)
	return tx
}

// Generate makes the blocks and inserts them into a new chain.
func (env *Env) Generate(n int, gen func(int, *BlockGen)) (*BlockChain, []*types.Block) {
	return env.GenerateWithCache(nil, n, gen)
}

func (env *Env) GenerateWithCache(cacheConfig *CacheConfig, n int, gen func(int, *BlockGen)) (*BlockChain, []*types.Block) {
	blocks, _ := GenerateChain(env.Config, env.Genesis, env.Sealer, env.DB, n, gen)
	chain, err := newBALChain(env.DB, cacheConfig, env.Config, env.Sealer, vm.Config{})
	require.NoError(env.T, err)
	env.T.Cleanup(func() { chain.Stop() })
	chain.Processor().RegisterBlockStateModule(env.Sealer)
	_, err = chain.InsertChain(blocks)
	require.NoError(env.T, err)
	return chain, blocks
}

// CheckBAL re-executes every block on a state opened ForTrie, which records
// nothing, and on one opened ForBuild, which records its list, then checks
// that the recorded list survives encoding and reproduces the state root when
// applied to the parent state.
func CheckBAL(t *testing.T, chain *BlockChain, blocks []*types.Block) []*types.BlockAccessList {
	lists := make([]*types.BlockAccessList, len(blocks))
	for i, block := range blocks {
		parent := chain.GetHeaderByHash(block.ParentHash())
		require.NotNil(t, parent)

		process := func(record bool) (types.Receipts, uint64, common.Hash, *state.StateDB) {
			use := state.ForTrie
			if record {
				use = state.ForBuild
			}
			statedb, err := chain.States().Open(parent, use)
			require.NoError(t, err)
			receipts, _, usedGas, _, _, err := chain.Processor().Process(block, statedb, vm.Config{})
			require.NoError(t, err)
			return receipts, usedGas, statedb.IntermediateRoot(true), statedb
		}
		receiptsOff, gasOff, rootOff := func() (types.Receipts, uint64, common.Hash) {
			r, g, root, statedb := process(false)
			require.Nil(t, statedb.BAL())
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

		list := statedb.BAL()
		enc, err := list.Encode()
		require.NoError(t, err)
		decoded, err := types.DecodeBAL(enc)
		require.NoError(t, err)
		reenc, err := decoded.Encode()
		require.NoError(t, err)
		require.Equal(t, enc, reenc)

		applied, err := chain.States().Open(parent, state.ForTrie)
		require.NoError(t, err)
		require.NoError(t, applied.ApplyBAL(decoded))
		require.Equal(t, block.Root(), applied.IntermediateRoot(true), "block %d: applied root mismatch", block.NumberU64())
		lists[i] = decoded
	}
	return lists
}

func FindAccess(t *testing.T, list *types.BlockAccessList, addr common.Address) *types.AccountChanges {
	for i := range list.Accounts {
		if list.Accounts[i].Address == addr {
			return &list.Accounts[i]
		}
	}
	t.Fatalf("account %s is not in the access list", addr.Hex())
	return nil
}

func BalanceIndices(a *types.AccountChanges) []uint32 {
	var idx []uint32
	for _, c := range a.BalanceChanges {
		idx = append(idx, c.BlockAccessIndex)
	}
	return idx
}

// RequireSameList checks that two lists have the same encoding.
func RequireSameList(t *testing.T, want, got *types.BlockAccessList) {
	t.Helper()
	wantEnc, err := want.Encode()
	require.NoError(t, err)
	gotEnc, err := got.Encode()
	require.NoError(t, err)
	require.Equal(t, wantEnc, gotEnc)
}

func MustKey(t *testing.T) *ecdsa.PrivateKey {
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	return key
}

type Fixture struct {
	Env    *Env
	Blocks []*types.Block
	Lists  []*types.BlockAccessList // as executed in order, item i is block i+1
}

// NewFixture makes a chain of n blocks, flushing every state, without
// recording their lists, and the lists of an in-order execution to compare.
func NewFixture(t *testing.T, hardfork string, n int) *Fixture {
	env := NewEnv(t, hardfork, 3)
	chain, blocks := env.GenerateWithCache(HistoryCacheConfig(), n, func(i int, b *BlockGen) {
		b.SetRewardbase(common.Address{0xbe, byte(i % 4)})
		b.AddTx(env.LegacyTx(b, 0, &StoreAddr, common.Big0, SlotData(byte(i%5), byte(i+1))))
		b.AddTx(env.LegacyTx(b, 1, &env.Addrs[2], common.Big1, nil))
		if i%3 == 0 {
			b.AddTx(env.LegacyTx(b, 2, nil, common.Big0, InitCode(StoreCode())))
		}
	})
	lists := CheckBAL(t, chain, blocks)
	chain.Stop()
	return &Fixture{Env: env, Blocks: blocks, Lists: lists}
}

// HistoryCacheConfig keeps the BAL history and flushes the state of every block
// to disk.
func HistoryCacheConfig() *CacheConfig {
	return &CacheConfig{
		CacheSize:           512,
		BlockInterval:       1,
		TriesInMemory:       DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
	}
}

// RequireLists checks the stored writes of the chain against the lists of
// blocks from to to.
func (f *Fixture) RequireLists(t *testing.T, chain *BlockChain, from, to uint64) {
	for n := from; n <= to; n++ {
		got, err := f.Env.DB.ReadCanonicalBAL(n)
		require.NoError(t, err, "block %d", n)
		want, err := f.Lists[n-1].Writes().Encode()
		require.NoError(t, err)
		require.Equal(t, want, got, "block %d", n)
	}
}

// WaitBALIndex waits until the BAL index of chain covers blocks before next.
func WaitBALIndex(t *testing.T, chain *BlockChain, next uint64) {
	t.Helper()
	for i := 0; i < 500; i++ {
		chain.States().BALs().SignalIndex()
		if _, n, ok := chain.States().BALs().Indexed(); ok && n == next {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, n, _ := chain.States().BALs().Indexed()
	t.Fatalf("BAL index reached block %d, want %d", n, next)
}

// newBALChain makes a chain in history mode, keeping its BAL history.
func newBALChain(db database.DBManager, cacheConfig *CacheConfig, chainConfig *params.ChainConfig, engine consensus.Sealer, vmConfig vm.Config) (*BlockChain, error) {
	if cacheConfig == nil {
		cacheConfig = &CacheConfig{
			CacheSize:            512,
			BlockInterval:        DefaultBlockInterval,
			TriesInMemory:        DefaultTriesInMemory,
			LivePruningRetention: DefaultLivePruningRetention,
			TrieNodeCacheConfig:  statedb.GetEmptyTrieNodeCacheConfig(),
			SnapshotCacheSize:    512,
			SnapshotAsyncGen:     true,
		}
	}
	if cacheConfig.HistoryMode != database.HistoryModeBAL {
		c := *cacheConfig
		c.HistoryMode = database.HistoryModeBAL
		cacheConfig = &c
	}
	bc, err := NewBlockChain(db, cacheConfig, chainConfig, engine, vmConfig)
	if err != nil {
		return nil, err
	}
	return bc, nil
}

// CompleteTrie reports whether every node of the state trie with the given
// root, of its storage tries and its code is stored in db.
func CompleteTrie(db database.DBManager, root common.Hash) error {
	sdb, err := state.New(root, state.NewDatabase(db), nil, nil)
	if err != nil {
		return err
	}
	it := state.NewNodeIterator(sdb)
	for it.Next() {
	}
	return it.Error
}

func WaitPruned(t *testing.T, db database.DBManager, limit uint64) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if n, err := db.ReadLastPrunedBlockNumber(); err == nil && n == limit {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	n, _ := db.ReadLastPrunedBlockNumber()
	t.Fatalf("pruned up to block %d, want %d", n, limit)
}
