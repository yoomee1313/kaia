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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus"
	"github.com/kaiachain/kaia/datasync/fetcher"
	"github.com/kaiachain/kaia/rlp"
	"github.com/stretchr/testify/require"
)

// countingChain counts how blocks are inserted.
type countingChain struct {
	*blockchain.BlockChain
	executed, applied atomic.Int64
}

func (c *countingChain) InsertChain(blocks types.Blocks) (int, error) {
	c.executed.Add(int64(len(blocks)))
	return c.BlockChain.InsertChain(blocks)
}

func (c *countingChain) InsertChainWithBALs(blocks []*Block) (int, error) {
	c.applied.Add(int64(len(blocks)))
	return InsertChain(c.BlockChain, blocks)
}

// TestBALTip has an EN follow the head through the fetcher: a block before
// Amber, whose header does not commit its BAL, is executed; from Amber a block
// is inserted at once with the receipts and BAL propagated after it, or
// fetched from a peer, and a block whose BAL cannot be had is executed.
func TestBALTip(t *testing.T) {
	defer func(d time.Duration) { tipBALWait = d }(tipBALWait)
	tipBALWait = 300 * time.Millisecond
	src, srcDB, dst, blocks, _ := newRangeChains(t, 12)
	chain := &countingChain{BlockChain: dst}
	const amber = 4

	server := NewSync(srcDB, src, nil)
	quit := make(chan struct{})
	defer close(quit)
	client := NewSync(dst.StateCache().TrieDB().DiskDB(), dst, func(string) {})
	client.quit = quit
	var announced sync.Map
	client.StartTip(chain, func(b *types.Block) { announced.Store(b.NumberU64(), true) }, func(h *types.Header) bool { return h.Number.Uint64() >= amber })
	peer := connect(client, server, "pn", nil)
	waitRanges(t, client, 1)

	// The fetcher wired as for a node applying BALs.
	verify := dst.Validator().ValidateHeader
	f := fetcher.New(
		func(hash common.Hash) *types.Block {
			if b := dst.GetBlockByHash(hash); b != nil {
				return b
			}
			return client.TipBlock(hash)
		},
		func(h *types.Header) error { return client.VerifyTipHeader(h, verify) },
		func(*types.Block) {}, func(*types.Block) {},
		client.TipHeight,
		client.InsertTipBlocks,
		func(id string) { t.Errorf("peer %s dropped", id) },
	)
	f.Start()
	defer f.Stop()

	propagate := func(b *types.Block) {
		m := &newBlockBAL{Hash: b.Hash(), Number: b.NumberU64(), List: srcDB.ReadBAL(b.Hash(), b.NumberU64()), Receipts: slimReceipts(srcDB.ReadReceipts(b.Hash(), b.NumberU64()))}
		client.receive(peer, m)
	}
	waitHead := func(n uint64) {
		t.Helper()
		require.Eventually(t, func() bool { return dst.CurrentBlock().NumberU64() == n }, 3*time.Second, 5*time.Millisecond, "head %d", dst.CurrentBlock().NumberU64())
	}

	// Blocks before Amber are executed even with their BALs.
	for _, b := range blocks[:amber-1] {
		propagate(b)
		require.NoError(t, f.Enqueue("pn", b))
		waitHead(b.NumberU64())
	}
	require.Equal(t, int64(amber-1), chain.executed.Load())
	require.Zero(t, chain.applied.Load())

	// From Amber a block is applied without waiting for its child; block 5's
	// BAL is fetched from the peer.
	for _, b := range blocks[amber-1 : 10] {
		if b.NumberU64() != 5 {
			propagate(b)
		}
		require.NoError(t, f.Enqueue("pn", b))
		waitHead(b.NumberU64())
	}
	require.Equal(t, int64(amber-1), chain.executed.Load(), "no block from Amber executed")
	require.Equal(t, int64(10-amber+1), chain.applied.Load())
	require.NotZero(t, peer.requestCount(), "the BAL of block 5 was fetched")
	_, ok := announced.Load(uint64(10))
	require.True(t, ok, "an inserted block is announced")

	// Without a BAL anywhere, the block waits, its child is verified at its
	// insertion, and it is executed after a while.
	client.RemovePeer("pn")
	require.NoError(t, f.Enqueue("pn", blocks[10]))
	require.Eventually(t, func() bool { return client.TipBlock(blocks[10].Hash()) != nil }, time.Second, 5*time.Millisecond)
	require.Equal(t, consensus.ErrFutureBlock, client.VerifyTipHeader(blocks[11].Header(), verify))
	waitHead(11)
	require.Equal(t, int64(amber), chain.executed.Load(), "block 11 had no BAL")
	propagate(blocks[11])
	require.NoError(t, f.Enqueue("pn", blocks[11]))
	waitHead(12)
	require.Equal(t, int64(10-amber+2), chain.applied.Load())

	for _, b := range blocks {
		hash, n := b.Hash(), b.NumberU64()
		require.Equal(t, hash, dst.GetHeaderByNumber(n).Hash())
		want, err := rlp.EncodeToBytes(storageReceipts(src.GetReceiptsByBlockHash(hash)))
		require.NoError(t, err)
		got, err := rlp.EncodeToBytes(storageReceipts(dst.GetReceiptsByBlockHash(hash)))
		require.NoError(t, err)
		require.Equal(t, want, got, "block %d", n)
	}
}

// TestBALTipNoTrust starts no tip without a header that commits a BAL, so the
// fetcher executes the new blocks as usual.
func TestBALTipNoTrust(t *testing.T) {
	_, _, dst, _, _ := newRangeChains(t, 1)
	client := NewSync(dst.StateCache().TrieDB().DiskDB(), dst, nil)
	client.StartTip(&countingChain{BlockChain: dst}, nil, nil)
	require.Nil(t, client.tip)
}
