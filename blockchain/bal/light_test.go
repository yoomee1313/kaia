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

package bal

import (
	"math/big"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

func TestCheckHistoryMode(t *testing.T) {
	db := database.NewMemoryDBManager()
	require.ErrorContains(t, CheckHistoryMode(db, "archive"), "unknown history mode")
	require.NoError(t, CheckHistoryMode(db, HistoryModeLight))
	require.NoError(t, CheckHistoryMode(db, HistoryModeLight))
	require.ErrorContains(t, CheckHistoryMode(db, HistoryModeHistory), "the database is of a light node")
	require.ErrorContains(t, CheckHistoryMode(db, ""), "the database is of a light node")

	// A database made before the modes, with blocks, is a full one.
	env := newBALTestEnv(t, "prague", 1)
	env.generate(2, func(int, *blockchain.BlockGen) {})
	require.ErrorContains(t, CheckHistoryMode(env.db, HistoryModeHistory), "the database is of a full node")
	require.NoError(t, CheckHistoryMode(env.db, HistoryModeFull))
}

// TestPruneHistory deletes the blocks of a light node older than its
// retention: their data goes, their canonical hashes and lists range stay
// consistent, and the kept blocks still serve.
func TestPruneHistory(t *testing.T) {
	defer func(n uint64) { MinHistoryBlocks = n }(MinHistoryBlocks)
	MinHistoryBlocks = 4
	env := newBALTestEnv(t, "prague", 2)
	cacheConfig := regenCacheConfig(true)
	cacheConfig.BALNoIndex = true
	chain, blocks := env.generateWithCache(cacheConfig, 30, func(i int, b *blockchain.BlockGen) {
		b.AddTx(env.legacyTx(b, 0, &env.addrs[1], big.NewInt(params.KAIA), nil))
	})
	require.Nil(t, of(chain).archive, "a light node does not index its lists")
	head := blocks[len(blocks)-1].Header()
	// Keep the blocks at or after the time of block 21.
	of(chain).cfg.HistoryRetention = time.Duration(head.Time.Uint64()-blocks[20].Time().Uint64()) * time.Second
	require.NoError(t, PruneHistory(chain))

	require.Equal(t, uint64(21), HistoryFirst(chain))
	first, next := Range(chain)
	require.Equal(t, [2]uint64{21, 31}, [2]uint64{first, next})
	for _, block := range blocks {
		n, hash := block.NumberU64(), block.Hash()
		require.Equal(t, hash, of(chain).db.ReadCanonicalHash(n), "block %d", n)
		tx := block.Transactions()[0].Hash()
		if n < 21 {
			require.Nil(t, chain.GetHeaderByNumber(n), "block %d", n)
			require.Nil(t, chain.GetBlockByNumber(n), "block %d", n)
			require.Nil(t, chain.GetReceiptsByBlockHash(hash), "block %d", n)
			require.Nil(t, ByHash(chain, hash, n), "block %d", n)
			require.Nil(t, chain.GetTd(hash, n), "block %d", n)
			txHash, _, _ := of(chain).db.ReadTxLookupEntry(tx)
			require.Equal(t, common.Hash{}, txHash, "block %d", n)
			require.ErrorIs(t, PrunedError(chain, n), ErrPrunedHistory)
		} else {
			require.NotNil(t, chain.GetBlockByNumber(n), "block %d", n)
			require.NotNil(t, chain.GetReceiptsByBlockHash(hash), "block %d", n)
			require.NotNil(t, ByHash(chain, hash, n), "block %d", n)
			require.NoError(t, PrunedError(chain, n))
		}
	}
	require.NotNil(t, chain.GetHeaderByNumber(0), "the genesis block stays")
	require.NoError(t, PrunedError(chain, 0))

	// The root of the block hashes of an unfinished segment is not stored.
	require.ErrorContains(t, of(chain).writeHeaderSegmentRoot(0), "no canonical hash")
	_, ok := HeaderSegmentRoot(chain, 0)
	require.False(t, ok)

	// Nothing more goes until the head moves on.
	require.NoError(t, PruneHistory(chain))
	require.Equal(t, uint64(21), HistoryFirst(chain))
}

// TestFillHeaderSegmentRoots stores the root of the block hashes of every
// finished segment that has none, from the canonical hashes.
func TestFillHeaderSegmentRoots(t *testing.T) {
	db := database.NewMemoryDBManager()
	head := uint64(3*SegmentSize - 1)
	for n := uint64(0); n <= head; n++ {
		db.WriteCanonicalHash(common.BigToHash(new(big.Int).SetUint64(n+1)), n)
	}
	kept := common.Hash{0xaa}
	require.NoError(t, db.WriteHeaderSegmentRoot(1, kept))
	c := &chain{db: db, cfg: &blockchain.CacheConfig{HeaderSegmentRoots: true}}

	require.True(t, c.headerSegmentRootsFilled(SegmentSize-1), "no segment is finished")
	require.NoError(t, c.fillHeaderSegmentRoots(SegmentSize-1, nil))
	_, ok := db.ReadHeaderSegmentRoot(0)
	require.False(t, ok, "the head is not past segment 0")

	require.False(t, c.headerSegmentRootsFilled(head))
	require.NoError(t, c.fillHeaderSegmentRoots(head, nil))
	require.True(t, c.headerSegmentRootsFilled(head))
	hashes := make([]common.Hash, SegmentSize)
	for n := range hashes {
		hashes[n] = db.ReadCanonicalHash(uint64(n))
	}
	want, err := SegmentRoot(hashes)
	require.NoError(t, err)
	got, ok := db.ReadHeaderSegmentRoot(0)
	require.True(t, ok)
	require.Equal(t, want, got)
	got, _ = db.ReadHeaderSegmentRoot(1)
	require.Equal(t, kept, got, "a stored root is kept")
	_, ok = db.ReadHeaderSegmentRoot(2)
	require.False(t, ok, "the head is not past segment 2")

	// A missing canonical hash stops the fill.
	db.DeleteCanonicalHash(2*SegmentSize + 5)
	require.ErrorContains(t, c.fillHeaderSegmentRoots(head+1, nil), "no canonical hash")
	require.False(t, c.headerSegmentRootsFilled(head+1))

	full := &chain{db: db, cfg: &blockchain.CacheConfig{}}
	require.True(t, full.headerSegmentRootsFilled(head+1), "a full node does not fill")
}

// TestKeepsHeader keeps the headers the gov and valset modules read after a
// restart: votes, governance changes and Istanbul checkpoints.
func TestKeepsHeader(t *testing.T) {
	header := func(n uint64) *types.Header { return &types.Header{Number: new(big.Int).SetUint64(n)} }
	require.False(t, keepsHeader(header(1023)))
	require.True(t, keepsHeader(header(1024)))
	h := header(5)
	h.Vote = []byte{0x01}
	require.True(t, keepsHeader(h))
	h = header(5)
	h.Governance = []byte{0x01}
	require.True(t, keepsHeader(h))
}
