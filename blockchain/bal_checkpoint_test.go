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
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
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

// TestRevert sets the head state of a chain back to every earlier block with
// the history and checks the root, through the deletion and recreation of a
// contract (shanghai) and EIP-7702 delegation (prague).
func TestRevert(t *testing.T) {
	for _, hardfork := range []string{"shanghai", "prague"} {
		c := newBALHistoryChain(t, hardfork)
		db := database.NewMemDB()
		builder, err := bal.NewIndexBuilder(db, 0, c.env.genesis.Root())
		require.NoError(t, err)
		for i, list := range c.lists {
			require.NoError(t, builder.Add(uint64(i+1), list))
		}
		require.NoError(t, builder.Flush())
		index, err := bal.OpenIndex(db)
		require.NoError(t, err)
		base, err := c.chain.StateAt(c.env.genesis.Root())
		require.NoError(t, err)
		history := bal.NewHistory(index, listSlice(c.lists), state.NewBALBase(base))
		headers := func(n uint64) (*types.Header, error) {
			if n == 0 {
				return c.env.genesis.Header(), nil
			}
			return c.blocks[n-1].Header(), nil
		}

		head := uint64(len(c.blocks))
		for target := uint64(0); target <= head; target++ {
			sdb, err := c.chain.StateAt(c.blocks[head-1].Root())
			require.NoError(t, err)
			stats, err := checkpoint.Revert(sdb, history, listSlice(c.lists), target, head, headers)
			require.NoError(t, err, "%s: block %d", hardfork, target)
			if hardfork == "shanghai" && target == 3 {
				// The child, recreated in 4, deleted in 6 and recreated in 8.
				require.NotZero(t, stats.Rebuilt)
			}
		}
	}
}

// TestRevertGenesisStorage reverts through a contract that has storage from
// genesis, never written afterwards, and is destroyed and recreated twice
// (shanghai, before EIP-6780). Rebuilding it before the first destruction needs
// the genesis slots, which only the base state has, and rebuilding it later
// must leave them out.
func TestRevertGenesisStorage(t *testing.T) {
	initCode := balInitCode(balDestructibleCode())
	child := crypto.CreateAddress2(balFactory, [32]byte{}, crypto.Keccak256(initCode))
	slot := func(n byte) common.Hash { return common.BytesToHash([]byte{n}) }
	env := newBALTestEnvWithAlloc(t, "shanghai", 2, GenesisAlloc{
		child: {Code: balDestructibleCode(), Balance: common.Big0, Storage: map[common.Hash]common.Hash{
			slot(9): common.HexToHash("0x99"), slot(1): common.HexToHash("0x01"),
		}},
	})
	chain, blocks := env.generate(7, func(i int, b *BlockGen) {
		switch i {
		case 0:
			b.AddTx(env.legacyTx(b, 1, &child, common.Big0, balSlotData(3, 0x33)))
		case 1, 3:
			b.AddTx(env.legacyTx(b, 1, &child, common.Big0, nil)) // destroy
		case 2, 4:
			b.AddTx(env.legacyTx(b, 1, &balFactory, common.Big0, initCode)) // recreate: slots 1 and 2
			b.AddTx(env.legacyTx(b, 1, &child, common.Big0, balSlotData(byte(4+i), 0x44)))
		}
		b.AddTx(env.legacyTx(b, 0, &env.addrs[1], common.Big1, nil))
	})
	lists := checkBAL(t, chain, blocks)

	// The genesis slots are in the state of block 1 and gone from block 2.
	s1, err := chain.StateAt(blocks[0].Root())
	require.NoError(t, err)
	require.Equal(t, common.HexToHash("0x99"), s1.GetState(child, slot(9)))
	s3, err := chain.StateAt(blocks[2].Root())
	require.NoError(t, err)
	require.Equal(t, common.Hash{}, s3.GetState(child, slot(9)))
	require.Equal(t, common.HexToHash("0x11"), s3.GetState(child, slot(1)))

	db := database.NewMemDB()
	builder, err := bal.NewIndexBuilder(db, 0, env.genesis.Root())
	require.NoError(t, err)
	for i, list := range lists {
		require.NoError(t, builder.Add(uint64(i+1), list))
	}
	require.NoError(t, builder.Flush())
	index, err := bal.OpenIndex(db)
	require.NoError(t, err)
	slots, err := index.SlotsBefore(child, uint64(len(blocks)))
	require.NoError(t, err)
	require.NotContains(t, slots, slot(9), "the genesis slot is never written")

	base, err := chain.StateAt(env.genesis.Root())
	require.NoError(t, err)
	history := bal.NewHistory(index, listSlice(lists), state.NewBALBase(base))
	headers := func(n uint64) (*types.Header, error) {
		if n == 0 {
			return env.genesis.Header(), nil
		}
		return blocks[n-1].Header(), nil
	}
	head := uint64(len(blocks))
	for target := uint64(0); target < head; target++ {
		sdb, err := chain.StateAt(blocks[head-1].Root())
		require.NoError(t, err)
		stats, err := checkpoint.Revert(sdb, history, listSlice(lists), target, head, headers)
		require.NoError(t, err, "block %d", target)
		// The child is destroyed in blocks 2 and 4 and recreated in 3 and 5.
		// Before 5 it is rebuilt where it exists at target, and removed where
		// it does not.
		switch target {
		case 2, 4:
			require.NotZero(t, stats.Removed, "block %d", target)
		case 0, 1, 3:
			require.NotZero(t, stats.Rebuilt, "block %d", target)
		}
		want, err := chain.StateAt(headers2Root(t, headers, target))
		require.NoError(t, err)
		for n := byte(0); n < 10; n++ {
			require.Equal(t, want.GetState(child, slot(n)), sdb.GetState(child, slot(n)), "block %d slot %d", target, n)
		}
	}
}

func headers2Root(t *testing.T, headers func(uint64) (*types.Header, error), n uint64) common.Hash {
	h, err := headers(n)
	require.NoError(t, err)
	return h.Root
}
