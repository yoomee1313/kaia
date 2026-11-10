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
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"math/rand"
	"runtime"
	"runtime/metrics"
	"sync"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

// benchTokenAddr holds a token whose balances are at the slot of the address:
// transfer(to, amount) with calldata to (32 bytes) || amount (32 bytes).
var benchTokenAddr = common.HexToAddress("0x000000000000000000000000000000000000e20c")

func benchTokenCode() []byte {
	return []byte{
		byte(vm.CALLER), byte(vm.DUP1), byte(vm.SLOAD), // c bal_c
		byte(vm.PUSH1), 0x20, byte(vm.CALLDATALOAD), // c bal_c amt
		byte(vm.SWAP1), byte(vm.SUB), // c bal_c-amt
		byte(vm.SWAP1), byte(vm.SSTORE),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD), byte(vm.DUP1), byte(vm.SLOAD), // to bal_to
		byte(vm.PUSH1), 0x20, byte(vm.CALLDATALOAD), byte(vm.ADD), // to bal_to+amt
		byte(vm.SWAP1), byte(vm.SSTORE),
		byte(vm.STOP),
	}
}

// benchLoopAddr runs keccak256 over 32 bytes of memory calldata[0:32] times.
var benchLoopAddr = common.HexToAddress("0x000000000000000000000000000000000000100f")

func benchLoopCode() []byte {
	return []byte{
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD), // n
		byte(vm.JUMPDEST),                                                       // pc 3
		byte(vm.PUSH1), 0x20, byte(vm.PUSH1), 0x00, byte(vm.SHA3), byte(vm.POP), // hash
		byte(vm.PUSH1), 0x01, byte(vm.SWAP1), byte(vm.SUB), // n-1
		byte(vm.DUP1), byte(vm.PUSH1), 0x03, byte(vm.JUMPI),
		byte(vm.STOP),
	}
}

type benchBlockSpec struct {
	tokens   int    // token transfers
	hot      bool   // every token transfer goes to one receiver
	payments int    // plain value transfers
	loops    int    // calls of the keccak loop
	iters    uint64 // iterations of each loop call
}

type benchBlock struct {
	chain  *BlockChain
	block  *types.Block
	parent *types.Header
	list   *access.BlockAccessList
}

var (
	benchBlocksMu sync.Mutex
	benchBlocks   = map[benchBlockSpec]*benchBlock{}
)

// makeBenchBlock builds a chain with one block of the given txs from distinct
// senders, from the Amber hardfork, and records its list in order.
func makeBenchBlock(b *testing.B, spec benchBlockSpec) *benchBlock {
	benchBlocksMu.Lock()
	defer benchBlocksMu.Unlock()
	if bb := benchBlocks[spec]; bb != nil {
		return bb
	}
	config := params.TestKaiaConfig("prague")
	config.AmberCompatibleBlock = big.NewInt(0)
	signer := types.LatestSignerForChainID(config.ChainID)
	rng := rand.New(rand.NewSource(1))
	n := spec.tokens + spec.payments + spec.loops
	keys := make([]*ecdsa.PrivateKey, n)
	storage := make(map[common.Hash]common.Hash, n)
	alloc := GenesisAlloc{balSystemAddr: {Code: []byte{byte(vm.STOP)}, Balance: common.Big0}}
	for i := range keys {
		key, err := crypto.GenerateKey()
		require.NoError(b, err)
		keys[i] = key
		addr := crypto.PubkeyToAddress(key.PublicKey)
		alloc[addr] = GenesisAccount{Balance: balFunds}
		if i < spec.tokens {
			storage[common.BytesToHash(addr[:])] = common.BigToHash(big.NewInt(1e18))
		}
	}
	alloc[benchTokenAddr] = GenesisAccount{Code: benchTokenCode(), Balance: common.Big0, Storage: storage}
	alloc[benchLoopAddr] = GenesisAccount{Code: benchLoopCode(), Balance: common.Big0}
	db := database.NewMemoryDBManager()
	genesis := (&Genesis{Config: config, Alloc: alloc}).MustCommit(db)
	sealer := balTestSealer{Faker: faker.NewFaker(), initAddr: crypto.PubkeyToAddress(keys[0].PublicKey)}

	randomAddr := func() common.Address {
		var a common.Address
		rng.Read(a[:])
		return a
	}
	hot := randomAddr()
	txs := make([]*types.Transaction, 0, n)
	for i, key := range keys {
		var (
			to     common.Address
			amount = common.Big0
			data   []byte
			gas    = uint64(100_000)
		)
		if i >= spec.tokens+spec.payments {
			to, data = benchLoopAddr, common.LeftPadBytes(new(big.Int).SetUint64(spec.iters).Bytes(), 32)
			gas = 100_000 + 100*spec.iters
		} else if i < spec.tokens {
			receiver := randomAddr()
			if spec.hot {
				receiver = hot
			}
			to = benchTokenAddr
			data = append(common.LeftPadBytes(receiver[:], 32), common.LeftPadBytes([]byte{1}, 32)...)
		} else {
			to, amount = randomAddr(), big.NewInt(1)
		}
		tx, err := types.SignTx(types.NewTransaction(0, to, amount, gas, balGasPrice, data), signer, key)
		require.NoError(b, err)
		txs = append(txs, tx)
	}
	blocks, _ := GenerateChain(config, genesis, sealer, db, 1, func(_ int, g *BlockGen) {
		for _, tx := range txs {
			g.AddTx(tx)
		}
	})
	// No snapshot, as a node runs without --snapshot, and none generated in
	// the background while the blocks run.
	chain, err := NewBlockChain(db, &CacheConfig{
		CacheSize:            512,
		BlockInterval:        DefaultBlockInterval,
		TriesInMemory:        DefaultTriesInMemory,
		LivePruningRetention: DefaultLivePruningRetention,
		TrieNodeCacheConfig:  statedb.GetEmptyTrieNodeCacheConfig(),
	}, config, sealer, vm.Config{})
	require.NoError(b, err)
	chain.Processor().RegisterBlockStateModule(sealer)
	_, err = chain.InsertChain(blocks)
	require.NoError(b, err)

	block := blocks[0]
	parent := chain.GetHeaderByHash(block.ParentHash())
	sdb, err := chain.StateAt(parent.Root)
	require.NoError(b, err)
	access.EnableRecorder(sdb)
	receipts, _, _, _, stats, err := chain.Processor().Process(context.Background(), block, sdb, vm.Config{})
	require.NoError(b, err)
	for i, r := range receipts {
		require.Equal(b, types.ReceiptStatusSuccessful, r.Status, "tx %d", i)
	}
	b.Logf("computation cost %d (limit %d)", stats.ComputationCost, params.DefaultAmberBlockComputationCostLimit)
	require.Equal(b, block.Root(), sdb.IntermediateRoot(true))
	bb := &benchBlock{chain: chain, block: block, parent: parent, list: access.Recorded(sdb)}
	benchBlocks[spec] = bb
	return bb
}

// peakHeap samples the live heap until stop is called and returns the peak
// above the heap at the start.
func peakHeap() func() uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() uint64 { metrics.Read(sample); return sample[0].Value.Uint64() }
	base, peak := read(), uint64(0)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			if v := read(); v > peak {
				peak = v
			}
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return func() uint64 {
		close(done)
		<-stopped
		if peak < base {
			return 0
		}
		return peak - base
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// benchInOrder executes the block in order with the recorder, as a validator
// does without a list, up to the state root and the encoded list.
func benchInOrder(b *testing.B, bb *benchBlock) {
	var total time.Duration
	var peak uint64
	b.ResetTimer()
	for range b.N {
		runtime.GC()
		stop := peakHeap()
		start := time.Now()
		sdb, err := bb.chain.StateAt(bb.parent.Root)
		require.NoError(b, err)
		access.EnableRecorder(sdb)
		_, _, _, _, _, err = bb.chain.Processor().Process(context.Background(), bb.block, sdb, vm.Config{})
		require.NoError(b, err)
		require.Equal(b, bb.block.Root(), sdb.IntermediateRoot(true))
		_, err = access.Recorded(sdb).Encode()
		require.NoError(b, err)
		total += time.Since(start)
		peak = max(peak, stop())
	}
	b.ReportMetric(ms(total)/float64(b.N), "total-ms")
	b.ReportMetric(float64(peak)/(1<<20), "peak-MiB")
}

// benchParallel executes the block in parallel with its list, as
// ExecuteWithAccessList does, up to the state root.
func benchParallel(b *testing.B, bb *benchBlock, workers int) {
	sp := bb.chain.Processor().(*StateProcessor)
	var sum ParallelTiming
	var prefetch, root, total time.Duration
	var peak uint64
	b.ResetTimer()
	for range b.N {
		runtime.GC()
		stop := peakHeap()
		start := time.Now()
		bb.chain.PrefetchAccessList(context.Background(), bb.parent.Root, bb.list, workers)
		prefetched := time.Now()
		var t ParallelTiming
		final, _, _, _, _, err := sp.processWithAccessList(context.Background(), bb.block, bb.parent, bb.list, vm.Config{}, workers, true, &t)
		require.NoError(b, err)
		processed := time.Now()
		require.Equal(b, bb.block.Root(), final.IntermediateRoot(true))
		end := time.Now()
		peak = max(peak, stop())

		prefetch += prefetched.Sub(start)
		root += end.Sub(processed)
		total += end.Sub(start)
		sum.Prepare += t.Prepare
		sum.Execute += t.Execute
		sum.Phases += t.Phases
		sum.Final += t.Final
		sum.FinalPre += t.FinalPre
		sum.Merge += t.Merge
	}
	n := float64(b.N)
	b.ReportMetric(ms(prefetch)/n, "prefetch-ms")
	b.ReportMetric(ms(sum.Prepare)/n, "prepare-cpu-ms")
	b.ReportMetric(ms(sum.Execute)/n, "exec-cpu-ms")
	b.ReportMetric(ms(sum.Phases)/n, "phases-ms")
	b.ReportMetric(ms(sum.Final)/n, "final-ms")
	b.ReportMetric(ms(sum.FinalPre)/n, "final-prep-ms")
	b.ReportMetric(ms(sum.Merge)/n, "merge-ms")
	b.ReportMetric(ms(root)/n, "root-ms")
	b.ReportMetric(ms(total)/n, "total-ms")
	b.ReportMetric(float64(peak)/(1<<20), "peak-MiB")
}

// BenchmarkAccessListExecution compares executing synthetic blocks in order and
// in parallel with their list:
//
//	go test ./blockchain -run '^$' -bench BenchmarkAccessListExecution -benchtime 5x
func BenchmarkAccessListExecution(b *testing.B) {
	cases := []struct {
		name string
		spec benchBlockSpec
	}{}
	for _, n := range []int{200, 1000, 5000} {
		cases = append(cases,
			struct {
				name string
				spec benchBlockSpec
			}{fmt.Sprintf("token-%d", n), benchBlockSpec{tokens: n}},
			struct {
				name string
				spec benchBlockSpec
			}{fmt.Sprintf("token-hot-%d", n), benchBlockSpec{tokens: n, hot: true}},
			struct {
				name string
				spec benchBlockSpec
			}{fmt.Sprintf("payment-%d", n), benchBlockSpec{payments: n}},
		)
	}
	// Token transfers add 3 entries each and payments 2, up to the limit.
	cases = append(cases, struct {
		name string
		spec benchBlockSpec
	}{"mix-limit", benchBlockSpec{tokens: 5000, payments: 8880}})
	// Loop calls up to the block computation cost limit, about 1,916 per
	// iteration: few heavy txs and many light ones.
	for _, c := range []struct {
		txs   int
		iters uint64
	}{{4, 52_000}, {40, 5_200}, {400, 520}} {
		cases = append(cases, struct {
			name string
			spec benchBlockSpec
		}{fmt.Sprintf("compute-%d", c.txs), benchBlockSpec{loops: c.txs, iters: c.iters}})
	}

	workerCounts := []int{1, 4, 8, runtime.GOMAXPROCS(0)}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			bb := makeBenchBlock(b, c.spec)
			b.Logf("%s: %d txs, %d list entries (limit %d)", c.name, len(bb.block.Transactions()), bb.list.WriteEntryCount(), params.DefaultAmberBlockAccessListEntryLimit)
			b.Run("in-order", func(b *testing.B) { benchInOrder(b, bb) })
			for _, w := range workerCounts {
				b.Run(fmt.Sprintf("parallel-%d", w), func(b *testing.B) { benchParallel(b, bb, w) })
			}
		})
	}
}
