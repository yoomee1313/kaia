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
	"path/filepath"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database/ancient"
)

// openStateArchive opens the state archive if it is enabled and supported by
// the configured backend. The archive must hold every node committed since
// the genesis, so it can only be enabled on a database without blocks, and
// once enabled it cannot be left out. With FreezerReadOnly, an existing
// archive is opened for reads only.
func (dbm *databaseManager) openStateArchive() error {
	dbc := dbm.config
	miscDB := dbm.getDatabase(MiscDB)
	marker, _ := miscDB.Get(stateArchiveDirKey)

	readonly := !dbc.EnableStateArchive && dbc.FreezerReadOnly && len(marker) != 0
	enabled := dbc.EnableStateArchive || readonly
	switch {
	case !enabled:
	case dbc.DBType == MemoryDB || dbc.DBType == DynamoDB:
		logger.Warn("State archive is not supported for the database type, disabling it", "DBType", dbc.DBType)
		enabled = false
	case dbc.RocksDBConfig != nil && dbc.RocksDBConfig.Secondary:
		logger.Warn("State archive is not supported for a secondary RocksDB yet, disabling it")
		enabled = false
	}
	if !enabled {
		if len(marker) != 0 {
			return fmt.Errorf("trie nodes were archived at %s, enable the state archive to read them", marker)
		}
		return nil
	}
	if len(marker) == 0 && !common.EmptyHash(dbm.ReadHeadBlockHash()) {
		return fmt.Errorf("the state archive must be enabled before the genesis block is written (e.g. at init)")
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
		dir = filepath.Join(root, ancient.StateArchiveName)
	}
	a, err := openStateArchiveFreezer(dir, miscDB, readonly)
	if err != nil {
		return err
	}
	// Chunks are fsynced before the chunk count is recorded, so the archive may
	// hold more chunks than recorded (a crash in between), but never fewer.
	var recorded uint64
	if data, _ := miscDB.Get(stateArchiveChunksKey); len(data) == 8 {
		if recorded = binary.BigEndian.Uint64(data); a.chunks < recorded {
			a.close()
			return fmt.Errorf("state archive at %s holds %d chunks, but %d were archived at %s; the archive is older than the database",
				dir, a.chunks, recorded, marker)
		}
	}
	dbm.stateArchive = a
	if readonly {
		logger.Info("Opened state archive read-only", "dir", dir, "chunks", a.chunks)
		return nil
	}
	// The exception index of the unrecorded chunks may be lost.
	if err := a.reindex(recorded); err != nil {
		a.close()
		dbm.stateArchive = nil
		return err
	}
	if err := a.recordSamples(); err != nil {
		a.close()
		dbm.stateArchive = nil
		return err
	}
	if string(marker) != dir {
		if err := miscDB.Put(stateArchiveDirKey, []byte(dir)); err != nil {
			return err
		}
	}
	logger.Info("Opened state archive", "dir", dir, "chunks", a.chunks)
	return nil
}

// closeStateArchive closes the state archive. It can be called more than once.
func (dbm *databaseManager) closeStateArchive() {
	if dbm.stateArchive == nil {
		return
	}
	dbm.stateArchiveCloseOnce.Do(func() {
		if err := dbm.stateArchive.close(); err != nil {
			logger.Error("Failed to close state archive", "err", err)
		}
	})
}

// StateArchiveEnabled reports whether the state archive is open.
func (dbm *databaseManager) StateArchiveEnabled() bool {
	return dbm.stateArchive != nil
}

// AppendStateArchive archives the trie nodes that the commit of the given
// block is about to write to the key-value store. It does nothing if the state
// archive is disabled.
func (dbm *databaseManager) AppendStateArchive(block uint64, nodes []ArchiveNode) error {
	if dbm.stateArchive == nil {
		return nil
	}
	return dbm.stateArchive.append(block, nodes)
}
