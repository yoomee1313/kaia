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

package nodecmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/urfave/cli/v2"
)

var (
	rebuildCommitFlag = &cli.Uint64Flag{
		Name:  "commit",
		Usage: "Commit the state to disk at every multiple of this block number",
		Value: 7200,
	}
	rebuildDirtyFlag = &cli.IntFlag{
		Name:  "dirty",
		Usage: "MiB of trie nodes held in memory before flushing",
		Value: 2048,
	}
	rebuildCacheFlag = &cli.IntFlag{
		Name:  "cache",
		Usage: "MiB of the trie node cache",
		Value: 4096,
	}
	rebuildBatchFlag = &cli.Uint64Flag{
		Name:  "batch",
		Usage: "Blocks applied before one state root check, at multiples of this block number",
		Value: 720,
	}
	rebuildWorkersFlag = &cli.IntFlag{
		Name:  "workers",
		Usage: "Goroutines hashing storage tries and reading the next batch ahead",
		Value: 8,
	}
	rebuildRetentionFlag = &cli.Uint64Flag{
		Name:  "retention",
		Usage: "Blocks an obsolete trie node is kept, as --state.live-pruning-retention",
		Value: blockchain.DefaultLivePruningRetention,
	}
	rebuildFlags = []cli.Flag{
		dbOutFlag, dbToFlag, rebuildCommitFlag, rebuildDirtyFlag, rebuildCacheFlag,
		rebuildBatchFlag, rebuildWorkersFlag, rebuildRetentionFlag,
	}
)

type rebuildOptions struct {
	out                               string
	to, commitEvery, batch, retention uint64
	dirtyMiB, cacheMiB, workers       int
	stateShards                       uint
	dbType                            database.DBType
	genesis                           *blockchain.Genesis // nil: Mainnet or Kairos, by the genesis hash
}

func dbRebuild(ctx *cli.Context) error {
	src, dir := openDBForTool(ctx)
	o := rebuildOptions{
		out: ctx.String(dbOutFlag.Name), to: ctx.Uint64(dbToFlag.Name),
		commitEvery: ctx.Uint64(rebuildCommitFlag.Name), batch: ctx.Uint64(rebuildBatchFlag.Name),
		retention: ctx.Uint64(rebuildRetentionFlag.Name), dirtyMiB: ctx.Int(rebuildDirtyFlag.Name),
		cacheMiB: ctx.Int(rebuildCacheFlag.Name), workers: ctx.Int(rebuildWorkersFlag.Name),
		stateShards: src.GetDBConfig().NumStateTrieShards, dbType: src.GetDBConfig().DBType,
	}
	if src.GetDBConfig().SingleDB {
		src.Close()
		return errors.New("a single database chain keeps blocks and state together and cannot be rebuilt")
	}
	return rebuildState(src, dir, o, os.Stdout)
}

// rebuildProgress is the last block whose state was committed to disk.
type rebuildProgress struct {
	Block uint64
	Root  common.Hash
}

type rebuildItem struct {
	number uint64
	list   *access.BlockAccessList
	root   common.Hash
	err    error
}

// rebuildGenesis returns the genesis of the chain of src, to rebuild its state.
func rebuildGenesis(src database.DBManager) (*blockchain.Genesis, error) {
	hash := src.ReadCanonicalHash(0)
	for _, g := range []*blockchain.Genesis{blockchain.DefaultGenesisBlock(), blockchain.DefaultKairosGenesisBlock()} {
		if g.ToBlock(common.Hash{}, nil).Hash() == hash {
			return g, nil
		}
	}
	return nil, fmt.Errorf("genesis %x is neither Mainnet nor Kairos", hash)
}

// rebuildState rebuilds the state from the genesis block by applying the block
// access lists stored in src, without executing any block, and writes the
// chain database of a live pruned node to o.out: the block data of src, whose
// directory is srcDir, with the rebuilt state. It closes src.
//
// Blocks are applied in batches that are hashed once and checked against the
// state root in the header of their last block; a failed batch is replayed
// block by block to find the first wrong block. Storage tries are hashed in
// parallel, and the trie nodes of the next batch are read ahead while a batch
// is applied. Trie nodes are deleted once they have been obsolete for
// o.retention blocks. The state is built in o.out.rebuild, where a restart
// resumes.
func rebuildState(src database.DBManager, srcDir string, o rebuildOptions, w io.Writer) error {
	defer src.Close()
	if o.out == "" {
		return errors.New("--out is required")
	}
	if _, err := os.Stat(o.out); err == nil {
		return fmt.Errorf("%s exists", o.out)
	}
	if o.batch == 0 || o.commitEvery == 0 {
		return errors.New("zero batch or commit interval")
	}
	work := o.out + ".rebuild"
	dst := database.NewDBManager(&database.DBConfig{
		Dir: work, DBType: o.dbType, NumStateTrieShards: o.stateShards, OpenFilesLimit: 1024, LevelDBCacheSize: 512,
	})
	closeDst := sync.OnceFunc(dst.Close)
	defer closeDst()

	progressFile := filepath.Join(work, "rebuild.progress")
	progress := rebuildProgress{}
	if enc, err := os.ReadFile(progressFile); err == nil {
		if err := json.Unmarshal(enc, &progress); err != nil {
			return err
		}
		fmt.Fprintf(w, "resuming after block %d, root %x\n", progress.Block, progress.Root)
	} else if errors.Is(err, os.ErrNotExist) {
		genesis := o.genesis
		if genesis == nil {
			var err error
			if genesis, err = rebuildGenesis(src); err != nil {
				return err
			}
		}
		progress.Root = genesis.ToBlock(common.Hash{}, dst).Root()
		if want := src.ReadHeader(src.ReadCanonicalHash(0), 0); want == nil || want.Root != progress.Root {
			return errors.New("the genesis state does not match the genesis header of the chain")
		}
		if o.retention > 0 {
			dst.WritePruningEnabled()
		}
	} else {
		return err
	}
	// Batches line up with commits and with the retention, so that nodes are
	// pruned exactly when the retention has passed at the end of a batch.
	if o.commitEvery%o.batch != 0 || (o.retention > 0 && o.retention%o.batch != 0) {
		fmt.Fprintf(w, "warning: batch %d does not divide commit %d and retention %d\n", o.batch, o.commitEvery, o.retention)
	}
	pruning := dst.ReadPruningEnabled() && o.retention > 0
	pruneFrom := uint64(1)
	if last, err := dst.ReadLastPrunedBlockNumber(); err == nil {
		pruneFrom = last + 1
	}

	first, next := src.ReadBALRange()
	if first > 1 {
		return fmt.Errorf("the stored lists start at block %d, the rebuild starts at the genesis block", first)
	}
	end := max(next, 1) - 1
	if o.to != 0 && o.to < end {
		end = o.to
	}
	fmt.Fprintf(w, "rebuilding the state of blocks %d-%d, live pruning %v with a retention of %d blocks\n", progress.Block+1, end, pruning, o.retention)

	// Read and decode the lists and the header roots ahead of the applier, in
	// batches ending at multiples of the batch size.
	batches := make(chan []rebuildItem, 4)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		defer close(batches)
		var batch []rebuildItem
		for n := progress.Block + 1; n <= end; n++ {
			item := rebuildItem{number: n}
			if item.list, item.err = bal.Read(src, n); item.err == nil {
				if h := src.ReadHeader(src.ReadCanonicalHash(n), n); h == nil {
					item.err = fmt.Errorf("no header for block %d", n)
				} else {
					item.root = h.Root
				}
			}
			batch = append(batch, item)
			if item.err != nil || n%o.batch == 0 || n == end {
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
		}
	}()

	cacheConfig := statedb.GetEmptyTrieNodeCacheConfig()
	cacheConfig.LocalCacheSizeMiB = o.cacheMiB
	stateDB := state.NewDatabaseWithNewCache(dst, cacheConfig)
	trieDB := stateDB.TrieDB()
	root := progress.Root
	trieDB.ReferenceRoot(root)

	start, lastLog := time.Now(), time.Now()
	from, shown := progress.Block, progress.Block
	var pruned int
	commit := func(number uint64, root common.Hash) error {
		if err := trieDB.Commit(root, false, number); err != nil {
			return err
		}
		if pruning && number > o.retention && number-o.retention >= pruneFrom {
			limit := number - o.retention
			pruned += dst.PruneMarkedTrieNodes(pruneFrom, limit+1, nil)
			dst.WriteLastPrunedBlockNumber(limit)
			pruneFrom = limit + 1
		}
		enc, _ := json.Marshal(rebuildProgress{Block: number, Root: root})
		tmp := progressFile + ".tmp"
		if err := os.WriteFile(tmp, enc, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, progressFile)
	}
	var reading atomic.Bool
	readAhead := func(batch []rebuildItem, root common.Hash) {
		if !reading.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer reading.Store(false)
			rebuildReadAhead(stateDB, root, batch, o.workers)
		}()
	}
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
		nextRoot, err := rebuildApply(stateDB, root, cur, o.workers, opts)
		if err != nil {
			return err
		}
		trieDB.ReferenceRoot(nextRoot)
		trieDB.Dereference(root)
		root = nextRoot

		n := cur[len(cur)-1].number
		if nodes, _, _ := trieDB.Size(); nodes > common.StorageSize(o.dirtyMiB)*1024*1024 {
			if err := trieDB.Cap(common.StorageSize(o.dirtyMiB)*1024*1024 - database.IdealBatchSize); err != nil {
				return err
			}
		}
		if n%o.commitEvery == 0 || n == end {
			if err := commit(n, root); err != nil {
				return err
			}
		}
		if time.Since(lastLog) > 30*time.Second {
			// The rate since the last line gives the eta: blocks get heavier.
			rate := float64(n-shown) / time.Since(lastLog).Seconds()
			lastLog, shown = time.Now(), n
			nodes, _, _ := trieDB.Size()
			fmt.Fprintf(w, "%s rebuilt %d/%d, %.0f blocks/s, dirty %s, pruned %d nodes, eta %s\n", lastLog.Format("15:04:05"), n, end, rate, nodes, pruned,
				(time.Duration(float64(end-n)/rate) * time.Second).Round(time.Minute))
		}
		cur, ok = next, more
	}
	if progress.Block < end {
		fmt.Fprintf(w, "rebuilt the state of block %d (root %x) from block %d in %s, every root matched\n",
			end, root, from, time.Since(start).Round(time.Second))
	}
	closeDst()
	src.Close()
	return assembleRebuilt(srcDir, work, o, w)
}

// rebuildApply applies a batch of lists to the state at root and returns the
// new root, committed to the trie database. If the root of the batch does not
// match the header of its last block, the batch is replayed block by block
// from root to report the first wrong block. opts marks obsolete nodes for
// pruning.
func rebuildApply(stateDB state.Database, root common.Hash, batch []rebuildItem, workers int, opts *statedb.TrieOpts) (common.Hash, error) {
	sdb, err := state.New(root, stateDB, nil, opts)
	if err != nil {
		return common.Hash{}, fmt.Errorf("block %d: %w", batch[0].number, err)
	}
	for _, item := range batch {
		if err := access.ApplyUnchecked(sdb, item.list); err != nil {
			return common.Hash{}, fmt.Errorf("block %d: %w", item.number, err)
		}
	}
	sdb.HashStorages(workers)
	last := batch[len(batch)-1]
	if got := sdb.IntermediateRoot(true); got != last.root || sdb.Error() != nil {
		return common.Hash{}, rebuildFindBad(stateDB, root, batch)
	}
	return sdb.Commit(true)
}

func rebuildFindBad(stateDB state.Database, root common.Hash, batch []rebuildItem) error {
	sdb, err := state.New(root, stateDB, nil, nil)
	if err != nil {
		return err
	}
	for _, item := range batch {
		if err := access.Apply(sdb, item.list); err != nil {
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

// rebuildReadAhead reads the accounts and slots of a batch from the state at
// root, so that their trie nodes are in the cache when the batch is applied.
// Errors are ignored: root may be released meanwhile.
func rebuildReadAhead(stateDB state.Database, root common.Hash, batch []rebuildItem, workers int) {
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

// isStateDir reports whether a directory of a chain database holds state,
// which the rebuilt state replaces.
func isStateDir(name string) bool {
	return strings.HasPrefix(name, "statetrie") || name == "snapshot"
}

// assembleRebuilt writes the chain database of a live pruned node to o.out:
// the block data of the chain database in srcDir, the state trie rebuilt in
// work, and the pruning flag, marks and progress of work. It then removes
// work.
func assembleRebuilt(srcDir, work string, o rebuildOptions, w io.Writer) error {
	tmp := o.out + ".assemble"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || isStateDir(e.Name()) {
			continue
		}
		n, err := copyTree(filepath.Join(srcDir, e.Name()), filepath.Join(tmp, e.Name()))
		if err != nil {
			return fmt.Errorf("copy %s: %w", e.Name(), err)
		}
		fmt.Fprintf(w, "copied %s: %s\n", e.Name(), common.StorageSize(n))
	}
	n, err := copyTree(filepath.Join(work, "statetrie"), filepath.Join(tmp, "statetrie"))
	if err != nil {
		return fmt.Errorf("copy statetrie: %w", err)
	}
	fmt.Fprintf(w, "copied the rebuilt statetrie: %s\n", common.StorageSize(n))

	// The pruning state of the rebuilt trie goes to the misc database.
	from := database.NewDBManager(&database.DBConfig{Dir: work, DBType: o.dbType, NumStateTrieShards: o.stateShards, OpenFilesLimit: 256})
	to := database.NewDBManager(&database.DBConfig{Dir: tmp, DBType: o.dbType, NumStateTrieShards: o.stateShards, OpenFilesLimit: 256, FreezerReadOnly: true})
	marks := 0
	if from.ReadPruningEnabled() {
		to.WritePruningEnabled()
		if last, err := from.ReadLastPrunedBlockNumber(); err == nil {
			to.WriteLastPrunedBlockNumber(last)
		}
		var progress rebuildProgress
		if enc, err := os.ReadFile(filepath.Join(work, "rebuild.progress")); err == nil {
			json.Unmarshal(enc, &progress)
		}
		const step = 10_000
		for start := uint64(0); start <= progress.Block; start += step {
			m := from.ReadPruningMarks(start, start+step)
			to.WritePruningMarks(m)
			marks += len(m)
		}
	}
	from.Close()

	// The head is kept: a node rewinds to the newest block with a state.
	head := to.ReadHeadBlockHash()
	number := to.ReadHeaderNumber(head)
	err = nil
	if number == nil {
		err = errors.New("no head block")
	} else {
		sdb := state.NewDatabase(to)
		for n := *number; ; n-- {
			h := to.ReadHeader(to.ReadCanonicalHash(n), n)
			if h == nil {
				err = fmt.Errorf("no header for block %d", n)
				break
			}
			if _, serr := state.New(h.Root, sdb, nil, nil); serr == nil {
				fmt.Fprintf(w, "head block %d, newest block with a state %d (root %x), %d pruning marks\n", *number, n, h.Root, marks)
				break
			}
			if *number-n > 1_000_000 || n == 0 {
				err = fmt.Errorf("no state in the 1M blocks below the head %d", *number)
				break
			}
		}
	}
	to.Close()
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, o.out); err != nil {
		return err
	}
	return os.RemoveAll(work)
}

// copyTree copies the regular files of a directory tree, but LOCK files, and
// returns the bytes copied.
func copyTree(src, dst string) (int64, error) {
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
		if !info.Mode().IsRegular() || info.Name() == "LOCK" || info.Name() == "FLOCK" {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, in)
		total += n
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	})
	return total, err
}
