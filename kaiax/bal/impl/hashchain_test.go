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

package impl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/kaiax/bal"
	"github.com/stretchr/testify/require"
)

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
			proof, err := SegmentProof(hashes, i)
			require.NoError(t, err)
			require.Len(t, proof, SegmentDepth)
			require.Equal(t, 416, len(proof)*common.HashLength)
			require.True(t, VerifySegmentProof(root, i, hashes[i], proof), "position %d", i)

			require.False(t, VerifySegmentProof(root, i, testHash(1<<40), proof), "other leaf")
			require.False(t, VerifySegmentProof(root, i^1, hashes[i], proof), "other position")
			tampered := append([]common.Hash{}, proof...)
			tampered[SegmentDepth-1][0] ^= 1
			require.False(t, VerifySegmentProof(root, i, hashes[i], tampered), "tampered proof")
			require.False(t, VerifySegmentProof(root, i, hashes[i], proof[:SegmentDepth-1]), "short proof")
		}
		_, err = SegmentProof(hashes, len(hashes))
		require.Error(t, err)
	}
}

// TestHashChain builds a chain whose last segment is partial, checks its
// segments and blocks, and reads it back from its file.
func TestHashChain(t *testing.T) {
	last := uint64(2*SegmentSize + 100)
	hash := func(n uint64) (common.Hash, error) { return testHash(n), nil }
	c, err := BuildHashChain(last, hash)
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
	shorter, err := BuildHashChain(last-1, hash)
	require.NoError(t, err)
	require.NotEqual(t, c.Root(), shorter.Root())

	// Tampered hashes, a missing block or another segment are rejected.
	hashes := testHashes(from, to)
	hashes[7] = testHash(1 << 40)
	require.ErrorIs(t, c.VerifySegment(2, hashes), ErrHashChain)
	require.Error(t, c.VerifySegment(2, hashes[:len(hashes)-1]))
	require.ErrorIs(t, c.VerifySegment(1, testHashes(0, SegmentSize-1)), ErrHashChain)
	require.Error(t, c.VerifySegment(3, nil))

	var buf bytes.Buffer
	_, err = c.WriteTo(&buf)
	require.NoError(t, err)
	require.Equal(t, hashChainHeaderSize+3*32, buf.Len())
	read, err := ReadHashChain(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Equal(t, c, read)
	require.Equal(t, c.Root(), read.Root())

	_, err = ReadHashChain(bytes.NewReader(buf.Bytes()[:buf.Len()-1]))
	require.Error(t, err, "truncated")
	bad := append([]byte{}, buf.Bytes()...)
	bad[0] = 'X'
	_, err = ReadHashChain(bytes.NewReader(bad))
	require.Error(t, err, "magic")
}

// TestHashChainBlock proves blocks with real lists, the genesis one included.
func TestHashChainBlock(t *testing.T) {
	lists := make([]*bal.BlockAccessList, 40)
	lists[0] = &bal.BlockAccessList{}
	for i := 1; i < len(lists); i++ {
		builder := bal.NewBuilder()
		builder.SetBalance(common.Address{byte(i)}, 1, big.NewInt(int64(i)))
		builder.ReadSlot(common.Address{0xee}, common.Hash{byte(i)})
		lists[i] = builder.Build()
	}
	hash := func(n uint64) (common.Hash, error) { return lists[n].Hash() }
	last := uint64(len(lists) - 1)
	c, err := BuildHashChain(last, hash)
	require.NoError(t, err)
	genesis, err := hash(0)
	require.NoError(t, err)
	require.Equal(t, EmptyListHash, genesis)

	for _, n := range []uint64{0, 1, 17, last} {
		proof, err := BlockProof(n, last, hash)
		require.NoError(t, err)
		require.NoError(t, c.VerifyBlock(n, lists[n], proof), "block %d", n)
		// The writes alone verify too.
		require.NoError(t, c.VerifyBlock(n, lists[n].Writes(), proof), "block %d", n)
	}
	proof, err := BlockProof(5, last, hash)
	require.NoError(t, err)
	require.ErrorIs(t, c.VerifyBlock(5, lists[6], proof), ErrHashChain)
	require.ErrorIs(t, c.VerifyBlock(6, lists[5], proof), ErrHashChain)
	err = c.VerifyBlock(last+1, lists[5], proof)
	require.True(t, errors.Is(err, errNotCovered))
	_, err = BlockProof(last+1, last, hash)
	require.Error(t, err)
}
