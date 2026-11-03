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
	"bytes"
	"fmt"
)

// segIter walks the entries of one segment in order, reading its blocks
// directly instead of through the cache.
type segIter struct {
	seg *segment
	t   int   // index block in the top index
	ib  block // index block t
	i   int   // data block in ib
	db  block // data block i
	j   int   // entry in db
	err error
}

func (it *segIter) next() bool {
	for it.err == nil {
		switch {
		case it.j+1 < len(it.db):
			it.j++
			return true
		case it.i+1 < len(it.ib):
			it.i++
			var h handle
			if h, _, it.err = decodeHandle(it.ib[it.i].value); it.err == nil {
				it.db, it.err = it.seg.readBlock(h)
				it.j = -1
			}
		case it.t+1 < len(it.seg.top):
			it.t++
			it.ib, it.err = it.seg.readBlock(it.seg.top[it.t].h)
			it.i = -1
			it.db, it.j = nil, -1
		default:
			return false
		}
	}
	return false
}

func (it *segIter) key() []byte   { return it.db[it.j].key }
func (it *segIter) value() []byte { return it.db[it.j].value }

// Iterator walks the entries that Get sees in a segment, in key order: the
// entries of the segment and of its bases, the newest value of each key.
type Iterator struct {
	iters   []*segIter // the segment first
	valid   []bool
	started bool
	key     []byte
	value   []byte
	err     error
}

// NewIterator returns an iterator over segment id and its bases.
func (s *Store) NewIterator(id uint64) (*Iterator, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	segs, err := s.chain(id)
	if err != nil {
		return nil, err
	}
	it := &Iterator{iters: make([]*segIter, len(segs)), valid: make([]bool, len(segs))}
	for k, seg := range segs {
		it.iters[k] = &segIter{seg: seg, t: -1, i: -1, j: -1}
	}
	return it, nil
}

// Next moves to the next key. It returns false at the end or on an error.
func (it *Iterator) Next() bool {
	if it.err != nil {
		return false
	}
	for k, sub := range it.iters {
		if !it.started || (it.valid[k] && bytes.Equal(sub.key(), it.key)) {
			it.valid[k] = sub.next()
			if sub.err != nil {
				it.err = fmt.Errorf("segment %d: %w", sub.seg.meta.ID, sub.err)
				return false
			}
		}
	}
	it.started = true
	best := -1
	for k, sub := range it.iters {
		if it.valid[k] && (best < 0 || bytes.Compare(sub.key(), it.iters[best].key()) < 0) {
			best = k
		}
	}
	if best < 0 {
		it.key, it.value = nil, nil
		return false
	}
	it.key, it.value = it.iters[best].key(), it.iters[best].value()
	return true
}

// Key returns the current key. The caller must not modify it.
func (it *Iterator) Key() []byte { return it.key }

// Value returns the current value. The caller must not modify it.
func (it *Iterator) Value() []byte { return it.value }

// Error returns the error that stopped the iterator, if any.
func (it *Iterator) Error() error { return it.err }

// Compact rewrites segment id with every entry of its chain, so that it no
// longer has a base. The id and the rest of the meta stay, and lookups give
// the same values; the bases are kept, as they are checkpoints of their own,
// and can be dropped with Remove once nothing is based on them.
func (s *Store) Compact(id uint64) error {
	if s.opts.ReadOnly {
		return ErrReadOnly
	}
	s.compactMu.Lock()
	defer s.compactMu.Unlock()

	meta, ok := s.Meta(id)
	if !ok {
		return fmt.Errorf("%w: %d", ErrUnknownSegment, id)
	}
	if meta.Base == 0 {
		return nil
	}
	it, err := s.NewIterator(id)
	if err != nil {
		return err
	}
	meta.Base = 0
	w, err := newWriter(s.dir, meta, s.opts, true, s.register)
	if err != nil {
		return err
	}
	for it.Next() {
		if err := w.Put(it.Key(), it.Value()); err != nil {
			w.Abort()
			return err
		}
	}
	if err := it.Error(); err != nil {
		w.Abort()
		return err
	}
	_, err = w.Finish()
	return err
}
