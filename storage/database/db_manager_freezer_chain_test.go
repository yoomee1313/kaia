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
	dbm := database.NewDBManager(&database.DBConfig{
		Dir: t.TempDir(), DBType: database.LevelDB, NumStateTrieShards: 1,
		EnableFreezer: true, FreezerThreshold: 5,
	})
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
