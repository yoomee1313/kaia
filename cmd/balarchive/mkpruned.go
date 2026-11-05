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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// The databases of a chaindata that hold blocks, as opposed to the state trie.
var blockDBs = []string{"header", "body", "receipts", "txlookup", "bridgeservice", "misc"}

// Keys of the misc database that make a database live pruned.
var (
	pruningEnabledKey     = []byte("PruningEnabled")
	pruningMarkPrefix     = []byte("Pruning-")
	lastPrunedBlockNumKey = []byte("lastPrunedBlockNumber")
)

// cmdMkPruned builds the chaindata of a live pruned node from the blocks of an
// existing chaindata and a live pruned state database, as `sync` writes it:
// the block databases are copied, the state trie is copied from the state
// database, and the pruning flag, marks and progress of its misc database are
// added. The source databases are only read.
//
//	balarchive mkpruned -chaindata SRC -state DIR -out DIR
func cmdMkPruned(args []string) error {
	fs := flag.NewFlagSet("mkpruned", flag.ExitOnError)
	src := fs.String("chaindata", "", "chaindata with the blocks")
	stateDir := fs.String("state", "", "live pruned state database written by sync")
	out := fs.String("out", "", "new chaindata directory (klay/chaindata of the new datadir)")
	fs.Parse(args)

	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s exists", *out)
	}
	start := time.Now()
	for _, name := range blockDBs {
		n, err := copyDir(filepath.Join(*src, name), filepath.Join(*out, name))
		if err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
		fmt.Printf("copied %s: %s\n", name, common.StorageSize(n))
	}
	n, err := copyDir(filepath.Join(*stateDir, "statetrie"), filepath.Join(*out, "statetrie"))
	if err != nil {
		return fmt.Errorf("copy statetrie: %w", err)
	}
	fmt.Printf("copied statetrie: %s\n", common.StorageSize(n))

	// The pruning keys of the state database go to the misc database.
	from, err := database.NewLevelDBWithOption(filepath.Join(*stateDir, "misc"), &opt.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := database.NewLevelDBWithOption(filepath.Join(*out, "misc"), &opt.Options{})
	if err != nil {
		return err
	}
	batch := to.NewBatch()
	for _, key := range [][]byte{pruningEnabledKey, lastPrunedBlockNumKey} {
		ok, err := from.Has(key)
		if err != nil {
			return err
		}
		if !ok {
			if string(key) == string(pruningEnabledKey) {
				return errors.New("the state database is not live pruned")
			}
			continue
		}
		value, err := from.Get(key)
		if err != nil {
			return err
		}
		if err := batch.Put(key, value); err != nil {
			return err
		}
	}
	it := from.NewIterator(pruningMarkPrefix, nil)
	marks := 0
	for it.Next() {
		if err := batch.Put(common.CopyBytes(it.Key()), common.CopyBytes(it.Value())); err != nil {
			return err
		}
		marks++
		if batch.ValueSize() > database.IdealBatchSize {
			if err := batch.Write(); err != nil {
				return err
			}
			batch.Reset()
		}
	}
	it.Release()
	if err := batch.Write(); err != nil {
		return err
	}
	to.Close()
	fmt.Printf("added the pruning flag and %d pruning marks\n", marks)

	// The head is kept: a node rewinds to the newest block with a state.
	dbm := openChain(*out)
	defer dbm.Close()
	if !dbm.ReadPruningEnabled() {
		return errors.New("pruning flag missing after the merge")
	}
	head := dbm.ReadHeadBlockHash()
	number := dbm.ReadHeaderNumber(head)
	if number == nil {
		return errors.New("no head block")
	}
	sdb := state.NewDatabase(dbm)
	for n := *number; ; n-- {
		h := dbm.ReadHeader(dbm.ReadCanonicalHash(n), n)
		if h == nil {
			return fmt.Errorf("no header for block %d", n)
		}
		if _, err := state.New(h.Root, sdb, nil, nil); err == nil {
			fmt.Printf("head block %d, newest block with a state %d (root %x), built in %s\n", *number, n, h.Root, time.Since(start).Round(time.Second))
			return nil
		}
		if *number-n > 1_000_000 {
			return fmt.Errorf("no state in the 1M blocks below the head %d", *number)
		}
	}
}

// copyDir copies the regular files of a directory tree and returns the bytes
// copied.
func copyDir(src, dst string) (int64, error) {
	var total int64
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() || info.Name() == "LOCK" {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		outFile, err := os.Create(target)
		if err != nil {
			return err
		}
		n, err := io.Copy(outFile, in)
		total += n
		if cerr := outFile.Close(); err == nil {
			err = cerr
		}
		return err
	})
	return total, err
}
