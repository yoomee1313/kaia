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

// processBlock executes a block on its parent state, opened ForExec, or with
// receipts applies the block access list it is inserted with on its parent
// state opened ForApply.
func (bc *BlockChain) processBlock(block *types.Block, receipts types.Receipts, statedb *state.StateDB) (types.Receipts, []*types.Log, uint64, []*vm.InternalTxTrace, ProcessStats, error) {
	if block.BAL() == nil {
		return bc.processor.Process(block, statedb, bc.vmConfig)
	}
	stats := ProcessStats{BeforeApplyTxs: time.Now()}
	receipts, logs, usedGas, err := bc.applyBALBlock(block, receipts, statedb)
	if err != nil {
		return nil, nil, 0, nil, stats, err
	}
	stats.AfterApplyTxs, stats.AfterFinalize = time.Now(), time.Now()
	return receipts, logs, usedGas, nil, stats, nil
}

// applyBALBlock returns the receipts, logs and gas of block, rebuilt from the
// consensus fields of given, and applies its block access list to statedb,
// its parent state. On the state of the batch, the list is applied without
// hashing once the receipts check.
func (bc *BlockChain) applyBALBlock(block *types.Block, given types.Receipts, statedb *state.StateDB) (types.Receipts, []*types.Log, uint64, error) {
	receipts := make(types.Receipts, len(given))
	for i, r := range given {
		logs := make([]*types.Log, len(r.Logs))
		for j, l := range r.Logs {
			cpy := *l
			logs[j] = &cpy
		}
		receipts[i] = &types.Receipt{Status: r.Status, GasUsed: r.GasUsed, Logs: logs}
		receipts[i].Bloom = types.CreateBloom(types.Receipts{receipts[i]})
	}
	if err := SetReceiptsData(bc.chainConfig, block, receipts); err != nil {
		return nil, nil, 0, fmt.Errorf("receipts of block %d: %w", block.NumberU64(), err)
	}
	var (
		logs    []*types.Log
		usedGas uint64
	)
	for _, r := range receipts {
		logs = append(logs, r.Logs...)
		usedGas += r.GasUsed
	}
	list, err := types.DecodeBAL(block.BAL())
	if err == nil {
		err = bc.states.ApplyBAL(statedb, list, func() error { return validateReceipts(block, receipts, usedGas) })
	}
	if err != nil {
		return nil, nil, 0, fmt.Errorf("block access list of block %d: %w", block.NumberU64(), err)
	}
	return receipts, logs, usedGas, nil
}
