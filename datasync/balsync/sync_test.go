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
	"bytes"
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/hashchain"
	"github.com/kaiachain/kaia/datasync/downloader"
	"github.com/kaiachain/kaia/networks/p2p"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

// balTestChain is a chain database with the BALs and receipts of blocks 1 to
// last.
type balTestChain struct {
	db       database.DBManager
	headers  map[common.Hash]*types.Header
	receipts map[common.Hash]types.Receipts
	order    []*types.Header // by number
}

func (c *balTestChain) GetHeaderByHash(hash common.Hash) *types.Header { return c.headers[hash] }
func (c *balTestChain) GetReceiptsByBlockHash(hash common.Hash) types.Receipts {
	return c.receipts[hash]
}

func newBALTestChain(t *testing.T, last uint64) *balTestChain {
	blockchain.InitDeriveSha(params.TestChainConfig.Copy())
	c := &balTestChain{db: database.NewMemoryDBManager(), headers: make(map[common.Hash]*types.Header), receipts: make(map[common.Hash]types.Receipts)}
	for n := uint64(0); n <= last; n++ {
		receipts := types.Receipts{{Status: types.ReceiptStatusSuccessful, GasUsed: 21000 + n, Logs: []*types.Log{
			{Address: common.Address{byte(n)}, Topics: []common.Hash{{byte(n >> 8)}}, Data: []byte{byte(n)}},
		}}}
		receipts[0].Bloom = types.CreateBloom(receipts)
		header := &types.Header{Number: new(big.Int).SetUint64(n), ReceiptHash: types.DeriveReceiptsRoot(receipts, new(big.Int).SetUint64(n)), Bloom: types.CreateBloom(receipts)}
		hash := header.Hash()
		c.headers[hash], c.receipts[hash] = header, receipts
		c.order = append(c.order, header)
		c.db.WriteCanonicalHash(hash, n)
		c.db.WriteReceipts(hash, n, receipts)
		if n == 0 {
			continue
		}
		list := &types.BlockAccessList{Accounts: []types.AccountChanges{{
			Address:        common.Address{byte(n), byte(n >> 8)},
			BalanceChanges: []types.BalanceChange{{BlockAccessIndex: 1, PostBalance: new(big.Int).SetUint64(n)}},
		}}}
		enc, err := list.Writes().Encode()
		require.NoError(t, err)
		require.NoError(t, c.db.WriteBAL(hash, n, enc))
	}
	require.NoError(t, c.db.WriteBALRange(1, last+1))
	return c
}

func (c *balTestChain) hashChain(t *testing.T, last uint64) *hashchain.Chain {
	chain, err := hashchain.Build(last, func(n uint64) (common.Hash, error) { return state.HashOf(c.db, n) })
	require.NoError(t, err)
	return chain
}

// loopConn delivers the messages one side sends to the other side's handler.
type loopConn struct {
	id      string
	version int
	to      *Sync
	back    *loopConn // the conn the other side sees
	tamper  func(code uint64, data interface{}) interface{}

	mu       sync.Mutex
	requests int
}

func (c *loopConn) GetID() string   { return c.id }
func (c *loopConn) GetVersion() int { return c.version }

func (c *loopConn) Send(code uint64, data interface{}) error {
	if c.tamper != nil {
		data = c.tamper(code, data)
	}
	if code == BALsRequestMsg || code == BlocksRequestMsg || code == SegmentHashesRequestMsg {
		c.mu.Lock()
		c.requests++
		c.mu.Unlock()
	}
	enc, err := rlp.EncodeToBytes(data)
	if err != nil {
		return err
	}
	msg := p2p.Msg{Code: code, Size: uint32(len(enc)), Payload: bytes.NewReader(enc)}
	go c.to.HandleMsg(c.back, msg)
	return nil
}

// sentConn keeps what is sent to it.
type sentConn struct {
	id      string
	version int
	sent    []sentMsg
}

type sentMsg struct {
	code uint64
	data interface{}
}

func (c *sentConn) GetID() string   { return c.id }
func (c *sentConn) GetVersion() int { return c.version }

func (c *sentConn) Send(code uint64, data interface{}) error {
	c.sent = append(c.sent, sentMsg{code, data})
	return nil
}

func (c *loopConn) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

// connect connects a client to a server named id; tamper changes what the
// server sends.
func connect(client, server *Sync, id string, tamper func(code uint64, data interface{}) interface{}) *loopConn {
	toServer := &loopConn{id: id, version: SyncVersion, to: server}
	toClient := &loopConn{id: "client-" + id, version: SyncVersion, to: client, tamper: tamper}
	toServer.back, toClient.back = toClient, toServer
	sr, cr := server.LocalRange(), client.LocalRange()
	server.AddPeer(toClient, &sr, &cr)
	client.AddPeer(toServer, &cr, &sr)
	return toServer
}

func waitRanges(t *testing.T, s *Sync, n int) {
	require.Eventually(t, func() bool { return len(s.PeerRanges()) == n }, time.Second, time.Millisecond)
}

// TestBALSyncSegmentHashes fetches the hashes of the BALs of segments, block
// 0 included and the partial last one, checked against the hash chain: a peer
// serving a wrong hash is dropped, and a checked segment is not fetched again.
func TestBALSyncSegmentHashes(t *testing.T) {
	defer func(d time.Duration) { balSyncNoPeer = d }(balSyncNoPeer)
	balSyncNoPeer = 300 * time.Millisecond

	last := uint64(2*hashchain.SegmentSize + 100)
	src := newBALTestChain(t, last)
	chain := src.hashChain(t, last)
	var dropped []string
	client := NewSync(database.NewMemoryDBManager(), &balTestChain{}, nil)
	client.hashChain = chain
	client.drop = func(id string) { dropped = append(dropped, id); client.RemovePeer(id) }
	tamper := func(code uint64, data interface{}) interface{} {
		if res, ok := data.(*segmentHashesResponse); ok && len(res.Hashes) > 7 {
			res.Hashes[7] = common.Hash{1}
		}
		return data
	}
	connect(client, NewSync(src.db, src, nil), "a-bad", tamper)
	good := connect(client, NewSync(src.db, src, nil), "b-good", nil)
	waitRanges(t, client, 2)

	ctx := context.Background()
	for k := uint64(0); k < 3; k++ {
		hashes, from, err := client.segmentHashes(ctx, k)
		require.NoError(t, err, "segment %d", k)
		f, to := chain.SegmentBlocks(k)
		require.Equal(t, f, from)
		require.Len(t, hashes, int(to-f+1))
		for i, h := range hashes {
			want, err := state.HashOf(src.db, from+uint64(i))
			require.NoError(t, err)
			require.Equal(t, want, h)
		}
	}
	require.Equal(t, []string{"a-bad"}, dropped)
	n := good.requestCount()
	_, _, err := client.segmentHashes(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, n, good.requestCount(), "a checked segment is kept")
}

var emptyBALWrites = func() []byte {
	enc, err := (&types.BlockAccessList{}).Writes().Encode()
	if err != nil {
		panic(err)
	}
	return enc
}()

func (c *balTestChain) rawBAL(t *testing.T, n uint64) []byte {
	if n == 0 {
		return emptyBALWrites
	}
	enc, err := c.db.ReadCanonicalBAL(n)
	require.NoError(t, err)
	return enc
}

func TestBALSyncPropagation(t *testing.T) {
	src := newBALTestChain(t, 3)
	server := NewSync(src.db, src, nil)

	header := src.order[2]
	block := types.NewBlockWithHeader(header)
	enc := src.rawBAL(t, 2)
	p69, p68 := &sentConn{id: "p69", version: SyncVersion}, &sentConn{id: "p68", version: SyncVersion - 1}
	server.peers["p69"] = &balPeer{conn: p69, pending: make(map[uint64]chan interface{})}
	receipts := slimReceipts(src.db.ReadReceipts(header.Hash(), 2))
	require.Len(t, receipts, 1)
	BroadcastBAL(server, block, []*sentConn{p69, p68})
	BroadcastBAL(server, block, []*sentConn{p69, p68})
	require.Equal(t, []sentMsg{{NewBlockBALMsg, &newBlockBAL{Hash: header.Hash(), Number: 2, List: enc, Receipts: receipts}}}, p69.sent)
	require.Empty(t, p68.sent)

	client := NewSync(database.NewMemoryDBManager(), &balTestChain{}, nil)
	conn := &loopConn{id: "server"}
	client.receive(conn, &newBlockBAL{Hash: header.Hash(), Number: 2, List: enc, Receipts: receipts})
	got, gotReceipts, ok := client.PropagatedBAL(header.Hash())
	require.True(t, ok)
	require.Equal(t, enc, got)
	full, err := fullReceipts(header, gotReceipts)
	require.NoError(t, err)
	require.Len(t, full, 1)
	server.receive(conn, &newBlockBAL{Hash: header.Hash(), Number: 2, List: []byte{0xc0}})
	_, _, ok = server.PropagatedBAL(header.Hash())
	require.False(t, ok, "the recorded BAL is kept")
}

// TestBALSyncRange announces the held range and serves only what it holds.
func TestBALSyncRange(t *testing.T) {
	src := newBALTestChain(t, 10)
	server := NewSync(src.db, src, nil)
	require.Equal(t, HistoryRange{BAL: 1, BALNext: 11}, server.LocalRange())
	require.Equal(t, HistoryRange{BAL: NoHistory}, NewSync(database.NewMemoryDBManager(), src, nil).LocalRange())

	lists := server.serveBALs([]common.Hash{src.order[9].Hash(), src.order[10].Hash(), {1}, src.order[3].Hash()})
	require.Equal(t, [][]byte{src.rawBAL(t, 9), src.rawBAL(t, 10)}, lists, "up to the first one not held")
	require.Empty(t, server.serveSegmentHashes(&segmentHashesRequest{Segment: 0, Last: 20}).Hashes, "segment not held")
	require.Len(t, server.serveSegmentHashes(&segmentHashesRequest{Segment: 0, Last: 10}).Hashes, 11)

	client := NewSync(database.NewMemoryDBManager(), &balTestChain{}, nil)
	connect(client, server, "server", nil)
	waitRanges(t, client, 1)
	require.Equal(t, map[string]HistoryRange{"server": {BAL: 1, BALNext: 11}}, client.PeerRanges())

	header := func(n uint64) *types.Header { return &types.Header{Number: new(big.Int).SetUint64(n)} }
	require.False(t, client.Applies(header(5)), "not applying BALs")
	client.applyBALs, client.hashChain = true, src.hashChain(t, 8)
	require.True(t, client.Applies(header(5)))
	require.False(t, client.Applies(header(0)), "the genesis")
	require.False(t, client.Applies(header(9)), "after the hash chain")
	require.True(t, client.Holds("server", downloader.PartBAL, 10))
	require.False(t, client.Holds("server", downloader.PartBAL, 11))
	require.True(t, client.Holds("server", downloader.PartReceipts, 0))
	require.True(t, client.Holds("old", downloader.PartBody, 5), "a peer before kaia/69 holds every body")
	require.False(t, client.Holds("old", downloader.PartBAL, 5), "and no BAL")
	client.RemovePeer("server")
	require.Empty(t, client.PeerRanges())
}

// TestHistoryRangeAnnounce announces the range again when any oldest block
// changes, when the BALs shrink, or when they grew by balAnnounceStep.
func TestHistoryRangeAnnounce(t *testing.T) {
	sent := HistoryRange{Header: 10, Body: 10, Receipt: 10, BAL: 10, BALNext: 5000}
	for _, tc := range []struct {
		change func(r *HistoryRange)
		want   bool
	}{
		{func(r *HistoryRange) {}, false},
		{func(r *HistoryRange) { r.Header++ }, true},
		{func(r *HistoryRange) { r.Body++ }, true},
		{func(r *HistoryRange) { r.Receipt++ }, true},
		{func(r *HistoryRange) { r.BAL++ }, true},
		{func(r *HistoryRange) { r.BALNext-- }, true},
		{func(r *HistoryRange) { r.BALNext += balAnnounceStep - 1 }, false},
		{func(r *HistoryRange) { r.BALNext += balAnnounceStep }, true},
	} {
		rng := sent
		tc.change(&rng)
		require.Equal(t, tc.want, rng.announces(sent), "%+v", rng)
	}

	src := newBALTestChain(t, 10)
	server := NewSync(src.db, src, nil)
	client := NewSync(database.NewMemoryDBManager(), &balTestChain{}, nil)
	connect(client, server, "server", nil)
	waitRanges(t, client, 1)
	require.Equal(t, HistoryRange{BAL: 1, BALNext: 11}, client.PeerRanges()["server"])

	require.NoError(t, src.db.WriteBALRange(1, 12))
	server.announce(server.LocalRange())
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, uint64(11), client.PeerRanges()["server"].BALNext, "a small growth waits")
	require.NoError(t, src.db.WriteBALRange(1, 11+balAnnounceStep))
	server.announce(server.LocalRange())
	require.Eventually(t, func() bool { return client.PeerRanges()["server"].BALNext == 11+balAnnounceStep }, time.Second, time.Millisecond)
}
