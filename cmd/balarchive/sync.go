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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
)

// syncProgress is the last block whose state was committed to disk.
type syncProgress struct {
	Block uint64
	Root  common.Hash
}

type syncItem struct {
	number uint64
	list   *bal.BlockAccessList
	root   common.Hash
	err    error
}

// cmdSync rebuilds the state from the genesis block by applying the block
// access lists, without executing any block, into a new database. Every block
// is checked against the state root of its header in the source chaindata.
//
//	balarchive sync -bal DIR -chaindata DIR -out DIR [-to N] [-commit N] [-cache MiB]
func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	balDir := fs.String("bal", "", "BAL freezer directory")
	chaindata := fs.String("chaindata", "", "chaindata with the headers to check against")
	out := fs.String("out", "", "directory of the new state database")
	to := fs.Uint64("to", 0, "last block (0 = last in the freezer)")
	commitEvery := fs.Uint64("commit", 10000, "blocks between commits to disk")
	dirtyMiB := fs.Int("dirty", 2048, "MiB of trie nodes held in memory before flushing")
	cacheMiB := fs.Int("cache", 4096, "MiB of the trie node cache")
	fs.Parse(args)

	freezer, err := bal.OpenFreezer(*balDir, true)
	if err != nil {
		return err
	}
	defer freezer.Close()
	src := openChain(*chaindata)
	defer src.Close()
	dst := database.NewDBManager(&database.DBConfig{
		Dir: *out, DBType: database.LevelDB, NumStateTrieShards: 4, OpenFilesLimit: 1024, LevelDBCacheSize: 512,
	})
	defer dst.Close()

	progressFile := filepath.Join(*out, "balsync.progress")
	progress := syncProgress{}
	if enc, err := os.ReadFile(progressFile); err == nil {
		if err := json.Unmarshal(enc, &progress); err != nil {
			return err
		}
		fmt.Printf("resuming after block %d, root %x\n", progress.Block, progress.Root)
	} else if errors.Is(err, os.ErrNotExist) {
		genesis := blockchain.DefaultKairosGenesisBlock().MustCommit(dst)
		want := src.ReadHeader(src.ReadCanonicalHash(0), 0).Root
		if genesis.Root() != want {
			return fmt.Errorf("genesis root %x, chaindata has %x: only Kairos is supported", genesis.Root(), want)
		}
		progress.Root = genesis.Root()
	} else {
		return err
	}

	end := freezer.Next() - 1
	if *to != 0 && *to < end {
		end = *to
	}

	// Read and decode the lists and the header roots ahead of the applier.
	items := make(chan syncItem, 4096)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		defer close(items)
		for n := progress.Block + 1; n <= end; {
			encs, err := freezer.ReadRawRange(n, min(end-n+1, 10_000), 16<<20)
			if err != nil {
				items <- syncItem{err: err}
				return
			}
			for _, enc := range encs {
				item := syncItem{number: n}
				if item.list, item.err = bal.Decode(enc); item.err == nil {
					if h := src.ReadHeader(src.ReadCanonicalHash(n), n); h != nil {
						item.root = h.Root
					} else {
						item.err = fmt.Errorf("no header for block %d", n)
					}
				}
				select {
				case items <- item:
				case <-quit:
					return
				}
				if item.err != nil {
					return
				}
				n++
			}
		}
	}()

	cacheConfig := statedb.GetEmptyTrieNodeCacheConfig()
	cacheConfig.LocalCacheSizeMiB = *cacheMiB
	stateDB := state.NewDatabaseWithNewCache(dst, cacheConfig)
	trieDB := stateDB.TrieDB()
	root := progress.Root
	trieDB.ReferenceRoot(root)

	start, last := time.Now(), time.Now()
	from := progress.Block
	commit := func(number uint64, root common.Hash) error {
		if err := trieDB.Commit(root, false, number); err != nil {
			return err
		}
		enc, _ := json.Marshal(syncProgress{Block: number, Root: root})
		tmp := progressFile + ".tmp"
		if err := os.WriteFile(tmp, enc, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, progressFile)
	}
	for item := range items {
		if item.err != nil {
			return fmt.Errorf("block %d: %w", item.number, item.err)
		}
		n := item.number
		sdb, err := state.New(root, stateDB, nil, nil)
		if err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		if err := sdb.ApplyBlockAccessList(item.list); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		next, err := sdb.Commit(true)
		if err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		if next != item.root {
			return fmt.Errorf("block %d: state root %x, header has %x", n, next, item.root)
		}
		trieDB.ReferenceRoot(next)
		trieDB.Dereference(root)
		root = next

		if nodes, _, _ := trieDB.Size(); nodes > common.StorageSize(*dirtyMiB)*1024*1024 {
			if err := trieDB.Cap(common.StorageSize(*dirtyMiB)*1024*1024 - database.IdealBatchSize); err != nil {
				return err
			}
		}
		if n%*commitEvery == 0 || n == end {
			if err := commit(n, root); err != nil {
				return err
			}
		}
		if time.Since(last) > 30*time.Second {
			last = time.Now()
			rate := float64(n-from) / time.Since(start).Seconds()
			nodes, _, _ := trieDB.Size()
			fmt.Printf("synced %d/%d, %.0f blocks/s, dirty %s, eta %s\n", n, end, rate, nodes,
				(time.Duration(float64(end-n)/rate) * time.Second).Round(time.Minute))
		}
	}
	fmt.Printf("rebuilt the state of block %d (root %x) from block %d in %s, every root matched\n",
		end, root, from, time.Since(start).Round(time.Second))
	return nil
}
