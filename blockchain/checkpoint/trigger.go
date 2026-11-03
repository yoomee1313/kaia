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
	"fmt"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/storage/database/checkpoint"
)

// Trigger decides which blocks get a checkpoint: the first block whose
// timestamp reaches a yearly wall-clock time, every block whose number is a
// multiple of an interval, or both. It only reads headers, so every node picks
// the same blocks.
type Trigger struct {
	yearly   *time.Time // month, day, hour, minute and zone of the yearly time
	interval uint64
}

// ParseTrigger parses the yearly time as "MM-DDTHH:MM" followed by a zone
// offset ("Z" or "+09:00"), and takes the block interval. Empty and 0 disable
// either.
func ParseTrigger(yearly string, interval uint64) (*Trigger, error) {
	t := &Trigger{interval: interval}
	if yearly != "" {
		// 2000 is a leap year, so 02-29 parses; time.Date moves it to Mar 1 in other years.
		v, err := time.Parse("2006-01-02T15:04Z07:00", "2000-"+yearly)
		if err != nil {
			return nil, fmt.Errorf("checkpoint time %q: want MM-DDTHH:MM with a zone, like 12-28T00:00+09:00: %w", yearly, err)
		}
		t.yearly = &v
	}
	return t, nil
}

// Enabled reports whether the trigger picks any block.
func (t *Trigger) Enabled() bool {
	return t != nil && (t.yearly != nil || t.interval > 0)
}

func (t *Trigger) boundary(year int) int64 {
	y := t.yearly
	return time.Date(year, y.Month(), y.Day(), y.Hour(), y.Minute(), 0, 0, y.Location()).Unix()
}

// Match returns whether header gets a checkpoint, and why. A yearly time
// matches the first block at or after it: parent is before it.
func (t *Trigger) Match(parent, header *types.Header) (checkpoint.Trigger, bool) {
	if !t.Enabled() {
		return "", false
	}
	if t.yearly != nil && parent != nil {
		prev, cur := parent.Time.Int64(), header.Time.Int64()
		loc := t.yearly.Location()
		for year := time.Unix(prev, 0).In(loc).Year(); year <= time.Unix(cur, 0).In(loc).Year(); year++ {
			if b := t.boundary(year); prev < b && b <= cur {
				return checkpoint.TriggerTime, true
			}
		}
	}
	if t.interval > 0 && header.Number.Uint64()%t.interval == 0 {
		return checkpoint.TriggerInterval, true
	}
	return "", false
}
