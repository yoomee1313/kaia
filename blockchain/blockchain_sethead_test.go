// Modifications Copyright 2024 The Kaia Authors
// Copyright 2021 The klaytn Authors
// This file is part of the klaytn library.
//
// The klaytn library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The klaytn library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the klaytn library. If not, see <http://www.gnu.org/licenses/>.
// Modified and improved for the Kaia development.

package blockchain

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests a sethead for a short canonical chain where a recent block was already
// committed to disk and then the sethead called. In this case we expect the full
// chain to be rolled back to the committed block. Everything above the sethead
// point should be deleted. In between the committed block and the requested head
// the data can remain as "fast sync" data to avoid redownloading it.
type rewindTest struct {
	canonicalBlocks int     // Number of blocks to generate for the canonical chain (heavier)
	sidechainBlocks int     // Number of blocks to generate for the side chain (lighter)
	commitBlock     uint64  // Block number for which to commit the state to disk
	pivotBlock      *uint64 // Pivot block number in case of fast sync

	setheadBlock       uint64 // Block number to set head back to
	expCanonicalBlocks int    // Number of canonical blocks expected to remain in the database (excl. genesis)
	expSidechainBlocks int    // Number of sidechain blocks expected to remain in the database (excl. genesis)
	expHeadHeader      uint64 // Block number of the expected head header
	expHeadFastBlock   uint64 // Block number of the expected head fast sync block
	expHeadBlock       uint64 // Block number of the expected head full block
}

func TestShortSetHead(t *testing.T) { testShortSetHead(t) }

func testShortSetHead(t *testing.T) {
	// Chain:
	//   G->C1->C2->C3->C4->C5->C6->C7->C8 (HEAD)
	//
	// Frozen: none
	// Commit: G, C4
	// Pivot : none
	//
	// SetHead(7)
	//
	// ------------------------------
	//
	// Expected in leveldb:
	//   G->C1->C2->C3->C4->C5->C6->C7
	//
	// Expected head header    : C7
	// Expected head fast block: C7
	// Expected head block     : C7
	testSetHead(t, &rewindTest{
		canonicalBlocks:    8,
		sidechainBlocks:    0,
		commitBlock:        4,
		pivotBlock:         nil,
		setheadBlock:       7,
		expCanonicalBlocks: 7,
		expSidechainBlocks: 0,
		expHeadHeader:      7,
		expHeadFastBlock:   7,
		expHeadBlock:       7,
	})
}

// Tests a sethead for a short canonical chain and a shorter side chain, where a
// recent block was already committed to disk and then sethead was called. In this
// test scenario the side chain is below the committed block. In this case we expect
// the canonical full chain to be rolled back to the committed block. Everything
// above the sethead point should be deleted. In between the committed block and
// the requested head the data can remain as "fast sync" data to avoid redownloading
// it. The side chain should be left alone as it was shorter.

func TestLongDeepSetHead(t *testing.T) { testLongDeepSetHead(t, false) }

func testLongDeepSetHead(t *testing.T, snapshots bool) {
	// Chain:
	//   G->C1->C2->C3->C4->C5->C6->C7->C8->C9->C10->C11->C12->C13->C14->C15->C16->C17->C18->C19->C20->C21->C22->C23->C24 (HEAD)
	//
	//
	// Commit: G, C4
	// Pivot : none
	//
	// SetHead(6)
	//
	// -----------------------------
	//
	// Expected in leveldb: none
	//
	// Expected head header    : C4
	// Expected head fast block: C4
	// Expected head block     : C4
	testSetHead(t, &rewindTest{
		canonicalBlocks:    24,
		sidechainBlocks:    0,
		commitBlock:        4,
		pivotBlock:         nil,
		setheadBlock:       6,
		expCanonicalBlocks: 6,
		expSidechainBlocks: 0,
		expHeadHeader:      6,
		expHeadFastBlock:   6,
		expHeadBlock:       6,
	})
}

func testSetHead(t *testing.T, tt *rewindTest) {
	db, chain, err := newCanonical(faker.NewFullFaker(), 0, true)
	if err != nil {
		t.Fatalf("failed to create pristine chain: %v", err)
	}
	defer chain.Stop()
	chain.Config().Istanbul = params.GetDefaultIstanbulConfig()

	// If sidechain blocks are needed, make a light chain and import it
	var sideblocks types.Blocks
	if tt.sidechainBlocks > 0 {
		sideblocks, _ = GenerateChain(params.TestChainConfig, chain.CurrentBlock(), faker.NewFaker(), db, tt.canonicalBlocks, func(i int, b *BlockGen) {})
		if _, err := chain.InsertChain(sideblocks); err != nil {
			t.Fatalf("Failed to import side chain: %v", err)
		}
	}

	canonblocks, _ := GenerateChain(params.TestChainConfig, chain.CurrentBlock(), faker.NewFaker(), db, tt.canonicalBlocks, func(i int, b *BlockGen) {})
	if _, err := chain.InsertChain(canonblocks[:tt.commitBlock]); err != nil {
		t.Fatalf("Failed to import canonical chain start: %v", err)
	}
	if tt.commitBlock > 0 {
		chain.StateCache().TrieDB().Commit(canonblocks[tt.commitBlock-1].Root(), true, tt.commitBlock)
	}
	if _, err := chain.InsertChain(canonblocks[tt.commitBlock:]); err != nil {
		t.Fatalf("Failed to import canonical chain tail: %v", err)
	}
	// Manually dereference anything not committed to not have to work with 128+ tries
	for _, block := range sideblocks {
		chain.StateCache().TrieDB().Dereference(block.Root())
	}
	for _, block := range canonblocks {
		chain.StateCache().TrieDB().Dereference(block.Root())
	}

	// Set the head of the chain back to the requested number
	chain.SetHead(tt.setheadBlock)

	// Iterate over all the remaining blocks and ensure there are no gaps
	verifyNoGaps(t, chain, true, canonblocks)
	verifyNoGaps(t, chain, false, sideblocks)
	verifyCutoff(t, chain, true, canonblocks, tt.expCanonicalBlocks)
	verifyCutoff(t, chain, false, sideblocks, tt.expSidechainBlocks)

	if head := chain.CurrentHeader(); head.Number.Uint64() != tt.expHeadHeader {
		t.Errorf("Head header mismatch!: have %d, want %d", head.Number, tt.expHeadHeader)
	}
	if head := chain.CurrentFastBlock(); head.NumberU64() != tt.expHeadFastBlock {
		t.Errorf("Head fast block mismatch: have %d, want %d", head.NumberU64(), tt.expHeadFastBlock)
	}
	if head := chain.CurrentBlock(); head.NumberU64() != tt.expHeadBlock {
		t.Errorf("Head block mismatch!!: have %d, want %d", head.NumberU64(), tt.expHeadBlock)
	}
}

// verifyNoGaps checks that there are no gaps after the initial set of blocks in
// the database and errors if found.
func verifyNoGaps(t *testing.T, chain *BlockChain, canonical bool, inserted types.Blocks) {
	t.Helper()

	var end uint64
	for i := uint64(0); i <= uint64(len(inserted)); i++ {
		header := chain.GetHeaderByNumber(i)
		if header == nil && end == 0 {
			end = i
		}
		if header != nil && end > 0 {
			if canonical {
				t.Errorf("Canonical header gap between #%d-#%d", end, i-1)
			} else {
				t.Errorf("Sidechain header gap between #%d-#%d", end, i-1)
			}
			end = 0 // Reset for further gap detection
		}
	}
	end = 0
	for i := uint64(0); i <= uint64(len(inserted)); i++ {
		block := chain.GetBlockByNumber(i)
		if block == nil && end == 0 {
			end = i
		}
		if block != nil && end > 0 {
			if canonical {
				t.Errorf("Canonical block gap between #%d-#%d", end, i-1)
			} else {
				t.Errorf("Sidechain block gap between #%d-#%d", end, i-1)
			}
			end = 0 // Reset for further gap detection
		}
	}
	end = 0
	for i := uint64(1); i <= uint64(len(inserted)); i++ {
		receipts := chain.GetReceiptsByBlockHash(inserted[i-1].Hash())
		if receipts == nil && end == 0 {
			end = i
		}
		if receipts != nil && end > 0 {
			if canonical {
				t.Errorf("Canonical receipt gap between #%d-#%d", end, i-1)
			} else {
				t.Errorf("Sidechain receipt gap between #%d-#%d", end, i-1)
			}
			end = 0 // Reset for further gap detection
		}
	}
}

// verifyCutoff checks that there are no chain data available in the chain after
// the specified limit, but that it is available before.
func verifyCutoff(t *testing.T, chain *BlockChain, canonical bool, inserted types.Blocks, head int) {
	t.Helper()

	for i := 1; i <= len(inserted); i++ {
		if i <= head {
			if header := chain.GetHeader(inserted[i-1].Hash(), uint64(i)); header == nil {
				if canonical {
					t.Errorf("Canonical header   #%2d [%x...] missing before cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				} else {
					t.Errorf("Sidechain header   #%2d [%x...] missing before cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				}
			}
			if block := chain.GetBlock(inserted[i-1].Hash(), uint64(i)); block == nil {
				if canonical {
					t.Errorf("Canonical block    #%2d [%x...] missing before cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				} else {
					t.Errorf("Sidechain block    #%2d [%x...] missing before cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				}
			}
			if receipts := chain.GetReceiptsByBlockHash(inserted[i-1].Hash()); receipts == nil {
				if canonical {
					t.Errorf("Canonical receipts #%2d [%x...] missing before cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				} else {
					t.Errorf("Sidechain receipts #%2d [%x...] missing before cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				}
			}
		} else {
			if header := chain.GetHeader(inserted[i-1].Hash(), uint64(i)); header != nil {
				if canonical {
					t.Errorf("Canonical header   #%2d [%x...] present after cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				} else {
					t.Errorf("Sidechain header   #%2d [%x...] present after cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				}
			}
			if block := chain.GetBlock(inserted[i-1].Hash(), uint64(i)); block != nil {
				if canonical {
					t.Errorf("Canonical block    #%2d [%x...] present after cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				} else {
					t.Errorf("Sidechain block    #%2d [%x...] present after cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				}
			}
			if receipts := chain.GetReceiptsByBlockHash(inserted[i-1].Hash()); receipts != nil {
				if canonical {
					t.Errorf("Canonical receipts #%2d [%x...] present after cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				} else {
					t.Errorf("Sidechain receipts #%2d [%x...] present after cap %d", inserted[i-1].Number(), inserted[i-1].Hash().Bytes()[:3], head)
				}
			}
		}
	}
}

func TestSetHeadEarlyExit(t *testing.T) {
	testSetHeadEarlyExit(t, &rewindTest{
		canonicalBlocks: 8,
		setheadBlock:    3,
	})
}

func testSetHeadEarlyExit(t *testing.T, tt *rewindTest) {
	db, chain, err := newCanonical(faker.NewFullFaker(), 0, true)
	if err != nil {
		t.Fatalf("failed to create pristine chain: %v", err)
	}
	defer chain.Stop()
	chain.Config().Istanbul = params.GetDefaultIstanbulConfig()

	canonblocks, _ := GenerateChain(params.TestChainConfig, chain.CurrentBlock(), faker.NewFaker(), db, tt.canonicalBlocks, func(i int, b *BlockGen) {})
	if _, err := chain.InsertChain(canonblocks); err != nil {
		t.Fatalf("Failed to import canonical chain start: %v", err)
	}

	db.WriteLastPrunedBlockNumber(tt.setheadBlock + 1)
	assert.Error(t, chain.SetHead(tt.setheadBlock))

	db.WriteLastPrunedBlockNumber(tt.setheadBlock)
	assert.Error(t, chain.SetHead(tt.setheadBlock))

	db.WriteLastPrunedBlockNumber(tt.setheadBlock - 1)
	assert.Nil(t, chain.SetHead(tt.setheadBlock))
}

// frozenTestDB reports a fixed number of frozen blocks and records the
// truncation of the freezer.
type frozenTestDB struct {
	database.DBManager
	frozen      uint64
	truncatedTo uint64
}

func (db *frozenTestDB) PauseFreezer() (uint64, func()) { return db.frozen, func() {} }

func (db *frozenTestDB) TruncateFrozen(items uint64) error {
	if items < db.frozen {
		db.frozen, db.truncatedTo = items, items
	}
	return nil
}

// TestSetHeadBelowFreezerToFindState checks that a rewind that has to go below
// the frozen blocks to find a block with state truncates the freezer down to
// the block with state.
func TestSetHeadBelowFreezerToFindState(t *testing.T) {
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		gspec   = &Genesis{Config: params.TestChainConfig, Alloc: GenesisAlloc{address: {Balance: big.NewInt(1000000000)}}}
		signer  = types.LatestSignerForChainID(gspec.Config.ChainID)
		db      = &frozenTestDB{DBManager: database.NewMemoryDBManager()}
	)
	genesis := gspec.MustCommit(db)
	chain, err := NewBlockChain(db, nil, gspec.Config, faker.NewFullFaker(), vm.Config{})
	require.NoError(t, err)
	defer chain.Stop()
	chain.Config().Istanbul = params.GetDefaultIstanbulConfig()

	// Every block changes the state, and only blocks up to 2 keep their state.
	blocks, _ := GenerateChain(gspec.Config, genesis, faker.NewFaker(), db, 8, func(i int, b *BlockGen) {
		tx, err := types.SignTx(types.NewTransaction(b.TxNonce(address), common.Address{byte(i + 1)}, big.NewInt(1), params.TxGas, nil, nil), signer, key)
		require.NoError(t, err)
		b.AddTx(tx)
	})
	_, err = chain.InsertChain(blocks)
	require.NoError(t, err)
	for _, b := range blocks[2:] {
		chain.StateCache().TrieDB().Dereference(b.Root())
		db.DeleteTrieNode(b.Root().ExtendZero())
	}

	db.frozen = 5 // blocks [0, 4] are frozen
	require.NoError(t, chain.SetHead(5))
	assert.Equal(t, uint64(2), chain.CurrentBlock().NumberU64())
	assert.Equal(t, uint64(2), chain.CurrentHeader().Number.Uint64())
	assert.Equal(t, uint64(3), db.truncatedTo)
}

// TestSetHeadBelowFreezer checks that SetHead truncates the freezer when it
// rewinds below the frozen blocks, and leaves it alone otherwise.
func TestSetHeadBelowFreezer(t *testing.T) {
	db := &frozenTestDB{DBManager: database.NewMemoryDBManager()}
	genesis := new(Genesis).MustCommit(db)
	chain, err := NewBlockChain(db, nil, params.TestChainConfig, faker.NewFullFaker(), vm.Config{})
	require.NoError(t, err)
	defer chain.Stop()
	chain.Config().Istanbul = params.GetDefaultIstanbulConfig()

	blocks, _ := GenerateChain(params.TestChainConfig, genesis, faker.NewFaker(), db, 8, func(i int, b *BlockGen) {})
	_, err = chain.InsertChain(blocks)
	require.NoError(t, err)

	db.frozen = 5 // blocks [0, 4] are frozen
	require.NoError(t, chain.SetHead(6))
	assert.Equal(t, uint64(5), db.frozen)
	assert.Zero(t, db.truncatedTo)

	require.NoError(t, chain.SetHead(3))
	assert.Equal(t, uint64(3), chain.CurrentBlock().NumberU64())
	assert.Equal(t, uint64(4), db.truncatedTo)
}

// rewindBlocks makes blocks that set slots of a contract, transfer and create,
// write and destroy a child contract over and over.
func rewindBlocks(env *Env, total int) []*types.Block {
	var (
		addrs    = env.Addrs
		contract = crypto.CreateAddress(addrs[0], 0)
		initCode = InitCode(DestructibleCode())
		child    = crypto.CreateAddress2(FactoryAddr, [32]byte{}, crypto.Keccak256(initCode))
	)
	blocks, _ := GenerateChain(env.Config, env.Genesis, env.Sealer, env.DB, total, func(i int, b *BlockGen) {
		n := i + 1
		switch {
		case n == 1:
			b.AddTx(env.LegacyTx(b, 0, nil, common.Big0, InitCode(StoreCode())))
		case n%5 == 1:
			b.AddTx(env.LegacyTx(b, 2, &FactoryAddr, common.Big0, initCode))
		case n%5 == 2:
			b.AddTx(env.LegacyTx(b, 2, &child, common.Big0, SlotData(3, byte(n))))
		case n%5 == 3:
			b.AddTx(env.LegacyTx(b, 2, &child, common.Big0, nil))
		}
		if n > 1 {
			b.AddTx(env.LegacyTx(b, 0, &contract, common.Big0, SlotData(byte(n%6), byte(n%4))))
		}
		b.AddTx(env.LegacyTx(b, 1, &addrs[3], big.NewInt(int64(n)), nil))
	})
	return blocks
}

// TestSetHeadHistory rewinds a live-pruned history node to a block next to a
// stored trie and to a committed one whose trie live pruning went over, which
// keeps its root but lost nodes below it: the tries are built from the BAL
// history, and the chain goes on from them, inserting the same blocks again.
// With a trie node cache the roots of pruned tries may still be cached, and
// with every trie in memory the trie to build from is in memory only.
func TestSetHeadHistory(t *testing.T) {
	t.Run("no cache", func(t *testing.T) { testSetHeadHistory(t, statedb.GetEmptyTrieNodeCacheConfig(), 2) })
	t.Run("trie node cache", func(t *testing.T) {
		testSetHeadHistory(t, &statedb.TrieNodeCacheConfig{CacheType: statedb.CacheTypeLocal, LocalCacheSizeMiB: 32}, 2)
	})
	t.Run("tries in memory", func(t *testing.T) {
		testSetHeadHistory(t, &statedb.TrieNodeCacheConfig{CacheType: statedb.CacheTypeLocal, LocalCacheSizeMiB: 32}, DefaultTriesInMemory)
	})
}

func testSetHeadHistory(t *testing.T, cacheConfig *statedb.TrieNodeCacheConfig, triesInMemory uint64) {
	env := NewEnv(t, "shanghai", 4)
	const total, first, retention = 40, 30, 4
	blocks := rewindBlocks(env, total)

	db := database.NewMemoryDBManager()
	env.Gspec.MustCommit(db)
	db.WritePruningEnabled()
	chain, err := newBALChain(db, &CacheConfig{
		CacheSize:            512,
		BlockInterval:        2,
		TriesInMemory:        triesInMemory,
		LivePruningRetention: retention,
		TrieNodeCacheConfig:  cacheConfig,
	}, env.Config, env.Sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { chain.Stop() })
	chain.Processor().RegisterBlockStateModule(env.Sealer)
	chain.SetGenesisSpec(env.Gspec)

	_, err = chain.InsertChain(blocks[:first])
	require.NoError(t, err)
	WaitBALIndex(t, chain, first+1)
	WaitPruned(t, db, first-retention)

	for _, n := range []uint64{first - 3, 8} {
		require.NoError(t, chain.SetHead(n))
		require.Equal(t, n, chain.CurrentBlock().NumberU64())
		require.True(t, chain.States().HasTrie(blocks[n-1].Root()), "block %d", n)
	}
	require.NoError(t, CompleteTrie(db, blocks[7].Root()))

	for i := 8; i < total; i++ {
		_, err = chain.InsertChain(blocks[i : i+1])
		require.NoError(t, err, "block %d", i+1)
	}
	require.Equal(t, uint64(total), chain.CurrentBlock().NumberU64())
	WaitPruned(t, db, total-retention)
	require.NoError(t, CompleteTrie(db, blocks[total-1].Root()))
	require.NoError(t, CompleteTrie(db, env.Genesis.Root()), "a history node keeps the genesis state")
}

// TestRestartHistory opens a history node again as after a crash, with the
// trie of the head lost: it is built forward from the last stored trie with
// the stored lists, so the head stays. With a wrong stored list after the
// stored trie, the head trie cannot be built, so the chain goes back to the
// stored trie and drops the lists above it, which the blocks executed again
// store anew.
func TestRestartHistory(t *testing.T) {
	const total, head, stored = 24, 20, 16
	for _, badList := range []bool{false, true} {
		t.Run(fmt.Sprintf("bad list %v", badList), func(t *testing.T) {
			env := NewEnv(t, "shanghai", 4)
			blocks := rewindBlocks(env, total)
			db := database.NewMemoryDBManager()
			env.Gspec.MustCommit(db)
			cacheConfig := &CacheConfig{
				CacheSize:           512,
				BlockInterval:       stored,
				TriesInMemory:       DefaultTriesInMemory,
				TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
			}
			crashed, err := newBALChain(db, cacheConfig, env.Config, env.Sealer, vm.Config{})
			require.NoError(t, err)
			t.Cleanup(func() { crashed.Stop() })
			crashed.Processor().RegisterBlockStateModule(env.Sealer)
			_, err = crashed.InsertChain(blocks[:head])
			require.NoError(t, err)
			require.Error(t, CompleteTrie(db, blocks[head-1].Root()), "the trie of the head is in memory only")
			want, err := db.ReadCanonicalBAL(18)
			require.NoError(t, err)
			if badList {
				wrong, err := db.ReadCanonicalBAL(17)
				require.NoError(t, err)
				require.NoError(t, db.WriteBAL(blocks[17].Hash(), 18, wrong))
			}

			chain, err := newBALChain(db, cacheConfig, env.Config, env.Sealer, vm.Config{})
			require.NoError(t, err)
			t.Cleanup(func() { chain.Stop() })
			if badList {
				require.Equal(t, uint64(stored), chain.CurrentBlock().NumberU64())
				for n := uint64(stored + 1); n <= head; n++ {
					_, err := db.ReadCanonicalBAL(n)
					require.ErrorIs(t, err, database.ErrBALNotStored, "block %d", n)
				}
			} else {
				require.Equal(t, uint64(head), chain.CurrentBlock().NumberU64())
				require.NoError(t, CompleteTrie(db, blocks[head-1].Root()))
			}

			chain.Processor().RegisterBlockStateModule(env.Sealer)
			_, err = chain.InsertChain(blocks[chain.CurrentBlock().NumberU64():])
			require.NoError(t, err)
			require.Equal(t, uint64(total), chain.CurrentBlock().NumberU64())
			got, err := db.ReadCanonicalBAL(18)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

// TestSetHeadHistoryFork rewinds a live-pruned history node below the last
// pruned block and goes on with other blocks than the deleted ones: the
// pruning marks the deleted blocks left must not prune the nodes the new head
// still has.
func TestSetHeadHistoryFork(t *testing.T) {
	for _, head := range []int{8, 27} {
		t.Run(fmt.Sprintf("head %d", head), func(t *testing.T) { testSetHeadHistoryFork(t, head) })
	}
}

func testSetHeadHistoryFork(t *testing.T, head int) {
	env := NewEnv(t, "shanghai", 4)
	const total, first, retention = 40, 30, 4
	blocks := rewindBlocks(env, total)

	db := database.NewMemoryDBManager()
	env.Gspec.MustCommit(db)
	db.WritePruningEnabled()
	chain, err := newBALChain(db, &CacheConfig{
		CacheSize:            512,
		BlockInterval:        2,
		TriesInMemory:        2,
		LivePruningRetention: retention,
		TrieNodeCacheConfig:  statedb.GetEmptyTrieNodeCacheConfig(),
	}, env.Config, env.Sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { chain.Stop() })
	chain.Processor().RegisterBlockStateModule(env.Sealer)
	chain.SetGenesisSpec(env.Gspec)

	_, err = chain.InsertChain(blocks[:first])
	require.NoError(t, err)
	WaitBALIndex(t, chain, first+1)
	WaitPruned(t, db, first-retention)
	require.NoError(t, chain.SetHead(uint64(head)))

	// Blocks that only transfer, leaving the contracts as they were at head.
	fork, _ := GenerateChain(env.Config, blocks[head-1], env.Sealer, env.DB, total, func(i int, b *BlockGen) {
		b.AddTx(env.LegacyTx(b, 1, &env.Addrs[3], big.NewInt(int64(i+1)), nil))
	})
	for i := range fork {
		_, err = chain.InsertChain(fork[i : i+1])
		require.NoError(t, err, "fork block %d", head+i+1)
	}
	// Tries are committed, and pruned, every 2 blocks.
	last := head + total
	last -= last % 2
	WaitPruned(t, db, uint64(last-retention))
	require.NoError(t, CompleteTrie(db, fork[last-head-1].Root()))
}

// TestHistoryModeSwitchFork opens the database of a full node, which kept the
// state of its head only, as a history node, and inserts a longer fork from
// the parent of the head: the blocks before it, which have neither a state
// trie nor a list, are executed again to get the state the fork needs.
func TestHistoryModeSwitchFork(t *testing.T) {
	env := NewEnv(t, "shanghai", 4)
	const head = 12
	blocks := rewindBlocks(env, head)
	fork, _ := GenerateChain(env.Config, blocks[head-2], env.Sealer, env.DB, 3, func(i int, b *BlockGen) {
		b.AddTx(env.LegacyTx(b, 1, &env.Addrs[3], big.NewInt(int64(i+100)), nil))
	})

	db := database.NewMemoryDBManager()
	env.Gspec.MustCommit(db)
	cacheConfig := &CacheConfig{
		CacheSize:           512,
		BlockInterval:       DefaultBlockInterval,
		TriesInMemory:       DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
	}
	full, err := NewBlockChain(db, cacheConfig, env.Config, env.Sealer, vm.Config{})
	require.NoError(t, err)
	full.Processor().RegisterBlockStateModule(env.Sealer)
	_, err = full.InsertChain(blocks)
	require.NoError(t, err)
	full.Stop()
	require.Error(t, CompleteTrie(db, blocks[head-2].Root()), "a full node keeps the state of its head only")

	chain, err := newBALChain(db, cacheConfig, env.Config, env.Sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { chain.Stop() })
	chain.Processor().RegisterBlockStateModule(env.Sealer)
	require.Equal(t, uint64(head), chain.CurrentBlock().NumberU64())
	_, err = chain.InsertChain(fork)
	require.NoError(t, err)
	require.Equal(t, fork[len(fork)-1].Hash(), chain.CurrentBlock().Hash())
}
