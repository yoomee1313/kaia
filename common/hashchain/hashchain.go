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

// Package hashchain keeps a signed chain of the hashes of the blocks of a
// network, which lets a node check data of blocks whose headers do not commit
// it, like data received from another node.
//
// The hash of block i is h_i, given by the user of the chain. Blocks are
// grouped into segments of SegmentSize blocks: segment k holds blocks
// k*SegmentSize to (k+1)*SegmentSize-1, and its root E'_k is the root of a
// binary Merkle tree of depth segmentDepth over the hashes of its blocks in
// order. A parent node is keccak256(left || right). The leaves after the last
// block of the chain, in its last segment, are zero hashes, so every segment
// root has the same shape and a block is proven by segmentDepth sibling
// hashes.
//
// The chain is the list of segment roots [E'_0, E'_1, ...] up to its last
// block. Its root R_pre is keccak256(L || last), where last is the last block
// as a big endian uint64 and L is the root of a binary Merkle tree over the
// segment roots padded with zero hashes to a power of two.
//
// A Spec names one kind of chain: the magic of its file, the domain of its
// signature and the npm package it is distributed in.
package hashchain

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
)

// Blocks are grouped into segments of SegmentSize blocks: segment k holds
// blocks k*SegmentSize to (k+1)*SegmentSize-1. The root of a segment is the
// root of a binary Merkle tree of depth segmentDepth over hashes of its blocks
// in order, whose parent nodes are keccak256(left || right) and whose missing
// leaves at the end are zero hashes.
const (
	SegmentSize  = 8192
	segmentDepth = 13 // log2(SegmentSize)
)

// Segment returns the segment of a block.
func Segment(number uint64) uint64 { return number / SegmentSize }

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

// segmentProof returns the SegmentDepth sibling hashes, from the leaf up, that
// prove the hash at position i of a segment whose blocks have the given hashes.
func segmentProof(hashes []common.Hash, i int) ([]common.Hash, error) {
	if i < 0 || i >= len(hashes) {
		return nil, fmt.Errorf("position %d out of %d hashes", i, len(hashes))
	}
	leaves, err := segmentLeaves(hashes)
	if err != nil {
		return nil, err
	}
	levels := merkleLevels(leaves)
	proof := make([]common.Hash, 0, segmentDepth)
	for _, level := range levels[:segmentDepth] {
		proof = append(proof, level[i^1])
		i /= 2
	}
	return proof, nil
}

// verifySegmentProof reports whether proof proves leaf at position i of the
// segment with the given root.
func verifySegmentProof(root common.Hash, i int, leaf common.Hash, proof []common.Hash) bool {
	if i < 0 || i >= SegmentSize || len(proof) != segmentDepth {
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

// merkleRoot returns the root of the tree over leaves, whose number is a power
// of two.
func merkleRoot(leaves []common.Hash) common.Hash {
	levels := merkleLevels(leaves)
	return levels[len(levels)-1][0]
}

var (
	// ErrMismatch is returned for hashes that do not match the chain.
	ErrMismatch = errors.New("hash chain mismatch")
	// ErrSignature is returned for a chain file whose signature does not verify.
	ErrSignature = errors.New("hash chain signature mismatch")

	errNotCovered = errors.New("block is not covered by the hash chain")
)

const fileVersion = 1

// Chain is the list of segment roots up to block Last.
type Chain struct {
	Last  uint64
	Roots []common.Hash // Roots[k] is E'_k
}

// Build computes the chain up to block last from the hash of every block.
func Build(last uint64, hash func(number uint64) (common.Hash, error)) (*Chain, error) {
	c := &Chain{Last: last}
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
func (c *Chain) Root() common.Hash {
	size := 1
	for size < len(c.Roots) {
		size *= 2
	}
	leaves := make([]common.Hash, size)
	copy(leaves, c.Roots)
	return crypto.Keccak256Hash(merkleRoot(leaves).Bytes(), binary.BigEndian.AppendUint64(nil, c.Last))
}

// Covers reports whether the chain covers a block.
func (c *Chain) Covers(number uint64) bool { return number <= c.Last }

// SegmentBlocks returns the first and the last block of segment k that the
// chain covers.
func (c *Chain) SegmentBlocks(k uint64) (from, to uint64) {
	from = k * SegmentSize
	return from, min(from+SegmentSize-1, c.Last)
}

// VerifySegment checks the hashes of the blocks of segment k, in order, that
// the chain covers.
func (c *Chain) VerifySegment(k uint64, hashes []common.Hash) error {
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
		return fmt.Errorf("%w: segment %d root %x, chain has %x", ErrMismatch, k, root, c.Roots[k])
	}
	return nil
}

// VerifyBlock checks the hash of a block against the chain with a proof from
// Proof.
func (c *Chain) VerifyBlock(number uint64, hash common.Hash, proof []common.Hash) error {
	if !c.Covers(number) {
		return fmt.Errorf("%w: block %d, last block %d", errNotCovered, number, c.Last)
	}
	if !verifySegmentProof(c.Roots[Segment(number)], int(number%SegmentSize), hash, proof) {
		return fmt.Errorf("%w: block %d", ErrMismatch, number)
	}
	return nil
}

// Proof returns the proof of a block for a chain up to block last, from the
// hash of every block of its segment.
func Proof(number, last uint64, hash func(number uint64) (common.Hash, error)) ([]common.Hash, error) {
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
	return segmentProof(hashes, int(number-from))
}

// Spec is a kind of chain. Its chains are distributed in the npm package
// Package, which an operator installs with npm; the node does not fetch it.
// The package holds the chain of each network as <genesis hash in hex>.bin,
// in the format of WriteTo, and its ed25519 signature, in hex, as
// <genesis hash in hex>.bin.sig. The signature is over Domain || genesis hash
// || file, so the chain of one network does not verify as the chain of
// another, nor a chain of one kind as one of another.
type Spec struct {
	Name    string // in errors, like "BAL"
	Magic   string // the 8 bytes a chain file starts with
	Domain  string // the domain of the signatures
	Package string // the npm package of the chain files
}

// The chain file is Magic, a version byte, the segment size as a big endian
// uint32, the last block as a big endian uint64 and the segment roots of 32
// bytes each.
const fileHeaderSize = 8 + 1 + 4 + 8

// WriteTo writes the chain file of c.
func (s Spec) WriteTo(w io.Writer, c *Chain) (int64, error) {
	buf := make([]byte, 0, fileHeaderSize+32*len(c.Roots))
	buf = append(buf, s.Magic...)
	buf = append(buf, fileVersion)
	buf = binary.BigEndian.AppendUint32(buf, SegmentSize)
	buf = binary.BigEndian.AppendUint64(buf, c.Last)
	for _, root := range c.Roots {
		buf = append(buf, root[:]...)
	}
	n, err := w.Write(buf)
	return int64(n), err
}

// Read reads a chain file.
func (s Spec) Read(r io.Reader) (*Chain, error) {
	enc, err := io.ReadAll(bufio.NewReader(r))
	if err != nil {
		return nil, err
	}
	if len(enc) < fileHeaderSize || string(enc[:8]) != s.Magic {
		return nil, fmt.Errorf("not a %s hash chain file", s.Name)
	}
	if enc[8] != fileVersion {
		return nil, fmt.Errorf("%s hash chain file version %d", s.Name, enc[8])
	}
	if size := binary.BigEndian.Uint32(enc[9:]); size != SegmentSize {
		return nil, fmt.Errorf("%s hash chain of %d-block segments", s.Name, size)
	}
	c := &Chain{Last: binary.BigEndian.Uint64(enc[13:])}
	roots := enc[fileHeaderSize:]
	if want := (c.Last/SegmentSize + 1) * 32; uint64(len(roots)) != want {
		return nil, fmt.Errorf("%s hash chain up to block %d has %d bytes of roots, not %d", s.Name, c.Last, len(roots), want)
	}
	for i := 0; i < len(roots); i += 32 {
		c.Roots = append(c.Roots, common.BytesToHash(roots[i:i+32]))
	}
	return c, nil
}

// Load reads the chain file at path.
func (s Spec) Load(path string) (*Chain, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return s.Read(f)
}

// FileName returns the name of the chain file of the network with the
// genesis block genesis.
func (s Spec) FileName(genesis common.Hash) string {
	return hex.EncodeToString(genesis[:]) + ".bin"
}

// Message returns what is signed for the chain file of the network with the
// genesis block genesis.
func (s Spec) Message(genesis common.Hash, file []byte) []byte {
	msg := make([]byte, 0, len(s.Domain)+common.HashLength+len(file))
	msg = append(msg, s.Domain...)
	msg = append(msg, genesis[:]...)
	return append(msg, file...)
}

// Verify checks the signature of the chain file of the network with the
// genesis block genesis, and returns the chain.
func (s Spec) Verify(pub ed25519.PublicKey, genesis common.Hash, file, sig []byte) (*Chain, error) {
	if len(pub) != ed25519.PublicKeySize || bytes.Equal(pub, make([]byte, ed25519.PublicKeySize)) {
		return nil, fmt.Errorf("no %s hash chain signing key in this build", s.Name)
	}
	if !ed25519.Verify(pub, s.Message(genesis, file), sig) {
		return nil, fmt.Errorf("%w for genesis %s", ErrSignature, genesis.Hex())
	}
	return s.Read(bytes.NewReader(file))
}

// Path returns the chain file of the network with the genesis block genesis:
// path itself if it is a file, or the file in the package directory path, or
// in path/node_modules/Package.
func (s Spec) Path(path string, genesis common.Hash) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return path, nil
	}
	name := s.FileName(genesis)
	for _, dir := range []string{path, filepath.Join(path, "node_modules", s.Package)} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return filepath.Join(dir, name), nil
		}
	}
	return "", fmt.Errorf("no %s hash chain of genesis %s (%s) in %s", s.Name, genesis.Hex(), name, path)
}

// LoadSigned reads the chain of the network with the genesis block genesis
// from path, a chain file or the directory of the package, and checks its
// signature, in the file next to it with the suffix .sig, with pub.
func (s Spec) LoadSigned(path string, genesis common.Hash, pub ed25519.PublicKey) (*Chain, error) {
	file, err := s.Path(path, genesis)
	if err != nil {
		return nil, err
	}
	enc, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	sigHex, err := os.ReadFile(file + ".sig")
	if err != nil {
		return nil, fmt.Errorf("signature of the %s hash chain: %w", s.Name, err)
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(sigHex)), "0x"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: %s is not a hex ed25519 signature", ErrSignature, file+".sig")
	}
	return s.Verify(pub, genesis, enc, sig)
}
