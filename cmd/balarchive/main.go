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

// balarchive builds and checks an archive made of a BAL freezer, a per-key
// index and state checkpoints. It is an experimental tool, run on a stopped
// node or on a copy.
//
//	balarchive index   -bal DIR -index DIR [-chaindata DIR] [-to N]
//	balarchive verify  -bal DIR -index DIR -chaindata DIR -at N [-window W] [-max K]
//	balarchive get     -bal DIR -index DIR -chaindata DIR -at N -addr A [-slot S]
//	balarchive export  -chaindata DIR -checkpoints DIR -at N
//	balarchive regen   -bal DIR -checkpoints DIR -chaindata DIR -id ID -to N [-addr A]
//	balarchive inspect -checkpoints DIR
package main

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/checkpoint"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	storecp "github.com/kaiachain/kaia/storage/database/checkpoint"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmds := map[string]func([]string) error{
		"index": cmdIndex, "verify": cmdVerify, "get": cmdGet,
		"export": cmdExport, "regen": cmdRegen, "inspect": cmdInspect,
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		usage()
	}
	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: balarchive index|verify|get|export|regen|inspect [flags], see -h of each")
	os.Exit(2)
}

type opts struct {
	fs                                       *flag.FlagSet
	balDir, indexDir, chaindata, checkpoints *string
	at, to, id, window, max, flushMB         *uint64
	addr, slot                               *string
}

func newOpts(name string) *opts {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return &opts{
		fs:          fs,
		balDir:      fs.String("bal", "", "BAL freezer directory (--bal.record-dir)"),
		indexDir:    fs.String("index", "", "index LevelDB directory"),
		chaindata:   fs.String("chaindata", "", "chaindata directory of the node"),
		checkpoints: fs.String("checkpoints", "", "state checkpoint directory"),
		at:          fs.Uint64("at", 0, "block number"),
		to:          fs.Uint64("to", 0, "last block (0 = all)"),
		id:          fs.Uint64("id", 0, "checkpoint segment id"),
		window:      fs.Uint64("window", 1000, "verify: keys changed in this many blocks before -at"),
		max:         fs.Uint64("max", 20000, "verify: most keys to check"),
		flushMB:     fs.Uint64("flush", 1024, "index: buffered MiB before a flush"),
		addr:        fs.String("addr", "", "account address"),
		slot:        fs.String("slot", "", "storage slot"),
	}
}

func openChain(dir string) database.DBManager {
	return database.NewDBManager(&database.DBConfig{
		Dir: dir, DBType: database.LevelDB, NumStateTrieShards: 4,
		OpenFilesLimit: 1024, LevelDBCacheSize: 256,
	})
}

func stateAt(dbm database.DBManager, number uint64) (*state.StateDB, common.Hash, error) {
	header := dbm.ReadHeader(dbm.ReadCanonicalHash(number), number)
	if header == nil {
		return nil, common.Hash{}, fmt.Errorf("no header for block %d", number)
	}
	sdb, err := state.New(header.Root, state.NewDatabase(dbm), nil, nil)
	if err != nil {
		return nil, header.Root, fmt.Errorf("state of block %d (root %x), a full node commits every 128 blocks: %w", number, header.Root, err)
	}
	return sdb, header.Root, nil
}

func openIndexDB(dir string) (database.Database, error) {
	return database.NewLevelDBWithOption(dir, &opt.Options{
		OpenFilesCacheCapacity: 1024, BlockCacheCapacity: 256 * opt.MiB, WriteBuffer: 128 * opt.MiB,
	})
}

func cmdIndex(args []string) error {
	o := newOpts("index")
	o.fs.Parse(args)
	freezer, err := bal.OpenFreezer(*o.balDir, true)
	if err != nil {
		return err
	}
	defer freezer.Close()
	db, err := openIndexDB(*o.indexDir)
	if err != nil {
		return err
	}
	defer db.Close()
	var root common.Hash
	if *o.chaindata != "" {
		dbm := openChain(*o.chaindata)
		defer dbm.Close()
		root = dbm.ReadHeader(dbm.ReadCanonicalHash(0), 0).Root
	}
	builder, err := bal.NewIndexBuilder(db, 0, root)
	if err != nil {
		return err
	}
	end := freezer.Next()
	if *o.to != 0 && *o.to+1 < end {
		end = *o.to + 1
	}
	start, last := time.Now(), time.Now()
	from := builder.Next()
	for number := from; number < end; {
		encs, err := freezer.ReadRawRange(number, min(end-number, 100_000), 64<<20)
		if err != nil {
			return err
		}
		for _, enc := range encs {
			list, err := bal.Decode(enc)
			if err != nil {
				return fmt.Errorf("block %d: %w", number, err)
			}
			if err := builder.Add(number, list); err != nil {
				return err
			}
			number++
		}
		if uint64(builder.Size()) > *o.flushMB<<20 {
			if err := builder.Flush(); err != nil {
				return err
			}
		}
		if time.Since(last) > 30*time.Second {
			last = time.Now()
			rate := float64(number-from) / time.Since(start).Seconds()
			fmt.Printf("indexed %d/%d, %.0f blocks/s, eta %s\n", number, end, rate, time.Duration(float64(end-number)/rate)*time.Second)
		}
	}
	if err := builder.Flush(); err != nil {
		return err
	}
	fmt.Printf("indexed blocks %d-%d in %s\n", from, end-1, time.Since(start).Round(time.Second))
	return nil
}

// history opens the index with the base state from the chaindata.
func history(o *opts) (*bal.History, database.DBManager, func(), error) {
	freezer, err := bal.OpenFreezer(*o.balDir, true)
	if err != nil {
		return nil, nil, nil, err
	}
	db, err := openIndexDB(*o.indexDir)
	if err != nil {
		freezer.Close()
		return nil, nil, nil, err
	}
	dbm := openChain(*o.chaindata)
	closeAll := func() { dbm.Close(); db.Close(); freezer.Close() }
	index, err := bal.OpenIndex(db)
	if err != nil || index == nil {
		closeAll()
		return nil, nil, nil, fmt.Errorf("no index in %s: %v", *o.indexDir, err)
	}
	base, _, err := stateAt(dbm, index.Meta().Base)
	if err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	return bal.NewHistory(index, freezer, state.NewBALBase(base)), dbm, closeAll, nil
}

func accountFields(acc account.Account) []byte {
	if acc == nil {
		return nil
	}
	acc = acc.DeepCopy()
	if pa := account.GetProgramAccount(acc); pa != nil {
		pa.SetStorageRoot(common.ExtHash{})
	}
	enc, _ := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(acc))
	return enc
}

func cmdVerify(args []string) error {
	o := newOpts("verify")
	o.fs.Parse(args)
	h, dbm, closeAll, err := history(o)
	if err != nil {
		return err
	}
	defer closeAll()
	sdb, _, err := stateAt(dbm, *o.at)
	if err != nil {
		return err
	}
	freezer, err := bal.OpenFreezer(*o.balDir, true)
	if err != nil {
		return err
	}
	defer freezer.Close()

	// Keys changed just before the block, and keys of random older blocks.
	keys := make(checkpoint.Keys)
	addList := func(n uint64) error {
		list, err := freezer.Read(n)
		if err != nil {
			return err
		}
		keys.AddList(list)
		return nil
	}
	for n := *o.at; n > 0 && n+*o.window > *o.at && uint64(len(keys)) < *o.max; n-- {
		if err := addList(n); err != nil {
			return err
		}
	}
	rng := rand.New(rand.NewSource(int64(*o.at)))
	for i := 0; i < 200 && *o.at > 1; i++ {
		if err := addList(1 + uint64(rng.Int63n(int64(*o.at)))); err != nil {
			return err
		}
	}

	start := time.Now()
	var accounts, slots, bad int
	report := func(format string, a ...interface{}) {
		bad++
		if bad <= 20 {
			fmt.Printf("MISMATCH "+format+"\n", a...)
		}
	}
	for addr, ss := range keys {
		if uint64(accounts+slots) >= *o.max {
			break
		}
		accounts++
		acc, err := h.Account(addr, *o.at)
		if err != nil {
			return fmt.Errorf("account %s: %w", addr.Hex(), err)
		}
		if !bytes.Equal(accountFields(acc), accountFields(sdb.GetAccount(addr))) {
			report("account %s", addr.Hex())
		}
		code, err := h.Code(addr, *o.at)
		if err != nil {
			return fmt.Errorf("code %s: %w", addr.Hex(), err)
		}
		if crypto.Keccak256Hash(code) != crypto.Keccak256Hash(sdb.GetCode(addr)) {
			report("code %s", addr.Hex())
		}
		for slot := range ss {
			slots++
			v, err := h.Storage(addr, slot, *o.at)
			if err != nil {
				return fmt.Errorf("storage %s %x: %w", addr.Hex(), slot, err)
			}
			if want := sdb.GetState(addr, slot); v != want {
				report("storage %s %x: history %x, state %x", addr.Hex(), slot, v, want)
			}
		}
	}
	if err := sdb.Error(); err != nil {
		return err
	}
	fmt.Printf("block %d: checked %d accounts and %d slots in %s, %d mismatches\n", *o.at, accounts, slots, time.Since(start).Round(time.Millisecond), bad)
	if bad > 0 {
		return fmt.Errorf("%d mismatches", bad)
	}
	return nil
}

func cmdGet(args []string) error {
	o := newOpts("get")
	o.fs.Parse(args)
	h, _, closeAll, err := history(o)
	if err != nil {
		return err
	}
	defer closeAll()
	addr := common.HexToAddress(*o.addr)
	start := time.Now()
	balance, err := h.Balance(addr, *o.at)
	if err != nil {
		return err
	}
	nonce, err := h.Nonce(addr, *o.at)
	if err != nil {
		return err
	}
	fmt.Printf("block %d %s: balance %s nonce %d (%s)\n", *o.at, addr.Hex(), balance, nonce, time.Since(start))
	if *o.slot != "" {
		start = time.Now()
		v, err := h.Storage(addr, common.HexToHash(*o.slot), *o.at)
		if err != nil {
			return err
		}
		fmt.Printf("slot %s: %x (%s)\n", *o.slot, v, time.Since(start))
	}
	return nil
}

func cmdExport(args []string) error {
	o := newOpts("export")
	o.fs.Parse(args)
	dbm := openChain(*o.chaindata)
	defer dbm.Close()
	_, root, err := stateAt(dbm, *o.at)
	if err != nil {
		return err
	}
	store, err := storecp.Open(*o.checkpoints, nil)
	if err != nil {
		return err
	}
	defer store.Close()
	meta := storecp.Meta{ID: 1, BlockNumber: *o.at, BlockHash: dbm.ReadCanonicalHash(*o.at), StateRoot: root, Trigger: storecp.TriggerInterval}
	if latest, ok := store.Latest(); ok {
		meta.ID, meta.Base = latest.ID+1, latest.ID
	}
	sdb := state.NewDatabase(dbm)
	start := time.Now()
	out, stats, err := checkpoint.Export(store, meta, sdb.TrieDB().Node, sdb.ContractCode, nil)
	if err != nil {
		return err
	}
	fmt.Printf("segment %d (base %d) at block %d: %d nodes, %d tombstones, %d codes, %s in %s\n",
		out.ID, out.Base, out.BlockNumber, stats.Nodes, stats.Tombstones, stats.Codes, common.StorageSize(stats.Bytes), time.Since(start).Round(time.Second))
	return nil
}

func cmdRegen(args []string) error {
	o := newOpts("regen")
	o.fs.Parse(args)
	freezer, err := bal.OpenFreezer(*o.balDir, true)
	if err != nil {
		return err
	}
	defer freezer.Close()
	store, err := storecp.Open(*o.checkpoints, nil)
	if err != nil {
		return err
	}
	defer store.Close()
	dbm := openChain(*o.chaindata)
	defer dbm.Close()
	check := func(n uint64) (common.Hash, error) {
		header := dbm.ReadHeader(dbm.ReadCanonicalHash(n), n)
		if header == nil {
			return common.Hash{}, fmt.Errorf("no header for block %d", n)
		}
		return header.Root, nil
	}
	extra := make(checkpoint.Keys)
	if *o.addr != "" {
		extra.Add(common.HexToAddress(*o.addr))
	}
	start := time.Now()
	sdb, reads, err := checkpoint.Regen(store, *o.id, freezer, *o.to, extra, check)
	if err != nil {
		return err
	}
	fmt.Printf("rebuilt block %d from segment %d with %d checkpoint reads in %s, every root matched\n", *o.to, *o.id, reads, time.Since(start).Round(time.Millisecond))
	if *o.addr != "" {
		addr := common.HexToAddress(*o.addr)
		fmt.Printf("%s: balance %s nonce %d\n", addr.Hex(), sdb.GetBalance(addr), sdb.GetNonce(addr))
	}
	return nil
}

func cmdInspect(args []string) error {
	o := newOpts("inspect")
	o.fs.Parse(args)
	return storecp.Inspect(*o.checkpoints, os.Stdout)
}
