// Copyright 2018 The klaytn Authors
// Copyright 2014 The go-ethereum Authors
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
// This file is derived from core/state/statdb.go (2018/06/04).
// Modified and improved for the klaytn development.

/*
Package state provides an uppermost caching layer of the Kaia state trie.
This package is used to read/write stateObject from/to StateDB and it also acts as a bridge between
the objects and the persistent layer.

# Overview of state package

stateObject represents a Kaia account identified by its address.
Once it is loaded from the persistent layer, it is cached and managed by StateDB.

StateDB caches stateObjects and mediates the operations to them. It reads
accounts, storage and code through a Reader, the snapshot or the block access
list history, and from the state trie what the reader does not serve.

Manager keeps the states of the blocks a chain writes: the trie nodes and the
snapshot, and for a history node the block access list history.

# Source Files

Related functions and variables are defined in the files listed below
  - bal.go                   : Recording, storing, indexing and reading the block access lists, and the BAL history Manager opens states from
  - database.go              : Defines Database and other interfaces used in the package
  - dump.go                  : Functions to dump the contents of StateDB both in raw format and indented format
  - journal.go               : journal and state changes to track the list of state modifications since the last state commit
  - manager.go               : Manager, which keeps the states of the written blocks in the state trie and the BAL history, and opens the states of blocks
  - state_migration.go       : Migration, warm-up and statistics of the state trie
  - state_object.go          : Implementation of stateObject
  - state_object_encoder.go  : stateObjectEncoder is used to encode stateObject in parallel manner
  - statedb.go               : Implementation of StateDB, with the recording of the block access list of the executed block into a Recorder and the applying of a list with Apply
  - sync.go                  : Functions to schedule a state trie download
  - trie.go                  : Flushing, garbage collection and pruning of the trie nodes of the written blocks, and the snapshot
*/
package state
