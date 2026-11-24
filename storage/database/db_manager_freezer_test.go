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
	"encoding/binary"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
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
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
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
// except for the hash to number mapping. The test blocks have total blockscore
// number+1, so that is deleted as well.
func assertBlockFrozen(t *testing.T, dbm *databaseManager, b *freezerTestBlock) {
	hash, number := b.hash(), b.header.Number.Uint64()
	assert.False(t, hasKV(dbm, headerDB, headerHashKey(number)), "kv canonical hash %d", number)
	assert.False(t, hasKV(dbm, headerDB, headerKey(number, hash)), "kv header %d", number)
	assert.False(t, hasKV(dbm, BodyDB, blockBodyKey(number, hash)), "kv body %d", number)
	assert.False(t, hasKV(dbm, ReceiptsDB, blockReceiptsKey(number, hash)), "kv receipts %d", number)
	assert.True(t, hasKV(dbm, headerDB, headerNumberKey(hash)), "kv header number %d", number)
	assert.False(t, hasKV(dbm, MiscDB, headerTDKey(number, hash)), "kv td %d", number)
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
	assert.Equal(t, int64(frozen), frozenBlocksGauge.Value())
	assert.Equal(t, int64(0), freezerLagGauge.Value())

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

// blockDBSize returns the on-disk size of the header, body and receipts databases.
func blockDBSize(t *testing.T, dir string) int64 {
	var size int64
	for _, entry := range []DBEntryType{headerDB, BodyDB, ReceiptsDB} {
		err := filepath.Walk(filepath.Join(dir, dbBaseDirs[entry]), func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				size += info.Size()
			}
			return err
		})
		require.NoError(t, err)
	}
	return size
}

// TestFreezer_Compaction checks that the freeze loop compacts the key ranges
// of the frozen blocks once enough blocks are frozen, so that their space is
// freed.
func TestFreezer_Compaction(t *testing.T) {
	const (
		numBlocks = 1000
		threshold = 10
	)
	defer func(interval uint64) { freezerCompactInterval = interval }(freezerCompactInterval)
	freezerCompactInterval = 500

	// Start from compacted tables, as a node does whose blocks are old. The
	// sizes are taken with the database closed, when the obsolete table files
	// of the compaction are removed.
	dir := t.TempDir()
	dbc := newFreezerTestConfig(dir, true, threshold)
	dbm := openFreezerTestDBM(t, dbc)
	writeFreezerTestChain(t, dbm, numBlocks)
	for _, entry := range []DBEntryType{headerDB, BodyDB, ReceiptsDB} {
		require.NoError(t, dbm.getDatabase(entry).Compact(nil, nil))
	}
	dbm.Close()
	before := blockDBSize(t, dir)

	// The loop starts a round as soon as the database is opened.
	compactions := freezeCompactTimer.Count()
	dbm = openFreezerTestDBM(t, dbc)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(numBlocks-threshold), dbm.FrozenBlocks())
	assert.Equal(t, compactions+1, freezeCompactTimer.Count())
	dbm.Close()
	after := blockDBSize(t, dir)

	// The total difficulty and the hash to number mappings of the frozen
	// blocks stay, so not everything is freed. Without the compaction, the size
	// grows because of the deletion markers.
	assert.Less(t, after, before/4, "block DB size before %d, after %d", before, after)
}

// TestFreezer_CompactionAfterRestart checks that the compaction of the blocks
// frozen while catching up starts where the first freeze loop started, also
// after a restart before it ran.
func TestFreezer_CompactionAfterRestart(t *testing.T) {
	defer func(interval uint64) { freezerCompactInterval = interval }(freezerCompactInterval)
	freezerCompactInterval = 1 << 40 // not while the blocks are frozen

	dir := t.TempDir()
	dbc := newFreezerTestConfig(dir, true, 10)
	dbm := openFreezerTestDBM(t, dbc)
	writeFreezerTestChain(t, dbm, 100)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(90), dbm.FrozenBlocks())
	require.Equal(t, uint64(0), dbm.frozenCompacted(), "the start of the first loop is kept")
	dbm.Close()

	freezerCompactInterval = 50
	compactions := freezeCompactTimer.Count()
	dbm = openFreezerTestDBM(t, dbc)
	defer dbm.Close()
	triggerFreeze(t, dbm)
	assert.Equal(t, compactions+1, freezeCompactTimer.Count(), "the blocks frozen before the restart are compacted")
	assert.Equal(t, uint64(90), dbm.frozenCompacted())
}

// TestFreezer_CloseTwice checks that closing the database twice does not panic.
// TestFreezer_ReadAhead checks that the blocks read ahead come back in order,
// that an error is returned for its block, and that stop ends the reads early.
func TestFreezer_ReadAhead(t *testing.T) {
	read := func(i int) (frozenBlock, error) {
		if i == 700 {
			return frozenBlock{}, errors.New("missing")
		}
		return frozenBlock{hash: common.BigToHash(big.NewInt(int64(i)))}, nil
	}
	get, stop := readAhead(1000, 4, 16, read)
	for i := range 700 {
		b, err := get(i)
		require.NoError(t, err)
		require.Equal(t, common.BigToHash(big.NewInt(int64(i))), b.hash)
	}
	_, err := get(700)
	assert.Error(t, err)
	stop()
}

func TestFreezer_CloseTwice(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 10))
	writeFreezerTestChain(t, dbm, 12)
	triggerFreeze(t, dbm)
	assert.Equal(t, uint64(2), dbm.FrozenBlocks())

	dbm.Close()
	assert.NotPanics(t, dbm.Close)
}

// TestFreezer_Pause checks that no block is frozen while the freezer is paused.
func TestFreezer_Pause(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 5))
	defer dbm.Close()
	blocks := writeFreezerTestChain(t, dbm, 20)

	frozen, resume := dbm.PauseFreezer()
	assert.Equal(t, uint64(0), frozen)
	done := make(chan struct{})
	dbm.freezerTrigger <- done
	select {
	case <-done:
		t.Fatal("freeze round finished while the freezer was paused")
	case <-time.After(200 * time.Millisecond):
	}
	assert.Equal(t, uint64(0), dbm.FrozenBlocks())
	for _, b := range blocks {
		assertBlockInKV(t, dbm, b)
	}

	resume()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("freeze loop did not finish after resume")
	}
	assert.Equal(t, uint64(15), dbm.FrozenBlocks())
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
			require.NoError(t, op.AppendRaw(ancient.ChainFreezerBALTable, number, nil))
			require.NoError(t, op.AppendRaw(ancient.ChainFreezerBALReadsTable, number, nil))
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

// TestFreezer_InterruptedDelete checks that the blocks of a batch whose
// deletion from the key-value store stopped after the bodies and receipts,
// before the headers, are deleted when the database is opened.
func TestFreezer_InterruptedDelete(t *testing.T) {
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 10))
	blocks := writeFreezerTestChain(t, dbm, 20)
	triggerFreeze(t, dbm)
	frozen := dbm.FrozenBlocks()
	require.Greater(t, frozen, uint64(2))
	// Put back the headers of the frozen blocks, as if their store was not
	// written yet.
	for _, b := range blocks[1:frozen] {
		dbm.WriteHeader(b.header)
		dbm.WriteCanonicalHash(b.hash(), b.header.Number.Uint64())
	}
	dbm.Close()

	dbm = openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 100))
	defer dbm.Close()
	require.Equal(t, frozen, dbm.FrozenBlocks())
	for _, b := range blocks[1:frozen] {
		assertBlockFrozen(t, dbm, b)
	}
	for _, b := range blocks {
		assertBlockReadable(t, dbm, b)
	}
}

// copyFreezerTestDir copies the freezer files in src to a new directory dst.
func copyFreezerTestDir(t *testing.T, src, dst string) {
	require.NoError(t, os.MkdirAll(dst, 0o755))
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	for _, e := range entries {
		if e.Name() != "FLOCK" {
			require.NoError(t, copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())))
		}
	}
}

// TestFreezer_InterruptedRewind checks that frozen blocks above the head
// header, left behind by a rewind that stopped before it truncated the
// freezer, are removed when the database is opened, but not in read-only mode.
func TestFreezer_InterruptedRewind(t *testing.T) {
	const (
		numBlocks = 20
		threshold = 5
		frozen    = numBlocks - threshold // blocks [0, head-threshold] are frozen
		head      = 8
	)
	dir := t.TempDir()
	dbc := newFreezerTestConfig(dir, true, threshold)
	dbm := openFreezerTestDBM(t, dbc)
	blocks := writeFreezerTestChain(t, dbm, numBlocks)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(frozen), dbm.FrozenBlocks())

	// The rewind moved the head markers and stopped.
	dbm.WriteHeadHeaderHash(blocks[head].hash())
	dbm.WriteHeadBlockHash(blocks[head].hash())
	dbm.Close()

	readOnly := newFreezerTestConfig(dir, false, threshold)
	readOnly.FreezerReadOnly = true
	dbm = openFreezerTestDBM(t, readOnly)
	assert.Equal(t, uint64(frozen), dbm.FrozenBlocks())
	dbm.Close()

	dbm = openFreezerTestDBM(t, dbc)
	defer dbm.Close()
	assert.Equal(t, uint64(head+1), dbm.FrozenBlocks())
	for _, b := range blocks[:head+1] {
		assertBlockReadable(t, dbm, b)
	}
	for _, b := range blocks[head+1 : frozen] {
		assert.Empty(t, dbm.ReadCanonicalHash(b.header.Number.Uint64()), "block %d", b.header.Number)
		assert.Nil(t, dbm.ReadHeader(b.hash(), b.header.Number.Uint64()), "block %d", b.header.Number)
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
	assert.ErrorContains(t, open(newFreezerTestConfig(dir, false, 5)), "the freezer is disabled")

	emptyDBC := newFreezerTestConfig(dir, true, 5)
	emptyDBC.AncientDir = t.TempDir()
	assert.ErrorContains(t, open(emptyDBC), "is empty")

	// An older copy of the freezer leaves a hole before the key-value store.
	staleRoot := t.TempDir()
	copyFreezerTestDir(t, filepath.Join(dir, DefaultAncientDirName, ancient.ChainFreezerName), filepath.Join(staleRoot, ancient.ChainFreezerName))
	dbm = openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 5))
	parent := dbm.ReadHeadBlockHash()
	for i := 10; i < 15; i++ {
		b := genFreezerTestBlock(t, uint64(i), parent, "canonical")
		writeFreezerTestBlock(dbm, b, true)
		parent = b.hash()
	}
	dbm.WriteHeadBlockHash(parent)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(10), dbm.FrozenBlocks())
	dbm.Close()
	staleDBC := newFreezerTestConfig(dir, true, 5)
	staleDBC.AncientDir = staleRoot
	assert.ErrorContains(t, open(staleDBC), "older than the chain")

	// The freezer directory can be moved, e.g. to another disk.
	movedRoot := filepath.Join(t.TempDir(), "moved")
	require.NoError(t, os.Rename(filepath.Join(dir, DefaultAncientDirName), movedRoot))
	movedDBC := newFreezerTestConfig(dir, true, 5)
	movedDBC.AncientDir = movedRoot
	assert.NoError(t, open(movedDBC))
}

// TestFreezer_ReadOnly checks that a tool can read frozen blocks without
// enabling the freezer, and that it never freezes blocks itself.
func TestFreezer_ReadOnly(t *testing.T) {
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 5))
	blocks := writeFreezerTestChain(t, dbm, 10)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(5), dbm.FrozenBlocks())
	dbm.Close()

	readOnly := func(ancientDir string) *DBConfig {
		dbc := newFreezerTestConfig(dir, false, 1)
		dbc.FreezerReadOnly = true
		dbc.AncientDir = ancientDir
		return dbc
	}
	dbm = openFreezerTestDBM(t, readOnly(""))
	assert.Equal(t, uint64(5), dbm.FrozenBlocks())
	assert.Nil(t, dbm.freezerTrigger, "no freeze loop")
	for _, b := range blocks {
		assertBlockReadable(t, dbm, b)
	}
	for _, b := range blocks[5:] {
		assertBlockInKV(t, dbm, b) // threshold 1 is ignored
	}
	dbm.Close()

	// A moved freezer is found with the ancient directory.
	movedRoot := filepath.Join(t.TempDir(), "moved")
	require.NoError(t, os.Rename(filepath.Join(dir, DefaultAncientDirName), movedRoot))
	dbm = openFreezerTestDBM(t, readOnly(movedRoot))
	assert.Equal(t, uint64(5), dbm.FrozenBlocks())
	assertBlockReadable(t, dbm, blocks[1])
	dbm.Close()

	// Without frozen blocks, read-only mode opens no freezer.
	dbm = openFreezerTestDBM(t, &DBConfig{Dir: t.TempDir(), DBType: LevelDB, NumStateTrieShards: 1, FreezerReadOnly: true})
	assert.Nil(t, dbm.freezer)
	dbm.Close()
}

// TestFreezer_Migration checks that a DB migration copies the freezer and that
// the migrated database reads frozen blocks from its own copy.
func TestFreezer_Migration(t *testing.T) {
	srcDir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(srcDir, true, 5))
	blocks := writeFreezerTestChain(t, dbm, 10)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(5), dbm.FrozenBlocks())
	dbm.Close()

	srcDBC := newFreezerTestConfig(srcDir, false, 0)
	srcDBC.FreezerReadOnly = true
	src := openFreezerTestDBM(t, srcDBC)

	// A destination that cannot hold the freezer is refused before copying.
	single := openFreezerTestDBM(t, &DBConfig{Dir: t.TempDir(), DBType: LevelDB, SingleDB: true, NumStateTrieShards: 1})
	assert.ErrorContains(t, src.StartDBMigration(single), "does not support the freezer")
	single.Close()

	dstDir := t.TempDir()
	dst := openFreezerTestDBM(t, newFreezerTestConfig(dstDir, false, 0))
	require.NoError(t, src.StartDBMigration(dst))
	src.Close()
	dst.Close()

	// The source freezer is no longer needed.
	require.NoError(t, os.RemoveAll(filepath.Join(srcDir, DefaultAncientDirName)))
	dst = openFreezerTestDBM(t, newFreezerTestConfig(dstDir, true, 5))
	defer dst.Close()
	assert.Equal(t, uint64(5), dst.FrozenBlocks())
	for _, b := range blocks {
		assertBlockReadable(t, dst, b)
	}
}

// TestFreezer_ReadError checks that a failing freezer read is counted rather
// than silently reported as a missing block.
func TestFreezer_ReadError(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 5))
	defer dbm.Close()
	blocks := writeFreezerTestChain(t, dbm, 10)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(5), dbm.FrozenBlocks())

	// Reads of blocks that are not frozen never touch the freezer.
	before := freezerReadErrorCounter.Count()
	assert.Nil(t, dbm.ReadHeader(common.HexToHash("0xdead"), 100))
	assert.Equal(t, before, freezerReadErrorCounter.Count())

	// Simulate a freezer that can no longer be read.
	require.NoError(t, dbm.freezer.Close())
	dbm.ClearHeaderChainCache()
	dbm.ClearBlockChainCache()
	assert.Nil(t, dbm.ReadBodyRLP(blocks[2].hash(), 2))
	assert.Greater(t, freezerReadErrorCounter.Count(), before)
	assertBlockInKV(t, dbm, blocks[7])
	assertBlockReadable(t, dbm, blocks[7])
}

// TestFreezer_Support checks which database setups open the freezer. One
// without freezer support runs without it.
func TestFreezer_Support(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dbc      DBConfig
		readonly bool
		enabled  bool
	}{
		{name: "disabled", dbc: DBConfig{DBType: MemoryDB}},
		{name: "leveldb", dbc: DBConfig{DBType: LevelDB, EnableFreezer: true}, enabled: true},
		{name: "memory", dbc: DBConfig{DBType: MemoryDB, EnableFreezer: true}},
		{name: "memory read-only", dbc: DBConfig{DBType: MemoryDB, FreezerReadOnly: true}, readonly: true},
		{name: "dynamo", dbc: DBConfig{DBType: DynamoDB, EnableFreezer: true}},
		{name: "single", dbc: DBConfig{DBType: LevelDB, SingleDB: true, EnableFreezer: true}},
		{name: "secondary rocksdb", dbc: DBConfig{DBType: RocksDB, EnableFreezer: true, RocksDBConfig: &RocksDBConfig{Secondary: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.enabled, checkFreezerSupport(&tc.dbc, tc.readonly))
		})
	}
}

// TestFreezer_Unsupported checks that databases without freezer support open
// without it.
func TestFreezer_Unsupported(t *testing.T) {
	for _, dbc := range []*DBConfig{
		{DBType: MemoryDB, EnableFreezer: true},
		{Dir: t.TempDir(), DBType: LevelDB, SingleDB: true, EnableFreezer: true},
	} {
		dbm := openFreezerTestDBM(t, dbc)
		assert.Nil(t, dbm.freezer, "DBType %s, single %v", dbc.DBType, dbc.SingleDB)
		dbm.Close()
	}
}

// TestFrozenHeader_Derived checks that the derived format restores headers
// with and without the optional fields, and that a header whose parent or
// number differ is stored unchanged.
func TestFrozenHeader_Derived(t *testing.T) {
	parent := common.HexToHash("0xabcd")
	blobGas := uint64(7)
	for name, h := range map[string]*types.Header{
		"legacy": {
			ParentHash: parent, Number: big.NewInt(300), BlockScore: big.NewInt(1), Time: big.NewInt(9),
			Extra: []byte("extra"), Governance: []byte{}, Vote: []byte{},
		},
		"randao": {
			ParentHash: parent, Number: big.NewInt(1 << 40), BlockScore: big.NewInt(1), Time: big.NewInt(9),
			Extra: []byte("extra"), Governance: []byte{1}, Vote: []byte{2}, BaseFee: big.NewInt(25),
			RandomReveal: make([]byte, 96), MixHash: make([]byte, 32), BlobGasUsed: &blobGas,
		},
	} {
		t.Run(name, func(t *testing.T) {
			number := h.Number.Uint64()
			data, err := rlp.EncodeToBytes(h)
			require.NoError(t, err)

			frozen := frozenHeader(number, parent, data)
			require.Equal(t, byte(derivedHeaderPrefix), frozen[0])
			assert.Less(t, len(frozen), len(data)-common.HashLength)
			restored, err := decodeDerivedHeader(frozen[1:], number, parent)
			require.NoError(t, err)
			assert.Equal(t, data, restored)

			// Another parent or number keeps the header.
			assert.Equal(t, data, frozenHeader(number, common.HexToHash("0x01"), data))
			assert.Equal(t, data, frozenHeader(number+1, parent, data))
		})
	}
	// The genesis is kept.
	genesis, _ := rlp.EncodeToBytes(&types.Header{Number: big.NewInt(0), BlockScore: big.NewInt(1), Time: big.NewInt(0)})
	assert.Equal(t, genesis, frozenHeader(0, common.Hash{}, genesis))
}

// TestFreezer_DerivedHeaders checks that the freeze loop stores headers in the
// derived format and reads them back.
func TestFreezer_DerivedHeaders(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 2))
	defer dbm.Close()
	blocks := writeFreezerTestChain(t, dbm, 8)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(6), dbm.FrozenBlocks())

	for _, b := range blocks[:6] {
		number := b.header.Number.Uint64()
		item, err := dbm.freezer.Ancient(ancient.ChainFreezerHeaderTable, number)
		require.NoError(t, err)
		if number == 0 {
			assert.NotEqual(t, byte(derivedHeaderPrefix), item[0])
			continue
		}
		assert.Equal(t, byte(derivedHeaderPrefix), item[0], "header %d", number)
		want, _ := rlp.EncodeToBytes(b.header)
		assert.Equal(t, want, []byte(dbm.ReadHeaderRLP(b.hash(), number)), "header %d", number)
		assert.Equal(t, b.hash(), dbm.ReadHeader(b.hash(), number).Hash(), "header %d", number)
	}
}

// TestFreezer_BAL checks that the freezer moves the block access lists of the
// blocks it freezes, empty for a block without one, that they read the same
// from the freezer, and that tables added to an existing freezer start at its
// head, leaving the lists of the blocks frozen before in the key-value store.
func TestFreezer_BAL(t *testing.T) {
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 2))
	blocks := writeFreezerTestChain(t, dbm, 10)
	bal := func(n uint64) []byte { return []byte{0xc1, byte(n)} }
	reads := func(n uint64) []byte { return []byte{0xc2, byte(n), 0x80} }
	for _, b := range blocks[1:] {
		if n := b.header.Number.Uint64(); n%2 == 1 {
			require.NoError(t, dbm.WriteBAL(b.hash(), n, bal(n)))
			require.NoError(t, dbm.WriteBALReads(b.hash(), n, reads(n)))
		}
	}
	triggerFreeze(t, dbm)
	frozen := dbm.FrozenBlocks()
	require.Equal(t, uint64(8), frozen)

	check := func(dbm *databaseManager) {
		for _, b := range blocks[1:] {
			hash, n := b.hash(), b.header.Number.Uint64()
			if n%2 == 1 {
				assert.Equal(t, bal(n), dbm.ReadBAL(hash, n), "bal %d", n)
				assert.Equal(t, reads(n), dbm.ReadBALReads(hash, n), "reads %d", n)
				assert.True(t, dbm.HasBAL(hash, n), "has bal %d", n)
			} else {
				assert.Nil(t, dbm.ReadBAL(hash, n), "no bal %d", n)
				assert.Nil(t, dbm.ReadBALReads(hash, n), "no reads %d", n)
				assert.False(t, dbm.HasBAL(hash, n), "has no bal %d", n)
			}
			// Not under another hash.
			assert.Nil(t, dbm.ReadBAL(common.HexToHash("0x01"), n))
		}
	}
	for _, b := range blocks[1:] {
		hash, n := b.hash(), b.header.Number.Uint64()
		assert.Equal(t, n >= frozen && n%2 == 1, hasKV(dbm, BALDB, balKey(balPrefix, n, hash)), "kv bal %d", n)
		assert.Equal(t, n >= frozen && n%2 == 1, hasKV(dbm, BALDB, balKey(balReadsPrefix, n, hash)), "kv reads %d", n)
	}
	check(dbm)
	dbm.Close()

	// A freezer from before the tables: they are added at its head, and the
	// lists of the blocks frozen before are still in the key-value store.
	chain := filepath.Join(dir, DefaultAncientDirName, ancient.ChainFreezerName)
	for _, table := range []string{ancient.ChainFreezerBALTable, ancient.ChainFreezerBALReadsTable} {
		files, err := filepath.Glob(filepath.Join(chain, table+".*"))
		require.NoError(t, err)
		require.NotEmpty(t, files)
		for _, f := range files {
			require.NoError(t, os.Remove(f))
		}
	}
	dbm = openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 100))
	defer dbm.Close()
	require.Equal(t, frozen, dbm.FrozenBlocks())
	tail, err := dbm.freezer.Tail(ancient.ChainFreezerBALGroup)
	require.NoError(t, err)
	require.Equal(t, frozen, tail)
	for _, b := range blocks[1:frozen] {
		if n := b.header.Number.Uint64(); n%2 == 1 {
			require.NoError(t, dbm.WriteBAL(b.hash(), n, bal(n)))
			require.NoError(t, dbm.WriteBALReads(b.hash(), n, reads(n)))
		}
	}
	errors := freezerReadErrorCounter.Count()
	check(dbm)
	assert.Equal(t, errors, freezerReadErrorCounter.Count(), "no read errors below the tail of the tables")
}

// TestFreezer_Unfreeze checks that the frozen blocks and their block access
// lists move back into the key-value store, that the database then opens
// without the freezer, and that they can be frozen again.
func TestFreezer_Unfreeze(t *testing.T) {
	setBloomBitsBlocks(t, 2)
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 2))
	blocks := writeFreezerTestChain(t, dbm, 10)
	bal := func(n uint64) []byte { return []byte{0xc1, byte(n)} }
	for _, b := range blocks[1:] {
		if n := b.header.Number.Uint64(); n%2 == 1 {
			require.NoError(t, dbm.WriteBAL(b.hash(), n, bal(n)))
			require.NoError(t, dbm.WriteBALReads(b.hash(), n, bal(n+100)))
		}
	}
	for section := uint64(0); section < 4; section++ {
		writeBloomSection(t, dbm, section, blocks[(section+1)*2-1].hash())
	}
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(8), dbm.FrozenBlocks())
	assertBlockFrozen(t, dbm, blocks[3])

	var last uint64
	require.NoError(t, dbm.Unfreeze(func(next, frozen uint64) { last = next }))
	require.Equal(t, uint64(8), last)
	require.Equal(t, uint64(0), dbm.FrozenBlocks())
	check := func(dbm *databaseManager) {
		for _, b := range blocks {
			assertBlockInKV(t, dbm, b)
			assertBlockReadable(t, dbm, b)
			hash, n := b.hash(), b.header.Number.Uint64()
			if n > 0 && n%2 == 1 {
				assert.True(t, hasKV(dbm, BALDB, balKey(balPrefix, n, hash)), "kv bal %d", n)
				assert.Equal(t, bal(n), dbm.ReadBAL(hash, n), "bal %d", n)
				assert.Equal(t, bal(n+100), dbm.ReadBALReads(hash, n), "reads %d", n)
			}
		}
		for section := uint64(0); section < 4; section++ {
			bits, err := dbm.ReadBloomBits(BloomBitsKey(0, section, blocks[(section+1)*2-1].hash()))
			require.NoError(t, err, "bloom bits of section %d", section)
			assert.Equal(t, []byte{byte(section), 0}, bits)
		}
	}
	check(dbm)
	// It does not freeze again on its own.
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(0), dbm.FrozenBlocks())
	dbm.Close()

	// Without the freezer, as a version from before it.
	dbm = openFreezerTestDBM(t, newFreezerTestConfig(dir, false, 0))
	check(dbm)
	dbm.Close()

	// And frozen again.
	dbm = openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 2))
	defer dbm.Close()
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(8), dbm.FrozenBlocks())
	for _, b := range blocks[1:8] {
		assertBlockFrozen(t, dbm, b)
		assertBlockReadable(t, dbm, b)
		if n := b.header.Number.Uint64(); n%2 == 1 {
			assert.Equal(t, bal(n), dbm.ReadBAL(b.hash(), n), "bal %d", n)
		}
	}
}

// TestFreezer_RestoreFormats checks that after Unfreeze the total blockscores,
// the receipts in their storage form, the tx lookup entries with their block
// hash and the pruning marks in the misc database are written again, as a
// version without the freezer reads them.
func TestFreezer_RestoreFormats(t *testing.T) {
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 2))
	defer dbm.Close()
	blocks := writeFreezerTestChain(t, dbm, 10)
	var want []types.Receipts
	for _, b := range blocks {
		block := types.NewBlockWithHeader(b.header).WithBody(b.body.Transactions)
		dbm.WriteBlockReceipts(block, b.receipts) // the slim form of this version
		dbm.WriteTxLookupEntries(block)
		want = append(want, dbm.ReadReceipts(b.hash(), b.header.Number.Uint64()))
	}
	marks := []PruningMark{{Number: 3, Hash: common.HexToHash("0x33").ExtendZero()}, {Number: 4, Hash: common.HexToHash("0x44").ExtendZero()}}
	dbm.WritePruningMarks(marks)
	require.Empty(t, dbm.ReadPruningMarks(0, 0), "the marks are in their freezer")
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(8), dbm.FrozenBlocks())

	require.Error(t, dbm.RestoreFreezerFormats(nil), "not before unfreezing")
	require.NoError(t, dbm.Unfreeze(nil))
	stages := make(map[string]uint64)
	require.NoError(t, dbm.RestoreFreezerFormats(func(stage string, n uint64) { stages[stage] = n }))
	assert.Equal(t, uint64(10), stages["total blockscores"])
	assert.Equal(t, uint64(10), stages["receipts"])
	assert.Equal(t, uint64(10), stages["tx lookup entries"]) // one transaction in every test block
	assert.Equal(t, uint64(2), stages["pruning marks"])

	misc := dbm.getDatabase(MiscDB)
	for i, b := range blocks {
		hash, n := b.hash(), b.header.Number.Uint64()
		data, err := misc.Get(headerTDKey(n, hash))
		require.NoError(t, err, "td %d", n)
		var td big.Int
		require.NoError(t, rlp.DecodeBytes(data, &td))
		assert.Equal(t, n+1, td.Uint64(), "td %d", n)

		raw, _ := dbm.getDatabase(ReceiptsDB).Get(blockReceiptsKey(n, hash))
		require.NotEmpty(t, raw)
		assert.GreaterOrEqual(t, raw[0], byte(0xc0), "receipts %d in their storage form", n)
		var storage []*types.ReceiptForStorage
		require.NoError(t, rlp.DecodeBytes(raw, &storage), "receipts %d", n)
		assert.Equal(t, len(want[i]), len(dbm.ReadReceipts(hash, n)))
		for j, r := range dbm.ReadReceipts(hash, n) {
			assert.Equal(t, want[i][j].TxHash, r.TxHash)
			assert.Equal(t, want[i][j].GasUsed, r.GasUsed)
			assert.Equal(t, want[i][j].Status, r.Status)
		}
		for _, tx := range b.body.Transactions {
			full, _ := readTxLookupRaw(t, dbm, tx.Hash())
			assert.True(t, full, "tx lookup entry of block %d with its block hash", n)
		}
	}
	assert.ElementsMatch(t, marks, dbm.ReadPruningMarks(0, 0))
	tail, _ := dbm.pruningMarks.Tail(ancient.PruningMarkGroup)
	head, _ := dbm.pruningMarks.Ancients()
	assert.Equal(t, head, tail, "the mark freezer is consumed")

	// Once more, as after a crash.
	require.NoError(t, dbm.RestoreFreezerFormats(nil))
}

// TestFreezer_DerivedTd checks that the freezer deletes the total blockscores
// an older version stored, of the genesis once it starts and of the blocks it
// freezes, and that the total blockscore of a block is still that of the
// genesis plus the number.
func TestFreezer_DerivedTd(t *testing.T) {
	dir := t.TempDir()
	// The keys of an older version.
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, false, 0))
	blocks := writeFreezerTestChain(t, dbm, 8)
	misc := dbm.getDatabase(MiscDB)
	for _, b := range blocks {
		number := b.header.Number.Uint64()
		data, err := rlp.EncodeToBytes(new(big.Int).SetUint64(number + 1))
		require.NoError(t, err)
		require.NoError(t, misc.Put(headerTDKey(number, b.hash()), data))
	}
	dbm.Close()

	dbm = openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 2))
	defer dbm.Close()
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(6), dbm.FrozenBlocks())

	for _, b := range blocks {
		hash, number := b.hash(), b.header.Number.Uint64()
		assert.Equal(t, number >= 6, hasKV(dbm, MiscDB, headerTDKey(number, hash)), "kv td %d", number)
		assert.Equal(t, new(big.Int).SetUint64(number+1), dbm.ReadTd(hash, number), "td %d", number)
	}
	// Not for another hash at a frozen height.
	assert.Nil(t, dbm.ReadTd(common.HexToHash("0x01"), 4))
}

func readTxLookupRaw(t *testing.T, dbm *databaseManager, txHash common.Hash) (full bool, slim bool) {
	data, _ := dbm.getDatabase(TxLookUpEntryDB).Get(TxLookupKey(txHash))
	require.NotEmpty(t, data)
	var entry TxLookupEntry
	if rlp.DecodeBytes(data, &entry) == nil {
		return true, false
	}
	var s slimTxLookupEntry
	require.NoError(t, rlp.DecodeBytes(data, &s))
	return false, true
}

// TestFreezer_SlimTxLookups checks that tx lookup entries are written without
// the block hash, that the freezer rewrites the full entries an older version
// wrote for the blocks it freezes, and that both kinds resolve.
func TestFreezer_SlimTxLookups(t *testing.T) {
	dir := t.TempDir()
	// Full entries, as an older version wrote them.
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, false, 0))
	blocks := writeFreezerTestChain(t, dbm, 8)
	for _, b := range blocks {
		block := types.NewBlockWithHeader(b.header).WithBody(b.body.Transactions)
		for i, tx := range block.Transactions() {
			data, err := rlp.EncodeToBytes(TxLookupEntry{BlockHash: block.Hash(), BlockIndex: block.NumberU64(), Index: uint64(i)})
			require.NoError(t, err)
			require.NoError(t, dbm.getDatabase(TxLookUpEntryDB).Put(TxLookupKey(tx.Hash()), data))
		}
		full, _ := readTxLookupRaw(t, dbm, b.body.Transactions[0].Hash())
		require.True(t, full)
	}
	dbm.Close()

	// New entries are slim, and the frozen blocks' entries are rewritten.
	dbm = openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 2))
	defer dbm.Close()
	parent := blocks[len(blocks)-1].hash()
	for i := 8; i < 10; i++ {
		b := genFreezerTestBlock(t, uint64(i), parent, "canonical")
		writeFreezerTestBlock(dbm, b, true)
		dbm.WriteTxLookupEntries(types.NewBlockWithHeader(b.header).WithBody(b.body.Transactions))
		blocks = append(blocks, b)
		parent = b.hash()
	}
	dbm.WriteHeadBlockHash(parent)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(8), dbm.FrozenBlocks())

	for _, b := range blocks[1:] {
		hash, number := b.hash(), b.header.Number.Uint64()
		txHash := b.body.Transactions[0].Hash()
		_, slim := readTxLookupRaw(t, dbm, txHash)
		// Blocks 1-7 were frozen and rewritten; 8 and 9 were written slim.
		assert.True(t, slim, "slim entry %d", number)

		gotHash, gotNumber, index := dbm.ReadTxLookupEntry(txHash)
		assert.Equal(t, hash, gotHash, "block hash %d", number)
		assert.Equal(t, number, gotNumber)
		assert.Equal(t, uint64(0), index)
		tx, _, _, _ := dbm.ReadTxAndLookupInfo(txHash)
		if assert.NotNil(t, tx, "tx %d", number) {
			assert.Equal(t, txHash, tx.Hash())
		}
	}

	// A slim entry whose block does not hold the transaction resolves to nothing.
	stale := common.HexToHash("0x5a1e")
	data, _ := rlp.EncodeToBytes(slimTxLookupEntry{BlockIndex: 3, Index: 0})
	require.NoError(t, dbm.getDatabase(TxLookUpEntryDB).Put(TxLookupKey(stale), data))
	gotHash, _, _ := dbm.ReadTxLookupEntry(stale)
	assert.Equal(t, common.Hash{}, gotHash)
}

// TestFreezer_SlimTxLookupsPass checks that the pass over the tx lookup store
// rewrites the full entries without the block hash a batch at a time, resumes
// where it stopped, leaves the slim ones as they are, and stops once done.
func TestFreezer_SlimTxLookupsPass(t *testing.T) {
	old := slimTxLookupsBatch
	slimTxLookupsBatch = 7
	t.Cleanup(func() { slimTxLookupsBatch = old })

	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), false, 0))
	defer dbm.Close()
	db := dbm.getDatabase(TxLookUpEntryDB)

	const n = 50
	want := make(map[common.Hash]slimTxLookupEntry, n)
	for i := 0; i < n; i++ {
		hash := crypto.Keccak256Hash([]byte{byte(i)})
		entry := slimTxLookupEntry{BlockIndex: uint64(100 + i), Index: uint64(i % 3)}
		var data []byte
		var err error
		if i%5 == 0 { // Already slim.
			data, err = rlp.EncodeToBytes(entry)
		} else {
			data, err = rlp.EncodeToBytes(TxLookupEntry{BlockHash: common.HexToHash("0x0b"), BlockIndex: entry.BlockIndex, Index: entry.Index})
		}
		require.NoError(t, err)
		require.NoError(t, db.Put(TxLookupKey(hash), data))
		want[hash] = entry
	}

	// One batch, then the rest from where it stopped.
	require.True(t, dbm.slimTxLookups())
	from, _ := dbm.getDatabase(MiscDB).Get(slimTxLookupsKey)
	require.Len(t, from, common.HashLength, "the pass stores where it stopped")
	full := 0
	for hash := range want {
		if f, _ := readTxLookupRaw(t, dbm, hash); f {
			full++
		}
	}
	require.Equal(t, n-n/5-slimTxLookupsBatch+1, full, "one batch of %d entries, one of them already slim, is rewritten", slimTxLookupsBatch)

	calls := 2 // the first one and the last one, which finds none left
	for dbm.slimTxLookups() {
		calls++
	}
	require.Equal(t, (n+slimTxLookupsBatch-1)/slimTxLookupsBatch, calls, "one call per batch")
	for hash, entry := range want {
		_, slim := readTxLookupRaw(t, dbm, hash)
		require.True(t, slim)
		data, _ := db.Get(TxLookupKey(hash))
		var got slimTxLookupEntry
		require.NoError(t, rlp.DecodeBytes(data, &got))
		require.Equal(t, entry, got)
	}

	// Done: a new full entry is left as it is.
	late := crypto.Keccak256Hash([]byte("late"))
	data, err := rlp.EncodeToBytes(TxLookupEntry{BlockHash: common.HexToHash("0x0c"), BlockIndex: 1, Index: 0})
	require.NoError(t, err)
	require.NoError(t, db.Put(TxLookupKey(late), data))
	require.False(t, dbm.slimTxLookups())
	f, _ := readTxLookupRaw(t, dbm, late)
	require.True(t, f)
}

// setBloomBitsBlocks shortens the bloom bits sections for a test.
func setBloomBitsBlocks(t *testing.T, n uint64) {
	old := bloomBitsSection
	bloomBitsSection = n
	t.Cleanup(func() { bloomBitsSection = old })
}

// writeBloomSection stores the bloom bits of a few bits of a section under
// the key with the given section head hash, as the bloom indexer does.
func writeBloomSection(t *testing.T, dbm *databaseManager, section uint64, head common.Hash) {
	for bit := uint(0); bit < 3; bit++ {
		dbm.WriteBloomBits(BloomBitsKey(bit, section, head), []byte{byte(section), byte(bit)})
	}
}

// TestFreezer_BloomBits checks that the bloom bits of frozen sections are
// rewritten without the section head hash, that sections are read under
// either key, and that a rewind into the freezer drops the rewritten bits of
// the sections no longer frozen.
func TestFreezer_BloomBits(t *testing.T) {
	setBloomBitsBlocks(t, 4)
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 2))
	defer dbm.Close()
	db := dbm.getDatabase(MiscDB)
	blocks := writeFreezerTestChain(t, dbm, 14) // blocks 0-11 are frozen: sections 0-2
	head := func(section uint64) common.Hash { return blocks[(section+1)*4-1].hash() }
	stale := common.Hash{0xff}

	writeBloomSection(t, dbm, 0, head(0))
	writeBloomSection(t, dbm, 1, head(1))
	writeBloomSection(t, dbm, 1, stale)
	writeBloomSection(t, dbm, 3, common.Hash{0x3})
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(12), dbm.FrozenBlocks())

	// Section 2 is not indexed yet, so the rewrite stops there.
	assert.Equal(t, uint64(2), dbm.frozenBloomSections())
	for section := uint64(0); section < 2; section++ {
		for bit := uint(0); bit < 3; bit++ {
			assert.False(t, hasKV(dbm, MiscDB, BloomBitsKey(bit, section, head(section))))
			assert.True(t, hasKV(dbm, MiscDB, BloomBitsShortKey(bit, section)))
			data, err := dbm.ReadBloomBits(BloomBitsKey(bit, section, head(section)))
			require.NoError(t, err)
			assert.Equal(t, []byte{byte(section), byte(bit)}, data)
		}
	}
	assert.False(t, hasKV(dbm, MiscDB, BloomBitsKey(0, 1, stale)), "a rewound section is deleted")

	writeBloomSection(t, dbm, 2, head(2))
	triggerFreeze(t, dbm)
	assert.Equal(t, uint64(3), dbm.frozenBloomSections())
	assert.True(t, hasKV(dbm, MiscDB, BloomBitsShortKey(1, 2)))
	// Section 3 is not frozen and keeps the hash.
	assert.True(t, hasKV(dbm, MiscDB, BloomBitsKey(1, 3, common.Hash{0x3})))
	assert.False(t, hasKV(dbm, MiscDB, BloomBitsShortKey(1, 3)))
	data, err := dbm.ReadBloomBits(BloomBitsKey(1, 3, common.Hash{0x3}))
	require.NoError(t, err)
	assert.Equal(t, []byte{3, 1}, data)

	// A rewind to block 5 keeps section 0 frozen.
	_, resume := dbm.PauseFreezer()
	require.NoError(t, dbm.TruncateFrozen(6))
	resume()
	assert.Equal(t, uint64(1), dbm.frozenBloomSections())
	assert.True(t, hasKV(dbm, MiscDB, BloomBitsShortKey(0, 0)))
	for section := uint64(1); section < 3; section++ {
		_, err := dbm.ReadBloomBits(BloomBitsKey(0, section, head(section)))
		assert.Error(t, err, "section %d", section)
	}
	has, _ := db.Has(frozenBloomSectionsKey)
	assert.True(t, has)
}

// TestFreezer_LegacyKeys checks that the legacy istanbul snapshots and
// governance entries of frozen blocks are deleted once the module migrations
// are done, and those of blocks not frozen are kept.
func TestFreezer_LegacyKeys(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 2))
	defer dbm.Close()
	db := dbm.getDatabase(MiscDB)
	blocks := writeFreezerTestChain(t, dbm, 14) // blocks 0-11 are frozen

	for _, num := range []uint64{0, 5, 13} {
		require.NoError(t, db.Put(makeKey(governancePrefix, num), []byte{1}))
	}
	unknown := common.Hash{0xee}
	for _, hash := range []common.Hash{blocks[0].hash(), blocks[8].hash(), blocks[12].hash(), unknown} {
		require.NoError(t, db.Put(snapshotKey(hash), []byte{1}))
	}
	require.NoError(t, db.Put(governanceHistoryKey, []byte{1}))
	has := func(key []byte) bool { return hasKV(dbm, MiscDB, key) }

	// Nothing is deleted before the migrations are done.
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(12), dbm.FrozenBlocks())
	assert.True(t, has(makeKey(governancePrefix, 0)))
	assert.True(t, has(snapshotKey(blocks[0].hash())))

	var zero [8]byte
	binary.BigEndian.PutUint64(zero[:], 0)
	require.NoError(t, db.Put(lowestScannedValidatorVoteNumKey, zero[:]))
	require.NoError(t, db.Put(govDataBlockNumsKey, []byte("[]")))
	triggerFreeze(t, dbm)
	assert.False(t, has(makeKey(governancePrefix, 0)))
	assert.False(t, has(makeKey(governancePrefix, 5)))
	assert.True(t, has(makeKey(governancePrefix, 13)))
	assert.False(t, has(snapshotKey(blocks[0].hash())))
	assert.False(t, has(snapshotKey(blocks[8].hash())))
	assert.False(t, has(snapshotKey(unknown)))
	assert.True(t, has(snapshotKey(blocks[12].hash())))
	assert.True(t, has(governanceHistoryKey))
	assert.False(t, has(legacyKeysDeletedKey))
	assert.True(t, has(govDataBlockNumsKey), "the module keys are kept")

	// Once no legacy key is left, the cleanup is done.
	require.NoError(t, db.Delete(makeKey(governancePrefix, 13)))
	require.NoError(t, db.Delete(snapshotKey(blocks[12].hash())))
	triggerFreeze(t, dbm)
	assert.False(t, has(governanceHistoryKey))
	assert.True(t, has(legacyKeysDeletedKey))
}

// TestFreezer_SlimReceipts checks that the freeze loop stores receipts in the
// derived format when they match their block, in the slim format otherwise,
// and that both read back as they were written.
func TestFreezer_SlimReceipts(t *testing.T) {
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(dir, true, 2))
	defer dbm.Close()

	blocks := writeFreezerTestChain(t, dbm, 6)
	// The logs of the test chain carry another block number, so they are frozen
	// in the slim format. Block 2 gets receipts that match it.
	b := blocks[2]
	receipts := genDerivableReceipts(b.hash(), 2, b.body.Transactions)
	dbm.WriteReceipts(b.hash(), 2, receipts)
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(4), dbm.FrozenBlocks())

	for _, fb := range blocks[1:4] {
		number := fb.header.Number.Uint64()
		item, err := dbm.freezer.Ancient(ancient.ChainFreezerReceiptTable, number)
		require.NoError(t, err)
		want, wantReceipts := byte(slimReceiptsPrefix), fb.receipts
		if number == 2 {
			want, wantReceipts = derivedReceiptsPrefix, receipts
		}
		assert.Equal(t, want, item[0], "receipts %d", number)
		assert.Equal(t, wantReceipts, dbm.ReadReceipts(fb.hash(), number), "receipts %d", number)
	}
	assert.Equal(t, receipts, dbm.ReadReceiptsByBlockHash(b.hash()))
}

// TestFreezer_MovesSlimReceipts checks that freezing moves receipts written in
// a slim format as they are.
func TestFreezer_MovesSlimReceipts(t *testing.T) {
	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 2))
	defer dbm.Close()

	blocks := writeFreezerTestChain(t, dbm, 6)
	wantPrefix := map[uint64]byte{}
	for _, fb := range blocks[1:4] {
		number := fb.header.Number.Uint64()
		block := types.NewBlockWithHeader(fb.header).WithBody(fb.body.Transactions)
		wantPrefix[number] = slimReceiptsPrefix // the logs carry another block number
		if number == 2 {
			fb.receipts = genDerivableReceipts(fb.hash(), number, fb.body.Transactions)
			wantPrefix[number] = derivedReceiptsPrefix
		}
		dbm.WriteBlockReceipts(block, fb.receipts)
	}
	triggerFreeze(t, dbm)
	require.Equal(t, uint64(4), dbm.FrozenBlocks())
	for _, fb := range blocks[1:4] {
		number := fb.header.Number.Uint64()
		item, err := dbm.freezer.Ancient(ancient.ChainFreezerReceiptTable, number)
		require.NoError(t, err)
		assert.Equal(t, wantPrefix[number], item[0], "frozen receipts %d", number)
		assert.Equal(t, fb.receipts, dbm.ReadReceipts(fb.hash(), number), "frozen receipts %d", number)
	}
}

// TestPruningMarks_Freezer checks that with the block freezer, pruning marks
// are appended to the pruning mark freezer and consumed from its tail, and that
// marks in the misc database are still consumed.
func TestPruningMarks_Freezer(t *testing.T) {
	dir := t.TempDir()
	dbc := newFreezerTestConfig(dir, true, 10)
	dbm := openFreezerTestDBM(t, dbc)
	require.NotNil(t, dbm.pruningMarks)

	node := func(i byte) common.ExtHash { return common.Hash{i}.ExtendZero() }
	trieDB := dbm.getDatabase(StateTrieDB)
	for i := byte(1); i <= 6; i++ {
		require.NoError(t, trieDB.Put(TrieNodeKey(node(i)), []byte{i}))
	}
	hasNode := func(i byte) bool {
		has, _ := trieDB.Has(TrieNodeKey(node(i)))
		return has
	}
	require.NoError(t, dbm.getDatabase(MiscDB).Put(pruningMarkKey(PruningMark{2, node(6)}), pruningMarkValue))
	dbm.WritePruningMarks([]PruningMark{{1, node(1)}, {2, node(2)}})
	dbm.WritePruningMarks(nil)
	dbm.WritePruningMarks([]PruningMark{{3, node(3)}})
	dbm.WritePruningMarks([]PruningMark{{5, node(5)}, {4, node(4)}})
	items, _ := dbm.pruningMarks.Ancients()
	assert.Equal(t, uint64(3), items)
	assert.Empty(t, dbm.ReadPruningMarks(3, 0), "marks are not put to the misc database")

	assert.Equal(t, 4, dbm.PruneMarkedTrieNodes(1, 4, nil))
	for i := byte(1); i <= 6; i++ {
		assert.Equal(t, i == 4 || i == 5, hasNode(i), "node %d", i)
	}
	assert.Empty(t, dbm.ReadPruningMarks(1, 0))
	dbm.Close()

	dbm = openFreezerTestDBM(t, dbc)
	defer dbm.Close()
	tail, _ := dbm.pruningMarks.Tail("marks")
	assert.Equal(t, uint64(2), tail)
	assert.Equal(t, 0, dbm.PruneMarkedTrieNodes(4, 5, nil))
	// The marks are handed to the given prune function, as the BAL hooks do.
	var pruned []PruningMark
	assert.Equal(t, 2, dbm.PruneMarkedTrieNodes(4, 6, func(marks []PruningMark) {
		pruned = append(pruned, marks...)
		dbm.PruneTrieNodes(marks)
	}))
	assert.ElementsMatch(t, []PruningMark{{5, node(5)}, {4, node(4)}}, pruned)
	assert.False(t, hasNode(4))
	assert.False(t, hasNode(5))
	assert.Equal(t, 0, dbm.PruneMarkedTrieNodes(6, 10, nil))

	// Without the freezer, the marks stay in the misc database.
	mem := NewMemoryDBManager()
	mem.WritePruningMarks([]PruningMark{{1, node(1)}})
	assert.Len(t, mem.ReadPruningMarks(1, 0), 1)
	assert.Equal(t, 1, mem.PruneMarkedTrieNodes(1, 2, nil))
	assert.Empty(t, mem.ReadPruningMarks(1, 0))
}

// TestPruningMarks_DeleteFrom deletes the marks of the blocks a rewind deleted,
// from the freezer and from the misc database: whole items at the head of the
// freezer, and the marks at or above the block of the item it stops in.
func TestPruningMarks_DeleteFrom(t *testing.T) {
	node := func(i byte) common.ExtHash { return common.Hash{i}.ExtendZero() }
	marksOf := func(dbm DBManager) []PruningMark {
		var marks []PruningMark
		dbm.PruneMarkedTrieNodes(0, 0, func(m []PruningMark) { marks = append(marks, m...) })
		return marks
	}

	dbm := openFreezerTestDBM(t, newFreezerTestConfig(t.TempDir(), true, 10))
	defer dbm.Close()
	dbm.WritePruningMarks([]PruningMark{{1, node(1)}, {2, node(2)}})
	dbm.WritePruningMarks([]PruningMark{{3, node(3)}, {4, node(4)}})
	dbm.WritePruningMarks([]PruningMark{{5, node(5)}})
	dbm.DeletePruningMarksFrom(4)
	items, _ := dbm.pruningMarks.Ancients()
	assert.Equal(t, uint64(2), items)
	assert.ElementsMatch(t, []PruningMark{{1, node(1)}, {2, node(2)}, {3, node(3)}}, marksOf(dbm))

	mem := NewMemoryDBManager()
	mem.WritePruningMarks([]PruningMark{{1, node(1)}, {3, node(3)}, {4, node(4)}})
	mem.DeletePruningMarksFrom(3)
	assert.Equal(t, []PruningMark{{1, node(1)}}, mem.ReadPruningMarks(0, 0))
}

// genDerivableReceipts makes receipts for the transactions of block (hash,
// number) whose derived fields are set as block processing sets them. Receipt
// i has i logs with up to three topics each, and receipt 1 created a contract.
func genDerivableReceipts(hash common.Hash, number uint64, txs types.Transactions) types.Receipts {
	var receipts types.Receipts
	logIndex := uint(0)
	for i, tx := range txs {
		r := &types.Receipt{
			Status:  types.ReceiptStatusSuccessful,
			TxHash:  tx.Hash(),
			GasUsed: uint64(21000 + i),
			Logs:    []*types.Log{}, // decoded receipts have an empty slice
		}
		if i == 1 {
			r.ContractAddress = common.BytesToAddress([]byte{0xcc, byte(i)})
		}
		for j := 0; j < i; j++ {
			log := &types.Log{
				Address:     common.BytesToAddress([]byte{byte(j), 0xbb}),
				Topics:      []common.Hash{},
				Data:        []byte{byte(i), byte(j)},
				BlockNumber: number,
				TxHash:      r.TxHash,
				TxIndex:     uint(i),
				BlockHash:   hash,
				Index:       logIndex,
			}
			for k := 0; k <= j && k < 3; k++ {
				log.Topics = append(log.Topics, common.BytesToHash([]byte{byte(i), byte(j), byte(k)}))
			}
			r.Logs = append(r.Logs, log)
			logIndex++
		}
		r.Bloom = types.CreateBloom(types.Receipts{r})
		receipts = append(receipts, r)
	}
	return receipts
}

func genReceiptTestTxs(t *testing.T, n int) types.Transactions {
	var txs types.Transactions
	for i := 0; i < n; i++ {
		tx, err := genTransaction(uint64(i + 1))
		require.NoError(t, err)
		txs = append(txs, tx)
	}
	return txs
}

func bodyOf(txs types.Transactions) func() *types.Body {
	return func() *types.Body { return &types.Body{Transactions: txs} }
}

// TestReceipts_Derived checks that the derived format leaves out the
// blooms and every field derived from the block, and restores the receipts
// exactly.
func TestReceipts_Derived(t *testing.T) {
	var (
		hash   = common.HexToHash("0xb10c")
		number = uint64(7)
		txs    = genReceiptTestTxs(t, 4)
	)
	receipts := genDerivableReceipts(hash, number, txs)
	data := encodeStorageReceipts(receipts)

	frozen := receiptsItemOf(t, hash, number, txs, data)
	require.Equal(t, byte(derivedReceiptsPrefix), frozen[0])
	var stored []*types.ReceiptForStorage
	require.NoError(t, rlp.DecodeBytes(data, &stored))
	slim, err := encodeSlimReceipts(stored)
	require.NoError(t, err)
	t.Logf("4 receipts, 6 logs: key-value %d bytes, slim %d, derived %d", len(data), len(slim), len(frozen))
	assert.Less(t, len(frozen), len(slim))
	assert.Less(t, len(slim), len(data))

	decoded, err := decodeReceipts(frozen, hash, number, bodyOf(txs))
	require.NoError(t, err)
	assert.Equal(t, receipts, decoded)

	// The slim and key-value formats still decode, without the body.
	noBody := func() *types.Body { t.Fatal("body read"); return nil }
	for _, item := range [][]byte{slim, data} {
		decoded, err = decodeReceipts(item, hash, number, noBody)
		require.NoError(t, err)
		assert.Equal(t, receipts, decoded)
	}

	// The derived format needs the body.
	_, err = decodeReceipts(frozen, hash, number, func() *types.Body { return nil })
	assert.Error(t, err)

	// A block without transactions.
	empty := receiptsItemOf(t, hash, number, nil, encodeStorageReceipts(types.Receipts{}))
	assert.Equal(t, byte(derivedReceiptsPrefix), empty[0])
	decoded, err = decodeReceipts(empty, hash, number, bodyOf(nil))
	require.NoError(t, err)
	assert.Empty(t, decoded)
}

// TestReceipts_Fallback checks that receipts whose stored fields differ from
// the derived ones are stored in a format that keeps them.
func TestReceipts_Fallback(t *testing.T) {
	var (
		hash   = common.HexToHash("0xb10c")
		number = uint64(7)
		txs    = genReceiptTestTxs(t, 4)
	)
	for name, tc := range map[string]struct {
		modify func(types.Receipts)
		txs    types.Transactions
		prefix byte
	}{
		"log block hash": {func(r types.Receipts) { r[2].Logs[0].BlockHash = common.Hash{} }, txs, slimReceiptsPrefix},
		"log index":      {func(r types.Receipts) { r[3].Logs[1].Index = 9 }, txs, slimReceiptsPrefix},
		"tx hash":        {func(r types.Receipts) { r[0].TxHash = common.Hash{1} }, txs, slimReceiptsPrefix},
		"tx count":       {func(types.Receipts) {}, txs[:3], slimReceiptsPrefix},
		"bloom":          {func(r types.Receipts) { r[2].Bloom[0] ^= 0x80 }, txs, 0}, // unchanged
	} {
		t.Run(name, func(t *testing.T) {
			receipts := genDerivableReceipts(hash, number, txs)
			tc.modify(receipts)
			data := encodeStorageReceipts(receipts)
			frozen := receiptsItemOf(t, hash, number, tc.txs, data)
			if tc.prefix == 0 {
				assert.Equal(t, data, frozen)
			} else {
				assert.Equal(t, tc.prefix, frozen[0])
			}
			decoded, err := decodeReceipts(frozen, hash, number, bodyOf(tc.txs))
			require.NoError(t, err)
			assert.Equal(t, receipts, decoded)
		})
	}
}

// receiptsItemOf converts a receipts item in the key-value format like
// blockReceiptsItem.
func receiptsItemOf(t *testing.T, hash common.Hash, number uint64, txs types.Transactions, data []byte) []byte {
	var receipts []*types.ReceiptForStorage
	require.NoError(t, rlp.DecodeBytes(data, &receipts))
	return slimReceiptsItem(hash, number, txs, receipts, data)
}

// TestWriteBlockReceipts checks that receipts are written in the derived
// format when they match their block and in the slim format otherwise, and
// that they read back as they were written.
func TestWriteBlockReceipts(t *testing.T) {
	dbm := NewMemoryDBManager().(*databaseManager)
	txs := genReceiptTestTxs(t, 3)
	for number, derivable := range map[uint64]bool{1: true, 2: false} {
		block := types.NewBlockWithHeader(&types.Header{Number: new(big.Int).SetUint64(number)}).WithBody(txs)
		dbm.WriteBlock(block)
		receipts := genDerivableReceipts(block.Hash(), number, txs)
		want := byte(derivedReceiptsPrefix)
		if !derivable {
			receipts[1].Logs[0].BlockNumber = number + 1
			want = slimReceiptsPrefix
		}
		dbm.WriteBlockReceipts(block, receipts)

		item, err := dbm.getDatabase(ReceiptsDB).Get(blockReceiptsKey(number, block.Hash()))
		require.NoError(t, err)
		assert.Equal(t, want, item[0], "receipts %d", number)
		assert.Equal(t, receipts, dbm.ReadReceipts(block.Hash(), number), "receipts %d", number)
	}
}

// TriggerFreeze runs one freeze cycle for tests outside the package.
func TriggerFreeze(t *testing.T, dbm DBManager) {
	triggerFreeze(t, dbm.(*databaseManager))
}
