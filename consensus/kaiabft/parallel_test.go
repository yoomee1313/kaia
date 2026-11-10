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

package kaiabft

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus"
	"github.com/kaiachain/kaia/consensus/bft"
	"github.com/kaiachain/kaia/consensus/mocks"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

// specChain is a fakeChain that serves the state and the tx lookup of a
// speculative execution.
type specChain struct {
	*fakeChain
}

func (c *specChain) PrunableStateAt(root common.Hash, _ uint64) (*state.StateDB, error) {
	return state.New(root, state.NewDatabase(database.NewMemoryDBManager()), nil, nil)
}

func (c *specChain) TxLookup() func(common.Hash) *types.Transaction { return nil }

// fakeParallel returns a given outcome for the parallel execution, and keeps
// the bad blocks reported to the chain.
type fakeParallel struct {
	res      *blockchain.SpeculativeResult
	err      error
	calls    int
	provided map[common.Hash]*access.BlockAccessList
	bad      []error
}

func (p *fakeParallel) ReportBadBlock(_ *types.Block, _ types.Receipts, err error) {
	p.bad = append(p.bad, err)
}

func (p *fakeParallel) ExecuteWithAccessList(context.Context, *types.Block, *types.Header, *access.BlockAccessList) (*blockchain.SpeculativeResult, error) {
	p.calls++
	return p.res, p.err
}

func (p *fakeParallel) ProvideAccessList(hash common.Hash, list *access.BlockAccessList) {
	if p.provided == nil {
		p.provided = make(map[common.Hash]*access.BlockAccessList)
	}
	p.provided[hash] = list
}

// parallelResult is a valid speculative result whose execution recorded list.
func parallelResult(t *testing.T, list *access.BlockAccessList, cost uint64) *blockchain.SpeculativeResult {
	sdb, err := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil, nil)
	require.NoError(t, err)
	access.EnableRecorder(sdb)
	access.SetMerged(sdb, list)
	return &blockchain.SpeculativeResult{State: sdb, ProcessStats: blockchain.ProcessStats{ComputationCost: cost}}
}

// runSpeculative executes block with list and returns the outcome posted to
// the machine.
func runSpeculative(t *testing.T, cfg *params.ChainConfig, block *types.Block, list *access.BlockAccessList, parallel *fakeParallel, inOrder func(*mocks.MockExecutor)) execResultEvent {
	return runSpeculativeOn(t, newSpecBackend(t, cfg, block, parallel), block, list, inOrder)
}

// newSpecBackend returns a backend that can execute block speculatively.
func newSpecBackend(t *testing.T, cfg *params.ChainConfig, block *types.Block, parallel *fakeParallel) *backend {
	parent := &types.Header{Number: block.Number(), Time: block.Time()}
	parent.Number.Sub(parent.Number, common.Big1)
	chain := &specChain{&fakeChain{cfg: cfg, current: newEmptyBlock(0, common.Hash{}), headers: map[common.Hash]*types.Header{block.ParentHash(): parent}}}
	b, _ := newTestBackend(t, chain)
	b.specCache = blockchain.NewSpeculativeResultCache()
	b.parallel = parallel
	b.badBlocks = parallel
	return b
}

// runSpeculativeOn executes block with list on b and returns the outcome
// posted to the machine.
func runSpeculativeOn(t *testing.T, b *backend, block *types.Block, list *access.BlockAccessList, inOrder func(*mocks.MockExecutor)) execResultEvent {
	ctrl := gomock.NewController(t)
	// The in-order executor fails the test on any call it does not expect.
	executor, clone := mocks.NewMockExecutor(ctrl), mocks.NewMockExecutor(ctrl)
	executor.EXPECT().Clone().Return(clone)
	if inOrder != nil {
		inOrder(clone)
	}
	b.executor = executor

	sub := b.eventMux.Subscribe(execResultEvent{})
	defer sub.Unsubscribe()
	b.startSpeculativeExecution(block, list)
	b.specWg.Wait()
	select {
	case ev := <-sub.Chan():
		return ev.Data.(execResultEvent)
	case <-time.After(2 * time.Second):
		t.Fatal("no execution result")
		return execResultEvent{}
	}
}

// From Amber a proposal runs in parallel with the BAL of its PRE-PREPARE. Its
// result is taken only if valid; otherwise the proposal runs in order, which
// alone rejects it.
func TestSpeculativeExecutionWithAccessList(t *testing.T) {
	block, enc := amberBlock(t, 10)
	list, err := access.Decode(enc)
	require.NoError(t, err)
	inOrderFails := func(e *mocks.MockExecutor) {
		e.EXPECT().ResetWithState(gomock.Any(), gomock.Any()).Return(nil)
		e.EXPECT().ProcessBlock(gomock.Any(), gomock.Any()).Return(nil, errors.New("invalid tx"))
	}

	t.Run("valid parallel result", func(t *testing.T) {
		parallel := &fakeParallel{res: parallelResult(t, list, 1)}
		ev := runSpeculative(t, amberConfig(), block, list, parallel, nil)
		require.Equal(t, 1, parallel.calls)
		require.NoError(t, ev.Err)
		require.Equal(t, enc, ev.AccessList)
		require.Empty(t, parallel.bad)
	})

	t.Run("parallel failure runs in order", func(t *testing.T) {
		parallel := &fakeParallel{err: blockchain.ErrAccessListMismatch}
		ev := runSpeculative(t, amberConfig(), block, list, parallel, inOrderFails)
		require.Equal(t, 1, parallel.calls)
		require.EqualError(t, ev.Err, "invalid tx")
		require.False(t, ev.Local)
		require.Equal(t, []error{ev.Err}, parallel.bad, "invalid in order: a bad block")
	})

	t.Run("invalid parallel result runs in order", func(t *testing.T) {
		cfg := amberConfig()
		cfg.AmberBlockComputationCostLimit = 10
		parallel := &fakeParallel{res: parallelResult(t, list, 11)}
		ev := runSpeculative(t, cfg, block, list, parallel, inOrderFails)
		require.Equal(t, 1, parallel.calls)
		require.EqualError(t, ev.Err, "invalid tx")
		require.Len(t, parallel.bad, 1, "only the in-order verdict is recorded")
	})

	t.Run("in-order result over a limit", func(t *testing.T) {
		cfg := amberConfig()
		cfg.AmberBlockComputationCostLimit = 10
		parallel := &fakeParallel{err: errors.New("missing trie node")}
		ev := runSpeculative(t, cfg, block, list, parallel, func(e *mocks.MockExecutor) {
			e.EXPECT().ResetWithState(gomock.Any(), gomock.Any()).Return(nil)
			e.EXPECT().ProcessBlock(gomock.Any(), gomock.Any()).Return(&consensus.ExecutionResult{State: parallelResult(t, list, 11).State, ComputationCost: 11}, nil)
		})
		require.ErrorIs(t, ev.Err, blockchain.ErrBlockComputationCostLimit)
		require.False(t, ev.Local)
		require.Equal(t, []error{ev.Err}, parallel.bad)
	})

	t.Run("local failure in order is not a rejection", func(t *testing.T) {
		parallel := &fakeParallel{err: errors.New("missing trie node")}
		ev := runSpeculative(t, amberConfig(), block, list, parallel, func(e *mocks.MockExecutor) {
			e.EXPECT().ResetWithState(gomock.Any(), gomock.Any()).Return(errors.New("closed"))
		})
		require.Error(t, ev.Err)
		require.True(t, ev.Local)
		require.Empty(t, parallel.bad, "a local failure is not a bad block")
	})

	t.Run("no list runs in order", func(t *testing.T) {
		parallel := &fakeParallel{res: parallelResult(t, list, 1)}
		ev := runSpeculative(t, amberConfig(), block, nil, parallel, inOrderFails)
		require.Zero(t, parallel.calls)
		require.Error(t, ev.Err)
	})
}

// A check of an in-order result that fails for a reason of this node leaves
// no usable result, so the block is executed again when it is proposed again.
func TestSpeculativeExecutionRetriesAfterLocalFailure(t *testing.T) {
	block, enc := amberBlock(t, 10)
	list, err := access.Decode(enc)
	require.NoError(t, err)
	parallel := &fakeParallel{}
	b := newSpecBackend(t, amberConfig(), block, parallel)
	inOrder := func(sdb *state.StateDB) func(*mocks.MockExecutor) {
		return func(e *mocks.MockExecutor) {
			e.EXPECT().ResetWithState(gomock.Any(), gomock.Any()).Return(nil)
			e.EXPECT().ProcessBlock(gomock.Any(), gomock.Any()).Return(&consensus.ExecutionResult{State: sdb}, nil)
		}
	}

	// Without a recorded BAL the result cannot be checked.
	unrecorded, err := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil, nil)
	require.NoError(t, err)
	ev := runSpeculativeOn(t, b, block, nil, inOrder(unrecorded))
	require.True(t, ev.Local)
	require.Empty(t, parallel.bad)
	require.False(t, b.specCache.HasUsable(block.Hash()))
	require.Nil(t, b.specCache.TryGet(block.Hash(), 0))

	ev = runSpeculativeOn(t, b, block, nil, inOrder(parallelResult(t, list, 1).State))
	require.NoError(t, ev.Err)
	require.Equal(t, enc, ev.AccessList)
	require.True(t, b.specCache.HasUsable(block.Hash()))
}

// An accepted PRE-PREPARE gives its checked BAL to the chain for InsertChain.
func TestPreprepareProvidesAccessList(t *testing.T) {
	proposal, enc := amberBlock(t, 10)
	parent := newEmptyBlock(9, common.Hash{})
	header := proposal.Header()
	header.ParentHash = parent.Hash()
	block := types.NewBlock(header, nil, nil)
	chain := &fakeChain{cfg: amberConfig(), current: parent, headers: map[common.Hash]*types.Header{parent.Hash(): parent.Header()}}
	b, key := newTestBackend(t, chain)
	parallel := &fakeParallel{}
	b.parallel = parallel
	self := crypto.PubkeyToAddress(key.PublicKey)
	proposerKey, err := crypto.GenerateKey()
	require.NoError(t, err)
	proposer := crypto.PubkeyToAddress(proposerKey.PublicKey)

	m := newTestMachine(b, 10, 0, nil, []common.Address{self, proposer})
	defer m.stopTimer()
	m.state = stateAcceptRequest
	m.proposer = proposer
	pp := &bft.Preprepare{View: &bft.View{Sequence: big.NewInt(10), Round: big.NewInt(0)}, Proposal: block, AccessList: enc}
	require.NoError(t, m.handlePreprepare(&wireMessage{Code: bft.MsgPreprepare, Msg: mustEncode(t, pp)}, proposer))
	got, err := parallel.provided[block.Hash()].Encode()
	require.NoError(t, err)
	require.Equal(t, enc, got)
}
