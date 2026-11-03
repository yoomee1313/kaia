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
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// small blocks so that segments of a few hundred entries have several index
// blocks.
var testOptions = &Options{BlockSize: 64}

func testKey(i int) []byte { return []byte(fmt.Sprintf("key-%05d", i)) }

func testMeta(id, base uint64) Meta {
	return Meta{
		ID:          id,
		Base:        base,
		BlockNumber: id * 1000,
		BlockHash:   common.BytesToHash([]byte{byte(id), 0xbb}),
		StateRoot:   common.BytesToHash([]byte{byte(id), 0x55}),
		Trigger:     TriggerInterval,
	}
}

// writeSegment writes the pairs of kv in key order as segment meta.ID.
func writeSegment(t *testing.T, s *Store, meta Meta, kv map[string]string) Meta {
	w, err := s.NewWriter(meta)
	require.NoError(t, err)
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		require.NoError(t, w.Put([]byte(k), []byte(kv[k])))
	}
	meta, err = w.Finish()
	require.NoError(t, err)
	return meta
}

func assertContents(t *testing.T, s *Store, id uint64, want map[string]string) {
	t.Helper()
	for k, v := range want {
		got, err := s.Get(id, []byte(k))
		require.NoError(t, err, "segment %d key %s", id, k)
		assert.Equal(t, v, string(got), "segment %d key %s", id, k)
	}
	it, err := s.NewIterator(id)
	require.NoError(t, err)
	got := make(map[string]string)
	var prev string
	for it.Next() {
		if len(got) > 0 {
			assert.Less(t, prev, string(it.Key()))
		}
		prev = string(it.Key())
		got[prev] = string(it.Value())
	}
	require.NoError(t, it.Error())
	assert.Equal(t, want, got, "iterator of segment %d", id)
}

func TestStore_Segment(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testOptions)
	require.NoError(t, err)

	kv := make(map[string]string)
	for i := 0; i < 500; i += 2 {
		kv[string(testKey(i))] = strings.Repeat(fmt.Sprint(i), i%7)
	}
	meta := testMeta(1, 0)
	meta.Extra = json.RawMessage(`{"pruning":"paused"}`)
	written := writeSegment(t, s, meta, kv)
	assert.Equal(t, uint64(len(kv)), written.Entries)
	assert.NotZero(t, written.Checksum)

	check := func(s *Store) {
		got, ok := s.Meta(1)
		require.True(t, ok)
		assert.Equal(t, written, got)
		assertContents(t, s, 1, kv)
		for _, i := range []int{-1, 1, 251, 499, 500} {
			_, err := s.Get(1, testKey(i))
			assert.ErrorIs(t, err, ErrNotFound, "key %d", i)
		}
		_, err := s.Get(1, nil)
		assert.ErrorIs(t, err, ErrNotFound)
		_, err = s.Get(2, testKey(0))
		assert.ErrorIs(t, err, ErrUnknownSegment)
		assert.NoError(t, s.Verify(1))
	}
	check(s)
	require.Greater(t, len(s.segments[1].top), 1, "the test wants several index blocks")
	require.NoError(t, s.Close())

	s, err = Open(dir, &Options{ReadOnly: true})
	require.NoError(t, err)
	defer s.Close()
	check(s)
	_, err = s.NewWriter(testMeta(2, 1))
	assert.ErrorIs(t, err, ErrReadOnly)

	var out bytes.Buffer
	require.NoError(t, Inspect(dir, &out))
	assert.Contains(t, out.String(), written.StateRoot.Hex())
}

func TestWriter_KeyOrder(t *testing.T) {
	s, err := Open(t.TempDir(), testOptions)
	require.NoError(t, err)
	defer s.Close()

	w, err := s.NewWriter(testMeta(1, 0))
	require.NoError(t, err)
	require.NoError(t, w.Put(nil, []byte("empty key")))
	require.NoError(t, w.Put([]byte("b"), []byte("1")))
	assert.ErrorIs(t, w.Put([]byte("b"), []byte("2")), ErrKeyOrder)
	assert.ErrorIs(t, w.Put([]byte("a"), []byte("3")), ErrKeyOrder)
	assert.ErrorIs(t, w.Put(nil, []byte("4")), ErrKeyOrder)
	require.NoError(t, w.Put([]byte("ba"), []byte("5")))
	_, err = w.Finish()
	require.NoError(t, err)
	assertContents(t, s, 1, map[string]string{"": "empty key", "b": "1", "ba": "5"})
}

func TestWriter_Crash(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testOptions)
	require.NoError(t, err)
	writeSegment(t, s, testMeta(1, 0), map[string]string{"a": "1"})

	// A writer that dies before Finish leaves only its temporary file.
	w, err := s.NewWriter(testMeta(2, 1))
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		require.NoError(t, w.Put(testKey(i), []byte("x")))
	}
	require.NoError(t, w.w.Flush())
	_, ok := s.Meta(2)
	assert.False(t, ok)
	require.NoError(t, s.Close())

	s, err = Open(dir, testOptions)
	require.NoError(t, err)
	defer s.Close()
	assert.Len(t, s.Segments(), 1)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, files, 1, "the temporary file is removed")

	// The segment is written again after the restart.
	writeSegment(t, s, testMeta(2, 1), map[string]string{"b": "2"})
	assertContents(t, s, 2, map[string]string{"a": "1", "b": "2"})

	// An aborted writer leaves nothing.
	w, err = s.NewWriter(testMeta(3, 2))
	require.NoError(t, err)
	require.NoError(t, w.Put([]byte("c"), []byte("3")))
	w.Abort()
	_, err = w.Finish()
	assert.Error(t, err)
	files, err = os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, files, 2)

	_, err = s.NewWriter(testMeta(2, 1))
	assert.ErrorIs(t, err, ErrSegmentExists)
	_, err = s.NewWriter(testMeta(4, 3))
	assert.ErrorIs(t, err, ErrUnknownSegment)
	_, err = NewWriter(dir, testMeta(2, 1), nil)
	assert.ErrorIs(t, err, ErrSegmentExists)
	_, err = NewWriter(dir, testMeta(3, 4), nil)
	assert.Error(t, err)
}

func TestStore_Chain(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testOptions)
	require.NoError(t, err)

	full := make(map[string]string)
	for i := 0; i < 300; i++ {
		full[string(testKey(i))] = "v1"
	}
	writeSegment(t, s, testMeta(1, 0), full)

	// Segment 2 changes the even keys and adds new ones; segment 3 changes
	// every third key.
	delta2 := make(map[string]string)
	for i := 0; i < 400; i += 2 {
		delta2[string(testKey(i))] = "v2"
	}
	writeSegment(t, s, testMeta(2, 1), delta2)
	delta3 := make(map[string]string)
	for i := 0; i < 300; i += 3 {
		delta3[string(testKey(i))] = "v3"
	}
	writeSegment(t, s, testMeta(3, 2), delta3)

	want := map[uint64]map[string]string{1: full, 2: {}, 3: {}}
	for _, id := range []uint64{2, 3} {
		for k, v := range want[id-1] {
			want[id][k] = v
		}
		for k, v := range map[uint64]map[string]string{2: delta2, 3: delta3}[id] {
			want[id][k] = v
		}
	}
	check := func(s *Store) {
		for id, kv := range want {
			assertContents(t, s, id, kv)
		}
	}
	check(s)
	chain, err := s.Chain(3)
	require.NoError(t, err)
	require.Len(t, chain, 3)
	assert.Equal(t, []uint64{3, 2, 1}, []uint64{chain[0].ID, chain[1].ID, chain[2].ID})
	latest, ok := s.Latest()
	require.True(t, ok)
	assert.Equal(t, uint64(3), latest.ID)

	// Compaction keeps what every segment sees.
	require.NoError(t, s.Compact(3))
	meta, _ := s.Meta(3)
	assert.Zero(t, meta.Base)
	assert.Equal(t, uint64(len(want[3])), meta.Entries)
	assert.NoError(t, s.Verify(3))
	check(s)

	// Segment 1 is the base of 2, which is no longer the base of anything.
	assert.Error(t, s.Remove(1))
	require.NoError(t, s.Remove(2))
	require.NoError(t, s.Remove(1))
	delete(want, 1)
	delete(want, 2)
	check(s)
	require.NoError(t, s.Close())

	s, err = Open(dir, testOptions)
	require.NoError(t, err)
	defer s.Close()
	assert.Len(t, s.Segments(), 1)
	check(s)
}

func TestStore_Corrupt(t *testing.T) {
	write := func(t *testing.T) (string, string) {
		dir := t.TempDir()
		s, err := Open(dir, testOptions)
		require.NoError(t, err)
		kv := make(map[string]string)
		for i := 0; i < 200; i++ {
			kv[string(testKey(i))] = "value"
		}
		writeSegment(t, s, testMeta(1, 0), kv)
		require.NoError(t, s.Close())
		return dir, filepath.Join(dir, segmentName(1))
	}
	flip := func(t *testing.T, path string, offset int64) {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		if offset < 0 {
			offset += int64(len(data))
		}
		data[offset] ^= 0x01
		require.NoError(t, os.WriteFile(path, data, 0o644))
	}

	t.Run("block", func(t *testing.T) {
		dir, path := write(t)
		flip(t, path, 3) // in the first data block
		s, err := Open(dir, testOptions)
		require.NoError(t, err)
		defer s.Close()
		_, err = s.Get(1, testKey(0))
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.ErrorIs(t, s.Verify(1), ErrCorrupt)
	})
	t.Run("meta", func(t *testing.T) {
		dir, path := write(t)
		flip(t, path, -footerSize-2)
		_, err := Open(dir, testOptions)
		assert.ErrorIs(t, err, ErrCorrupt)
	})
	t.Run("footer", func(t *testing.T) {
		dir, path := write(t)
		flip(t, path, -1)
		_, err := Open(dir, testOptions)
		assert.ErrorIs(t, err, ErrCorrupt)
	})
	t.Run("truncated", func(t *testing.T) {
		dir, path := write(t)
		st, err := os.Stat(path)
		require.NoError(t, err)
		require.NoError(t, os.Truncate(path, st.Size()-5))
		_, err = Open(dir, testOptions)
		assert.ErrorIs(t, err, ErrCorrupt)
	})
	t.Run("missing base", func(t *testing.T) {
		dir, path := write(t)
		s, err := Open(dir, testOptions)
		require.NoError(t, err)
		writeSegment(t, s, testMeta(2, 1), map[string]string{"a": "b"})
		require.NoError(t, s.Close())
		require.NoError(t, os.Remove(path))
		_, err = Open(dir, testOptions)
		assert.ErrorIs(t, err, ErrUnknownSegment)
	})
}

// TestStore_ReadAhead checks that a lookup after the previous data read also
// caches the blocks that follow it.
func TestStore_ReadAhead(t *testing.T) {
	for _, tc := range []struct {
		readAhead int
		cached    int
	}{{-1, 2}, {1 << 20, 0}} {
		dir := t.TempDir()
		s, err := Open(dir, &Options{BlockSize: 256, ReadAhead: tc.readAhead})
		require.NoError(t, err)
		kv := make(map[string]string)
		for i := 0; i < 200; i++ {
			kv[string(testKey(i))] = "value"
		}
		writeSegment(t, s, testMeta(1, 0), kv)
		_, err = s.Get(1, testKey(0))
		require.NoError(t, err)
		if tc.cached > 0 {
			assert.Equal(t, tc.cached, s.cache.Len(), "an index block and a data block")
		} else {
			assert.Greater(t, s.cache.Len(), 4)
		}
		require.NoError(t, s.Close())
	}
}

var (
	benchOnce sync.Once
	benchDir  string
	benchTemp bool
	benchKeys [][]byte
)

func TestMain(m *testing.M) {
	code := m.Run()
	if benchTemp {
		os.RemoveAll(benchDir)
	}
	os.Exit(code)
}

// benchStore writes a segment of 200k storage-trie-like entries, once. Set
// CHECKPOINT_BENCH_DIR to put it on the disk to measure, e.g. an HDD; the page
// cache should be dropped between runs for cold numbers.
func benchStore(b *testing.B) string {
	benchOnce.Do(func() {
		benchDir = os.Getenv("CHECKPOINT_BENCH_DIR")
		if benchDir == "" {
			var err error
			if benchDir, err = os.MkdirTemp("", "checkpoint-bench"); err != nil {
				b.Fatal(err)
			}
			benchTemp = true
		}
		rnd := rand.New(rand.NewSource(1))
		for i := 0; i < 200_000; i++ {
			key := make([]byte, 33+rnd.Intn(8))
			key[0] = 0x01
			rnd.Read(key[1:])
			benchKeys = append(benchKeys, key)
		}
		sort.Slice(benchKeys, func(i, j int) bool { return bytes.Compare(benchKeys[i], benchKeys[j]) < 0 })
		s, err := Open(benchDir, nil)
		if err != nil {
			b.Fatal(err)
		}
		defer s.Close()
		if _, ok := s.Meta(1); ok {
			return
		}
		w, err := s.NewWriter(testMeta(1, 0))
		if err != nil {
			b.Fatal(err)
		}
		value := make([]byte, 200)
		for _, key := range benchKeys {
			rnd.Read(value)
			if err := w.Put(key, value); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := w.Finish(); err != nil {
			b.Fatal(err)
		}
	})
	return benchDir
}

func BenchmarkStore_Get(b *testing.B) {
	dir := benchStore(b)
	for _, sorted := range []bool{true, false} {
		b.Run(map[bool]string{true: "sorted", false: "random"}[sorted], func(b *testing.B) {
			s, err := Open(dir, &Options{CacheBlocks: 256})
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			order := rand.New(rand.NewSource(2)).Perm(len(benchKeys))
			b.SetBytes(200)
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				i := n % len(benchKeys)
				if !sorted {
					i = order[i]
				}
				if _, err := s.Get(1, benchKeys[i]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
