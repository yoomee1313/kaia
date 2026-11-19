// Modifications Copyright 2024 The Kaia Authors
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

package work

import (
	"errors"
	"sync"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus"
	"github.com/kaiachain/kaia/event"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/work/builder"
)

var executorLogger = log.NewModuleLogger(log.ConsensusIstanbul)

var (
	ErrExecutorNotInitialized = errors.New("executor not initialized, call Reset first")
	ErrStateRootMismatch      = errors.New("state root mismatch")
)

// DefaultExecutor implements the consensus.Executor interface.
// It provides transaction execution functionality that can be used by consensus engines.
type DefaultExecutor struct {
	mu sync.RWMutex

	config   *params.ChainConfig
	chain    BlockChain
	signer   types.Signer
	nodeAddr common.Address

	// Current execution state
	state    *state.StateDB
	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt
	logs     []*types.Log
	usedGas  uint64

	// exec runs the block, from Execute or ExecuteTransactions to FinalizeState.
	exec *blockchain.BlockExecution

	// Transaction bundling modules for gasless transactions
	txBundlingModules []builder.TxBundlingModule

	// initialized indicates whether Reset has been called
	initialized bool
}

// NewDefaultExecutor creates a new DefaultExecutor instance.
func NewDefaultExecutor(config *params.ChainConfig, chain BlockChain, nodeAddr common.Address) *DefaultExecutor {
	return &DefaultExecutor{
		config:   config,
		chain:    chain,
		nodeAddr: nodeAddr,
		signer:   types.MakeSigner(config, nil),
	}
}

// Clone returns a fresh DefaultExecutor that shares immutable configuration
// with e but carries its own mutable execution state.
func (e *DefaultExecutor) Clone() consensus.Executor {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return &DefaultExecutor{
		config:            e.config,
		chain:             e.chain,
		nodeAddr:          e.nodeAddr,
		signer:            e.signer,
		txBundlingModules: e.txBundlingModules,
	}
}

// SetTxBundlingModules sets the transaction bundling modules for gasless transactions.
func (e *DefaultExecutor) SetTxBundlingModules(modules []builder.TxBundlingModule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.txBundlingModules = modules
}

// ResetWithState initializes the executor with a pre-existing state and header.
// This is useful when the caller has already prepared the state and header.
func (e *DefaultExecutor) ResetWithState(statedb *state.StateDB, header *types.Header) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.state = statedb
	e.header = header
	e.txs = nil
	e.receipts = nil
	e.logs = nil
	e.usedGas = 0
	e.exec = nil
	e.signer = types.MakeSigner(e.config, header.Number)
	e.initialized = true

	return nil
}

// Execute executes a batch of transactions and returns the execution result.
// This uses work.Task.CommitTransactions for full transaction execution with bundle handling.
func (e *DefaultExecutor) Execute(txs *types.TransactionsByPriceAndNonce, mux *event.TypeMux) (*consensus.ExecutionResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.initialized {
		return nil, ErrExecutorNotInitialized
	}

	// Create Task for transaction execution with bundle handling and time limits
	task := NewTask(e.config, e.signer, e.state, e.header)
	if err := task.Begin(e.chain, e.nodeAddr); err != nil {
		return nil, err
	}
	e.exec = task.exec

	// Execute transactions using CommitTransactions (includes posting pending events)
	task.CommitTransactions(mux, txs, e.chain, e.nodeAddr, e.txBundlingModules)

	// Get results from task
	e.txs = task.Transactions()
	e.receipts = task.Receipts()
	e.usedGas = e.header.GasUsed

	return e.buildResult(), nil
}

// ExecuteTransactions executes the given transactions in the provided order
// without re-sorting. This is the validator-path primitive used during
// speculative execution of a received pre-prepare block.
//
// It runs the block like StateProcessor.Process, through a
// blockchain.BlockExecution, but excludes FinalizeState — the caller must
// invoke FinalizeState separately after this returns.
func (e *DefaultExecutor) ExecuteTransactions(txs []*types.Transaction) (*consensus.ExecutionResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.initialized {
		return nil, ErrExecutorNotInitialized
	}

	// Extract the block proposer from the header seal — must match
	// StateProcessor.Process so COINBASE and reward distribution are identical.
	// Using e.nodeAddr here would be wrong: the local validator is not
	// necessarily the proposer of this block.
	author, _ := e.chain.Sealer().Author(e.header)

	exec, err := blockchain.BeginBlockExecution(e.chain, e.config, e.header, e.state, blockchain.BlockExecutionConfig{
		Author:   author,
		ForWrite: true,
	})
	if err != nil {
		return nil, err
	}
	receipts, allLogs, _, err := exec.ApplyTxs(txs)
	if err != nil {
		return nil, err
	}

	e.exec = exec
	e.header.GasUsed = exec.UsedGas()
	e.txs = txs
	e.receipts = receipts
	e.logs = allLogs
	e.usedGas = exec.UsedGas()

	return e.buildResult(), nil
}

// FinalizeState runs post-transaction state modifications and assembles final block.
func (e *DefaultExecutor) FinalizeState(result *consensus.ExecutionResult) (*types.Block, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if !e.initialized {
		return nil, ErrExecutorNotInitialized
	}
	if result == nil {
		return nil, errors.New("execution result is nil")
	}
	if e.exec == nil {
		return nil, ErrExecutorNotInitialized
	}
	return e.exec.Finish(result.Txs, result.Receipts)
}

// buildResult builds and returns the current execution result.
func (e *DefaultExecutor) buildResult() *consensus.ExecutionResult {
	return &consensus.ExecutionResult{
		State:    e.state,
		Receipts: e.receipts,
		Logs:     e.logs,
		UsedGas:  e.usedGas,
		Txs:      e.txs,
	}
}
