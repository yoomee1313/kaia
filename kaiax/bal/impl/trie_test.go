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

package impl

import (
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

type proofNodes [][]byte

func (p *proofNodes) WriteMerkleProof(_, value []byte) { *p = append(*p, common.CopyBytes(value)) }

func verifyProof(t *testing.T, root common.Hash, key []byte, nodes proofNodes) []byte {
	t.Helper()
	db := database.NewMemoryDBManager()
	for _, n := range nodes {
		db.WriteMerkleProof(crypto.Keccak256(n), n)
	}
	value, err, _ := statedb.VerifyProof(root, key, db)
	require.NoError(t, err)
	return value
}

// completeTrie reports whether every node of the state trie with the given
// root, of its storage tries and its code is stored in db.
func completeTrie(db database.DBManager, root common.Hash) error {
	sdb, err := state.New(root, state.NewDatabase(db), nil, nil)
	if err != nil {
		return err
	}
	it := state.NewNodeIterator(sdb)
	for it.Next() {
	}
	return it.Error
}

func waitTrieBuild(t *testing.T, chain *blockchain.BlockChain, number uint64) TrieBuildStatus {
	t.Helper()
	require.NoError(t, StartTrieBuild(chain, number))
	for i := 0; i < 1000; i++ {
		if s := GetTrieBuildStatus(chain); !s.Running {
			require.Empty(t, s.Error, "block %d", number)
			require.Equal(t, "done", s.Stage)
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the state trie of block %d is not built", number)
	return TrieBuildStatus{}
}

func waitPruned(t *testing.T, db database.DBManager, limit uint64) {
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

// TestBuildStateTrie builds the state tries of past blocks on a live-pruned
// history node, checks their roots, values and proofs against the chain made
// without pruning, and checks that they survive the head advancing and
// pruning the nodes they share with it, while a trie that is not kept is
// pruned.
func TestBuildStateTrie(t *testing.T) {
	genesisStore := common.HexToAddress("0x00000000000000000000000000000000000e0a10")
	genesisSlots := make(map[common.Hash]common.Hash)
	for i := 1; i <= 20; i++ {
		genesisSlots[common.BigToHash(big.NewInt(int64(i)))] = common.BigToHash(big.NewInt(int64(0x100 + i)))
	}
	env := newBALTestEnvWithAlloc(t, "shanghai", 6, blockchain.GenesisAlloc{
		genesisStore: {Code: balStoreCode(), Balance: common.Big0, Storage: genesisSlots},
	})
	addrs := env.addrs
	var (
		newEOA   = common.HexToAddress("0x00000000000000000000000000000000000e0a02")
		contract = crypto.CreateAddress(addrs[0], 0)
		initCode = balInitCode(balDestructibleCode())
		child    = crypto.CreateAddress2(balFactory, [32]byte{}, crypto.Keccak256(initCode))
		late     = crypto.CreateAddress(addrs[3], 1)
	)
	const first, total = 30, 60
	blocks, _ := blockchain.GenerateChain(env.config, env.genesis, env.sealer, env.db, total, func(i int, b *blockchain.BlockGen) {
		n := i + 1
		b.SetRewardbase(common.Address{0xbe, byte(i % 3)})
		switch n {
		case 1:
			b.AddTx(env.legacyTx(b, 0, nil, common.Big0, balInitCode(balStoreCode())))
			b.AddTx(env.legacyTx(b, 1, &newEOA, big.NewInt(params.KAIA), nil))
		case 2:
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, balSlotData(1, 0)))
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, balSlotData(5, 0x55)))
			b.AddTx(env.legacyTx(b, 2, &balFactory, common.Big0, initCode))
		case 3:
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, balSlotData(3, 0x33)))
		case 4:
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, nil))
			b.AddTx(env.legacyTx(b, 2, &balFactory, common.Big0, initCode))
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, balSlotData(4, 0x44)))
		case 5:
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, nil))
		case 7:
			b.AddTx(env.legacyTx(b, 2, &balFactory, common.Big0, initCode))
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, balSlotData(5, 0)))
		case 9:
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, balSlotData(9, 0x99)))
		case 33:
			b.AddTx(env.legacyTx(b, 3, &genesisStore, common.Big0, balSlotData(7, 0x77)))
			b.AddTx(env.legacyTx(b, 3, nil, common.Big0, balInitCode(balStoreCode())))
		case 35:
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, nil))
		case 41:
			b.AddTx(env.legacyTx(b, 3, &genesisStore, common.Big0, balSlotData(15, 0)))
			b.AddTx(env.legacyTx(b, 3, &late, common.Big0, balSlotData(1, 0)))
		}
		if n >= 8 {
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, balSlotData(byte(10+n%5), byte(n))))
		}
		b.AddTx(env.legacyTx(b, 5, &addrs[1], big.NewInt(int64(n)), nil))
	})

	// A history node: the genesis is written before live pruning is on.
	db := database.NewMemoryDBManager()
	env.gspec.MustCommit(db)
	db.WritePruningEnabled()
	require.NoError(t, WriteHistoryMode(db, HistoryModeHistory))
	const retention = 4
	chain, err := NewBlockChain(db, &blockchain.CacheConfig{
		CacheSize:            512,
		BlockInterval:        2,
		TriesInMemory:        2,
		LivePruningRetention: retention,
		TrieNodeCacheConfig:  statedb.GetEmptyTrieNodeCacheConfig(),
	}, &Config{Record: true}, env.config, env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { StopChain(chain) })
	chain.Processor().RegisterBlockStateModule(env.sealer)
	genesisDB := database.NewMemoryDBManager()
	env.gspec.MustCommit(genesisDB)
	of(chain).archive.genesisMu.Lock()
	of(chain).archive.genesis = state.NewDatabase(genesisDB)
	of(chain).archive.genesisMu.Unlock()

	_, err = chain.InsertChain(blocks[:first])
	require.NoError(t, err)
	waitBALIndex(t, chain, first+1)
	waitPruned(t, db, first-retention)

	accounts := map[common.Address]bool{genesisStore: true, contract: true, child: true, late: true, newEOA: true}
	for _, addr := range addrs {
		accounts[addr] = true
	}
	for i := range blocks[:first] {
		list, err := Read(db, uint64(i+1))
		require.NoError(t, err)
		for _, a := range list.Accounts {
			accounts[a.Address] = true
		}
	}
	slots := []common.Hash{}
	for i := 0; i <= 25; i++ {
		slots = append(slots, common.BigToHash(big.NewInt(int64(i))))
	}
	header := func(n uint64) *types.Header { return chain.GetHeaderByNumber(n) }

	check := func(n uint64) {
		t.Helper()
		root := header(n).Root
		require.NoError(t, completeTrie(db, root), "block %d", n)
		want, err := state.New(root, state.NewDatabase(env.db), nil, nil)
		require.NoError(t, err)
		got, err := state.New(root, state.NewDatabase(db), nil, nil)
		require.NoError(t, err)
		require.Equal(t, root, got.IntermediateRoot(true))
		for addr := range accounts {
			msg := fmt.Sprintf("block %d account %s", n, addr.Hex())
			require.Equal(t, want.Exist(addr), got.Exist(addr), msg)
			require.Equal(t, want.GetBalance(addr), got.GetBalance(addr), msg)
			require.Equal(t, want.GetNonce(addr), got.GetNonce(addr), msg)
			require.Equal(t, want.GetCodeHash(addr), got.GetCodeHash(addr), msg)
			require.Equal(t, accountFields(t, want.GetAccount(addr)), accountFields(t, got.GetAccount(addr)), msg)
			for _, slot := range slots {
				require.Equal(t, want.GetState(addr, slot), got.GetState(addr, slot), "%s slot %x", msg, slot)
			}
			if !got.IsProgramAccount(addr) {
				continue
			}
			ext, err := got.GetContractStorageRoot(addr)
			require.NoError(t, err)
			wantRoot, err := want.GetContractStorageRoot(addr)
			require.NoError(t, err)
			require.Equal(t, wantRoot.Unextend(), ext.Unextend(), msg)
		}

		// The proofs of eth_getProof, through StateAt.
		sdb, err := chain.StateAt(root)
		require.NoError(t, err)
		require.False(t, ReadOnly(sdb.Database()), "block %d", n)
		for _, addr := range []common.Address{contract, child, genesisStore} {
			if !sdb.IsProgramAccount(addr) {
				continue
			}
			ext, err := sdb.GetContractStorageRoot(addr)
			require.NoError(t, err)
			tr, err := statedb.NewStorageTrie(ext, sdb.Database().TrieDB(), nil)
			require.NoError(t, err)
			for _, slot := range slots {
				var proof proofNodes
				key := crypto.Keccak256(slot[:])
				require.NoError(t, tr.Prove(key, 0, &proof))
				value := verifyProof(t, ext.Unextend(), key, proof)
				var stored []byte
				if value != nil {
					require.NoError(t, rlp.DecodeBytes(value, &stored))
				}
				require.Equal(t, want.GetState(addr, slot), common.BytesToHash(stored), "block %d account %s slot %x", n, addr.Hex(), slot)
			}
		}
		tr, err := statedb.NewTrie(root, sdb.Database().TrieDB(), nil)
		require.NoError(t, err)
		var proof proofNodes
		require.NoError(t, tr.Prove(crypto.Keccak256(contract[:]), 0, &proof))
		require.NotNil(t, verifyProof(t, root, crypto.Keccak256(contract[:]), proof), "block %d", n)
	}

	// Pruned tries: the BAL history serves their values but no proofs.
	built := []uint64{3, 5, 12, 24}
	for _, n := range built {
		require.Error(t, completeTrie(db, header(n).Root), "block %d", n)
		sdb, err := chain.StateAt(header(n).Root)
		require.NoError(t, err)
		require.True(t, ReadOnly(sdb.Database()))
	}
	// A storage root with a nonce is not found without it.
	sdb, err := chain.StateAt(header(first).Root)
	require.NoError(t, err)
	ext, err := sdb.GetContractStorageRoot(contract)
	require.NoError(t, err)
	require.False(t, ext.IsZeroExtended())
	_, err = statedb.NewTrie(ext.Unextend(), sdb.Database().TrieDB(), nil)
	require.Error(t, err)

	for _, n := range built {
		s := waitTrieBuild(t, chain, n)
		require.Equal(t, uint64(first), s.From)
		require.Greater(t, s.Accounts, 0)
		check(n)
	}
	// The last stored trie is kept as it is, and the trie of a block before it
	// is left to pruning.
	require.NoError(t, completeTrie(db, header(first-2).Root))
	s := waitTrieBuild(t, chain, first)
	require.Equal(t, uint64(first), s.From)
	require.Zero(t, s.Accounts)
	built = append(built, first)
	check(first)
	s = waitTrieBuild(t, chain, 12)
	require.Zero(t, s.Accounts)

	// The head goes on, rewriting the genesis storage, the contracts and the
	// accounts the kept tries share with the head, which live pruning marks.
	_, err = chain.InsertChain(blocks[first:])
	require.NoError(t, err)
	waitBALIndex(t, chain, total+1)
	waitPruned(t, db, total-retention)
	for _, n := range built {
		check(n)
	}
	require.Error(t, completeTrie(db, header(first-2).Root))
	ok, _ := db.HasTrieNode(header(40).Root.ExtendZero())
	require.False(t, ok, "the root of a later block is pruned")

	// A build after the head moved on, over a later destruction.
	waitTrieBuild(t, chain, 9)
	check(9)

	// The kept tries survive a restart.
	StopChain(chain)
	chain, err = NewBlockChain(db, &blockchain.CacheConfig{
		CacheSize:            512,
		BlockInterval:        2,
		TriesInMemory:        2,
		LivePruningRetention: retention,
		TrieNodeCacheConfig:  statedb.GetEmptyTrieNodeCacheConfig(),
	}, &Config{Record: true}, env.config, env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { StopChain(chain) })
	of(chain).tries.mu.Lock()
	of(chain).tries.load(db)
	require.Len(t, of(chain).tries.kept, len(built)+1)
	of(chain).tries.mu.Unlock()
}

func TestStateTrieBuildModes(t *testing.T) {
	env := newBALTestEnv(t, "shanghai", 2)
	chain, _ := env.generateWithCache(regenCacheConfig(), &Config{Record: true}, 2, func(i int, b *blockchain.BlockGen) {})
	require.ErrorContains(t, StartTrieBuild(chain, 1), "history nodes only")
	require.Equal(t, TrieBuildStatus{}, GetTrieBuildStatus(chain))
	require.Error(t, StopTrieBuild(chain))
}
