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

package state

import (
	"errors"

	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
)

var (
	// ErrReadOnly is returned for committing a state that has no state trie.
	ErrReadOnly = errors.New("the state is read-only, it has no state trie")
	// ErrNoProof is returned for a Merkle proof of a state that has no state trie.
	ErrNoProof = errors.New("state proofs are not available, the state has no state trie")
	// ErrNoIteration is returned for iterating a state that has no state trie.
	ErrNoIteration = errors.New("iterating the state is not available, the state has no state trie")
)

// Reader reads the state at the end of one block: its accounts, storage and
// code. A StateDB opened with one reads from it instead of the snapshot and
// the state trie, which then only hash, commit, prove and iterate.
type Reader interface {
	// Account returns the account, or nil if it does not exist. The caller
	// may modify it.
	Account(addr common.Address) (account.Account, error)
	// Storage returns the value of a storage slot, zero if it is not set.
	Storage(addr common.Address, slot common.Hash) (common.Hash, error)
	// Code returns the code of addr, whose hash is codeHash.
	Code(addr common.Address, codeHash common.Hash) ([]byte, error)
}

// NewWithReader opens the state with root whose accounts, storage and code
// are read from reader, and whose tries are opened from db.
func NewWithReader(root common.Hash, db Database, reader Reader) (*StateDB, error) {
	s, err := New(root, db, nil, nil)
	if err != nil {
		return nil, err
	}
	s.reader = reader
	return s, nil
}

// Reader returns the reader the state is read from, or nil if it is read from
// the snapshot and the state trie.
func (s *StateDB) Reader() Reader {
	return s.reader
}

// ReadOnly reports whether the state has no state trie, so it is read from
// its reader only and cannot be committed, proved or iterated.
func (s *StateDB) ReadOnly() bool {
	return ReadOnly(s.db)
}

// ReadOnly reports whether db holds no state trie, so a state on it is read
// from its reader only and cannot be committed, proved or iterated.
func ReadOnly(db Database) bool {
	_, ok := db.(*readOnlyDatabase)
	return ok
}

// readOnlyDatabase is the database of a state that has no state trie: its
// tries hold nothing, keep no write and hash to the root of the block.
type readOnlyDatabase struct {
	root   common.Hash
	trieDB *statedb.Database
}

// NewReadOnlyDatabase returns the database of a state with root that has no
// state trie, to open a StateDB on with NewWithReader.
func NewReadOnlyDatabase(root common.Hash) Database {
	return &readOnlyDatabase{root: root, trieDB: statedb.NewDatabase(database.NewMemoryDBManager())}
}

func (db *readOnlyDatabase) OpenTrie(root common.Hash, opts *statedb.TrieOpts) (Trie, error) {
	return &readOnlyTrie{root: db.root.ExtendZero()}, nil
}

func (db *readOnlyDatabase) OpenStorageTrie(addr common.Address, root common.ExtHash, opts *statedb.TrieOpts) (Trie, error) {
	return &readOnlyTrie{root: root}, nil
}

func (db *readOnlyDatabase) CopyTrie(t Trie) Trie {
	if t, ok := t.(*readOnlyTrie); ok {
		c := *t
		return &c
	}
	return t
}

func (db *readOnlyDatabase) ContractCode(codeHash common.Hash) ([]byte, error) {
	return nil, errors.New("code is read from the reader of the state: " + codeHash.Hex())
}

func (db *readOnlyDatabase) DeleteCode(codeHash common.Hash) {}

func (db *readOnlyDatabase) ContractCodeSize(codeHash common.Hash) (int, error) {
	code, err := db.ContractCode(codeHash)
	return len(code), err
}

func (db *readOnlyDatabase) TrieDB() *statedb.Database { return db.trieDB }
func (db *readOnlyDatabase) RLockGCCachedNode()        {}
func (db *readOnlyDatabase) RUnlockGCCachedNode()      {}

// readOnlyTrie is a trie of a state without a state trie. Its reads are never
// used, its writes are dropped, and it hashes to the root it was opened with,
// which does not follow the writes.
type readOnlyTrie struct {
	root common.ExtHash
}

func (t *readOnlyTrie) GetKey([]byte) []byte                              { return nil }
func (t *readOnlyTrie) TryGet(key []byte) ([]byte, error)                 { return nil, nil }
func (t *readOnlyTrie) TryUpdate(key, value []byte) error                 { return nil }
func (t *readOnlyTrie) TryUpdateWithKeys(_, _, _, _ []byte) error         { return nil }
func (t *readOnlyTrie) TryDelete(key []byte) error                        { return nil }
func (t *readOnlyTrie) Hash() common.Hash                                 { return t.root.Unextend() }
func (t *readOnlyTrie) HashExt() common.ExtHash                           { return t.root }
func (t *readOnlyTrie) NodeIterator(startKey []byte) statedb.NodeIterator { return noIterator{} }

func (t *readOnlyTrie) Commit(onleaf statedb.LeafCallback) (common.Hash, error) {
	return common.Hash{}, ErrReadOnly
}

func (t *readOnlyTrie) CommitExt(onleaf statedb.LeafCallback) (common.ExtHash, error) {
	return common.ExtHash{}, ErrReadOnly
}

func (t *readOnlyTrie) Prove(key []byte, fromLevel uint, proofDb database.DBManager) error {
	return ErrNoProof
}

// noIterator is a node iterator that fails at once.
type noIterator struct{}

func (noIterator) Next(bool) bool                 { return false }
func (noIterator) Error() error                   { return ErrNoIteration }
func (noIterator) Hash() common.Hash              { return common.Hash{} }
func (noIterator) Parent() common.Hash            { return common.Hash{} }
func (noIterator) Path() []byte                   { return nil }
func (noIterator) Leaf() bool                     { return false }
func (noIterator) LeafKey() []byte                { return nil }
func (noIterator) LeafBlob() []byte               { return nil }
func (noIterator) LeafProof() [][]byte            { return nil }
func (noIterator) AddResolver(database.DBManager) {}
