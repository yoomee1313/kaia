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

package bal

import (
	"fmt"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
)

// Block is a block with the data to insert it without executing it: its
// receipts, of which only the consensus fields are used, and the stored
// writes of its block access list.
type Block struct {
	Block    *types.Block
	Receipts types.Receipts
	BAL      []byte
}

// InsertChain inserts blocks like BlockChain.InsertChain, but applies their
// block access lists to the parent states instead of executing them. Their
// headers, seals included, are verified as usual, and their receipts, gas,
// bloom and state roots are checked against the headers, so a wrong list or
// receipt is rejected like a wrong execution.
//
// A light node checks and commits the state root only at the blocks the trie
// is committed at and at the last block, see the batch of package blockchain. A wrong
// root there rolls the chain back to the last checked block.
func InsertChain(bc *blockchain.BlockChain, blocks []*Block) (int, error) {
	c := of(bc)
	chain := make(types.Blocks, len(blocks))
	for i, b := range blocks {
		chain[i] = b.Block
		c.inserted.Store(b.Block.Hash(), b)
	}
	defer func() {
		for _, b := range blocks {
			c.inserted.Delete(b.Block.Hash())
		}
	}()
	return bc.InsertChain(chain)
}

func (c *chain) ApplyBlock(block *types.Block, statedb *state.StateDB, check func(types.Receipts, uint64) error) (types.Receipts, []*types.Log, uint64, error) {
	v, _ := c.inserted.Load(block.Hash())
	b := v.(*Block)
	list, err := access.Decode(b.BAL)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("block access list of block %d: %w", block.NumberU64(), err)
	}
	// From the Amber hardfork the header commits the hash of the list, which
	// BlockValidator.ValidateState does not check for a block inserted with it.
	if c.bc.Config().IsAmberForkEnabled(block.Number()) {
		hash, err := list.Hash()
		if err != nil {
			return nil, nil, 0, fmt.Errorf("block access list of block %d: %w", block.NumberU64(), err)
		}
		if want := block.Header().BlockAccessListHash; want == nil || *want != hash {
			return nil, nil, 0, fmt.Errorf("block access list of block %d does not match the hash in its header", block.NumberU64())
		}
	}
	receipts := make(types.Receipts, len(b.Receipts))
	for i, r := range b.Receipts {
		logs := make([]*types.Log, len(r.Logs))
		for j, l := range r.Logs {
			cpy := *l
			logs[j] = &cpy
		}
		receipts[i] = &types.Receipt{Status: r.Status, GasUsed: r.GasUsed, Logs: logs}
		receipts[i].Bloom = types.CreateBloom(types.Receipts{receipts[i]})
	}
	if err := blockchain.SetReceiptsData(c.bc.Config(), block, receipts); err != nil {
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
	if check != nil {
		if err := check(receipts, usedGas); err != nil {
			return nil, nil, 0, err
		}
	}
	if check != nil {
		err = access.ApplyUnchecked(statedb, list)
	} else {
		err = access.Apply(statedb, list)
	}
	if err != nil {
		return nil, nil, 0, fmt.Errorf("block access list of block %d: %w", block.NumberU64(), err)
	}
	return receipts, logs, usedGas, nil
}

// Record stores the list of an inserted block: the one it was inserted with,
// or the one recorded while it was executed.
func (c *chain) Record(block *types.Block, parentRoot common.Hash, statedb *state.StateDB) {
	if c.rec == nil {
		return
	}
	v, ok := c.inserted.Load(block.Hash())
	if !ok {
		c.rec.record(c, block, parentRoot, statedb)
		return
	}
	n := block.NumberU64()
	if err := c.rec.store(n, block.Hash(), v.(*Block).BAL, nil, n); err != nil {
		logger.Error("Failed to write block access list", "number", n, "err", err)
		return
	}
	c.signalIndex()
}

// ByHash returns the stored writes of the block access list of a block.
func ByHash(bc *blockchain.BlockChain, hash common.Hash, number uint64) []byte {
	return of(bc).db.ReadBAL(hash, number)
}

// Range returns the blocks whose lists are all stored, first to next-1.
func Range(bc *blockchain.BlockChain) (first, next uint64) { return of(bc).db.ReadBALRange() }
