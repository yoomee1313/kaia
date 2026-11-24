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

package balsync

import (
	"context"
	"fmt"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/hashchain"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/datasync/downloader"
)

// A node that syncs by applying BALs has the downloader fetch the blocks up to
// the last block of the signed BAL hash chain with their receipts and BALs,
// and insert them without executing them: the BAL of a block is checked
// against the hashes of its segment, and the seal, receipts and state root of
// the block as the chain inserts it. Before Amber a header does not
// commit the BAL of its block, so the blocks after the last block of the chain
// are executed.

var _ downloader.BALs = (*Sync)(nil)

// Applies reports whether the node inserts the block of header by applying
// its BAL: the block is covered by the hash chain and a peer holds its BAL.
func (s *Sync) Applies(header *types.Header) bool {
	n := header.Number.Uint64()
	if s == nil || !s.applyBALs || s.hashChain == nil || n == 0 || !s.hashChain.Covers(n) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.rng != nil && p.rng.holdsBALs(n, n) {
			return true
		}
	}
	return false
}

// Holds reports whether peer id holds a part of block number by its announced
// history range. A peer before kaia/69 holds every body and receipt and no
// BAL.
func (s *Sync) Holds(id string, part downloader.Part, number uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[id]
	if p == nil || p.rng == nil {
		return part != downloader.PartBAL
	}
	switch part {
	case downloader.PartBody:
		return p.rng.Body <= number
	case downloader.PartReceipts:
		return p.rng.Receipt <= number
	default:
		return p.rng.holdsBALs(number, number)
	}
}

// RequestBALs asks peer id for the BALs of the blocks with hashes.
func (s *Sync) RequestBALs(id string, hashes []common.Hash) error {
	s.mu.Lock()
	p := s.peers[id]
	s.mu.Unlock()
	if p == nil {
		return errBALSyncGone
	}
	return p.conn.Send(BALsRequestMsg, hashes)
}

// DeliverBALsTo hands the BALs peers send to deliver.
func (s *Sync) DeliverBALsTo(deliver func(id string, lists [][]byte) error) {
	s.deliverBALs = deliver
}

// InsertChain checks the BALs of blocks against the hash chain and inserts the
// blocks by applying them.
func (s *Sync) InsertChain(ctx context.Context, blocks types.Blocks, receipts []types.Receipts, lists [][]byte) (int, error) {
	bc, ok := ChainOf(s.chain)
	if !ok {
		return 0, fmt.Errorf("the chain does not insert blocks with their block access lists")
	}
	batch := make(types.Blocks, len(blocks))
	for i, b := range blocks {
		n := b.NumberU64()
		hashes, from, err := s.segmentHashes(ctx, hashchain.Segment(n))
		if err != nil {
			return i, err
		}
		if crypto.Keccak256Hash(lists[i]) != hashes[n-from] {
			return i, fmt.Errorf("%w: block %d", downloader.ErrBALMismatch, n)
		}
		batch[i] = b.WithBAL(lists[i])
	}
	return bc.InsertChainContext(ctx, batch, receipts)
}

// segmentHashes returns the hashes of the BALs of segment k of the hash chain,
// fetched from a peer and checked against the chain, and the first block of
// the segment.
func (s *Sync) segmentHashes(ctx context.Context, k uint64) ([]common.Hash, uint64, error) {
	chain := s.hashChain
	from, to := chain.SegmentBlocks(k)
	if v, ok := s.verified.Get(k); ok {
		return v.([]common.Hash), from, nil
	}
	var hashes []common.Hash
	err := s.fetch(ctx, fmt.Sprintf("BAL hashes of segment %d", k),
		func(r *HistoryRange) bool { return r.holdsBALs(max(from, 1), to) },
		func(p *balPeer) error {
			res, err := s.request(ctx, p, SegmentHashesRequestMsg, func(id uint64) interface{} {
				return &segmentHashesRequest{ID: id, Segment: k, Last: chain.Last}
			})
			if err != nil {
				return err
			}
			if hashes = res.(*segmentHashesResponse).Hashes; len(hashes) == 0 {
				return fmt.Errorf("%w: segment %d", errBALSyncNoPeer, k)
			}
			if err := chain.VerifySegment(k, hashes); err != nil {
				return fmt.Errorf("%w: %v", errBALSyncMismatch, err)
			}
			return nil
		})
	if err != nil {
		return nil, 0, err
	}
	s.verified.Add(k, hashes)
	return hashes, from, nil
}
