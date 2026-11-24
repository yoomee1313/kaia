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

package hashchain

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/stretchr/testify/require"
)

var testSpec = Spec{Name: "test", Magic: "TESTHASH", Domain: "test-hashchain", Package: "test-hashchain"}

// testHash is a stand-in for the hash of block n.
func testHash(n uint64) common.Hash {
	return crypto.Keccak256Hash(binary.BigEndian.AppendUint64(nil, n))
}

func testHashes(from, to uint64) []common.Hash {
	var hashes []common.Hash
	for n := from; n <= to; n++ {
		hashes = append(hashes, testHash(n))
	}
	return hashes
}

// naiveRoot computes a Merkle root recursively, as the definition reads.
func naiveRoot(leaves []common.Hash) common.Hash {
	if len(leaves) == 1 {
		return leaves[0]
	}
	half := len(leaves) / 2
	l, r := naiveRoot(leaves[:half]), naiveRoot(leaves[half:])
	return crypto.Keccak256Hash(l[:], r[:])
}

func TestSegmentRoot(t *testing.T) {
	full := testHashes(0, SegmentSize-1)
	root, err := SegmentRoot(full)
	require.NoError(t, err)
	require.Equal(t, naiveRoot(full), root)

	// A partial segment has zero leaves after its last block.
	partial := testHashes(0, 99)
	padded := make([]common.Hash, SegmentSize)
	copy(padded, partial)
	root, err = SegmentRoot(partial)
	require.NoError(t, err)
	require.Equal(t, naiveRoot(padded), root)
	require.NotEqual(t, naiveRoot(full), root)

	_, err = SegmentRoot(testHashes(0, SegmentSize))
	require.Error(t, err)
}

func TestSegmentProof(t *testing.T) {
	for _, hashes := range [][]common.Hash{testHashes(0, SegmentSize-1), testHashes(0, 100)} {
		root, err := SegmentRoot(hashes)
		require.NoError(t, err)
		for _, i := range []int{0, 1, 2, 63, len(hashes) / 2, len(hashes) - 1} {
			proof, err := segmentProof(hashes, i)
			require.NoError(t, err)
			require.Len(t, proof, segmentDepth)
			require.Equal(t, 416, len(proof)*common.HashLength)
			require.True(t, verifySegmentProof(root, i, hashes[i], proof), "position %d", i)

			require.False(t, verifySegmentProof(root, i, testHash(1<<40), proof), "other leaf")
			require.False(t, verifySegmentProof(root, i^1, hashes[i], proof), "other position")
			tampered := append([]common.Hash{}, proof...)
			tampered[segmentDepth-1][0] ^= 1
			require.False(t, verifySegmentProof(root, i, hashes[i], tampered), "tampered proof")
			require.False(t, verifySegmentProof(root, i, hashes[i], proof[:segmentDepth-1]), "short proof")
		}
		_, err = segmentProof(hashes, len(hashes))
		require.Error(t, err)
	}
}

// TestHashChain builds a chain whose last segment is partial, checks its
// segments and blocks, and reads it back from its file.
func TestHashChain(t *testing.T) {
	last := uint64(2*SegmentSize + 100)
	hash := func(n uint64) (common.Hash, error) { return testHash(n), nil }
	c, err := Build(last, hash)
	require.NoError(t, err)
	require.Len(t, c.Roots, 3)
	for k := uint64(0); k < 3; k++ {
		from, to := c.SegmentBlocks(k)
		require.Equal(t, k*SegmentSize, from)
		require.NoError(t, c.VerifySegment(k, testHashes(from, to)), "segment %d", k)
	}
	from, to := c.SegmentBlocks(2)
	require.Equal(t, last, to)

	// The root binds the segment roots, in order, and the last block.
	padded := []common.Hash{c.Roots[0], c.Roots[1], c.Roots[2], {}}
	listRoot := naiveRoot(padded)
	require.Equal(t, crypto.Keccak256Hash(listRoot[:], binary.BigEndian.AppendUint64(nil, last)), c.Root())
	shorter, err := Build(last-1, hash)
	require.NoError(t, err)
	require.NotEqual(t, c.Root(), shorter.Root())

	// Tampered hashes, a missing block or another segment are rejected.
	hashes := testHashes(from, to)
	hashes[7] = testHash(1 << 40)
	require.ErrorIs(t, c.VerifySegment(2, hashes), ErrMismatch)
	require.Error(t, c.VerifySegment(2, hashes[:len(hashes)-1]))
	require.ErrorIs(t, c.VerifySegment(1, testHashes(0, SegmentSize-1)), ErrMismatch)
	require.Error(t, c.VerifySegment(3, nil))

	var buf bytes.Buffer
	_, err = testSpec.WriteTo(&buf, c)
	require.NoError(t, err)
	require.Equal(t, fileHeaderSize+3*32, buf.Len())
	read, err := testSpec.Read(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Equal(t, c, read)
	require.Equal(t, c.Root(), read.Root())

	_, err = testSpec.Read(bytes.NewReader(buf.Bytes()[:buf.Len()-1]))
	require.Error(t, err, "truncated")
	bad := append([]byte{}, buf.Bytes()...)
	bad[0] = 'X'
	_, err = testSpec.Read(bytes.NewReader(bad))
	require.Error(t, err, "magic")
}
