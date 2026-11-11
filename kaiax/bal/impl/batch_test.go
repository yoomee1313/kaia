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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/kaiax"
	"github.com/kaiachain/kaia/kaiax/bal"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

const balBatchInterval = 16

// balBatchFixture is a chain of 60 blocks with every state, and a light chain
// at genesis to insert them into with their lists.
type balBatchFixture struct {
	*regenFixture
	src   *blockchain.BlockChain
	light *blockchain.BlockChain
}

func newBALBatchFixture(t *testing.T) *balBatchFixture {
	f := newRegenFixture(t, "prague", 60)
	src, err := NewBlockChain(f.env.db, regenCacheConfig(), nil, f.env.config, f.env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { StopChain(src) })
	db := database.NewMemoryDBManager()
	f.env.gspec.MustCommit(db)
	light, err := NewBlockChain(db, &blockchain.CacheConfig{
		CacheSize:           512,
		BlockInterval:       balBatchInterval,
		TriesInMemory:       blockchain.DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
	}, &Config{Record: true, NoIndex: true}, f.env.config, f.env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { StopChain(light) })
	return &balBatchFixture{regenFixture: f, src: src, light: light}
}

// balBlocks returns blocks from to to with their receipts and lists, the list
// of block tamper, if any, changed.
func (f *balBatchFixture) balBlocks(t *testing.T, from, to, tamper uint64) []*Block {
	var blocks []*Block
	for n := from; n <= to; n++ {
		block := f.blocks[n-1]
		enc, err := f.lists[n-1].Writes().Encode()
		require.NoError(t, err)
		if n == tamper {
			list, err := bal.Decode(enc)
			require.NoError(t, err)
			a := findAccess(t, list, balStoreAddr)
			a.StorageWrites = append(a.StorageWrites, bal.SlotWrites{
				Slot:    common.HexToHash("0xff"),
				Changes: []bal.SlotChange{{Index: a.StorageWrites[0].Changes[0].Index, Value: common.HexToHash("0x01")}},
			})
			enc, err = list.Encode()
			require.NoError(t, err)
		}
		blocks = append(blocks, &Block{Block: block, Receipts: f.src.GetReceiptsByBlockHash(block.Hash()), BAL: enc})
	}
	return blocks
}

// requireState checks the light state at block n against the full one.
func (f *balBatchFixture) requireState(t *testing.T, n uint64) {
	t.Helper()
	root := f.blocks[n-1].Root()
	want, err := f.src.StateAt(root)
	require.NoError(t, err)
	got, err := f.light.StateAt(root)
	require.NoError(t, err, "block %d", n)
	for _, addr := range append([]common.Address{balStoreAddr}, f.env.addrs...) {
		require.Equal(t, want.GetBalance(addr), got.GetBalance(addr), "block %d", n)
		require.Equal(t, want.GetNonce(addr), got.GetNonce(addr), "block %d", n)
	}
	for slot := range 5 {
		key := common.BytesToHash([]byte{byte(slot)})
		require.Equal(t, want.GetState(balStoreAddr, key), got.GetState(balStoreAddr, key), "block %d slot %d", n, slot)
	}
}

// TestBALBatchLight inserts a light chain in batches and checks that it gets
// the same head, receipts and state, with the state committed only at the
// commit interval and at the end of each batch.
func TestBALBatchLight(t *testing.T) {
	f := newBALBatchFixture(t)
	ends := []uint64{10, 40, 60}
	from := uint64(1)
	for _, to := range ends {
		_, err := InsertChain(f.light, f.balBlocks(t, from, to, 0))
		require.NoError(t, err)
		require.Equal(t, to, f.light.CurrentBlock().NumberU64())
		require.Equal(t, f.blocks[to-1].Hash(), f.light.CurrentBlock().Hash())
		f.requireState(t, to)
		from = to + 1
	}
	for n := uint64(1); n <= 60; n++ {
		block := f.light.GetBlockByNumber(n)
		require.NotNil(t, block)
		receipts := f.light.GetReceiptsByBlockHash(block.Hash())
		require.Equal(t, block.ReceiptHash(), types.DeriveReceiptsRoot(receipts, block.Number()), "block %d", n)
		committed := n%balBatchInterval == 0 || n == 10 || n == 40 || n == 60
		require.Equal(t, committed, f.light.HasState(block.Root()), "block %d", n)
		if committed {
			f.requireState(t, n)
		}
	}
	f.requireLists(t, f.light, 1, 60)
}

// TestBALBatchTampered inserts a batch with a wrong list in the middle: the
// batch is rejected at the next commit, and the chain is back at the last
// checked block, without the events of the dropped blocks.
func TestBALBatchTampered(t *testing.T) {
	f := newBALBatchFixture(t)
	_, err := InsertChain(f.light, f.balBlocks(t, 1, 10, 0))
	require.NoError(t, err)

	events := make(chan blockchain.ChainEvent, 100)
	sub := f.light.SubscribeChainEvent(events)
	defer sub.Unsubscribe()

	n, err := InsertChain(f.light, f.balBlocks(t, 11, 40, 20))
	require.ErrorContains(t, err, "invalid merkle root")
	require.Equal(t, 6, n)
	require.Equal(t, uint64(16), f.light.CurrentBlock().NumberU64())
	require.Nil(t, f.light.GetHeaderByNumber(17))
	require.Nil(t, f.light.GetBlockByHash(f.blocks[19].Hash()))
	require.True(t, f.light.HasState(f.blocks[15].Root()))
	f.requireState(t, 16)
	for len(events) > 0 {
		require.LessOrEqual(t, (<-events).Block.NumberU64(), uint64(16))
	}

	_, err = InsertChain(f.light, f.balBlocks(t, 17, 60, 0))
	require.NoError(t, err)
	require.Equal(t, f.blocks[59].Hash(), f.light.CurrentBlock().Hash())
	f.requireState(t, 60)
	f.requireLists(t, f.light, 1, 60)
}

// TestBALBatchTamperedLast rejects a wrong list in the last block of a batch,
// which has no block written after the last checked one to roll back.
func TestBALBatchTamperedLast(t *testing.T) {
	f := newBALBatchFixture(t)
	_, err := InsertChain(f.light, f.balBlocks(t, 1, 16, 0))
	require.NoError(t, err)
	_, err = InsertChain(f.light, f.balBlocks(t, 17, 17, 17))
	require.ErrorContains(t, err, "invalid merkle root")
	require.Equal(t, uint64(16), f.light.CurrentBlock().NumberU64())
	require.Nil(t, f.light.GetBlockByHash(f.blocks[16].Hash()))
	f.requireState(t, 16)
}

// balStateReader is an execution module reading the state of each inserted
// block and of its parent, like the valset and gasless modules.
type balStateReader struct {
	f     *balBatchFixture
	t     *testing.T
	mu    sync.Mutex
	err   error
	reads int
}

func (r *balStateReader) PostInsertBlock(block *types.Block) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	parent := r.f.light.GetHeaderByHash(block.ParentHash())
	for _, h := range []*types.Header{block.Header(), parent} {
		if !r.f.light.HasState(h.Root) {
			r.err = fmt.Errorf("block %d: no state", h.Number.Uint64())
			return r.err
		}
		got, err := r.f.light.StateAt(h.Root)
		if err != nil {
			r.err = fmt.Errorf("block %d: %w", h.Number.Uint64(), err)
			return r.err
		}
		want, err := r.f.src.StateAt(h.Root)
		if err != nil {
			return err
		}
		for _, addr := range r.f.env.addrs {
			if want.GetBalance(addr).Cmp(got.GetBalance(addr)) != 0 {
				r.err = fmt.Errorf("block %d: balance of %x", h.Number.Uint64(), addr)
				return r.err
			}
		}
		r.reads++
	}
	return nil
}

// TestBALBatchModules checks that modules read the state of the blocks inside
// a batch while it is inserted, and that other goroutines can read it too.
func TestBALBatchModules(t *testing.T) {
	f := newBALBatchFixture(t)
	r := &balStateReader{f: f, t: t}
	f.light.RegisterKaiaxModules(nil, nil, []kaiax.ExecutionModule{r}, nil, nil, nil)

	var (
		done  = make(chan struct{})
		wg    sync.WaitGroup
		reads atomic.Int64
	)
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			head := f.light.CurrentBlock()
			if sdb, err := f.light.StateAt(head.Root()); err == nil {
				sdb.GetBalance(f.env.addrs[2])
				reads.Add(1)
			}
		}
	})
	_, err := InsertChain(f.light, f.balBlocks(t, 1, 60, 0))
	close(done)
	wg.Wait()
	require.NoError(t, err)
	require.NoError(t, r.err)
	require.Equal(t, 120, r.reads)
	require.Positive(t, reads.Load())

	require.True(t, f.light.HasState(f.blocks[47].Root()))
	require.False(t, f.light.HasState(f.blocks[50].Root()))
	_, err = f.light.StateAt(f.blocks[50].Root())
	require.Error(t, err)
}
