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
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/params"
	"github.com/stretchr/testify/require"
)

// newAmberTestEnv returns an env whose Amber hardfork starts at block 2.
func newAmberTestEnv(t *testing.T) *balTestEnv {
	env := newBALTestEnv(t, "prague", 2)
	env.config.AmberCompatibleBlock = big.NewInt(2)
	return env
}

func amberTestTxs(env *balTestEnv) func(int, *BlockGen) {
	return func(i int, b *BlockGen) {
		b.AddTx(env.legacyTx(b, 0, &env.addrs[1], big.NewInt(params.KAIA), nil))
		b.AddTx(env.legacyTx(b, 1, &balStoreAddr, common.Big0, balSlotData(byte(i), byte(i+1))))
	}
}

// TestAmberBlockAccessListHash checks that blocks from the Amber hardfork commit
// the hash of the changes of their block access list and are accepted.
func TestAmberBlockAccessListHash(t *testing.T) {
	env := newAmberTestEnv(t)
	chain, blocks := env.generate(4, amberTestTxs(env))
	lists := checkBAL(t, chain, blocks)

	for i, block := range blocks {
		if !env.config.IsAmberForkEnabled(block.Number()) {
			require.Nil(t, block.Header().BlockAccessListHash, "block %d", block.NumberU64())
			continue
		}
		require.NotNil(t, block.Header().BlockAccessListHash, "block %d", block.NumberU64())
		hash, err := lists[i].ChangesHash()
		require.NoError(t, err)
		require.Equal(t, hash, *block.Header().BlockAccessListHash, "block %d", block.NumberU64())
	}
	require.Equal(t, blocks[len(blocks)-1].Hash(), chain.CurrentBlock().Hash())
}

// TestAmberBlockAccessListHashInvalid checks that a block with a wrong, missing
// or premature block access list hash is rejected.
func TestAmberBlockAccessListHashInvalid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		index  int // block to tamper with
		hash   func(h *common.Hash) *common.Hash
		errMsg string
	}{
		{"wrong", 2, func(h *common.Hash) *common.Hash { w := *h; w[0] ^= 1; return &w }, "invalid block access list hash"},
		{"missing", 2, func(*common.Hash) *common.Hash { return nil }, "missing block access list hash"},
		{"premature", 0, func(*common.Hash) *common.Hash { return &common.Hash{1} }, "block access list hash before the Amber hardfork"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newAmberTestEnv(t)
			blocks, _ := GenerateChain(env.config, env.genesis, env.sealer, env.db, 3, amberTestTxs(env))
			chain, err := NewBlockChain(env.db, nil, env.config, env.sealer, vm.Config{})
			require.NoError(t, err)
			defer chain.Stop()
			chain.Processor().RegisterBlockStateModule(env.sealer)

			_, err = chain.InsertChain(blocks[:tc.index])
			require.NoError(t, err)
			header := types.CopyHeader(blocks[tc.index].Header())
			header.BlockAccessListHash = tc.hash(header.BlockAccessListHash)
			tampered := blocks[tc.index].WithSeal(header)
			_, err = chain.InsertChain(types.Blocks{tampered})
			require.ErrorContains(t, err, tc.errMsg)
		})
	}
}
