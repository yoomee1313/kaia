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
	"math/big"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

func unix(t *testing.T, s string) uint64 {
	tm, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return uint64(tm.Unix())
}

// The quarter starts at 00:00:00 UTC, and a block at that second is the first
// of the new quarter.
func TestIsFirstBlockOfQuarter(t *testing.T) {
	for _, c := range []struct {
		parent, block string
		want          bool
	}{
		{"2026-03-31T23:59:59Z", "2026-04-01T00:00:00Z", true},
		{"2026-03-31T23:59:55Z", "2026-04-01T00:00:05Z", true},
		{"2026-04-01T00:00:00Z", "2026-04-01T00:00:01Z", false},
		{"2026-06-30T23:59:59Z", "2026-07-01T00:00:00Z", true},
		{"2026-09-30T23:59:59Z", "2026-10-01T00:00:00Z", true},
		{"2026-12-31T23:59:59Z", "2027-01-01T00:00:00Z", true},
		{"2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z", false}, // a month, not a quarter
		{"2026-05-01T00:00:00Z", "2026-05-01T00:00:00Z", false}, // not after the parent
		// Midnight in another zone is not a boundary.
		{"2026-03-31T14:59:59Z", "2026-04-01T00:00:00+09:00", false},
	} {
		require.Equal(t, c.want, IsFirstBlockOfQuarter(unix(t, c.parent), unix(t, c.block)), "%s -> %s", c.parent, c.block)
	}
}

func TestCumulativeSegments(t *testing.T) {
	for number, want := range map[uint64]uint64{0: 0, 1: 0, 8192: 0, 8193: 1, 16384: 1, 16385: 2} {
		require.Equal(t, want, CumulativeSegments(number), "block %d", number)
	}
}

func testHash(i uint64) common.Hash {
	return crypto.Keccak256Hash(binary.BigEndian.AppendUint64(nil, i))
}

func naiveRoot(leaves []common.Hash) common.Hash {
	if len(leaves) == 1 {
		return leaves[0]
	}
	l, r := naiveRoot(leaves[:len(leaves)/2]), naiveRoot(leaves[len(leaves)/2:])
	return crypto.Keccak256Hash(l[:], r[:])
}

func TestHeaderSegmentRoot(t *testing.T) {
	hashes := make([]common.Hash, HeaderSegmentSize)
	for i := range hashes {
		hashes[i] = testHash(uint64(i))
	}
	root, err := HeaderSegmentRoot(hashes)
	require.NoError(t, err)
	require.Equal(t, naiveRoot(hashes), root)
	require.Equal(t, 1<<HeaderSegmentDepth, HeaderSegmentSize)

	_, err = HeaderSegmentRoot(hashes[:HeaderSegmentSize-1])
	require.Error(t, err)
}

func TestCumulativeRoot(t *testing.T) {
	e := make([]common.Hash, 5)
	for i := range e {
		e[i] = testHash(100 + uint64(i))
	}
	size := func(k uint64) []byte { return binary.BigEndian.AppendUint64(nil, k) }
	h := func(a, b common.Hash) common.Hash { return crypto.Keccak256Hash(a[:], b[:]) }

	require.Equal(t, crypto.Keccak256Hash(size(0)), CumulativeRoot(nil))
	require.Equal(t, crypto.Keccak256Hash(size(1), e[0][:]), CumulativeRoot(e[:1]))
	require.Equal(t, crypto.Keccak256Hash(size(2), h(e[0], e[1]).Bytes()), CumulativeRoot(e[:2]))
	require.Equal(t, crypto.Keccak256Hash(size(3), h(e[0], e[1]).Bytes(), e[2][:]), CumulativeRoot(e[:3]))
	four := h(h(e[0], e[1]), h(e[2], e[3]))
	require.Equal(t, crypto.Keccak256Hash(size(5), four[:], e[4][:]), CumulativeRoot(e))
}

// newSegmentChain returns a chain from the Amber hardfork whose canonical
// hashes are made up up to block last, with the header of block last.
func newSegmentChain(t *testing.T, last uint64) (*BlockChain, *types.Header) {
	env := newBALTestEnv(t, "prague", 1)
	env.config.AmberCompatibleBlock = big.NewInt(0)
	chain, err := NewBlockChain(env.db, nil, env.config, env.sealer, vm.Config{})
	require.NoError(t, err)
	t.Cleanup(chain.Stop)
	for n := uint64(1); n < last; n++ {
		env.db.WriteCanonicalHash(testHash(n), n)
	}
	parent := &types.Header{Number: new(big.Int).SetUint64(last), Time: big.NewInt(0), ParentHash: testHash(last - 1)}
	env.db.WriteCanonicalHash(parent.Hash(), last)
	return chain, parent
}

// The cumulative root of a block covers the segments before the last multiple
// of 8192 below it, from the node's canonical hashes.
func TestVerifyCumulativeRoot(t *testing.T) {
	chain, parent := newSegmentChain(t, 2*HeaderSegmentSize)
	number := 2*HeaderSegmentSize + 1 // two whole segments before it
	hashOf := func(n uint64) common.Hash { return chain.db.ReadCanonicalHash(n) }
	segment := func(k uint64) common.Hash {
		hashes := make([]common.Hash, HeaderSegmentSize)
		for i := range hashes {
			hashes[i] = hashOf(k*HeaderSegmentSize + uint64(i))
		}
		return naiveRoot(hashes)
	}
	want := CumulativeRoot([]common.Hash{segment(0), segment(1)})
	got, err := cumulativeRootOf(uint64(number), hashOf)
	require.NoError(t, err)
	require.Equal(t, want, got)

	boundary := unix(t, "2026-10-01T00:00:00Z")
	parent.Time = new(big.Int).SetUint64(boundary - 1)
	chain.db.WriteCanonicalHash(parent.Hash(), parent.Number.Uint64())
	header := &types.Header{Number: big.NewInt(int64(number)), ParentHash: parent.Hash(), Time: new(big.Int).SetUint64(boundary)}

	// The producer sets it, and the check takes it.
	require.NoError(t, chain.setCumulativeRoot(header, parent))
	require.Equal(t, want, *header.CumulativeRoot)
	require.NoError(t, chain.verifyCumulativeRoot(header, parent))
	require.Equal(t, uint64(2), chain.HeaderSegmentRoots())

	wrong := types.CopyHeader(header)
	wrong.CumulativeRoot = &common.Hash{1}
	require.ErrorIs(t, chain.verifyCumulativeRoot(wrong, parent), ErrInvalidCumulativeRoot)
	missing := types.CopyHeader(header)
	missing.CumulativeRoot = nil
	require.ErrorIs(t, chain.verifyCumulativeRoot(missing, parent), ErrMissingCumulativeRoot)
	later := types.CopyHeader(header)
	later.Time = new(big.Int).SetUint64(boundary + 10)
	parent.Time = new(big.Int).SetUint64(boundary)
	require.ErrorIs(t, chain.verifyCumulativeRoot(later, parent), ErrUnexpectedCumulativeRoot)
	later.CumulativeRoot = nil
	require.NoError(t, chain.verifyCumulativeRoot(later, parent))

	// The roots are stored, and a new chain on the database reads them.
	roots, err := loadHeaderSegmentRoots(chain.db.GetMiscDB())
	require.NoError(t, err)
	require.Equal(t, []common.Hash{segment(0), segment(1)}, roots.roots)
	roots.truncate(1)
	reloaded, err := loadHeaderSegmentRoots(chain.db.GetMiscDB())
	require.NoError(t, err)
	require.Equal(t, []common.Hash{segment(0)}, reloaded.roots)
}

// A node without the headers of a segment cannot check the root, which is
// not a fault of the block.
func TestCumulativeRootUnavailable(t *testing.T) {
	chain, parent := newSegmentChain(t, HeaderSegmentSize+1)
	chain.db.DeleteCanonicalHash(100)
	boundary := unix(t, "2026-10-01T00:00:00Z")
	parent.Time = new(big.Int).SetUint64(boundary - 1)
	chain.db.WriteCanonicalHash(parent.Hash(), parent.Number.Uint64())
	root := common.Hash{1}
	header := &types.Header{Number: big.NewInt(HeaderSegmentSize + 2), ParentHash: parent.Hash(), Time: new(big.Int).SetUint64(boundary), CumulativeRoot: &root}
	err := chain.verifyCumulativeRoot(header, parent)
	require.ErrorIs(t, err, ErrCumulativeRootUnavailable)
	require.NotErrorIs(t, err, ErrInvalidCumulativeRoot)

	// Nor if its parent is not canonical.
	other := types.CopyHeader(parent)
	other.Extra = []byte{1}
	header.ParentHash = other.Hash()
	require.ErrorIs(t, chain.verifyCumulativeRoot(header, other), ErrCumulativeRootUnavailable)
}

// newQuarterEnv returns an env whose block 3 is the first block of a quarter,
// with Amber from amber.
func newQuarterEnv(t *testing.T, amber int64) *balTestEnv {
	env := newBALTestEnv(t, "prague", 2)
	env.config.AmberCompatibleBlock = big.NewInt(amber)
	env.db = database.NewMemoryDBManager()
	env.gspec.Timestamp = unix(t, "2026-10-01T00:00:00Z") - 25 // blocks are 10 s apart
	env.genesis = env.gspec.MustCommit(env.db)
	return env
}

// The first block of a quarter from Amber carries the cumulative root, and a
// chain rejects a block that lacks it, has a wrong one, or has one elsewhere.
func TestInsertChainCumulativeRoot(t *testing.T) {
	env := newQuarterEnv(t, 0)
	chain, blocks := env.generate(5, amberTestTxs(env))
	for i, block := range blocks {
		if i == 2 {
			require.NotNil(t, block.Header().CumulativeRoot)
			require.Equal(t, CumulativeRoot(nil), *block.Header().CumulativeRoot)
		} else {
			require.Nil(t, block.Header().CumulativeRoot, "block %d", block.NumberU64())
		}
	}
	require.Equal(t, blocks[4].Hash(), chain.CurrentBlock().Hash())

	with := func(block *types.Block, root *common.Hash) *types.Block {
		header := block.Header()
		header.CumulativeRoot = root
		return types.NewBlockWithHeader(header).WithBody(block.Transactions())
	}
	insert := func(chain []*types.Block) error {
		imported := newImportChain(t, env)
		_, err := imported.InsertChain(chain)
		return err
	}
	require.ErrorIs(t, insert([]*types.Block{blocks[0], blocks[1], with(blocks[2], nil)}), ErrMissingCumulativeRoot)
	require.ErrorIs(t, insert([]*types.Block{blocks[0], blocks[1], with(blocks[2], &common.Hash{1})}), ErrInvalidCumulativeRoot)
	root := CumulativeRoot(nil)
	require.ErrorIs(t, insert([]*types.Block{blocks[0], with(blocks[1], &root)}), ErrUnexpectedCumulativeRoot)

	// Before Amber no block has one.
	env = newQuarterEnv(t, HeaderSegmentSize)
	_, blocks = env.generate(4, nil)
	for _, block := range blocks {
		require.Nil(t, block.Header().CumulativeRoot)
	}
	require.ErrorIs(t, insert([]*types.Block{blocks[0], blocks[1], with(blocks[2], &root)}), ErrUnexpectedCumulativeRoot)
}

// A range of whole segments of headers is checked against a cumulative root
// and the segment roots it covers.
func TestVerifyHeaderRange(t *testing.T) {
	headers := make([]*types.Header, 3*HeaderSegmentSize)
	for i := range headers {
		h := &types.Header{Number: big.NewInt(int64(i)), Time: big.NewInt(int64(i))}
		if i > 0 {
			h.ParentHash = headers[i-1].Hash()
		}
		headers[i] = h
	}
	segments := make([]common.Hash, 3)
	for k := range segments {
		hashes := make([]common.Hash, HeaderSegmentSize)
		for i := range hashes {
			hashes[i] = headers[k*HeaderSegmentSize+i].Hash()
		}
		segments[k] = naiveRoot(hashes)
	}
	root := CumulativeRoot(segments)
	seg := func(k int) []*types.Header { return headers[k*HeaderSegmentSize : (k+1)*HeaderSegmentSize] }

	require.NoError(t, VerifyHeaderRange(root, segments, 0, headers))
	require.NoError(t, VerifyHeaderRange(root, segments, 1, seg(1)))
	require.NoError(t, VerifyHeaderRange(root, segments, 2, seg(2)))

	// The roots of the other segments are bound by the root.
	other := append([]common.Hash(nil), segments...)
	other[0] = common.Hash{1}
	require.ErrorIs(t, VerifyHeaderRange(root, other, 1, seg(1)), ErrInvalidCumulativeRoot)

	// A changed header changes its segment root, or breaks the links.
	changed := append([]*types.Header(nil), seg(1)...)
	last := types.CopyHeader(changed[HeaderSegmentSize-1])
	last.Extra = []byte{1}
	changed[HeaderSegmentSize-1] = last
	require.ErrorIs(t, VerifyHeaderRange(root, segments, 1, changed), ErrInvalidCumulativeRoot)
	changed = append([]*types.Header(nil), seg(1)...)
	mid := types.CopyHeader(changed[10])
	mid.Extra = []byte{1}
	changed[10] = mid
	require.ErrorContains(t, VerifyHeaderRange(root, segments, 1, changed), "does not link")

	require.Error(t, VerifyHeaderRange(root, segments, 1, seg(1)[1:]), "not whole segments")
	require.Error(t, VerifyHeaderRange(root, segments, 0, seg(1)), "wrong numbers")
	require.Error(t, VerifyHeaderRange(root, segments[:2], 2, seg(2)), "beyond the root")
}
