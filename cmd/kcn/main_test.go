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

	bal_impl "github.com/kaiachain/kaia/kaiax/bal/impl"
	"github.com/stretchr/testify/require"
)

func TestCheckHistoryMode(t *testing.T) {
	require.NoError(t, checkHistoryMode(defaultHistoryMode))
	require.NoError(t, checkHistoryMode(bal_impl.HistoryModeHistory))
	require.NoError(t, checkHistoryMode(bal_impl.HistoryModeLight))
	require.ErrorContains(t, checkHistoryMode(bal_impl.HistoryModeFull), "a consensus node runs in the history or light mode, not --history.mode full")
}
