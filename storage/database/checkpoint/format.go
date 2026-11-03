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

// Package checkpoint implements an incremental segment store for state
// checkpoints. A segment is an immutable file of key-value pairs sorted by key.
// It may name a base segment, and a lookup in a segment falls through to its
// base chain, so a segment only holds what changed since its base.
//
// A segment file is laid out as
//
//	block 0 | block 1 | ... | top index | meta | footer
//
// Every block is snappy compressed and holds sorted entries whose keys share
// a prefix with the previous key. Data blocks hold the key-value pairs. Index
// blocks hold, for each data block, its first key and its handle (offset,
// length, checksum); an index block is written after its last data block. The
// top index, kept in memory while the segment is open, holds the first key and
// handle of each index block. The meta is JSON. The footer is fixed size:
//
//	top offset (8) | top length (4) | meta length (4) | checksum of top and meta (4) | version (4) | magic (8)
//
// All integers in the footer are little endian, and checksums are CRC-32C.
package checkpoint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"

	"github.com/golang/snappy"
	"github.com/kaiachain/kaia/log"
)

var logger = log.NewModuleLogger(log.StorageDatabase)

const (
	segmentSuffix = ".seg"
	tmpSuffix     = ".tmp"

	footerSize    = 32
	formatVersion = 1
	footerMagic   = 0x316970636169616b // "kaiacpi1"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

var (
	// ErrNotFound is returned when no segment of a chain holds the key.
	ErrNotFound = errors.New("checkpoint: key not found")
	// ErrKeyOrder is returned by Put when a key is not above the previous one.
	ErrKeyOrder = errors.New("checkpoint: keys must be put in strictly increasing order")
	// ErrCorrupt is returned when a segment file fails a checksum or is malformed.
	ErrCorrupt = errors.New("checkpoint: corrupt segment")
	// ErrUnknownSegment is returned for a segment id the store does not hold.
	ErrUnknownSegment = errors.New("checkpoint: unknown segment")
	// ErrSegmentExists is returned when writing a segment id that exists.
	ErrSegmentExists = errors.New("checkpoint: segment exists")
	// ErrReadOnly is returned for a write to a store opened read-only.
	ErrReadOnly = errors.New("checkpoint: store is read-only")
)

func segmentName(id uint64) string {
	return fmt.Sprintf("%020d%s", id, segmentSuffix)
}

// handle locates a block in a segment file.
type handle struct {
	offset uint64
	length uint32
	crc    uint32
}

func (h handle) end() uint64 { return h.offset + uint64(h.length) }

func appendHandle(dst []byte, h handle) []byte {
	dst = binary.AppendUvarint(dst, h.offset)
	dst = binary.AppendUvarint(dst, uint64(h.length))
	return binary.LittleEndian.AppendUint32(dst, h.crc)
}

func decodeHandle(b []byte) (handle, int, error) {
	offset, n := binary.Uvarint(b)
	if n <= 0 {
		return handle{}, 0, ErrCorrupt
	}
	length, m := binary.Uvarint(b[n:])
	if m <= 0 || length > 1<<31 || len(b) < n+m+4 {
		return handle{}, 0, ErrCorrupt
	}
	crc := binary.LittleEndian.Uint32(b[n+m:])
	return handle{offset: offset, length: uint32(length), crc: crc}, n + m + 4, nil
}

// blockBuilder accumulates the uncompressed payload of a block.
type blockBuilder struct {
	buf   []byte
	first []byte
	prev  []byte
	n     int
}

func (b *blockBuilder) add(key, value []byte) {
	shared := 0
	if b.n == 0 {
		b.first = append(b.first[:0], key...)
	} else {
		for shared < len(key) && shared < len(b.prev) && key[shared] == b.prev[shared] {
			shared++
		}
	}
	b.buf = binary.AppendUvarint(b.buf, uint64(shared))
	b.buf = binary.AppendUvarint(b.buf, uint64(len(key)-shared))
	b.buf = binary.AppendUvarint(b.buf, uint64(len(value)))
	b.buf = append(b.buf, key[shared:]...)
	b.buf = append(b.buf, value...)
	b.prev = append(b.prev[:0], key...)
	b.n++
}

func (b *blockBuilder) reset() {
	b.buf = b.buf[:0]
	b.n = 0
}

type entry struct {
	key, value []byte
}

// block is a decoded block. Its values point into the decompressed payload.
type block []entry

// decodeBlock checks the compressed block raw against its handle and decodes it.
func decodeBlock(raw []byte, h handle) (block, error) {
	if uint32(len(raw)) != h.length || crc32.Checksum(raw, castagnoli) != h.crc {
		return nil, fmt.Errorf("%w: block at offset %d fails its checksum", ErrCorrupt, h.offset)
	}
	payload, err := snappy.Decode(nil, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: block at offset %d: %v", ErrCorrupt, h.offset, err)
	}
	var (
		b    block
		prev []byte
	)
	for len(payload) > 0 {
		var fields [3]uint64
		for i := range fields {
			v, n := binary.Uvarint(payload)
			if n <= 0 {
				return nil, fmt.Errorf("%w: block at offset %d is malformed", ErrCorrupt, h.offset)
			}
			fields[i] = v
			payload = payload[n:]
		}
		shared, unshared, vlen := fields[0], fields[1], fields[2]
		if shared > uint64(len(prev)) || unshared > uint64(len(payload)) || vlen > uint64(len(payload))-unshared {
			return nil, fmt.Errorf("%w: block at offset %d is malformed", ErrCorrupt, h.offset)
		}
		key := make([]byte, shared+unshared)
		copy(key, prev[:shared])
		copy(key[shared:], payload[:unshared])
		if len(b) > 0 && bytes.Compare(key, prev) <= 0 {
			return nil, fmt.Errorf("%w: block at offset %d is unsorted", ErrCorrupt, h.offset)
		}
		value := payload[unshared : unshared+vlen : unshared+vlen]
		payload = payload[unshared+vlen:]
		b = append(b, entry{key: key, value: value})
		prev = key
	}
	return b, nil
}

// floor returns the index of the last entry whose key is not above key, or
// -1 if there is none.
func (b block) floor(key []byte) int {
	return sort.Search(len(b), func(i int) bool { return bytes.Compare(b[i].key, key) > 0 }) - 1
}

// topEntry is an index block in the top index.
type topEntry struct {
	first []byte
	h     handle
}

func encodeTop(top []topEntry) []byte {
	buf := binary.AppendUvarint(nil, uint64(len(top)))
	for _, t := range top {
		buf = binary.AppendUvarint(buf, uint64(len(t.first)))
		buf = append(buf, t.first...)
		buf = appendHandle(buf, t.h)
	}
	return buf
}

func decodeTop(b []byte) ([]topEntry, error) {
	count, n := binary.Uvarint(b)
	if n <= 0 || count > uint64(len(b)) {
		return nil, ErrCorrupt
	}
	b = b[n:]
	top := make([]topEntry, 0, count)
	for i := uint64(0); i < count; i++ {
		klen, n := binary.Uvarint(b)
		if n <= 0 || klen > uint64(len(b)-n) {
			return nil, ErrCorrupt
		}
		first := bytes.Clone(b[n : n+int(klen)])
		h, m, err := decodeHandle(b[n+int(klen):])
		if err != nil {
			return nil, err
		}
		top = append(top, topEntry{first: first, h: h})
		b = b[n+int(klen)+m:]
	}
	if len(b) != 0 {
		return nil, ErrCorrupt
	}
	return top, nil
}

type footer struct {
	topOffset uint64
	topLength uint32
	metaLen   uint32
	crc       uint32
}

func (f footer) encode() []byte {
	buf := make([]byte, 0, footerSize)
	buf = binary.LittleEndian.AppendUint64(buf, f.topOffset)
	buf = binary.LittleEndian.AppendUint32(buf, f.topLength)
	buf = binary.LittleEndian.AppendUint32(buf, f.metaLen)
	buf = binary.LittleEndian.AppendUint32(buf, f.crc)
	buf = binary.LittleEndian.AppendUint32(buf, formatVersion)
	return binary.LittleEndian.AppendUint64(buf, footerMagic)
}

func decodeFooter(b []byte) (footer, error) {
	if len(b) != footerSize || binary.LittleEndian.Uint64(b[24:]) != footerMagic {
		return footer{}, fmt.Errorf("%w: bad footer", ErrCorrupt)
	}
	if v := binary.LittleEndian.Uint32(b[20:]); v != formatVersion {
		return footer{}, fmt.Errorf("checkpoint: unsupported segment version %d", v)
	}
	return footer{
		topOffset: binary.LittleEndian.Uint64(b),
		topLength: binary.LittleEndian.Uint32(b[8:]),
		metaLen:   binary.LittleEndian.Uint32(b[12:]),
		crc:       binary.LittleEndian.Uint32(b[16:]),
	}, nil
}
