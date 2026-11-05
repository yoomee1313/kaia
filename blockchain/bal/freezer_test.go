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
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/stretchr/testify/require"
)

func TestFreezerFirst(t *testing.T) {
	list := func(n byte) []byte {
		enc, err := (&BlockAccessList{Accounts: []AccountAccess{{Address: common.Address{n}}}}).Encode()
		require.NoError(t, err)
		return enc
	}

	// From genesis: block 0 is an empty list, block 1 is next.
	dir := t.TempDir()
	f, err := OpenFreezer(dir, false)
	require.NoError(t, err)
	require.Equal(t, uint64(0), f.First())
	require.Equal(t, uint64(1), f.Next())
	require.NoError(t, f.Append(1, list(1)))
	require.NoError(t, f.Close())
	_, err = OpenFreezerAt(dir, 5)
	require.Error(t, err, "a genesis freezer cannot start later")

	// From block 100: no genesis list, items are blocks 100, 101, ...
	dir = t.TempDir()
	f, err = OpenFreezerAt(dir, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(100), f.First())
	require.Equal(t, uint64(100), f.Next())
	require.ErrorIs(t, f.Append(101, list(1)), ErrFreezerGap)
	require.NoError(t, f.Append(100, list(100)))
	require.NoError(t, f.Append(101, list(101)))
	_, err = f.ReadRaw(99)
	require.ErrorIs(t, err, ErrBeforeFirst)
	require.NoError(t, f.Close())

	f, err = OpenFreezer(dir, true)
	require.NoError(t, err)
	require.Equal(t, uint64(100), f.First())
	require.Equal(t, uint64(102), f.Next())
	got, err := f.Read(101)
	require.NoError(t, err)
	require.Equal(t, common.Address{101}, got.Accounts[0].Address)
	encs, err := f.ReadRawRange(100, 2, 0)
	require.NoError(t, err)
	require.Equal(t, [][]byte{list(100), list(101)}, encs)
	require.NoError(t, f.Close())

	_, err = OpenFreezerAt(dir, 50)
	require.Error(t, err)
	f, err = OpenFreezerAt(dir, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(102), f.Next())
	require.NoError(t, f.Close())
}
