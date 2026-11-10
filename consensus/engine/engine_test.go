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

package engine

import (
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/params"
	"github.com/stretchr/testify/require"
)

func TestCheckEngineAmber(t *testing.T) {
	amber := &params.ChainConfig{AmberCompatibleBlock: big.NewInt(100)}
	require.NoError(t, CheckEngine(KaiaBFT, amber))
	require.ErrorIs(t, CheckEngine(Istanbul, amber), ErrIstanbulWithAmber)
	require.ErrorIs(t, CheckEngine("", amber), ErrIstanbulWithAmber, "an unset type selects Istanbul")
	require.NoError(t, CheckEngine(Istanbul, &params.ChainConfig{}))
	require.NoError(t, CheckEngine(KaiaBFT, &params.ChainConfig{}))
}
