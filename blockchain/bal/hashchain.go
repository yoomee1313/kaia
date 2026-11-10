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

package bal

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/storage/database"
)

// The pre-Amber BAL hash chain lets a node check block access lists of blocks
// whose headers do not commit one, like lists received from another node.
//
// The hash of block i is h_i = Hash() of its list; block 0, the genesis block,
// is not executed and has the empty list. Blocks are grouped into segments of
// SegmentSize blocks: segment k holds blocks k*SegmentSize to
// (k+1)*SegmentSize-1, and its root E'_k is the root of a binary Merkle tree
// of depth SegmentDepth over the hashes of its blocks in order. A parent node
// is keccak256(left || right). The leaves after the last block of the chain,
// in its last segment, are zero hashes, so every segment root has the same
// shape and a block is proven by SegmentDepth sibling hashes.
//
// The chain is the list of segment roots [E'_0, E'_1, ...] up to its last
// block. Its root R_pre is keccak256(L || last), where last is the last block
// as a big endian uint64 and L is the root of a binary Merkle tree over the
// segment roots padded with zero hashes to a power of two.
const (
	SegmentSize  = 8192
	SegmentDepth = 13 // log2(SegmentSize)
)

var (
	ErrHashChain = errors.New("BAL hash chain mismatch")

	errNotCovered = errors.New("block is not covered by the BAL hash chain")

	hashChainMagic = []byte("KAIABALH")
)

const hashChainVersion = 1

// EmptyListHash is the hash of the empty list, the one of the genesis block.
var EmptyListHash = func() common.Hash {
	h, err := (&access.BlockAccessList{}).Hash()
	if err != nil {
		panic(err)
	}
	return h
}()

func merkleParent(left, right common.Hash) common.Hash {
	return crypto.Keccak256Hash(left[:], right[:])
}

// merkleLevels returns the levels of the tree over leaves, whose number is a
// power of two, from the leaves up to the root.
func merkleLevels(leaves []common.Hash) [][]common.Hash {
	levels := [][]common.Hash{leaves}
	for level := leaves; len(level) > 1; {
		up := make([]common.Hash, len(level)/2)
		for i := range up {
			up[i] = merkleParent(level[2*i], level[2*i+1])
		}
		levels = append(levels, up)
		level = up
	}
	return levels
}

func segmentLeaves(hashes []common.Hash) ([]common.Hash, error) {
	if len(hashes) > SegmentSize {
		return nil, fmt.Errorf("%d hashes in a segment of %d blocks", len(hashes), SegmentSize)
	}
	leaves := make([]common.Hash, SegmentSize)
	copy(leaves, hashes)
	return leaves, nil
}

// SegmentRoot returns the root of a segment whose blocks have the given
// hashes, in order; missing blocks at the end have zero hashes.
func SegmentRoot(hashes []common.Hash) (common.Hash, error) {
	leaves, err := segmentLeaves(hashes)
	if err != nil {
		return common.Hash{}, err
	}
	levels := merkleLevels(leaves)
	return levels[len(levels)-1][0], nil
}

// SegmentProof returns the SegmentDepth sibling hashes, from the leaf up, that
// prove the hash at position i of a segment whose blocks have the given hashes.
func SegmentProof(hashes []common.Hash, i int) ([]common.Hash, error) {
	if i < 0 || i >= len(hashes) {
		return nil, fmt.Errorf("position %d out of %d hashes", i, len(hashes))
	}
	leaves, err := segmentLeaves(hashes)
	if err != nil {
		return nil, err
	}
	levels := merkleLevels(leaves)
	proof := make([]common.Hash, 0, SegmentDepth)
	for _, level := range levels[:SegmentDepth] {
		proof = append(proof, level[i^1])
		i /= 2
	}
	return proof, nil
}

// VerifySegmentProof reports whether proof proves leaf at position i of the
// segment with the given root.
func VerifySegmentProof(root common.Hash, i int, leaf common.Hash, proof []common.Hash) bool {
	if i < 0 || i >= SegmentSize || len(proof) != SegmentDepth {
		return false
	}
	h := leaf
	for _, sibling := range proof {
		if i%2 == 0 {
			h = merkleParent(h, sibling)
		} else {
			h = merkleParent(sibling, h)
		}
		i /= 2
	}
	return h == root
}

// HashChain is the list of segment roots up to block Last.
type HashChain struct {
	Last  uint64
	Roots []common.Hash // Roots[k] is E'_k
}

// BuildHashChain computes the chain up to block last from the hash of every
// block, see HashOf.
func BuildHashChain(last uint64, hash func(number uint64) (common.Hash, error)) (*HashChain, error) {
	c := &HashChain{Last: last}
	hashes := make([]common.Hash, 0, SegmentSize)
	for n := uint64(0); n <= last; n++ {
		h, err := hash(n)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", n, err)
		}
		hashes = append(hashes, h)
		if len(hashes) == SegmentSize || n == last {
			root, err := SegmentRoot(hashes)
			if err != nil {
				return nil, err
			}
			c.Roots = append(c.Roots, root)
			hashes = hashes[:0]
		}
	}
	return c, nil
}

// Root returns R_pre, the root of the chain.
func (c *HashChain) Root() common.Hash {
	size := 1
	for size < len(c.Roots) {
		size *= 2
	}
	leaves := make([]common.Hash, size)
	copy(leaves, c.Roots)
	levels := merkleLevels(leaves)
	return crypto.Keccak256Hash(levels[len(levels)-1][0][:], binary.BigEndian.AppendUint64(nil, c.Last))
}

// Covers reports whether the chain covers a block.
func (c *HashChain) Covers(number uint64) bool { return number <= c.Last }

// Segment returns the segment of a block.
func Segment(number uint64) uint64 { return number / SegmentSize }

// SegmentBlocks returns the first and the last block of segment k that the
// chain covers.
func (c *HashChain) SegmentBlocks(k uint64) (from, to uint64) {
	from = k * SegmentSize
	return from, min(from+SegmentSize-1, c.Last)
}

// VerifySegment checks the hashes of the blocks of segment k, in order, that
// the chain covers.
func (c *HashChain) VerifySegment(k uint64, hashes []common.Hash) error {
	if k >= uint64(len(c.Roots)) {
		return fmt.Errorf("%w: segment %d, last block %d", errNotCovered, k, c.Last)
	}
	if from, to := c.SegmentBlocks(k); uint64(len(hashes)) != to-from+1 {
		return fmt.Errorf("segment %d holds blocks %d-%d, not %d hashes", k, from, to, len(hashes))
	}
	root, err := SegmentRoot(hashes)
	if err != nil {
		return err
	}
	if root != c.Roots[k] {
		return fmt.Errorf("%w: segment %d root %x, chain has %x", ErrHashChain, k, root, c.Roots[k])
	}
	return nil
}

// VerifyBlock checks the list of a block against the chain with a proof from
// BlockProof.
func (c *HashChain) VerifyBlock(number uint64, list *access.BlockAccessList, proof []common.Hash) error {
	if !c.Covers(number) {
		return fmt.Errorf("%w: block %d, last block %d", errNotCovered, number, c.Last)
	}
	h, err := list.Hash()
	if err != nil {
		return err
	}
	if !VerifySegmentProof(c.Roots[Segment(number)], int(number%SegmentSize), h, proof) {
		return fmt.Errorf("%w: block %d", ErrHashChain, number)
	}
	return nil
}

// BlockProof returns the proof of a block for a chain up to block last, from
// the hash of every block of its segment, see HashOf.
func BlockProof(number, last uint64, hash func(number uint64) (common.Hash, error)) ([]common.Hash, error) {
	if number > last {
		return nil, fmt.Errorf("%w: block %d, last block %d", errNotCovered, number, last)
	}
	from := Segment(number) * SegmentSize
	to := min(from+SegmentSize-1, last)
	hashes := make([]common.Hash, 0, to-from+1)
	for n := from; n <= to; n++ {
		h, err := hash(n)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", n, err)
		}
		hashes = append(hashes, h)
	}
	return SegmentProof(hashes, int(number-from))
}

// HashOf returns the hash of a canonical block of db, the keccak256 of its
// stored writes, or EmptyListHash for the genesis block.
func HashOf(db database.DBManager, number uint64) (common.Hash, error) {
	if number == 0 {
		return EmptyListHash, nil
	}
	enc, err := ReadRaw(db, number)
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash(enc), nil
}

// The chain file is "KAIABALH", a version byte, the segment size as a big
// endian uint32, the last block as a big endian uint64 and the segment roots
// of 32 bytes each.
const hashChainHeaderSize = 8 + 1 + 4 + 8

// WriteTo writes the chain file.
func (c *HashChain) WriteTo(w io.Writer) (int64, error) {
	buf := make([]byte, 0, hashChainHeaderSize+32*len(c.Roots))
	buf = append(buf, hashChainMagic...)
	buf = append(buf, hashChainVersion)
	buf = binary.BigEndian.AppendUint32(buf, SegmentSize)
	buf = binary.BigEndian.AppendUint64(buf, c.Last)
	for _, root := range c.Roots {
		buf = append(buf, root[:]...)
	}
	n, err := w.Write(buf)
	return int64(n), err
}

// ReadHashChain reads a chain file.
func ReadHashChain(r io.Reader) (*HashChain, error) {
	enc, err := io.ReadAll(bufio.NewReader(r))
	if err != nil {
		return nil, err
	}
	if len(enc) < hashChainHeaderSize || !bytes.Equal(enc[:8], hashChainMagic) {
		return nil, errors.New("not a BAL hash chain file")
	}
	if enc[8] != hashChainVersion {
		return nil, fmt.Errorf("BAL hash chain file version %d", enc[8])
	}
	if size := binary.BigEndian.Uint32(enc[9:]); size != SegmentSize {
		return nil, fmt.Errorf("BAL hash chain of %d-block segments", size)
	}
	c := &HashChain{Last: binary.BigEndian.Uint64(enc[13:])}
	roots := enc[hashChainHeaderSize:]
	if want := (c.Last/SegmentSize + 1) * 32; uint64(len(roots)) != want {
		return nil, fmt.Errorf("BAL hash chain up to block %d has %d bytes of roots, not %d", c.Last, len(roots), want)
	}
	for i := 0; i < len(roots); i += 32 {
		c.Roots = append(c.Roots, common.BytesToHash(roots[i:i+32]))
	}
	return c, nil
}

// LoadHashChain reads the chain file at path.
func LoadHashChain(path string) (*HashChain, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadHashChain(f)
}
