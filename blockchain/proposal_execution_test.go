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
	"testing"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/stretchr/testify/require"
)

// ExecuteProposal tells a valid, an invalid and an unexecutable proposal apart
// and records no bad block.
func TestExecuteProposal(t *testing.T) {
	env := newAmberTestEnv(t)
	blocks, _ := GenerateChain(env.config, env.genesis, env.sealer, env.db, 2, amberTestTxs(env))
	chain := newImportChain(t, env)

	require.NoError(t, chain.ExecuteProposal(blocks[0]))

	header := types.CopyHeader(blocks[0].Header())
	header.Root = common.Hash{1}
	invalid := blocks[0].WithSeal(header)
	err := chain.ExecuteProposal(invalid)
	require.ErrorContains(t, err, "invalid merkle root")
	require.NotErrorIs(t, err, ErrProposalExecutionLocal)
	require.False(t, chain.HasBadBlock(invalid.Hash()))

	// The parent of block 2 is not inserted.
	err = chain.ExecuteProposal(blocks[1])
	require.ErrorIs(t, err, ErrProposalExecutionLocal)
	require.False(t, chain.HasBadBlock(blocks[1].Hash()))
}
