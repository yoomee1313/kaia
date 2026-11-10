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

package blockchain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
)

// From the Amber hardfork the first block of each quarter carries
// Header.CumulativeRoot, a commitment to the hashes of the earlier headers
// that every node recomputes from its own headers.
//
// Quarter boundary: 00:00:00 UTC on January 1, April 1, July 1 and October 1.
// A block is the first of a quarter if its timestamp (Header.Time, Unix
// seconds; TimeFoS is ignored) is at or after a boundary and the timestamp of
// its parent is before it: parent.Time < boundary <= header.Time. A block whose
// timestamp is exactly a boundary is the first of the new quarter.
//
// Segment k holds blocks [k*8192, (k+1)*8192). Its root E_k is the root of the
// binary Merkle tree of depth 13 whose leaves are the hashes of the 8192
// blocks in order, with parent = keccak256(left || right), as in the BAL hash
// chain.
//
// The cumulative root of block N covers the K = (N-1)/8192 segments before the
// last multiple of 8192 below N, blocks [0, K*8192). It is a Merkle mountain
// range over E_0 .. E_{K-1}: K is split into powers of two from the largest,
// each peak is the root of the perfect tree, parent = keccak256(left || right),
// over the next 2^h roots in order, and
//
//	CumulativeRoot = keccak256(BE64(K) || peak_1 || ... || peak_m)
//
// with the peaks from the oldest. With K = 0 it is keccak256(BE64(0)). The
// pre-Amber BAL hash chain root is not part of it.

const (
	HeaderSegmentSize  = params.HeaderSegmentSize
	HeaderSegmentDepth = 13
)

var (
	ErrMissingCumulativeRoot    = errors.New("missing cumulative root in the first block of a quarter")
	ErrUnexpectedCumulativeRoot = errors.New("cumulative root outside the first block of a quarter")
	ErrInvalidCumulativeRoot    = errors.New("invalid cumulative root")

	// ErrCumulativeRootUnavailable is a failure of this node, which lacks the
	// headers to compute the root; it says nothing about the block.
	ErrCumulativeRootUnavailable = errors.New("cumulative root cannot be computed from the local headers")

	headerSegmentRootPrefix = []byte("headerSegmentRoot")
	headerSegmentCountKey   = []byte("headerSegmentRootCount")
)

// quarterOf numbers the UTC calendar quarter of a timestamp.
func quarterOf(timestamp uint64) int64 {
	t := time.Unix(int64(timestamp), 0).UTC()
	return int64(t.Year())*4 + int64(t.Month()-1)/3
}

// IsFirstBlockOfQuarter reports whether a block with the given timestamp, whose
// parent has parentTime, is the first block of a quarter.
func IsFirstBlockOfQuarter(parentTime, blockTime uint64) bool {
	return parentTime < blockTime && quarterOf(parentTime) < quarterOf(blockTime)
}

// CumulativeSegments returns K, the number of segments the cumulative root of
// block number covers.
func CumulativeSegments(number uint64) uint64 {
	if number == 0 {
		return 0
	}
	return (number - 1) / HeaderSegmentSize
}

// merkleRoot returns the root of the perfect binary tree over leaves, whose
// length is a power of two.
func merkleRoot(leaves []common.Hash) common.Hash {
	level := append([]common.Hash(nil), leaves...)
	for len(level) > 1 {
		for i := range len(level) / 2 {
			level[i] = crypto.Keccak256Hash(level[2*i][:], level[2*i+1][:])
		}
		level = level[:len(level)/2]
	}
	return level[0]
}

// HeaderSegmentRoot returns E_k of the hashes of the 8192 blocks of a segment.
func HeaderSegmentRoot(hashes []common.Hash) (common.Hash, error) {
	if len(hashes) != HeaderSegmentSize {
		return common.Hash{}, fmt.Errorf("header segment of %d hashes, want %d", len(hashes), HeaderSegmentSize)
	}
	return merkleRoot(hashes), nil
}

// CumulativeRoot returns the Merkle mountain range root over the segment roots.
func CumulativeRoot(roots []common.Hash) common.Hash {
	buf := binary.BigEndian.AppendUint64(nil, uint64(len(roots)))
	for rest := roots; len(rest) > 0; {
		size := 1
		for size*2 <= len(rest) {
			size *= 2
		}
		peak := merkleRoot(rest[:size])
		buf = append(buf, peak[:]...)
		rest = rest[size:]
	}
	return crypto.Keccak256Hash(buf)
}

// cumulativeRootOf computes the cumulative root of block number from the
// hashes of the earlier blocks, without stored segment roots.
func cumulativeRootOf(number uint64, hashOf func(uint64) common.Hash) (common.Hash, error) {
	s := &headerSegmentRoots{db: database.NewMemDB()}
	if err := s.fill(CumulativeSegments(number), hashOf, nil); err != nil {
		return common.Hash{}, err
	}
	return CumulativeRoot(s.roots), nil
}

// VerifyHeaderRange checks that headers are the ones a cumulative root
// commits to, as a node does with headers it got from an archive or a
// snapshot instead of syncing them. headers must be the consecutive headers of
// whole segments, from block first*8192, linked by their parent hashes.
// segments is E_0 .. E_{K-1} of the root, whose entries for the segments of
// headers are recomputed and must match; the others are bound by the root.
func VerifyHeaderRange(root common.Hash, segments []common.Hash, first uint64, headers []*types.Header) error {
	if len(headers)%HeaderSegmentSize != 0 {
		return fmt.Errorf("%d headers are not whole segments", len(headers))
	}
	n := uint64(len(headers) / HeaderSegmentSize)
	if first+n > uint64(len(segments)) {
		return fmt.Errorf("segments %d-%d are beyond the %d of the root", first, first+n-1, len(segments))
	}
	if got := CumulativeRoot(segments); got != root {
		return fmt.Errorf("%w: segment roots give %x, want %x", ErrInvalidCumulativeRoot, got, root)
	}
	hashes := make([]common.Hash, HeaderSegmentSize)
	for k := range n {
		for i := range hashes {
			j := k*HeaderSegmentSize + uint64(i)
			h := headers[j]
			number := first*HeaderSegmentSize + j
			if h.Number == nil || !h.Number.IsUint64() || h.Number.Uint64() != number {
				return fmt.Errorf("header %d has number %v, want %d", j, h.Number, number)
			}
			if j > 0 && h.ParentHash != hashes[(i+HeaderSegmentSize-1)%HeaderSegmentSize] {
				return fmt.Errorf("header %d does not link to its parent", number)
			}
			hashes[i] = h.Hash()
		}
		got, err := HeaderSegmentRoot(hashes)
		if err != nil {
			return err
		}
		if want := segments[first+k]; got != want {
			return fmt.Errorf("%w: segment %d has root %x, want %x", ErrInvalidCumulativeRoot, first+k, got, want)
		}
	}
	return nil
}

// headerSegmentRoots keeps E_0 .. E_{n-1} in the misc database and in memory.
// They are derived from the canonical headers, so a node fills them as its
// header chain grows; they are dropped when the chain is rewound below them.
type headerSegmentRoots struct {
	mu    sync.Mutex
	db    database.Database
	roots []common.Hash
}

func headerSegmentRootKey(k uint64) []byte {
	return binary.BigEndian.AppendUint64(append([]byte(nil), headerSegmentRootPrefix...), k)
}

func loadHeaderSegmentRoots(db database.Database) (*headerSegmentRoots, error) {
	s := &headerSegmentRoots{db: db}
	enc, err := db.Get(headerSegmentCountKey)
	if err != nil || len(enc) != 8 {
		return s, nil // none stored
	}
	count := binary.BigEndian.Uint64(enc)
	for k := range count {
		v, err := db.Get(headerSegmentRootKey(k))
		if err != nil || len(v) != common.HashLength {
			logger.Warn("Header segment roots end early, the rest is filled again", "stored", count, "read", k)
			break
		}
		s.roots = append(s.roots, common.BytesToHash(v))
	}
	return s, nil
}

// fill computes the missing roots of the first n segments from hashOf, which
// returns the canonical hash of a block or the zero hash if it is unknown. It
// stops early, without an error, once stop is closed.
func (s *headerSegmentRoots) fill(n uint64, hashOf func(uint64) common.Hash, stop <-chan struct{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	hashes := make([]common.Hash, HeaderSegmentSize)
	for k := uint64(len(s.roots)); k < n; k++ {
		select {
		case <-stop:
			return nil
		default:
		}
		for i := range hashes {
			number := k*HeaderSegmentSize + uint64(i)
			if hashes[i] = hashOf(number); hashes[i] == (common.Hash{}) {
				return fmt.Errorf("header segment %d: no canonical hash of block %d", k, number)
			}
		}
		root, err := HeaderSegmentRoot(hashes)
		if err != nil {
			return err
		}
		if err := s.db.Put(headerSegmentRootKey(k), root[:]); err != nil {
			return err
		}
		if err := s.db.Put(headerSegmentCountKey, binary.BigEndian.AppendUint64(nil, k+1)); err != nil {
			return err
		}
		s.roots = append(s.roots, root)
	}
	return nil
}

// truncate keeps the roots of the first n segments only.
func (s *headerSegmentRoots) truncate(n uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if uint64(len(s.roots)) <= n {
		return
	}
	s.roots = s.roots[:n]
	if err := s.db.Put(headerSegmentCountKey, binary.BigEndian.AppendUint64(nil, n)); err != nil {
		logger.Error("Failed to truncate the header segment roots", "segments", n, "err", err)
	}
}

// first returns a copy of the first n roots, which must be filled.
func (s *headerSegmentRoots) first(n uint64) []common.Hash {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]common.Hash(nil), s.roots[:n]...)
}

// HeaderSegmentRoots returns the number of segments whose root is filled.
func (bc *BlockChain) HeaderSegmentRoots() uint64 {
	bc.segmentRoots.mu.Lock()
	defer bc.segmentRoots.mu.Unlock()
	return uint64(len(bc.segmentRoots.roots))
}

// fillHeaderSegmentRoots fills the roots of every segment the canonical header
// chain completes.
func (bc *BlockChain) fillHeaderSegmentRoots() error {
	return bc.segmentRoots.fill((bc.CurrentHeader().Number.Uint64()+1)/HeaderSegmentSize, bc.db.ReadCanonicalHash, bc.quit)
}

// cumulativeRootAt returns the cumulative root of block number, whose parent
// must be canonical.
func (bc *BlockChain) cumulativeRootAt(number uint64) (common.Hash, error) {
	k := CumulativeSegments(number)
	if err := bc.segmentRoots.fill(k, bc.db.ReadCanonicalHash, nil); err != nil {
		return common.Hash{}, err
	}
	return CumulativeRoot(bc.segmentRoots.first(k)), nil
}

// setCumulativeRoot sets the cumulative root of a header this node builds.
func (bc *BlockChain) setCumulativeRoot(header, parent *types.Header) error {
	header.CumulativeRoot = nil
	if !bc.chainConfig.IsAmberForkEnabled(header.Number) || !IsFirstBlockOfQuarter(parent.Time.Uint64(), header.Time.Uint64()) {
		return nil
	}
	root, err := bc.cumulativeRootAt(header.Number.Uint64())
	if err != nil {
		return err
	}
	header.CumulativeRoot = &root
	return nil
}

// verifyCumulativeRoot checks that the header has a cumulative root if and
// only if it is the first block of a quarter from Amber, and that it is the
// one of this node's headers.
func (bc *BlockChain) verifyCumulativeRoot(header, parent *types.Header) error {
	first := bc.chainConfig.IsAmberForkEnabled(header.Number) && IsFirstBlockOfQuarter(parent.Time.Uint64(), header.Time.Uint64())
	switch {
	case !first && header.CumulativeRoot != nil:
		return ErrUnexpectedCumulativeRoot
	case !first:
		return nil
	case header.CumulativeRoot == nil:
		return ErrMissingCumulativeRoot
	}
	if bc.db.ReadCanonicalHash(parent.Number.Uint64()) != parent.Hash() {
		return fmt.Errorf("%w: parent %d is not canonical", ErrCumulativeRootUnavailable, parent.Number.Uint64())
	}
	root, err := bc.cumulativeRootAt(header.Number.Uint64())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCumulativeRootUnavailable, err)
	}
	if root != *header.CumulativeRoot {
		return fmt.Errorf("%w (remote: %x local: %x)", ErrInvalidCumulativeRoot, *header.CumulativeRoot, root)
	}
	return nil
}
