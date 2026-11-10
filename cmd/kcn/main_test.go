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

package main

import (
	"testing"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/stretchr/testify/require"
)

func TestCheckHistoryMode(t *testing.T) {
	require.NoError(t, checkHistoryMode(defaultHistoryMode))
	require.NoError(t, checkHistoryMode(bal.HistoryModeHistory))
	require.NoError(t, checkHistoryMode(bal.HistoryModeLight))
	require.ErrorContains(t, checkHistoryMode(bal.HistoryModeFull), "a consensus node runs in the history or light mode, not --history.mode full")
}
