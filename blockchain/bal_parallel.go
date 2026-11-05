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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
)

// ErrAccessListMismatch is returned when executing a block does not record the
// block access list it was executed with.
var ErrAccessListMismatch = errors.New("block access list does not match the execution")

// PrefetchAccessList reads every account and slot of a block access list from
// the state at root with the given number of goroutines, so that their trie
// nodes are cached before the block is executed.
func (bc *BlockChain) PrefetchAccessList(root common.Hash, list *bal.BlockAccessList, workers int) {
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
			for i := int(next.Add(1) - 1); i < len(keys); i = int(next.Add(1) - 1) {
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
// with ValidateState, as after Process.
func (p *StateProcessor) ProcessWithAccessList(block *types.Block, parentRoot common.Hash, list *bal.BlockAccessList, cfg vm.Config, workers int, withReads bool) (*state.StateDB, types.Receipts, []*types.Log, uint64, error) {
	var (
		header = block.Header()
		txs    = block.Transactions()
		n      = len(txs)
		phases = make([]*bal.BlockAccessList, n+2)
	)
	author, _ := p.bc.sealer.Author(header) // Ignore error, as Process does

	open := func(phase int) (*state.StateDB, error) {
		sdb, err := state.New(parentRoot, p.bc.stateCache, p.bc.snaps, nil)
		if err != nil {
			return nil, err
		}
		if phase > 0 {
			if err := sdb.ApplyBlockAccessListBefore(list, phase); err != nil {
				return nil, fmt.Errorf("phase %d: %w", phase, err)
			}
		}
		sdb.EnableBlockAccessRecorderAt(phase)
		return sdb, nil
	}

	// InitializeState and the txs run at the same time.
	var (
		receipts = make(types.Receipts, n)
		gas      = make([]uint64, n)
		errs     = make([]error, n+1)
		wg       sync.WaitGroup
		next     atomic.Int64
	)
	run := func(phase int) {
		sdb, err := open(phase)
		if err != nil {
			errs[phase] = err
			return
		}
		if phase == 0 {
			if err := p.InitializeState(header, sdb); err != nil {
				errs[phase] = fmt.Errorf("%w: %w", ErrInitializeState, err)
				return
			}
		} else {
			tx := txs[phase-1]
			sdb.SetTxContext(tx.Hash(), block.Hash(), phase-1)
			// The EVM fills in its config, so every phase has its own.
			txCfg := cfg
			receipt, _, err := p.bc.ApplyTransaction(p.config, &author, sdb, header, tx, &gas[phase-1], &txCfg)
			if err != nil {
				errs[phase] = fmt.Errorf("tx %d: %w", phase-1, err)
				return
			}
			receipts[phase-1] = receipt
		}
		phases[phase] = sdb.BlockAccessList()
	}
	for range max(1, workers) {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i <= n; i = int(next.Add(1) - 1) {
				run(i)
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, nil, nil, 0, err
		}
	}

	// Logs are numbered across the block, as one state numbers them in order.
	var (
		usedGas uint64
		logs    []*types.Log
	)
	for i, receipt := range receipts {
		usedGas += gas[i]
		for _, l := range receipt.Logs {
			l.Index = uint(len(logs))
			logs = append(logs, l)
		}
	}

	// FinalizeState needs the receipts of all txs.
	final, err := open(n + 1)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	header.GasUsed = usedGas
	if _, err := p.FinalizeState(header, final, txs, receipts); err != nil {
		return nil, nil, nil, 0, err
	}
	phases[n+1] = final.BlockAccessList()

	got, want := bal.Merge(phases...), list
	if !withReads {
		got, want = got.Writes(), list.Writes()
	}
	gotEnc, err := got.Encode()
	if err != nil {
		return nil, nil, nil, 0, err
	}
	wantEnc, err := want.Encode()
	if err != nil {
		return nil, nil, nil, 0, err
	}
	if !bytes.Equal(gotEnc, wantEnc) {
		return nil, nil, nil, 0, fmt.Errorf("%w: block %d, %s", ErrAccessListMismatch, block.NumberU64(), firstDifference(got, want))
	}
	return final, receipts, logs, usedGas, nil
}

// firstDifference describes the first account whose entries differ.
func firstDifference(got, want *bal.BlockAccessList) string {
	accounts := func(l *bal.BlockAccessList) map[common.Address][]byte {
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

// AccessListBenchmark compares executing a block in order and in parallel with
// its recorded block access list.
type AccessListBenchmark struct {
	Number      uint64 `json:"number"`
	Txs         int    `json:"txs"`
	Sequential  string `json:"sequential"`
	Prefetch    string `json:"prefetch"`
	Parallel    string `json:"parallel"`
	RootMatch   bool   `json:"rootMatch"`
	WritesMatch bool   `json:"writesMatch"`
	ReadsMatch  bool   `json:"readsMatch"`
	Error       string `json:"error,omitempty"`
}

// BenchmarkAccessList executes a block whose list was recorded by this node,
// once in order to warm the caches, then in order and in parallel, and
// compares the results. The state of the parent must be available.
func (bc *BlockChain) BenchmarkAccessList(number uint64, workers int) (*AccessListBenchmark, error) {
	if bc.bal == nil {
		return nil, errors.New("block access lists are not recorded (--bal.record-dir)")
	}
	block := bc.GetBlockByNumber(number)
	if block == nil || number == 0 {
		return nil, fmt.Errorf("no block %d", number)
	}
	parent := bc.GetHeaderByHash(block.ParentHash())
	if parent == nil {
		return nil, fmt.Errorf("no parent of block %d", number)
	}
	list, err := bc.bal.freezer.Read(number)
	if err != nil {
		return nil, err
	}
	sequential := func() (time.Duration, *state.StateDB, error) {
		sdb, err := bc.StateAt(parent.Root)
		if err != nil {
			return 0, nil, err
		}
		sdb.EnableBlockAccessRecorder()
		start := time.Now()
		if _, _, _, _, _, err := bc.processor.Process(block, sdb, bc.vmConfig); err != nil {
			return 0, nil, err
		}
		sdb.IntermediateRoot(true)
		return time.Since(start), sdb, nil
	}
	if _, _, err := sequential(); err != nil {
		return nil, err
	}
	seqTime, seq, err := sequential()
	if err != nil {
		return nil, err
	}
	res := &AccessListBenchmark{Number: number, Txs: len(block.Transactions()), Sequential: seqTime.String()}

	start := time.Now()
	bc.PrefetchAccessList(parent.Root, list, workers)
	res.Prefetch = time.Since(start).String()
	sp, ok := bc.processor.(*StateProcessor)
	if !ok {
		return nil, errors.New("no state processor")
	}
	start = time.Now()
	final, _, _, _, err := sp.ProcessWithAccessList(block, parent.Root, list, bc.vmConfig, workers, false)
	res.Parallel = time.Since(start).String()
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	res.WritesMatch = true
	res.RootMatch = final.IntermediateRoot(true) == block.Root() && seq.IntermediateRoot(true) == block.Root()
	recorded, err := seq.BlockAccessList().Encode()
	if err != nil {
		return nil, err
	}
	stored, err := list.Encode()
	if err != nil {
		return nil, err
	}
	res.ReadsMatch = bytes.Equal(recorded, stored)
	return res, nil
}
