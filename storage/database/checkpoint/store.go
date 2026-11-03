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
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"

	lru "github.com/hashicorp/golang-lru"
)

// segment is an open segment file.
type segment struct {
	meta Meta
	path string
	f    *os.File
	top  []topEntry
	size int64
	// dataEnd is where the blocks end and the top index starts.
	dataEnd uint64
	// lastEnd is the end of the last data read, to detect sorted lookups.
	lastEnd atomic.Uint64
}

func openSegment(path string) (*segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	seg, err := loadSegment(f, path)
	if err != nil {
		f.Close()
		return nil, err
	}
	return seg, nil
}

func loadSegment(f *os.File, path string) (*segment, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size < footerSize {
		return nil, fmt.Errorf("%w: file too short", ErrCorrupt)
	}
	buf := make([]byte, footerSize)
	if _, err := f.ReadAt(buf, size-footerSize); err != nil {
		return nil, err
	}
	ft, err := decodeFooter(buf)
	if err != nil {
		return nil, err
	}
	if ft.topOffset+uint64(ft.topLength)+uint64(ft.metaLen)+footerSize != uint64(size) {
		return nil, fmt.Errorf("%w: footer does not match the file size", ErrCorrupt)
	}
	tail := make([]byte, int(ft.topLength)+int(ft.metaLen))
	if _, err := f.ReadAt(tail, int64(ft.topOffset)); err != nil {
		return nil, err
	}
	if crc32.Checksum(tail, castagnoli) != ft.crc {
		return nil, fmt.Errorf("%w: top index or meta fails its checksum", ErrCorrupt)
	}
	top, err := decodeTop(tail[:ft.topLength])
	if err != nil {
		return nil, err
	}
	for _, t := range top {
		if t.h.end() > ft.topOffset {
			return nil, fmt.Errorf("%w: index block past the blocks", ErrCorrupt)
		}
	}
	seg := &segment{path: path, f: f, top: top, size: size, dataEnd: ft.topOffset}
	if err := json.Unmarshal(tail[ft.topLength:], &seg.meta); err != nil {
		return nil, fmt.Errorf("%w: meta: %v", ErrCorrupt, err)
	}
	return seg, nil
}

func (seg *segment) read(from, to uint64) ([]byte, error) {
	if to > seg.dataEnd || from > to {
		return nil, fmt.Errorf("%w: block past the blocks", ErrCorrupt)
	}
	buf := make([]byte, to-from)
	if _, err := seg.f.ReadAt(buf, int64(from)); err != nil {
		return nil, err
	}
	return buf, nil
}

func (seg *segment) readBlock(h handle) (block, error) {
	raw, err := seg.read(h.offset, h.end())
	if err != nil {
		return nil, err
	}
	return decodeBlock(raw, h)
}

// Store is a directory of segments.
type Store struct {
	dir   string
	opts  Options
	cache *lru.Cache

	mu       sync.RWMutex
	segments map[uint64]*segment
	// retired are segments replaced by compaction. They stay open for the
	// iterators that may still read them.
	retired []*segment
	closed  bool

	compactMu sync.Mutex
}

type cacheKey struct {
	seg    *segment
	offset uint64
}

// Open opens the segments in dir and checks that every base is there. Unless
// read-only, it creates dir if missing and removes unfinished segments.
func Open(dir string, opts *Options) (*Store, error) {
	o := opts.withDefaults()
	if !o.ReadOnly {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	cache, err := lru.New(o.CacheBlocks)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, opts: o, cache: cache, segments: make(map[uint64]*segment)}
	for _, file := range files {
		name, path := file.Name(), filepath.Join(dir, file.Name())
		switch {
		case strings.HasSuffix(name, segmentSuffix+tmpSuffix):
			if !o.ReadOnly {
				logger.Info("Removing an unfinished checkpoint segment", "file", path)
				if err := os.Remove(path); err != nil {
					s.Close()
					return nil, err
				}
			}
		case strings.HasSuffix(name, segmentSuffix):
			id, err := strconv.ParseUint(strings.TrimSuffix(name, segmentSuffix), 10, 64)
			if err != nil {
				logger.Warn("Skipping a file with a segment suffix", "file", path)
				continue
			}
			seg, err := openSegment(path)
			if err == nil && seg.meta.ID != id {
				seg.f.Close()
				err = fmt.Errorf("%w: meta has id %d", ErrCorrupt, seg.meta.ID)
			}
			if err != nil {
				s.Close()
				return nil, fmt.Errorf("checkpoint segment %s: %w", path, err)
			}
			s.segments[id] = seg
		}
	}
	for id, seg := range s.segments {
		if base := seg.meta.Base; base != 0 {
			if base >= id || s.segments[base] == nil {
				s.Close()
				return nil, fmt.Errorf("%w: base %d of segment %d", ErrUnknownSegment, base, id)
			}
		}
	}
	return s, nil
}

// Dir returns the directory of the store.
func (s *Store) Dir() string { return s.dir }

// Segments returns the meta of every segment, in id order.
func (s *Store) Segments() []Meta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	metas := make([]Meta, 0, len(s.segments))
	for _, seg := range s.segments {
		metas = append(metas, seg.meta)
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].ID < metas[j].ID })
	return metas
}

// Meta returns the meta of a segment.
func (s *Store) Meta(id uint64) (Meta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if seg := s.segments[id]; seg != nil {
		return seg.meta, true
	}
	return Meta{}, false
}

// Latest returns the meta of the segment with the highest id.
func (s *Store) Latest() (Meta, bool) {
	metas := s.Segments()
	if len(metas) == 0 {
		return Meta{}, false
	}
	return metas[len(metas)-1], true
}

// Chain returns the meta of a segment and of its bases, the segment first.
func (s *Store) Chain(id uint64) ([]Meta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	segs, err := s.chain(id)
	if err != nil {
		return nil, err
	}
	metas := make([]Meta, len(segs))
	for i, seg := range segs {
		metas[i] = seg.meta
	}
	return metas, nil
}

func (s *Store) chain(id uint64) ([]*segment, error) {
	if s.closed {
		return nil, os.ErrClosed
	}
	var segs []*segment
	for id != 0 {
		seg := s.segments[id]
		if seg == nil {
			return nil, fmt.Errorf("%w: %d", ErrUnknownSegment, id)
		}
		segs = append(segs, seg)
		id = seg.meta.Base
	}
	return segs, nil
}

// Get returns the value of key in segment id, falling through to its bases.
// It returns ErrNotFound if no segment of the chain holds the key.
func (s *Store) Get(id uint64, key []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	segs, err := s.chain(id)
	if err != nil {
		return nil, err
	}
	for _, seg := range segs {
		value, err := s.lookup(seg, key)
		if err == nil {
			return bytes.Clone(value), nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return nil, ErrNotFound
}

func (s *Store) lookup(seg *segment, key []byte) ([]byte, error) {
	t := sort.Search(len(seg.top), func(i int) bool { return bytes.Compare(seg.top[i].first, key) > 0 }) - 1
	if t < 0 {
		return nil, ErrNotFound
	}
	ib, err := s.indexBlock(seg, seg.top[t].h)
	if err != nil {
		return nil, err
	}
	i := ib.floor(key)
	if i < 0 {
		return nil, fmt.Errorf("%w: index block does not start with its top key", ErrCorrupt)
	}
	db, err := s.dataBlock(seg, ib, i)
	if err != nil {
		return nil, err
	}
	if j := db.floor(key); j >= 0 && bytes.Equal(db[j].key, key) {
		return db[j].value, nil
	}
	return nil, ErrNotFound
}

func (s *Store) indexBlock(seg *segment, h handle) (block, error) {
	if b, ok := s.cache.Get(cacheKey{seg, h.offset}); ok {
		return b.(block), nil
	}
	b, err := seg.readBlock(h)
	if err != nil {
		return nil, err
	}
	s.cache.Add(cacheKey{seg, h.offset}, b)
	return b, nil
}

// dataBlock returns data block i of index block ib. A miss right after the
// last data read of the segment reads ahead the next blocks of ib in the same
// read, so lookups in key order read the file sequentially.
func (s *Store) dataBlock(seg *segment, ib block, i int) (block, error) {
	h, _, err := decodeHandle(ib[i].value)
	if err != nil {
		return nil, err
	}
	if b, ok := s.cache.Get(cacheKey{seg, h.offset}); ok {
		return b.(block), nil
	}
	handles := []handle{h}
	if ra := uint64(s.opts.ReadAhead); s.opts.ReadAhead > 0 {
		if last := seg.lastEnd.Load(); h.offset >= last && h.offset-last <= ra {
			for k := i + 1; k < len(ib); k++ {
				next, _, err := decodeHandle(ib[k].value)
				if err != nil || next.offset != handles[len(handles)-1].end() || next.end()-h.offset > ra {
					break
				}
				handles = append(handles, next)
			}
		}
	}
	end := handles[len(handles)-1].end()
	raw, err := seg.read(h.offset, end)
	if err != nil {
		return nil, err
	}
	seg.lastEnd.Store(end)
	var first block
	for k, hk := range handles {
		b, err := decodeBlock(raw[hk.offset-h.offset:hk.end()-h.offset], hk)
		if err != nil {
			if k == 0 {
				return nil, err
			}
			break
		}
		s.cache.Add(cacheKey{seg, hk.offset}, b)
		if k == 0 {
			first = b
		}
	}
	return first, nil
}

// NewWriter starts a segment of the store. Its id must be new and its base,
// if any, must be in the store. The segment joins the store on Finish.
func (s *Store) NewWriter(meta Meta) (*Writer, error) {
	if s.opts.ReadOnly {
		return nil, ErrReadOnly
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, os.ErrClosed
	}
	if meta.ID == 0 {
		return nil, errors.New("checkpoint: segment id must be above zero")
	}
	if s.segments[meta.ID] != nil {
		return nil, fmt.Errorf("%w: %d", ErrSegmentExists, meta.ID)
	}
	if meta.Base != 0 {
		if meta.Base >= meta.ID {
			return nil, fmt.Errorf("checkpoint: base %d of segment %d is not below it", meta.Base, meta.ID)
		}
		if s.segments[meta.Base] == nil {
			return nil, fmt.Errorf("%w: base %d", ErrUnknownSegment, meta.Base)
		}
	}
	return newWriter(s.dir, meta, s.opts, false, s.register)
}

// register adds a finished segment file, replacing one of the same id.
func (s *Store) register(path string) error {
	seg, err := openSegment(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		seg.f.Close()
		return os.ErrClosed
	}
	if old := s.segments[seg.meta.ID]; old != nil {
		s.retired = append(s.retired, old)
	}
	s.segments[seg.meta.ID] = seg
	return nil
}

// Remove deletes a segment that no other segment has as its base.
func (s *Store) Remove(id uint64) error {
	if s.opts.ReadOnly {
		return ErrReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seg := s.segments[id]
	if seg == nil {
		return fmt.Errorf("%w: %d", ErrUnknownSegment, id)
	}
	for other, o := range s.segments {
		if o.meta.Base == id {
			return fmt.Errorf("checkpoint: segment %d is the base of segment %d", id, other)
		}
	}
	if err := os.Remove(seg.path); err != nil {
		return err
	}
	delete(s.segments, id)
	s.retired = append(s.retired, seg)
	return syncDir(s.dir)
}

// Verify reads a whole segment and checks its checksums, key order and count.
func (s *Store) Verify(id uint64) error {
	s.mu.RLock()
	seg := s.segments[id]
	s.mu.RUnlock()
	if seg == nil {
		return fmt.Errorf("%w: %d", ErrUnknownSegment, id)
	}
	sum := crc32.New(castagnoli)
	if _, err := io.Copy(sum, io.NewSectionReader(seg.f, 0, int64(seg.dataEnd))); err != nil {
		return err
	}
	if sum.Sum32() != seg.meta.Checksum {
		return fmt.Errorf("%w: segment %d fails its checksum", ErrCorrupt, id)
	}
	it := &segIter{seg: seg, t: -1, i: -1, j: -1}
	var (
		count uint64
		prev  []byte
	)
	for it.next() {
		if count > 0 && bytes.Compare(it.key(), prev) <= 0 {
			return fmt.Errorf("%w: segment %d is unsorted", ErrCorrupt, id)
		}
		prev = it.key()
		count++
	}
	if it.err != nil {
		return it.err
	}
	if count != seg.meta.Entries {
		return fmt.Errorf("%w: segment %d has %d entries, meta says %d", ErrCorrupt, id, count, seg.meta.Entries)
	}
	return nil
}

// Close closes the segment files.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	for _, seg := range s.segments {
		errs = append(errs, seg.f.Close())
	}
	for _, seg := range s.retired {
		errs = append(errs, seg.f.Close())
	}
	s.cache.Purge()
	return errors.Join(errs...)
}

// Inspect writes a table of the segments in dir to w.
func Inspect(dir string, w io.Writer) error {
	s, err := Open(dir, &Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer s.Close()
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tBASE\tDEPTH\tBLOCK\tBLOCK HASH\tSTATE ROOT\tTRIGGER\tENTRIES\tSIZE")
	for _, meta := range s.Segments() {
		chain, err := s.Chain(meta.ID)
		if err != nil {
			return err
		}
		s.mu.RLock()
		size := s.segments[meta.ID].size
		s.mu.RUnlock()
		fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%s\t%s\t%s\t%d\t%d\n",
			meta.ID, meta.Base, len(chain), meta.BlockNumber, meta.BlockHash.Hex(), meta.StateRoot.Hex(), meta.Trigger, meta.Entries, size)
	}
	return tw.Flush()
}
