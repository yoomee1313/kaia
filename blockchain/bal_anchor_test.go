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
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

// newArchiveBALChain inserts n blocks into an archive chain, which stores the
// state of every block.
func newArchiveBALChain(t *testing.T, n int) (*balTestEnv, *BlockChain) {
	env := newBALTestEnv(t, "prague", 2)
	chain, _ := env.generateWithCache(&CacheConfig{
		ArchiveMode:         true,
		CacheSize:           512,
		BlockInterval:       DefaultBlockInterval,
		TriesInMemory:       DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
	}, n, func(i int, b *BlockGen) {
		b.AddTx(env.legacyTx(b, 0, &balStoreAddr, common.Big0, balSlotData(byte(i), byte(i+1))))
		b.AddTx(env.legacyTx(b, 1, &env.addrs[0], common.Big1, nil))
	})
	return env, chain
}

// dropStatesBefore deletes the root nodes of the states of the blocks below
// number, as a state migration or live pruning drops the oldest states.
func dropStatesBefore(chain *BlockChain, number uint64) {
	for n := uint64(0); n < number; n++ {
		chain.db.DeleteTrieNode(chain.GetHeaderByNumber(n).Root.ExtendZero())
	}
}

func TestFindStateAnchors(t *testing.T) {
	_, chain := newArchiveBALChain(t, 40)
	for _, first := range []uint64{0, 1, 13, 39, 40} {
		dropStatesBefore(chain, first)
		anchors, err := chain.FindStateAnchors(0, 40, 1, 10)
		require.NoError(t, err, "first %d", first)
		require.Equal(t, first, anchors.First)
		require.Empty(t, anchors.Missing)
	}

	// At multiples of 8 the first one with a state is 40.
	anchors, err := chain.FindStateAnchors(0, 40, 8, 10)
	require.NoError(t, err)
	require.Equal(t, uint64(40), anchors.First)

	_, err = chain.FindStateAnchors(0, 39, 8, 10)
	require.ErrorIs(t, err, errNoStateAnchor)
}

func TestFindStateAnchorsGap(t *testing.T) {
	_, chain := newArchiveBALChain(t, 40)
	dropStatesBefore(chain, 8)
	chain.db.DeleteTrieNode(chain.GetHeaderByNumber(24).Root.ExtendZero())

	anchors, err := chain.FindStateAnchors(0, 40, 8, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(8), anchors.First)
	require.Equal(t, []uint64{24}, anchors.Missing)

	n, err := chain.stateAnchorAtOrBefore(30, 8, 8)
	require.NoError(t, err)
	require.Equal(t, uint64(16), n, "24 has no state, so 16")
	n, err = chain.stateAnchorAtOrBefore(32, 8, 8)
	require.NoError(t, err)
	require.Equal(t, uint64(32), n)
}
