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

	"github.com/kaiachain/kaia/params"
)

var (
	// ErrBlockComputationCostLimit is returned when the txs of a block from the
	// Amber hardfork use more opcode computation cost than the chain config
	// allows. A block producer leaves out the tx that crosses the limit.
	ErrBlockComputationCostLimit = errors.New("block computation cost limit exceeded")
	// ErrBlockAccessListEntryLimit is returned when the block access list of a
	// block from the Amber hardfork has more entries than the chain config
	// allows.
	ErrBlockAccessListEntryLimit = errors.New("block access list entry limit exceeded")
	// ErrProposalSizeLimit is returned when the RLP of a block from the Amber
	// hardfork and of its block access list exceed params.MaxProposalSize
	// together. A block producer leaves out the tx that would cross it.
	ErrProposalSizeLimit = errors.New("block and block access list size limit exceeded")
)

// CheckBlockComputationCost checks the sum of the opcode computation costs of
// the txs of a block against the Amber limit. Blocks before Amber have none.
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
// of a block, see access.BlockAccessList.WriteEntryCount, against the Amber
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

// CheckProposalSize checks the RLP sizes of a block and of its block access
// list, reads included, against params.MaxProposalSize. Blocks before Amber
// have no list. Like the entry limit, only consensus nodes can check it.
func CheckProposalSize(config *params.ChainConfig, number *big.Int, blockSize, listSize uint64) error {
	if !config.IsAmberForkEnabled(number) {
		return nil
	}
	if size := blockSize + listSize; size > params.MaxProposalSize {
		return fmt.Errorf("%w: %d > %d", ErrProposalSizeLimit, size, params.MaxProposalSize)
	}
	return nil
}
