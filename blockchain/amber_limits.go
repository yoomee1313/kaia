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
	"errors"
	"fmt"
	"math/big"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/params"
)

var (
	// ErrBlockComputationCostLimit is returned when the txs of a block from the
	// Amber hardfork use more opcode computation cost than the chain config
	// allows. It is a rule of the block, checked by every node that executes
	// it. A block producer leaves out the tx that crosses the limit.
	ErrBlockComputationCostLimit = errors.New("block computation cost limit exceeded")
	// ErrBlockAccessListEntryLimit is returned when the block access list of a
	// block from the Amber hardfork has more entries than the chain config
	// allows.
	ErrBlockAccessListEntryLimit = errors.New("block access list entry limit exceeded")
	// ErrProposalSizeLimit is returned when the txs of an Amber proposal and its
	// block access list hint exceed params.MaxProposalSize together. A block
	// producer leaves out the tx that would cross it.
	ErrProposalSizeLimit = errors.New("txs and block access list hint size limit exceeded")
)

// CheckBlockComputationCost checks the sum of the opcode computation costs of
// the txs of a block against the Amber limit. Blocks before Amber have none.
// It is a rule of the block: every node that executes the block checks it.
func CheckBlockComputationCost(config *params.ChainConfig, number *big.Int, cost uint64) error {
	if !config.IsAmberForkEnabled(number) {
		return nil
	}
	if limit := config.BlockComputationCostLimit(); cost > limit {
		return fmt.Errorf("%w: %d > %d", ErrBlockComputationCostLimit, cost, limit)
	}
	return nil
}

// CheckBlockAccessListEntries checks the write entries of the block access list
// of a block, see bal.BlockAccessList.WriteEntryCount, against the Amber
// limit. Blocks before Amber have none. It is a rule of the block: the writes
// are what the header commits, so every node that has them can check it.
func CheckBlockAccessListEntries(config *params.ChainConfig, number *big.Int, entries uint64) error {
	if !config.IsAmberForkEnabled(number) {
		return nil
	}
	if limit := config.BlockAccessListEntryLimit(); entries > limit {
		return fmt.Errorf("%w: %d > %d", ErrBlockAccessListEntryLimit, entries, limit)
	}
	return nil
}

// CheckProposalSize checks the RLP sizes of the txs of an Amber proposal and of
// its block access list hint against params.MaxProposalSize. The hint is
// counted once: a proposal sent in fragments carries the hint of the whole
// block in its final message, and the hints of its fragments, which hold the
// same entries, are bounded by the bytes a node keeps for a view. Blocks before
// Amber have no hint. Only consensus nodes can check it, as the hint is not
// part of the block.
func CheckProposalSize(config *params.ChainConfig, number *big.Int, txsSize, hintSize uint64) error {
	if !config.IsAmberForkEnabled(number) {
		return nil
	}
	if size := txsSize + hintSize; size > params.MaxProposalSize {
		return fmt.Errorf("%w: %d > %d", ErrProposalSizeLimit, size, params.MaxProposalSize)
	}
	return nil
}

// TxsSize returns the sum of the RLP sizes of txs, which the proposal size
// limit counts.
func TxsSize(txs types.Transactions) uint64 {
	var size uint64
	for _, tx := range txs {
		size += uint64(tx.Size())
	}
	return size
}
