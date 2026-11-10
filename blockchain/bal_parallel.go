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
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
)

// ErrAccessListMismatch is returned when executing a block does not record the
// block access list it was executed with.
var ErrAccessListMismatch = errors.New("block access list does not match the execution")

var errParallelUnsupported = errors.New("parallel execution does not support this vm config")

// ParallelExecutionWorkers is the number of goroutines that execute a block
// with its block access list.
var ParallelExecutionWorkers = runtime.GOMAXPROCS(0)

// parallelSupported reports whether the txs of a block can run in parallel
// with cfg: a tracer, internal tx traces and a tx hook are per block, not per
// tx.
func parallelSupported(cfg vm.Config) bool {
	return !cfg.Debug && cfg.Tracer == nil && !cfg.EnableInternalTxTracing && !cfg.EnableOpDebug && cfg.BeforeTxFinalise == nil
}

// ExecuteWithAccessList executes a block from the Amber hardfork in parallel
// with its block access list, reads included, after prefetching the keys of
// the list, and returns the result as the speculative cache holds it: the
// state to commit, whose BlockAccessList is the list of the whole block, and
// the receipts, the bloom and the receipt root, which the caller checks with
// ValidateStateWithCache.
//
// The list only says what to prefetch and where each phase starts. An error
// says nothing about the block, as the list may be wrong while the block is
// right: the caller executes the block in order instead, which decides.
func (bc *BlockChain) ExecuteWithAccessList(ctx context.Context, block *types.Block, parent *types.Header, list *access.BlockAccessList) (*SpeculativeResult, error) {
	if !bc.chainConfig.IsAmberForkEnabled(block.Number()) {
		return nil, errors.New("block access list before the Amber hardfork")
	}
	sp, ok := bc.processor.(*StateProcessor)
	if !ok {
		return nil, errors.New("no state processor")
	}
	bc.PrefetchAccessList(ctx, parent.Root, list, ParallelExecutionWorkers)
	final, receipts, logs, usedGas, stats, err := sp.ProcessWithAccessList(ctx, block, parent, list, bc.vmConfig, ParallelExecutionWorkers, true)
	if err != nil {
		return nil, err
	}
	return &SpeculativeResult{
		State:        final,
		Receipts:     receipts,
		Logs:         logs,
		UsedGas:      usedGas,
		ProcessStats: stats,
		Bloom:        types.CreateBloom(receipts),
		ReceiptHash:  types.DeriveReceiptsRoot(receipts, block.Number()),
	}, nil
}

// PrefetchAccessList reads every account and slot of a block access list from
// the state at root with the given number of goroutines, so that their trie
// nodes are cached before the block is executed. It stops early when ctx is
// done.
func (bc *BlockChain) PrefetchAccessList(ctx context.Context, root common.Hash, list *access.BlockAccessList, workers int) {
	type key struct {
		addr common.Address
		slot *common.Hash
	}
	var keys []key
	for i := range list.Accounts {
		a := &list.Accounts[i]
		keys = append(keys, key{addr: a.Address})
		for j := range a.StorageWrites {
			keys = append(keys, key{addr: a.Address, slot: &a.StorageWrites[j].Slot})
		}
		for j := range a.StorageReads {
			keys = append(keys, key{addr: a.Address, slot: &a.StorageReads[j]})
		}
	}
	var (
		wg   sync.WaitGroup
		next atomic.Int64
	)
	for range max(1, workers) {
		wg.Go(func() {
			sdb, err := state.New(root, bc.stateCache, bc.snaps, nil)
			if err != nil {
				return
			}
			for i := int(next.Add(1) - 1); i < len(keys) && ctx.Err() == nil; i = int(next.Add(1) - 1) {
				if k := keys[i]; k.slot == nil {
					sdb.GetBalance(k.addr)
				} else {
					sdb.GetState(k.addr, *k.slot)
				}
			}
		})
	}
	wg.Wait()
}

// ProcessWithAccessList executes a block with its block access list: every
// phase (InitializeState, each tx, FinalizeState) runs on its own state, the
// parent state with the changes of the earlier phases of the list applied, so
// the txs run in parallel on the given number of goroutines. The changes each
// phase records are merged and must equal the writes of the list, and its
// reads too if withReads is set; otherwise ErrAccessListMismatch is returned
// and the block must be processed in order.
//
// It returns the state at the end of the block, whose root the caller checks
// with ValidateState, as after Process. The state is opened like the one
// InsertChain commits, and its BlockAccessList is the merged list.
func (p *StateProcessor) ProcessWithAccessList(ctx context.Context, block *types.Block, parent *types.Header, list *access.BlockAccessList, cfg vm.Config, workers int, withReads bool) (*state.StateDB, types.Receipts, []*types.Log, uint64, ProcessStats, error) {
	return p.processWithAccessList(ctx, block, parent, list, cfg, workers, withReads, nil)
}

// ParallelTiming is where the time of ProcessWithAccessList goes. Prepare and
// Execute are summed over the phases before FinalizeState, which run on
// several goroutines; the others are elapsed times.
type ParallelTiming struct {
	Prepare  time.Duration // open the state of each phase and apply the list before it
	Execute  time.Duration // run InitializeState or the tx of each phase
	Phases   time.Duration // elapsed time of the phases before FinalizeState
	Final    time.Duration // open and prepare the state of FinalizeState, and run it
	FinalPre time.Duration // the part of Final that prepares the state
	Merge    time.Duration // merge, encode and compare the lists
}

func (p *StateProcessor) processWithAccessList(ctx context.Context, block *types.Block, parent *types.Header, list *access.BlockAccessList, cfg vm.Config, workers int, withReads bool, timing *ParallelTiming) (*state.StateDB, types.Receipts, []*types.Log, uint64, ProcessStats, error) {
	var (
		header = block.Header()
		txs    = block.Transactions()
		n      = len(txs)
		phases = make([]*access.BlockAccessList, n+2)
		stats  ProcessStats
	)
	if !parallelSupported(cfg) {
		return nil, nil, nil, 0, stats, errParallelUnsupported
	}
	author, _ := p.bc.sealer.Author(header) // Ignore error, as Process does

	// The phases before FinalizeState see the earlier phases of the list as
	// they load accounts. The last one gets the list applied, as its state is
	// committed, and it marks obsolete nodes for live pruning as the state of
	// Process does.
	index := access.NewIndex(list)
	open := func(phase int) (*state.StateDB, error) {
		if phase == n+1 {
			sdb, err := p.bc.PrunableStateAt(parent.Root, parent.Number.Uint64())
			if err != nil {
				return nil, err
			}
			if err := access.ApplyBefore(sdb, list, phase); err != nil {
				return nil, fmt.Errorf("phase %d: %w", phase, err)
			}
			access.EnableRecorderAt(sdb, phase)
			return sdb, nil
		}
		sdb, err := p.bc.StateAt(parent.Root)
		if err != nil {
			return nil, err
		}
		if phase > 0 {
			access.UseBefore(sdb, index, phase)
		}
		access.EnableRecorderAt(sdb, phase)
		return sdb, nil
	}

	// InitializeState and the txs run at the same time.
	var (
		receipts = make(types.Receipts, n)
		gas      = make([]uint64, n)
		costs    = make([]uint64, n)
		errs     = make([]error, n+1)
		wg       sync.WaitGroup
		next     atomic.Int64
	)
	var prepare, execute atomic.Int64
	run := func(phase int) {
		start := time.Now()
		sdb, err := open(phase)
		if err != nil {
			errs[phase] = err
			return
		}
		prepared := time.Now()
		prepare.Add(int64(prepared.Sub(start)))
		defer func() { execute.Add(int64(time.Since(prepared))) }()
		if phase == 0 {
			if err := p.InitializeState(header, sdb); err != nil {
				errs[phase] = fmt.Errorf("phase 0: %w", err)
				return
			}
		} else {
			tx := txs[phase-1]
			sdb.SetTxContext(tx.Hash(), block.Hash(), phase-1)
			// The EVM fills in its config, so every phase has its own.
			txCfg := cfg
			txCfg.BeforeTxFinalise = func(cost uint64) error {
				costs[phase-1] = cost
				return nil
			}
			receipt, _, err := p.bc.ApplyTransaction(p.config, &author, sdb, header, tx, &gas[phase-1], &txCfg)
			if err != nil {
				errs[phase] = fmt.Errorf("tx %d: %w", phase-1, err)
				return
			}
			receipts[phase-1] = receipt
		}
		if err := sdb.Error(); err != nil {
			errs[phase] = fmt.Errorf("phase %d: %w", phase, err)
			return
		}
		phases[phase] = access.Recorded(sdb)
	}
	stats.BeforeApplyTxs = time.Now()
	for range max(1, workers) {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i <= n && ctx.Err() == nil; i = int(next.Add(1) - 1) {
				run(i)
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, 0, stats, err
	}
	for _, err := range errs {
		if err != nil {
			return nil, nil, nil, 0, stats, err
		}
	}
	stats.AfterApplyTxs = time.Now()
	if timing != nil {
		timing.Prepare, timing.Execute = time.Duration(prepare.Load()), time.Duration(execute.Load())
		timing.Phases = stats.AfterApplyTxs.Sub(stats.BeforeApplyTxs)
	}

	// Logs are numbered across the block, as one state numbers them in order.
	var (
		usedGas uint64
		logs    []*types.Log
	)
	for i, receipt := range receipts {
		usedGas += gas[i]
		stats.ComputationCost += costs[i]
		for _, l := range receipt.Logs {
			l.Index = uint(len(logs))
			logs = append(logs, l)
		}
	}

	// FinalizeState needs the receipts of all txs.
	finalStart := time.Now()
	final, err := open(n + 1)
	if err != nil {
		return nil, nil, nil, 0, stats, err
	}
	if timing != nil {
		timing.FinalPre = time.Since(finalStart)
	}
	header.GasUsed = usedGas
	if _, err := p.FinalizeState(header, final, txs, receipts); err != nil {
		return nil, nil, nil, 0, stats, err
	}
	stats.AfterFinalize = time.Now()
	phases[n+1] = access.Recorded(final)
	if timing != nil {
		timing.Final = stats.AfterFinalize.Sub(finalStart)
		defer func(start time.Time) { timing.Merge = time.Since(start) }(time.Now())
	}

	merged := access.Merge(phases...)
	got, want := merged, list
	if !withReads {
		got, want = got.Writes(), list.Writes()
	}
	gotEnc, err := got.Encode()
	if err != nil {
		return nil, nil, nil, 0, stats, err
	}
	wantEnc, err := want.Encode()
	if err != nil {
		return nil, nil, nil, 0, stats, err
	}
	if !bytes.Equal(gotEnc, wantEnc) {
		return nil, nil, nil, 0, stats, fmt.Errorf("%w: block %d, %s", ErrAccessListMismatch, block.NumberU64(), firstDifference(got, want))
	}
	access.SetMerged(final, merged)
	return final, receipts, logs, usedGas, stats, nil
}

// firstDifference describes the first account whose entries differ.
func firstDifference(got, want *access.BlockAccessList) string {
	accounts := func(l *access.BlockAccessList) map[common.Address][]byte {
		m := make(map[common.Address][]byte, len(l.Accounts))
		for i := range l.Accounts {
			enc, _ := rlp.EncodeToBytes(&l.Accounts[i])
			m[l.Accounts[i].Address] = enc
		}
		return m
	}
	g, w := accounts(got), accounts(want)
	for i := range want.Accounts {
		addr := want.Accounts[i].Address
		if !bytes.Equal(g[addr], w[addr]) {
			return fmt.Sprintf("account %s: executed %x, list %x", addr.Hex(), g[addr], w[addr])
		}
	}
	for i := range got.Accounts {
		if addr := got.Accounts[i].Address; w[addr] == nil {
			return fmt.Sprintf("account %s is not in the list", addr.Hex())
		}
	}
	return "same accounts"
}
