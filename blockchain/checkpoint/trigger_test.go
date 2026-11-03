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

package checkpoint

import (
	"math/big"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/storage/database/checkpoint"
	"github.com/stretchr/testify/require"
)

func header(number uint64, at time.Time) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(number), Time: big.NewInt(at.Unix())}
}

func TestTrigger(t *testing.T) {
	kst := time.FixedZone("KST", 9*3600)
	boundary := time.Date(2026, 12, 28, 0, 0, 0, 0, kst)

	tr, err := ParseTrigger("12-28T00:00+09:00", 0)
	require.NoError(t, err)
	tests := []struct {
		parent, cur time.Time
		want        bool
	}{
		{boundary.Add(-2 * time.Second), boundary.Add(-time.Second), false},
		{boundary.Add(-time.Second), boundary, true},                  // exactly at the time
		{boundary.Add(-time.Second), boundary.Add(time.Second), true}, // first block after it
		{boundary, boundary.Add(time.Second), false},                  // already passed
		{boundary.AddDate(0, 0, 5), boundary.AddDate(0, 0, 5).Add(time.Second), false},
		{boundary.AddDate(1, 0, 0).Add(-time.Second), boundary.AddDate(1, 0, 0), true},
		// A gap over the new year still finds the boundary of the earlier year.
		{boundary.Add(-time.Hour), boundary.AddDate(0, 0, 10), true},
	}
	for i, tt := range tests {
		kind, ok := tr.Match(header(10, tt.parent), header(11, tt.cur))
		require.Equal(t, tt.want, ok, "case %d", i)
		if ok {
			require.Equal(t, checkpoint.TriggerTime, kind)
		}
	}
	// The same instant in UTC is 12-27T15:00Z.
	utc, err := ParseTrigger("12-27T15:00Z", 0)
	require.NoError(t, err)
	_, ok := utc.Match(header(10, boundary.Add(-time.Second)), header(11, boundary))
	require.True(t, ok)

	iv, err := ParseTrigger("", 1000)
	require.NoError(t, err)
	kind, ok := iv.Match(header(1999, boundary), header(2000, boundary.Add(time.Second)))
	require.True(t, ok)
	require.Equal(t, checkpoint.TriggerInterval, kind)
	_, ok = iv.Match(header(2000, boundary), header(2001, boundary.Add(time.Second)))
	require.False(t, ok)

	both, err := ParseTrigger("12-28T00:00+09:00", 1000)
	require.NoError(t, err)
	kind, ok = both.Match(header(1999, boundary.Add(-time.Second)), header(2000, boundary))
	require.True(t, ok)
	require.Equal(t, checkpoint.TriggerTime, kind)

	off, err := ParseTrigger("", 0)
	require.NoError(t, err)
	require.False(t, off.Enabled())
	_, err = ParseTrigger("28-12", 0)
	require.Error(t, err)
}
