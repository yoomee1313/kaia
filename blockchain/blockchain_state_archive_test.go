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
	"encoding/binary"
	"math/big"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStatePruningWithStateArchive checks that the state of every block stays
// readable with live pruning and the state archive, also after the counter of
// the node keys went backwards, after a rewind below the prune window, and
// through merkle proofs.
func TestStatePruningWithStateArchive(t *testing.T) {
	log.EnableLogForTest(log.LvlCrit, log.LvlInfo)
	defer common.ResetExtHashCounterForTest(uint64(time.Now().UnixNano() >> 8))

	var (
		db = database.NewDBManager(&database.DBConfig{
			Dir: t.TempDir(), DBType: database.LevelDB, NumStateTrieShards: 1,
			EnableStateArchive: true,
		})
		genDB   = database.NewMemoryDBManager()
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		addr2   = common.HexToAddress("0xaaaa")
		// CALLVALUE PUSH1 0 SSTORE STOP: stores the value of the call in slot 0.
		contract = common.HexToAddress("0xcccc")

		gspec = &Genesis{
			Config: params.TestChainConfig,
			Alloc: GenesisAlloc{
				addr1:    {Balance: big.NewInt(10000000000000)},
				contract: {Balance: common.Big0, Code: common.FromHex("0x3460005500")},
			},
		}
		genesis = gspec.MustCommit(db)
		signer  = types.LatestSignerForChainID(gspec.Config.ChainID)
		sealer  = faker.NewFaker()

		retention = uint64(3)
		numBlocks = 20
	)
	defer db.Close()
	gspec.MustCommit(genDB)
	require.True(t, db.StateArchiveEnabled())
	db.WritePruningEnabled()

	cacheConfig := &CacheConfig{
		ArchiveMode:          true, // Commit every block, as an archive node does
		CacheSize:            512,
		BlockInterval:        DefaultBlockInterval,
		TriesInMemory:        DefaultTriesInMemory,
		LivePruningRetention: retention,
		TrieNodeCacheConfig:  statedb.GetEmptyTrieNodeCacheConfig(),
	}
	// Block n sends n to addr2 in total, and stores n in slot 0 of the contract.
	genBlocks := func(parent *types.Block, n int) []*types.Block {
		blocks, _ := GenerateChain(gspec.Config, parent, sealer, genDB, n, func(i int, gen *BlockGen) {
			tx, _ := types.SignTx(types.NewTransaction(
				gen.TxNonce(addr1), addr2, common.Big1, 21000, common.Big1, nil), signer, key1)
			gen.AddTx(tx)
			tx, _ = types.SignTx(types.NewTransaction(
				gen.TxNonce(addr1), contract, gen.Number(), 100000, common.Big1, nil), signer, key1)
			gen.AddTx(tx)
		})
		return blocks
	}
	chain := genBlocks(genesis, numBlocks)

	bc, err := NewBlockChain(db, cacheConfig, gspec.Config, sealer, vm.Config{})
	require.NoError(t, err)
	_, err = bc.InsertChain(chain[:numBlocks/2])
	require.NoError(t, err)

	// Move the counter of the node keys back, as a clock going backwards between
	// restarts would. The next nodes are archived as exceptions until their
	// counters pass the archived ones again.
	state, err := bc.State()
	require.NoError(t, err)
	storageRoot, err := state.GetContractStorageRoot(contract)
	require.NoError(t, err)
	counter := storageRoot.Counter()
	var buf [8]byte
	copy(buf[1:], counter[:])
	common.ResetExtHashCounterForTest(binary.BigEndian.Uint64(buf[:]) - 50)

	_, err = bc.InsertChain(chain[numBlocks/2:])
	require.NoError(t, err)
	require.Equal(t, uint64(numBlocks), bc.CurrentBlock().NumberU64())

	pruneNum := uint64(numBlocks) - retention
	require.Eventually(t, func() bool {
		n, err := db.ReadLastPrunedBlockNumber()
		return err == nil && n == pruneNum
	}, 5*time.Second, 10*time.Millisecond)

	// Reopen the blockchain with a clean trie cache, so that the nodes are read
	// from disk.
	bc.Stop()
	bc, err = NewBlockChain(db, cacheConfig, gspec.Config, sealer, vm.Config{})
	require.NoError(t, err)
	defer func() { bc.Stop() }()

	assertState := func(block *types.Block, want int64) {
		t.Helper()
		state, err := bc.StateAt(block.Root())
		require.NoError(t, err, block.NumberU64())
		assert.Equal(t, want, state.GetBalance(addr2).Int64(), block.NumberU64())
		assert.Equal(t, common.BigToHash(big.NewInt(want)), state.GetState(contract, common.Hash{}), block.NumberU64())
	}
	for num := uint64(0); num <= uint64(numBlocks); num++ {
		block := bc.GetBlockByNumber(num)
		assertState(block, int64(num))
		// The pruned roots are gone from the key-value store; the archive served them.
		if num >= 1 && num <= pruneNum {
			ok, _ := db.GetStateTrieDB().Has(block.Root().Bytes())
			assert.False(t, ok, "root of block %d is still in the key-value store", num)
		}
	}

	// A merkle proof of an account at a pruned block, checked as a client
	// checks the nodes returned by eth_getProof.
	root := bc.GetBlockByNumber(3).Root()
	trie, err := statedb.NewTrie(root, bc.StateCache().TrieDB(), nil)
	require.NoError(t, err)
	var proof proofNodes
	key := crypto.Keccak256(addr2.Bytes())
	require.NoError(t, trie.Prove(key, 0, &proof))
	proofDB := database.NewMemoryDBManager()
	for _, enc := range proof {
		proofDB.WriteTrieNode(crypto.Keccak256Hash(enc).ExtendZero(), enc)
	}
	val, err, _ := statedb.VerifyProof(root, key, proofDB)
	require.NoError(t, err)
	assert.NotEmpty(t, val)

	// A rewind below the prune window works, and the chain continues from there.
	require.NoError(t, bc.SetHead(2))
	require.Equal(t, uint64(2), bc.CurrentBlock().NumberU64())
	fork := genBlocks(chain[1], 5)
	_, err = bc.InsertChain(fork)
	require.NoError(t, err)
	for _, block := range fork {
		assertState(block, int64(block.NumberU64()))
	}
}

// proofNodes collects the encoded nodes of a merkle proof, as eth_getProof
// returns them. With live pruning, Trie.Prove writes the nodes below the root
// under their ExtHash, which VerifyProof does not look up.
type proofNodes [][]byte

func (p *proofNodes) WriteMerkleProof(key, value []byte) {
	*p = append(*p, value)
}
