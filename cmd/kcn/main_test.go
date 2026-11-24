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

	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

func TestCheckGCMode(t *testing.T) {
	require.NoError(t, checkGCMode(defaultGCMode))
	require.NoError(t, checkGCMode(string(database.HistoryModeBAL)))
	require.ErrorContains(t, checkGCMode("full"), "a consensus node runs with --gcmode history, not full")
	require.ErrorContains(t, checkGCMode("archive"), "not archive")
}
