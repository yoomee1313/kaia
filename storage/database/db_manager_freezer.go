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
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database/ancient"
	"github.com/rcrowley/go-metrics"
)

const (
	// DefaultFreezerThreshold is the number of recent blocks kept in the
	// key-value store. Kaia blocks are final, so it does not guard against
	// reorgs; it bounds how far SetHead can rewind and keeps the blocks RPC
	// reads most on the fast disk. It is two days of 1-second blocks.
	DefaultFreezerThreshold = 172800

	freezerRecheckInterval = time.Minute

	// freezerBatchLimit is the number of blocks frozen before an fsync and
	// their deletion from the key-value store.
	freezerBatchLimit = 30000

	// freezerCompactChunk is the number of frozen blocks whose key ranges one
	// compaction covers. A compaction is not stopped on shutdown, so it bounds
	// how long a shutdown waits for one.
	freezerCompactChunk = 100000

	// freezeReadWorkers is the number of goroutines that read the blocks of a
	// batch to freeze, at most freezeReadAhead blocks ahead of the writes.
	freezeReadWorkers = 8
	freezeReadAhead   = 1024

	DefaultAncientDirName = "ancient"
)

// freezerCompactInterval is the number of frozen blocks after which their key
// ranges are compacted. Tests lower it.
var freezerCompactInterval uint64 = freezerBatchLimit

// frozenCompactedKey holds the first frozen block whose key ranges are not
// compacted yet, so that a restart goes on compacting from there.
var frozenCompactedKey = []byte("FrozenCompacted")

// slimTxLookupsKey holds the tx hash from which the tx lookup entries are left
// to slim, or slimTxLookupsDone once they all are.
var (
	slimTxLookupsKey  = []byte("SlimTxLookups")
	slimTxLookupsDone = []byte{0}
)

// slimTxLookupsBatch is the number of tx lookup entries one call of
// slimTxLookups reads. Tests lower it.
var slimTxLookupsBatch = 1_000_000

var (
	frozenBlocksGauge  = metrics.NewRegisteredGauge(dbMetricPrefix+"ancient/frozen", nil)
	freezerLagGauge    = metrics.NewRegisteredGauge(dbMetricPrefix+"ancient/lag", nil)
	freezeTimer        = metrics.NewRegisteredTimer(dbMetricPrefix+"ancient/freeze/time", nil)
	freezeCompactTimer = metrics.NewRegisteredTimer(dbMetricPrefix+"ancient/compact/time", nil)

	freezerReadErrorCounter = metrics.NewRegisteredCounter(dbMetricPrefix+"ancient/readerror", nil)
)

func crit(msg string, err error) {
	if err != nil {
		logger.Crit(msg, "err", err)
	}
}

// checkFreezerSupport reports whether the freezer is to be opened. A setup
// without freezer support runs without it.
func checkFreezerSupport(dbc *DBConfig, readonly bool) bool {
	if !dbc.EnableFreezer && !readonly {
		return false
	}
	var reason string
	switch {
	case dbc.SingleDB:
		reason = "a single database"
	case dbc.DBType == MemoryDB || dbc.DBType == DynamoDB:
		reason = "database type " + string(dbc.DBType)
	case dbc.RocksDBConfig != nil && dbc.RocksDBConfig.Secondary:
		reason = "a secondary RocksDB"
	default:
		return true
	}
	logger.Warn("Freezer is not supported with " + reason + " yet, disabling it")
	return false
}

// openFreezer opens the block freezer and starts the freeze loop. It fails if
// blocks were frozen before but the freezer is now disabled or empty. With
// FreezerReadOnly, an existing freezer is opened for reads only.
func (dbm *databaseManager) openFreezer() error {
	dbc := dbm.config
	miscDB := dbm.getDatabase(MiscDB)
	marker, _ := miscDB.Get(ancientDirKey)

	readonly := !dbc.EnableFreezer && dbc.FreezerReadOnly && len(marker) != 0
	if !checkFreezerSupport(dbc, readonly) {
		if len(marker) != 0 {
			return fmt.Errorf("old blocks were moved to the freezer at %s, but the freezer is disabled", marker)
		}
		return nil
	}

	dir := string(marker)
	if !readonly || dbc.AncientDir != "" {
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
// store and, unless readonly, repairs what an unclean shutdown left behind.
func (dbm *databaseManager) checkFreezer(readonly bool) error {
	frozen, _ := dbm.freezer.Ancients()
	if frozen == 0 {
		return nil
	}
	hdb := dbm.getDatabase(headerDB)

	if kvGenesis, _ := hdb.Get(headerHashKey(0)); len(kvGenesis) != 0 {
		if frozenGenesis := dbm.readAncientHash(0); !bytes.Equal(kvGenesis, frozenGenesis) {
			return fmt.Errorf("freezer genesis mismatch: kv %x, freezer %x", kvGenesis, frozenGenesis)
		}
	}

	// A rewind moves the head markers before it truncates the freezer.
	if hh := dbm.ReadHeaderNumber(dbm.ReadHeadHeaderHash()); !readonly && hh != nil && *hh+1 < frozen {
		if err := dbm.TruncateFrozen(*hh + 1); err != nil {
			return err
		}
		frozen = *hh + 1
	}

	// Otherwise the freezer is an older copy and the blocks in between are gone.
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

	// A crash between the fsync of a batch and its deletion leaves at most one
	// batch of frozen blocks in the key-value store too.
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

func (dbm *databaseManager) closeFreezer() {
	if dbm.freezer == nil {
		return
	}
	dbm.freezerCloseOnce.Do(func() {
		if dbm.freezerQuit != nil {
			close(dbm.freezerQuit)
			dbm.freezerWg.Wait()
		}
		if err := dbm.freezer.Close(); err != nil {
			logger.Error("Failed to close block freezer", "err", err)
		}
		dbm.closePruningMarkFreezer()
	})
}

func (dbm *databaseManager) freezerClosing() bool {
	select {
	case <-dbm.freezerQuit:
		return true
	default:
		return false
	}
}

func (dbm *databaseManager) freezerDir() string {
	dir, _ := dbm.freezer.AncientDatadir()
	return dir
}

// FrozenBlocks returns the number of frozen blocks, i.e. blocks
// [0, FrozenBlocks()) are frozen.
func (dbm *databaseManager) FrozenBlocks() uint64 {
	if dbm.freezer == nil {
		return 0
	}
	frozen, _ := dbm.freezer.Ancients()
	return frozen
}

// PauseFreezer stops the freeze loop until resume is called, so that a rewind
// does not race it.
func (dbm *databaseManager) PauseFreezer() (frozen uint64, resume func()) {
	dbm.freezeLock.Lock()
	return dbm.FrozenBlocks(), dbm.freezeLock.Unlock
}

// TruncateFrozen removes the frozen blocks from items on. The caller must hold
// PauseFreezer.
func (dbm *databaseManager) TruncateFrozen(items uint64) error {
	frozen := dbm.FrozenBlocks()
	if dbm.freezer == nil || items >= frozen {
		return nil
	}
	if _, err := dbm.freezer.TruncateHead(items); err != nil {
		return err
	}
	if err := dbm.freezer.SyncAncient(); err != nil {
		return err
	}
	dbm.unfreezeBloomBits(items)
	frozenBlocksGauge.Update(int64(items))
	logger.Warn("Truncated block freezer", "from", frozen, "to", items)
	return nil
}

// Unfreeze moves the frozen blocks back into the key-value store and empties
// the freezer, as if no block had been frozen, for a version without the
// freezer or to freeze them again. The derived headers are restored, and the
// receipts are copied as they are, as the key-value store reads them too. It
// stops freezing for good, so the database is to be closed after it. After a
// crash it can run again: the blocks are written again.
func (dbm *databaseManager) Unfreeze(progress func(next, frozen uint64)) error {
	if dbm.freezer == nil || dbm.config.FreezerReadOnly {
		return errors.New("the block freezer is not open for writing")
	}
	dbm.freezeLock.Lock()
	defer dbm.freezeLock.Unlock()
	dbm.unfrozen.Store(true)

	frozen := dbm.FrozenBlocks()
	balTail, err := dbm.freezer.Tail(ancient.ChainFreezerBALGroup)
	if err != nil {
		return err
	}
	read := func(kind string, first, n uint64) ([][]byte, error) {
		items, err := dbm.freezer.AncientRange(kind, first, n, 0)
		if err == nil && uint64(len(items)) != n {
			err = fmt.Errorf("%d items from %d, want %d", len(items), first, n)
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", kind, err)
		}
		return items, nil
	}
	hb, bb, rb, ab := dbm.NewBatch(headerDB), dbm.NewBatch(BodyDB), dbm.NewBatch(ReceiptsDB), dbm.NewBatch(BALDB)
	defer func() {
		for _, b := range []Batch{hb, bb, rb, ab} {
			b.Release()
		}
	}()
	var parent common.Hash
	for first := uint64(0); first < frozen; first += freezerBatchLimit {
		n := min(freezerBatchLimit, frozen-first)
		var items [4][][]byte
		for i, kind := range []string{ancient.ChainFreezerHashTable, ancient.ChainFreezerHeaderTable, ancient.ChainFreezerBodiesTable, ancient.ChainFreezerReceiptTable} {
			if items[i], err = read(kind, first, n); err != nil {
				return err
			}
		}
		// A block frozen before the list tables were added keeps its list
		// in the key-value store.
		var bals, balReads [][]byte
		if balFirst := max(first, balTail); balFirst < first+n {
			if bals, err = read(ancient.ChainFreezerBALTable, balFirst, first+n-balFirst); err != nil {
				return err
			}
			if balReads, err = read(ancient.ChainFreezerBALReadsTable, balFirst, first+n-balFirst); err != nil {
				return err
			}
			pad := make([][]byte, balFirst-first)
			bals, balReads = append(pad, bals...), append(append([][]byte{}, pad...), balReads...)
		}
		for i := range n {
			number, hash := first+i, common.BytesToHash(items[0][i])
			if number > 0 {
				header := items[1][i]
				if len(header) != 0 && header[0] == derivedHeaderPrefix {
					if header, err = decodeDerivedHeader(header[1:], number, parent); err != nil {
						return fmt.Errorf("frozen header %d: %w", number, err)
					}
				}
				err := errors.Join(hb.Put(headerKey(number, hash), header), hb.Put(headerHashKey(number), hash[:]),
					hb.Put(headerNumberKey(hash), common.Int64ToByteBigEndian(number)),
					bb.Put(blockBodyKey(number, hash), items[2][i]), rb.Put(blockReceiptsKey(number, hash), items[3][i]))
				if bals != nil && len(bals[i]) != 0 {
					err = errors.Join(err, ab.Put(balKey(balPrefix, number, hash), bals[i]))
				}
				if balReads != nil && len(balReads[i]) != 0 {
					err = errors.Join(err, ab.Put(balKey(balReadsPrefix, number, hash), balReads[i]))
				}
				if err != nil {
					return err
				}
				if _, err := WriteBatchesOverThreshold(hb, bb, rb, ab); err != nil {
					return err
				}
			}
			parent = hash
		}
		if _, err := WriteBatches(hb, bb, rb, ab); err != nil {
			return err
		}
		if progress != nil {
			progress(first+n, frozen)
		}
	}

	// The bloom bits get their section head hashes back, while the heads
	// are still frozen.
	misc := dbm.getDatabase(MiscDB)
	batch := misc.NewBatch()
	defer batch.Release()
	sections := dbm.frozenBloomSections()
	for section := uint64(0); section < sections; section++ {
		head := common.BytesToHash(dbm.readAncientHash((section+1)*bloomBitsSection - 1))
		for bit := uint(0); bit < types.BloomBitLength; bit++ {
			short := BloomBitsShortKey(bit, section)
			bits, _ := misc.Get(short)
			if len(bits) == 0 {
				continue
			}
			if err := errors.Join(batch.Put(BloomBitsKey(bit, section, head), bits), batch.Delete(short)); err != nil {
				return err
			}
			if _, err := WriteBatchesOverThreshold(batch); err != nil {
				return err
			}
		}
	}
	writeFrozenBloomSections(batch, 0)

	// Only once every block is in the key-value store.
	if err := dbm.TruncateFrozen(0); err != nil {
		return err
	}
	crit("Failed to delete the freezer directory", misc.Delete(ancientDirKey))
	crit("Failed to delete the compacted frozen blocks", misc.Delete(frozenCompactedKey))
	dbm.cm.clearHeaderChainCache()
	dbm.cm.clearBlockChainCache()
	logger.Info("Moved the frozen blocks back into the key-value store", "blocks", frozen)
	return nil
}

// RestoreFreezerFormats writes again, once Unfreeze emptied the freezer, what
// this version stores its own way, for a version without the freezer: the
// total blockscore of every canonical block, the receipts in their storage
// form, the tx lookup entries with their block hash, and the pending pruning
// marks in the misc database. The legacy istanbul snapshots and governance
// entries that the freeze loop deleted are not restored. After a crash it can
// run again. progress is called with the stage and the items it rewrote.
func (dbm *databaseManager) RestoreFreezerFormats(progress func(stage string, n uint64)) error {
	if dbm.FrozenBlocks() != 0 {
		return fmt.Errorf("the freezer still holds %d blocks, unfreeze them first", dbm.FrozenBlocks())
	}
	if progress == nil {
		progress = func(string, uint64) {}
	}
	genesis := dbm.ReadHeader(dbm.ReadCanonicalHash(0), 0)
	if genesis == nil || genesis.BlockScore == nil {
		return errors.New("genesis header missing")
	}
	// rewrite goes through the keys of prefix in db and puts what put returns
	// for them, in batches.
	rewrite := func(stage string, src Database, dst Batch, prefix []byte, put func(key, value []byte) error) error {
		it := src.NewIterator(prefix, nil)
		defer it.Release()
		var n uint64
		for it.Next() {
			before := dst.ValueSize()
			if err := put(it.Key(), it.Value()); err != nil {
				return err
			}
			if dst.ValueSize() != before {
				n++
				if n%1_000_000 == 0 {
					progress(stage, n)
				}
			}
			if _, err := WriteBatchesOverThreshold(dst); err != nil {
				return err
			}
		}
		if err := it.Error(); err != nil {
			return err
		}
		if err := dst.Write(); err != nil {
			return err
		}
		dst.Reset()
		progress(stage, n)
		return nil
	}

	misc := dbm.NewBatch(MiscDB)
	defer misc.Release()
	hashKeyLen := len(headerHashKey(0))
	if err := rewrite("total blockscores", dbm.getDatabase(headerDB), misc, headerPrefix, func(key, value []byte) error {
		if len(key) != hashKeyLen || !bytes.HasSuffix(key, headerHashSuffix) {
			return nil
		}
		number := binary.BigEndian.Uint64(key[len(headerPrefix):])
		td, err := rlp.EncodeToBytes(new(big.Int).Add(genesis.BlockScore, new(big.Int).SetUint64(number)))
		if err != nil {
			return err
		}
		return misc.Put(headerTDKey(number, common.BytesToHash(value)), td)
	}); err != nil {
		return err
	}

	receipts := dbm.NewBatch(ReceiptsDB)
	defer receipts.Release()
	receiptKeyLen := len(blockReceiptsKey(0, common.Hash{}))
	if err := rewrite("receipts", dbm.getDatabase(ReceiptsDB), receipts, blockReceiptsPrefix, func(key, value []byte) error {
		if len(key) != receiptKeyLen || len(value) == 0 || (value[0] != derivedReceiptsPrefix && value[0] != slimReceiptsPrefix) {
			return nil
		}
		number := binary.BigEndian.Uint64(key[len(blockReceiptsPrefix):])
		hash := common.BytesToHash(key[len(blockReceiptsPrefix)+8:])
		decoded, err := decodeReceipts(value, hash, number, func() *types.Body { return dbm.ReadBody(hash, number) })
		if err != nil {
			return fmt.Errorf("receipts of block %d: %w", number, err)
		}
		return receipts.Put(common.CopyBytes(key), encodeStorageReceipts(decoded))
	}); err != nil {
		return err
	}

	lookups := dbm.NewBatch(TxLookUpEntryDB)
	defer lookups.Release()
	if err := rewrite("tx lookup entries", dbm.getDatabase(TxLookUpEntryDB), lookups, txLookupPrefix, func(key, value []byte) error {
		var full TxLookupEntry
		if rlp.DecodeBytes(value, &full) == nil {
			return nil
		}
		var slim slimTxLookupEntry
		if err := rlp.DecodeBytes(value, &slim); err != nil {
			return nil
		}
		hash := dbm.ReadCanonicalHash(slim.BlockIndex)
		if common.EmptyHash(hash) {
			return nil // a stale entry, which resolves to nothing either way
		}
		enc, err := rlp.EncodeToBytes(TxLookupEntry{BlockHash: hash, BlockIndex: slim.BlockIndex, Index: slim.Index})
		if err != nil {
			return err
		}
		return lookups.Put(common.CopyBytes(key), enc)
	}); err != nil {
		return err
	}

	if dbm.pruningMarks != nil {
		var n uint64
		for {
			tail, _ := dbm.pruningMarks.Tail(ancient.PruningMarkGroup)
			head, _ := dbm.pruningMarks.Ancients()
			if tail >= head {
				break
			}
			items, err := dbm.pruningMarks.AncientRange(ancient.PruningMarkTable, tail, min(head-tail, pruningMarkBatchItems), pruningMarkBatchBytes)
			if err != nil {
				return err
			}
			for _, item := range items {
				var marks []PruningMark
				if err := rlp.DecodeBytes(item, &marks); err != nil {
					return err
				}
				for _, mark := range marks {
					if err := misc.Put(pruningMarkKey(mark), pruningMarkValue); err != nil {
						return err
					}
				}
				n += uint64(len(marks))
			}
			if err := misc.Write(); err != nil {
				return err
			}
			misc.Reset()
			if _, err := dbm.pruningMarks.TruncateTail(ancient.PruningMarkGroup, tail+uint64(len(items))); err != nil {
				return err
			}
		}
		if err := dbm.pruningMarks.SyncAncient(); err != nil {
			return err
		}
		progress("pruning marks", n)
	}
	logger.Info("Restored the formats of a version without the freezer")
	return nil
}

// readAncientHash returns the frozen canonical hash of a block number, or nil.
func (dbm *databaseManager) readAncientHash(number uint64) []byte {
	if dbm.freezer == nil || number >= dbm.FrozenBlocks() {
		return nil
	}
	return dbm.retrieveAncient(ancient.ChainFreezerHashTable, number)
}

// retrieveAncient returns an item of a frozen block. An error means the
// freezer cannot be read; the item is then reported as missing.
func (dbm *databaseManager) retrieveAncient(kind string, number uint64) []byte {
	data, err := dbm.freezer.Ancient(kind, number)
	if err != nil {
		freezerReadErrorCounter.Inc(1)
		logger.Error("Failed to read from the block freezer", "kind", kind, "number", number, "err", err)
		return nil
	}
	return data
}

// isFrozenCanon reports whether the block is frozen. Only canonical blocks are.
func (dbm *databaseManager) isFrozenCanon(hash common.Hash, number uint64) bool {
	data := dbm.readAncientHash(number)
	return len(data) != 0 && bytes.Equal(data, hash[:])
}

// kvBAL returns the writes and the reads of the block access list of a block
// in the key-value store, empty if there are none.
func (dbm *databaseManager) kvBAL(number uint64, hash common.Hash) (bal, reads []byte) {
	db := dbm.GetBALDB()
	bal, _ = db.Get(balKey(balPrefix, number, hash))
	reads, _ = db.Get(balKey(balReadsPrefix, number, hash))
	return bal, reads
}

// readFrozenBAL returns the frozen writes or reads of the block access list of
// a block, nil if there are none. A block frozen before its table was added has
// none: its list stays in the key-value store.
func (dbm *databaseManager) readFrozenBAL(kind string, hash common.Hash, number uint64) []byte {
	if !dbm.isFrozenCanon(hash, number) {
		return nil
	}
	if tail, err := dbm.freezer.Tail(ancient.ChainFreezerBALGroup); err != nil || number < tail {
		return nil
	}
	if data := dbm.retrieveAncient(kind, number); len(data) != 0 {
		return data
	}
	return nil
}

func (dbm *databaseManager) readAncient(kind string, hash common.Hash, number uint64) []byte {
	if !dbm.isFrozenCanon(hash, number) {
		return nil
	}
	return dbm.retrieveAncient(kind, number)
}

// freeze periodically moves the blocks older than the freezer threshold from
// the key-value store into the freezer.
func (dbm *databaseManager) freeze() {
	defer dbm.freezerWg.Done()
	defer logger.Info("Block freezer shutting down")

	var (
		backoff     bool
		triggered   chan struct{} // Used in tests
		compactFrom = dbm.frozenCompacted()
	)
	timer := time.NewTimer(freezerRecheckInterval)
	defer timer.Stop()

	// The total blockscore is derived; an older version stored that of the
	// genesis, which is never frozen, too.
	if hash := dbm.ReadCanonicalHash(0); !common.EmptyHash(hash) && !dbm.config.FreezerReadOnly {
		crit("Failed to delete the total blockscore of the genesis", dbm.getDatabase(MiscDB).Delete(headerTDKey(0, hash)))
	}

	for !dbm.freezerClosing() {
		if backoff {
			if triggered != nil {
				triggered <- struct{}{}
				triggered = nil
			}
			select {
			case <-timer.C:
				timer.Reset(freezerRecheckInterval)
			case triggered = <-dbm.freezerTrigger:
			case <-dbm.freezerQuit:
				return
			}
			backoff = false
		}
		_, n, err := dbm.freezeOnce()
		if err != nil {
			logger.Error("Error in block freeze operation", "err", err)
		}
		if n == freezerBatchLimit {
			continue
		}
		// Once the blocks are caught up, the old tx lookup entries are slimmed
		// a batch at a time, so that freezing goes on in between. It is shorter
		// than the compaction below, so it goes first.
		if dbm.slimTxLookups() {
			continue
		}
		// Deleted keys free disk space only once compacted, and the background
		// compaction rarely reaches old blocks. A compaction takes longer than
		// freezing its blocks, so the blocks a catch-up froze are compacted
		// once it is done.
		if end := dbm.FrozenBlocks(); end > compactFrom && end-compactFrom >= freezerCompactInterval {
			compactFrom = dbm.compactFrozen(compactFrom, end-1)
		}
		backoff = true
		dbm.trimFrozenMisc()
	}
}

// frozenCompacted returns the first frozen block whose key ranges are not
// compacted yet. The first time the freeze loop starts, it is the freezer head,
// stored at once: the compaction waits until the blocks are caught up, and a
// restart before then must not move its start up to the head of that time.
func (dbm *databaseManager) frozenCompacted() uint64 {
	misc := dbm.getDatabase(MiscDB)
	if data, _ := misc.Get(frozenCompactedKey); len(data) == 8 {
		return binary.BigEndian.Uint64(data)
	}
	frozen := dbm.FrozenBlocks()
	crit("Failed to store the compacted frozen blocks", misc.Put(frozenCompactedKey, binary.BigEndian.AppendUint64(nil, frozen)))
	return frozen
}

// freezeOnce freezes the next batch below the threshold and returns its first
// block number and size.
func (dbm *databaseManager) freezeOnce() (uint64, uint64, error) {
	dbm.freezeLock.Lock()
	defer dbm.freezeLock.Unlock()
	if dbm.unfrozen.Load() {
		return 0, 0, nil
	}

	threshold, ok := dbm.freezeThreshold()
	frozen := dbm.FrozenBlocks()
	if !ok || frozen > threshold {
		freezerLagGauge.Update(0)
		return 0, 0, nil
	}
	first, last := frozen, min(threshold, frozen+freezerBatchLimit-1)
	start := time.Now()
	if err := dbm.freezeRange(first, last); err != nil {
		freezerLagGauge.Update(int64(threshold - frozen + 1))
		return 0, 0, err
	}
	elapsed := time.Since(start)
	freezeTimer.Update(elapsed)
	freezerLagGauge.Update(int64(threshold - last))
	logger.Info("Froze chain segment", "from", first, "to", last, "elapsed", common.PrettyDuration(elapsed))
	return first, last - first + 1, nil
}

// compactFrozen compacts the key ranges of the frozen blocks first to last in
// chunks of freezerCompactChunk blocks, storing after each chunk how far it
// got. It stops between chunks on shutdown and returns the first block left.
func (dbm *databaseManager) compactFrozen(first, last uint64) uint64 {
	for first <= last && !dbm.freezerClosing() {
		end := min(last, first+freezerCompactChunk-1)
		start := time.Now()
		for _, r := range []struct {
			entry  DBEntryType
			prefix []byte
		}{
			{headerDB, headerPrefix},
			{BodyDB, blockBodyPrefix},
			{ReceiptsDB, blockReceiptsPrefix},
			{BALDB, balPrefix},
			{BALDB, balReadsPrefix},
		} {
			from := append(append([]byte{}, r.prefix...), common.Int64ToByteBigEndian(first)...)
			to := append(append([]byte{}, r.prefix...), common.Int64ToByteBigEndian(end+1)...)
			if err := dbm.getDatabase(r.entry).Compact(from, to); err != nil {
				logger.Warn("Failed to compact frozen blocks", "db", r.entry, "from", first, "to", end, "err", err)
			}
		}
		elapsed := time.Since(start)
		freezeCompactTimer.Update(elapsed)
		logger.Info("Compacted frozen blocks", "from", first, "to", end, "elapsed", common.PrettyDuration(elapsed))
		first = end + 1
		crit("Failed to store the compacted frozen blocks", dbm.getDatabase(MiscDB).Put(frozenCompactedKey, binary.BigEndian.AppendUint64(nil, first)))
	}
	return first
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

// frozenBlock is a block in its key-value encoding.
type frozenBlock struct {
	hash                   common.Hash
	header, body, receipts []byte
	txs                    types.Transactions
	bal, balReads          []byte // empty if the block has no block access list
}

// writeFrozen appends the n blocks from first that block returns to the
// freezer, flushes them and deletes them from the key-value store. The fields
// that can be derived are left out of the headers and receipts.
func (dbm *databaseManager) writeFrozen(first uint64, n int, block func(i int) (frozenBlock, error)) error {
	var parent common.Hash
	if first > 0 {
		parent = common.BytesToHash(dbm.readAncientHash(first - 1))
	} else {
		dbm.firstFreezeWarn.Do(func() {
			logger.Warn("Moving old blocks to the freezer for the first time. Once they are moved, this database can no longer be opened by versions without the freezer",
				"dir", dbm.freezerDir(), "threshold", dbm.config.FreezerThreshold)
		})
	}
	hashes := make([]common.Hash, n)
	_, err := dbm.freezer.ModifyAncients(func(op ancient.AncientWriteOp) error {
		for i := range n {
			b, err := block(i)
			if err != nil {
				return err
			}
			number := first + uint64(i)
			for _, item := range []struct {
				table string
				data  []byte
			}{
				{ancient.ChainFreezerHashTable, b.hash[:]},
				{ancient.ChainFreezerHeaderTable, frozenHeader(number, parent, b.header)},
				{ancient.ChainFreezerBodiesTable, b.body},
				{ancient.ChainFreezerReceiptTable, frozenReceipts(b.hash, number, b.txs, b.receipts)},
				{ancient.ChainFreezerBALTable, b.bal},
				{ancient.ChainFreezerBALReadsTable, b.balReads},
			} {
				if err := op.AppendRaw(item.table, number, item.data); err != nil {
					return fmt.Errorf("can't write %s of block %d to freezer: %v", item.table, number, err)
				}
			}
			hashes[i], parent = b.hash, b.hash
		}
		return nil
	})
	if err != nil {
		return err
	}
	crit("Failed to flush frozen tables", dbm.freezer.SyncAncient())
	// Remember the freezer before any block is gone from the key-value store.
	crit("Failed to store the freezer directory", dbm.getDatabase(MiscDB).Put(ancientDirKey, []byte(dbm.freezerDir())))
	crit("Failed to delete frozen blocks", dbm.deleteFrozenBlocks(first, hashes))
	frozenBlocksGauge.Update(int64(first) + int64(n))
	return nil
}

// freezeRange freezes the canonical blocks [first, last] of the key-value
// store. The genesis block is kept in the key-value store.
func (dbm *databaseManager) freezeRange(first, last uint64) error {
	hdb, bdb, rdb := dbm.getDatabase(headerDB), dbm.getDatabase(BodyDB), dbm.getDatabase(ReceiptsDB)
	n := int(last - first + 1)
	get, stop := readAhead(n, freezeReadWorkers, freezeReadAhead, func(i int) (frozenBlock, error) {
		number := first + uint64(i)
		hash, _ := hdb.Get(headerHashKey(number))
		if len(hash) == 0 {
			return frozenBlock{}, fmt.Errorf("canonical hash missing, can't freeze block %d", number)
		}
		b := frozenBlock{hash: common.BytesToHash(hash)}
		b.header, _ = hdb.Get(headerKey(number, b.hash))
		b.body, _ = bdb.Get(blockBodyKey(number, b.hash))
		b.receipts, _ = rdb.Get(blockReceiptsKey(number, b.hash))
		if len(b.header) == 0 || len(b.body) == 0 || len(b.receipts) == 0 {
			return frozenBlock{}, fmt.Errorf("header, body or receipts missing, can't freeze block %d", number)
		}
		var body types.Body
		if err := rlp.DecodeBytes(b.body, &body); err != nil {
			return frozenBlock{}, fmt.Errorf("invalid block body, can't freeze block %d: %v", number, err)
		}
		b.txs = body.Transactions
		b.bal, b.balReads = dbm.kvBAL(number, b.hash)
		return b, nil
	})
	err := dbm.writeFrozen(first, n, get)
	stop()
	return err
}

type readResult struct {
	b   frozenBlock
	err error
}

// readAhead reads the blocks 0 to n-1 with read in workers goroutines, at most
// ahead blocks ahead of get, so that the reads of a batch wait on the disk
// together. get returns them and must be called in order; stop ends the reads
// and is called once get is done with them.
func readAhead(n, workers, ahead int, read func(i int) (frozenBlock, error)) (get func(i int) (frozenBlock, error), stop func()) {
	results := make([]chan readResult, n)
	for i := range results {
		results[i] = make(chan readResult, 1)
	}
	var (
		jobs   = make(chan int)
		tokens = make(chan struct{}, ahead)
		quit   = make(chan struct{})
		wg     sync.WaitGroup
	)
	wg.Add(1 + workers)
	go func() {
		defer wg.Done()
		defer close(jobs)
		for i := range n {
			select {
			case tokens <- struct{}{}:
			case <-quit:
				return
			}
			select {
			case jobs <- i:
			case <-quit:
				return
			}
		}
	}()
	for range workers {
		go func() {
			defer wg.Done()
			for i := range jobs {
				b, err := read(i)
				results[i] <- readResult{b, err}
			}
		}()
	}
	get = func(i int) (frozenBlock, error) {
		r := <-results[i]
		<-tokens
		return r.b, r.err
	}
	stop = func() {
		close(quit)
		wg.Wait()
	}
	return get, stop
}

// slimTxLookups rewrites up to slimTxLookupsBatch tx lookup entries that an
// older version wrote with the block hash without it, so that a chain synced
// before the freezer saves the space too. It reads the store in key order from
// where it stopped, as the keys are hashes, and returns whether entries are
// left. A slim entry is checked against the canonical block when it is read,
// so the entries need no check here.
func (dbm *databaseManager) slimTxLookups() bool {
	misc := dbm.getDatabase(MiscDB)
	from, _ := misc.Get(slimTxLookupsKey)
	if bytes.Equal(from, slimTxLookupsDone) {
		return false
	}
	if len(from) == 0 {
		logger.Info("Slimming transaction lookup entries")
	}
	start := time.Now()
	db := dbm.getDatabase(TxLookUpEntryDB)
	it := db.NewIterator(txLookupPrefix, from)
	defer it.Release()
	batch := db.NewBatch()
	defer batch.Release()

	var (
		read, slimmed int
		next          []byte
	)
	for it.Next() {
		if read == slimTxLookupsBatch {
			next = common.CopyBytes(it.Key()[len(txLookupPrefix):])
			break
		}
		read++
		var entry TxLookupEntry
		if rlp.DecodeBytes(it.Value(), &entry) != nil {
			continue
		}
		slim, err := rlp.EncodeToBytes(slimTxLookupEntry{BlockIndex: entry.BlockIndex, Index: entry.Index})
		crit("Failed to encode transaction lookup entry", err)
		crit("Failed to store transaction lookup entry", batch.Put(common.CopyBytes(it.Key()), slim))
		slimmed++
		_, err = WriteBatchesOverThreshold(batch)
		crit("Failed to store transaction lookup entries", err)
	}
	crit("Failed to read transaction lookup entries", it.Error())
	crit("Failed to store transaction lookup entries", batch.Write())
	done := next == nil
	if done {
		next = slimTxLookupsDone
	}
	crit("Failed to store the slimmed transaction lookup entries", misc.Put(slimTxLookupsKey, next))
	logger.Info("Slimmed transaction lookup entries", "read", read, "slimmed", slimmed, "done", done, "elapsed", common.PrettyDuration(time.Since(start)))
	if done {
		dbm.compactTxLookups()
	}
	return !done
}

// compactTxLookups compacts the tx lookup store, so that the entries the full
// ones were rewritten over free their space. It goes through the key space in
// 16 ranges and stops between them on shutdown, leaving the rest to the
// background compaction.
func (dbm *databaseManager) compactTxLookups() {
	db := dbm.getDatabase(TxLookUpEntryDB)
	start := time.Now()
	for i := 0; i < 16 && !dbm.freezerClosing(); i++ {
		// The keys whose hash starts with a byte from 16*i to 16*i+15.
		from := append(common.CopyBytes(txLookupPrefix), byte(16*i))
		to := append(common.CopyBytes(txLookupPrefix), byte(16*i+16))
		if i == 15 {
			to = []byte{txLookupPrefix[0] + 1}
		}
		if err := db.Compact(from, to); err != nil {
			logger.Warn("Failed to compact transaction lookup entries", "err", err)
			return
		}
	}
	logger.Info("Compacted transaction lookup entries", "elapsed", common.PrettyDuration(time.Since(start)))
}

// FreezeSyncedBlocks writes the leading synced blocks that continue the
// freezer, are canonical and are more than FreezerThreshold below headHeader
// straight into the freezer, and returns how many it took. The caller writes
// the rest to the key-value store.
func (dbm *databaseManager) FreezeSyncedBlocks(blocks types.Blocks, receipts []types.Receipts, headHeader uint64) (int, error) {
	if dbm.freezer == nil || dbm.config.FreezerReadOnly || dbm.unfrozen.Load() || len(blocks) == 0 || headHeader <= dbm.config.FreezerThreshold {
		return 0, nil
	}
	limit := headHeader - dbm.config.FreezerThreshold
	first := blocks[0].NumberU64()
	if first == 0 || first > limit {
		return 0, nil
	}

	dbm.freezeLock.Lock()
	defer dbm.freezeLock.Unlock()

	// Catch up with the blocks synced before the head header was far enough ahead.
	for frozen := dbm.FrozenBlocks(); frozen < first; frozen = dbm.FrozenBlocks() {
		last := min(first-1, frozen+freezerBatchLimit-1)
		if err := dbm.freezeRange(frozen, last); err != nil {
			logger.Warn("Cannot freeze synced blocks", "from", frozen, "to", last, "err", err)
			return 0, nil
		}
		logger.Info("Froze chain segment", "from", frozen, "to", last)
	}
	if dbm.FrozenBlocks() != first {
		return 0, nil
	}

	hdb := dbm.getDatabase(headerDB)
	n := 0
	for ; n < len(blocks); n++ {
		number := blocks[n].NumberU64()
		if number != first+uint64(n) || number > limit {
			break
		}
		if canon, _ := hdb.Get(headerHashKey(number)); !bytes.Equal(canon, blocks[n].Hash().Bytes()) {
			break
		}
	}
	if n == 0 {
		return 0, nil
	}
	err := dbm.writeFrozen(first, n, func(i int) (frozenBlock, error) {
		block := blocks[i]
		header, err := rlp.EncodeToBytes(block.Header())
		if err != nil {
			return frozenBlock{}, err
		}
		body, err := rlp.EncodeToBytes(block.Body())
		if err != nil {
			return frozenBlock{}, err
		}
		bal, balReads := dbm.kvBAL(block.NumberU64(), block.Hash())
		return frozenBlock{block.Hash(), header, body, encodeStorageReceipts(receipts[i]), block.Transactions(), bal, balReads}, nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// deleteFrozenBlocks deletes the frozen canonical blocks from first, and the
// side chain blocks at the same heights, from the key-value store. The hash to
// number mappings of canonical blocks are kept.
func (dbm *databaseManager) deleteFrozenBlocks(first uint64, hashes []common.Hash) error {
	if len(hashes) == 0 {
		return nil
	}
	last := first + uint64(len(hashes)) - 1

	// Collected before the canonical headers are gone. Kaia has instant
	// finality, so there are normally none.
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
		if hash := common.BytesToHash(key[len(headerPrefix)+8:]); hash != hashes[number-first] {
			sideHashes = append(sideHashes, hash)
			sideNumbers = append(sideNumbers, number)
		}
	}
	it.Release()
	if err := it.Error(); err != nil {
		return err
	}

	hb, bb, rb, tb, ab := dbm.NewBatch(headerDB), dbm.NewBatch(BodyDB), dbm.NewBatch(ReceiptsDB), dbm.NewBatch(MiscDB), dbm.NewBatch(BALDB)
	defer func() {
		for _, b := range []Batch{hb, bb, rb, tb, ab} {
			b.Release()
		}
	}()
	for i, hash := range hashes {
		number := first + uint64(i)
		if number == 0 {
			continue
		}
		// The total blockscore is derived; an older version stored it.
		if err := errors.Join(hb.Delete(headerKey(number, hash)), hb.Delete(headerHashKey(number)),
			bb.Delete(blockBodyKey(number, hash)), rb.Delete(blockReceiptsKey(number, hash)),
			tb.Delete(headerTDKey(number, hash)),
			ab.Delete(balKey(balPrefix, number, hash)), ab.Delete(balKey(balReadsPrefix, number, hash))); err != nil {
			return err
		}
	}
	// The header store goes last: a crash in between leaves the canonical
	// hashes, by which the leftovers are found and deleted on the next open.
	if _, err := WriteBatches(bb, rb, tb, ab, hb); err != nil {
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

// migrateFreezer copies the block and pruning mark freezers into the default
// freezer directory of the migrated database and records them in its MiscDB.
func (dbm *databaseManager) migrateFreezer(dstdbm DBManager, quit chan struct{}) error {
	if dbm.FrozenBlocks() == 0 {
		return nil
	}
	copyTo := func(src, dst string, key []byte) error {
		if err := copyFreezerDir(src, dst, quit); err != nil {
			return err
		}
		return dstdbm.getDatabase(MiscDB).Put(key, []byte(dst))
	}
	start := time.Now()
	src := dbm.freezerDir()
	dst := filepath.Join(dstdbm.GetDBConfig().Dir, DefaultAncientDirName, ancient.ChainFreezerName)
	if err := copyTo(src, dst, ancientDirKey); err != nil {
		return err
	}
	if dbm.pruningMarks != nil {
		pruningSrc, _ := dbm.pruningMarks.AncientDatadir()
		if err := copyTo(pruningSrc, filepath.Join(filepath.Dir(dst), ancient.PruningMarkFreezerName), pruningMarkDirKey); err != nil {
			return err
		}
	}
	logger.Info("Finish freezer migration", "src", src, "dst", dst, "frozen", dbm.FrozenBlocks(), "elapsed", time.Since(start))
	return nil
}

// copyFreezerDir copies the files of a freezer into dst, which must be empty.
func copyFreezerDir(src, dst string, quit chan struct{}) error {
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
	return nil
}

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
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// A frozen header in the derived format leaves out the parent hash and number,
// which are the frozen hash of the previous block and the item number. It
// starts with derivedHeaderPrefix; an RLP list starts with at least 0xc0.
const (
	derivedHeaderPrefix   = 0x02
	headerParentHashField = 0
	headerNumberField     = 7
)

func splitRLPList(data []byte) ([]rlp.RawValue, error) {
	content, rest, err := rlp.SplitList(data)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing bytes after list")
	}
	var elems []rlp.RawValue
	for len(content) > 0 {
		_, _, tail, err := rlp.Split(content)
		if err != nil {
			return nil, err
		}
		elems = append(elems, content[:len(content)-len(tail)])
		content = tail
	}
	return elems, nil
}

func derivedHeaderFields(number uint64, parent common.Hash) (rlp.RawValue, rlp.RawValue) {
	p, _ := rlp.EncodeToBytes(parent)
	n, _ := rlp.EncodeToBytes(new(big.Int).SetUint64(number))
	return p, n
}

func encodeDerivedHeader(number uint64, parent common.Hash, header []byte) ([]byte, error) {
	elems, err := splitRLPList(header)
	if err != nil {
		return nil, err
	}
	if len(elems) <= headerNumberField {
		return nil, fmt.Errorf("header has %d fields", len(elems))
	}
	wantParent, wantNumber := derivedHeaderFields(number, parent)
	if !bytes.Equal(elems[headerParentHashField], wantParent) || !bytes.Equal(elems[headerNumberField], wantNumber) {
		return nil, errors.New("header parent hash or number does not match the freezer")
	}
	rest := append(append([]rlp.RawValue{}, elems[headerParentHashField+1:headerNumberField]...), elems[headerNumberField+1:]...)
	enc, err := rlp.EncodeToBytes(rest)
	if err != nil {
		return nil, err
	}
	return append([]byte{derivedHeaderPrefix}, enc...), nil
}

func decodeDerivedHeader(data []byte, number uint64, parent common.Hash) ([]byte, error) {
	rest, err := splitRLPList(data)
	if err != nil {
		return nil, err
	}
	if len(rest) < headerNumberField-1 {
		return nil, fmt.Errorf("derived header has %d fields", len(rest))
	}
	parentRLP, numberRLP := derivedHeaderFields(number, parent)
	elems := append([]rlp.RawValue{parentRLP}, rest[:headerNumberField-1]...)
	elems = append(append(elems, numberRLP), rest[headerNumberField-1:]...)
	return rlp.EncodeToBytes(elems)
}

// frozenHeader returns the header item to freeze: the derived format if it
// restores the header exactly, otherwise the header unchanged.
func frozenHeader(number uint64, parent common.Hash, header []byte) []byte {
	if number == 0 {
		return header
	}
	enc, err := encodeDerivedHeader(number, parent, header)
	if err == nil {
		var restored []byte
		if restored, err = decodeDerivedHeader(enc[1:], number, parent); err == nil {
			if bytes.Equal(restored, header) {
				return enc
			}
			err = errors.New("restored header differs")
		}
	}
	logger.Warn("Freezing header without deriving its fields", "number", number, "err", err)
	return header
}

// readFrozenHeader returns the frozen header of a canonical block in the
// key-value format, or nil.
func (dbm *databaseManager) readFrozenHeader(hash common.Hash, number uint64) []byte {
	data := dbm.readAncient(ancient.ChainFreezerHeaderTable, hash, number)
	if len(data) == 0 || data[0] != derivedHeaderPrefix {
		return data
	}
	parent := dbm.readAncientHash(number - 1)
	if len(parent) == 0 {
		freezerReadErrorCounter.Inc(1)
		logger.Error("Parent hash of a frozen header missing", "number", number)
		return nil
	}
	header, err := decodeDerivedHeader(data[1:], number, common.BytesToHash(parent))
	if err != nil {
		freezerReadErrorCounter.Inc(1)
		logger.Error("Invalid frozen header", "number", number, "err", err)
		return nil
	}
	return header
}

// Once blocks are frozen, the freeze loop trims their misc database entries:
//
//   - The bloom bits of a frozen section are stored without the section head
//     hash in their keys. Kaia has no reorgs, so the hash only told apart the
//     sections a rewind abandoned, and those are deleted with it.
//   - The legacy istanbul snapshots and governance entries of frozen blocks are
//     deleted once the valset and header gov modules rebuilt their data.

var (
	frozenBloomSectionsKey = []byte("FrozenBloomSections")
	legacyKeysDeletedKey   = []byte("LegacyKeysDeleted")

	// Owned by kaiax/valset and kaiax/gov/headergov.
	lowestScannedValidatorVoteNumKey = []byte("lowestScannedValidatorVoteNum")
	govDataBlockNumsKey              = []byte("governanceDataBlockNums")
)

const (
	bloomBitsShortKeyLen   = 1 + 2 + 8
	bloomBitsLongKeyLen    = bloomBitsShortKeyLen + common.HashLength
	legacySnapshotKeyLen   = 8 + common.HashLength // snapshotKeyPrefix + hash
	legacyGovernanceKeyLen = 10 + 8                // governancePrefix + num (uint64 little endian)

	frozenBloomBatchSections = 8
)

// bloomBitsSection is the number of blocks of a bloom bits section. Tests lower it.
var bloomBitsSection = params.BloomBitsBlocks

// BloomBitsShortKey = bloomBitsPrefix + bit (uint16 big endian) + section (uint64 big endian)
func BloomBitsShortKey(bit uint, section uint64) []byte {
	key := make([]byte, bloomBitsShortKeyLen)
	copy(key, bloomBitsPrefix)
	binary.BigEndian.PutUint16(key[1:], uint16(bit))
	binary.BigEndian.PutUint64(key[3:], section)
	return key
}

// readBloomBits reads the bloom bits of a key with the section head hash,
// trying the key without it first.
func (dbm *databaseManager) readBloomBits(key []byte) ([]byte, error) {
	db := dbm.getDatabase(MiscDB)
	if dbm.freezer != nil && len(key) == bloomBitsLongKeyLen && bytes.HasPrefix(key, bloomBitsPrefix) {
		if data, err := db.Get(key[:bloomBitsShortKeyLen]); err == nil && len(data) != 0 {
			return data, nil
		}
	}
	return db.Get(key)
}

func (dbm *databaseManager) trimFrozenMisc() {
	for dbm.shortenFrozenBloomBits(frozenBloomBatchSections) {
		if dbm.freezerClosing() {
			return
		}
	}
	dbm.deleteFrozenLegacyKeys()
}

// frozenBloomSections returns the number of sections whose bloom bits are
// stored without the section head hash.
func (dbm *databaseManager) frozenBloomSections() uint64 {
	data, _ := dbm.getDatabase(MiscDB).Get(frozenBloomSectionsKey)
	if len(data) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(data)
}

func writeFrozenBloomSections(batch Batch, sections uint64) {
	var enc [8]byte
	binary.BigEndian.PutUint64(enc[:], sections)
	crit("Failed to store frozen bloom bits", batch.Put(frozenBloomSectionsKey, enc[:]))
	crit("Failed to store frozen bloom bits", batch.Write())
}

// shortenFrozenBloomBits rewrites the bloom bits of up to n frozen sections
// without the section head hash. It stops at a section the bloom indexer has
// not stored yet, and returns whether it rewrote a section.
func (dbm *databaseManager) shortenFrozenBloomBits(n int) bool {
	dbm.freezeLock.Lock()
	defer dbm.freezeLock.Unlock()

	db := dbm.getDatabase(MiscDB)
	batch := db.NewBatch()
	defer batch.Release()

	first, frozen := dbm.frozenBloomSections(), dbm.FrozenBlocks()
	next := first
	for ; next < first+uint64(n) && (next+1)*bloomBitsSection <= frozen; next++ {
		head := common.BytesToHash(dbm.readAncientHash((next+1)*bloomBitsSection - 1))
		indexed, err := shortenBloomSection(db, batch, next, head)
		if err != nil {
			logger.Error("Failed to rewrite frozen bloom bits", "section", next, "err", err)
			return false
		}
		if !indexed {
			break
		}
	}
	if next == first {
		return false
	}
	writeFrozenBloomSections(batch, next)
	return true
}

// shortenBloomSection moves the bloom bits of a section to the keys without
// the section head hash, and deletes those a rewind left. It reports false if
// the bloom indexer has not stored the section yet.
func shortenBloomSection(db Database, batch Batch, section uint64, head common.Hash) (bool, error) {
	if has, _ := db.Has(BloomBitsKey(0, section, head)); !has {
		if has, _ := db.Has(BloomBitsShortKey(0, section)); !has {
			return false, nil
		}
	}
	for bit := uint(0); bit < types.BloomBitLength; bit++ {
		if err := shortenBloomBit(db, batch, BloomBitsShortKey(bit, section), head); err != nil {
			return false, err
		}
	}
	return true, nil
}

func shortenBloomBit(db Database, batch Batch, short []byte, head common.Hash) error {
	it := db.NewIterator(short, nil)
	defer it.Release()
	for it.Next() {
		key := it.Key()
		if len(key) != bloomBitsLongKeyLen {
			continue
		}
		if bytes.Equal(key[bloomBitsShortKeyLen:], head[:]) {
			if err := batch.Put(short, common.CopyBytes(it.Value())); err != nil {
				return err
			}
		}
		if err := batch.Delete(common.CopyBytes(key)); err != nil {
			return err
		}
	}
	return it.Error()
}

// unfreezeBloomBits deletes the short bloom bits of the sections that are no
// longer frozen once the freezer keeps items blocks. The bloom indexer stores
// them again, with the hash.
func (dbm *databaseManager) unfreezeBloomBits(items uint64) {
	first, next := items/bloomBitsSection, dbm.frozenBloomSections()
	if next <= first {
		return
	}
	batch := dbm.NewBatch(MiscDB)
	defer batch.Release()
	for section := first; section < next; section++ {
		for bit := uint(0); bit < types.BloomBitLength; bit++ {
			crit("Failed to delete frozen bloom bits", batch.Delete(BloomBitsShortKey(bit, section)))
		}
		_, err := WriteBatchesOverThreshold(batch)
		crit("Failed to delete frozen bloom bits", err)
	}
	writeFrozenBloomSections(batch, first)
}

// legacyMigrated reports whether the valset module scanned the votes of every
// block and the header gov module indexed the governance blocks.
func (dbm *databaseManager) legacyMigrated() bool {
	db := dbm.getDatabase(MiscDB)
	lowest, _ := db.Get(lowestScannedValidatorVoteNumKey)
	if len(lowest) != 8 || binary.BigEndian.Uint64(lowest) != 0 {
		return false
	}
	has, _ := db.Has(govDataBlockNumsKey)
	return has
}

func (dbm *databaseManager) deleteFrozenLegacyKeys() {
	db := dbm.getDatabase(MiscDB)
	if has, _ := db.Has(legacyKeysDeletedKey); has || !dbm.legacyMigrated() {
		return
	}
	dbm.freezeLock.Lock()
	defer dbm.freezeLock.Unlock()

	frozen := dbm.FrozenBlocks()
	batch := db.NewBatch()
	defer batch.Release()
	governance, left := dbm.deleteLegacyKeys(batch, governancePrefix, legacyGovernanceKeyLen, func(key []byte) bool {
		return binary.LittleEndian.Uint64(key[len(governancePrefix):]) < frozen
	})
	snapshots, snapshotsLeft := dbm.deleteLegacyKeys(batch, snapshotKeyPrefix, legacySnapshotKeyLen, func(key []byte) bool {
		number := dbm.ReadHeaderNumber(common.BytesToHash(key[len(snapshotKeyPrefix):]))
		return number == nil || *number < frozen
	})
	if left+snapshotsLeft == 0 {
		crit("Failed to delete legacy keys", batch.Delete(governanceHistoryKey))
		crit("Failed to delete legacy keys", batch.Put(legacyKeysDeletedKey, []byte{1}))
	}
	crit("Failed to delete legacy keys", batch.Write())
	if snapshots+governance > 0 {
		logger.Info("Deleted legacy istanbul snapshots and governance entries of frozen blocks", "snapshots", snapshots, "governance", governance)
	}
}

// deleteLegacyKeys adds to the batch the deletion of the misc keys of the
// prefix and length that frozen accepts, and returns how many it deleted and
// how many it left.
func (dbm *databaseManager) deleteLegacyKeys(batch Batch, prefix []byte, keyLen int, frozen func(key []byte) bool) (int, int) {
	var deleted, left int
	it := dbm.getDatabase(MiscDB).NewIterator(prefix, nil)
	defer it.Release()
	for it.Next() {
		key := it.Key()
		if len(key) != keyLen {
			continue
		}
		if !frozen(key) {
			left++
			continue
		}
		crit("Failed to delete legacy keys", batch.Delete(common.CopyBytes(key)))
		_, err := WriteBatchesOverThreshold(batch)
		crit("Failed to delete legacy keys", err)
		deleted++
	}
	if err := it.Error(); err != nil {
		logger.Error("Failed to delete legacy keys", "err", err)
		left++
	}
	return deleted, left
}

// With the block freezer enabled, the KIP-111 pruning marks of a trie database
// flush are appended to a freezer of their own as one item, and the prune loop
// consumes them from the tail. Marks in the misc database are still consumed.

var pruningMarkDirKey = []byte("PruningMarkDir")

const (
	pruningMarkBatchItems = 1024
	pruningMarkBatchBytes = 64 * 1024 * 1024
)

// openPruningMarkFreezer opens the pruning mark freezer next to the block
// freezer if the block freezer is writable.
func (dbm *databaseManager) openPruningMarkFreezer() error {
	marker, _ := dbm.getDatabase(MiscDB).Get(pruningMarkDirKey)
	if dbm.freezer == nil || dbm.freezerQuit == nil {
		if len(marker) != 0 {
			logger.Warn("Pruning marks are in a freezer that is not opened; their trie nodes are not pruned", "dir", string(marker))
		}
		return nil
	}
	dir := string(marker)
	if dir == "" {
		dir = filepath.Join(filepath.Dir(dbm.freezerDir()), ancient.PruningMarkFreezerName)
	}
	f, err := ancient.NewPruningMarkFreezer(dir, dbMetricPrefix+"pruning/", false)
	if err != nil {
		return err
	}
	dbm.pruningMarks = f
	return nil
}

func (dbm *databaseManager) closePruningMarkFreezer() {
	if dbm.pruningMarks == nil {
		return
	}
	if err := dbm.pruningMarks.Close(); err != nil {
		logger.Error("Failed to close pruning mark freezer", "err", err)
	}
}

func (dbm *databaseManager) appendPruningMarks(marks []PruningMark) {
	if len(marks) == 0 {
		return
	}
	data, err := rlp.EncodeToBytes(marks)
	crit("Failed to encode trie pruning marks", err)
	dbm.pruningMarkDirOnce.Do(func() {
		dir, _ := dbm.pruningMarks.AncientDatadir()
		crit("Failed to store the pruning mark freezer directory", dbm.getDatabase(MiscDB).Put(pruningMarkDirKey, []byte(dir)))
	})
	_, err = dbm.pruningMarks.ModifyAncients(func(op ancient.AncientWriteOp) error {
		next, _ := dbm.pruningMarks.Ancients()
		return op.AppendRaw(ancient.PruningMarkTable, next, data)
	})
	crit("Failed to store trie pruning marks", err)
}

// PruneMarkedTrieNodes hands the marks of the blocks [startNumber, endNumber)
// to prune, nil for PruneTrieNodes, deletes them and returns how many there
// were. A pruning mark freezer item is consumed once all its marks are below
// endNumber.
func (dbm *databaseManager) PruneMarkedTrieNodes(startNumber, endNumber uint64, prune func([]PruningMark)) int {
	if prune == nil {
		prune = dbm.PruneTrieNodes
	}
	marks := dbm.ReadPruningMarks(startNumber, endNumber)
	if len(marks) != 0 {
		prune(marks)
		dbm.DeletePruningMarks(marks)
	}
	count := len(marks)
	if dbm.pruningMarks != nil {
		count += dbm.pruneFrozenMarks(endNumber, prune)
	}
	return count
}

// DeletePruningMarksFrom deletes the pruning marks of block number and the
// blocks after it, which a rewind deleted.
func (dbm *databaseManager) DeletePruningMarksFrom(number uint64) {
	if marks := dbm.ReadPruningMarks(number, 0); len(marks) != 0 {
		dbm.DeletePruningMarks(marks)
	}
	if dbm.pruningMarks != nil {
		dbm.truncateFrozenMarksFrom(number)
	}
}

// truncateFrozenMarksFrom removes the marks of block number and after from the
// head of the pruning mark freezer. Its items hold the marks of one flush each,
// in order, so it drops items from the head and writes back the marks below
// number of the last one it drops.
func (dbm *databaseManager) truncateFrozenMarksFrom(number uint64) {
	tail, _ := dbm.pruningMarks.Tail(ancient.PruningMarkGroup)
	head, _ := dbm.pruningMarks.Ancients()
	var kept []PruningMark
	for head > tail {
		item, err := dbm.pruningMarks.Ancient(ancient.PruningMarkTable, head-1)
		if err != nil {
			logger.Error("Failed to read trie pruning marks", "item", head-1, "err", err)
			return
		}
		var marks []PruningMark
		if err := rlp.DecodeBytes(item, &marks); err != nil {
			logger.Error("Invalid trie pruning marks", "item", head-1, "err", err)
			return
		}
		if marksBelow(marks, number) {
			break
		}
		for _, m := range marks {
			if m.Number < number {
				kept = append(kept, m)
			}
		}
		head--
		if len(kept) != 0 {
			break
		}
	}
	if current, _ := dbm.pruningMarks.Ancients(); head == current {
		return
	}
	if _, err := dbm.pruningMarks.TruncateHead(head); err != nil {
		logger.Error("Failed to truncate trie pruning marks", "to", head, "err", err)
		return
	}
	dbm.appendPruningMarks(kept)
	if err := dbm.pruningMarks.SyncAncient(); err != nil {
		logger.Error("Failed to flush trie pruning marks", "err", err)
	}
}

func (dbm *databaseManager) pruneFrozenMarks(endNumber uint64, prune func([]PruningMark)) int {
	count := 0
	for {
		tail, _ := dbm.pruningMarks.Tail(ancient.PruningMarkGroup)
		head, _ := dbm.pruningMarks.Ancients()
		if tail >= head {
			return count
		}
		items, err := dbm.pruningMarks.AncientRange(ancient.PruningMarkTable, tail, min(head-tail, pruningMarkBatchItems), pruningMarkBatchBytes)
		if err != nil {
			logger.Error("Failed to read trie pruning marks", "from", tail, "err", err)
			return count
		}
		var marks []PruningMark
		next := tail
		for _, item := range items {
			var itemMarks []PruningMark
			if err := rlp.DecodeBytes(item, &itemMarks); err != nil {
				logger.Error("Invalid trie pruning marks", "item", next, "err", err)
				return count
			}
			if !marksBelow(itemMarks, endNumber) {
				break
			}
			marks = append(marks, itemMarks...)
			next++
		}
		if next == tail {
			return count
		}
		prune(marks)
		if _, err := dbm.pruningMarks.TruncateTail(ancient.PruningMarkGroup, next); err != nil {
			logger.Error("Failed to truncate trie pruning marks", "to", next, "err", err)
			return count
		}
		if err := dbm.pruningMarks.SyncAncient(); err != nil {
			logger.Error("Failed to flush trie pruning marks", "err", err)
		}
		count += len(marks)
		if next < tail+uint64(len(items)) {
			return count
		}
	}
}

// marksBelow reports whether all marks are below endNumber; 0 takes every mark.
func marksBelow(marks []PruningMark, endNumber uint64) bool {
	if endNumber == 0 {
		return true
	}
	for _, mark := range marks {
		if mark.Number >= endNumber {
			return false
		}
	}
	return true
}

// Receipts are stored in one of three formats. The key-value format of older
// versions is an RLP list, which starts with at least 0xc0, so a prefix byte
// below that tells the others apart.
const (
	// slimReceiptsPrefix leaves out the blooms, derived from the logs.
	slimReceiptsPrefix = 0x01
	// derivedReceiptsPrefix also leaves out every field derived from the block
	// and its transactions. It is read together with the block body.
	derivedReceiptsPrefix = 0x02
)

type slimReceiptRLP struct {
	Status          uint
	TxHash          common.Hash
	ContractAddress common.Address
	Logs            []*types.LogForStorage
	GasUsed         uint64
}

// derivedReceiptRLP keeps the contract address, since deriving it would need a
// signature recovery. It is empty unless the transaction created a contract.
type derivedReceiptRLP struct {
	Status          uint
	ContractAddress []byte
	Logs            []derivedLogRLP
	GasUsed         uint64
}

type derivedLogRLP struct {
	Address common.Address
	Topics  []common.Hash
	Data    []byte
}

var (
	errSlimReceiptsBloom    = errors.New("receipt bloom does not match its logs")
	errDerivedReceiptsField = errors.New("receipt field does not match the block")
)

func bloomMatches(r *types.ReceiptForStorage) bool {
	return types.CreateBloom(types.Receipts{(*types.Receipt)(r)}) == r.Bloom
}

func encodeSlimReceipts(receipts []*types.ReceiptForStorage) ([]byte, error) {
	slim := make([]slimReceiptRLP, len(receipts))
	for i, r := range receipts {
		if !bloomMatches(r) {
			return nil, errSlimReceiptsBloom
		}
		slim[i] = slimReceiptRLP{r.Status, r.TxHash, r.ContractAddress, make([]*types.LogForStorage, len(r.Logs)), r.GasUsed}
		for j, log := range r.Logs {
			slim[i].Logs[j] = (*types.LogForStorage)(log)
		}
	}
	enc, err := rlp.EncodeToBytes(slim)
	if err != nil {
		return nil, err
	}
	return append([]byte{slimReceiptsPrefix}, enc...), nil
}

func encodeDerivedReceipts(receipts []*types.ReceiptForStorage, hash common.Hash, number uint64, txs types.Transactions) ([]byte, error) {
	if len(receipts) != len(txs) {
		return nil, fmt.Errorf("%w: %d receipts, %d transactions", errDerivedReceiptsField, len(receipts), len(txs))
	}
	derived := make([]derivedReceiptRLP, len(receipts))
	logIndex := uint(0)
	for i, r := range receipts {
		txHash := txs[i].Hash()
		if r.TxHash != txHash {
			return nil, fmt.Errorf("%w: tx hash of receipt %d", errDerivedReceiptsField, i)
		}
		if !bloomMatches(r) {
			return nil, errSlimReceiptsBloom
		}
		derived[i] = derivedReceiptRLP{Status: r.Status, Logs: make([]derivedLogRLP, len(r.Logs)), GasUsed: r.GasUsed}
		if r.ContractAddress != (common.Address{}) {
			derived[i].ContractAddress = r.ContractAddress.Bytes()
		}
		for j, log := range r.Logs {
			if log.BlockNumber != number || log.BlockHash != hash || log.TxHash != txHash || log.TxIndex != uint(i) || log.Index != logIndex || log.Removed {
				return nil, fmt.Errorf("%w: log %d of receipt %d", errDerivedReceiptsField, j, i)
			}
			derived[i].Logs[j] = derivedLogRLP{Address: log.Address, Topics: log.Topics, Data: log.Data}
			logIndex++
		}
	}
	enc, err := rlp.EncodeToBytes(derived)
	if err != nil {
		return nil, err
	}
	return append([]byte{derivedReceiptsPrefix}, enc...), nil
}

// blockReceiptsItem returns the receipts item of a block in the smallest format
// that restores them exactly.
func blockReceiptsItem(hash common.Hash, number uint64, txs types.Transactions, receipts types.Receipts) []byte {
	storage := make([]*types.ReceiptForStorage, len(receipts))
	for i, r := range receipts {
		storage[i] = (*types.ReceiptForStorage)(r)
	}
	return slimReceiptsItem(hash, number, txs, storage, nil)
}

// frozenReceipts returns the receipts item to freeze. An item in the key-value
// format, written by an older version, is converted.
func frozenReceipts(hash common.Hash, number uint64, txs types.Transactions, data []byte) []byte {
	if len(data) > 0 && (data[0] == slimReceiptsPrefix || data[0] == derivedReceiptsPrefix) {
		return data
	}
	var receipts []*types.ReceiptForStorage
	if err := rlp.DecodeBytes(data, &receipts); err != nil {
		logger.Warn("Freezing undecodable receipts unchanged", "number", number, "err", err)
		return data
	}
	return slimReceiptsItem(hash, number, txs, receipts, data)
}

// slimReceiptsItem converts receipts into the derived format if it restores
// them exactly, else into the slim format if that does, else returns data, or
// their key-value encoding if data is nil.
func slimReceiptsItem(hash common.Hash, number uint64, txs types.Transactions, receipts []*types.ReceiptForStorage, data []byte) []byte {
	enc, err := encodeDerivedReceipts(receipts, hash, number, txs)
	if err == nil {
		return enc
	}
	logger.Warn("Storing receipts without deriving their fields", "number", number, "err", err)
	if enc, err := encodeSlimReceipts(receipts); err == nil {
		return enc
	}
	logger.Warn("Storing receipts without slimming", "number", number, "err", err)
	if data == nil {
		data, err = rlp.EncodeToBytes(receipts)
		crit("Failed to encode block receipts", err)
	}
	return data
}

// decodeReceipts decodes a receipts item in any format. body is read for the
// derived format only.
func decodeReceipts(data []byte, hash common.Hash, number uint64, body func() *types.Body) (types.Receipts, error) {
	if len(data) == 0 {
		return nil, errors.New("empty receipts item")
	}
	switch data[0] {
	case derivedReceiptsPrefix:
		return decodeDerivedReceipts(data[1:], hash, number, body)
	case slimReceiptsPrefix:
		var slim []slimReceiptRLP
		if err := rlp.DecodeBytes(data[1:], &slim); err != nil {
			return nil, err
		}
		receipts := make(types.Receipts, len(slim))
		for i, s := range slim {
			r := &types.Receipt{Status: s.Status, Logs: make([]*types.Log, len(s.Logs)), TxHash: s.TxHash, ContractAddress: s.ContractAddress, GasUsed: s.GasUsed}
			for j, log := range s.Logs {
				r.Logs[j] = (*types.Log)(log)
			}
			r.Bloom = types.CreateBloom(types.Receipts{r})
			receipts[i] = r
		}
		return receipts, nil
	}
	var storageReceipts []*types.ReceiptForStorage
	if err := rlp.DecodeBytes(data, &storageReceipts); err != nil {
		return nil, err
	}
	receipts := make(types.Receipts, len(storageReceipts))
	for i, receipt := range storageReceipts {
		receipts[i] = (*types.Receipt)(receipt)
	}
	return receipts, nil
}

func decodeDerivedReceipts(data []byte, hash common.Hash, number uint64, body func() *types.Body) (types.Receipts, error) {
	var derived []derivedReceiptRLP
	if err := rlp.DecodeBytes(data, &derived); err != nil {
		return nil, err
	}
	b := body()
	if b == nil {
		return nil, errors.New("block body missing")
	}
	if len(b.Transactions) != len(derived) {
		return nil, fmt.Errorf("%d receipts, %d transactions", len(derived), len(b.Transactions))
	}
	receipts := make(types.Receipts, len(derived))
	logIndex := uint(0)
	for i, d := range derived {
		txHash := b.Transactions[i].Hash()
		r := &types.Receipt{Status: d.Status, Logs: make([]*types.Log, len(d.Logs)), TxHash: txHash, GasUsed: d.GasUsed}
		if len(d.ContractAddress) != 0 {
			r.ContractAddress = common.BytesToAddress(d.ContractAddress)
		}
		for j, l := range d.Logs {
			r.Logs[j] = &types.Log{
				Address: l.Address, Topics: l.Topics, Data: l.Data,
				BlockNumber: number, TxHash: txHash, TxIndex: uint(i), BlockHash: hash, Index: logIndex,
			}
			logIndex++
		}
		r.Bloom = types.CreateBloom(types.Receipts{r})
		receipts[i] = r
	}
	return receipts, nil
}
