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
	"math/big"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
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
	chain, blocks := env.generateWithCache(regenCacheConfig(""), n, func(i int, b *BlockGen) {
		b.SetRewardbase(common.Address{0xbe, byte(i % 4)})
		b.AddTx(env.legacyTx(b, 0, &balStoreAddr, common.Big0, balSlotData(byte(i%5), byte(i+1))))
		b.AddTx(env.legacyTx(b, 1, &env.addrs[2], common.Big1, nil))
		if i%3 == 0 {
			b.AddTx(env.legacyTx(b, 2, nil, common.Big0, balInitCode(balStoreCode())))
		}
	})
	lists := checkBAL(t, chain, blocks)
	chain.Stop()
	return &regenFixture{env: env, blocks: blocks, lists: lists}
}

func regenCacheConfig(balDir string) *CacheConfig {
	return &CacheConfig{
		ArchiveMode:         true,
		CacheSize:           512,
		BlockInterval:       DefaultBlockInterval,
		TriesInMemory:       DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
		BALRecordDir:        balDir,
	}
}

// open opens the chain again, recording lists into a new freezer.
func (f *regenFixture) open(t *testing.T) *BlockChain {
	chain, err := NewBlockChain(f.env.db, regenCacheConfig(t.TempDir()), f.env.config, f.env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(chain.Stop)
	chain.Processor().RegisterBlockStateModule(f.env.sealer)
	return chain
}

func waitRegen(t *testing.T, chain *BlockChain) BALRegenStatus {
	for i := 0; i < 1000; i++ {
		if s := chain.BALRegenStatus(); !s.Running {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("regeneration did not finish")
	return BALRegenStatus{}
}

// requireLists checks the freezer of the chain against the lists of blocks
// from to to.
func (f *regenFixture) requireLists(t *testing.T, chain *BlockChain, from, to uint64) {
	for n := from; n <= to; n++ {
		got, err := chain.bal.freezer.ReadRaw(n)
		require.NoError(t, err, "block %d", n)
		want, err := f.lists[n-1].Encode()
		require.NoError(t, err)
		require.Equal(t, want, got, "block %d", n)
	}
}

// TestBALRegen regenerates the lists of every block from the stored states,
// with one worker and with several, and verifies them again.
func TestBALRegen(t *testing.T) {
	f := newRegenFixture(t, "prague", 300)
	head := uint64(len(f.blocks))
	for _, workers := range []int{1, 4} {
		chain := f.open(t)
		require.Equal(t, uint64(1), chain.bal.freezer.Next())
		require.NoError(t, chain.StartBALRegen(BALRegenConfig{Workers: workers}))
		s := waitRegen(t, chain)
		require.Empty(t, s.Error)
		require.Equal(t, head+1, s.Next)
		require.Equal(t, 3, s.Intervals) // 1-128, 129-256, 257-300
		f.requireLists(t, chain, 1, head)

		require.NoError(t, chain.StartBALRegen(BALRegenConfig{Mode: BALRegenVerify, From: 100, Workers: workers}))
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
	chain.db.DeleteTrieNode(chain.GetHeaderByNumber(128).Root.ExtendZero())

	err := chain.StartBALRegen(BALRegenConfig{Workers: 2})
	require.ErrorContains(t, err, "no stored state before block 1")

	// The freezer starts after the first stored state.
	require.NoError(t, chain.StartBALRegen(BALRegenConfig{From: 51, Workers: 2}))
	s := waitRegen(t, chain)
	require.Empty(t, s.Error)
	require.Equal(t, uint64(51), chain.bal.freezer.First())
	f.requireLists(t, chain, 51, 200)
	_, err = chain.bal.freezer.ReadRaw(50)
	require.ErrorIs(t, err, bal.ErrBeforeFirst)
}

// TestBALRegenHeadBlocks inserts blocks while a regeneration fills in the
// earlier ones: their lists wait and follow.
func TestBALRegenHeadBlocks(t *testing.T) {
	f := newRegenFixture(t, "prague", 260)
	// Drop the last 10 blocks, to insert them again during the regeneration.
	chain := f.open(t)
	require.NoError(t, chain.SetHead(250))
	require.NoError(t, chain.bal.hold())
	_, err := chain.InsertChain(f.blocks[250:])
	require.NoError(t, err)
	require.Equal(t, uint64(1), chain.bal.freezer.Next(), "inserted blocks wait")

	require.NoError(t, chain.StartBALRegen(BALRegenConfig{To: 250, Workers: 3}))
	s := waitRegen(t, chain)
	require.Empty(t, s.Error)
	require.Equal(t, uint64(261), chain.bal.freezer.Next())
	f.requireLists(t, chain, 1, 260)

	// Blocks after it are appended directly again.
	chain.bal.mu.Lock()
	holding := chain.bal.holding
	chain.bal.mu.Unlock()
	require.False(t, holding)
}

// TestStartHistoryConversion converts a node whose states start at block 80:
// the lists of the blocks after it are regenerated, and nothing happens once
// they are there.
func TestStartHistoryConversion(t *testing.T) {
	f := newRegenFixture(t, "prague", 200)
	chain := f.open(t)
	dropStatesBefore(chain, 80)

	require.NoError(t, chain.StartHistoryConversion(1<<30, 2))
	s := waitRegen(t, chain)
	require.Empty(t, s.Error)
	require.Equal(t, uint64(81), chain.bal.freezer.First())
	require.Equal(t, uint64(201), chain.bal.freezer.Next())
	f.requireLists(t, chain, 81, 200)

	require.NoError(t, chain.StartHistoryConversion(1<<30, 2))
	again := chain.BALRegenStatus()
	require.Equal(t, [3]uint64{s.From, s.To, s.Next}, [3]uint64{again.From, again.To, again.Next}, "no new regeneration")
}

// writeFreezer writes the lists of the fixture to a new freezer, the one of
// block tamper replaced by the one of the block before it.
func (f *regenFixture) writeFreezer(t *testing.T, tamper uint64) string {
	dir := t.TempDir()
	fr, err := bal.OpenFreezer(dir, false)
	require.NoError(t, err)
	defer fr.Close()
	for i, list := range f.lists {
		n := uint64(i + 1)
		if n == tamper {
			list = f.lists[i-1]
		}
		enc, err := list.Encode()
		require.NoError(t, err)
		require.NoError(t, fr.Append(n, enc))
	}
	return dir
}

// TestBALRegenFromGenesis fills in the lists before the BAL history of a node
// whose oldest states are gone, by executing from genesis and by importing,
// across the Amber hardfork, and checks that both give the lists of an
// in-order execution, which match the hashes of the Amber headers.
func TestBALRegenFromGenesis(t *testing.T) {
	env := newBALTestEnv(t, "prague", 3)
	env.config.AmberCompatibleBlock = big.NewInt(60)
	f := newRegenFixtureFor(t, env, 200)
	for _, block := range f.blocks[59:] {
		require.NotNil(t, block.Header().BlockAccessListHash, "block %d", block.NumberU64())
	}
	defer func(old uint64) { balRegenCommitInterval = old }(balRegenCommitInterval)
	balRegenCommitInterval = 32

	source := f.writeFreezer(t, 0)
	for _, cfg := range []BALRegenConfig{
		{Source: BALRegenGenesis, Genesis: env.gspec},
		{Source: BALRegenImport, ImportDir: source, Genesis: env.gspec},
	} {
		chain := f.open(t)
		dropStatesBefore(chain, 80)
		require.NoError(t, chain.StartHistoryConversion(1<<30, 2))
		require.Empty(t, waitRegen(t, chain).Error)
		require.Equal(t, uint64(81), chain.bal.freezer.First())

		_, err := chain.StateAt(chain.GetHeaderByNumber(10).Root)
		err = chain.HistoryError(10, err)
		require.ErrorIs(t, err, ErrNoHistory)
		require.ErrorContains(t, err, "before block 81")

		// Blocks 1-80 go in front of the freezer.
		require.NoError(t, chain.StartBALRegen(cfg), cfg.Source)
		s := waitRegen(t, chain)
		require.Empty(t, s.Error, cfg.Source)
		require.Equal(t, uint64(81), s.Next, cfg.Source)
		require.Equal(t, uint64(0), chain.bal.freezer.First(), cfg.Source)
		require.Equal(t, uint64(201), chain.bal.freezer.Next(), cfg.Source)
		f.requireLists(t, chain, 1, 200)
		first, _ := chain.BALHistoryFirst()
		require.Equal(t, uint64(1), first)

		// And they verify from genesis.
		require.NoError(t, chain.StartBALRegen(BALRegenConfig{Source: BALRegenGenesis, Genesis: env.gspec, Mode: BALRegenVerify}))
		s = waitRegen(t, chain)
		require.Empty(t, s.Error)
		require.Zero(t, s.Mismatches)
		require.Equal(t, uint64(201), s.Next)
	}
}

// TestBALRegenImportTampered rejects an imported list that does not give the
// state root of its block.
func TestBALRegenImportTampered(t *testing.T) {
	f := newRegenFixture(t, "prague", 60)
	chain := f.open(t)
	require.NoError(t, chain.StartBALRegen(BALRegenConfig{Source: BALRegenImport, ImportDir: f.writeFreezer(t, 40), Genesis: f.env.gspec}))
	s := waitRegen(t, chain)
	require.Contains(t, s.Error, "block 40: state root")
	require.Equal(t, uint64(40), chain.bal.freezer.Next(), "the lists before it are kept")
}
