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

package downloader

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
)

// Part is a part of a block that the downloader fetches.
type Part int

const (
	PartBody Part = iota
	PartReceipts
	PartBAL
)

// ErrBALMismatch is returned by BALs.InsertChain for a block access list
// that does not match.
var ErrBALMismatch = errors.New("block access list does not match")

// BALs lets the downloader insert blocks by applying their block access lists
// (BALs), fetched with their receipts, instead of executing them.
type BALs interface {
	// Applies reports whether the block of header is inserted by applying
	// its BAL.
	Applies(header *types.Header) bool

	// Holds reports whether peer id holds a part of block number.
	Holds(id string, part Part, number uint64) bool

	// RequestBALs asks peer id for the BALs of the blocks with hashes.
	RequestBALs(id string, hashes []common.Hash) error

	// InsertChain checks the BALs of blocks and inserts the blocks by
	// applying them. On a BAL that does not match, it returns the index of
	// its block and ErrBALMismatch.
	InsertChain(ctx context.Context, blocks types.Blocks, receipts []types.Receipts, lists [][]byte) (int, error)
}

// SetBALs makes the downloader insert the blocks that bals applies by
// applying their BALs.
func (d *Downloader) SetBALs(bals BALs) {
	d.bals = bals
}

// MaxBALFetch is how many BALs a request asks for.
var MaxBALFetch = 256

// fetchBALs iteratively downloads the scheduled BALs, taking any available
// peers, reserving a chunk of BALs for each, waiting for delivery and also
// periodically checking for timeouts.
func (d *Downloader) fetchBALs(from uint64) error {
	logger.Debug("Downloading block access lists", "origin", from)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*balPack)
			return d.queue.DeliverBALs(pack.peerId, pack.lists)
		}
		expire = func() map[string]int { return d.queue.ExpireBALs(d.requestTTL()) }
		fetch  = func(p *peerConnection, req *fetchRequest) error {
			return p.FetchBALs(req, d.bals.RequestBALs)
		}
		capacity = func(p *peerConnection) int { return p.BALCapacity(d.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int, deliveryTime time.Time) {
			p.SetBALsIdle(accepted, deliveryTime)
		}
	)
	err := d.fetchParts(d.balCh, deliver, d.balWakeCh, expire,
		d.queue.PendingBALs, d.queue.InFlightBALs, d.queue.ReserveBALs,
		nil, fetch, d.queue.CancelBALs, capacity, d.peers.BALIdlePeers, setIdle, "bals")

	logger.Debug("Block access list download terminated", "err", err)
	return err
}

// DeliverBALs injects a new batch of BALs received from a remote node.
func (d *Downloader) DeliverBALs(id string, lists [][]byte) error {
	return d.deliver(id, d.balCh, &balPack{id, lists}, balInMeter, balDropMeter)
}

// insertBALResults inserts the blocks of results by applying their BALs. A
// peer that sent a BAL that does not match is dropped.
func (d *Downloader) insertBALResults(ctx context.Context, results []*fetchResult) error {
	blocks := make(types.Blocks, len(results))
	receipts := make([]types.Receipts, len(results))
	lists := make([][]byte, len(results))
	for i, result := range results {
		blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.Transactions)
		receipts[i], lists[i] = result.Receipts, result.BAL
	}
	n, err := d.bals.InsertChain(ctx, blocks, receipts, lists)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrBALMismatch) && n < len(results) {
		peer := results[n].BALPeer
		logger.Warn("Dropping a peer whose block access list does not match", "peer", peer, "number", blocks[n].Number())
		if d.dropPeer != nil {
			d.dropPeer(peer)
		}
		return fmt.Errorf("%w: block %d: %v", errInvalidBAL, blocks[n].NumberU64(), err)
	}
	if ctx.Err() != nil {
		return errCanceled
	}
	logger.Debug("Downloaded item processing failed", "number", blocks[n].Number(), "hash", blocks[n].Hash(), "err", err)
	return fmt.Errorf("%w: %v", errInvalidChain, err)
}
