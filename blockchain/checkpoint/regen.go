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
	"fmt"
	"sort"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/database/checkpoint"
	"github.com/kaiachain/kaia/storage/statedb"
)

// Keys are the accounts and slots whose trie paths a rebuilt state must hold.
type Keys map[common.Address]map[common.Hash]bool

// Add adds an account and some of its slots.
func (k Keys) Add(addr common.Address, slots ...common.Hash) {
	m := k[addr]
	if m == nil {
		m = make(map[common.Hash]bool)
		k[addr] = m
	}
	for _, slot := range slots {
		m[slot] = true
	}
}

// AddList adds every account and written slot of a block access list.
func (k Keys) AddList(list *bal.BlockAccessList) {
	for i := range list.Accounts {
		a := &list.Accounts[i]
		k.Add(a.Address)
		for _, w := range a.StorageWrites {
			k.Add(a.Address, w.Slot)
		}
	}
}

type hashedKey struct {
	hash common.Hash
	path []byte
}

func sortedPaths[T comparable](keys map[T]bool, hash func(T) common.Hash) []hashedKey {
	out := make([]hashedKey, 0, len(keys))
	for k := range keys {
		h := hash(k)
		out = append(out, hashedKey{hash: h, path: statedb.KeybytesToHex(h[:])})
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].hash[:], out[j].hash[:]) < 0 })
	return out
}

// loader copies the nodes on the paths of some keys from a segment chain into
// a node database, with the children of every node on a path.
type loader struct {
	store *checkpoint.Store
	id    uint64
	dbm   database.DBManager
	seen  map[common.ExtHash]bool
	reads uint64
}

func (l *loader) load(key []byte, hash common.ExtHash) ([]byte, error) {
	blob, err := l.store.Get(l.id, key)
	l.reads++
	if err != nil || len(blob) == 0 {
		return nil, fmt.Errorf("%w: key %x: %v", errMissingNode, key, err)
	}
	if !l.seen[hash] {
		l.seen[hash] = true
		l.dbm.WriteTrieNode(hash, blob)
	}
	return blob, nil
}

// path loads the nodes from the root to target and returns the leaf value at
// target, or nil if the trie has none.
func (l *loader) path(root common.ExtHash, key func([]byte) []byte, target []byte) ([]byte, error) {
	cur := ref{path: []byte{}, hash: root}
	for {
		blob, err := l.load(key(cur.path), cur.hash)
		if err != nil {
			return nil, err
		}
		kids, lkey, value, err := children(cur.path, blob)
		if err != nil {
			return nil, err
		}
		if value != nil {
			if bytes.Equal(append(common.CopyBytes(cur.path), lkey...), target) {
				return value, nil
			}
			return nil, nil
		}
		// The other children are loaded too: a deletion resolves the child left
		// in a branch, and that child can be the one an extension pointed to,
		// once an insertion has split the extension.
		var next *ref
		for i := range kids {
			if hasPrefix(target, kids[i].path) {
				next = &kids[i]
			} else if _, err := l.load(key(kids[i].path), kids[i].hash); err != nil {
				return nil, err
			}
		}
		if next == nil {
			return nil, nil
		}
		cur = *next
	}
}

// Rebuild returns the state of the block of a segment with the nodes needed to
// read and update the given keys, in a new in-memory node database. Other
// parts of the trie stay unresolved, so only these keys may be used.
func Rebuild(store *checkpoint.Store, id uint64, keys Keys) (*state.StateDB, uint64, error) {
	meta, ok := store.Meta(id)
	if !ok {
		return nil, 0, fmt.Errorf("%w: %d", checkpoint.ErrUnknownSegment, id)
	}
	dbm := database.NewMemoryDBManager()
	l := &loader{store: store, id: id, dbm: dbm, seen: make(map[common.ExtHash]bool)}

	if refs := rootRefs(meta.StateRoot); len(refs) > 0 {
		accounts := make(map[common.Address]bool, len(keys))
		for addr := range keys {
			accounts[addr] = true
		}
		byHash := make(map[common.Hash]common.Address, len(keys))
		paths := sortedPaths(accounts, func(a common.Address) common.Hash {
			h := crypto.Keccak256Hash(a[:])
			byHash[h] = a
			return h
		})
		for _, p := range paths {
			value, err := l.path(refs[0].hash, accountPathKey, p.path)
			if err != nil {
				return nil, l.reads, err
			}
			if value == nil {
				continue
			}
			acc, err := decodeLeaf(value)
			if err != nil {
				return nil, l.reads, err
			}
			if acc.codeHash != (common.Hash{}) && !dbm.HasCode(acc.codeHash) {
				code, err := store.Get(id, codeKey(acc.codeHash))
				l.reads++
				if err != nil {
					return nil, l.reads, fmt.Errorf("code %x: %w", acc.codeHash, err)
				}
				dbm.WriteCode(acc.codeHash, code)
			}
			slots := keys[byHash[p.hash]]
			root := acc.storageRoot
			srefs := storageRefs(root)
			if len(slots) == 0 || len(srefs) == 0 {
				continue
			}
			key := func(path []byte) []byte { return storagePathKey(p.hash, path) }
			for _, s := range sortedPaths(slots, func(s common.Hash) common.Hash { return crypto.Keccak256Hash(s[:]) }) {
				if _, err := l.path(root, key, s.path); err != nil {
					return nil, l.reads, err
				}
			}
		}
	}
	sdb, err := state.New(meta.StateRoot, state.NewDatabase(dbm), nil, nil)
	return sdb, l.reads, err
}

// Regen rebuilds the state at block to from the segment id, at the block of
// the segment, by applying the block access lists after it. headers, if set,
// returns the header of a block: a list is checked against the list hash of
// the header before it is applied, and the state root after. extra holds the
// keys the caller reads afterwards, such as the ones of a proof.
func Regen(store *checkpoint.Store, id uint64, lists bal.ListReader, to uint64, extra Keys, headers func(uint64) (*types.Header, error)) (*state.StateDB, uint64, error) {
	meta, ok := store.Meta(id)
	if !ok {
		return nil, 0, fmt.Errorf("%w: %d", checkpoint.ErrUnknownSegment, id)
	}
	if to < meta.BlockNumber {
		return nil, 0, fmt.Errorf("block %d is before the checkpoint at %d", to, meta.BlockNumber)
	}
	keys := make(Keys)
	for addr, slots := range extra {
		for slot := range slots {
			keys.Add(addr, slot)
		}
		keys.Add(addr)
	}
	// The lists are read twice, for the keys and then to apply them, so that
	// they are not all held in memory.
	for n := meta.BlockNumber + 1; n <= to; n++ {
		list, err := lists.Read(n)
		if err != nil {
			return nil, 0, fmt.Errorf("block %d: %w", n, err)
		}
		keys.AddList(list)
	}
	sdb, reads, err := Rebuild(store, id, keys)
	if err != nil {
		return nil, reads, err
	}
	for n := meta.BlockNumber + 1; n <= to; n++ {
		list, err := lists.Read(n)
		if err != nil {
			return nil, reads, fmt.Errorf("block %d: %w", n, err)
		}
		var header *types.Header
		if headers != nil {
			if header, err = headers(n); err != nil {
				return nil, reads, err
			}
			if err := list.CheckHash(header.BlockAccessListHash); err != nil {
				return nil, reads, fmt.Errorf("block %d: %w", n, err)
			}
		}
		if err := sdb.ApplyBlockAccessList(list); err != nil {
			return nil, reads, fmt.Errorf("block %d: %w", n, err)
		}
		root := sdb.IntermediateRoot(true)
		if err := sdb.Error(); err != nil {
			return nil, reads, fmt.Errorf("block %d: %w", n, err)
		}
		if header != nil && root != header.Root {
			return nil, reads, fmt.Errorf("block %d: state root %x, want %x", n, root, header.Root)
		}
	}
	return sdb, reads, nil
}
