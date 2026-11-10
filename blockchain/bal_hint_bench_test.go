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
	"fmt"
	"testing"

	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/rlp"
	"github.com/stretchr/testify/require"
)

// BenchmarkAccessListHint measures the block access list hint of an Amber
// proposal on synthetic blocks: the bytes it saves in the PRE-PREPARE, the time
// the proposer takes to make it (strip and the check that it rebuilds the
// list), the time a validator takes to rebuild the list before it executes the
// block, with the senders of the txs cached or recovered again, the time the
// proposer takes to make the hints of its fragments, and the parallel
// execution with the hint against the one with the whole list and against
// executing the block in order:
//
//	go test ./blockchain -run '^$' -bench BenchmarkAccessListHint -benchtime 5x
func BenchmarkAccessListHint(b *testing.B) {
	cases := []struct {
		name string
		spec benchBlockSpec
	}{
		{"payment-known-10000", benchBlockSpec{payments: 10_000, known: true}},
		{"payment-new-10000", benchBlockSpec{payments: 10_000}},
		{"token-5000", benchBlockSpec{tokens: 5000}},
		{"mix-known", benchBlockSpec{tokens: 2000, payments: 8000, known: true}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			bb := makeBenchBlock(b, c.spec)
			hint, err := bb.chain.AccessListHint(bb.block, bb.list)
			require.NoError(b, err)
			listEnc, err := bb.list.Encode()
			require.NoError(b, err)
			hintEnc, err := hint.Encode()
			require.NoError(b, err)
			blockEnc, err := rlp.EncodeToBytes(bb.block)
			require.NoError(b, err)
			b.Logf("%s: %d txs, block %d B, list %d B (%d entries), hint %d B (%d entries), PRE-PREPARE %d B -> %d B",
				c.name, len(bb.block.Transactions()), len(blockEnc), len(listEnc), bb.list.EntryCount(), len(hintEnc), hint.EntryCount(),
				len(blockEnc)+len(listEnc), len(blockEnc)+len(hintEnc))

			report := func(b *testing.B) {
				b.ReportMetric(float64(len(listEnc)), "list-B")
				b.ReportMetric(float64(len(hintEnc)), "hint-B")
			}
			// Executing in order, as a validator does without a list: with the
			// recorder an Amber block needs, and without one.
			b.Run("in-order", func(b *testing.B) { benchInOrder(b, bb) })
			b.Run("in-order-no-bal", func(b *testing.B) {
				for b.Loop() {
					sdb, err := bb.chain.StateAt(bb.parent.Root)
					require.NoError(b, err)
					_, _, _, _, _, err = bb.chain.Processor().Process(context.Background(), bb.block, sdb, vm.Config{})
					require.NoError(b, err)
					require.Equal(b, bb.block.Root(), sdb.IntermediateRoot(true))
				}
			})
			b.Run("hint", func(b *testing.B) {
				for b.Loop() {
					_, err := bb.chain.AccessListHint(bb.block, bb.list)
					require.NoError(b, err)
				}
				report(b)
			})
			b.Run("rebuild", func(b *testing.B) {
				for b.Loop() {
					_, err := bb.chain.RebuildAccessList(bb.block, bb.parent, hint)
					require.NoError(b, err)
				}
			})
			b.Run("rebuild-cold-senders", func(b *testing.B) {
				for range b.N {
					b.StopTimer()
					var fresh types.Block
					require.NoError(b, rlp.DecodeBytes(blockEnc, &fresh))
					b.StartTimer()
					_, err := bb.chain.RebuildAccessList(&fresh, bb.parent, hint)
					require.NoError(b, err)
				}
			})
			b.Run(fmt.Sprintf("fragments-%d", 512), func(b *testing.B) {
				txs := bb.block.Transactions()
				for b.Loop() {
					hinter, err := bb.chain.NewFragmentHinter(bb.block.Header())
					require.NoError(b, err)
					for end := 512; ; end += 512 {
						end = min(end, len(txs))
						_, err := hinter.Next(bb.list, txs[:end])
						require.NoError(b, err)
						if end == len(txs) {
							break
						}
					}
				}
			})
			for _, given := range []struct {
				name string
				list *access.BlockAccessList
			}{{"parallel-with-hint", hint}, {"parallel-with-list", bb.list}} {
				b.Run(given.name, func(b *testing.B) {
					for b.Loop() {
						res, err := bb.chain.ExecuteWithAccessList(context.Background(), bb.block, bb.parent, given.list)
						require.NoError(b, err)
						require.Equal(b, bb.block.Root(), res.State.IntermediateRoot(true))
					}
				})
			}
		})
	}
}
