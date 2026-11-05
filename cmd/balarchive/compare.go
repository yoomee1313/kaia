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
	"flag"
	"fmt"
	"syscall"
	"time"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/checkpoint"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
)

func maxRSS() string {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return common.StorageSize(ru.Maxrss).String() // bytes on darwin
}

type compareDBs struct {
	freezer *bal.Freezer
	src     database.DBManager // headers
	dst     database.DBManager // trie nodes
	state   state.Database
	headers func(uint64) (*types.Header, error)
}

func openCompare(balDir, chaindata, stateDir string, cacheMiB int) (*compareDBs, func(), error) {
	freezer, err := bal.OpenFreezer(balDir, true)
	if err != nil {
		return nil, nil, err
	}
	src := openChain(chaindata)
	dst := database.NewDBManager(&database.DBConfig{
		Dir: stateDir, DBType: database.LevelDB, NumStateTrieShards: 4, OpenFilesLimit: 1024, LevelDBCacheSize: 512,
	})
	cacheConfig := statedb.GetEmptyTrieNodeCacheConfig()
	cacheConfig.LocalCacheSizeMiB = cacheMiB
	c := &compareDBs{freezer: freezer, src: src, dst: dst, state: state.NewDatabaseWithNewCache(dst, cacheConfig)}
	c.headers = func(n uint64) (*types.Header, error) {
		h := src.ReadHeader(src.ReadCanonicalHash(n), n)
		if h == nil {
			return nil, fmt.Errorf("no header for block %d", n)
		}
		return h, nil
	}
	return c, func() { dst.Close(); src.Close(); freezer.Close() }, nil
}

// cmdRevert builds the state of a block from the state of a later block by
// setting the keys changed in between back with the history.
//
//	balarchive revert -bal DIR -index DIR -chaindata DIR -state DIR -head H -target T
func cmdRevert(args []string) error {
	fs := flag.NewFlagSet("revert", flag.ExitOnError)
	balDir := fs.String("bal", "", "BAL freezer directory")
	indexDir := fs.String("index", "", "index LevelDB directory")
	chaindata := fs.String("chaindata", "", "chaindata with the headers")
	stateDir := fs.String("state", "", "database with the state of the head")
	head := fs.Uint64("head", 0, "block whose state is the start")
	target := fs.Uint64("target", 0, "block whose state is built")
	cached := fs.Int("lists", 4096, "decoded lists kept in memory")
	cacheMiB := fs.Int("cache", 4096, "MiB of the trie node cache")
	fs.Parse(args)

	c, closeAll, err := openCompare(*balDir, *chaindata, *stateDir, *cacheMiB)
	if err != nil {
		return err
	}
	defer closeAll()
	db, err := openIndexDB(*indexDir)
	if err != nil {
		return err
	}
	defer db.Close()
	index, err := bal.OpenIndex(db)
	if err != nil || index == nil {
		return fmt.Errorf("no index in %s: %v", *indexDir, err)
	}
	genesis, err := c.headers(index.Meta().Base)
	if err != nil {
		return err
	}
	base, err := state.New(genesis.Root, c.state, nil, nil)
	if err != nil {
		return err
	}
	history := bal.NewHistoryWithCache(index, c.freezer, state.NewBALBase(base), *cached)
	h, err := c.headers(*head)
	if err != nil {
		return err
	}
	sdb, err := state.New(h.Root, c.state, nil, nil)
	if err != nil {
		return err
	}
	start := time.Now()
	stats, err := checkpoint.Revert(sdb, history, c.freezer, *target, *head, c.headers)
	if err != nil {
		return err
	}
	fmt.Printf("revert %d -> %d (%d blocks): %d accounts, %d slots, %d rebuilt (%d slots written), %d removed in %s, max RSS %s, root matched\n",
		*head, *target, *head-*target, stats.Accounts, stats.Slots, stats.Rebuilt, stats.Written, stats.Removed,
		time.Since(start).Round(time.Millisecond), maxRSS())
	return nil
}

// cmdForward builds the state of a block from the state of an earlier block
// by applying the lists in between, as Regen does from a checkpoint, and
// checks the root of the last block only.
//
//	balarchive forward -bal DIR -chaindata DIR -state DIR -from C -target T
func cmdForward(args []string) error {
	fs := flag.NewFlagSet("forward", flag.ExitOnError)
	balDir := fs.String("bal", "", "BAL freezer directory")
	chaindata := fs.String("chaindata", "", "chaindata with the headers")
	stateDir := fs.String("state", "", "database with the state of the start block")
	from := fs.Uint64("from", 0, "block whose state is the start, like a checkpoint")
	target := fs.Uint64("target", 0, "block whose state is built")
	workers := fs.Int("workers", 8, "goroutines hashing storage tries")
	cacheMiB := fs.Int("cache", 4096, "MiB of the trie node cache")
	fs.Parse(args)

	c, closeAll, err := openCompare(*balDir, *chaindata, *stateDir, *cacheMiB)
	if err != nil {
		return err
	}
	defer closeAll()
	h, err := c.headers(*from)
	if err != nil {
		return err
	}
	sdb, err := state.New(h.Root, c.state, nil, nil)
	if err != nil {
		return err
	}
	start := time.Now()
	accounts, slots := make(map[common.Address]struct{}), make(map[[2]common.Hash]struct{})
	for n := *from + 1; n <= *target; n++ {
		list, err := c.freezer.Read(n)
		if err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		hn, err := c.headers(n)
		if err != nil {
			return err
		}
		if err := list.CheckHash(hn.BlockAccessListHash); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		for i := range list.Accounts {
			a := &list.Accounts[i]
			if !a.Changed() {
				continue
			}
			accounts[a.Address] = struct{}{}
			for _, w := range a.StorageWrites {
				slots[[2]common.Hash{common.BytesToHash(a.Address[:]), w.Slot}] = struct{}{}
			}
		}
		if err := sdb.ApplyBlockAccessListUnchecked(list); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
	}
	sdb.HashStorages(*workers)
	ht, err := c.headers(*target)
	if err != nil {
		return err
	}
	if root := sdb.IntermediateRoot(true); root != ht.Root || sdb.Error() != nil {
		return fmt.Errorf("block %d: state root %x, header has %x (%v)", *target, root, ht.Root, sdb.Error())
	}
	fmt.Printf("forward %d -> %d (%d blocks): %d accounts, %d slots in %s, max RSS %s, root matched\n",
		*from, *target, *target-*from, len(accounts), len(slots), time.Since(start).Round(time.Millisecond), maxRSS())
	return nil
}
