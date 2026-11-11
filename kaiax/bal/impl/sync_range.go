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
	"context"
	"fmt"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
)

// A node that syncs by applying BALs fetches the blocks up to the last block
// of the signed BAL hash chain with their receipts and BALs by segments, and
// inserts them without executing them: the BALs of a segment are checked
// against the chain before any of them is applied, and the seals, receipts
// and state roots of the blocks as the chain inserts them. Before Amber a
// header does not commit the BAL of its block, so the blocks after the last
// block of the chain are executed by the downloader.

// blocksRequest asks for the blocks From to To with their receipts and BALs.
type blocksRequest struct {
	ID, From, To uint64
}

type rangeBlock struct {
	Header   *types.Header
	Body     rlp.RawValue
	Receipts []*slimReceipt
	BAL      []byte
}

// blocksResponse holds the requested blocks in order, up to the first one the
// node does not hold whole, or the response size limit.
type blocksResponse struct {
	ID     uint64
	Blocks []*rangeBlock
}

// maxRangeBlocks is how many blocks a request asks for.
const maxRangeBlocks = 1024

func (s *Sync) serveBlocks(req *blocksRequest) *blocksResponse {
	res := &blocksResponse{ID: req.ID}
	size := 0
	for n := max(req.From, 1); n <= req.To && n < max(req.From, 1)+maxRangeBlocks && size < softResponseLimit; n++ {
		hash := s.db.ReadCanonicalHash(n)
		header := s.db.ReadHeader(hash, n)
		body := s.db.ReadBodyRLP(hash, n)
		list := ReadBAL(s.db, hash, n)
		receipts := s.db.ReadReceipts(hash, n)
		if header == nil || body == nil || list == nil || (receipts == nil && header.ReceiptHash != types.GetEmptyRootHash(header.Number)) {
			break
		}
		res.Blocks = append(res.Blocks, &rangeBlock{Header: header, Body: body, Receipts: slimReceipts(receipts), BAL: list})
		size += len(body) + len(list) + 600
		for _, r := range receipts {
			size += 16 + len(r.Logs)*128
		}
	}
	return res
}

func (r *HistoryRange) holdsBlocks(from, to uint64) bool {
	return r.Header <= from && r.Body <= from && r.Receipt <= from && r.holdsBALs(from, to)
}

// checkBlock checks a fetched block against its header and returns it as one
// to insert.
func checkBlock(n uint64, b *rangeBlock) (*Block, error) {
	if b.Header == nil || b.Header.Number == nil || b.Header.Number.Uint64() != n {
		return nil, fmt.Errorf("%w: not block %d", errBALSyncMismatch, n)
	}
	var body types.Body
	if err := rlp.DecodeBytes(b.Body, &body); err != nil {
		return nil, fmt.Errorf("%w: body of block %d: %v", errBALSyncMismatch, n, err)
	}
	if root := types.DeriveTransactionsRoot(types.Transactions(body.Transactions), b.Header.Number); root != b.Header.TxHash {
		return nil, fmt.Errorf("%w: block %d transactions root", errBALSyncMismatch, n)
	}
	receipts, err := fullReceipts(b.Header, b.Receipts)
	if err != nil {
		return nil, err
	}
	return &Block{Block: types.NewBlockWithHeader(b.Header).WithBody(body.Transactions), Receipts: receipts, BAL: b.BAL}, nil
}

// FetchBlocks returns blocks from to to, in one segment, with their receipts
// and BALs from one peer, checked against their headers and, with a chain
// whose segment they are, against the chain.
func (s *Sync) FetchBlocks(ctx context.Context, chain *HashChain, from, to uint64) ([]*Block, error) {
	blocks, _, err := s.fetchBlocks(ctx, chain, from, to)
	return blocks, err
}

// fetchBlocks is FetchBlocks, also returning the peer the blocks came from.
func (s *Sync) fetchBlocks(ctx context.Context, chain *HashChain, from, to uint64) ([]*Block, string, error) {
	k := Segment(from)
	if from == 0 || to < from || Segment(to) != k {
		return nil, "", fmt.Errorf("blocks %d-%d are not in one segment", from, to)
	}
	check := chain != nil && chain.Covers(from)
	if check {
		if f, t := chain.SegmentBlocks(k); max(f, 1) != from || t != to {
			return nil, "", fmt.Errorf("blocks %d-%d are not segment %d of the BAL hash chain", from, to, k)
		}
	}
	var (
		out    []*Block
		peerID string
	)
	err := s.fetch(ctx, fmt.Sprintf("blocks %d-%d", from, to),
		func(r *HistoryRange) bool { return r.holdsBlocks(from, to) },
		func(p *balPeer) error {
			out, peerID = out[:0], p.conn.GetID()
			for next := from; next <= to; {
				res, err := s.request(ctx, p, BlocksRequestMsg, func(id uint64) interface{} {
					return &blocksRequest{ID: id, From: next, To: to}
				})
				if err != nil {
					return err
				}
				blocks := res.(*blocksResponse).Blocks
				if len(blocks) == 0 {
					return fmt.Errorf("%w: block %d", errBALSyncNoPeer, next)
				}
				if uint64(len(blocks)) > to-next+1 {
					return fmt.Errorf("%w: %d blocks for blocks %d-%d", errBALSyncMismatch, len(blocks), next, to)
				}
				for _, rb := range blocks {
					b, err := checkBlock(next, rb)
					if err != nil {
						return err
					}
					if len(out) > 0 && b.Block.ParentHash() != out[len(out)-1].Block.Hash() {
						return fmt.Errorf("%w: block %d does not follow block %d", errBALSyncMismatch, next, next-1)
					}
					out = append(out, b)
					next++
				}
			}
			if !check {
				return nil
			}
			hashes := make([]common.Hash, 0, len(out)+1)
			if from == 1 {
				hashes = append(hashes, EmptyListHash)
			}
			for _, b := range out {
				hashes = append(hashes, crypto.Keccak256Hash(b.BAL))
			}
			if err := chain.VerifySegment(k, hashes); err != nil {
				return fmt.Errorf("%w: %v", errBALSyncMismatch, err)
			}
			return nil
		})
	if err != nil {
		return nil, "", err
	}
	return out, peerID, nil
}

// rangeTarget returns the last block that peers hold whole after from.
func (s *Sync) rangeTarget(from uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var target uint64
	for _, p := range s.peers {
		if p.rng != nil && p.rng.holdsBlocks(from, from) {
			target = max(target, p.rng.BALNext-1)
		}
	}
	return target
}

// RangeChain inserts blocks by applying their BALs.
type RangeChain interface {
	CurrentBlock() *types.Block
	InsertChainWithBALs(blocks []*Block) (int, error)
}

// ChainOf returns chain as one that inserts blocks with their BALs: a
// BlockChain inserts them with InsertChain.
func ChainOf(chain interface{}) (TipChain, bool) {
	if bc, ok := chain.(*blockchain.BlockChain); ok {
		return chainInserter{bc}, true
	}
	c, ok := chain.(TipChain)
	return c, ok
}

type chainInserter struct {
	*blockchain.BlockChain
}

func (c chainInserter) InsertChainWithBALs(blocks []*Block) (int, error) {
	return InsertChain(c.BlockChain, blocks)
}

// rangeAhead is how many ranges are fetched ahead of the insertion.
var rangeAhead = 2

// SyncRanges applies the blocks that peers hold after the head of bc, up to
// the last block of chain, segment by segment. It returns the number of
// inserted blocks.
func (s *Sync) SyncRanges(ctx context.Context, bc RangeChain, chain *HashChain) (int, error) {
	if chain == nil {
		return 0, nil
	}
	head := bc.CurrentBlock().NumberU64()
	target := min(s.rangeTarget(head+1), chain.Last)
	if target <= head {
		return 0, nil
	}
	syncLogger.Info("Syncing by applying block access lists", "from", head+1, "to", target, "hashChainLast", chain.Last)

	type fetched struct {
		blocks []*Block
		peer   string
		err    error
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan chan fetched, rangeAhead)
	go func() {
		defer close(queue)
		for from := head + 1; from <= target; {
			start, to := chain.SegmentBlocks(Segment(from))
			start = max(start, 1)
			ch := make(chan fetched, 1)
			select {
			case queue <- ch:
			case <-ctx.Done():
				return
			}
			go func(start, to uint64) {
				blocks, peer, err := s.fetchBlocks(ctx, chain, start, to)
				ch <- fetched{blocks, peer, err}
			}(start, to)
			from = to + 1
		}
	}()

	inserted := 0
	for ch := range queue {
		res := <-ch
		if res.err != nil {
			return inserted, res.err
		}
		batch := res.blocks
		for len(batch) > 0 && batch[0].Block.NumberU64() <= head {
			batch = batch[1:]
		}
		if len(batch) == 0 {
			continue
		}
		n, err := bc.InsertChainWithBALs(batch)
		inserted += n
		if err != nil {
			syncLogger.Warn("Dropping a peer whose blocks do not insert", "peer", res.peer, "err", err)
			s.drop(res.peer)
			return inserted, fmt.Errorf("blocks %d-%d: %w", batch[0].Block.NumberU64(), batch[len(batch)-1].Block.NumberU64(), err)
		}
		inserted += len(batch) - n
	}
	syncLogger.Info("Synced by applying block access lists", "head", bc.CurrentBlock().NumberU64(), "inserted", inserted)
	return inserted, nil
}

// errNoHashChain tells how to give a node applying BALs the hash chain.
var errNoHashChain = fmt.Errorf("pre-Amber block access lists are applied only once checked against the BAL hash chain signed by the Kaia Foundation: "+
	"install it with `npm install %s` and set --bal.hash-chain to the directory it was installed in, or set --sync.apply-bals=false to execute the blocks", HashChainPackage)

// Configure sets how the node syncs: apply tells whether it applies BALs,
// which needs the signed hash chain of the network of the genesis block, in
// the file or package directory hashChain.
func (s *Sync) Configure(hashChain string, apply bool, genesis func() common.Hash, quit <-chan struct{}) error {
	s.quit = quit
	s.applyBALs = apply
	if hashChain == "" {
		if apply {
			return errNoHashChain
		}
		return nil
	}
	chain, err := LoadSignedHashChain(hashChain, genesis(), HashChainPublicKey)
	if err != nil {
		return fmt.Errorf("BAL hash chain %s: %w; %w", hashChain, err, errNoHashChain)
	}
	syncLogger.Info("Loaded the signed BAL hash chain", "last", chain.Last, "root", chain.Root())
	s.hashChain = chain
	return nil
}

// Synchronise syncs by applying BALs, if the node does, up to the last block
// of the hash chain; the blocks after it are left to the downloader, which
// executes them. It reports whether the caller is done, which it is only when
// another call is syncing.
func (s *Sync) Synchronise(chain interface{}) (done bool) {
	bc, ok := ChainOf(chain)
	if s == nil || !s.applyBALs || s.hashChain == nil || !ok || bc.CurrentBlock().NumberU64() >= s.hashChain.Last {
		return false
	}
	if !s.rangeRunning.CompareAndSwap(false, true) {
		return true
	}
	defer s.rangeRunning.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	if s.rangeTarget(bc.CurrentBlock().NumberU64()+1) == 0 {
		syncLogger.Warn("No peer serves block access lists, executing blocks instead")
		return false
	}
	for {
		n, err := s.SyncRanges(ctx, bc, s.hashChain)
		if err != nil {
			syncLogger.Warn("Failed to sync by applying block access lists, executing blocks instead", "err", err)
			return false
		}
		if n == 0 {
			return false
		}
	}
}
