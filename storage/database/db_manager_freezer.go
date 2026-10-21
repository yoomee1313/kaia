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
//
// The freeze loop is derived from go-ethereum core/rawdb/chain_freezer.go (2026/10/21).

package database

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database/ancient"
	"github.com/rcrowley/go-metrics"
)

const (
	// DefaultFreezerThreshold is the number of recent blocks kept in the
	// key-value store when the freezer is enabled. Kaia blocks are final as
	// soon as they are committed, so the threshold does not guard against
	// reorgs like go-ethereum's 90,000 blocks. It bounds how far SetHead can
	// rewind (rewinding into the freezer is refused) and keeps the blocks that
	// RPC reads most often on the fast disk. 172,800 blocks is two days of
	// 1-second blocks, enough to cover a state-recovery rewind and to give an
	// operator time to react before a block moves out of the key-value store.
	DefaultFreezerThreshold = 172800

	// freezerRecheckInterval is the frequency to check the key-value database
	// for chain progression that might permit new blocks to be frozen.
	freezerRecheckInterval = time.Minute

	// freezerBatchLimit is the maximum number of blocks to freeze in one batch
	// before doing an fsync and deleting them from the key-value store.
	freezerBatchLimit = 30000

	// freezerCompactInterval is the number of frozen blocks after which their
	// key ranges are compacted. A range compaction flushes the memtable, so it
	// is not run after every small round.
	freezerCompactInterval = freezerBatchLimit

	// DefaultAncientDirName is the freezer root under chaindata used when no
	// ancient directory is configured.
	DefaultAncientDirName = "ancient"
)

var (
	frozenBlocksGauge  = metrics.NewRegisteredGauge(dbMetricPrefix+"ancient/frozen", nil)
	freezerLagGauge    = metrics.NewRegisteredGauge(dbMetricPrefix+"ancient/lag", nil)
	freezeTimer        = metrics.NewRegisteredTimer(dbMetricPrefix+"ancient/freeze/time", nil)
	freezeCompactTimer = metrics.NewRegisteredTimer(dbMetricPrefix+"ancient/compact/time", nil)

	freezerReadErrorCounter = metrics.NewRegisteredCounter(dbMetricPrefix+"ancient/readerror", nil)
)

// openFreezer opens the block freezer if it is enabled and supported by the
// configured backend. It fails on an I/O or consistency error, and if blocks
// were frozen before but the freezer is now disabled or empty. With
// FreezerReadOnly, an existing freezer is opened for reads only, and no freeze
// loop is started.
func (dbm *databaseManager) openFreezer() error {
	dbc := dbm.config
	miscDB := dbm.getDatabase(MiscDB)
	marker, _ := miscDB.Get(ancientDirKey)

	readonly := !dbc.EnableFreezer && dbc.FreezerReadOnly && len(marker) != 0
	enabled := dbc.EnableFreezer || readonly
	switch {
	case !enabled:
	case dbc.DBType == MemoryDB || dbc.DBType == DynamoDB:
		logger.Warn("Freezer is not supported for the database type, disabling it", "DBType", dbc.DBType)
		enabled = false
	case dbc.RocksDBConfig != nil && dbc.RocksDBConfig.Secondary:
		logger.Warn("Freezer is not supported for a secondary RocksDB yet, disabling it")
		enabled = false
	}
	if !enabled {
		if len(marker) != 0 {
			return fmt.Errorf("old blocks were moved to the freezer at %s, enable the freezer to read them", marker)
		}
		return nil
	}

	var dir string
	if readonly && dbc.AncientDir == "" {
		dir = string(marker)
	} else {
		root := dbc.AncientDir
		if root == "" {
			root = DefaultAncientDirName
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(dbc.Dir, root)
		}
		dir = filepath.Join(root, ancient.ChainFreezerName)
	}
	f, err := ancient.NewChainFreezer(dir, dbMetricPrefix, readonly)
	if err != nil {
		return err
	}
	frozen, _ := f.Ancients()
	if frozen == 0 && len(marker) != 0 {
		f.Close()
		return fmt.Errorf("freezer at %s is empty, but old blocks were moved to the freezer at %s", dir, marker)
	}
	dbm.freezer = f
	if err := dbm.checkFreezer(readonly); err != nil {
		f.Close()
		dbm.freezer = nil
		return err
	}
	frozenBlocksGauge.Update(int64(frozen))
	if readonly {
		logger.Info("Opened block freezer read-only", "dir", dir, "frozen", frozen)
		return nil
	}
	if frozen > 0 && string(marker) != dir {
		if err := miscDB.Put(ancientDirKey, []byte(dir)); err != nil {
			return err
		}
	}
	logger.Info("Opened block freezer", "dir", dir, "frozen", frozen, "threshold", dbc.FreezerThreshold)

	dbm.freezerQuit = make(chan struct{})
	dbm.freezerTrigger = make(chan chan struct{})
	dbm.freezerWg.Add(1)
	go dbm.freeze()
	return nil
}

// checkFreezer verifies that the freezer belongs to the chain in the key-value
// store and, unless readonly, finishes the deletion of a batch that was frozen
// right before an unclean shutdown.
func (dbm *databaseManager) checkFreezer(readonly bool) error {
	frozen, _ := dbm.freezer.Ancients()
	if frozen == 0 {
		return nil
	}
	hdb := dbm.getDatabase(headerDB)

	// The genesis is kept in the key-value store, so it can be compared.
	if kvGenesis, _ := hdb.Get(headerHashKey(0)); len(kvGenesis) != 0 {
		if frozenGenesis := dbm.readAncientHash(0); !bytes.Equal(kvGenesis, frozenGenesis) {
			return fmt.Errorf("freezer genesis mismatch: kv %x, freezer %x", kvGenesis, frozenGenesis)
		}
	}

	// The key-value store must continue where the freezer ends. Otherwise the
	// freezer is an older copy and the blocks in between are gone.
	if head := dbm.ReadHeaderNumber(dbm.ReadHeadBlockHash()); head != nil && *head >= frozen {
		data, _ := hdb.Get(headerHashKey(frozen))
		if len(data) == 0 {
			return fmt.Errorf("freezer at %s ends at block %d, but block %d is not in the key-value store; the freezer is older than the chain",
				dbm.freezerDir(), frozen-1, frozen)
		}
		header := new(types.Header)
		if err := rlp.DecodeBytes(dbm.ReadHeaderRLP(common.BytesToHash(data), frozen), header); err != nil {
			return fmt.Errorf("invalid header of block %d after the freezer: %v", frozen, err)
		}
		if frozenHash := dbm.readAncientHash(frozen - 1); !bytes.Equal(header.ParentHash[:], frozenHash) {
			return fmt.Errorf("block %d does not follow the freezer head: parent %x, freezer %x", frozen, header.ParentHash, frozenHash)
		}
	}

	// Blocks are fsynced into the freezer before they are deleted from the
	// key-value store. A crash in between leaves a tail of frozen blocks in
	// both stores; one batch at most, since the next batch only starts after
	// the deletion.
	first := frozen
	for first > 1 {
		data, _ := hdb.Get(headerHashKey(first - 1))
		if len(data) == 0 {
			break
		}
		if frozenHash := dbm.readAncientHash(first - 1); !bytes.Equal(data, frozenHash) {
			return fmt.Errorf("canonical hash mismatch at frozen block %d: kv %x, freezer %x", first-1, data, frozenHash)
		}
		first--
	}
	if first == frozen || readonly {
		return nil
	}
	hashes := make([]common.Hash, 0, frozen-first)
	for number := first; number < frozen; number++ {
		hashes = append(hashes, common.BytesToHash(dbm.readAncientHash(number)))
	}
	logger.Info("Deleting leftover frozen blocks from key-value store", "from", first, "to", frozen-1)
	return dbm.deleteFrozenBlocks(first, hashes)
}

// closeFreezer stops the freeze loop and closes the freezer.
func (dbm *databaseManager) closeFreezer() {
	if dbm.freezer == nil {
		return
	}
	if dbm.freezerQuit != nil {
		close(dbm.freezerQuit)
		dbm.freezerWg.Wait()
	}
	if err := dbm.freezer.Close(); err != nil {
		logger.Error("Failed to close block freezer", "err", err)
	}
}

// freezerDir returns the directory of the open freezer.
func (dbm *databaseManager) freezerDir() string {
	dir, _ := dbm.freezer.AncientDatadir()
	return dir
}

// FrozenBlocks returns the number of blocks stored in the freezer, i.e. blocks
// [0, FrozenBlocks()) are frozen. It returns 0 if the freezer is disabled.
func (dbm *databaseManager) FrozenBlocks() uint64 {
	if dbm.freezer == nil {
		return 0
	}
	frozen, _ := dbm.freezer.Ancients()
	return frozen
}

// PauseFreezer stops the freeze loop from moving blocks out of the key-value
// store until resume is called, and returns the number of frozen blocks. A
// rewind holds it so that the blocks it deletes are not frozen meanwhile.
func (dbm *databaseManager) PauseFreezer() (frozen uint64, resume func()) {
	dbm.freezeLock.Lock()
	return dbm.FrozenBlocks(), dbm.freezeLock.Unlock
}

// readAncientHash returns the frozen canonical hash of the given block number,
// or nil if the block is not frozen.
func (dbm *databaseManager) readAncientHash(number uint64) []byte {
	if dbm.freezer == nil || number >= dbm.FrozenBlocks() {
		return nil
	}
	return dbm.retrieveAncient(ancient.ChainFreezerHashTable, number)
}

// retrieveAncient returns a frozen item. The block must be frozen, so an error
// means the freezer cannot be read, e.g. because its disk failed. It is logged
// and counted, and the item is reported as missing.
func (dbm *databaseManager) retrieveAncient(kind string, number uint64) []byte {
	data, err := dbm.freezer.Ancient(kind, number)
	if err != nil {
		freezerReadErrorCounter.Inc(1)
		logger.Error("Failed to read from the block freezer", "kind", kind, "number", number, "err", err)
		return nil
	}
	return data
}

// isFrozenCanon reports whether the block is frozen. Only canonical blocks are
// frozen, so a block whose hash differs from the frozen canonical hash at its
// number is not in the freezer.
func (dbm *databaseManager) isFrozenCanon(hash common.Hash, number uint64) bool {
	data := dbm.readAncientHash(number)
	return len(data) != 0 && bytes.Equal(data, hash[:])
}

// readAncient returns a frozen item of the given block, or nil if the block is
// not frozen.
func (dbm *databaseManager) readAncient(kind string, hash common.Hash, number uint64) []byte {
	if !dbm.isFrozenCanon(hash, number) {
		return nil
	}
	return dbm.retrieveAncient(kind, number)
}

// freeze is a background thread that periodically moves blocks older than the
// freezer threshold from the key-value store into the freezer.
func (dbm *databaseManager) freeze() {
	defer dbm.freezerWg.Done()

	var (
		backoff     bool
		triggered   chan struct{} // Used in tests
		compactFrom = dbm.FrozenBlocks()
	)
	timer := time.NewTimer(freezerRecheckInterval)
	defer timer.Stop()

	for {
		select {
		case <-dbm.freezerQuit:
			logger.Info("Block freezer shutting down")
			return
		default:
		}
		if backoff {
			// If we were doing a manual trigger, notify it
			if triggered != nil {
				triggered <- struct{}{}
				triggered = nil
			}
			select {
			case <-timer.C:
				backoff = false
				timer.Reset(freezerRecheckInterval)
			case triggered = <-dbm.freezerTrigger:
				backoff = false
			case <-dbm.freezerQuit:
				logger.Info("Block freezer shutting down")
				return
			}
		}
		first, n, err := dbm.freezeOnce()
		if err != nil {
			logger.Error("Error in block freeze operation", "err", err)
		}
		if end := first + n; n > 0 && end-compactFrom >= freezerCompactInterval {
			dbm.compactFrozen(compactFrom, end-1)
			compactFrom = end
		}
		// Avoid database thrashing with tiny writes
		if n < freezerBatchLimit {
			backoff = true
		}
	}
}

// freezeOnce freezes the next batch of blocks below the threshold and returns
// the first block number and the number of blocks it froze.
func (dbm *databaseManager) freezeOnce() (uint64, uint64, error) {
	dbm.freezeLock.Lock()
	defer dbm.freezeLock.Unlock()

	threshold, ok := dbm.freezeThreshold()
	if !ok {
		freezerLagGauge.Update(0)
		return 0, 0, nil
	}
	// Short circuit if the blocks below threshold are already frozen.
	frozen := dbm.FrozenBlocks()
	if frozen > threshold {
		freezerLagGauge.Update(0)
		return 0, 0, nil
	}
	first, last := frozen, threshold
	if last-first+1 > freezerBatchLimit {
		last = first + freezerBatchLimit - 1
	}
	start := time.Now()
	if err := dbm.freezeRange(first, last); err != nil {
		freezerLagGauge.Update(int64(threshold - frozen + 1))
		return 0, 0, err
	}
	elapsed := time.Since(start)
	freezeTimer.Update(elapsed)
	frozenBlocksGauge.Update(int64(last + 1))
	freezerLagGauge.Update(int64(threshold - last))
	logger.Info("Froze chain segment", "from", first, "to", last, "elapsed", common.PrettyDuration(elapsed))
	return first, last - first + 1, nil
}

// compactFrozen compacts the key ranges of the frozen blocks [first, last] in
// the header, body and receipts databases. Deleted keys only free disk space
// once they are compacted, and old block data is rarely touched by the
// background compaction. Blocks frozen since the last compaction before a
// restart are left to the background compaction.
func (dbm *databaseManager) compactFrozen(first, last uint64) {
	start := time.Now()
	for _, r := range []struct {
		entry  DBEntryType
		prefix []byte
	}{
		{headerDB, headerPrefix},
		{BodyDB, blockBodyPrefix},
		{ReceiptsDB, blockReceiptsPrefix},
	} {
		from := append(append([]byte{}, r.prefix...), common.Int64ToByteBigEndian(first)...)
		to := append(append([]byte{}, r.prefix...), common.Int64ToByteBigEndian(last+1)...)
		if err := dbm.getDatabase(r.entry).Compact(from, to); err != nil {
			logger.Warn("Failed to compact frozen blocks", "db", r.entry, "from", first, "to", last, "err", err)
		}
	}
	freezeCompactTimer.Update(time.Since(start))
}

// freezeThreshold returns the highest block number that may be frozen.
func (dbm *databaseManager) freezeThreshold() (uint64, bool) {
	hash := dbm.ReadHeadBlockHash()
	if common.EmptyHash(hash) {
		return 0, false
	}
	head := dbm.ReadHeaderNumber(hash)
	if head == nil || *head <= dbm.config.FreezerThreshold {
		return 0, false
	}
	return *head - dbm.config.FreezerThreshold, true
}

// freezeRange moves the canonical blocks [first, last] from the key-value
// store into the freezer, then deletes them and any side chain block at the
// same heights from the key-value store. The genesis block is kept in the
// key-value store.
func (dbm *databaseManager) freezeRange(first, last uint64) error {
	var (
		hdb    = dbm.getDatabase(headerDB)
		bdb    = dbm.getDatabase(BodyDB)
		rdb    = dbm.getDatabase(ReceiptsDB)
		hashes = make([]common.Hash, 0, last-first+1)
	)
	_, err := dbm.freezer.ModifyAncients(func(op ancient.AncientWriteOp) error {
		for number := first; number <= last; number++ {
			hashData, _ := hdb.Get(headerHashKey(number))
			if len(hashData) == 0 {
				return fmt.Errorf("canonical hash missing, can't freeze block %d", number)
			}
			hash := common.BytesToHash(hashData)
			header, _ := hdb.Get(headerKey(number, hash))
			if len(header) == 0 {
				return fmt.Errorf("block header missing, can't freeze block %d", number)
			}
			body, _ := bdb.Get(blockBodyKey(number, hash))
			if len(body) == 0 {
				return fmt.Errorf("block body missing, can't freeze block %d", number)
			}
			receipts, _ := rdb.Get(blockReceiptsKey(number, hash))
			if len(receipts) == 0 {
				return fmt.Errorf("block receipts missing, can't freeze block %d", number)
			}
			if err := op.AppendRaw(ancient.ChainFreezerHashTable, number, hash[:]); err != nil {
				return fmt.Errorf("can't write hash to freezer: %v", err)
			}
			if err := op.AppendRaw(ancient.ChainFreezerHeaderTable, number, header); err != nil {
				return fmt.Errorf("can't write header to freezer: %v", err)
			}
			if err := op.AppendRaw(ancient.ChainFreezerBodiesTable, number, body); err != nil {
				return fmt.Errorf("can't write body to freezer: %v", err)
			}
			if err := op.AppendRaw(ancient.ChainFreezerReceiptTable, number, receipts); err != nil {
				return fmt.Errorf("can't write receipts to freezer: %v", err)
			}
			hashes = append(hashes, hash)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Batch of blocks have been frozen, flush them before wiping from key-value store
	if err := dbm.freezer.SyncAncient(); err != nil {
		logger.Crit("Failed to flush frozen tables", "err", err)
	}
	// Remember the freezer before any block is gone from the key-value store.
	if err := dbm.getDatabase(MiscDB).Put(ancientDirKey, []byte(dbm.freezerDir())); err != nil {
		logger.Crit("Failed to store the freezer directory", "err", err)
	}
	if err := dbm.deleteFrozenBlocks(first, hashes); err != nil {
		logger.Crit("Failed to delete frozen blocks", "err", err)
	}
	return nil
}

// deleteFrozenBlocks deletes the frozen canonical blocks starting at first,
// and the side chain blocks at the same heights, from the key-value store. The
// hash to number mapping and the total difficulty of canonical blocks are kept.
func (dbm *databaseManager) deleteFrozenBlocks(first uint64, hashes []common.Hash) error {
	if len(hashes) == 0 {
		return nil
	}
	last := first + uint64(len(hashes)) - 1

	// Side chain blocks are collected before the canonical headers are gone.
	// Kaia has instant finality, so there are normally none.
	var sideHashes []common.Hash
	var sideNumbers []uint64
	it := dbm.getDatabase(headerDB).NewIterator(headerPrefix, common.Int64ToByteBigEndian(first))
	for it.Next() {
		key := it.Key()
		if len(key) != len(headerPrefix)+8+common.HashLength {
			continue // canonical hash or total difficulty key
		}
		number := binary.BigEndian.Uint64(key[len(headerPrefix):])
		if number > last {
			break
		}
		hash := common.BytesToHash(key[len(headerPrefix)+8:])
		if hash != hashes[number-first] {
			sideHashes = append(sideHashes, hash)
			sideNumbers = append(sideNumbers, number)
		}
	}
	it.Release()
	if err := it.Error(); err != nil {
		return err
	}

	headerBatch := dbm.NewBatch(headerDB)
	bodyBatch := dbm.NewBatch(BodyDB)
	receiptsBatch := dbm.NewBatch(ReceiptsDB)
	defer headerBatch.Release()
	defer bodyBatch.Release()
	defer receiptsBatch.Release()

	for i, hash := range hashes {
		number := first + uint64(i)
		if number == 0 {
			continue // Always keep the genesis block in the key-value store
		}
		if err := headerBatch.Delete(headerKey(number, hash)); err != nil {
			return err
		}
		if err := headerBatch.Delete(headerHashKey(number)); err != nil {
			return err
		}
		if err := bodyBatch.Delete(blockBodyKey(number, hash)); err != nil {
			return err
		}
		if err := receiptsBatch.Delete(blockReceiptsKey(number, hash)); err != nil {
			return err
		}
	}
	if _, err := WriteBatches(headerBatch, bodyBatch, receiptsBatch); err != nil {
		return err
	}

	for i, hash := range sideHashes {
		logger.Debug("Deleting side chain block below the freezer head", "number", sideNumbers[i], "hash", hash)
		dbm.DeleteBlock(hash, sideNumbers[i])
	}
	return nil
}

// checkFreezerMigration fails if dbm has frozen blocks and the destination of a
// DB migration cannot hold them.
func (dbm *databaseManager) checkFreezerMigration(dstdbm DBManager) error {
	if dbm.FrozenBlocks() == 0 {
		return nil
	}
	dst := dstdbm.GetDBConfig()
	if dst.SingleDB || dst.DBType == MemoryDB || dst.DBType == DynamoDB {
		return fmt.Errorf("the source has %d frozen blocks, but the destination (DBType %s, single %v) does not support the freezer",
			dbm.FrozenBlocks(), dst.DBType, dst.SingleDB)
	}
	return nil
}

// migrateFreezer copies the freezer files of dbm into the default freezer
// directory of the migrated database, and records that directory in its
// MiscDB. The copied MiscDB still names the source freezer until then.
func (dbm *databaseManager) migrateFreezer(dstdbm DBManager, quit chan struct{}) error {
	if dbm.FrozenBlocks() == 0 {
		return nil
	}
	src := dbm.freezerDir()
	dst := filepath.Join(dstdbm.GetDBConfig().Dir, DefaultAncientDirName, ancient.ChainFreezerName)
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		return fmt.Errorf("freezer directory %s of the destination is not empty", dst)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	start := time.Now()
	for _, e := range entries {
		if e.IsDir() || e.Name() == "FLOCK" {
			continue
		}
		select {
		case <-quit:
			return ErrDBMigrationInterrupted
		default:
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return fmt.Errorf("failed to copy freezer file %s: %w", e.Name(), err)
		}
	}
	if err := dstdbm.getDatabase(MiscDB).Put(ancientDirKey, []byte(dst)); err != nil {
		return err
	}
	logger.Info("Finish freezer migration", "src", src, "dst", dst, "frozen", dbm.FrozenBlocks(), "elapsed", time.Since(start))
	return nil
}

// copyFile copies the file at src to dst and syncs it.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
