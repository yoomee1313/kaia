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
	"bytes"
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
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
		list := &access.BlockAccessList{Accounts: []access.AccountAccess{{
			Address:        common.Address{byte(n), byte(n >> 8)},
			BalanceChanges: []access.BalanceChange{{Index: 1, Balance: new(big.Int).SetUint64(n)}},
		}}}
		enc, err := list.Writes().Encode()
		require.NoError(t, err)
		require.NoError(t, c.db.WriteBAL(hash, n, enc))
	}
	require.NoError(t, c.db.WriteBALRange(1, last+1))
	return c
}

func (c *balTestChain) hashChain(t *testing.T, last uint64) *HashChain {
	chain, err := BuildHashChain(last, func(n uint64) (common.Hash, error) { return HashOf(c.db, n) })
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
	if code == BALsRequestMsg || code == BALProofRequestMsg || code == SlimReceiptsRequestMsg || code == BlocksRequestMsg {
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

// tamperBAL changes the BAL of block n in responses.
func tamperBAL(n uint64) func(code uint64, data interface{}) interface{} {
	return func(code uint64, data interface{}) interface{} {
		switch res := data.(type) {
		case *balsResponse:
			res.Lists = tamperLists(append([][]byte{}, res.Lists...), n)
		case *balProofResponse:
			if res.List != nil {
				list, _ := access.Decode(res.List)
				list.Accounts[0].BalanceChanges[0].Balance = big.NewInt(12345)
				res.List, _ = list.Encode()
			}
		case *slimReceiptsResponse:
			for _, r := range res.Receipts {
				r[0].GasUsed++
			}
		}
		return data
	}
}

// tamperLists replaces the BAL of block n, recognised by its address, with the
// one of the block before.
func tamperLists(lists [][]byte, n uint64) [][]byte {
	for i, enc := range lists {
		list, err := access.Decode(enc)
		if err != nil || len(list.Accounts) == 0 {
			continue
		}
		if a := list.Accounts[0].Address; a == (common.Address{byte(n), byte(n >> 8)}) {
			list.Accounts[0].BalanceChanges[0].Balance = big.NewInt(1)
			lists[i], _ = list.Encode()
		}
	}
	return lists
}

func TestBALSyncSegments(t *testing.T) {
	defer func(d time.Duration) { balSyncNoPeer = d }(balSyncNoPeer)
	balSyncNoPeer = 300 * time.Millisecond

	last := uint64(2*SegmentSize + 100)
	src := newBALTestChain(t, last)
	chain := src.hashChain(t, last)
	var dropped []string
	var mu sync.Mutex
	client := NewSync(database.NewMemoryDBManager(), &balTestChain{}, nil)
	client.drop = func(id string) {
		mu.Lock()
		dropped = append(dropped, id)
		mu.Unlock()
		client.RemovePeer(id)
	}
	ctx := context.Background()

	// A peer serving a wrong BAL is dropped; the segment then comes from the
	// next peer, in parts as the responses are cut short.
	bad := connect(client, NewSync(src.db, src, nil), "a-bad", tamperBAL(SegmentSize+7))
	short := func(code uint64, data interface{}) interface{} {
		if res, ok := data.(*balsResponse); ok && len(res.Lists) > 1000 {
			res.Lists = res.Lists[:1000]
		}
		return data
	}
	good := connect(client, NewSync(src.db, src, nil), "b-good", short)
	waitRanges(t, client, 2)
	from, to := chain.SegmentBlocks(1)
	encs, err := client.FetchBALs(ctx, chain, from, to)
	require.NoError(t, err)
	require.Len(t, encs, SegmentSize)
	for i, enc := range encs {
		require.Equal(t, src.rawBAL(t, from+uint64(i)), enc)
	}
	require.Equal(t, []string{"a-bad"}, dropped)
	require.Equal(t, 1, bad.requestCount())
	require.Equal(t, 9, good.requestCount())

	// Segments are fetched from several peers at once, block 0 included, and
	// the partial last one.
	other := connect(client, NewSync(src.db, src, nil), "c-good", nil)
	waitRanges(t, client, 2)
	var wg sync.WaitGroup
	for k := uint64(0); k < 3; k++ {
		wg.Add(1)
		go func(k uint64) {
			defer wg.Done()
			from, to := chain.SegmentBlocks(k)
			encs, err := client.FetchBALs(ctx, chain, from, to)
			require.NoError(t, err)
			require.Len(t, encs, int(to-from+1))
		}(k)
	}
	wg.Wait()
	require.NotZero(t, other.requestCount())

	// Blocks after the chain come unchecked, and from a peer holding them.
	encs, err = client.FetchBALs(ctx, nil, last-5, last)
	require.NoError(t, err)
	require.Len(t, encs, 6)
	_, err = client.FetchBALs(ctx, nil, last+1, last+2)
	require.ErrorIs(t, err, errBALSyncNoPeer)
	_, err = client.FetchBALs(ctx, chain, from, to-1)
	require.ErrorContains(t, err, "not segment")
}

func (c *balTestChain) rawBAL(t *testing.T, n uint64) []byte {
	if n == 0 {
		return emptyBALWrites
	}
	enc, err := ReadRaw(c.db, n)
	require.NoError(t, err)
	return enc
}

func TestBALSyncBlockProof(t *testing.T) {
	defer func(d time.Duration) { balSyncNoPeer = d }(balSyncNoPeer)
	balSyncNoPeer = 300 * time.Millisecond

	last := uint64(SegmentSize + 50)
	src := newBALTestChain(t, last)
	chain := src.hashChain(t, last)
	var dropped []string
	client := NewSync(database.NewMemoryDBManager(), &balTestChain{}, nil)
	client.drop = func(id string) { dropped = append(dropped, id); client.RemovePeer(id) }

	connect(client, NewSync(src.db, src, nil), "a-bad", tamperBAL(0))
	connect(client, NewSync(src.db, src, nil), "b-good", nil)
	waitRanges(t, client, 2)
	for _, n := range []uint64{1, 8191, SegmentSize + 3, last} {
		list, err := client.FetchBAL(context.Background(), chain, n)
		require.NoError(t, err, "block %d", n)
		enc, err := list.Writes().Encode()
		require.NoError(t, err)
		require.Equal(t, src.rawBAL(t, n), enc)
	}
	require.Equal(t, []string{"a-bad"}, dropped)
	_, err := client.FetchBAL(context.Background(), chain, last+1)
	require.ErrorContains(t, err, "after the BAL hash chain")
}

func TestBALSyncReceipts(t *testing.T) {
	src := newBALTestChain(t, 300)
	var dropped []string
	client := NewSync(database.NewMemoryDBManager(), &balTestChain{}, nil)
	client.drop = func(id string) { dropped = append(dropped, id); client.RemovePeer(id) }

	connect(client, NewSync(src.db, src, nil), "a-bad", tamperBAL(0))
	connect(client, NewSync(src.db, src, nil), "b-good", nil)
	waitRanges(t, client, 2)
	receipts, err := client.FetchReceipts(context.Background(), src.order[1:])
	require.NoError(t, err)
	require.Len(t, receipts, 300)
	for i, r := range receipts {
		want, err := rlp.EncodeToBytes(src.receipts[src.order[i+1].Hash()])
		require.NoError(t, err)
		got, err := rlp.EncodeToBytes(r)
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Equal(t, src.order[i+1].Bloom, r[0].Bloom)
	}
	require.Equal(t, []string{"a-bad"}, dropped)
}

// TestBALSyncPropagation sends the BAL and receipts of a block to kaia/69
// peers only, once each, and keeps a propagated BAL unless the block's own is
// recorded.
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

	res := server.serveBALs(&balsRequest{From: 9, To: 12})
	require.Len(t, res.Lists, 2)
	require.Empty(t, server.serveBALs(&balsRequest{From: SegmentSize - 1, To: SegmentSize}).Lists, "across segments")
	require.Nil(t, server.serveProof(&balProofRequest{Number: 3, Last: 20}).List, "segment not held")
	require.NotNil(t, server.serveProof(&balProofRequest{Number: 3, Last: 10}).List)

	client := NewSync(database.NewMemoryDBManager(), &balTestChain{}, nil)
	connect(client, server, "server", nil)
	waitRanges(t, client, 1)
	require.Equal(t, map[string]HistoryRange{"server": {BAL: 1, BALNext: 11}}, client.PeerRanges())
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

	require.NoError(t, src.db.WriteHistoryFirst(4))
	server.announce(server.LocalRange())
	require.Eventually(t, func() bool { return client.PeerRanges()["server"].Receipt == 4 }, time.Second, time.Millisecond)
	require.Equal(t, HistoryRange{Header: 4, Body: 4, Receipt: 4, BAL: 1, BALNext: 11}, client.PeerRanges()["server"])

	require.NoError(t, src.db.WriteBALRange(1, 12))
	server.announce(server.LocalRange())
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, uint64(11), client.PeerRanges()["server"].BALNext, "a small growth waits")
	require.NoError(t, src.db.WriteBALRange(1, 11+balAnnounceStep))
	server.announce(server.LocalRange())
	require.Eventually(t, func() bool { return client.PeerRanges()["server"].BALNext == 11+balAnnounceStep }, time.Second, time.Millisecond)
}
