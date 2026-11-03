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

	"github.com/kaiachain/kaia/blockchain/checkpoint"
	"github.com/kaiachain/kaia/common"
	storecp "github.com/kaiachain/kaia/storage/database/checkpoint"
	"github.com/stretchr/testify/require"
)

// chainEntries returns what a lookup through the chain of a segment sees: the
// newest value of every trie node, without tombstones, and the code entries,
// which are never removed.
func chainEntries(t *testing.T, store *storecp.Store, id uint64) (nodes, codes map[string]string) {
	it, err := store.NewIterator(id)
	require.NoError(t, err)
	nodes, codes = make(map[string]string), make(map[string]string)
	for it.Next() {
		switch {
		case len(it.Value()) == 0:
		case it.Key()[0] == 0x02:
			codes[string(it.Key())] = string(it.Value())
		default:
			nodes[string(it.Key())] = string(it.Value())
		}
	}
	require.NoError(t, it.Error())
	return nodes, codes
}

// TestBALCheckpoint exports checkpoints as a full segment and increments, checks
// that every chain holds exactly the trie of its block, and rebuilds later
// states from them with the block access lists.
func TestBALCheckpoint(t *testing.T) {
	t.Run("shanghai", func(t *testing.T) { testBALCheckpoint(t, "shanghai") })
	t.Run("prague", func(t *testing.T) { testBALCheckpoint(t, "prague") })
}

func testBALCheckpoint(t *testing.T, hardfork string) {
	c := newBALHistoryChain(t, hardfork)
	nodes := checkpoint.NodeReader(c.chain.StateCache().TrieDB().Node)
	code := checkpoint.CodeReader(c.chain.StateCache().ContractCode)
	headers := map[uint64]common.Hash{0: c.env.genesis.Root()}
	for _, b := range c.blocks {
		headers[b.NumberU64()] = b.Root()
	}
	lists := listSlice(c.lists)
	check := func(n uint64) (common.Hash, error) { return headers[n], nil }

	store, err := storecp.Open(t.TempDir(), nil)
	require.NoError(t, err)
	defer store.Close()
	full, err := storecp.Open(t.TempDir(), nil)
	require.NoError(t, err)
	defer full.Close()

	// Checkpoints at blocks 2, 4, 6 and 9: the child is recreated in 4 and
	// deleted in 6 (shanghai), and recreated again in 8.
	var prev uint64
	for i, number := range []uint64{2, 4, 6, 9} {
		id := uint64(i + 1)
		meta := storecp.Meta{ID: id, Base: prev, BlockNumber: number, StateRoot: headers[number], Trigger: storecp.TriggerInterval}
		_, stats, err := checkpoint.Export(store, meta, nodes, code)
		require.NoError(t, err, "block %d", number)
		require.NoError(t, store.Verify(id))

		_, fullStats, err := checkpoint.Export(full, storecp.Meta{ID: id, BlockNumber: number, StateRoot: headers[number], Trigger: storecp.TriggerInterval}, nodes, code)
		require.NoError(t, err)
		fullNodes, fullCodes := chainEntries(t, full, id)
		nodes, codes := chainEntries(t, store, id)
		require.Equal(t, fullNodes, nodes, "block %d: the chain differs from a full export", number)
		for k, v := range fullCodes {
			require.Equal(t, v, codes[k], "block %d: the chain misses code %x", number, k)
		}
		if prev != 0 {
			require.Less(t, stats.Nodes, fullStats.Nodes, "block %d: the increment is not smaller", number)
		}
		t.Logf("block %d: increment %d nodes %d tombstones %d codes, full %d nodes %d codes", number, stats.Nodes, stats.Tombstones, stats.Codes, fullStats.Nodes, fullStats.Codes)
		prev = id
	}

	// Every block after a checkpoint is rebuilt with the right root.
	for id := uint64(1); id <= 4; id++ {
		meta, _ := store.Meta(id)
		sdb, _, err := checkpoint.Regen(store, id, lists, uint64(len(c.blocks)), checkpoint.Keys{c.contract: nil}, check)
		require.NoError(t, err, "from block %d", meta.BlockNumber)

		want, err := c.chain.StateAt(c.blocks[len(c.blocks)-1].Root())
		require.NoError(t, err)
		balance := sdb.GetBalance(c.env.addrs[1])
		require.NoError(t, sdb.Error())
		require.Equal(t, want.GetBalance(c.env.addrs[1]), balance)
		require.Equal(t, want.GetState(c.contract, common.BytesToHash([]byte{5})), sdb.GetState(c.contract, common.BytesToHash([]byte{5})))
	}

	// A rebuilt state at a checkpoint proves its keys.
	sdb, _, err := checkpoint.Rebuild(store, 2, checkpoint.Keys{c.contract: {common.BytesToHash([]byte{5}): true}})
	require.NoError(t, err)
	want, err := c.chain.StateAt(headers[4])
	require.NoError(t, err)
	require.Equal(t, want.GetState(c.contract, common.BytesToHash([]byte{5})), sdb.GetState(c.contract, common.BytesToHash([]byte{5})))
}
