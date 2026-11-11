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
	"math/big"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/kaiax/bal"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

type regenFixture struct {
	env    *balTestEnv
	blocks []*types.Block
	lists  []*bal.BlockAccessList // as executed in order, item i is block i+1
}

// newRegenFixture makes a chain of n blocks in an archive database without
// recording their lists, and the lists of an in-order execution to compare.
func newRegenFixture(t *testing.T, hardfork string, n int) *regenFixture {
	return newRegenFixtureFor(t, newBALTestEnv(t, hardfork, 3), n)
}

func newRegenFixtureFor(t *testing.T, env *balTestEnv, n int) *regenFixture {
	chain, blocks := env.generateWithCache(regenCacheConfig(), nil, n, func(i int, b *blockchain.BlockGen) {
		b.SetRewardbase(common.Address{0xbe, byte(i % 4)})
		b.AddTx(env.legacyTx(b, 0, &balStoreAddr, common.Big0, balSlotData(byte(i%5), byte(i+1))))
		b.AddTx(env.legacyTx(b, 1, &env.addrs[2], common.Big1, nil))
		if i%3 == 0 {
			b.AddTx(env.legacyTx(b, 2, nil, common.Big0, balInitCode(balStoreCode())))
		}
	})
	lists := checkBAL(t, chain, blocks)
	StopChain(chain)
	return &regenFixture{env: env, blocks: blocks, lists: lists}
}

func regenCacheConfig() *blockchain.CacheConfig {
	return &blockchain.CacheConfig{
		ArchiveMode:         true,
		CacheSize:           512,
		BlockInterval:       blockchain.DefaultBlockInterval,
		TriesInMemory:       blockchain.DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
	}
}

// open opens the chain again without stored lists, recording them.
func (f *regenFixture) open(t *testing.T) *blockchain.BlockChain {
	for _, block := range f.blocks {
		DeleteBAL(f.env.db, block.Hash(), block.NumberU64())
		DeleteBALReads(f.env.db, block.Hash(), block.NumberU64())
	}
	require.NoError(t, WriteBALRange(f.env.db, 0, 0))
	chain, err := NewBlockChain(f.env.db, regenCacheConfig(), &Config{Record: true}, f.env.config, f.env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { StopChain(chain) })
	chain.Processor().RegisterBlockStateModule(f.env.sealer)
	return chain
}

// requireSpan checks the range of blocks whose lists are all stored.
func requireSpan(t *testing.T, chain *blockchain.BlockChain, first, next uint64) {
	t.Helper()
	f, n := of(chain).rec.span()
	require.Equal(t, [2]uint64{first, next}, [2]uint64{f, n})
	f, n = ReadBALRange(of(chain).db)
	require.Equal(t, [2]uint64{first, next}, [2]uint64{f, n}, "stored range")
}

func waitRegen(t *testing.T, chain *blockchain.BlockChain) RegenStatus {
	for i := 0; i < 1000; i++ {
		if s := GetRegenStatus(chain); !s.Running {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("regeneration did not finish")
	return RegenStatus{}
}

// requireLists checks the stored writes of the chain against the lists of
// blocks from to to.
func (f *regenFixture) requireLists(t *testing.T, chain *blockchain.BlockChain, from, to uint64) {
	for n := from; n <= to; n++ {
		got, err := ReadRaw(of(chain).db, n)
		require.NoError(t, err, "block %d", n)
		want, err := f.lists[n-1].Writes().Encode()
		require.NoError(t, err)
		require.Equal(t, want, got, "block %d", n)
	}
}

// requireFull checks the whole stored lists, with their reads, of blocks from
// to to.
func (f *regenFixture) requireFull(t *testing.T, chain *blockchain.BlockChain, from, to uint64) {
	for n := from; n <= to; n++ {
		got, err := ReadFull(of(chain).db, n)
		require.NoError(t, err, "block %d", n)
		requireSameList(t, f.lists[n-1], got)
	}
}

// requireNoReads checks that the reads of blocks from to to are not stored.
func (f *regenFixture) requireNoReads(t *testing.T, chain *blockchain.BlockChain, from, to uint64) {
	for n := from; n <= to; n++ {
		_, err := ReadFull(of(chain).db, n)
		require.ErrorIs(t, err, ErrReadsNotStored, "block %d", n)
	}
}

// TestBALRegen regenerates the lists of every block from the stored states,
// with one worker and with several, and verifies them again.
func TestBALRegen(t *testing.T) {
	f := newRegenFixture(t, "prague", 300)
	head := uint64(len(f.blocks))
	for _, workers := range []int{1, 4} {
		chain := f.open(t)
		requireSpan(t, chain, 0, 0)
		require.NoError(t, StartRegen(chain, RegenConfig{Workers: workers}))
		s := waitRegen(t, chain)
		require.Empty(t, s.Error)
		require.Equal(t, head+1, s.Next)
		require.Equal(t, 3, s.Intervals) // 1-128, 129-256, 257-300
		f.requireLists(t, chain, 1, head)
		f.requireFull(t, chain, 1, head)
		requireSpan(t, chain, 1, head+1)

		require.NoError(t, StartRegen(chain, RegenConfig{Mode: RegenVerify, From: 100, Workers: workers}))
		s = waitRegen(t, chain)
		require.Empty(t, s.Error)
		require.Zero(t, s.Mismatches)
		require.Equal(t, head+1, s.Next)
	}
}

// TestBALRegenLaterState regenerates on a chain whose oldest states are gone,
// and through a later gap in the stored states.
func TestBALRegenLaterState(t *testing.T) {
	f := newRegenFixture(t, "shanghai", 200)
	chain := f.open(t)
	dropStatesBefore(chain, 50)
	of(chain).db.DeleteTrieNode(chain.GetHeaderByNumber(128).Root.ExtendZero())

	err := StartRegen(chain, RegenConfig{Workers: 2})
	require.ErrorContains(t, err, "no stored state before block 1")

	// The lists start after the first stored state.
	require.NoError(t, StartRegen(chain, RegenConfig{From: 51, Workers: 2}))
	s := waitRegen(t, chain)
	require.Empty(t, s.Error)
	requireSpan(t, chain, 51, 201)
	f.requireLists(t, chain, 51, 200)
	_, err = ReadRaw(of(chain).db, 50)
	require.ErrorIs(t, err, ErrNotStored)

	// Lists that would not join the stored ones are refused.
	err = StartRegen(chain, RegenConfig{From: 10, To: 20})
	require.ErrorContains(t, err, "touch the stored lists of blocks 51-200")
}

// TestBALRegenHeadBlocks inserts blocks before a regeneration fills in the
// earlier ones: their lists join once the blocks in between are stored, and
// a rewind drops them.
func TestBALRegenHeadBlocks(t *testing.T) {
	f := newRegenFixture(t, "prague", 260)
	// Drop the last 10 blocks, to insert them again with a recorder.
	chain := f.open(t)
	require.NoError(t, chain.SetHead(250))
	_, err := chain.InsertChain(f.blocks[250:])
	require.NoError(t, err)
	requireSpan(t, chain, 251, 261)

	require.NoError(t, StartRegen(chain, RegenConfig{From: 1, Workers: 3}))
	s := waitRegen(t, chain)
	require.Empty(t, s.Error)
	require.Equal(t, uint64(251), s.Next)
	requireSpan(t, chain, 1, 261)
	f.requireLists(t, chain, 1, 260)

	require.NoError(t, chain.SetHead(255))
	requireSpan(t, chain, 1, 256)
	require.False(t, HasBAL(of(chain).db, f.blocks[257].Hash(), 258))
}

// TestStartHistoryConversion converts a node whose states start at block 80:
// the lists of the blocks after it are regenerated, and nothing happens once
// they are there.
func TestStartHistoryConversion(t *testing.T) {
	f := newRegenFixture(t, "prague", 200)
	chain := f.open(t)
	dropStatesBefore(chain, 80)

	require.NoError(t, StartHistoryConversion(chain, 1<<30, 2))
	s := waitRegen(t, chain)
	require.Empty(t, s.Error)
	requireSpan(t, chain, 81, 201)
	f.requireLists(t, chain, 81, 200)

	require.NoError(t, StartHistoryConversion(chain, 1<<30, 2))
	again := GetRegenStatus(chain)
	require.Equal(t, [3]uint64{s.From, s.To, s.Next}, [3]uint64{again.From, again.To, again.Next}, "no new regeneration")
}

// TestBALRegenFromGenesis fills in the lists before the BAL history of a node
// whose oldest states are gone by executing from genesis, and checks that it
// gives the lists of an in-order execution.
func TestBALRegenFromGenesis(t *testing.T) {
	env := newBALTestEnv(t, "prague", 3)
	f := newRegenFixtureFor(t, env, 200)
	defer func(old uint64) { regenCommitInterval = old }(regenCommitInterval)
	regenCommitInterval = 32

	for _, cfg := range []RegenConfig{
		{Source: RegenGenesis, Genesis: env.gspec},
	} {
		chain := f.open(t)
		dropStatesBefore(chain, 80)
		require.NoError(t, StartHistoryConversion(chain, 1<<30, 2))
		require.Empty(t, waitRegen(t, chain).Error)
		requireSpan(t, chain, 81, 201)

		_, err := chain.StateAt(chain.GetHeaderByNumber(10).Root)
		err = HistoryError(chain, 10, err)
		require.ErrorIs(t, err, ErrNoHistory)
		require.ErrorContains(t, err, "before block 81")

		// Blocks 1-80 join the stored lists.
		require.NoError(t, StartRegen(chain, cfg), cfg.Source)
		s := waitRegen(t, chain)
		require.Empty(t, s.Error, cfg.Source)
		require.Equal(t, uint64(81), s.Next, cfg.Source)
		requireSpan(t, chain, 1, 201)
		f.requireLists(t, chain, 1, 200)
		first, _ := RecordedFirst(chain)
		require.Equal(t, uint64(1), first)

		// And they verify from genesis.
		require.NoError(t, StartRegen(chain, RegenConfig{Source: RegenGenesis, Genesis: env.gspec, Mode: RegenVerify}))
		s = waitRegen(t, chain)
		require.Empty(t, s.Error)
		require.Zero(t, s.Mismatches)
		require.Equal(t, uint64(201), s.Next)
	}
}

// TestBALRegenReads deletes the reads of stored lists, which leaves the writes
// and the history as they are, and builds the reads again by executing the
// blocks, from the stored states and from genesis.
func TestBALRegenReads(t *testing.T) {
	f := newRegenFixture(t, "prague", 150)
	head := uint64(len(f.blocks))
	for _, cfg := range []RegenConfig{
		{Mode: RegenReads, Workers: 2},
		{Mode: RegenReads, Source: RegenGenesis, Genesis: f.env.gspec},
	} {
		chain := f.open(t)
		require.NoError(t, StartRegen(chain, RegenConfig{SkipReads: true, Workers: 2}))
		require.Empty(t, waitRegen(t, chain).Error)
		requireSpan(t, chain, 1, head+1)
		f.requireLists(t, chain, 1, head)
		f.requireNoReads(t, chain, 1, head)
		for n := uint64(1); n <= head; n++ {
			enc, err := ReadRaw(of(chain).db, n)
			require.NoError(t, err)
			hash, err := f.lists[n-1].Hash()
			require.NoError(t, err)
			require.Equal(t, hash, crypto.Keccak256Hash(enc), "the stored writes of block %d hash to its BAL hash", n)
		}

		require.NoError(t, StartRegen(chain, cfg), cfg.Source)
		s := waitRegen(t, chain)
		require.Empty(t, s.Error, cfg.Source)
		require.Zero(t, s.Mismatches, cfg.Source)
		require.Equal(t, head+1, s.Next, cfg.Source)
		requireSpan(t, chain, 1, head+1)
		f.requireFull(t, chain, 1, head)

		// Deleting them again changes nothing but the whole lists.
		for _, block := range f.blocks[:10] {
			DeleteBALReads(of(chain).db, block.Hash(), block.NumberU64())
		}
		f.requireNoReads(t, chain, 1, 10)
		f.requireLists(t, chain, 1, head)
		require.NoError(t, StartRegen(chain, RegenConfig{Mode: RegenVerify, Workers: 2}))
		s = waitRegen(t, chain)
		require.Empty(t, s.Error)
		require.Zero(t, s.Mismatches)
	}

	chain := f.open(t)
	err := StartRegen(chain, RegenConfig{Mode: RegenReads})
	require.ErrorContains(t, err, "reads mode needs stored lists")
}

// TestGenesisSpecKeepsDeriveSha rebuilds the genesis state of a Kairos chain
// without building the default Kairos genesis, which would reset DeriveSha
// for the blocks the node inserts after.
func TestGenesisSpecKeepsDeriveSha(t *testing.T) {
	config := params.TestChainConfig.Copy()
	config.ChainID = new(big.Int).Set(params.KairosChainConfig.ChainID)
	config.DeriveShaImpl = types.ImplDeriveShaOriginal
	db := database.NewMemoryDBManager()
	gspec := &blockchain.Genesis{Config: config}
	gspec.MustCommit(db)
	bc, err := NewBlockChain(db, nil, nil, config, faker.NewFaker(), vm.Config{})
	require.NoError(t, err)
	defer StopChain(bc)

	receipts := types.Receipts{{Status: types.ReceiptStatusSuccessful, GasUsed: 21000}}
	want := types.DeriveShaImpl(receipts, common.Big1)
	_, err = of(bc).genesisSpec(&RegenConfig{})
	require.Equal(t, want, types.DeriveShaImpl(receipts, common.Big1))
	require.Error(t, err, "no genesis given")

	SetGenesis(bc, gspec)
	got, err := of(bc).genesisSpec(&RegenConfig{})
	require.NoError(t, err)
	require.Equal(t, gspec, got)
	require.Equal(t, want, types.DeriveShaImpl(receipts, common.Big1))
}

// TestBALRegenAppliedNode runs the opt-in regenerations on a node that
// inserted its blocks by applying the BALs it received: verify mode checks the
// received writes by executing the blocks, and reads mode adds their reads.
func TestBALRegenAppliedNode(t *testing.T) {
	f := newRegenFixture(t, "prague", 60)
	src, err := NewBlockChain(f.env.db, regenCacheConfig(), nil, f.env.config, f.env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { StopChain(src) })
	db := database.NewMemoryDBManager()
	f.env.gspec.MustCommit(db)
	chain, err := NewBlockChain(db, regenCacheConfig(), &Config{Record: true}, f.env.config, f.env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { StopChain(chain) })
	chain.Processor().RegisterBlockStateModule(f.env.sealer)
	blocks := make([]*Block, len(f.blocks))
	for i, block := range f.blocks {
		enc, err := f.lists[i].Writes().Encode()
		require.NoError(t, err)
		blocks[i] = &Block{Block: block, Receipts: src.GetReceiptsByBlockHash(block.Hash()), BAL: enc}
	}
	_, err = InsertChain(chain, blocks)
	require.NoError(t, err)
	head := uint64(len(f.blocks))
	requireSpan(t, chain, 1, head+1)
	f.requireLists(t, chain, 1, head)
	f.requireNoReads(t, chain, 1, head)

	require.NoError(t, StartRegen(chain, RegenConfig{Mode: RegenVerify, Workers: 2}))
	s := waitRegen(t, chain)
	require.Empty(t, s.Error)
	require.Zero(t, s.Mismatches)
	require.Equal(t, head+1, s.Next)

	require.NoError(t, StartRegen(chain, RegenConfig{Mode: RegenReads, Workers: 2}))
	s = waitRegen(t, chain)
	require.Empty(t, s.Error)
	require.Zero(t, s.Mismatches)
	f.requireFull(t, chain, 1, head)
}
