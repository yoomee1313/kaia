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
	"path/filepath"
	"time"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database/ancient"
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

	// DefaultAncientDirName is the freezer root under chaindata used when no
	// ancient directory is configured.
	DefaultAncientDirName = "ancient"
)

// openFreezer opens the block freezer if it is enabled and supported by the
// configured backend. It fails on an I/O or consistency error, and if blocks
// were frozen before but the freezer is now disabled or empty.
func (dbm *databaseManager) openFreezer() error {
	dbc := dbm.config
	enabled := dbc.EnableFreezer
	switch {
	case !enabled:
	case dbc.DBType == MemoryDB || dbc.DBType == DynamoDB:
		logger.Warn("Freezer is not supported for the database type, disabling it", "DBType", dbc.DBType)
		enabled = false
	case dbc.RocksDBConfig != nil && dbc.RocksDBConfig.Secondary:
		logger.Warn("Freezer is not supported for a secondary RocksDB yet, disabling it")
		enabled = false
	}
	miscDB := dbm.getDatabase(MiscDB)
	marker, _ := miscDB.Get(ancientDirKey)
	if !enabled {
		if len(marker) != 0 {
			return fmt.Errorf("old blocks were moved to the freezer at %s, enable the freezer to read them", marker)
		}
		return nil
	}

	root := dbc.AncientDir
	if root == "" {
		root = DefaultAncientDirName
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(dbc.Dir, root)
	}
	dir := filepath.Join(root, ancient.ChainFreezerName)
	f, err := ancient.NewChainFreezer(dir, dbMetricPrefix, false)
	if err != nil {
		return err
	}
	frozen, _ := f.Ancients()
	if frozen == 0 && len(marker) != 0 {
		f.Close()
		return fmt.Errorf("freezer at %s is empty, but old blocks were moved to the freezer at %s", dir, marker)
	}
	dbm.freezer = f
	if err := dbm.checkFreezer(); err != nil {
		f.Close()
		dbm.freezer = nil
		return err
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
// store and finishes the deletion of a batch that was frozen right before an
// unclean shutdown.
func (dbm *databaseManager) checkFreezer() error {
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
	if first == frozen {
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
	close(dbm.freezerQuit)
	dbm.freezerWg.Wait()
	if err := dbm.freezer.Close(); err != nil {
		logger.Error("Failed to close block freezer", "err", err)
	}
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

// readAncientHash returns the frozen canonical hash of the given block number,
// or nil if the block is not frozen.
func (dbm *databaseManager) readAncientHash(number uint64) []byte {
	if dbm.freezer == nil {
		return nil
	}
	data, _ := dbm.freezer.Ancient(ancient.ChainFreezerHashTable, number)
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
	data, _ := dbm.freezer.Ancient(kind, number)
	return data
}

// freeze is a background thread that periodically moves blocks older than the
// freezer threshold from the key-value store into the freezer.
func (dbm *databaseManager) freeze() {
	defer dbm.freezerWg.Done()

	var (
		backoff   bool
		triggered chan struct{} // Used in tests
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
		threshold, ok := dbm.freezeThreshold()
		if !ok {
			backoff = true
			continue
		}
		// Short circuit if the blocks below threshold are already frozen.
		frozen := dbm.FrozenBlocks()
		if frozen > threshold {
			backoff = true
			continue
		}
		first, last := frozen, threshold
		if last-first+1 > freezerBatchLimit {
			last = first + freezerBatchLimit - 1
		}
		start := time.Now()
		if err := dbm.freezeRange(first, last); err != nil {
			logger.Error("Error in block freeze operation", "err", err)
			backoff = true
			continue
		}
		logger.Info("Froze chain segment", "from", first, "to", last, "elapsed", common.PrettyDuration(time.Since(start)))

		// Avoid database thrashing with tiny writes
		if last-first+1 < freezerBatchLimit {
			backoff = true
		}
	}
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
	dir, _ := dbm.freezer.AncientDatadir()
	if err := dbm.getDatabase(MiscDB).Put(ancientDirKey, []byte(dir)); err != nil {
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
