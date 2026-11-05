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
	"sync"
	"sync/atomic"
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
// access lists, without executing any block, into a new database. A list is
// first checked against the list hash of its header, from the Amber hardfork,
// so that its per-tx values and reads are covered too. Blocks are then
// applied in batches that are hashed once and checked against the state root
// in the header of their last block; a batch of 1 checks every block, and a
// failed batch is replayed block by block to find the first wrong block.
// Storage tries are hashed in parallel, and the trie nodes of the next batch
// are read ahead while a batch is applied. A new database gets live pruning:
// trie nodes are deleted once they have been obsolete for -retention blocks, as
// a node does with --state.live-pruning-retention.
//
//	balarchive sync -bal DIR -chaindata DIR -out DIR [-to N] [-batch N] [-workers N] [-retention N]
func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	balDir := fs.String("bal", "", "BAL freezer directory")
	chaindata := fs.String("chaindata", "", "chaindata with the headers to check against")
	out := fs.String("out", "", "directory of the new state database")
	to := fs.Uint64("to", 0, "last block (0 = last in the freezer)")
	commitEvery := fs.Uint64("commit", 7200, "commits to disk at every multiple of this block number")
	dirtyMiB := fs.Int("dirty", 2048, "MiB of trie nodes held in memory before flushing")
	cacheMiB := fs.Int("cache", 4096, "MiB of the trie node cache")
	batchSize := fs.Uint64("batch", 720, "blocks applied before one root check, at multiples of this block number")
	workers := fs.Int("workers", 8, "goroutines hashing storage tries and reading ahead")
	prefetch := fs.Bool("prefetch", true, "read the trie nodes of the next batch ahead")
	retention := fs.Uint64("retention", blockchain.DefaultLivePruningRetention, "blocks an obsolete trie node is kept, 0 = no pruning (only for a new database)")
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
		if *retention > 0 {
			dst.WritePruningEnabled()
		}
	} else {
		return err
	}
	// Batches line up with commits and with the retention, so that nodes are
	// pruned exactly when the retention has passed at the end of a batch.
	if *commitEvery%*batchSize != 0 || (*retention > 0 && *retention%*batchSize != 0) {
		fmt.Printf("warning: batch %d does not divide commit %d and retention %d\n", *batchSize, *commitEvery, *retention)
	}
	pruning := dst.ReadPruningEnabled() && *retention > 0
	pruneFrom := uint64(1)
	if last, err := dst.ReadLastPrunedBlockNumber(); err == nil {
		pruneFrom = last + 1
	}
	fmt.Printf("live pruning: %v, retention %d blocks\n", pruning, *retention)

	end := freezer.Next() - 1
	if *to != 0 && *to < end {
		end = *to
	}

	// Read and decode the lists and the header roots ahead of the applier, in
	// batches ending at multiples of the batch size.
	batches := make(chan []syncItem, 4)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		defer close(batches)
		var batch []syncItem
		for n := progress.Block + 1; n <= end; {
			encs, err := freezer.ReadRawRange(n, min(end-n+1, 10_000), 16<<20)
			if err != nil {
				batches <- []syncItem{{number: n, err: err}}
				return
			}
			for _, enc := range encs {
				item := syncItem{number: n}
				if item.list, item.err = bal.Decode(enc); item.err == nil {
					if h := src.ReadHeader(src.ReadCanonicalHash(n), n); h == nil {
						item.err = fmt.Errorf("no header for block %d", n)
					} else if item.err = item.list.CheckHash(h.BlockAccessListHash); item.err == nil {
						// Lists of blocks with a list hash are checked before they are applied.
						item.root = h.Root
					}
				}
				batch = append(batch, item)
				if item.err != nil || n%*batchSize == 0 || n == end {
					select {
					case batches <- batch:
					case <-quit:
						return
					}
					if item.err != nil {
						return
					}
					batch = nil
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

	// Every commit appends a line to the metrics file, which survives restarts.
	metricsPath := filepath.Join(*out, "balsync.metrics.csv")
	_, statErr := os.Stat(metricsPath)
	metrics, err := os.OpenFile(metricsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer metrics.Close()
	if errors.Is(statErr, os.ErrNotExist) {
		fmt.Fprintln(metrics, "time,block,run_elapsed_s,interval_blocks,interval_s,interval_blocks_per_s,commit_s,prune_s,dirty_bytes,pruned_nodes_total,batch,commit_every,retention")
	}

	start, last := time.Now(), time.Now()
	from := progress.Block
	lastBlock, lastTime := from, start
	var pruned uint64
	commit := func(number uint64, root common.Hash) error {
		commitStart := time.Now()
		dirty, _, _ := trieDB.Size()
		if err := trieDB.Commit(root, false, number); err != nil {
			return err
		}
		pruneStart := time.Now()
		// Committed marks of nodes obsolete for the retention are deleted.
		if pruning && number > *retention && number-*retention >= pruneFrom {
			limit := number - *retention
			marks := dst.ReadPruningMarks(pruneFrom, limit+1)
			dst.PruneTrieNodes(marks)
			dst.DeletePruningMarks(marks)
			dst.WriteLastPrunedBlockNumber(limit)
			pruneFrom, pruned = limit+1, pruned+uint64(len(marks))
		}
		now := time.Now()
		interval := now.Sub(lastTime).Seconds()
		fmt.Fprintf(metrics, "%s,%d,%.1f,%d,%.1f,%.0f,%.2f,%.2f,%d,%d,%d,%d,%d\n",
			now.Format(time.RFC3339), number, now.Sub(start).Seconds(), number-lastBlock, interval,
			float64(number-lastBlock)/interval, pruneStart.Sub(commitStart).Seconds(), now.Sub(pruneStart).Seconds(),
			uint64(dirty), pruned, *batchSize, *commitEvery, *retention)
		lastBlock, lastTime = number, now

		enc, _ := json.Marshal(syncProgress{Block: number, Root: root})
		tmp := progressFile + ".tmp"
		if err := os.WriteFile(tmp, enc, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, progressFile)
	}
	var prefetching atomic.Bool
	readAhead := func(batch []syncItem, root common.Hash) {
		if !*prefetch || !prefetching.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer prefetching.Store(false)
			syncReadAhead(stateDB, root, batch, *workers)
		}()
	}
	shown := from
	cur, ok := <-batches
	for ok {
		next, more := <-batches
		if more {
			readAhead(next, root)
		}
		if err := cur[len(cur)-1].err; err != nil {
			return fmt.Errorf("block %d: %w", cur[len(cur)-1].number, err)
		}
		var opts *statedb.TrieOpts
		if pruning {
			opts = &statedb.TrieOpts{PruningBlockNumber: cur[len(cur)-1].number}
		}
		nextRoot, err := syncApply(stateDB, root, cur, *workers, opts)
		if err != nil {
			return err
		}
		trieDB.ReferenceRoot(nextRoot)
		trieDB.Dereference(root)
		root = nextRoot

		n := cur[len(cur)-1].number
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
			// The rate since the last line gives the eta: blocks get heavier.
			rate := float64(n-shown) / time.Since(last).Seconds()
			last, shown = time.Now(), n
			nodes, _, _ := trieDB.Size()
			fmt.Printf("%s synced %d/%d, %.0f blocks/s, dirty %s, pruned %d nodes, eta %s\n", last.Format("15:04:05"), n, end, rate, nodes, pruned,
				(time.Duration(float64(end-n)/rate) * time.Second).Round(time.Minute))
		}
		cur, ok = next, more
	}
	fmt.Printf("rebuilt the state of block %d (root %x) from block %d in %s, every root matched\n",
		end, root, from, time.Since(start).Round(time.Second))
	return nil
}

// syncApply applies a batch of lists to the state at root and returns the new
// root, committed to the trie database. If the root of the batch does not match
// the header of its last block, the batch is replayed block by block from root
// to report the first wrong block. opts marks obsolete nodes for pruning.
func syncApply(stateDB state.Database, root common.Hash, batch []syncItem, workers int, opts *statedb.TrieOpts) (common.Hash, error) {
	sdb, err := state.New(root, stateDB, nil, opts)
	if err != nil {
		return common.Hash{}, fmt.Errorf("block %d: %w", batch[0].number, err)
	}
	for _, item := range batch {
		if err := sdb.ApplyBlockAccessListUnchecked(item.list); err != nil {
			return common.Hash{}, fmt.Errorf("block %d: %w", item.number, err)
		}
	}
	sdb.HashStorages(workers)
	last := batch[len(batch)-1]
	if got := sdb.IntermediateRoot(true); got != last.root || sdb.Error() != nil {
		return common.Hash{}, syncFindBad(stateDB, root, batch)
	}
	return sdb.Commit(true)
}

func syncFindBad(stateDB state.Database, root common.Hash, batch []syncItem) error {
	sdb, err := state.New(root, stateDB, nil, nil)
	if err != nil {
		return err
	}
	for _, item := range batch {
		if err := sdb.ApplyBlockAccessList(item.list); err != nil {
			return fmt.Errorf("block %d: %w", item.number, err)
		}
		if got := sdb.IntermediateRoot(true); got != item.root {
			return fmt.Errorf("block %d: state root %x, header has %x", item.number, got, item.root)
		}
		if err := sdb.Error(); err != nil {
			return fmt.Errorf("block %d: %w", item.number, err)
		}
	}
	return fmt.Errorf("blocks %d-%d: the batch root did not match, but every block does", batch[0].number, batch[len(batch)-1].number)
}

// syncReadAhead reads the accounts and slots of a batch from the state at root,
// so that their trie nodes are in the cache when the batch is applied. Errors
// are ignored: root may be released meanwhile.
func syncReadAhead(stateDB state.Database, root common.Hash, batch []syncItem, workers int) {
	type key struct {
		addr common.Address
		slot *common.Hash
	}
	var keys []key
	for _, item := range batch {
		for i := range item.list.Accounts {
			a := &item.list.Accounts[i]
			keys = append(keys, key{addr: a.Address})
			for j := range a.StorageWrites {
				keys = append(keys, key{addr: a.Address, slot: &a.StorageWrites[j].Slot})
			}
		}
	}
	var (
		wg   sync.WaitGroup
		next atomic.Int64
	)
	for range max(1, workers) {
		wg.Go(func() {
			sdb, err := state.New(root, stateDB, nil, nil)
			if err != nil {
				return
			}
			for i := int(next.Add(1) - 1); i < len(keys); i = int(next.Add(1) - 1) {
				if k := keys[i]; k.slot == nil {
					sdb.GetBalance(k.addr)
				} else {
					sdb.GetState(k.addr, *k.slot)
				}
			}
		})
	}
	wg.Wait()
}
