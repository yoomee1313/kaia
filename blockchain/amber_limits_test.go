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

	"github.com/kaiachain/kaia/params"
	"github.com/stretchr/testify/require"
)

func TestAmberLimitChecks(t *testing.T) {
	config := params.TestKaiaConfig("prague")
	config.AmberCompatibleBlock = big.NewInt(params.HeaderSegmentSize)
	before, at := big.NewInt(params.HeaderSegmentSize-1), big.NewInt(params.HeaderSegmentSize)

	// Defaults.
	require.Equal(t, uint64(params.DefaultAmberBlockComputationCostLimit), config.BlockComputationCostLimit())
	require.Equal(t, uint64(params.DefaultAmberBlockAccessListEntryLimit), config.BlockAccessListEntryLimit())

	config.AmberBlockComputationCostLimit = 1000
	config.AmberBlockAccessListEntryLimit = 50

	require.NoError(t, CheckBlockComputationCost(config, at, 1000), "exactly the limit")
	require.ErrorIs(t, CheckBlockComputationCost(config, at, 1001), ErrBlockComputationCostLimit)
	require.NoError(t, CheckBlockComputationCost(config, before, 1001), "no limit before Amber")

	require.NoError(t, CheckBlockAccessListEntries(config, at, 50), "exactly the limit")
	require.ErrorIs(t, CheckBlockAccessListEntries(config, at, 51), ErrBlockAccessListEntryLimit)
	require.NoError(t, CheckBlockAccessListEntries(config, before, 51), "no limit before Amber")

	require.NoError(t, CheckProposalSize(config, at, params.MaxProposalSize-100, 100), "exactly the limit")
	require.ErrorIs(t, CheckProposalSize(config, at, params.MaxProposalSize-100, 101), ErrProposalSizeLimit)
	require.NoError(t, CheckProposalSize(config, before, params.MaxProposalSize, 1), "no limit before Amber")

	config.AmberCompatibleBlock = nil
	require.NoError(t, CheckBlockComputationCost(config, at, 1001), "no Amber")
}
