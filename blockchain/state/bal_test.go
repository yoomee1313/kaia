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
	"fmt"
	"math/big"
	"slices"
	"testing"

	"errors"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/blockchain/types/accountkey"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/hashchain"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

// TestRecorderList records changes made through a StateDB, index by index as
// a block runs, and checks the list: a later write at the same index replaces
// the earlier one, a written slot is not listed as read, a deletion drops what
// its index wrote before it, a recreation at a later index records the whole
// account, and the reads of an unchanged account are sorted.
func TestRecorderList(t *testing.T) {
	var (
		a            = common.Address{0xa}
		b            = common.Address{0xb}
		slot1, slot2 = common.Hash{1}, common.Hash{2}
	)
	db := NewDatabase(database.NewMemoryDBManager())
	base, err := New(common.Hash{}, db, nil, nil)
	require.NoError(t, err)
	require.NoError(t, base.SetCode(a, []byte{1}))
	base.AddBalance(a, big.NewInt(10))
	base.AddBalance(b, big.NewInt(10))
	base.SetState(b, slot1, common.Hash{9})
	base.SetState(b, slot2, common.Hash{8})
	root, err := base.Commit(true)
	require.NoError(t, err)

	s, err := New(root, db, nil, nil)
	require.NoError(t, err)
	s.StartBAL(0)
	s.SetBalance(a, big.NewInt(1))
	s.EndBlockAccessInitialization()

	s.SetTxContext(common.Hash{1}, common.Hash{}, 0)
	s.SetBalance(a, big.NewInt(2))
	s.SetBalance(a, big.NewInt(3))
	s.GetState(a, slot1)
	s.SetState(a, slot1, common.Hash{0x11})
	s.GetState(b, slot2)
	s.GetState(b, slot1)
	s.Finalise(true, true)

	s.SetTxContext(common.Hash{2}, common.Hash{}, 1)
	s.SetState(a, slot2, common.Hash{0x22})
	s.SetNonce(a, 5)
	s.SelfDestruct(a)
	s.Finalise(true, true)

	s.SetTxContext(common.Hash{3}, common.Hash{}, 2)
	require.NoError(t, s.SetCode(a, []byte{2}))
	s.SetState(a, slot2, common.Hash{0x23})
	s.Finalise(true, true)
	s.SetBlockAccessIndex(4)
	s.IntermediateRoot(true)

	list := s.BAL()
	require.NoError(t, list.Validate())
	require.Len(t, list.Accounts, 2)

	acc := list.Accounts[0]
	require.Equal(t, a, acc.Address)
	require.Equal(t, []types.BalanceChange{{BlockAccessIndex: 0, PostBalance: big.NewInt(1)}, {BlockAccessIndex: 1, PostBalance: big.NewInt(3)}}, acc.BalanceChanges)
	require.Empty(t, acc.NonceChanges)
	require.Equal(t, []uint32{2}, acc.Deleted)
	require.Empty(t, acc.StorageCleared, "the deletion already drops the storage")
	require.Len(t, acc.AccountRecords, 1)
	require.Equal(t, uint32(3), acc.AccountRecords[0].BlockAccessIndex)
	require.Equal(t, []types.CodeChange{{BlockAccessIndex: 3, NewCode: []byte{2}}}, acc.CodeChanges)
	require.Equal(t, []types.SlotChanges{
		{Slot: slot1, Changes: []types.StorageChange{{BlockAccessIndex: 1, NewValue: common.Hash{0x11}}}},
		{Slot: slot2, Changes: []types.StorageChange{{BlockAccessIndex: 3, NewValue: common.Hash{0x23}}}},
	}, acc.StorageChanges)
	require.Empty(t, acc.StorageReads)

	acc = list.Accounts[1]
	require.False(t, acc.Changed())
	require.Equal(t, []common.Hash{slot1, slot2}, acc.StorageReads)

	enc, err := list.Encode()
	require.NoError(t, err)
	dec, err := types.DecodeBAL(enc)
	require.NoError(t, err)
	reenc, err := dec.Encode()
	require.NoError(t, err)
	require.Equal(t, enc, reenc)
}

func openTestStore(t *testing.T, db database.DBManager, cfg BALConfig) *BALHistory {
	head := func() *types.Header { return &types.Header{Number: new(big.Int)} }
	s, err := OpenBALHistory(db, cfg, head, nil)
	require.NoError(t, err)
	return s
}

// TestStoreRange checks how stored lists join the range of blocks whose lists
// are all stored: a span that does not touch it joins once the blocks in
// between are stored, and a rewind drops the blocks after it. A block written
// after a gap, which a node leaves by running in another mode, starts the
// range again, and the earlier lists join it again once the gap is filled.
func TestStoreRange(t *testing.T) {
	t.Run("join", testStoreRangeJoin)
	t.Run("after gap", testStoreRangeAfterGap)
}

func testStoreRangeJoin(t *testing.T) {
	db := database.NewMemoryDBManager()
	hash := func(n uint64) common.Hash { return common.Hash{byte(n)} }
	for n := uint64(1); n <= 10; n++ {
		db.WriteCanonicalHash(hash(n), n)
	}
	s := openTestStore(t, db, BALConfig{})
	put := func(n, from uint64) {
		t.Helper()
		require.NoError(t, s.Put(n, hash(n), []byte{byte(n)}, nil, from))
	}
	span := func(first, next uint64) {
		t.Helper()
		f, n := s.Range()
		require.Equal(t, [2]uint64{first, next}, [2]uint64{f, n})
		f, n = db.ReadBALRange()
		require.Equal(t, [2]uint64{first, next}, [2]uint64{f, n}, "stored range")
	}
	span(0, 0)
	put(5, 5)
	span(5, 6)
	put(8, 8)
	span(5, 6)
	put(2, 2)
	put(3, 2)
	span(5, 6)
	put(4, 2)
	span(2, 6)
	put(6, 6)
	put(7, 7)
	span(2, 9)

	// A block in the range is not recorded again.
	again := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(3)})
	s.PutExecuted(again, &types.BlockAccessList{})
	require.Nil(t, db.ReadBAL(again.Hash(), 3))

	for n := uint64(8); n >= 7; n-- {
		s.RewindDelete(hash(n), n)
	}
	span(2, 7)
	f, n := openTestStore(t, db, BALConfig{}).Range()
	require.Equal(t, [2]uint64{2, 7}, [2]uint64{f, n}, "reopened")
	s.RewindDelete(hash(1), 1)
	span(0, 0)
}

func testStoreRangeAfterGap(t *testing.T) {
	db := database.NewMemoryDBManager()
	blocks := make([]*types.Block, 10)
	for n := range blocks {
		blocks[n] = types.NewBlockWithHeader(&types.Header{Number: big.NewInt(int64(n)), Time: big.NewInt(int64(n))})
		db.WriteCanonicalHash(blocks[n].Hash(), uint64(n))
	}
	writes, err := (&types.BlockAccessList{}).Encode()
	require.NoError(t, err)
	s := openTestStore(t, db, BALConfig{})
	span := func(first, next uint64) {
		t.Helper()
		f, n := s.Range()
		require.Equal(t, [2]uint64{first, next}, [2]uint64{f, n})
	}
	for n := 1; n <= 3; n++ {
		s.PutInserted(blocks[n], writes)
	}
	span(1, 4)

	// The node ran as a full node for blocks 4-6.
	s.PutInserted(blocks[7], writes)
	span(7, 8)
	s.PutInserted(blocks[8], writes)
	span(7, 9)

	// A regeneration fills blocks 4-6.
	for n := uint64(4); n <= 6; n++ {
		require.NoError(t, s.Put(n, blocks[n].Hash(), writes, nil, 4))
	}
	span(1, 9)
}

// testBALChain is a chain of block states written with StateDB changes, whose
// lists are recorded as blocks run and stored in a BAL history.
type testBALChain struct {
	diskDB   database.DBManager
	db       Database
	history  *BALHistory
	headers  []*types.Header // headers[n] is block n
	lists    []*types.BlockAccessList
	contract common.Address
	child    common.Address
	accounts map[common.Address]bool
	slots    map[common.Address]map[common.Hash]bool
}

// newTestBALChain runs blocks that change balances, nonces, code, storage and
// keys, destroy a contract and recreate it in the same block and later, and
// delegate an EOA, like StateProcessor: InitializeState at index 0, each tx
// finalised at its index, and FinalizeState after them.
func newTestBALChain(t *testing.T) *testBALChain {
	var (
		system   = common.HexToAddress("0x5a")
		reward   = common.HexToAddress("0xbe")
		eoa      = common.HexToAddress("0xe0a1")
		sender   = common.HexToAddress("0xe0a2")
		keyed    = common.HexToAddress("0xe0a3")
		contract = common.HexToAddress("0xc0")
		child    = common.HexToAddress("0xc1")
		untouch  = common.HexToAddress("0xe0ff")
		code     = []byte{0x60, 0x00}
		slot     = func(n byte) common.Hash { return common.Hash{n} }
		value    = func(n byte) common.Hash { return common.Hash{31: n} }
		rules    = params.TestKaiaConfig("prague").Rules(common.Big0)
	)
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	type tx func(s *StateDB)
	blocks := [][]tx{
		1: {
			func(s *StateDB) {
				s.AddBalance(eoa, common.Big1)
				s.SetCode(contract, code)
				s.SetState(contract, slot(1), value(0x11))
			},
			func(s *StateDB) { s.SetNonce(sender, 1) },
		},
		2: {
			func(s *StateDB) {
				s.SetState(contract, slot(1), common.Hash{})
				s.SetState(contract, slot(2), value(0x22))
			},
			func(s *StateDB) { s.SetCode(child, code); s.SetState(child, slot(3), value(0x33)) },
		},
		3: {
			func(s *StateDB) { s.SelfDestruct(child) },
			func(s *StateDB) { s.SetCode(child, code); s.SetState(child, slot(4), value(0x44)) },
		},
		4: {
			func(s *StateDB) {
				require.NoError(t, s.UpdateKey(keyed, accountkey.NewAccountKeyPublicWithValue(&key.PublicKey), 4))
			},
			func(s *StateDB) {
				require.NoError(t, s.SetCodeToEOA(sender, append([]byte{0xef, 0x01, 0x00}, contract[:]...), rules))
			},
		},
		5: {func(s *StateDB) { s.SelfDestruct(child) }},
		6: {},
		7: {
			func(s *StateDB) { s.SetCode(child, code); s.SetState(child, slot(1), value(0x71)) },
			func(s *StateDB) { s.SetState(contract, slot(2), common.Hash{}); s.AddBalance(eoa, common.Big2) },
		},
		8: {func(s *StateDB) { s.SetState(sender, slot(5), value(0x55)) }},
	}

	diskDB := database.NewMemoryDBManager()
	db := NewDatabase(diskDB)
	genesis, err := New(common.Hash{}, db, nil, nil)
	require.NoError(t, err)
	for _, addr := range []common.Address{eoa, sender, untouch} {
		genesis.AddBalance(addr, big.NewInt(100))
	}
	genesis.CreateEOA(keyed, false, accountkey.NewAccountKeyLegacy())
	genesis.AddBalance(keyed, big.NewInt(100))
	root, err := genesis.Commit(true)
	require.NoError(t, err)
	require.NoError(t, db.TrieDB().Commit(root, false, 0))

	c := &testBALChain{
		diskDB: diskDB, db: db, contract: contract, child: child,
		headers:  []*types.Header{{Number: common.Big0, Root: root, BlockScore: common.Big1, Time: common.Big0}},
		accounts: map[common.Address]bool{system: true, reward: true, keyed: true, untouch: true},
		slots:    map[common.Address]map[common.Hash]bool{},
	}
	diskDB.WriteHeader(c.headers[0])
	diskDB.WriteCanonicalHash(c.headers[0].Hash(), 0)
	c.history, err = OpenBALHistory(diskDB, BALConfig{}, func() *types.Header { return c.headers[len(c.headers)-1] },
		func(_ uint64, root common.Hash) (BaseState, error) {
			sdb, err := New(root, db, nil, nil)
			return NewBALBase(sdb), err
		})
	require.NoError(t, err)

	for n := 1; n < len(blocks); n++ {
		s, err := New(root, db, nil, nil)
		require.NoError(t, err)
		s.StartBAL(0)
		s.SetState(system, common.Hash{}, value(byte(n)))
		s.EndBlockAccessInitialization()
		for i, tx := range blocks[n] {
			s.SetTxContext(common.Hash{byte(n), byte(i)}, common.Hash{}, i)
			tx(s)
			s.Finalise(true, true)
		}
		s.SetBlockAccessIndex(len(blocks[n]) + 1)
		s.AddBalance(reward, common.Big1)
		root = s.IntermediateRoot(true)
		list := s.BAL()
		committed, err := s.Commit(true)
		require.NoError(t, err)
		require.Equal(t, root, committed)
		require.NoError(t, db.TrieDB().Commit(root, false, uint64(n)))

		header := &types.Header{ParentHash: c.headers[n-1].Hash(), Number: big.NewInt(int64(n)), Root: root, BlockScore: common.Big1, Time: big.NewInt(int64(n))}
		diskDB.WriteHeader(header)
		diskDB.WriteCanonicalHash(header.Hash(), uint64(n))
		c.history.PutExecuted(types.NewBlockWithHeader(header), list)
		c.headers, c.lists = append(c.headers, header), append(c.lists, list)
		for _, a := range list.Accounts {
			c.accounts[a.Address] = true
			for _, w := range a.StorageChanges {
				if c.slots[a.Address] == nil {
					c.slots[a.Address] = make(map[common.Hash]bool)
				}
				c.slots[a.Address][w.Slot] = true
			}
		}
	}
	for _, addr := range []common.Address{contract, child, keyed, sender} {
		require.True(t, c.accounts[addr], addr.Hex())
	}
	_, next, ok := c.history.Indexed()
	require.True(t, ok)
	require.Equal(t, uint64(len(blocks)), next)
	return c
}

// requireSameState checks every touched account and slot of got against want.
func (c *testBALChain) requireSameState(t *testing.T, n int, want, got *StateDB) {
	t.Helper()
	fields := func(acc account.Account) []byte {
		if acc == nil {
			return nil
		}
		acc = acc.DeepCopy()
		if pa := account.GetProgramAccount(acc); pa != nil {
			pa.SetStorageRoot(common.ExtHash{})
		}
		enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(acc))
		require.NoError(t, err)
		return enc
	}
	for addr := range c.accounts {
		msg := fmt.Sprintf("block %d account %s", n, addr.Hex())
		require.Equal(t, want.Exist(addr), got.Exist(addr), msg)
		require.Equal(t, want.GetBalance(addr), got.GetBalance(addr), msg)
		require.Equal(t, want.GetNonce(addr), got.GetNonce(addr), msg)
		require.Equal(t, want.GetCodeHash(addr), got.GetCodeHash(addr), msg)
		require.Equal(t, common.CopyBytes(want.GetCode(addr)), common.CopyBytes(got.GetCode(addr)), msg)
		require.Equal(t, fields(want.GetAccount(addr)), fields(got.GetAccount(addr)), msg)
		for slot := range c.slots[addr] {
			require.Equal(t, want.GetState(addr, slot), got.GetState(addr, slot), "%s slot %x", msg, slot)
		}
	}
	require.NoError(t, got.Error(), "block %d", n)
}

// TestBALHistoryState opens the state of every block from the BAL history and
// compares it, values and iterated storage, with the trie, then checks what a
// read-only state refuses, which
// blocks it is not opened for, and that a state opened before a rewind fails
// once the index is rebuilt.
func TestBALHistoryState(t *testing.T) {
	c := newTestBALChain(t)
	for _, n := range []int{3, 5} {
		i := slices.IndexFunc(c.lists[n-1].Accounts, func(a types.AccountChanges) bool { return a.Address == c.child })
		require.NotEqual(t, -1, i, "block %d", n)
		require.Equal(t, []uint32{1}, c.lists[n-1].Accounts[i].Deleted, "block %d destroys the child in tx 0", n)
	}
	for n, header := range c.headers {
		want, err := New(header.Root, c.db, nil, nil)
		require.NoError(t, err)
		got, err := OpenBALState(c.history, header, c.db)
		require.NoError(t, err, "block %d", n)
		require.True(t, got.ReadOnly())
		c.requireSameState(t, n, want, got)
		require.Equal(t, header.Root, got.IntermediateRoot(true), "block %d", n)

		// The storage of every account is iterated as on the trie.
		for addr := range c.accounts {
			storage := func(sdb *StateDB) [][2]common.Hash {
				var kv [][2]common.Hash
				sdb.ForEachStorage(addr, func(k, v common.Hash) bool {
					kv = append(kv, [2]common.Hash{k, v})
					return true
				})
				require.NoError(t, sdb.Error(), "block %d account %s", n, addr.Hex())
				return kv
			}
			require.Equal(t, storage(want), storage(got), "block %d account %s", n, addr.Hex())
		}
	}

	// Commits and proofs need the trie.
	sdb, err := OpenBALState(c.history, c.headers[4], c.db)
	require.NoError(t, err)
	_, err = sdb.Commit(true)
	require.ErrorIs(t, err, ErrReadOnly)
	require.Nil(t, sdb.StorageTrie(c.contract))

	// Only canonical indexed blocks are served.
	side := types.CopyHeader(c.headers[4])
	side.Extra = []byte{1}
	_, err = OpenBALState(c.history, side, c.db)
	require.ErrorIs(t, err, ErrNotCovered)
	_, err = OpenBALState(c.history, &types.Header{Number: big.NewInt(100)}, c.db)
	require.ErrorIs(t, err, ErrNotCovered)

	// A rewind drops the lists above the new head from the index, and a state
	// opened before fails instead of reading the truncated index.
	old, err := OpenBALState(c.history, c.headers[4], c.db)
	require.NoError(t, err)
	for n := 8; n >= 7; n-- {
		c.history.RewindDelete(c.headers[n].Hash(), uint64(n))
		c.diskDB.DeleteCanonicalHash(uint64(n))
	}
	old.GetBalance(c.contract)
	require.ErrorIs(t, old.Error(), ErrRebuilt)
	sdb, err = OpenBALState(c.history, c.headers[4], c.db)
	require.NoError(t, err)
	sdb.GetBalance(c.contract)
	require.NoError(t, sdb.Error())
	_, err = OpenBALState(c.history, c.headers[8], c.db)
	require.ErrorIs(t, err, ErrNotCovered)

	// A list stored before its block is canonical waits for the next update.
	first, _ := c.diskDB.ReadBALRange()
	require.NoError(t, c.diskDB.WriteBAL(common.Hash{0x07}, 7, c.diskDB.ReadBAL(c.headers[6].Hash(), 6)))
	require.NoError(t, c.diskDB.WriteBALRange(first, 8))
	require.NoError(t, c.history.UpdateIndex(nil))
	_, next, _ := c.history.Indexed()
	require.Equal(t, uint64(7), next)
}

// TestHashChainBlock proves blocks with real lists, the genesis one included.
func TestHashChainBlock(t *testing.T) {
	lists := make([]*types.BlockAccessList, 40)
	lists[0] = &types.BlockAccessList{}
	for i := 1; i < len(lists); i++ {
		lists[i] = &types.BlockAccessList{Accounts: []types.AccountChanges{
			{Address: common.Address{byte(i)}, BalanceChanges: []types.BalanceChange{{BlockAccessIndex: 1, PostBalance: big.NewInt(int64(i))}}},
			{Address: common.Address{0xee}, StorageReads: []common.Hash{{byte(i)}}},
		}}
	}
	hash := func(n uint64) (common.Hash, error) { return lists[n].Hash() }
	last := uint64(len(lists) - 1)
	c, err := hashchain.Build(last, hash)
	require.NoError(t, err)
	genesis, err := hash(0)
	require.NoError(t, err)
	require.Equal(t, EmptyListHash, genesis)

	for _, n := range []uint64{0, 1, 17, last} {
		proof, err := hashchain.Proof(n, last, hash)
		require.NoError(t, err)
		require.NoError(t, VerifyHashChainBlock(c, n, lists[n], proof), "block %d", n)
		// The writes alone verify too.
		require.NoError(t, VerifyHashChainBlock(c, n, lists[n].Writes(), proof), "block %d", n)
	}
	proof, err := hashchain.Proof(5, last, hash)
	require.NoError(t, err)
	require.ErrorIs(t, VerifyHashChainBlock(c, 5, lists[6], proof), hashchain.ErrMismatch)
	require.ErrorIs(t, VerifyHashChainBlock(c, 6, lists[5], proof), hashchain.ErrMismatch)
	err = VerifyHashChainBlock(c, last+1, lists[5], proof)
	require.True(t, err != nil && !errors.Is(err, hashchain.ErrMismatch))
	_, err = hashchain.Proof(last+1, last, hash)
	require.Error(t, err)
}
