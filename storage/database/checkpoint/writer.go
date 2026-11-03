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

package checkpoint

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"

	"github.com/golang/snappy"
	"github.com/kaiachain/kaia/common"
)

// Trigger tells what made a checkpoint.
type Trigger string

const (
	// TriggerTime is a checkpoint at the first block past a yearly time.
	TriggerTime Trigger = "time"
	// TriggerInterval is a checkpoint at a multiple of a block interval.
	TriggerInterval Trigger = "interval"
)

// Meta describes a segment. It is stored as JSON in the segment file.
type Meta struct {
	// ID identifies the segment. It is above zero and above the id of its base.
	ID uint64 `json:"id"`
	// Base is the id of the segment that lookups fall through to, or zero for
	// a segment that holds the whole state.
	Base        uint64      `json:"base,omitempty"`
	BlockNumber uint64      `json:"blockNumber"`
	BlockHash   common.Hash `json:"blockHash"`
	StateRoot   common.Hash `json:"stateRoot"`
	Trigger     Trigger     `json:"trigger"`
	// Entries is the number of key-value pairs, set by Finish.
	Entries uint64 `json:"entries"`
	// Checksum is the CRC-32C of the blocks, set by Finish.
	Checksum uint32 `json:"checksum"`
	// Extra is free for the user of the store.
	Extra json.RawMessage `json:"extra,omitempty"`
}

// Options configure a store and its writers.
type Options struct {
	// BlockSize is the uncompressed size a block is cut at. Default 16 KiB.
	BlockSize int
	// CacheBlocks is the number of decoded blocks kept in memory. Default 4096.
	CacheBlocks int
	// ReadAhead is the number of bytes read past a block missed right after
	// the previously read one. Default 256 KiB; negative disables it.
	ReadAhead int
	// ReadOnly opens the store without touching its directory.
	ReadOnly bool
}

func (o *Options) withDefaults() Options {
	var opts Options
	if o != nil {
		opts = *o
	}
	if opts.BlockSize <= 0 {
		opts.BlockSize = 16 << 10
	}
	if opts.CacheBlocks <= 0 {
		opts.CacheBlocks = 4096
	}
	if opts.ReadAhead == 0 {
		opts.ReadAhead = 256 << 10
	}
	return opts
}

// Writer writes a segment. Keys must be put in strictly increasing order, and
// only one writer may write a given segment id at a time.
// Nothing is visible until Finish returns; a writer that is aborted or dies
// leaves only a temporary file, which the store removes when it opens.
type Writer struct {
	dir, tmpPath, path string
	f                  *os.File
	w                  *bufio.Writer
	meta               Meta
	blockSize          int
	replace            bool
	onFinish           func(path string) error

	data, index blockBuilder
	top         []topEntry
	offset      uint64
	sum         hash.Hash32
	last        []byte
	entries     uint64
	err         error
	done        bool
}

// NewWriter starts segment meta.ID in dir. The base segment, if any, must be
// in dir. Entries and Checksum of meta are ignored.
func NewWriter(dir string, meta Meta, opts *Options) (*Writer, error) {
	if meta.ID == 0 {
		return nil, errors.New("checkpoint: segment id must be above zero")
	}
	if meta.Base != 0 {
		if meta.Base >= meta.ID {
			return nil, fmt.Errorf("checkpoint: base %d of segment %d is not below it", meta.Base, meta.ID)
		}
		if _, err := os.Stat(filepath.Join(dir, segmentName(meta.Base))); err != nil {
			return nil, fmt.Errorf("%w: base %d: %v", ErrUnknownSegment, meta.Base, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, segmentName(meta.ID))); err == nil {
		return nil, fmt.Errorf("%w: %d", ErrSegmentExists, meta.ID)
	}
	return newWriter(dir, meta, opts.withDefaults(), false, nil)
}

func newWriter(dir string, meta Meta, opts Options, replace bool, onFinish func(string) error) (*Writer, error) {
	path := filepath.Join(dir, segmentName(meta.ID))
	tmpPath := path + tmpSuffix
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	meta.Entries, meta.Checksum = 0, 0
	return &Writer{
		dir:       dir,
		tmpPath:   tmpPath,
		path:      path,
		f:         f,
		w:         bufio.NewWriterSize(f, 1<<20),
		meta:      meta,
		blockSize: opts.BlockSize,
		replace:   replace,
		onFinish:  onFinish,
		sum:       crc32.New(castagnoli),
	}, nil
}

// Put adds a key-value pair. The key must be above the previous one.
func (w *Writer) Put(key, value []byte) error {
	if w.err != nil {
		return w.err
	}
	if w.done {
		return errors.New("checkpoint: writer is finished")
	}
	if w.entries > 0 && bytes.Compare(key, w.last) <= 0 {
		return fmt.Errorf("%w: %x after %x", ErrKeyOrder, key, w.last)
	}
	w.data.add(key, value)
	w.last = append(w.last[:0], key...)
	w.entries++
	if len(w.data.buf) >= w.blockSize {
		w.flushData()
	}
	return w.err
}

// Entries returns the number of pairs put so far.
func (w *Writer) Entries() uint64 { return w.entries }

func (w *Writer) writeBlock(payload []byte) handle {
	raw := snappy.Encode(nil, payload)
	h := handle{offset: w.offset, length: uint32(len(raw)), crc: crc32.Checksum(raw, castagnoli)}
	if _, err := w.w.Write(raw); err != nil {
		w.fail(err)
	}
	w.sum.Write(raw)
	w.offset += uint64(len(raw))
	return h
}

func (w *Writer) flushData() {
	if w.data.n == 0 {
		return
	}
	h := w.writeBlock(w.data.buf)
	w.index.add(w.data.first, appendHandle(nil, h))
	w.data.reset()
	if len(w.index.buf) >= w.blockSize {
		w.flushIndex()
	}
}

func (w *Writer) flushIndex() {
	if w.index.n == 0 {
		return
	}
	h := w.writeBlock(w.index.buf)
	w.top = append(w.top, topEntry{first: bytes.Clone(w.index.first), h: h})
	w.index.reset()
}

func (w *Writer) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

// Finish writes the indexes and the meta, syncs the file and renames it into
// place. It returns the meta of the segment.
func (w *Writer) Finish() (Meta, error) {
	if w.err != nil {
		w.Abort()
		return Meta{}, w.err
	}
	if w.done {
		return Meta{}, errors.New("checkpoint: writer is finished")
	}
	w.flushData()
	w.flushIndex()
	w.meta.Entries = w.entries
	w.meta.Checksum = w.sum.Sum32()

	top := encodeTop(w.top)
	meta, err := json.Marshal(w.meta)
	if err != nil {
		w.fail(err)
	}
	crc := crc32.Update(crc32.Checksum(top, castagnoli), castagnoli, meta)
	for _, b := range [][]byte{top, meta, footer{topOffset: w.offset, topLength: uint32(len(top)), metaLen: uint32(len(meta)), crc: crc}.encode()} {
		if _, err := w.w.Write(b); err != nil {
			w.fail(err)
		}
	}
	if w.err == nil {
		w.fail(w.w.Flush())
	}
	if w.err == nil {
		w.fail(w.f.Sync())
	}
	if w.err == nil {
		w.fail(w.f.Close())
		w.f = nil
	}
	if w.err == nil && !w.replace {
		if _, err := os.Stat(w.path); err == nil {
			w.fail(fmt.Errorf("%w: %d", ErrSegmentExists, w.meta.ID))
		}
	}
	if w.err == nil {
		w.fail(os.Rename(w.tmpPath, w.path))
	}
	if w.err != nil {
		w.Abort()
		return Meta{}, w.err
	}
	w.done = true
	if err := syncDir(w.dir); err != nil {
		return Meta{}, err
	}
	if w.onFinish != nil {
		if err := w.onFinish(w.path); err != nil {
			return Meta{}, err
		}
	}
	return w.meta, nil
}

// Abort drops the segment.
func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	os.Remove(w.tmpPath)
}

// syncDir flushes the directory entries, e.g. a renamed file, to disk.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
