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
	"time"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/checkpoint"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	storecp "github.com/kaiachain/kaia/storage/database/checkpoint"
	"github.com/kaiachain/kaia/storage/statedb"
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
	check := func(n uint64) (*types.Header, error) { return c.blocks[n-1].Header(), nil }

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
		_, stats, err := checkpoint.Export(store, meta, nodes, code, nil)
		require.NoError(t, err, "block %d", number)
		require.NoError(t, store.Verify(id))

		_, fullStats, err := checkpoint.Export(full, storecp.Meta{ID: id, BlockNumber: number, StateRoot: headers[number], Trigger: storecp.TriggerInterval}, nodes, code, nil)
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
		// Regen only resolves the keys of the lists and of extra.
		extra := checkpoint.Keys{}
		extra.Add(c.env.addrs[1])
		extra.Add(c.contract, common.BytesToHash([]byte{5}))
		sdb, _, err := checkpoint.Regen(store, id, lists, uint64(len(c.blocks)), extra, check)
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

// TestBlockChainCheckpoint inserts blocks into a chain that writes a state
// checkpoint every third block, then rebuilds the head state from the first
// checkpoint.
func TestBlockChainCheckpoint(t *testing.T) {
	c := newBALHistoryChain(t, "shanghai")
	dir := t.TempDir()
	db := database.NewMemoryDBManager()
	c.env.gspec.MustCommit(db)
	chain, err := NewBlockChain(db, &CacheConfig{
		CacheSize:               512,
		BlockInterval:           DefaultBlockInterval,
		TriesInMemory:           DefaultTriesInMemory,
		TrieNodeCacheConfig:     statedb.GetEmptyTrieNodeCacheConfig(),
		CheckpointDir:           dir,
		CheckpointBlockInterval: 3,
	}, c.env.config, c.env.sealer, vm.Config{})
	require.NoError(t, err)
	chain.Processor().RegisterBlockStateModule(c.env.sealer)
	for _, block := range c.blocks {
		_, err := chain.InsertChain([]*types.Block{block})
		require.NoError(t, err)
		for chain.checkpoints.busy.Load() {
			time.Sleep(time.Millisecond)
		}
	}
	chain.Stop()

	store, err := storecp.Open(dir, nil)
	require.NoError(t, err)
	defer store.Close()
	segments := store.Segments()
	require.Len(t, segments, 3)
	for i, meta := range segments {
		require.Equal(t, uint64(3*(i+1)), meta.BlockNumber)
		require.Equal(t, c.blocks[meta.BlockNumber-1].Root(), meta.StateRoot)
		require.Equal(t, uint64(i), meta.Base)
		require.Equal(t, storecp.TriggerInterval, meta.Trigger)
	}
	check := func(n uint64) (*types.Header, error) { return c.blocks[n-1].Header(), nil }
	_, _, err = checkpoint.Regen(store, segments[0].ID, listSlice(c.lists), uint64(len(c.blocks)), nil, check)
	require.NoError(t, err)
}

// TestBALApplyUnchecked applies the lists of several blocks in a row, hashing
// once per batch, and checks the root at the end of every batch.
func TestBALApplyUnchecked(t *testing.T) {
	for _, hardfork := range []string{"shanghai", "prague"} {
		c := newBALHistoryChain(t, hardfork)
		for _, size := range []int{1, 2, 3, 4, len(c.lists)} {
			root := c.env.genesis.Root()
			for start := 0; start < len(c.lists); start += size {
				end := min(start+size, len(c.lists))
				sdb, err := c.chain.StateAt(root)
				require.NoError(t, err)
				for _, list := range c.lists[start:end] {
					require.NoError(t, sdb.ApplyBlockAccessListUnchecked(list))
				}
				sdb.HashStorages(4)
				require.Equal(t, c.blocks[end-1].Root(), sdb.IntermediateRoot(true), "%s: batch %d of blocks %d-%d", hardfork, size, start+1, end)
				root, err = sdb.Commit(true)
				require.NoError(t, err)
				require.NoError(t, sdb.Error())
			}
		}
	}
}

// TestRegenListHash checks that Regen rejects a list whose writes differ from
// the hash in the header before applying it, and accepts a list whose reads
// differ or are left out, since the hash covers the writes only.
func TestRegenListHash(t *testing.T) {
	env := newAmberTestEnv(t)
	chain, blocks := env.generate(4, amberTestTxs(env))
	lists := checkBAL(t, chain, blocks)
	require.Nil(t, blocks[0].Header().BlockAccessListHash)
	require.NotNil(t, blocks[3].Header().BlockAccessListHash)

	store, err := storecp.Open(t.TempDir(), nil)
	require.NoError(t, err)
	defer store.Close()
	_, _, err = checkpoint.Export(store, storecp.Meta{ID: 1, BlockNumber: 0, StateRoot: env.genesis.Root(), Trigger: storecp.TriggerInterval},
		chain.StateCache().TrieDB().Node, chain.StateCache().ContractCode, nil)
	require.NoError(t, err)
	headers := func(n uint64) (*types.Header, error) { return blocks[n-1].Header(), nil }
	regen := func(lists []*bal.BlockAccessList) error {
		_, _, err := checkpoint.Regen(store, 1, listSlice(lists), 4, nil, headers)
		return err
	}
	require.NoError(t, regen(lists))

	// tamper returns the lists with list i copied and changed by f.
	tamper := func(i int, f func(*bal.AccountAccess)) []*bal.BlockAccessList {
		out := make([]*bal.BlockAccessList, len(lists))
		copy(out, lists)
		enc, err := lists[i].Encode()
		require.NoError(t, err)
		out[i], err = bal.Decode(enc)
		require.NoError(t, err)
		for j := range out[i].Accounts {
			if a := &out[i].Accounts[j]; a.Address == balStoreAddr {
				f(a)
			}
		}
		require.NoError(t, out[i].Validate())
		return out
	}
	extraRead := func(a *bal.AccountAccess) {
		a.StorageReads = append(a.StorageReads, common.HexToHash("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"))
	}
	otherValue := func(a *bal.AccountAccess) {
		w := &a.StorageWrites[0]
		w.Changes[len(w.Changes)-1].Value[0] ^= 1
	}

	// Reads are not covered by the hash: a list with another read, or with
	// the writes only, passes.
	require.NoError(t, regen(tamper(2, extraRead)))
	writesOnly := make([]*bal.BlockAccessList, len(lists))
	for i, l := range lists {
		writesOnly[i] = l.Writes()
	}
	require.NoError(t, regen(writesOnly))

	// A different write is rejected by the hash before it is applied.
	err = regen(tamper(2, otherValue))
	require.ErrorIs(t, err, bal.ErrHashMismatch)
	require.ErrorContains(t, err, "block 3")

	// Before the Amber hardfork only the state root catches it, after applying.
	err = regen(tamper(0, otherValue))
	require.NotErrorIs(t, err, bal.ErrHashMismatch)
	require.ErrorContains(t, err, "block 1: state root")
}
