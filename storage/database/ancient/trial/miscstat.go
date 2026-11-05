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

//go:build ignore

// miscstat counts the keys of a LevelDB database by key class, read-only, and
// prints their key and value bytes and the on-disk size LevelDB reports for
// each class. Run it on a copy of the database, never on a live one:
//
//	go run miscstat.go <leveldb dir> [samples per class]
package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

type stat struct {
	n, kb, vb     uint64
	first, last   []byte
	lens          map[int]uint64
	samples       [][]byte
	sampleValLens []int
}

// class names a key by its leading ASCII run, or by its first byte, plus the
// shapes of the fixed-layout keys.
func class(k []byte) string {
	if len(k) == 42 && k[0] == 'h' && k[41] == 't' {
		return "h<num><hash>t (TD)"
	}
	if len(k) == 43 && k[0] == 'B' {
		return "B<bit><section><hash> (bloombits)"
	}
	i := 0
	for i < len(k) && i < 40 {
		c := k[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-' || c == '_' {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return fmt.Sprintf("0x%02x", k[0])
	}
	return string(k[:i])
}

func main() {
	dir := os.Args[1]
	samples := 3
	if len(os.Args) > 2 {
		samples, _ = strconv.Atoi(os.Args[2])
	}
	db, err := leveldb.OpenFile(dir, &opt.Options{ReadOnly: true, ErrorIfMissing: true, OpenFilesCacheCapacity: 64, BlockCacheCapacity: 8 << 20})
	if err != nil {
		panic(err)
	}
	defer db.Close()

	stats := map[string]*stat{}
	it := db.NewIterator(nil, &opt.ReadOptions{DontFillCache: true})
	start, total := time.Now(), uint64(0)
	for it.Next() {
		k, v := it.Key(), it.Value()
		c := class(k)
		s := stats[c]
		if s == nil {
			s = &stat{first: append([]byte{}, k...), lens: map[int]uint64{}}
			stats[c] = s
		}
		s.n++
		s.kb += uint64(len(k))
		s.vb += uint64(len(v))
		s.last = append(s.last[:0], k...)
		s.lens[len(k)]++
		if len(s.samples) < samples {
			s.samples = append(s.samples, append([]byte{}, k...))
			s.sampleValLens = append(s.sampleValLens, len(v))
		}
		total++
		if total%20_000_000 == 0 {
			fmt.Fprintf(os.Stderr, "%d keys, %s\n", total, time.Since(start).Round(time.Second))
		}
	}
	it.Release()
	if err := it.Error(); err != nil {
		panic(err)
	}

	names := make([]string, 0, len(stats))
	for c := range stats {
		names = append(names, c)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := stats[names[i]], stats[names[j]]
		return a.kb+a.vb > b.kb+b.vb
	})
	var tk, tv uint64
	fmt.Printf("%-40s %14s %14s %14s %14s %8s\n", "class", "keys", "key bytes", "value bytes", "disk (SizeOf)", "avg val")
	for _, c := range names {
		s := stats[c]
		tk += s.kb
		tv += s.vb
		limit := append(append([]byte{}, s.last...), 0)
		sizes, _ := db.SizeOf([]util.Range{{Start: s.first, Limit: limit}})
		fmt.Printf("%-40s %14d %14d %14d %14d %8d\n", c, s.n, s.kb, s.vb, sizes.Sum(), s.vb/s.n)
	}
	fmt.Printf("%-40s %14d %14d %14d\n", "TOTAL", total, tk, tv)
	fmt.Printf("\nscan took %s\n\nsamples:\n", time.Since(start).Round(time.Second))
	for _, c := range names {
		s := stats[c]
		fmt.Printf("%s: key lengths %v\n", c, s.lens)
		for i, k := range s.samples {
			fmt.Printf("  %s (value %d B)\n", hex.EncodeToString(k), s.sampleValLens[i])
		}
	}
}
