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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/storage/database/ancient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStateArchiveTestConfig(dir string, enable bool) *DBConfig {
	return &DBConfig{
		Dir: dir, DBType: LevelDB, NumStateTrieShards: 1,
		EnableStateArchive: enable,
	}
}

// useSmallStateArchiveChunks makes chunks and sample intervals small, so that a
// test with a few nodes covers many chunks and sample groups.
func useSmallStateArchiveChunks(t *testing.T) {
	chunkSize, interval := archiveChunkSize, archiveSampleInterval
	archiveChunkSize, archiveSampleInterval = 200, 3
	t.Cleanup(func() { archiveChunkSize, archiveSampleInterval = chunkSize, interval })
}

// archiveTestNode makes a node with the given counter and a hash and content
// derived from the counter and seed.
func archiveTestNode(counter uint64, seed string) ArchiveNode {
	node := []byte(fmt.Sprintf("node-%d-%s", counter, seed))
	var hash common.ExtHash
	copy(hash[:common.HashLength], crypto.Keccak256(node))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	copy(hash[common.HashLength:], buf[1:])
	return ArchiveNode{Hash: hash, Node: node}
}

func archiveTestNodes(from, to uint64, seed string) []ArchiveNode {
	var nodes []ArchiveNode
	for c := from; c < to; c++ {
		nodes = append(nodes, archiveTestNode(c, seed))
	}
	return nodes
}

// assertArchived checks that every node is read back through ReadTrieNode,
// although none of them is in the key-value store.
func assertArchived(t *testing.T, dbm DBManager, nodes []ArchiveNode) {
	t.Helper()
	for _, n := range nodes {
		val, err := dbm.ReadTrieNode(n.Hash)
		require.NoError(t, err, "counter %d", extHashCounter(n.Hash))
		require.Equal(t, n.Node, val, "counter %d", extHashCounter(n.Hash))
	}
}

func countArchiveExceptions(t *testing.T, dbm *databaseManager) int {
	it := dbm.getDatabase(MiscDB).NewIterator(stateArchiveNodePrefix, nil)
	defer it.Release()
	n := 0
	for it.Next() {
		n++
	}
	return n
}

func TestStateArchive_ChunkCodec(t *testing.T) {
	nodes := append([]ArchiveNode{archiveTestNode(0, "root")}, archiveTestNodes(10, 20, "a")...)
	nodes = append(nodes, ArchiveNode{Hash: archiveTestNode(20, "empty").Hash, Node: []byte{}})
	data := encodeArchiveChunk(nodes)

	decoded, err := decodeArchiveChunk(data)
	require.NoError(t, err)
	require.Len(t, decoded, len(nodes))
	for i, n := range nodes {
		assert.Equal(t, n.Hash, decoded[i].Hash)
		assert.Equal(t, n.Node, []byte(decoded[i].Node))

		found, err := findInArchiveChunk(data, n.Hash)
		require.NoError(t, err)
		assert.Equal(t, n.Node, found)
	}

	// Same counter, other hash: not found.
	found, err := findInArchiveChunk(data, archiveTestNode(15, "b").Hash)
	assert.NoError(t, err)
	assert.Nil(t, found)

	// Truncated and padded chunks are invalid.
	_, err = decodeArchiveChunk(data[:len(data)-1])
	assert.Error(t, err)
	_, err = decodeArchiveChunk(append(data, 0))
	assert.Error(t, err)
}

// TestStateArchive_AppendAndRead covers in-order nodes over many chunks and
// sample groups, zero-counter roots, out-of-order nodes and counter collisions.
func TestStateArchive_AppendAndRead(t *testing.T) {
	useSmallStateArchiveChunks(t)
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	require.True(t, dbm.StateArchiveEnabled())

	var all []ArchiveNode
	appendNodes := func(block uint64, nodes ...ArchiveNode) {
		t.Helper()
		require.NoError(t, dbm.AppendStateArchive(block, nodes))
		all = append(all, nodes...)
	}

	// In order, with a root each.
	for b := uint64(0); b < 10; b++ {
		appendNodes(b, append(archiveTestNodes(1000+b*20, 1000+b*20+20, "a"), archiveTestNode(0, fmt.Sprint("root", b)))...)
	}
	// Only the roots go to the exception index.
	assert.Equal(t, 10, countArchiveExceptions(t, dbm))

	// The same root again (a block without state changes) is not stored twice.
	chunks := dbm.stateArchive.chunks
	appendNodes(10, archiveTestNode(0, "root9"))
	assert.Equal(t, chunks, dbm.stateArchive.chunks)

	// Out of order: below the last counter, a counter collision with another
	// hash, and an append of exceptions only.
	appendNodes(11, append(archiveTestNodes(500, 520, "late"), archiveTestNode(1005, "collision"), archiveTestNode(1300, "next"))...)
	appendNodes(12, archiveTestNodes(600, 605, "only-exceptions")...)
	appendNodes(13, archiveTestNodes(1301, 1310, "after")...)
	assert.Equal(t, uint64(1309), dbm.stateArchive.lastCounter)
	assert.Greater(t, len(dbm.stateArchive.samples), 2)
	// The roots, the late nodes, the collision and the exceptions-only append.
	assert.Equal(t, 10+20+1+5, countArchiveExceptions(t, dbm))

	assertArchived(t, dbm, all)

	// Unknown nodes are missing.
	for _, n := range []ArchiveNode{archiveTestNode(0, "unknown"), archiveTestNode(1010, "unknown"), archiveTestNode(5000, "unknown"), archiveTestNode(1, "unknown")} {
		val, err := dbm.ReadTrieNode(n.Hash)
		assert.Error(t, err)
		assert.Nil(t, val)
	}

	// After a reopen, the in-memory index is rebuilt and appends continue.
	dbm.Close()
	dbm = openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	defer dbm.Close()
	assert.Equal(t, uint64(1309), dbm.stateArchive.lastCounter)
	assertArchived(t, dbm, all)
	appendNodes(14, archiveTestNodes(1310, 1320, "reopened")...)
	appendNodes(15, archiveTestNodes(1305, 1306, "reopened-late")...)
	assertArchived(t, dbm, all)

	// A node in the key-value store is read from there.
	n := archiveTestNode(2000, "kv")
	dbm.WriteTrieNode(n.Hash, n.Node)
	assertArchived(t, dbm, []ArchiveNode{n})
}

// TestStateArchive_CrashBeforeKV checks that nodes appended to the archive,
// whose key-value write was lost (a crash after the archive append), and whose
// chunk count was not recorded either, are harmless after a restart.
func TestStateArchive_CrashBeforeKV(t *testing.T) {
	useSmallStateArchiveChunks(t)
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	first := archiveTestNodes(100, 120, "a")
	require.NoError(t, dbm.AppendStateArchive(1, first))
	recorded, _ := dbm.getDatabase(MiscDB).Get(stateArchiveChunksKey)

	lost := archiveTestNodes(120, 140, "lost")
	require.NoError(t, dbm.AppendStateArchive(2, lost))
	// Undo the chunk count, as if the crash came before it was written.
	require.NoError(t, dbm.getDatabase(MiscDB).Put(stateArchiveChunksKey, recorded))
	dbm.Close()

	dbm = openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	defer dbm.Close()
	assertArchived(t, dbm, first)
	// The block is processed again and its nodes get new counters.
	again := archiveTestNodes(200, 220, "again")
	require.NoError(t, dbm.AppendStateArchive(2, again))
	assertArchived(t, dbm, append(first, again...))
}

// TestStateArchive_Misuse checks the cases where the archive would miss nodes.
func TestStateArchive_Misuse(t *testing.T) {
	open := func(dbc *DBConfig) error {
		dbm, err := databaseDBManager(dbc)
		require.NoError(t, err)
		defer dbm.Close()
		if err := dbm.openStateArchive(); err != nil {
			return err
		}
		dbm.closeStateArchive()
		return nil
	}

	// Not enabled at the genesis.
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newStateArchiveTestConfig(dir, false))
	dbm.WriteHeadBlockHash(common.HexToHash("0x01"))
	dbm.Close()
	assert.ErrorContains(t, open(newStateArchiveTestConfig(dir, true)), "before the genesis block")

	// Enabled at the genesis, then left out.
	dir = t.TempDir()
	dbm = openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	require.NoError(t, dbm.AppendStateArchive(0, archiveTestNodes(1, 10, "a")))
	dbm.WriteHeadBlockHash(common.HexToHash("0x01"))
	dbm.Close()
	assert.ErrorContains(t, open(newStateArchiveTestConfig(dir, false)), "enable the state archive")

	// Read-only for offline tools.
	roDBC := newStateArchiveTestConfig(dir, false)
	roDBC.FreezerReadOnly = true
	dbm = openFreezerTestDBM(t, roDBC)
	require.True(t, dbm.StateArchiveEnabled())
	assertArchived(t, dbm, archiveTestNodes(1, 10, "a"))
	assert.ErrorIs(t, dbm.AppendStateArchive(1, archiveTestNodes(10, 11, "b")), errStateArchiveReadOnly)
	dbm.Close()

	// An empty archive at another path, and an older copy of the archive.
	emptyDBC := newStateArchiveTestConfig(dir, true)
	emptyDBC.AncientDir = t.TempDir()
	assert.ErrorContains(t, open(emptyDBC), "older than the database")

	staleRoot := t.TempDir()
	copyFreezerTestDir(t, filepath.Join(dir, DefaultAncientDirName, ancient.StateArchiveName), filepath.Join(staleRoot, ancient.StateArchiveName))
	dbm = openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	require.NoError(t, dbm.AppendStateArchive(1, archiveTestNodes(10, 20, "b")))
	dbm.Close()
	staleDBC := newStateArchiveTestConfig(dir, true)
	staleDBC.AncientDir = staleRoot
	assert.ErrorContains(t, open(staleDBC), "older than the database")

	// The archive directory can be moved, e.g. to another disk.
	movedRoot := filepath.Join(t.TempDir(), "moved")
	require.NoError(t, os.Rename(filepath.Join(dir, DefaultAncientDirName), movedRoot))
	movedDBC := newStateArchiveTestConfig(dir, true)
	movedDBC.AncientDir = movedRoot
	assert.NoError(t, open(movedDBC))
}

func countMiscPrefix(t *testing.T, dbm *databaseManager, prefix []byte) int {
	it := dbm.getDatabase(MiscDB).NewIterator(prefix, nil)
	defer it.Release()
	n := 0
	for it.Next() {
		n++
	}
	return n
}

// deleteMiscPrefix deletes the MiscDB entries with the given prefix, as an OS
// crash may lose them when the trie nodes stay in the key-value store.
func deleteMiscPrefix(t *testing.T, dbm *databaseManager, prefix []byte) {
	db := dbm.getDatabase(MiscDB)
	it := db.NewIterator(prefix, nil)
	var keys [][]byte
	for it.Next() {
		keys = append(keys, append([]byte{}, it.Key()...))
	}
	it.Release()
	for _, key := range keys {
		require.NoError(t, db.Delete(key))
	}
}

// TestStateArchive_LostIndex checks that the exception index, the samples and
// the chunk count are rebuilt on open when MiscDB lost them after the chunks
// were fsynced.
func TestStateArchive_LostIndex(t *testing.T) {
	useSmallStateArchiveChunks(t)
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	var all []ArchiveNode
	for b := uint64(0); b < 6; b++ {
		nodes := append(archiveTestNodes(1000+b*20, 1000+b*20+20, "a"), archiveTestNode(0, fmt.Sprint("root", b)))
		if b == 3 {
			nodes = append(nodes, archiveTestNodes(500, 505, "late")...)
		}
		require.NoError(t, dbm.AppendStateArchive(b, nodes))
		all = append(all, nodes...)
	}
	exceptions := countArchiveExceptions(t, dbm)
	samples := len(dbm.stateArchive.samples)
	require.Equal(t, 6+5, exceptions)
	require.Equal(t, samples, countMiscPrefix(t, dbm, stateArchiveSamplePrefix))

	// Lose everything the appends wrote to MiscDB.
	deleteMiscPrefix(t, dbm, stateArchiveNodePrefix)
	deleteMiscPrefix(t, dbm, stateArchiveSamplePrefix)
	require.NoError(t, dbm.getDatabase(MiscDB).Delete(stateArchiveChunksKey))
	dbm.Close()

	dbm = openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	defer dbm.Close()
	assert.Equal(t, exceptions, countArchiveExceptions(t, dbm))
	assert.Equal(t, samples, countMiscPrefix(t, dbm, stateArchiveSamplePrefix))
	recorded, _ := dbm.getDatabase(MiscDB).Get(stateArchiveChunksKey)
	assert.Equal(t, common.Int64ToByteBigEndian(dbm.stateArchive.chunks), recorded)
	assertArchived(t, dbm, all)
}

// TestStateArchive_RecordedSamples checks that the samples are loaded from
// MiscDB on open, and that wrong recorded samples are ignored.
func TestStateArchive_RecordedSamples(t *testing.T) {
	useSmallStateArchiveChunks(t)
	dir := t.TempDir()
	dbm := openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	nodes := archiveTestNodes(1000, 1100, "a")
	require.NoError(t, dbm.AppendStateArchive(1, nodes))
	samples := slices.Clone(dbm.stateArchive.samples)
	require.Greater(t, len(samples), 2)
	dbm.Close()

	dbm = openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	assert.Equal(t, samples, dbm.stateArchive.samples)
	assert.Equal(t, len(samples), dbm.stateArchive.recordedSamples)
	assertArchived(t, dbm, nodes)

	// A recorded sample that does not match the freezer drops them all.
	last := uint64(len(samples)-1) * archiveSampleInterval
	require.NoError(t, dbm.getDatabase(MiscDB).Put(stateArchiveSampleKey(last), common.Int64ToByteBigEndian(1)))
	dbm.Close()
	dbm = openFreezerTestDBM(t, newStateArchiveTestConfig(dir, true))
	defer dbm.Close()
	assert.Equal(t, samples, dbm.stateArchive.samples)
	assertArchived(t, dbm, nodes)
}

// TestStateArchive_Migration checks that db-migration copies the archive and
// points the destination at the copy.
func TestStateArchive_Migration(t *testing.T) {
	useSmallStateArchiveChunks(t)
	srcDir := t.TempDir()
	dbm := openFreezerTestDBM(t, newStateArchiveTestConfig(srcDir, true))
	nodes := append(archiveTestNodes(1000, 1050, "a"), archiveTestNode(0, "root"))
	require.NoError(t, dbm.AppendStateArchive(1, nodes))
	dbm.WriteHeadBlockHash(common.HexToHash("0x01"))
	dbm.Close()

	srcDBC := newStateArchiveTestConfig(srcDir, false)
	srcDBC.FreezerReadOnly = true
	src := openFreezerTestDBM(t, srcDBC)
	require.True(t, src.StateArchiveEnabled())

	single := openFreezerTestDBM(t, &DBConfig{Dir: t.TempDir(), DBType: LevelDB, SingleDB: true, NumStateTrieShards: 1})
	assert.ErrorContains(t, src.StartDBMigration(single), "does not support the freezer")
	single.Close()

	dstDir := t.TempDir()
	dst := openFreezerTestDBM(t, newStateArchiveTestConfig(dstDir, false))
	require.NoError(t, src.StartDBMigration(dst))
	src.Close()
	dst.Close()

	// The source archive is no longer needed.
	require.NoError(t, os.RemoveAll(filepath.Join(srcDir, DefaultAncientDirName)))
	dst = openFreezerTestDBM(t, newStateArchiveTestConfig(dstDir, true))
	defer dst.Close()
	require.True(t, dst.StateArchiveEnabled())
	assert.Equal(t, filepath.Join(dstDir, DefaultAncientDirName, ancient.StateArchiveName), dst.stateArchive.dir())
	assertArchived(t, dst, nodes)
}
