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
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database/ancient"
	"github.com/rcrowley/go-metrics"
)

// The state archive keeps every trie node committed by a live-pruning node in
// append-only freezer files, so that historical state stays readable after
// the prune loop deleted the nodes from the key-value store.
//
// Nodes are appended in chunks sorted by the counter of their ExtHash. Counters
// grow with time, so every node whose counter is above all archived counters
// ("in order") is found by a binary search on the first counter of each chunk.
// The other nodes ("exceptions"), such as the account trie roots that carry a
// zero counter, are found through the exception index in MiscDB, which maps
// the ExtHash to the chunk number.
//
// The first counter of a chunk is the counter of its first in-order node. A
// chunk that holds exceptions only gets the highest archived counter plus one.
// So the first counters never decrease, and an in-order node is in the last
// chunk whose first counter is not above the node's counter.

// ArchiveNode is a trie node to be appended to the state archive.
type ArchiveNode struct {
	Hash common.ExtHash
	Node []byte
}

var (
	// archiveChunkSize is the size of the encoded nodes after which a chunk is
	// closed. It is a variable so that tests can lower it.
	archiveChunkSize = 64 * 1024

	// archiveSampleInterval is the number of chunks between two first counters
	// kept in memory. A lookup searches the samples, then reads the first
	// counters of one interval from disk. It is a variable so that tests can
	// lower it.
	archiveSampleInterval uint64 = 1024
)

var errStateArchiveReadOnly = errors.New("state archive is read-only")

var (
	archiveAppendTimer     = metrics.NewRegisteredTimer(dbMetricPrefix+"statearchive/append/time", nil)
	archiveAppendBytes     = metrics.NewRegisteredMeter(dbMetricPrefix+"statearchive/append/bytes", nil)
	archiveAppendNodes     = metrics.NewRegisteredMeter(dbMetricPrefix+"statearchive/append/nodes", nil)
	archiveReadTimer       = metrics.NewRegisteredTimer(dbMetricPrefix+"statearchive/read/time", nil)
	archiveReadMissCounter = metrics.NewRegisteredCounter(dbMetricPrefix+"statearchive/read/miss", nil)
	archiveReadErrCounter  = metrics.NewRegisteredCounter(dbMetricPrefix+"statearchive/read/error", nil)
	archiveExceptionMeter  = metrics.NewRegisteredMeter(dbMetricPrefix+"statearchive/exception", nil)
	archiveChunksGauge     = metrics.NewRegisteredGauge(dbMetricPrefix+"statearchive/chunks", nil)
)

// stateArchive is the trie node freezer and its exception index.
type stateArchive struct {
	freezer  *ancient.Freezer
	index    Database // MiscDB, holds the exception index and the chunk count
	readonly bool

	// lock serializes appends. Lookups take it only to copy chunks and samples,
	// so that a slow disk read does not hold up the commit of a block.
	lock        sync.RWMutex
	chunks      uint64   // Number of chunks
	lastCounter uint64   // Highest counter of an in-order node, or the one before the first counter
	samples     []uint64 // First counter of every archiveSampleInterval-th chunk
}

// openStateArchiveFreezer opens the trie node freezer at dir and loads its
// in-memory index.
func openStateArchiveFreezer(dir string, index Database, readonly bool) (*stateArchive, error) {
	f, err := ancient.NewStateArchiveFreezer(dir, dbMetricPrefix+"statearchive/", readonly)
	if err != nil {
		return nil, err
	}
	a := &stateArchive{freezer: f, index: index, readonly: readonly}
	if err := a.load(); err != nil {
		f.Close()
		return nil, err
	}
	return a, nil
}

// load rebuilds the samples and the last counter from the freezer files.
func (a *stateArchive) load() error {
	chunks, err := a.freezer.Ancients()
	if err != nil {
		return err
	}
	a.chunks, a.samples, a.lastCounter = chunks, nil, 0
	for i := uint64(0); i < chunks; i += archiveSampleInterval {
		first, err := a.readFirst(i)
		if err != nil {
			return err
		}
		a.samples = append(a.samples, first)
	}
	if chunks == 0 {
		return nil
	}
	first, err := a.readFirst(chunks - 1)
	if err != nil {
		return err
	}
	data, err := a.freezer.Ancient(ancient.StateArchiveNodesTable, chunks-1)
	if err != nil {
		return err
	}
	nodes, err := decodeArchiveChunk(data)
	if err != nil {
		return fmt.Errorf("invalid state archive chunk %d: %v", chunks-1, err)
	}
	a.lastCounter = first - 1
	if len(nodes) > 0 {
		a.lastCounter = max(a.lastCounter, extHashCounter(nodes[len(nodes)-1].Hash))
	}
	archiveChunksGauge.Update(int64(chunks))
	return nil
}

func (a *stateArchive) close() error {
	return a.freezer.Close()
}

func (a *stateArchive) dir() string {
	dir, _ := a.freezer.AncientDatadir()
	return dir
}

func (a *stateArchive) readFirst(chunk uint64) (uint64, error) {
	data, err := a.freezer.Ancient(ancient.StateArchiveFirstTable, chunk)
	if err != nil {
		return 0, err
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("invalid first counter of state archive chunk %d", chunk)
	}
	return binary.BigEndian.Uint64(data), nil
}

// append archives the given nodes, which the commit of block is about to write
// to the key-value store. The nodes are fsynced, and the exception index is
// written, before it returns, so that a node in the key-value store is always
// in the archive as well.
func (a *stateArchive) append(block uint64, nodes []ArchiveNode) error {
	if a.readonly {
		return errStateArchiveReadOnly
	}
	if len(nodes) == 0 {
		return nil
	}
	start := time.Now()
	a.lock.Lock()
	defer a.lock.Unlock()

	// Sort by counter. The exceptions are not above the last counter, so they
	// come first. An exception that is already archived (e.g. the state root of
	// a block that changed no state) is not stored again.
	sorted := slices.Clone(nodes)
	slices.SortFunc(sorted, func(x, y ArchiveNode) int { return compareExtHash(x.Hash, y.Hash) })
	sorted = slices.CompactFunc(sorted, func(x, y ArchiveNode) bool { return x.Hash == y.Hash })
	sorted = slices.DeleteFunc(sorted, func(n ArchiveNode) bool {
		if c := extHashCounter(n.Hash); c != 0 && c > a.lastCounter {
			return false
		}
		ok, _ := a.index.Has(stateArchiveNodeKey(n.Hash))
		return ok
	})
	if len(sorted) == 0 {
		return nil
	}

	type chunk struct {
		first uint64
		data  []byte
	}
	type exception struct {
		hash  common.ExtHash
		chunk uint64
	}
	var (
		chunks     []chunk
		cur        []ArchiveNode
		size       int
		lastBefore = a.lastCounter
		last       = a.lastCounter
		exceptions []exception
	)
	closeChunk := func() {
		first := lastBefore + 1
		for _, n := range cur {
			if c := extHashCounter(n.Hash); c != 0 && c > lastBefore {
				first = c
				break
			}
		}
		chunks = append(chunks, chunk{first: first, data: encodeArchiveChunk(cur)})
		cur, size = nil, 0
	}
	for _, n := range sorted {
		if c := extHashCounter(n.Hash); c != 0 && c > lastBefore {
			last = c
		} else {
			exceptions = append(exceptions, exception{n.Hash, a.chunks + uint64(len(chunks))})
		}
		cur = append(cur, n)
		size += archiveNodeHeaderSize + len(n.Node)
		if size >= archiveChunkSize {
			closeChunk()
		}
	}
	if len(cur) > 0 {
		closeChunk()
	}

	// Append and fsync the chunks.
	var written int64
	_, err := a.freezer.ModifyAncients(func(op ancient.AncientWriteOp) error {
		var buf [8]byte
		for i, c := range chunks {
			number := a.chunks + uint64(i)
			if err := op.AppendRaw(ancient.StateArchiveNodesTable, number, c.data); err != nil {
				return err
			}
			binary.BigEndian.PutUint64(buf[:], c.first)
			if err := op.AppendRaw(ancient.StateArchiveFirstTable, number, buf[:]); err != nil {
				return err
			}
			binary.BigEndian.PutUint64(buf[:], block)
			if err := op.AppendRaw(ancient.StateArchiveBlockTable, number, buf[:]); err != nil {
				return err
			}
			written += int64(len(c.data))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := a.freezer.SyncAncient(); err != nil {
		return err
	}

	// Index the exceptions and record the chunk count.
	batch := a.index.NewBatch()
	defer batch.Release()
	for _, e := range exceptions {
		var number [8]byte
		binary.BigEndian.PutUint64(number[:], e.chunk)
		if err := batch.Put(stateArchiveNodeKey(e.hash), number[:]); err != nil {
			return err
		}
		if _, err := WriteBatchesOverThreshold(batch); err != nil {
			return err
		}
	}
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], a.chunks+uint64(len(chunks)))
	if err := batch.Put(stateArchiveChunksKey, count[:]); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return err
	}

	// Publish the new chunks to lookups.
	for i, c := range chunks {
		if number := a.chunks + uint64(i); number%archiveSampleInterval == 0 {
			a.samples = append(a.samples, c.first)
		}
	}
	a.chunks += uint64(len(chunks))
	a.lastCounter = last

	archiveAppendTimer.UpdateSince(start)
	archiveAppendBytes.Mark(written)
	archiveAppendNodes.Mark(int64(len(sorted)))
	archiveExceptionMeter.Mark(int64(len(exceptions)))
	archiveChunksGauge.Update(int64(a.chunks))
	return nil
}

// lookup returns the archived node with the given hash, or nil if it is not
// archived.
func (a *stateArchive) lookup(hash common.ExtHash) []byte {
	start := time.Now()
	defer archiveReadTimer.UpdateSince(start)

	a.lock.RLock()
	chunks, samples := a.chunks, a.samples
	a.lock.RUnlock()

	if counter := extHashCounter(hash); counter != 0 {
		if chunk, ok := a.findChunk(counter, chunks, samples); ok {
			if node := a.readNode(chunk, hash); node != nil {
				return node
			}
		}
	}
	data, _ := a.index.Get(stateArchiveNodeKey(hash))
	if len(data) == 8 {
		if chunk := binary.BigEndian.Uint64(data); chunk < chunks {
			if node := a.readNode(chunk, hash); node != nil {
				return node
			}
		}
	}
	archiveReadMissCounter.Inc(1)
	return nil
}

// findChunk returns the last of the first chunks whose first counter is not
// above the given counter.
func (a *stateArchive) findChunk(counter uint64, chunks uint64, samples []uint64) (uint64, bool) {
	group := sort.Search(len(samples), func(i int) bool { return samples[i] > counter }) - 1
	if group < 0 {
		return 0, false
	}
	from := uint64(group) * archiveSampleInterval
	count := min(archiveSampleInterval, chunks-from)
	items, err := a.freezer.AncientRange(ancient.StateArchiveFirstTable, from, count, 0)
	if err != nil {
		archiveReadErrCounter.Inc(1)
		logger.Error("Failed to read from the state archive", "kind", ancient.StateArchiveFirstTable, "from", from, "err", err)
		return 0, false
	}
	i := sort.Search(len(items), func(i int) bool { return binary.BigEndian.Uint64(items[i]) > counter }) - 1
	if i < 0 {
		return 0, false
	}
	return from + uint64(i), true
}

// readNode returns the node with the given hash from the chunk, or nil if the
// chunk does not hold it.
func (a *stateArchive) readNode(chunk uint64, hash common.ExtHash) []byte {
	data, err := a.freezer.Ancient(ancient.StateArchiveNodesTable, chunk)
	if err != nil {
		archiveReadErrCounter.Inc(1)
		logger.Error("Failed to read from the state archive", "kind", ancient.StateArchiveNodesTable, "chunk", chunk, "err", err)
		return nil
	}
	node, err := findInArchiveChunk(data, hash)
	if err != nil {
		archiveReadErrCounter.Inc(1)
		logger.Error("Invalid state archive chunk", "chunk", chunk, "err", err)
		return nil
	}
	return node
}

// archiveNodeHeaderSize is the size of a node in a chunk without its RLP and
// its length prefix.
const archiveNodeHeaderSize = common.ExtHashCounterLength + common.HashLength

// encodeArchiveChunk encodes the sorted nodes as
//
//	count(uvarint) | repeat count times: counter(7) | hash(32) | len(uvarint) | rlp
func encodeArchiveChunk(nodes []ArchiveNode) []byte {
	size := binary.MaxVarintLen64
	for _, n := range nodes {
		size += archiveNodeHeaderSize + binary.MaxVarintLen64 + len(n.Node)
	}
	out := make([]byte, 0, size)
	out = binary.AppendUvarint(out, uint64(len(nodes)))
	for _, n := range nodes {
		counter := n.Hash.Counter()
		hash := n.Hash.Unextend()
		out = append(out, counter[:]...)
		out = append(out, hash[:]...)
		out = binary.AppendUvarint(out, uint64(len(n.Node)))
		out = append(out, n.Node...)
	}
	return out
}

// archiveChunkIterator walks the nodes of an encoded chunk without copying.
type archiveChunkIterator struct {
	data  []byte
	left  uint64
	hash  common.ExtHash
	node  []byte
	error error
}

func newArchiveChunkIterator(data []byte) *archiveChunkIterator {
	count, n := binary.Uvarint(data)
	if n <= 0 {
		return &archiveChunkIterator{error: errors.New("invalid node count")}
	}
	return &archiveChunkIterator{data: data[n:], left: count}
}

func (it *archiveChunkIterator) next() bool {
	if it.error != nil || it.left == 0 {
		if it.error == nil && len(it.data) != 0 {
			it.error = errors.New("trailing bytes")
		}
		return false
	}
	if len(it.data) < archiveNodeHeaderSize {
		it.error = errors.New("truncated node")
		return false
	}
	copy(it.hash[common.HashLength:], it.data[:common.ExtHashCounterLength])
	copy(it.hash[:common.HashLength], it.data[common.ExtHashCounterLength:archiveNodeHeaderSize])
	size, n := binary.Uvarint(it.data[archiveNodeHeaderSize:])
	if n <= 0 || uint64(len(it.data)-archiveNodeHeaderSize-n) < size {
		it.error = errors.New("truncated node")
		return false
	}
	body := it.data[archiveNodeHeaderSize+n:]
	it.node = body[:size:size]
	it.data = body[size:]
	it.left--
	return true
}

// decodeArchiveChunk decodes all nodes of a chunk.
func decodeArchiveChunk(data []byte) ([]ArchiveNode, error) {
	var nodes []ArchiveNode
	it := newArchiveChunkIterator(data)
	for it.next() {
		nodes = append(nodes, ArchiveNode{Hash: it.hash, Node: it.node})
	}
	return nodes, it.error
}

// findInArchiveChunk returns a copy of the node with the given hash from an
// encoded chunk, or nil if the chunk does not hold it.
func findInArchiveChunk(data []byte, hash common.ExtHash) ([]byte, error) {
	it := newArchiveChunkIterator(data)
	for it.next() {
		switch cmp := compareExtHash(it.hash, hash); {
		case cmp == 0:
			return bytes.Clone(it.node), nil
		case cmp > 0:
			return nil, nil
		}
	}
	return nil, it.error
}

// extHashCounter returns the counter of the ExtHash as a number.
func extHashCounter(hash common.ExtHash) uint64 {
	var buf [8]byte
	copy(buf[1:], hash[common.HashLength:])
	return binary.BigEndian.Uint64(buf[:])
}

// compareExtHash orders ExtHashes by counter, then by hash.
func compareExtHash(x, y common.ExtHash) int {
	if c := bytes.Compare(x[common.HashLength:], y[common.HashLength:]); c != 0 {
		return c
	}
	return bytes.Compare(x[:common.HashLength], y[:common.HashLength])
}
