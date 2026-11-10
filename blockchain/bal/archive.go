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

package bal

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/storage/database"
)

var logger = log.NewModuleLogger(log.Blockchain)

// ErrNotCovered is returned for a state root that is not of a block the BAL
// history serves.
var ErrNotCovered = errors.New("state is not covered by the BAL history")

// BaseFunc returns the state of block number, whose root is root, which the
// index starts from.
type BaseFunc func(number uint64, root common.Hash) (BaseState, error)

// Archive keeps the BAL index of a chain database up to date with its stored
// lists and serves the state of the indexed blocks. The index starts from the
// block before the first stored list, whose state comes from Base.
type Archive struct {
	chain   database.DBManager
	indexDB database.Database
	base    BaseFunc
	lists   *cachedLists

	mu    sync.Mutex // serialises Update
	gen   atomic.Uint64
	reset atomic.Bool
}

func NewArchive(chain database.DBManager, indexDB database.Database, base BaseFunc) *Archive {
	cache, _ := lru.New(256)
	return &Archive{chain: chain, indexDB: indexDB, base: base, lists: &cachedLists{db: chain, cache: cache}}
}

// cachedLists reads the writes of the stored lists, keeping the last ones.
type cachedLists struct {
	db    database.DBManager
	cache *lru.Cache
}

func (l *cachedLists) Read(number uint64) (*access.BlockAccessList, error) {
	if v, ok := l.cache.Get(number); ok {
		return v.(*access.BlockAccessList), nil
	}
	list, err := Read(l.db, number)
	if err != nil {
		return nil, err
	}
	l.cache.Add(number, list)
	return list, nil
}

// Truncate drops the index if it covers blocks after number, which are gone.
func (a *Archive) Truncate(number uint64) {
	if ix, err := OpenIndex(a.indexDB); err == nil && ix != nil && ix.Meta().Next > number {
		a.reset.Store(true)
	}
}

// Covered returns the blocks the index covers, base to next-1.
func (a *Archive) Covered() (base, next uint64, ok bool) {
	ix, err := OpenIndex(a.indexDB)
	if err != nil || ix == nil {
		return 0, 0, false
	}
	return ix.Meta().Base, ix.Meta().Next, true
}

// flushBytes is roughly how much an update buffers before writing the index.
var flushBytes = 64 << 20

// Update indexes the stored lists after the end of the index. The index is
// rebuilt if the stored lists start at another block, or after Truncate. It
// stops early, keeping what it indexed, when stop is closed.
func (a *Archive) Update(stop <-chan struct{}) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	first, next := a.chain.ReadBALRange()
	if first == next {
		return nil
	}
	base := max(first, 1) - 1
	header := a.chain.ReadHeader(a.chain.ReadCanonicalHash(base), base)
	if header == nil {
		return fmt.Errorf("no header of block %d", base)
	}
	if ix, err := OpenIndex(a.indexDB); err != nil {
		return err
	} else if ix != nil {
		meta := ix.Meta()
		replaced, err := a.replaced(ix)
		if err != nil {
			return err
		}
		if a.reset.Load() || replaced || meta.Base != base || meta.BaseRoot != header.Root || meta.Next > next {
			logger.Info("Rebuilding the BAL index", "base", base, "lists", next-first, "oldBase", meta.Base, "oldNext", meta.Next)
			a.gen.Add(1)
			a.lists.cache.Purge()
			if err := ResetIndex(a.indexDB); err != nil {
				return err
			}
		}
	}
	a.reset.Store(false)
	builder, err := NewIndexBuilder(a.indexDB, base, header.Root)
	if err != nil {
		return err
	}
	if builder.Next() == base+1 {
		builder.AddRoot(base, header.Root)
	}
	for n := builder.Next(); n < next; n++ {
		select {
		case <-stop:
			return builder.Flush()
		default:
		}
		hash := a.chain.ReadCanonicalHash(n)
		if common.EmptyHash(hash) {
			break // stored before the block is canonical: indexed with the next update
		}
		list, err := Read(a.chain, n)
		if err != nil {
			return err
		}
		h := a.chain.ReadHeader(hash, n)
		if h == nil {
			return fmt.Errorf("no header of block %d", n)
		}
		if err := builder.Add(n, list); err != nil {
			return err
		}
		builder.AddRoot(n, h.Root)
		if builder.Size() > flushBytes {
			if err := builder.Flush(); err != nil {
				return err
			}
		}
	}
	return builder.Flush()
}

// replaced reports whether the state root of the last indexed block is not
// indexed, so that the index holds the lists of blocks a reorg replaced.
func (a *Archive) replaced(ix *Index) (bool, error) {
	last := ix.Meta().Next - 1
	h := a.chain.ReadHeader(a.chain.ReadCanonicalHash(last), last)
	if h == nil {
		return true, nil
	}
	number, ok, err := ix.BlockOf(h.Root)
	if err != nil || !ok {
		return !ok, err
	}
	other := a.chain.ReadHeader(a.chain.ReadCanonicalHash(number), number)
	return other == nil || other.Root != h.Root, nil
}

// StateAt opens the state with the given root from the BAL history, reading
// what no list changed from the state trie of the block in trie if it is
// there, or from the base of the history otherwise. It returns ErrNotCovered
// if root is not the root of a canonical indexed block.
func (a *Archive) StateAt(root common.Hash, trie state.Database) (*state.StateDB, error) {
	gen := a.gen.Load()
	ix, err := OpenIndex(a.indexDB)
	if err != nil {
		return nil, err
	}
	if ix == nil {
		return nil, ErrNotCovered
	}
	number, ok, err := ix.BlockOf(root)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotCovered
	}
	if h := a.chain.ReadHeader(a.chain.ReadCanonicalHash(number), number); h == nil || h.Root != root {
		return nil, ErrNotCovered
	}
	valid := func() bool { return a.gen.Load() == gen }
	if trie != nil {
		if sdb, err := state.New(root, trie, nil, nil); err == nil {
			h := NewHistory(ix, a.lists, &lockedBase{base: access.NewStateBase(sdb)})
			db, err := NewDatabaseOnTrie(h, number, root, trie, valid)
			if err != nil {
				return nil, err
			}
			return state.New(root, db, nil, nil)
		}
	}
	meta := ix.Meta()
	base, err := a.base(meta.Base, meta.BaseRoot)
	if err != nil {
		return nil, fmt.Errorf("base state of the BAL history at block %d: %w", meta.Base, err)
	}
	h := NewHistory(ix, a.lists, &lockedBase{base: base})
	return state.New(root, NewDatabase(h, number, root, valid), nil, nil)
}

// History returns the history of the indexed blocks, reading what no list
// changed from the base of the index and keeping up to cached decoded lists,
// and whether it is still valid: it is not after the index was rebuilt.
func (a *Archive) History(cached int) (*History, func() bool, error) {
	gen := a.gen.Load()
	ix, err := OpenIndex(a.indexDB)
	if err != nil {
		return nil, nil, err
	}
	if ix == nil {
		return nil, nil, ErrNotCovered
	}
	meta := ix.Meta()
	base, err := a.base(meta.Base, meta.BaseRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("base state of the BAL history at block %d: %w", meta.Base, err)
	}
	h := NewHistoryWithCache(ix, a.lists, &lockedBase{base: base}, cached)
	return h, func() bool { return a.gen.Load() == gen }, nil
}

// lockedBase serialises the reads of a base state, which copies of a state
// share.
type lockedBase struct {
	mu   sync.Mutex
	base BaseState
}

func (b *lockedBase) Account(addr common.Address) (account.Account, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.base.Account(addr)
}

func (b *lockedBase) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.base.Storage(addr, slot)
}

func (b *lockedBase) Code(codeHash common.Hash) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.base.Code(codeHash)
}

// Slots lists the slots of the base if it can.
func (b *lockedBase) Slots(addr common.Address) ([]common.Hash, error) {
	l, ok := b.base.(interface {
		Slots(addr common.Address) ([]common.Hash, error)
	})
	if !ok {
		return nil, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return l.Slots(addr)
}
