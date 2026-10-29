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

package database_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newFreezerChain(t *testing.T) (database.DBManager, *blockchain.BlockChain, *types.Block) {
	return newFreezerChainAt(t, t.TempDir())
}

func openFreezerDB(dir string) database.DBManager {
	return database.NewDBManager(&database.DBConfig{
		Dir: dir, DBType: database.LevelDB, NumStateTrieShards: 1,
		EnableFreezer: true, FreezerThreshold: 5,
	})
}

func newFreezerChainAt(t *testing.T, dir string) (database.DBManager, *blockchain.BlockChain, *types.Block) {
	dbm := openFreezerDB(dir)
	genesis := new(blockchain.Genesis).MustCommit(dbm)
	chain, err := blockchain.NewBlockChain(dbm, nil, params.TestChainConfig, faker.NewFullFaker(), vm.Config{})
	require.NoError(t, err)
	return dbm, chain, genesis
}

// importBlocks inserts the RLP stream like utils.ImportChain does: the genesis
// is skipped, and so are blocks the chain already has (with state from the
// head on).
func importBlocks(t *testing.T, chain *blockchain.BlockChain, r io.Reader) int {
	var missing types.Blocks
	stream := rlp.NewStream(r, 0)
	for {
		var b types.Block
		if err := stream.Decode(&b); err == io.EOF {
			break
		} else {
			require.NoError(t, err)
		}
		if b.NumberU64() == 0 {
			continue
		}
		if b.NumberU64() < chain.CurrentBlock().NumberU64() && chain.HasBlock(b.Hash(), b.NumberU64()) ||
			chain.HasBlockAndState(b.Hash(), b.NumberU64()) {
			continue
		}
		missing = append(missing, &b)
	}
	if len(missing) > 0 {
		_, err := chain.InsertChain(missing)
		require.NoError(t, err)
	}
	return len(missing)
}

// TestFreezer_ExportImport exports a chain whose old blocks are frozen and
// imports it into a new node and back into the same node.
func TestFreezer_ExportImport(t *testing.T) {
	const numBlocks = 20

	srcDB, src, genesis := newFreezerChain(t)
	defer srcDB.Close()
	defer src.Stop()
	blocks, _ := blockchain.GenerateChain(params.TestChainConfig, genesis, faker.NewFaker(), srcDB, numBlocks, func(int, *blockchain.BlockGen) {})
	_, err := src.InsertChain(blocks)
	require.NoError(t, err)
	database.TriggerFreeze(t, srcDB)
	require.Equal(t, uint64(numBlocks+1-5), srcDB.FrozenBlocks())

	var exported bytes.Buffer
	require.NoError(t, src.Export(&exported))

	// A new node imports every block, and freezes them later.
	dstDB, dst, _ := newFreezerChain(t)
	defer dstDB.Close()
	defer dst.Stop()
	assert.Equal(t, numBlocks, importBlocks(t, dst, bytes.NewReader(exported.Bytes())))
	assert.Equal(t, uint64(numBlocks), dst.CurrentBlock().NumberU64())
	database.TriggerFreeze(t, dstDB)
	assert.Equal(t, srcDB.FrozenBlocks(), dstDB.FrozenBlocks())
	for _, b := range blocks {
		assert.Equal(t, b.Hash(), dst.GetBlockByNumber(b.NumberU64()).Hash())
	}

	// The source node finds the frozen blocks and has nothing to import.
	assert.Equal(t, 0, importBlocks(t, src, bytes.NewReader(exported.Bytes())))
}

// TestFreezer_SetHead rewinds a chain below its frozen blocks, builds a fork on
// the new head, freezes it and reopens the database.
func TestFreezer_SetHead(t *testing.T) {
	const (
		numBlocks = 20
		head      = 8
	)
	dir := t.TempDir()
	dbm, chain, genesis := newFreezerChainAt(t, dir)
	blocks, _ := blockchain.GenerateChain(params.TestChainConfig, genesis, faker.NewFaker(), dbm, numBlocks, func(int, *blockchain.BlockGen) {})
	_, err := chain.InsertChain(blocks)
	require.NoError(t, err)
	database.TriggerFreeze(t, dbm)
	require.Equal(t, uint64(numBlocks+1-5), dbm.FrozenBlocks())

	require.NoError(t, chain.SetHead(head))
	assert.Equal(t, uint64(head), chain.CurrentBlock().NumberU64())
	assert.Equal(t, uint64(head+1), dbm.FrozenBlocks())
	for _, b := range blocks[:head] {
		assert.Equal(t, b.Hash(), chain.GetBlockByNumber(b.NumberU64()).Hash(), "block %d", b.NumberU64())
	}
	for _, b := range blocks[head:] {
		assert.Nil(t, chain.GetBlockByNumber(b.NumberU64()), "block %d", b.NumberU64())
		assert.False(t, chain.HasBlock(b.Hash(), b.NumberU64()), "block %d", b.NumberU64())
	}

	// A fork on the new head is inserted and frozen in place of the old blocks.
	fork, _ := blockchain.GenerateChain(params.TestChainConfig, blocks[head-1], faker.NewFaker(), dbm, numBlocks, func(_ int, b *blockchain.BlockGen) {
		b.SetExtra([]byte("fork"))
	})
	_, err = chain.InsertChain(fork)
	require.NoError(t, err)
	database.TriggerFreeze(t, dbm)
	require.Equal(t, uint64(head+numBlocks+1-5), dbm.FrozenBlocks())
	for _, b := range fork {
		assert.Equal(t, b.Hash(), chain.GetBlockByNumber(b.NumberU64()).Hash(), "fork block %d", b.NumberU64())
	}
	chain.Stop()
	dbm.Close()

	// The rewound and refrozen chain opens again.
	dbm = openFreezerDB(dir)
	defer dbm.Close()
	chain, err = blockchain.NewBlockChain(dbm, nil, params.TestChainConfig, faker.NewFullFaker(), vm.Config{})
	require.NoError(t, err)
	defer chain.Stop()
	assert.Equal(t, uint64(head+numBlocks+1-5), dbm.FrozenBlocks())
	assert.Equal(t, fork[len(fork)-1].Hash(), chain.CurrentBlock().Hash())
	for _, b := range append(blocks[:head], fork...) {
		assert.Equal(t, b.Hash(), dbm.ReadCanonicalHash(b.NumberU64()), "block %d", b.NumberU64())
	}
}
