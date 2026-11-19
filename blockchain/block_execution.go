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
	"fmt"
	"time"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/params"
)

// ExecutionChain is the chain a BlockExecution runs on.
type ExecutionChain interface {
	Processor() Processor
	PreExecuteBlock(statedb *state.StateDB)
	ApplyTransaction(config *params.ChainConfig, author *common.Address, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64, cfg *vm.Config) (*types.Receipt, *vm.InternalTxTrace, error)
}

// BlockExecutionConfig configures a BlockExecution.
type BlockExecutionConfig struct {
	// Author is the proposer of the block, who receives the fees.
	Author common.Address
	// BlockHash tags the logs. It is zero for a block being built.
	BlockHash common.Hash
	// VMConfig is the EVM configuration, the default one if nil.
	VMConfig *vm.Config
	// UsedGas is the gas counter of the block, an internal one if nil.
	UsedGas *uint64
	// ForWrite tells that the block is executed to be written, so the
	// execution state modules see its execution. It is unset when a block is
	// executed again to read its states, like to trace it.
	ForWrite bool
}

// BlockExecution executes one block on its parent state in three steps:
// BeginBlockExecution runs the changes before the txs, ApplyTx runs one tx,
// and Finish runs the changes after the txs. Importing blocks, validating
// proposals and building blocks all go through it, so a block is executed the
// same way on every path. A proposer picks each tx by the results of the
// previous ones; the others run the txs of the block with ApplyTxs.
type BlockExecution struct {
	chain     ExecutionChain
	config    *params.ChainConfig
	header    *types.Header
	statedb   *state.StateDB
	author    common.Address
	blockHash common.Hash
	vmConfig  *vm.Config
	usedGas   *uint64
}

// BeginBlockExecution starts executing the block of header on statedb, its
// parent state, and runs the changes before the txs.
func BeginBlockExecution(chain ExecutionChain, config *params.ChainConfig, header *types.Header, statedb *state.StateDB, cfg BlockExecutionConfig) (*BlockExecution, error) {
	e := &BlockExecution{
		chain:     chain,
		config:    config,
		header:    header,
		statedb:   statedb,
		author:    cfg.Author,
		blockHash: cfg.BlockHash,
		vmConfig:  cfg.VMConfig,
		usedGas:   cfg.UsedGas,
	}
	if e.vmConfig == nil {
		e.vmConfig = &vm.Config{}
	}
	if e.usedGas == nil {
		e.usedGas = new(uint64)
	}
	if cfg.ForWrite {
		chain.PreExecuteBlock(statedb)
	}
	if err := chain.Processor().InitializeState(header, statedb); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInitializeState, err)
	}
	return e, nil
}

// WithVMConfig returns the execution with its txs run with cfg.
func (e *BlockExecution) WithVMConfig(cfg *vm.Config) *BlockExecution {
	c := *e
	c.vmConfig = cfg
	return &c
}

// ApplyTx runs tx as the index-th tx of the block. If it fails, the tx is not
// part of the block, and reverting its changes is left to the caller.
func (e *BlockExecution) ApplyTx(index int, tx *types.Transaction) (*types.Receipt, *vm.InternalTxTrace, error) {
	e.statedb.SetTxContext(tx.Hash(), e.blockHash, index)
	return e.chain.ApplyTransaction(e.config, &e.author, e.statedb, e.header, tx, e.usedGas, e.vmConfig)
}

// ApplyTxs runs the txs of a block in order. A tx that fails fails the block.
func (e *BlockExecution) ApplyTxs(txs types.Transactions) (types.Receipts, []*types.Log, []*vm.InternalTxTrace, error) {
	var (
		receipts types.Receipts
		logs     []*types.Log
		traces   []*vm.InternalTxTrace
	)
	for i, tx := range txs {
		receipt, trace, err := e.ApplyTx(i, tx)
		if err != nil {
			return nil, nil, nil, err
		}
		receipts = append(receipts, receipt)
		logs = append(logs, receipt.Logs...)
		traces = append(traces, trace)
	}
	return receipts, logs, traces, nil
}

// UsedGas returns the gas used by the txs run so far.
func (e *BlockExecution) UsedGas() uint64 {
	return *e.usedGas
}

// Finish runs the changes after the txs and assembles the block.
func (e *BlockExecution) Finish(txs []*types.Transaction, receipts types.Receipts) (*types.Block, error) {
	return e.chain.Processor().FinalizeState(e.header, e.statedb, txs, receipts)
}

// processorChain is a chain whose blocks are run by another processor.
type processorChain struct {
	*BlockChain
	processor Processor
}

func (c processorChain) Processor() Processor { return c.processor }

// executeBlock runs block on statedb, its parent state, with processor. With
// forWrite the block is to be written, so the execution state modules see its
// execution.
func executeBlock(bc *BlockChain, processor Processor, block *types.Block, statedb *state.StateDB, cfg vm.Config, forWrite bool) (types.Receipts, []*types.Log, uint64, []*vm.InternalTxTrace, ProcessStats, error) {
	var stats ProcessStats
	header := block.Header()
	author, _ := bc.sealer.Author(header) // Ignore error, we're past header validation
	exec, err := BeginBlockExecution(processorChain{bc, processor}, bc.chainConfig, header, statedb, BlockExecutionConfig{
		Author:    author,
		BlockHash: block.Hash(),
		VMConfig:  &cfg,
		ForWrite:  forWrite,
	})
	if err != nil {
		return nil, nil, 0, nil, stats, err
	}

	stats.BeforeApplyTxs = time.Now()
	receipts, logs, traces, err := exec.ApplyTxs(block.Transactions())
	if err != nil {
		return nil, nil, 0, nil, stats, err
	}
	stats.AfterApplyTxs = time.Now()

	if _, err := exec.Finish(block.Transactions(), receipts); err != nil {
		return nil, nil, 0, nil, stats, err
	}
	stats.AfterFinalize = time.Now()
	return receipts, logs, exec.UsedGas(), traces, stats, nil
}
