// Modifications Copyright 2024 The Kaia Authors
// Modifications Copyright 2018 The klaytn Authors
// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
//
// This file is derived from core/state/statedb.go (2018/06/04).
// Modified and improved for the klaytn development.
// Modified and improved for the Kaia development.

package state

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/blockchain/types/accountkey"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/snapshot"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
)

type revision struct {
	id           int
	journalIndex int
}

var (
	// emptyState is the known hash of an empty state trie entry.
	emptyState = crypto.Keccak256Hash(nil)

	// emptyCode is the known hash of the empty EVM bytecode.
	emptyCode = crypto.Keccak256Hash(nil)

	logger = log.NewModuleLogger(log.BlockchainState)

	// TODO-Kaia EnabledExpensive and DBConfig.EnableDBPerfMetrics will be merged
	EnabledExpensive = false
)

// StateDBs within the Kaia protocol are used to cache stateObjects from Merkle Patricia Trie
// and mediate the operations to them.
type StateDB struct {
	db       Database
	trie     Trie // nil for a read-only state, see NewReadOnly
	trieOpts *statedb.TrieOpts
	reader   Reader      // reads accounts, storage and code before the trie, if set
	root     common.Hash // the root of a read-only state

	snap *snapshotState // the snapshot layer read from and the changes for the next one, nil without a snapshot

	// This map holds 'live' objects, which will get modified while processing a state transition.
	stateObjects             map[common.Address]*stateObject
	stateObjectsDirty        map[common.Address]struct{}
	stateObjectsDirtyStorage map[common.Address]struct{}

	// DB error.
	// State objects are used by the consensus core and VM which are
	// unable to deal with database-level errors. Any error that occurs
	// during a database read is memoized here and will eventually be returned
	// by StateDB.Commit.
	dbErr error

	// The refund counter, also used by state transitioning.
	refund uint64

	thash, bhash common.Hash
	txIndex      int
	logs         map[common.Hash][]*types.Log
	logSize      uint

	preimages map[common.Hash][]byte

	// Per-transaction access list
	accessList *accessList

	// Transient storage
	transientStorage transientStorage

	// Journal of state modifications. This is the backbone of
	// Snapshot and RevertToSnapshot.
	journal        *journal
	validRevisions []revision
	nextRevisionId int

	prefetching bool

	// Recorder of the block access list, nil unless started, and where its
	// next recording starts reading the journal.
	balRecorder *Recorder
	balOffset   int

	// Measurements gathered during execution for debugging purposes
	AccountReads         time.Duration
	AccountHashes        time.Duration
	AccountUpdates       time.Duration
	AccountCommits       time.Duration
	StorageReads         time.Duration
	StorageHashes        time.Duration
	StorageUpdates       time.Duration
	StorageCommits       time.Duration
	SnapshotAccountReads time.Duration
	SnapshotStorageReads time.Duration
	SnapshotCommits      time.Duration
}

// Create a new state from a given trie.
func New(root common.Hash, db Database, snaps *snapshot.Tree, opts *statedb.TrieOpts) (*StateDB, error) {
	// For FlatTrie, find the canonical block number from the state root. FlatTrie database is addressed by the block numbers, not state roots.
	if opts == nil {
		opts = &statedb.TrieOpts{}
	}
	if blockNum, err := blockNumberFromRoot(db.TrieDB().DiskDB(), root); err != nil {
		return nil, err
	} else {
		opts.BaseBlockNumber = blockNum
	}

	// Open the account trie.
	tr, err := db.OpenTrie(root, opts)
	if err != nil {
		return nil, err
	}

	// For FlatTrie, supply the account trie to the storage tries.
	if at, ok := tr.(*statedb.FlatAccountTrie); ok {
		opts.AccountTrie = at
	}

	sdb := &StateDB{
		db:                       db,
		trie:                     tr,
		trieOpts:                 opts,
		stateObjects:             make(map[common.Address]*stateObject),
		stateObjectsDirtyStorage: make(map[common.Address]struct{}),
		stateObjectsDirty:        make(map[common.Address]struct{}),
		logs:                     make(map[common.Hash][]*types.Log),
		preimages:                make(map[common.Hash][]byte),
		accessList:               newAccessList(),
		transientStorage:         newTransientStorage(),
		journal:                  newJournal(),
	}
	if sdb.snap = newSnapshotState(snaps, root); sdb.snap != nil {
		sdb.reader = snapshotReader{sdb.snap.layer}
	}
	if opts != nil && opts.Prefetching {
		sdb.prefetching = true
	}
	return sdb, nil
}

var (
	// ErrReadOnly is returned for committing a state that has no state trie.
	ErrReadOnly = errors.New("the state is read-only, it has no state trie")
	// ErrNoProof is returned for a Merkle proof of a state that has no state trie.
	ErrNoProof = errors.New("state proofs are not available, the state has no state trie")
	// ErrNoIteration is returned for iterating a state that has no state trie.
	ErrNoIteration = errors.New("iterating the state is not available, the state has no state trie")

	// errNotServed is returned by a Reader for a value it cannot serve, which
	// is then read from the state trie.
	errNotServed = errors.New("not served by the reader")
)

// Reader reads the state at the end of one block: its accounts, storage and
// code. A StateDB reads from its reader, the snapshot or one it is opened
// with, and from the state trie only what the reader does not serve. The
// state trie hashes, commits, proves and iterates.
type Reader interface {
	// Account returns the account, or nil if it does not exist. The caller
	// may modify it.
	Account(addr common.Address) (account.Account, error)
	// Storage returns the value of a storage slot, zero if it is not set.
	Storage(addr common.Address, slot common.Hash) (common.Hash, error)
	// Code returns the code of addr, whose hash is codeHash.
	Code(addr common.Address, codeHash common.Hash) ([]byte, error)
	// CodeSize returns the size of the code of addr, whose hash is codeHash.
	CodeSize(addr common.Address, codeHash common.Hash) (int, error)
}

// Reader returns the reader the state is read from, or nil if it is read from
// the state trie only.
func (s *StateDB) Reader() Reader {
	return s.reader
}

// NewReadOnly opens the state with root without its state trie: accounts,
// storage and code are read from reader, and code it does not serve from db.
// Its changes are kept in memory only: it does not hash them, and it cannot
// be committed, proved or iterated.
func NewReadOnly(root common.Hash, db Database, reader Reader) *StateDB {
	return &StateDB{
		db:                       db,
		trieOpts:                 &statedb.TrieOpts{},
		reader:                   reader,
		root:                     root,
		stateObjects:             make(map[common.Address]*stateObject),
		stateObjectsDirtyStorage: make(map[common.Address]struct{}),
		stateObjectsDirty:        make(map[common.Address]struct{}),
		logs:                     make(map[common.Hash][]*types.Log),
		preimages:                make(map[common.Hash][]byte),
		accessList:               newAccessList(),
		transientStorage:         newTransientStorage(),
		journal:                  newJournal(),
	}
}

// ReadOnly reports whether the state has no state trie, so it is read from
// its reader only and cannot be committed, proved or iterated.
func (s *StateDB) ReadOnly() bool {
	return s.trie == nil
}

func blockNumberFromRoot(dbm database.DBManager, root common.Hash) (uint64, error) {
	// Skip if not FlatTrie.
	dm := dbm.GetDomainsManager()
	if dm == nil {
		return 0, nil
	}

	// Skip if empty (i.e. temporary) trie or at genesis block.
	if common.EmptyHash(root) || root == types.EmptyRootHash {
		return 0, nil
	}

	num, ok, err := dm.ReadBlockNumByRoot(root.Bytes())
	if err != nil {
		logger.Warn("cannot find block number from stateRoot", "root", root.Hex(), "err", err)
		return 0, err
	} else if ok {
		return num, nil
	} else {
		logger.Warn("cannot find block number from stateRoot", "root", root.Hex(), "err", nil)
		return 0, fmt.Errorf("block number not found for root %s", root.Hex())
	}
}

// RLockGCCachedNode locks the GC lock of CachedNode.
func (s *StateDB) LockGCCachedNode() {
	s.db.RLockGCCachedNode()
}

// RUnlockGCCachedNode unlocks the GC lock of CachedNode.
func (s *StateDB) UnlockGCCachedNode() {
	s.db.RUnlockGCCachedNode()
}

// setError remembers the first non-nil error it is called with.
func (s *StateDB) setError(err error) {
	if s.dbErr == nil {
		s.dbErr = err
	}
}

func (s *StateDB) Error() error {
	return s.dbErr
}

// Reset clears out all ephemeral state objects from the state db, but keeps
// the underlying state trie to avoid reloading data for the next operations.
func (s *StateDB) Reset(root common.Hash) error {
	if s.ReadOnly() {
		return ErrReadOnly
	}
	blockNum, err := blockNumberFromRoot(s.db.TrieDB().DiskDB(), root)
	if err != nil {
		return err
	}
	s.trieOpts.BaseBlockNumber = blockNum

	tr, err := s.db.OpenTrie(root, s.trieOpts)
	if err != nil {
		return err
	}
	s.trie = tr
	s.stateObjects = make(map[common.Address]*stateObject)
	s.stateObjectsDirty = make(map[common.Address]struct{})
	s.thash = common.Hash{}
	s.bhash = common.Hash{}
	s.txIndex = 0
	s.logs = make(map[common.Hash][]*types.Log)
	s.logSize = 0
	s.preimages = make(map[common.Hash][]byte)
	s.clearJournalAndRefund()
	s.accessList = newAccessList()
	return nil
}

func (s *StateDB) AddLog(log *types.Log) {
	s.journal.append(addLogChange{txhash: s.thash})

	log.TxHash = s.thash
	log.BlockHash = s.bhash
	log.TxIndex = uint(s.txIndex)
	log.Index = s.logSize
	s.logs[s.thash] = append(s.logs[s.thash], log)
	s.logSize++
}

func (s *StateDB) GetLogs(hash common.Hash) []*types.Log {
	return s.logs[hash]
}

func (s *StateDB) Logs() []*types.Log {
	var logs []*types.Log
	for _, lgs := range s.logs {
		logs = append(logs, lgs...)
	}
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].Index < logs[j].Index
	})
	return logs
}

// AddPreimage records a SHA3 preimage seen by the VM.
func (s *StateDB) AddPreimage(hash common.Hash, preimage []byte) {
	if _, ok := s.preimages[hash]; !ok {
		s.journal.append(addPreimageChange{hash: hash})
		pi := make([]byte, len(preimage))
		copy(pi, preimage)
		s.preimages[hash] = pi
	}
}

// Preimages returns a list of SHA3 preimages that have been submitted.
func (s *StateDB) Preimages() map[common.Hash][]byte {
	return s.preimages
}

// AddRefund adds gas to the refund counter
func (s *StateDB) AddRefund(gas uint64) {
	s.journal.append(refundChange{prev: s.refund})
	s.refund += gas
}

// SubRefund removes gas from the refund counter.
// This method will panic if the refund counter goes below zero
func (s *StateDB) SubRefund(gas uint64) {
	s.journal.append(refundChange{prev: s.refund})
	if gas > s.refund {
		panic("Refund counter below zero")
	}
	s.refund -= gas
}

// Exist reports whether the given account address exists in the state.
// Notably this also returns true for self-destructed accounts.
func (s *StateDB) Exist(addr common.Address) bool {
	return s.getStateObject(addr) != nil
}

// Empty returns whether the state object is either non-existent
// or empty according to the EIP161 specification (balance = nonce = code = 0)
func (s *StateDB) Empty(addr common.Address) bool {
	so := s.getStateObject(addr)
	return so == nil || so.empty()
}

// Retrieve the balance from the given address or 0 if object not found
func (s *StateDB) GetBalance(addr common.Address) *big.Int {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Balance()
	}
	return common.Big0
}

func (s *StateDB) GetNonce(addr common.Address) uint64 {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Nonce()
	}
	return 0
}

func (s *StateDB) GetCode(addr common.Address) []byte {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Code(s.db)
	}
	return nil
}

func (s *StateDB) GetStorageRoot(addr common.Address) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Root()
	}
	return common.Hash{}
}

func (s *StateDB) GetAccount(addr common.Address) account.Account {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.account
	}
	return nil
}

func (s *StateDB) IsContractAccount(addr common.Address) bool {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.IsContractAccount()
	}
	return false
}

//func (self *StateDB) IsHumanReadable(addr common.Address) bool {
//	stateObject := self.getStateObject(addr)
//	if stateObject != nil {
//		return stateObject.HumanReadable()
//	}
//	return false
//}

func (s *StateDB) GetCodeSize(addr common.Address) int {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.CodeSize(s.db)
	}
	return 0
}

func (s *StateDB) GetCodeHash(addr common.Address) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return common.BytesToHash(emptyCodeHash)
	}
	return common.BytesToHash(stateObject.CodeHash())
}

// GetState retrieves a value from the given account's storage trie.
func (s *StateDB) GetState(addr common.Address, hash common.Hash) common.Hash {
	s.balRecorder.ReadSlot(addr, hash)
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetState(s.db, hash)
	}
	return common.Hash{}
}

// GetCommittedState retrieves a value from the given account's committed storage trie.
func (s *StateDB) GetCommittedState(addr common.Address, hash common.Hash) common.Hash {
	s.balRecorder.ReadSlot(addr, hash)
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetCommittedState(s.db, hash)
	}
	return common.Hash{}
}

// IsContractAvailable returns true if the account corresponding to the given address implements ProgramAccount.
func (s *StateDB) IsContractAvailable(addr common.Address) bool {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.IsContractAvailable()
	}
	return false
}

// IsProgramAccount returns true if the account corresponding to the given address implements ProgramAccount.
func (s *StateDB) IsProgramAccount(addr common.Address) bool {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.IsProgramAccount()
	}
	return false
}

func (s *StateDB) IsValidCodeFormat(addr common.Address) bool {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		pa := account.GetProgramAccount(stateObject.account)
		if pa != nil {
			return pa.GetCodeFormat().Validate()
		}
		return false
	}
	return false
}

// GetVmVersion returns true when the address is an SCA or an EOA with code.
func (s *StateDB) GetVmVersion(addr common.Address) (params.VmVersion, bool) {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		pa := account.GetProgramAccount(stateObject.account)
		if pa != nil && !(bytes.Equal(pa.GetCodeHash(), emptyCodeHash) && pa.Type() == account.ExternallyOwnedAccountType) {
			return pa.GetVmVersion(), true
		}
		return params.VmVersion0, false
	}
	return params.VmVersion0, false
}

func (s *StateDB) GetKey(addr common.Address) accountkey.AccountKey {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetKey()
	}
	return accountkey.NewAccountKeyLegacy()
}

// Database retrieves the low level database supporting the lower level trie ops.
func (s *StateDB) Database() Database {
	return s.db
}

// StorageTrie returns the storage trie of an account.
// The return value is a copy and is nil for non-existent accounts and for a
// read-only state.
func (s *StateDB) StorageTrie(addr common.Address) Trie {
	stateObject := s.getStateObject(addr)
	if stateObject == nil || s.ReadOnly() {
		return nil
	}
	cpy := stateObject.deepCopy(s)
	return cpy.updateStorageTrie(s.db)
}

func (s *StateDB) HasSelfDestructed(addr common.Address) bool {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.selfDestructed
	}
	return false
}

/*
 * SETTERS
 */

// AddBalance adds amount to the account associated with addr.
func (s *StateDB) AddBalance(addr common.Address, amount *big.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.AddBalance(amount)
	}
}

// SubBalance subtracts amount from the account associated with addr.
func (s *StateDB) SubBalance(addr common.Address, amount *big.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SubBalance(amount)
	}
}

func (s *StateDB) SetBalance(addr common.Address, amount *big.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetBalance(amount)
	}
}

// IncNonce increases the nonce of the account of the given address by one.
func (s *StateDB) IncNonce(addr common.Address) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.IncNonce()
	}
}

func (s *StateDB) SetNonce(addr common.Address, nonce uint64) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetNonce(nonce)
	}
}

func (s *StateDB) SetCode(addr common.Address, code []byte) error {
	stateObject := s.GetOrNewSmartContract(addr)
	if stateObject != nil {
		return stateObject.SetCode(crypto.Keccak256Hash(code), code)
	}

	return nil
}

func (s *StateDB) SetCodeToEOA(addr common.Address, code []byte, r params.Rules) error {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		pa := account.GetProgramAccount(stateObject.account)
		if pa == nil {
			return nil
		}
		// Journal CodeInfo via the stateObject so it is restored on revert.
		if bytes.Equal(code, []byte{}) {
			stateObject.SetCodeInfo(params.CodeInfo(0))
		} else {
			stateObject.SetCodeInfo(params.NewCodeInfoWithRules(params.CodeFormatEVM, r))
		}
		return stateObject.SetCode(crypto.Keccak256Hash(code), code)
	}

	return nil
}

func (s *StateDB) SetState(addr common.Address, key, value common.Hash) {
	stateObject := s.GetOrNewSmartContract(addr)
	if stateObject != nil {
		stateObject.SetState(s.db, key, value)
	}
}

// SetStorage replaces the entire storage for the specified account with given
// storage. This function should only be used for debugging.
func (s *StateDB) SetStorage(addr common.Address, storage map[common.Hash]common.Hash) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetStorage(storage)
	}
}

// UpdateKey updates the account's key with the given key.
func (s *StateDB) UpdateKey(addr common.Address, newKey accountkey.AccountKey, currentBlockNumber uint64) error {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.UpdateKey(newKey, currentBlockNumber)
	}

	return errAccountDoesNotExist
}

// SelfDestruct marks the given account as self-destructed.
// This clears the account balance.
//
// The account's state object is still available until the state is committed,
// getStateObject will return a non-nil account after SelfDestruct.
func (s *StateDB) SelfDestruct(addr common.Address) {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return
	}
	s.journal.append(selfDestructChange{
		account:     &addr,
		prev:        stateObject.selfDestructed,
		prevbalance: new(big.Int).Set(stateObject.Balance()),
	})
	stateObject.markSelfdestructed()
	stateObject.account.SetBalance(new(big.Int))
}

func (s *StateDB) SelfDestruct6780(addr common.Address) {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return
	}

	if stateObject.created {
		s.SelfDestruct(addr)
	}
}

// SetTransientState sets transient storage for a given account. It
// adds the change to the journal so that it can be rolled back
// to its previous value if there is a revert.
func (s *StateDB) SetTransientState(addr common.Address, key, value common.Hash) {
	prev := s.GetTransientState(addr, key)
	if prev == value {
		return
	}

	s.journal.append(transientStorageChange{
		account:  &addr,
		key:      key,
		prevalue: prev,
	})

	s.setTransientState(addr, key, value)
}

// setTransientState is a lower level setter for transient storage. It
// is called during a revert to prevent modifications to the journal.
func (s *StateDB) setTransientState(addr common.Address, key, value common.Hash) {
	s.transientStorage.Set(addr, key, value)
}

// GetTransientState gets transient storage for a given account.
func (s *StateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	return s.transientStorage.Get(addr, key)
}

//
// Setting, updating & deleting state object methods.
//

// updateStateObject writes the given object to the statedb.
func (s *StateDB) updateStateObject(stateObject *stateObject) {
	if s.ReadOnly() {
		return
	}
	// Track the amount of time wasted on updating the account from the trie
	if EnabledExpensive {
		defer func(start time.Time) { s.AccountUpdates += time.Since(start) }(time.Now())
	}
	addr := stateObject.Address()
	var snapshotData []byte
	if data := stateObject.encoded.Load(); data != nil {
		encodedData := data.(*encodedData)
		if encodedData.err != nil {
			panic(fmt.Errorf("can't encode object at %x: %v", addr[:], encodedData.err))
		}
		s.setError(s.trie.TryUpdateWithKeys(addr[:],
			encodedData.trieHashKey, encodedData.trieHexKey, encodedData.data))
		stateObject.encoded = atomic.Value{}
		snapshotData = encodedData.data
	} else {
		data, err := rlp.EncodeToBytes(stateObject)
		if err != nil {
			panic(fmt.Errorf("can't encode object at %x: %v", addr[:], err))
		}
		s.setError(s.trie.TryUpdate(addr[:], data))
		snapshotData = data
	}

	// If state snapshotting is active, cache the data til commit. Note, this
	// update mechanism is not symmetric to the deletion, because whereas it is
	// enough to track account updates at commit time, deletions need tracking
	// at transaction boundary level to ensure we capture state clearing.
	s.snap.updateAccount(stateObject.addrHash, snapshotData)
}

// deleteStateObject removes the given object from the state trie.
func (s *StateDB) deleteStateObject(stateObject *stateObject) {
	if s.ReadOnly() {
		return
	}
	// Track the amount of time wasted on deleting the account from the trie
	if EnabledExpensive {
		defer func(start time.Time) { s.AccountUpdates += time.Since(start) }(time.Now())
	}
	stateObject.deleted = true
	addr := stateObject.Address()
	s.setError(s.trie.TryDelete(addr[:]))
}

// getStateObject retrieves a state object given by the address, returning nil if
// the object is not found or was deleted in this execution context. If you need
// to differentiate between non-existent/just-deleted, use getDeletedStateObject.
func (s *StateDB) getStateObject(addr common.Address) *stateObject {
	if obj := s.getDeletedStateObject(addr); obj != nil && !obj.deleted {
		return obj
	}
	return nil
}

// getDeletedStateObject is similar to getStateObject, but instead of returning
// nil for a deleted state object, it returns the actual object with the deleted
// flag set. This is needed by the state journal to revert to the correct s-
// destructed object instead of wiping all knowledge about the state object.
func (s *StateDB) getDeletedStateObject(addr common.Address) *stateObject {
	// First, check stateObjects if there is "live" object.
	if obj := s.stateObjects[addr]; obj != nil {
		s.balRecorder.ReadAccount(addr)
		return obj
	}
	obj := s.loadStateObject(addr)
	s.balRecorder.Loaded(addr, stateAccount(obj))
	return obj
}

// loadStateObject loads the object for the given address from the reader or
// the database and inserts it into the live set. It returns nil if not found.
func (s *StateDB) loadStateObject(addr common.Address) *stateObject {
	var (
		acc account.Account
		err = errNotServed
	)
	if s.reader != nil {
		if EnabledExpensive && s.readsSnapshot() {
			defer func(start time.Time) { s.SnapshotAccountReads += time.Since(start) }(time.Now())
		}
		if acc, err = s.reader.Account(addr); err == nil && acc == nil {
			return nil
		}
		if err != nil && err != errNotServed {
			s.setError(err)
			return nil
		}
	}
	// If the reader did not serve it, load from the database
	if err == errNotServed {
		if s.ReadOnly() {
			return nil
		}
		// Track the amount of time wasted on loading the object from the database
		if EnabledExpensive {
			defer func(start time.Time) { s.AccountReads += time.Since(start) }(time.Now())
		}
		// Second, the object for given address is not cached.
		// Load the object from the database.
		enc, err := s.trie.TryGet(addr[:])
		if len(enc) == 0 {
			s.setError(err)
			return nil
		}
		serializer := account.NewAccountSerializer()
		if err := rlp.DecodeBytes(enc, serializer); err != nil {
			logger.Error("Failed to decode state object", "addr", addr, "err", err)
			return nil
		}
		acc = serializer.GetAccount()
	}
	// Insert into the live set.
	obj := newObject(s, addr, acc)
	s.setStateObject(obj)

	return obj
}

// readsSnapshot reports whether the reader is the snapshot.
func (s *StateDB) readsSnapshot() bool {
	_, ok := s.reader.(snapshotReader)
	return ok
}

func (s *StateDB) setStateObject(object *stateObject) {
	s.stateObjects[object.Address()] = object
}

// Retrieve a state object or create a new state object if nil.
func (s *StateDB) GetOrNewStateObject(addr common.Address) *stateObject {
	stateObject := s.getStateObject(addr)
	if stateObject == nil || stateObject.deleted {
		stateObject, _ = s.createObject(addr)
	}
	return stateObject
}

// Retrieve a state object or create a new state object if nil.
func (s *StateDB) GetOrNewSmartContract(addr common.Address) *stateObject {
	stateObject := s.getStateObject(addr)
	if stateObject == nil || stateObject.deleted {
		stateObject, _ = s.createObjectWithMap(addr, account.SmartContractAccountType, map[account.AccountValueKeyType]interface{}{
			account.AccountValueKeyNonce:         uint64(1),
			account.AccountValueKeyHumanReadable: false,
			account.AccountValueKeyAccountKey:    accountkey.NewAccountKeyFail(),
		})
	}
	return stateObject
}

// createObject creates a new state object. If there is an existing account with
// the given address, it is overwritten and returned as the second return value.
func (s *StateDB) createObject(addr common.Address) (newobj, prev *stateObject) {
	prev = s.getDeletedStateObject(addr) // Note, prev might have been deleted, we need that!

	var prevdestruct bool
	if prev != nil {
		prevdestruct = s.snap.markDestructed(prev.addrHash)
	}
	acc, err := account.NewAccountWithType(account.ExternallyOwnedAccountType)
	if err != nil {
		logger.Error("An error occurred on call NewAccountWithType", "err", err)
	}
	newobj = newObject(s, addr, acc)
	newobj.setNonce(0) // sets the object to dirty
	if prev == nil {
		s.journal.append(createObjectChange{account: &addr})
	} else {
		s.journal.append(resetObjectChange{prev: prev, prevdestruct: prevdestruct})
	}

	s.setStateObject(newobj)
	if prev != nil && !prev.deleted {
		return newobj, prev
	}
	return newobj, nil
}

// createObjectWithMap creates a new state object with the given parameters (accountType and values).
// If there is an existing account with the given address, it is overwritten and
// returned as the second return value.
func (s *StateDB) createObjectWithMap(addr common.Address, accountType account.AccountType,
	values map[account.AccountValueKeyType]interface{},
) (newobj, prev *stateObject) {
	acc, err := account.NewAccountWithMap(accountType, values)
	if err != nil {
		logger.Error("An error occurred on call NewAccountWithMap", "err", err)
	}
	newobj, prev = s.createObjectWithAccount(addr, acc)
	newobj.setNonce(0) // sets the object to dirty
	return newobj, prev
}

// createObjectWithAccount creates a new state object holding the given account.
// If there is an existing account with the given address, it is overwritten and
// returned as the second return value.
func (s *StateDB) createObjectWithAccount(addr common.Address, acc account.Account) (newobj, prev *stateObject) {
	prev = s.getDeletedStateObject(addr) // Note, prev might have been deleted, we need that!

	var prevdestruct bool
	if prev != nil {
		prevdestruct = s.snap.markDestructed(prev.addrHash)
	}
	newobj = newObject(s, addr, acc)
	if prev == nil {
		s.journal.append(createObjectChange{account: &addr})
	} else {
		s.journal.append(resetObjectChange{prev: prev, prevdestruct: prevdestruct})
	}

	s.setStateObject(newobj)
	if prev != nil && !prev.deleted {
		return newobj, prev
	}
	return newobj, nil
}

// CreateAccount explicitly creates a state object. If a state object with the address
// already exists, the balance is carried over to the new account.
// Carrying over the balance ensures that Ether doesn't disappear.
//
// CreateAccount is currently used for test code only. Instead,
// use CreateEOA, CreateSmartContractAccount, or CreateSmartContractAccountWithKey to create a typed account.
func (s *StateDB) CreateAccount(addr common.Address) {
	new, prev := s.createObject(addr)
	if prev != nil {
		new.setBalance(prev.account.GetBalance())
	}
}

func (s *StateDB) CreateEOA(addr common.Address, humanReadable bool, key accountkey.AccountKey) {
	values := map[account.AccountValueKeyType]interface{}{
		account.AccountValueKeyHumanReadable: humanReadable,
		account.AccountValueKeyAccountKey:    key,
	}
	new, prev := s.createObjectWithMap(addr, account.ExternallyOwnedAccountType, values)
	if prev != nil {
		new.setBalance(prev.account.GetBalance())
	}
}

func (s *StateDB) CreateSmartContractAccount(addr common.Address, format params.CodeFormat, r params.Rules) {
	s.CreateSmartContractAccountWithKey(addr, false, accountkey.NewAccountKeyFail(), format, r)
}

func (s *StateDB) CreateSmartContractAccountWithKey(addr common.Address, humanReadable bool, key accountkey.AccountKey, format params.CodeFormat, r params.Rules) {
	values := map[account.AccountValueKeyType]interface{}{
		account.AccountValueKeyNonce:         uint64(1),
		account.AccountValueKeyHumanReadable: humanReadable,
		account.AccountValueKeyAccountKey:    key,
		account.AccountValueKeyCodeInfo:      params.NewCodeInfoWithRules(format, r),
	}
	new, prev := s.createObjectWithMap(addr, account.SmartContractAccountType, values)
	new.created = true
	if prev != nil {
		new.setBalance(prev.account.GetBalance())
	}
}

// SetLegacyAccountForTest is setting an account as Legacy Account to StateDB.
// SetLegacyAccountForTest should be used for only tests.
func (s *StateDB) SetLegacyAccountForTest(addr common.Address, nonce uint64, balance *big.Int, root common.Hash, codeHash []byte) {
	acc := &account.LegacyAccount{
		Nonce:    nonce,
		Balance:  balance,
		Root:     root,
		CodeHash: codeHash,
	}
	newobj := newObject(s, addr, acc)
	s.journal.append(createObjectChange{account: &addr})
	newobj.created = true
	s.setStateObject(newobj)
}

func (s *StateDB) ForEachAccount(cb func(addr common.Address, data account.Account)) {
	if s.ReadOnly() {
		s.setError(ErrNoIteration)
		return
	}
	it := statedb.NewIterator(s.trie.NodeIterator(nil))
	for it.Next() {
		addr := s.trie.GetKey(it.Key)
		serializer := account.NewAccountSerializer()
		if err := rlp.DecodeBytes(it.Value, serializer); err != nil {
			panic(err)
		}
		data := serializer.GetAccount()
		cb(common.BytesToAddress(addr), data)
	}
}

func (s *StateDB) ForEachStorage(addr common.Address, cb func(key, value common.Hash) bool) {
	so := s.getStateObject(addr)
	if so == nil {
		return
	}
	if s.ReadOnly() {
		s.forEachReadOnlyStorage(so, cb)
		return
	}
	it := statedb.NewIterator(so.getStorageTrie(s.db).NodeIterator(nil))
	for it.Next() {
		key := common.BytesToHash(s.trie.GetKey(it.Key))
		if value, dirty := so.dirtyStorage[key]; dirty {
			cb(key, value)
			continue
		}
		enc := it.Value
		if len(enc) > 0 {
			_, content, _, _ := rlp.Split(enc)
			cb(key, common.BytesToHash(content))
		} else {
			cb(key, common.Hash{})
		}
	}
}

// SlotLister is a Reader that lists the slots an account may have set.
type SlotLister interface {
	Slots(addr common.Address) ([]common.Hash, error)
}

// forEachReadOnlyStorage calls cb with the set slots of so, a state object of
// a read-only state, in the order of their hashes like the storage trie: the
// slots the reader lists and the ones written in memory. ErrNoIteration is
// set if the reader cannot list slots.
func (s *StateDB) forEachReadOnlyStorage(so *stateObject, cb func(key, value common.Hash) bool) {
	l, ok := s.reader.(SlotLister)
	if !ok {
		s.setError(ErrNoIteration)
		return
	}
	slots, err := l.Slots(so.address)
	if err != nil {
		s.setError(err)
		return
	}
	hashed := make(map[common.Hash]common.Hash, len(slots)+len(so.dirtyStorage))
	for _, slot := range slots {
		hashed[crypto.Keccak256Hash(slot[:])] = slot
	}
	for slot := range so.dirtyStorage {
		hashed[crypto.Keccak256Hash(slot[:])] = slot
	}
	keys := make([]common.Hash, 0, len(hashed))
	for h := range hashed {
		keys = append(keys, h)
	}
	slices.SortFunc(keys, func(a, b common.Hash) int { return bytes.Compare(a[:], b[:]) })
	for _, h := range keys {
		slot := hashed[h]
		value := so.GetState(s.db, slot)
		if value == (common.Hash{}) {
			continue
		}
		if !cb(slot, value) {
			return
		}
	}
}

// Copy creates a deep, independent copy of the state.
// Snapshots of the copied state cannot be applied to the copy.
func (s *StateDB) Copy() *StateDB {
	state := &StateDB{}
	copyStateDB(state, s)
	return state
}

func (s *StateDB) Set(src *StateDB) {
	copyStateDB(s, src)
}

func copyStateDB(dst, src *StateDB) {
	// Copy all the basic fields, initialize the memory ones
	dst.db = src.db
	if src.trie != nil {
		dst.trie = src.db.CopyTrie(src.trie)
	} else {
		dst.trie = nil
	}
	dst.trieOpts = src.trieOpts
	dst.reader = src.reader
	dst.root = src.root

	dst.stateObjects = make(map[common.Address]*stateObject, len(src.stateObjects))
	dst.stateObjectsDirty = make(map[common.Address]struct{}, len(src.stateObjectsDirty))
	dst.stateObjectsDirtyStorage = make(map[common.Address]struct{}, len(src.stateObjectsDirtyStorage))

	dst.dbErr = src.dbErr
	dst.refund = src.refund

	dst.thash = src.thash
	dst.bhash = src.bhash
	dst.txIndex = src.txIndex
	dst.logs = make(map[common.Hash][]*types.Log, len(src.logs))
	dst.logSize = src.logSize

	dst.preimages = make(map[common.Hash][]byte, len(src.preimages))

	// Do we need to copy the access list? In practice: No. At the start of a
	// transaction, the access list is empty. In practice, we only ever copy state
	// _between_ transactions/blocks, never in the middle of a transaction.
	// However, it doesn't cost us much to copy an empty list, so we do it anyway
	// to not blow up if we ever decide copy it in the middle of a transaction
	dst.accessList = src.accessList.Copy()
	dst.transientStorage = src.transientStorage.Copy()
	dst.journal = newJournal()

	// The recorder is copied too, so that a state restored from a copy keeps
	// the changes recorded until the copy.
	dst.balRecorder = src.balRecorder.Copy()

	// Copy the dirty states, logs, and preimages
	for addr := range src.journal.dirties {
		// As documented [here](https://github.com/ethereum/go-ethereum/pull/16485#issuecomment-380438527),
		// and in the Finalise-method, there is a case where an object is in the journal but not
		// in the stateObjects: OOG after touch on ripeMD prior to Byzantium. Thus, we need to check for
		// nil
		if object, exist := src.stateObjects[addr]; exist {
			dst.stateObjects[addr] = object.deepCopy(dst)
			dst.stateObjectsDirty[addr] = struct{}{}
		}
	}
	// Above, we don't copy the actual journal. This means that if the copy is copied, the
	// loop above will be a no-op, since the copy's journal is empty.
	// Thus, here we iterate over stateObjects, to enable copies of copies
	for addr := range src.stateObjectsDirty {
		if _, exist := dst.stateObjects[addr]; !exist {
			dst.stateObjects[addr] = src.stateObjects[addr].deepCopy(dst)
			dst.stateObjectsDirty[addr] = struct{}{}
		}
	}
	for addr := range src.stateObjectsDirtyStorage {
		dst.stateObjectsDirtyStorage[addr] = struct{}{}
	}

	// Deep copy logs
	deepCopyLogs(src, dst)

	// Copy preimages
	maps.Copy(dst.preimages, src.preimages)

	// In order for the miner to be able to use and make additions to the
	// snapshot tree, we need to copy that aswell. Otherwise, any block mined by
	// ourselves will cause gaps in the tree, and force the miner to operate
	// trie-backed only.
	dst.snap = src.snap.copy()
}

// deepCopyLogs deep-copies StateDB.logs from the left to the right.
func deepCopyLogs(from, to *StateDB) {
	for hash, logs := range from.logs {
		copied := make([]*types.Log, len(logs))
		for i, log := range logs {
			copied[i] = new(types.Log)
			*copied[i] = *log
		}
		to.logs[hash] = copied
	}
}

// Snapshot returns an identifier for the current revision of the state.
func (s *StateDB) Snapshot() int {
	id := s.nextRevisionId
	s.nextRevisionId++
	s.validRevisions = append(s.validRevisions, revision{id, s.journal.length()})
	return id
}

// RevertToSnapshot reverts all state changes made since the given revision.
func (s *StateDB) RevertToSnapshot(revid int) {
	// Find the snapshot in the stack of valid snapshots.
	idx := sort.Search(len(s.validRevisions), func(i int) bool {
		return s.validRevisions[i].id >= revid
	})
	if idx == len(s.validRevisions) || s.validRevisions[idx].id != revid {
		panic(fmt.Errorf("revision id %v cannot be reverted", revid))
	}
	snapshot := s.validRevisions[idx].journalIndex

	// Replay the journal to undo changes and remove invalidated snapshots
	s.journal.revert(s, snapshot)
	s.validRevisions = s.validRevisions[:idx]

	// Reverting to the start of the journal drops a whole tx, as a block
	// producer does with a tx it cannot include, so its reads are dropped too.
	// A tx that reverts in the EVM keeps its reads: it is still included, and
	// its first snapshot is taken after the gas is bought.
	if snapshot == 0 {
		s.balRecorder.DropReads()
	}
}

// GetRefund returns the current value of the refund counter.
func (s *StateDB) GetRefund() uint64 {
	return s.refund
}

// Finalise finalises the state by removing the self destructed objects
// and clears the journal as well as the refunds.
func (stateDB *StateDB) Finalise(deleteEmptyObjects bool, setStorageRoot bool) {
	stateDB.balRecorder.Finalise(stateDB.balChanges(true, deleteEmptyObjects))
	for addr := range stateDB.journal.dirties {
		so, exist := stateDB.stateObjects[addr]
		if !exist {
			// ripeMD is 'touched' at block 1714175, in tx 0x1237f737031e40bcde4a8b7e717b2d15e3ecadfe49bb1bbc71ee9deb09c6fcf2
			// That tx goes out of gas, and although the notion of 'touched' does not exist there, the
			// touch-event will still be recorded in the journal. Since ripeMD is a special snowflake,
			// it will persist in the journal even though the journal is reverted. In this special circumstance,
			// it may exist in `stateDB.journal.dirties` but not in `stateDB.stateObjects`.
			// Thus, we can safely ignore it here
			continue
		}

		if so.selfDestructed || (deleteEmptyObjects && so.empty()) {
			stateDB.deleteStateObject(so)

			// If state snapshotting is active, also mark the destruction there.
			// Note, we can't do this only at the end of a block because multiple
			// transactions within the same block might self destruct and then
			// resurrect an account; but the snapshotter needs both events.
			stateDB.snap.deleteAccount(so.addrHash)
		} else {
			so.updateStorageTrie(stateDB.db)
			so.setStorageRoot(setStorageRoot, stateDB.stateObjectsDirtyStorage)
			stateDB.updateStateObject(so)
		}
		so.created = false
		stateDB.stateObjectsDirty[addr] = struct{}{}
	}
	// Invalidate journal because reverting across transactions is not allowed.
	stateDB.clearJournalAndRefund()

	if setStorageRoot && len(stateDB.stateObjectsDirtyStorage) > 0 {
		for addr := range stateDB.stateObjectsDirtyStorage {
			if so := stateDB.getStateObject(addr); so != nil {
				so.updateStorageRoot(stateDB.db)
				stateDB.updateStateObject(so)
			}
		}
		stateDB.stateObjectsDirtyStorage = make(map[common.Address]struct{})
	}
}

// IntermediateRoot computes the current root hash of the state statedb.
// It is called in between transactions to get the root hash that
// goes into transaction receipts.
func (s *StateDB) IntermediateRoot(deleteEmptyObjects bool) common.Hash {
	s.Finalise(deleteEmptyObjects, true)
	// Track the amount of time wasted on hashing the account trie
	if s.ReadOnly() {
		return s.root // a read-only state cannot hash its changes
	}
	if EnabledExpensive {
		defer func(start time.Time) { s.AccountHashes += time.Since(start) }(time.Now())
	}
	return s.trie.Hash()
}

// SetTxContext sets the current transaction hash and index and block hash which is
// used when the EVM emits new state logs.
func (s *StateDB) SetTxContext(thash, bhash common.Hash, ti int) {
	s.thash = thash
	s.bhash = bhash
	s.txIndex = ti
	s.SetBlockAccessIndex(ti + 1)
}

func (s *StateDB) clearJournalAndRefund() {
	s.journal = newJournal()
	s.validRevisions = s.validRevisions[:0]
	s.refund = 0
}

// Commit writes the state to the underlying in-memory trie database.
func (s *StateDB) Commit(deleteEmptyObjects bool) (root common.Hash, err error) {
	if s.ReadOnly() {
		return common.Hash{}, ErrReadOnly
	}
	if s.dbErr != nil {
		return common.Hash{}, fmt.Errorf("commit aborted due to earlier error: %v", s.dbErr)
	}

	defer s.clearJournalAndRefund()

	for addr := range s.journal.dirties {
		s.stateObjectsDirty[addr] = struct{}{}
	}

	objectEncoder := getStateObjectEncoder(len(s.stateObjects))
	var stateObjectsToUpdate []*stateObject
	// Commit objects to the trie.
	for addr, stateObject := range s.stateObjects {
		_, isDirty := s.stateObjectsDirty[addr]
		switch {
		case stateObject.selfDestructed || (isDirty && deleteEmptyObjects && stateObject.empty()):
			// If the object has been removed, don't bother syncing it
			// and just mark it for deletion in the trie.
			s.deleteStateObject(stateObject)
		case isDirty:
			// Write any contract code associated with the state object.
			if stateObject.code != nil && stateObject.dirtyCode {
				s.db.TrieDB().DiskDB().WriteCode(common.BytesToHash(stateObject.CodeHash()), stateObject.code)
				stateObject.dirtyCode = false
			}
			// Write any storage changes in the state object to its storage trie.
			if err := stateObject.CommitStorageTrie(s.db); err != nil {
				return common.Hash{}, err
			}
			// Update the object in the main account trie.
			stateObjectsToUpdate = append(stateObjectsToUpdate, stateObject)
			objectEncoder.encode(stateObject)
		}
		delete(s.stateObjectsDirty, addr)
	}

	for _, so := range stateObjectsToUpdate {
		s.updateStateObject(so)
	}

	// Write the account trie changes, measuring the amount of wasted time
	if EnabledExpensive {
		defer func(start time.Time) { s.AccountCommits += time.Since(start) }(time.Now())
	}
	root, err = s.trie.Commit(func(_ [][]byte, _ []byte, leaf []byte, parent common.ExtHash, parentDepth int) error {
		serializer := account.NewAccountSerializer()
		if err := rlp.DecodeBytes(leaf, serializer); err != nil {
			logger.Warn("RLP decode failed", "err", err, "leaf", string(leaf))
			return nil
		}
		acc := serializer.GetAccount()
		if pa := account.GetProgramAccount(acc); pa != nil {
			if pa.GetStorageRoot().Unextend() != emptyState {
				s.db.TrieDB().Reference(pa.GetStorageRoot(), parent)
			}
		}
		return nil
	})

	// If snapshotting is enabled, update the snapshot tree with this new version
	if s.snap != nil {
		if EnabledExpensive {
			defer func(start time.Time) { s.SnapshotCommits += time.Since(start) }(time.Now())
		}
		s.snap.commit(root)
		if s.readsSnapshot() {
			s.reader = nil
		}
		s.snap = nil
	}
	return root, err
}

// GetTxHash returns the hash of current running transaction.
func (s *StateDB) GetTxHash() common.Hash {
	return s.thash
}

var (
	errNotExistingAddress = errors.New("there is no account corresponding to the given address")
	errNotProgramAccount  = errors.New("given address is not a program account")
)

func (s *StateDB) GetContractStorageRoot(contractAddr common.Address) (common.ExtHash, error) {
	acc := s.GetAccount(contractAddr)
	if acc == nil {
		return common.ExtHash{}, errNotExistingAddress
	}
	pa := account.GetProgramAccount(acc)
	if pa == nil {
		return common.ExtHash{}, errNotProgramAccount
	}
	return pa.GetStorageRoot(), nil
}

// Prepare handles the preparatory steps for executing a state transition with.
// This method must be invoked before state transition.
//
// Kore fork:
// - Add sender to access list (2929)
// - Add feepayer to access list (only for Kaia)
// - Add destination to access list (2929)
// - Add precompiles to access list (2929)
//
// Shanghai fork:
// - Add coinbase to access list (EIP-3651)
// Potential EIPs:
// - Reset transient storage(1153)
func (s *StateDB) Prepare(rules params.Rules, sender, feepayer, coinbase common.Address, dst *common.Address, precompiles []common.Address, list types.AccessList) {
	if rules.IsKore {
		// Clear out any leftover from previous executions
		s.accessList = newAccessList()

		s.AddAddressToAccessList(sender)
		if !common.EmptyAddress(feepayer) {
			s.AddAddressToAccessList(feepayer)
		}
		if dst != nil {
			s.AddAddressToAccessList(*dst)
			// If it's a create-tx, the destination will be added inside evm.create
		}
		for _, addr := range precompiles {
			s.AddAddressToAccessList(addr)
		}
	}
	if rules.IsShanghai {
		s.AddAddressToAccessList(coinbase)
	}
	if rules.IsCancun {
		// Optional accessList is the accessList mentioned through tx args.
		for _, el := range list {
			s.AddAddressToAccessList(el.Address)
			for _, key := range el.StorageKeys {
				s.AddSlotToAccessList(el.Address, key)
			}
		}
	}
	// Reset transient storage at the beginning of transaction execution
	s.transientStorage = newTransientStorage()
}

// AddAddressToAccessList adds the given address to the access list
func (s *StateDB) AddAddressToAccessList(addr common.Address) {
	if s.accessList.AddAddress(addr) {
		s.journal.append(accessListAddAccountChange{&addr})
	}
}

// AddSlotToAccessList adds the given (address, slot)-tuple to the access list
func (s *StateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	addrMod, slotMod := s.accessList.AddSlot(addr, slot)
	if addrMod {
		// In practice, this should not happen, since there is no way to enter the
		// scope of 'address' without having the 'address' become already added
		// to the access list (via call-variant, create, etc).
		// Better safe than sorry, though
		s.journal.append(accessListAddAccountChange{&addr})
	}
	if slotMod {
		s.journal.append(accessListAddSlotChange{
			address: &addr,
			slot:    &slot,
		})
	}
}

// AddressInAccessList returns true if the given address is in the access list.
func (s *StateDB) AddressInAccessList(addr common.Address) bool {
	return s.accessList.ContainsAddress(addr)
}

// SlotInAccessList returns true if the given (address, slot)-tuple is in the access list.
func (s *StateDB) SlotInAccessList(addr common.Address, slot common.Hash) (addressPresent bool, slotPresent bool) {
	return s.accessList.Contains(addr, slot)
}

var errNoCodeAccount = errors.New("no account to apply code to")

// StartBAL starts recording the block access list at index idx: 0 for
// InitializeState, i+1 for the i-th tx, and len(txs)+1 for FinalizeState. The
// state must hold the start of that index, the parent state for 0: the
// accounts already loaded are the base of their changes.
func (s *StateDB) StartBAL(idx int) {
	r := NewRecorder(idx)
	for addr, obj := range s.stateObjects {
		acc := obj.account
		if obj.deleted {
			acc = nil
		}
		r.Base(addr, acc)
	}
	s.balRecorder, s.balOffset = r, 0
}

// BAL returns the block access list recorded since StartBAL, or nil if
// none is recorded. Changes are recorded at Finalise, so it is complete
// after IntermediateRoot of the block. Storage roots are not recorded.
func (s *StateDB) BAL() *types.BlockAccessList {
	return s.balRecorder.List()
}

// SetBlockAccessIndex moves the block access list on to index idx, see
// StartBAL.
func (s *StateDB) SetBlockAccessIndex(idx int) {
	s.balRecorder.StartIndex(idx, s.balChanges(false, false))
}

// EndBlockAccessInitialization ends index 0, InitializeState, which may not be
// finalised.
func (s *StateDB) EndBlockAccessInitialization() {
	s.balRecorder.EndInitialization(s.balChanges(false, false))
}

// balChanges returns a function that returns the accounts changed since the
// last recording, see accessChanges. If final is set, it runs at Finalise,
// which clears the journal.
func (s *StateDB) balChanges(final, deleteEmptyObjects bool) func() []Change {
	return func() []Change {
		changes, next := s.accessChanges(s.balOffset, final, deleteEmptyObjects)
		s.balOffset = next
		if final {
			s.balOffset = 0
		}
		return changes
	}
}

// stateAccount returns the account of obj, nil if there is none.
func stateAccount(obj *stateObject) account.Account {
	if obj == nil {
		return nil
	}
	return obj.account
}

// accessChanges returns the accounts changed by the journal entries from
// offset on, the same set Finalise writes to the trie, and the offset after
// them. With final it is called at Finalise, and reports the deletions that
// Finalise is about to apply.
func (s *StateDB) accessChanges(offset int, final, deleteEmptyObjects bool) ([]Change, int) {
	var entries []journalEntry
	if offset < len(s.journal.entries) {
		entries = s.journal.entries[offset:]
	}
	written := make(map[common.Address]map[common.Hash]common.Hash)
	recreated := make(map[common.Address]bool)
	for _, entry := range entries {
		switch e := entry.(type) {
		case storageChange:
			slots := written[*e.account]
			if slots == nil {
				slots = make(map[common.Hash]common.Hash)
				written[*e.account] = slots
			}
			if _, ok := slots[e.key]; !ok {
				slots[e.key] = e.prevalue
			}
		case resetObjectChange:
			recreated[e.prev.address] = true
		}
	}
	changes := make([]Change, 0, len(s.journal.dirties))
	for addr := range s.journal.dirties {
		obj, exist := s.stateObjects[addr]
		if !exist {
			continue
		}
		c := Change{
			Address:   addr,
			Deleted:   final && (obj.selfDestructed || (deleteEmptyObjects && obj.empty())),
			Recreated: recreated[addr],
			Account:   obj.account,
			Code:      func() []byte { return obj.Code(s.db) },
		}
		if pre := written[addr]; len(pre) > 0 && !c.Deleted {
			c.Slots = make(map[common.Hash]SlotWrite, len(pre))
			for slot, value := range pre {
				c.Slots[slot] = SlotWrite{Pre: value, Post: obj.GetState(s.db, slot)}
			}
		}
		changes = append(changes, c)
	}
	return changes, len(s.journal.entries)
}

// SetAccount replaces the whole account at addr with acc. The storage root of
// acc is not trusted: the current storage is kept, or dropped if clearStorage
// is set. It is used to apply a block access list.
func (s *StateDB) SetAccount(addr common.Address, acc account.Account, clearStorage bool) {
	acc = acc.DeepCopy()
	pa := account.GetProgramAccount(acc)
	prev := s.getStateObject(addr)
	if prev == nil || clearStorage {
		if pa != nil {
			pa.SetStorageRoot(common.ExtHash{})
		}
		newobj, _ := s.createObjectWithAccount(addr, acc)
		newobj.touch() // sets the object to dirty
		return
	}

	if pa != nil {
		var root common.ExtHash
		if prevPa := account.GetProgramAccount(prev.account); prevPa != nil {
			root = prevPa.GetStorageRoot()
		}
		pa.SetStorageRoot(root)
	}
	s.journal.append(accountChange{
		account:       &prev.address,
		prev:          prev.account,
		prevcode:      prev.code,
		prevDirtyCode: prev.dirtyCode,
	})
	codeHash := prev.CodeHash()
	prev.account = acc
	if !bytes.Equal(codeHash, prev.CodeHash()) {
		prev.code, prev.dirtyCode = nil, false
	}
}

// ApplyCode sets the code of the account at addr from a block access list.
// Unlike SetCode it does not load the previous code: a whole account record
// applied just before may already carry the new code hash, and that code is
// only in the list.
func (s *StateDB) ApplyCode(addr common.Address, code []byte) error {
	obj := s.getStateObject(addr)
	if obj == nil {
		return errNoCodeAccount
	}
	s.journal.append(codeChange{
		account:  &obj.address,
		prevhash: obj.CodeHash(),
		prevcode: obj.code,
	})
	return obj.setCode(crypto.Keccak256Hash(code), code)
}

// HashStorages computes the storage roots left out by Finalise with
// setStorageRoot unset, with up to workers storage tries hashed at a time, and
// writes the accounts to the account trie.
func (s *StateDB) HashStorages(workers int) {
	objs := make([]*stateObject, 0, len(s.stateObjectsDirtyStorage))
	for addr := range s.stateObjectsDirtyStorage {
		if so := s.getStateObject(addr); so != nil {
			objs = append(objs, so)
		}
	}
	s.stateObjectsDirtyStorage = make(map[common.Address]struct{})

	// Each object hashes its own storage trie, so they do not share state.
	var (
		wg   sync.WaitGroup
		next atomic.Int64
	)
	for range max(1, workers) {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i < len(objs); i = int(next.Add(1) - 1) {
				objs[i].updateStorageRoot(s.db)
			}
		})
	}
	wg.Wait()
	for _, so := range objs {
		s.updateStateObject(so)
	}
}

// ApplyBAL writes the final values of a block access list to the state, which
// must be opened at the parent root of the block, and checks the storage
// roots of the list. The caller then checks that IntermediateRoot(true)
// equals the state root of the header.
func (s *StateDB) ApplyBAL(list *types.BlockAccessList) error {
	if err := Apply(s, list); err != nil {
		return err
	}
	s.IntermediateRoot(true)
	return CheckStorageRoots(s, list)
}

// ApplyBALUnchecked writes the final values of a block access list like
// ApplyBAL and finalises the block without hashing, so that several blocks can be
// applied in a row and checked once at the end: call HashStorages, then
// IntermediateRoot. The storage roots of the list are not checked.
func (s *StateDB) ApplyBALUnchecked(list *types.BlockAccessList) error {
	if err := Apply(s, list); err != nil {
		return err
	}
	s.Finalise(true, false)
	return nil
}

// snapshotReader reads accounts and storage from a snapshot layer. What the
// layer cannot serve, like while it is generated or after it went stale, and
// code are read from the state trie.
type snapshotReader struct {
	snap snapshot.Snapshot
}

func (r snapshotReader) Account(addr common.Address) (account.Account, error) {
	acc, err := r.snap.Account(crypto.Keccak256Hash(addr.Bytes()))
	if err != nil {
		return nil, errNotServed
	}
	return acc, nil
}

func (r snapshotReader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	enc, err := r.snap.Storage(crypto.Keccak256Hash(addr.Bytes()), crypto.Keccak256Hash(slot.Bytes()))
	if err != nil {
		return common.Hash{}, errNotServed
	}
	var value common.Hash
	if len(enc) > 0 {
		_, content, _, err := rlp.Split(enc)
		if err != nil {
			return common.Hash{}, err
		}
		value.SetBytes(content)
	}
	return value, nil
}

func (r snapshotReader) Code(common.Address, common.Hash) ([]byte, error) {
	return nil, errNotServed
}

func (r snapshotReader) CodeSize(common.Address, common.Hash) (int, error) {
	return 0, errNotServed
}

// snapshotState is the snapshot layer of the state a StateDB is opened at,
// which it reads from, and the changes it collects to add the layer of the
// state it commits.
type snapshotState struct {
	tree  *snapshot.Tree
	layer snapshot.Snapshot

	destructs map[common.Hash]struct{}
	accounts  map[common.Hash][]byte
	storage   map[common.Hash]map[common.Hash][]byte
}

// newSnapshotState returns the snapshot state of the state with root, or nil
// if tree has no layer for it.
func newSnapshotState(tree *snapshot.Tree, root common.Hash) *snapshotState {
	if tree == nil {
		return nil
	}
	layer := tree.Snapshot(root)
	if layer == nil {
		return nil
	}
	return &snapshotState{
		tree:      tree,
		layer:     layer,
		destructs: make(map[common.Hash]struct{}),
		accounts:  make(map[common.Hash][]byte),
		storage:   make(map[common.Hash]map[common.Hash][]byte),
	}
}

// destructed reports whether the account was destructed in this block, so
// that its storage in the layer no longer holds.
func (s *snapshotState) destructed(addrHash common.Hash) bool {
	if s == nil {
		return false
	}
	_, ok := s.destructs[addrHash]
	return ok
}

// markDestructed marks the account destructed as it is recreated, and reports
// whether it already was.
func (s *snapshotState) markDestructed(addrHash common.Hash) bool {
	if s == nil {
		return false
	}
	if _, ok := s.destructs[addrHash]; ok {
		return true
	}
	s.destructs[addrHash] = struct{}{}
	return false
}

// unmarkDestructed reverts markDestructed.
func (s *snapshotState) unmarkDestructed(addrHash common.Hash) {
	if s != nil {
		delete(s.destructs, addrHash)
	}
}

// deleteAccount records the deletion of the account. Deletions are recorded
// as they happen, since a later tx of the block may resurrect the account.
func (s *snapshotState) deleteAccount(addrHash common.Hash) {
	if s == nil {
		return
	}
	s.destructs[addrHash] = struct{}{}
	delete(s.accounts, addrHash)
	delete(s.storage, addrHash)
}

// updateAccount records the encoded account.
func (s *snapshotState) updateAccount(addrHash common.Hash, data []byte) {
	if s != nil {
		s.accounts[addrHash] = data
	}
}

// storageOf returns the storage changes of the account to record into, or nil
// without a snapshot.
func (s *snapshotState) storageOf(addrHash common.Hash) map[common.Hash][]byte {
	if s == nil {
		return nil
	}
	storage := s.storage[addrHash]
	if storage == nil {
		storage = make(map[common.Hash][]byte)
		s.storage[addrHash] = storage
	}
	return storage
}

func (s *snapshotState) copy() *snapshotState {
	if s == nil {
		return nil
	}
	cpy := &snapshotState{
		tree:      s.tree,
		layer:     s.layer,
		destructs: maps.Clone(s.destructs),
		accounts:  maps.Clone(s.accounts),
		storage:   make(map[common.Hash]map[common.Hash][]byte, len(s.storage)),
	}
	for addrHash, storage := range s.storage {
		cpy.storage[addrHash] = maps.Clone(storage)
	}
	return cpy
}

// commit adds the layer of the state with root on top of the layer it was
// opened at, and keeps 128 diff layers in memory.
func (s *snapshotState) commit(root common.Hash) {
	parent := s.layer.Root()
	if parent == root {
		return
	}
	if err := s.tree.Update(root, parent, s.destructs, s.accounts, s.storage); err != nil {
		logger.Warn("Failed to update snapshot tree", "from", parent, "to", root, "err", err)
	}
	// Keep 128 diff layers in the memory, persistent layer is 129th.
	// - head layer is paired with HEAD state
	// - head-1 layer is paired with HEAD-1 state
	// - head-127 layer(bottom-most diff layer) is paired with HEAD-127 state
	if err := s.tree.Cap(root, 128); err != nil {
		logger.Warn("Failed to cap snapshot tree", "root", root, "layers", 128, "err", err)
	}
}
