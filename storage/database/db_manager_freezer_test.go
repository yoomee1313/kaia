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

package database

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database/ancient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type freezerTestBlock struct {
	header   *types.Header
	body     *types.Body
	receipts types.Receipts
}

func (b *freezerTestBlock) hash() common.Hash { return b.header.Hash() }

func newFreezerTestConfig(dir string, enable bool, threshold uint64) *DBConfig {
	return &DBConfig{
		Dir: dir, DBType: LevelDB, NumStateTrieShards: 1,
		EnableFreezer: enable, FreezerThreshold: threshold,
	}
}

func openFreezerTestDBM(t *testing.T, dbc *DBConfig) *databaseManager {
	dbm, ok := NewDBManager(dbc).(*databaseManager)
	require.True(t, ok)
	return dbm
}

// genFreezerTestBlock creates a block with one transaction and one receipt.
func genFreezerTestBlock(t *testing.T, number uint64, parent common.Hash, extra string) *freezerTestBlock {
	tx, err := genTransaction(number + 1)
	require.NoError(t, err)
	receipt := genReceipt(int(number) + 1)
	receipt.TxHash = tx.Hash()
	return &freezerTestBlock{
		header: &types.Header{
			ParentHash: parent,
			Number:     new(big.Int).SetUint64(number),
			BlockScore: big.NewInt(1),
			Time:       new(big.Int).SetUint64(number),
			Extra:      []byte(extra),
		},
		body:     &types.Body{Transactions: types.Transactions{tx}},
		receipts: types.Receipts{receipt},
	}
}

// writeFreezerTestBlock stores a block the way the blockchain does.
func writeFreezerTestBlock(dbm DBManager, b *freezerTestBlock, canonical bool) {
	hash, number := b.hash(), b.header.Number.Uint64()
	dbm.WriteTd(hash, number, new(big.Int).SetUint64(number+1))
	dbm.WriteBlock(types.NewBlockWithHeader(b.header).WithBody(b.body.Transactions))
	dbm.WriteReceipts(hash, number, b.receipts)
	if canonical {
		dbm.WriteCanonicalHash(hash, number)
	}
}

// writeFreezerTestChain writes a canonical chain of n blocks and sets its head.
func writeFreezerTestChain(t *testing.T, dbm DBManager, n int) []*freezerTestBlock {
	blocks := make([]*freezerTestBlock, n)
	parent := common.Hash{}
	for i := range blocks {
		blocks[i] = genFreezerTestBlock(t, uint64(i), parent, "canonical")
		writeFreezerTestBlock(dbm, blocks[i], true)
		parent = blocks[i].hash()
	}
	dbm.WriteHeadBlockHash(parent)
	return blocks
}

// triggerFreeze runs one freeze cycle and waits until the loop is idle again.
func triggerFreeze(t *testing.T, dbm *databaseManager) {
	done := make(chan struct{})
	select {
	case dbm.freezerTrigger <- done:
	case <-time.After(10 * time.Second):
		t.Fatal("freeze loop did not accept the trigger")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("freeze loop did not finish")
	}
}

// hasKV reports whether the block data of the given kind is in the key-value store.
func hasKV(dbm *databaseManager, entry DBEntryType, key []byte) bool {
	has, _ := dbm.getDatabase(entry).Has(key)
	return has
}

// assertBlockReadable checks that every block read path returns the block.
func assertBlockReadable(t *testing.T, dbm DBManager, b *freezerTestBlock) {
	hash, number := b.hash(), b.header.Number.Uint64()
	assert.Equal(t, hash, dbm.ReadCanonicalHash(number), "canonical hash %d", number)

	assert.True(t, dbm.HasHeader(hash, number), "has header %d", number)
	assert.Equal(t, hash, dbm.ReadHeader(hash, number).Hash(), "header %d", number)
	headerRLP, _ := rlp.EncodeToBytes(b.header)
	assert.Equal(t, headerRLP, []byte(dbm.ReadHeaderRLP(hash, number)), "header rlp %d", number)
	assert.Equal(t, number, *dbm.ReadHeaderNumber(hash), "header number %d", number)

	assert.True(t, dbm.HasBody(hash, number), "has body %d", number)
	assert.Equal(t, b.body.Transactions[0].Hash(), dbm.ReadBody(hash, number).Transactions[0].Hash(), "body %d", number)
	bodyRLP, _ := rlp.EncodeToBytes(b.body)
	assert.Equal(t, bodyRLP, []byte(dbm.ReadBodyRLP(hash, number)), "body rlp %d", number)
	assert.Equal(t, bodyRLP, []byte(dbm.ReadBodyRLPByHash(hash)), "body rlp by hash %d", number)

	assert.Equal(t, b.receipts, dbm.ReadReceipts(hash, number), "receipts %d", number)
	assert.Equal(t, b.receipts, dbm.ReadReceiptsByBlockHash(hash), "receipts by hash %d", number)

	assert.Equal(t, hash, dbm.ReadBlockByNumber(number).Hash(), "block %d", number)
	assert.Equal(t, new(big.Int).SetUint64(number+1), dbm.ReadTd(hash, number), "td %d", number)
}

// assertBlockFrozen checks that the block was deleted from the key-value store,
// except for the hash to number mapping and total difficulty.
func assertBlockFrozen(t *testing.T, dbm *databaseManager, b *freezerTestBlock) {
	hash, number := b.hash(), b.header.Number.Uint64()
	assert.False(t, hasKV(dbm, headerDB, headerHashKey(number)), "kv canonical hash %d", number)
	assert.False(t, hasKV(dbm, headerDB, headerKey(number, hash)), "kv header %d", number)
	assert.False(t, hasKV(dbm, BodyDB, blockBodyKey(number, hash)), "kv body %d", number)
	assert.False(t, hasKV(dbm, ReceiptsDB, blockReceiptsKey(number, hash)), "kv receipts %d", number)
	assert.True(t, hasKV(dbm, headerDB, headerNumberKey(hash)), "kv header number %d", number)
	assert.True(t, hasKV(dbm, MiscDB, headerTDKey(number, hash)), "kv td %d", number)
}

func assertBlockInKV(t *testing.T, dbm *databaseManager, b *freezerTestBlock) {
	hash, number := b.hash(), b.header.Number.Uint64()
	assert.True(t, hasKV(dbm, headerDB, headerHashKey(number)), "kv canonical hash %d", number)
	assert.True(t, hasKV(dbm, headerDB, headerKey(number, hash)), "kv header %d", number)
	assert.True(t, hasKV(dbm, BodyDB, blockBodyKey(number, hash)), "kv body %d", number)
	assert.True(t, hasKV(dbm, ReceiptsDB, blockReceiptsKey(number, hash)), "kv receipts %d", number)
}

// TestFreezer_FreezeAndRead checks that the freeze loop moves old blocks out of
// the key-value store and that every read path falls through to the freezer.
func TestFreezer_FreezeAndRead(t *testing.T) {
	const (
		numBlocks = 20
		threshold = 5
		frozen    = numBlocks - threshold // blocks [0, head-threshold] are frozen
	)
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, true, threshold))

	blocks := writeFreezerTestChain(t, dbm, numBlocks)
	side := genFreezerTestBlock(t, 3, blocks[2].hash(), "side")
	writeFreezerTestBlock(dbm, side, false)

	triggerFreeze(t, dbm)
	require.Equal(t, uint64(frozen), dbm.FrozenBlocks())

	// The genesis block stays in the key-value store.
	assertBlockInKV(t, dbm, blocks[0])
	for _, b := range blocks[1:frozen] {
		assertBlockFrozen(t, dbm, b)
	}
	for _, b := range blocks[frozen:] {
		assertBlockInKV(t, dbm, b)
	}
	// The side chain block at a frozen height is deleted entirely.
	assert.False(t, hasKV(dbm, headerDB, headerKey(3, side.hash())))
	assert.False(t, hasKV(dbm, headerDB, headerNumberKey(side.hash())))
	assert.False(t, hasKV(dbm, BodyDB, blockBodyKey(3, side.hash())))
	assert.False(t, hasKV(dbm, ReceiptsDB, blockReceiptsKey(3, side.hash())))

	// Reads work with warm caches and after reopening with cold caches.
	for _, b := range blocks {
		assertBlockReadable(t, dbm, b)
	}
	dbm.Close()

	dbm = openFreezerTestDBM(t, newFreezerTestConfig(dir, true, threshold))
	defer dbm.Close()
	require.Equal(t, uint64(frozen), dbm.FrozenBlocks())
	for _, b := range blocks {
		assertBlockReadable(t, dbm, b)
	}

	// A frozen item is only returned for the canonical hash.
	fake := common.HexToHash("0xdeadbeef")
	assert.False(t, dbm.HasHeader(fake, 5))
	assert.Nil(t, dbm.ReadHeader(fake, 5))
	assert.False(t, dbm.HasBody(fake, 5))
	assert.Nil(t, dbm.ReadBodyRLP(fake, 5))
	assert.Nil(t, dbm.ReadReceipts(fake, 5))
	assert.Nil(t, dbm.ReadHeader(side.hash(), 3))
	assert.Nil(t, dbm.ReadHeaderRLP(blocks[5].hash(), 6)) // canonical hash, wrong number

	// New blocks advance the freezer on the next cycle.
	parent := blocks[numBlocks-1].hash()
	for i := numBlocks; i < numBlocks+10; i++ {
		b := genFreezerTestBlock(t, uint64(i), parent, "canonical")
		writeFreezerTestBlock(dbm, b, true)
		blocks = append(blocks, b)
		parent = b.hash()
	}
	dbm.WriteHeadBlockHash(parent)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(frozen+10), dbm.FrozenBlocks())
	for _, b := range blocks[frozen : frozen+10] {
		assertBlockFrozen(t, dbm, b)
		assertBlockReadable(t, dbm, b)
	}
}

// TestFreezer_BelowThreshold checks that nothing is frozen until the chain is
// longer than the threshold.
func TestFreezer_BelowThreshold(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 10))
	defer dbm.Close()

	blocks := writeFreezerTestChain(t, dbm, 11) // head = 10 = threshold
	triggerFreeze(t, dbm)
	assert.Equal(t, uint64(0), dbm.FrozenBlocks())
	for _, b := range blocks {
		assertBlockInKV(t, dbm, b)
	}
}

// TestFreezer_InterruptedBatch simulates a crash after a batch was flushed to
// the freezer but before it was deleted from the key-value store. Reopening
// finishes the deletion.
func TestFreezer_InterruptedBatch(t *testing.T) {
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, false, 0))
	blocks := writeFreezerTestChain(t, dbm, 20)
	dbm.Close()

	// Freeze blocks [0, 10) without touching the key-value store.
	f, err := ancient.NewChainFreezer(filepath.Join(dir, DefaultAncientDirName, ancient.ChainFreezerName), "", false)
	require.NoError(t, err)
	_, err = f.ModifyAncients(func(op ancient.AncientWriteOp) error {
		for _, b := range blocks[:10] {
			hash, number := b.hash(), b.header.Number.Uint64()
			headerRLP, _ := rlp.EncodeToBytes(b.header)
			bodyRLP, _ := rlp.EncodeToBytes(b.body)
			storageReceipts := []*types.ReceiptForStorage{(*types.ReceiptForStorage)(b.receipts[0])}
			receiptsRLP, _ := rlp.EncodeToBytes(storageReceipts)
			require.NoError(t, op.AppendRaw(ancient.ChainFreezerHashTable, number, hash[:]))
			require.NoError(t, op.AppendRaw(ancient.ChainFreezerHeaderTable, number, headerRLP))
			require.NoError(t, op.AppendRaw(ancient.ChainFreezerBodiesTable, number, bodyRLP))
			require.NoError(t, op.AppendRaw(ancient.ChainFreezerReceiptTable, number, receiptsRLP))
		}
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, f.SyncAncient())
	require.NoError(t, f.Close())

	// The threshold keeps the freeze loop from moving anything else.
	dbm = openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 100))
	defer dbm.Close()
	require.Equal(t, uint64(10), dbm.FrozenBlocks())
	assertBlockInKV(t, dbm, blocks[0])
	for _, b := range blocks[1:10] {
		assertBlockFrozen(t, dbm, b)
	}
	for _, b := range blocks[10:] {
		assertBlockInKV(t, dbm, b)
	}
	for _, b := range blocks {
		assertBlockReadable(t, dbm, b)
	}
}

// TestFreezer_RequiredAfterFreezing checks that a database whose blocks were
// frozen cannot be opened without the freezer or with an empty freezer.
func TestFreezer_RequiredAfterFreezing(t *testing.T) {
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 5))
	writeFreezerTestChain(t, dbm, 10)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(5), dbm.FrozenBlocks())
	dbm.Close()

	// databaseDBManager does not open the freezer, so openFreezer can be checked
	// without NewDBManager calling logger.Crit.
	open := func(dbc *DBConfig) error {
		dbm, err := databaseDBManager(dbc)
		require.NoError(t, err)
		defer dbm.Close()
		return dbm.openFreezer()
	}
	assert.ErrorContains(t, open(newFreezerTestConfig(dir, false, 5)), "enable the freezer")

	emptyDBC := newFreezerTestConfig(dir, true, 5)
	emptyDBC.AncientDir = t.TempDir()
	assert.ErrorContains(t, open(emptyDBC), "is empty")

	// The freezer directory can be moved, e.g. to another disk.
	movedRoot := filepath.Join(t.TempDir(), "moved")
	require.NoError(t, os.Rename(filepath.Join(dir, DefaultAncientDirName), movedRoot))
	movedDBC := newFreezerTestConfig(dir, true, 5)
	movedDBC.AncientDir = movedRoot
	assert.NoError(t, open(movedDBC))
}
