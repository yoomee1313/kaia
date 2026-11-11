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

package impl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/kaiax/bal"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
)

// Sources that start from the genesis block instead of a stored state.
const (
	RegenGenesis = "genesis" // execute every block from the genesis state
	RegenImport  = "import"  // apply the lists stored by another node, checking every root
	RegenPeers   = "peers"   // apply the lists of other nodes fetched over p2p, checking every root
)

// Fetcher fetches the stored writes of the lists of other nodes, see the
// peers source.
type Fetcher interface {
	// FetchBALs returns the writes of the lists of blocks from to to, which
	// are in one segment. With a chain, they are the blocks of one of its
	// segments, checked against it: a node whose lists do not match is
	// dropped and the segment is fetched from another one.
	FetchBALs(ctx context.Context, chain *HashChain, from, to uint64) ([][]byte, error)
}

// importPrefetch is how many segments the peers source fetches ahead.
var importPrefetch uint64 = 4

// blocks between commits of the temporary state of a source from genesis
var regenCommitInterval uint64 = 2048

// SetGenesis gives the genesis the chain was made with, to rebuild its state.
func SetGenesis(bc *blockchain.BlockChain, genesis *blockchain.Genesis) {
	of(bc).genesis.Store(genesis)
}

// genesisSpec returns the genesis of the chain, to rebuild its state. It does
// not build the default genesis of a network, which would reset DeriveSha.
func (c *BALModule) genesisSpec(cfg *RegenConfig) (*blockchain.Genesis, error) {
	if cfg.Genesis != nil {
		return cfg.Genesis, nil
	}
	if g := c.genesis.Load(); g != nil {
		return g, nil
	}
	return nil, fmt.Errorf("no genesis of chain %d to start from", c.bc.Config().ChainID)
}

// runFromGenesis rebuilds the state from the genesis block in a temporary
// database, executing every block (genesis source) or applying the lists in
// the chaindata of another node (import source), checks the state root of
// every block, and commits the lists of the blocks from From on.
func (r *regen) runFromGenesis() {
	defer r.finish()

	var source *importLists
	switch r.cfg.Source {
	case RegenImport:
		src, closeSrc, err := r.c.openImport(&r.cfg)
		if err != nil {
			r.fail(err)
			return
		}
		defer closeSrc()
		source = &importLists{db: src, chain: r.cfg.HashChain, to: r.cfg.To}
	case RegenPeers:
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-r.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		source = &importLists{fetcher: r.cfg.Fetcher, ctx: ctx, chain: r.cfg.HashChain, to: r.cfg.To}
	}
	if c := r.cfg.HashChain; c != nil {
		logger.Info("Checking imported block access lists against the BAL hash chain", "last", c.Last, "root", c.Root())
	}
	genesis, err := r.c.genesisSpec(&r.cfg)
	if err != nil {
		r.fail(err)
		return
	}
	tempDir := filepath.Join(r.dir, "state")
	dbm := database.NewDBManager(&database.DBConfig{Dir: tempDir, DBType: database.LevelDB, OpenFilesLimit: 256, LevelDBCacheSize: 64, NumStateTrieShards: 1})
	defer os.RemoveAll(tempDir)
	defer dbm.Close()
	if root := genesis.ToBlock(common.Hash{}, dbm).Root(); root != r.c.bc.Genesis().Root() {
		r.fail(fmt.Errorf("genesis state root %x, chain has %x", root, r.c.bc.Genesis().Root()))
		return
	}

	cache := statedb.GetEmptyTrieNodeCacheConfig()
	cache.LocalCacheSizeMiB = int(r.cfg.Memory / 2 >> 20)
	sdbDB := state.NewDatabaseWithNewCache(dbm, cache)
	root := r.c.bc.Genesis().Root()
	sdb, err := state.New(root, sdbDB, nil, nil)
	if err != nil {
		r.fail(err)
		return
	}
	r.mu.Lock()
	r.status.Intervals = 1
	r.mu.Unlock()
	for n := uint64(1); n <= r.cfg.To; n++ {
		if r.stopped() {
			r.fail(errRegenStopped)
			return
		}
		block := r.c.bc.GetBlockByNumber(n)
		if block == nil {
			r.fail(fmt.Errorf("no block %d", n))
			return
		}
		var list *bal.BlockAccessList
		if source == nil {
			bal.EnableRecorderAt(sdb, 0)
			if _, _, _, _, _, err := r.c.bc.Processor().Process(block, sdb, r.c.vmConfig); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
			if err := sdb.Error(); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
			list = bal.Recorded(sdb)
		} else {
			if list, err = source.read(n); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
			if err := bal.Apply(sdb, list); err != nil {
				r.fail(fmt.Errorf("block %d: %w", n, err))
				return
			}
		}
		if got := sdb.IntermediateRoot(true); got != block.Root() {
			r.mu.Lock()
			r.status.Mismatches++
			r.mu.Unlock()
			r.fail(fmt.Errorf("block %d: state root %x, header has %x", n, got, block.Root()))
			return
		}
		// Imported lists hold the writes only; executed ones have the reads.
		if n >= r.cfg.From {
			if err := r.commitList(n, list, source == nil); err != nil {
				r.fail(err)
				return
			}
		}
		// The temporary state is written out now and then to bound memory.
		if n%regenCommitInterval == 0 || heapBytes() > r.cfg.Memory+heapBase() {
			if root, err = sdb.Commit(true); err != nil {
				r.fail(err)
				return
			}
			if err := sdbDB.TrieDB().Commit(root, false, n); err != nil {
				r.fail(err)
				return
			}
			if sdb, err = state.New(root, sdbDB, nil, nil); err != nil {
				r.fail(err)
				return
			}
		}
	}
	r.mu.Lock()
	r.status.Done = 1
	r.mu.Unlock()
	logger.Info("Rebuilt the state from genesis", "number", r.cfg.To)
}

// importLists reads the writes of the lists of another node, from its
// chaindata or, with a fetcher, from other nodes over p2p, up to the last
// block of the hash chain. The writes of a segment, whose keccak256 are the
// hashes of their blocks, are all read and checked against the chain before
// any list of it is decoded and returned.
type importLists struct {
	db      database.DBManager
	fetcher Fetcher
	ctx     context.Context
	chain   *HashChain
	to      uint64 // last block to import, at most the last of the chain

	from, next uint64                 // blocks of lists
	lists      []*bal.BlockAccessList // lists of blocks from to next-1
	ahead      map[uint64]chan fetchedBALs
}

type fetchedBALs struct {
	encs [][]byte
	err  error
}

func (l *importLists) read(n uint64) (*bal.BlockAccessList, error) {
	if !l.chain.Covers(n) {
		return nil, fmt.Errorf("%w: block %d is after the last block %d", ErrHashChain, n, l.chain.Last)
	}
	if n < l.from || n >= l.next {
		from, to := l.chain.SegmentBlocks(Segment(n))
		encs, err := l.segment(from, to)
		if err != nil {
			return nil, err
		}
		lists := make([]*bal.BlockAccessList, len(encs))
		for i, enc := range encs {
			if from+uint64(i) == 0 {
				lists[i] = &bal.BlockAccessList{}
				continue
			}
			list, err := bal.Decode(enc)
			if err != nil {
				return nil, fmt.Errorf("block %d: %w", from+uint64(i), err)
			}
			lists[i] = list
		}
		l.from, l.next, l.lists = from, to+1, lists
	}
	return l.lists[n-l.from], nil
}

// segment returns the writes of the segment of blocks from to to, checked
// against the chain.
func (l *importLists) segment(from, to uint64) ([][]byte, error) {
	if l.fetcher != nil {
		return l.fetch(from, to)
	}
	encs := make([][]byte, 0, to-from+1)
	hashes := make([]common.Hash, 0, to-from+1)
	for m := from; m <= to; m++ {
		if m == 0 {
			encs, hashes = append(encs, nil), append(hashes, EmptyListHash)
			continue
		}
		enc, err := ReadRaw(l.db, m)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", m, err)
		}
		encs, hashes = append(encs, enc), append(hashes, crypto.Keccak256Hash(enc))
	}
	if err := l.chain.VerifySegment(Segment(from), hashes); err != nil {
		return nil, fmt.Errorf("blocks %d-%d: %w", from, to, err)
	}
	return encs, nil
}

// fetch returns the writes of the segment of blocks from to to from other
// nodes, and starts fetching the next segments.
func (l *importLists) fetch(from, to uint64) ([][]byte, error) {
	if l.ahead == nil {
		l.ahead = make(map[uint64]chan fetchedBALs)
	}
	k := Segment(from)
	for next := k; next <= k+importPrefetch && next < uint64(len(l.chain.Roots)) && next*SegmentSize <= l.to; next++ {
		if _, ok := l.ahead[next]; ok {
			continue
		}
		ch := make(chan fetchedBALs, 1)
		l.ahead[next] = ch
		go func(k uint64) {
			from, to := l.chain.SegmentBlocks(k)
			encs, err := l.fetcher.FetchBALs(l.ctx, l.chain, from, to)
			ch <- fetchedBALs{encs, err}
		}(next)
	}
	res := <-l.ahead[k]
	delete(l.ahead, k)
	return res.encs, res.err
}

// commitList stores the list of a block, or compares its writes with the
// stored ones and, in reads mode, stores its reads if they match. hasReads
// tells whether the list holds the reads of the block, as an executed one
// does. Lists are committed in block order from From on.
func (r *regen) commitList(n uint64, list *bal.BlockAccessList, hasReads bool) error {
	hash := r.c.db.ReadCanonicalHash(n)
	writes, err := list.Writes().Encode()
	if err != nil {
		return fmt.Errorf("block %d: %w", n, err)
	}
	var reads []byte
	if hasReads && (r.cfg.Mode == RegenReads || (r.cfg.Mode == RegenWrite && !r.cfg.SkipReads)) {
		if reads, err = list.Reads().Encode(); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
	}
	if r.cfg.Mode == RegenWrite {
		if err := r.c.rec.store(n, hash, writes, reads, r.cfg.From); err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
	} else {
		stored := ReadBAL(r.c.db, hash, n)
		if stored == nil {
			return fmt.Errorf("block %d: %w", n, ErrNotStored)
		}
		if !bytes.Equal(stored, writes) {
			r.mu.Lock()
			r.status.Mismatches++
			r.mu.Unlock()
			logger.Warn("Regenerated block access list differs from the stored one", "number", n)
		} else if r.cfg.Mode == RegenReads {
			if err := WriteBALReads(r.c.db, hash, n, reads); err != nil {
				return fmt.Errorf("block %d: %w", n, err)
			}
		}
	}
	r.mu.Lock()
	r.status.Next = n + 1
	r.committed++
	r.mu.Unlock()
	return nil
}

// openImport opens the chaindata of another node to import its lists, and
// returns how to close it.
func (c *BALModule) openImport(cfg *RegenConfig) (database.DBManager, func(), error) {
	if cfg.ImportDB != nil {
		return cfg.ImportDB, func() {}, nil
	}
	if _, err := os.Stat(cfg.ImportDir); err != nil {
		return nil, nil, err
	}
	dbc := *c.db.GetDBConfig()
	dbc.Dir = cfg.ImportDir
	db := database.NewDBManager(&dbc)
	return db, db.Close, nil
}

// regenDir returns the directory of the work files of a regeneration.
func (c *BALModule) regenDir() (string, error) {
	if dir := c.db.GetDBConfig().Dir; dir != "" {
		return filepath.Join(dir, "balregen"), nil
	}
	return os.MkdirTemp("", "balregen")
}

// RecordedFirst returns the first block with a block access list, and
// whether the node records them.
func RecordedFirst(bc *blockchain.BlockChain) (uint64, bool) { return of(bc).recordedFirst() }

func (c *BALModule) recordedFirst() (uint64, bool) {
	if c.rec == nil {
		return 0, false
	}
	first, next := c.rec.span()
	if first == next {
		return c.bc.CurrentBlock().NumberU64() + 1, true
	}
	return first, true
}

// ErrNoHistory is returned for the state of a block before the BAL history.
var ErrNoHistory = errors.New("no history")

// HistoryError explains why the state of a block is missing: the history of
// the node starts later, and admin_startBALRegen can build the earlier part,
// or the BAL index has not reached the block yet.
func HistoryError(bc *blockchain.BlockChain, number uint64, err error) error {
	return of(bc).historyError(number, err)
}

func (c *BALModule) historyError(number uint64, err error) error {
	if c.cfg.HistoryRetention > 0 {
		return fmt.Errorf("%w: a light node keeps the recent states only: %v", ErrNoHistory, err)
	}
	first, ok := c.recordedFirst()
	if !ok {
		return err
	}
	if number < first {
		return fmt.Errorf("%w before block %d, regenerate it with admin_startBALRegen: %v", ErrNoHistory, first, err)
	}
	if base, next, ok := c.indexed(); !ok || number >= next || number < base {
		return fmt.Errorf("%w yet: the BAL index covers blocks %d to %d: %v", ErrNoHistory, base, int64(next)-1, err)
	}
	return err
}
