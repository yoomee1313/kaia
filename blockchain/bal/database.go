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

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
)

var (
	// ErrNoProof is returned for a Merkle proof, which needs the state trie.
	ErrNoProof = errors.New("state proofs are not available from the BAL history, it holds no state trie")
	// ErrNoIteration is returned for iterating a state, which needs the state trie.
	ErrNoIteration = errors.New("iterating the state is not available from the BAL history, it holds no state trie")
	// ErrReadOnly is returned for committing a state read from the BAL history.
	ErrReadOnly = errors.New("the state from the BAL history is read-only")
	// ErrRebuilt is returned by a state opened before the index was rebuilt.
	ErrRebuilt = errors.New("the BAL index was rebuilt, open the state again")

	// storageRoot is set on contract accounts: their storage is read by
	// address from the history and has no root.
	storageRoot = common.HexToHash("0xbabababababababababababababababababababababababababababababababa").ExtendZero()
)

// Database is a state.Database serving the state at the end of one block from
// a history. Without the state trie of the block, it is read-only and writes
// to its tries stay in memory; with it, writes, hashes, commits, proofs and
// iteration go to the trie.
type Database struct {
	history *History
	number  uint64
	root    common.Hash
	valid   func() bool

	trie     state.Database
	accounts state.Trie // the account trie of the block, for storage roots

	trieDB *statedb.Database
	mu     sync.Mutex
	codes  map[common.Hash][]byte
}

// NewDatabase returns the state at the end of block number, whose state root
// is root, read from h. valid, if not nil, reports whether h is still usable.
func NewDatabase(h *History, number uint64, root common.Hash, valid func() bool) *Database {
	return &Database{
		history: h, number: number, root: root, valid: valid,
		trieDB: statedb.NewDatabase(database.NewMemoryDBManager()),
		codes:  make(map[common.Hash][]byte),
	}
}

// NewDatabaseOnTrie returns the state at the end of block number, whose state
// root is root, read from h, whose base is the state of the block, and from
// the state trie of the block in trie.
func NewDatabaseOnTrie(h *History, number uint64, root common.Hash, trie state.Database, valid func() bool) (*Database, error) {
	accounts, err := trie.OpenTrie(root, nil)
	if err != nil {
		return nil, err
	}
	db := NewDatabase(h, number, root, valid)
	db.trie, db.accounts, db.trieDB = trie, accounts, trie.TrieDB()
	return db, nil
}

// ReadOnly reports whether db is a state from the BAL history without the
// state trie of its block.
func ReadOnly(db state.Database) bool {
	d, ok := db.(*Database)
	return ok && d.trie == nil
}

// Number returns the block of the state.
func (db *Database) Number() uint64 { return db.number }

func (db *Database) check() error {
	if db.valid != nil && !db.valid() {
		return ErrRebuilt
	}
	return nil
}

func (db *Database) OpenTrie(root common.Hash, opts *statedb.TrieOpts) (state.Trie, error) {
	t := &accountTrie{overlayTrie{overlay: make(map[string][]byte)}, db}
	if db.trie != nil {
		inner, err := db.trie.OpenTrie(root, opts)
		if err != nil {
			return nil, err
		}
		t.inner = inner
	}
	return t, nil
}

func (db *Database) OpenStorageTrie(addr common.Address, root common.ExtHash, opts *statedb.TrieOpts) (state.Trie, error) {
	fresh := common.EmptyExtHash(root) || root.Unextend() == types.EmptyRootHash
	if fresh && db.trie != nil {
		return db.trie.OpenStorageTrie(addr, root, opts)
	}
	t := &storageTrie{overlayTrie{overlay: make(map[string][]byte)}, db, addr, fresh}
	if db.trie != nil {
		inner, err := db.trie.OpenStorageTrie(addr, root, opts)
		if err != nil {
			return nil, err
		}
		t.inner = inner
	}
	return t, nil
}

func (db *Database) CopyTrie(t state.Trie) state.Trie {
	switch t := t.(type) {
	case *accountTrie:
		return &accountTrie{db.copyOverlay(t.overlayTrie), t.db}
	case *storageTrie:
		return &storageTrie{db.copyOverlay(t.overlayTrie), t.db, t.addr, t.fresh}
	}
	return t
}

func (db *Database) ContractCode(codeHash common.Hash) ([]byte, error) {
	db.mu.Lock()
	code, ok := db.codes[codeHash]
	db.mu.Unlock()
	if ok {
		return code, nil
	}
	if codeHash == crypto.Keccak256Hash(nil) {
		return nil, nil
	}
	if db.trie != nil {
		return db.trie.ContractCode(codeHash)
	}
	return nil, errors.New("code not loaded from the BAL history: " + codeHash.Hex())
}

func (db *Database) DeleteCode(codeHash common.Hash) {
	if db.trie != nil {
		db.trie.DeleteCode(codeHash)
	}
}

func (db *Database) ContractCodeSize(codeHash common.Hash) (int, error) {
	code, err := db.ContractCode(codeHash)
	return len(code), err
}

func (db *Database) TrieDB() *statedb.Database { return db.trieDB }

func (db *Database) RLockGCCachedNode() {
	if db.trie != nil {
		db.trie.RLockGCCachedNode()
	}
}

func (db *Database) RUnlockGCCachedNode() {
	if db.trie != nil {
		db.trie.RUnlockGCCachedNode()
	}
}

func (db *Database) account(addr common.Address) ([]byte, error) {
	if err := db.check(); err != nil {
		return nil, err
	}
	acc, err := db.history.Account(addr, db.number)
	if err != nil || acc == nil {
		return nil, err
	}
	if pa := account.GetProgramAccount(acc); pa != nil {
		code, err := db.history.Code(addr, db.number)
		if err != nil {
			return nil, err
		}
		db.mu.Lock()
		db.codes[crypto.Keccak256Hash(code)] = code
		db.mu.Unlock()
		root, err := db.storageRoot(addr)
		if err != nil {
			return nil, err
		}
		pa.SetStorageRoot(root)
	}
	return rlp.EncodeToBytes(account.NewAccountSerializerExtWithAccount(acc))
}

// storageRoot returns the storage root to set on a contract account: the one
// in the state trie of the block, or a placeholder without the trie.
func (db *Database) storageRoot(addr common.Address) (common.ExtHash, error) {
	if db.trie == nil {
		return storageRoot, nil
	}
	db.mu.Lock()
	enc, err := db.accounts.TryGet(addr[:])
	db.mu.Unlock()
	if err != nil {
		return common.ExtHash{}, err
	}
	if enc == nil {
		return common.ExtHash{}, fmt.Errorf("the BAL history has account %s, which the state trie of block %d has not", addr.Hex(), db.number)
	}
	ser := account.NewAccountSerializer()
	if err := rlp.DecodeBytes(enc, ser); err != nil {
		return common.ExtHash{}, err
	}
	pa := account.GetProgramAccount(ser.GetAccount())
	if pa == nil {
		return common.ExtHash{}, fmt.Errorf("account %s is a contract in the BAL history, not in the state trie of block %d", addr.Hex(), db.number)
	}
	return pa.GetStorageRoot(), nil
}

func (db *Database) storage(addr common.Address, slot []byte) ([]byte, error) {
	if err := db.check(); err != nil {
		return nil, err
	}
	value, err := db.history.Storage(addr, common.BytesToHash(slot), db.number)
	if err != nil || value == (common.Hash{}) {
		return nil, err
	}
	return rlp.EncodeToBytes(common.TrimLeftZeroes(value[:]))
}

func (db *Database) copyOverlay(t overlayTrie) overlayTrie {
	c := overlayTrie{overlay: make(map[string][]byte, len(t.overlay))}
	for k, v := range t.overlay {
		c.overlay[k] = v
	}
	if t.inner != nil {
		c.inner = db.trie.CopyTrie(t.inner)
	}
	return c
}

// overlayTrie keeps the writes made to a trie in memory, and passes them on to
// the state trie if there is one.
type overlayTrie struct {
	overlay map[string][]byte // nil value: deleted
	inner   state.Trie
}

func (t *overlayTrie) get(key []byte, read func() ([]byte, error)) ([]byte, error) {
	if v, ok := t.overlay[string(key)]; ok {
		return v, nil
	}
	return read()
}

func (t *overlayTrie) GetKey(key []byte) []byte {
	if t.inner != nil {
		return t.inner.GetKey(key)
	}
	return nil
}

func (t *overlayTrie) TryUpdate(key, value []byte) error {
	t.overlay[string(key)] = common.CopyBytes(value)
	if t.inner != nil {
		return t.inner.TryUpdate(key, value)
	}
	return nil
}

func (t *overlayTrie) TryUpdateWithKeys(key, hashKey, hexKey, value []byte) error {
	t.overlay[string(key)] = common.CopyBytes(value)
	if t.inner != nil {
		return t.inner.TryUpdateWithKeys(key, hashKey, hexKey, value)
	}
	return nil
}

func (t *overlayTrie) TryDelete(key []byte) error {
	t.overlay[string(key)] = nil
	if t.inner != nil {
		return t.inner.TryDelete(key)
	}
	return nil
}

func (t *overlayTrie) Commit(onleaf statedb.LeafCallback) (common.Hash, error) {
	if t.inner != nil {
		return t.inner.Commit(onleaf)
	}
	return common.Hash{}, ErrReadOnly
}

func (t *overlayTrie) CommitExt(onleaf statedb.LeafCallback) (common.ExtHash, error) {
	if t.inner != nil {
		return t.inner.CommitExt(onleaf)
	}
	return common.ExtHash{}, ErrReadOnly
}

func (t *overlayTrie) NodeIterator(startKey []byte) statedb.NodeIterator {
	if t.inner != nil {
		return t.inner.NodeIterator(startKey)
	}
	return errIterator{}
}

func (t *overlayTrie) Prove(key []byte, fromLevel uint, proofDb database.DBManager) error {
	if t.inner != nil {
		return t.inner.Prove(key, fromLevel, proofDb)
	}
	return ErrNoProof
}

type accountTrie struct {
	overlayTrie
	db *Database
}

func (t *accountTrie) TryGet(key []byte) ([]byte, error) {
	return t.get(key, func() ([]byte, error) { return t.db.account(common.BytesToAddress(key)) })
}

// Hash returns the root of the block without the state trie, which does not
// follow the writes.
func (t *accountTrie) Hash() common.Hash {
	if t.inner != nil {
		return t.inner.Hash()
	}
	return t.db.root
}

func (t *accountTrie) HashExt() common.ExtHash {
	if t.inner != nil {
		return t.inner.HashExt()
	}
	return t.db.root.ExtendZero()
}

type storageTrie struct {
	overlayTrie
	db    *Database
	addr  common.Address
	fresh bool
}

func (t *storageTrie) TryGet(key []byte) ([]byte, error) {
	return t.get(key, func() ([]byte, error) {
		if t.fresh {
			return nil, nil
		}
		return t.db.storage(t.addr, key)
	})
}

func (t *storageTrie) Hash() common.Hash {
	if t.inner != nil {
		return t.inner.Hash()
	}
	return storageRoot.Unextend()
}

func (t *storageTrie) HashExt() common.ExtHash {
	if t.inner != nil {
		return t.inner.HashExt()
	}
	return storageRoot
}

// errIterator is a node iterator that fails at once.
type errIterator struct{}

func (errIterator) Next(bool) bool                 { return false }
func (errIterator) Error() error                   { return ErrNoIteration }
func (errIterator) Hash() common.Hash              { return common.Hash{} }
func (errIterator) Parent() common.Hash            { return common.Hash{} }
func (errIterator) Path() []byte                   { return nil }
func (errIterator) Leaf() bool                     { return false }
func (errIterator) LeafKey() []byte                { return nil }
func (errIterator) LeafBlob() []byte               { return nil }
func (errIterator) LeafProof() [][]byte            { return nil }
func (errIterator) AddResolver(database.DBManager) {}
