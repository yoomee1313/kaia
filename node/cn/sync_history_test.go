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

package cn

import (
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/kaiachain/kaia"
	"github.com/kaiachain/kaia/common"
	bal_impl "github.com/kaiachain/kaia/kaiax/bal/impl"
	"github.com/kaiachain/kaia/networks/p2p"
	"github.com/kaiachain/kaia/node/cn/mocks"
	"github.com/kaiachain/kaia/storage/database"
	workmocks "github.com/kaiachain/kaia/work/mocks"
	"github.com/stretchr/testify/require"
)

// TestMarkSyncedWaitsForHeaderSegments keeps a history or light node syncing
// until every finished segment has the root of its block hashes.
func TestMarkSyncedWaitsForHeaderSegments(t *testing.T) {
	defer func(d time.Duration) { historyFilledPoll = d }(historyFilledPoll)
	historyFilledPoll = time.Millisecond
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	downloader := mocks.NewMockProtocolManagerDownloader(ctrl)
	downloader.EXPECT().Progress().Return(kaia.SyncProgress{CurrentBlock: 10, HighestBlock: 10}).AnyTimes()
	chain := workmocks.NewMockBlockChain(ctrl)
	var filled atomic.Bool

	pm := &ProtocolManager{blockchain: chain, downloader: downloader, quitSync: make(chan struct{}), segmentsFilled: filled.Load}
	pm.markSynced()
	pm.markSynced()
	time.Sleep(10 * time.Millisecond)
	require.Zero(t, pm.acceptTxs.Load())
	require.Equal(t, uint64(11), pm.SyncProgress().HighestBlock)

	filled.Store(true)
	require.Eventually(t, func() bool { return pm.acceptTxs.Load() == 1 }, time.Second, time.Millisecond)
	require.Equal(t, uint64(10), pm.SyncProgress().HighestBlock)

	// A full node is synced at once.
	filled.Store(false)
	full := &ProtocolManager{blockchain: chain, downloader: downloader, quitSync: make(chan struct{})}
	full.markSynced()
	require.Equal(t, uint32(1), full.acceptTxs.Load())
	require.Equal(t, uint64(10), full.SyncProgress().HighestBlock)
}

// versionConn is a kaia/69 peer to a bal_impl.Sync that sends nothing.
type versionConn struct {
	id      string
	version int
}

func (c *versionConn) GetID() string                  { return c.id }
func (c *versionConn) GetVersion() int                { return c.version }
func (c *versionConn) Send(uint64, interface{}) error { return nil }

// TestHistoryRangeHandshake exchanges the history ranges in the kaia/69
// handshake only.
func TestHistoryRangeHandshake(t *testing.T) {
	genesis, td, chainID := common.Hash{1}, big.NewInt(1), big.NewInt(1)
	for _, v := range []int{kaia68, kaia69} {
		rw1, rw2 := p2p.MsgPipe()
		p1, p2 := newPeer(v, p2pPeers[0], rw1), newPeer(v, p2pPeers[1], rw2)
		r1 := &bal_impl.HistoryRange{Header: 5, Body: 6, Receipt: 7, BAL: 8, BALNext: 100}
		r2 := &bal_impl.HistoryRange{BAL: bal_impl.NoHistory}
		errc := make(chan error, 2)
		go func() { errc <- p1.Handshake(1, chainID, td, common.Hash{2}, genesis, r1) }()
		go func() { errc <- p2.Handshake(1, chainID, td, common.Hash{3}, genesis, r2) }()
		require.NoError(t, <-errc)
		require.NoError(t, <-errc)
		if v < kaia69 {
			require.Nil(t, p1.HistoryRange())
			require.Nil(t, p2.HistoryRange())
		} else {
			require.Equal(t, r2, p1.HistoryRange())
			require.Equal(t, r1, p2.HistoryRange())
		}
		rw1.Close()
	}

	// A kaia/69 status without a range is rejected.
	rw1, rw2 := p2p.MsgPipe()
	defer rw1.Close()
	p1 := newPeer(kaia69, p2pPeers[0], rw1)
	go func() {
		msg, err := rw2.ReadMsg()
		if err != nil {
			return
		}
		msg.Discard()
		p2p.Send(rw2, StatusMsg, &statusData{ProtocolVersion: kaia69, NetworkId: 1, TD: td, CurrentBlock: common.Hash{3}, GenesisBlock: genesis, ChainID: chainID})
	}()
	require.ErrorContains(t, p1.Handshake(1, chainID, td, common.Hash{2}, genesis, &bal_impl.HistoryRange{BAL: bal_impl.NoHistory}), "no history range")
}

// TestHoldsBlocksFrom picks a peer for the downloader by the headers, bodies
// and receipts its announced range holds.
func TestHoldsBlocksFrom(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	peer := func(id string, td int64, rng *bal_impl.HistoryRange) *MockPeer {
		p := NewMockPeer(ctrl)
		p.EXPECT().GetID().Return(id).AnyTimes()
		p.EXPECT().HistoryRange().Return(rng).AnyTimes()
		p.EXPECT().Head().Return(common.Hash{}, big.NewInt(td)).AnyTimes()
		return p
	}
	light := peer("light", 30, &bal_impl.HistoryRange{Header: 100, Body: 100, Receipt: 100, BAL: 100, BALNext: 200})
	full := peer("full", 20, &bal_impl.HistoryRange{BAL: bal_impl.NoHistory})
	old := peer("old", 10, nil)
	peers := NewMockPeerSet(ctrl)
	peers.EXPECT().Peers().Return(map[string]Peer{"light": light, "full": full, "old": old}).AnyTimes()
	pm := &ProtocolManager{peers: peers, balSync: bal_impl.NewSync(database.NewMemoryDBManager(), nil, nil)}

	require.True(t, pm.holdsBlocksFrom(light, 100))
	require.False(t, pm.holdsBlocksFrom(light, 99))
	require.True(t, pm.holdsBlocksFrom(old, 0), "a peer before kaia/69 holds all")
	require.Equal(t, Peer(light), pm.bestPeerHoldingBlocks(150))
	require.Equal(t, Peer(full), pm.bestPeerHoldingBlocks(50))

	// A range announced later counts over the handshake one.
	lightConn := &versionConn{id: "light", version: kaia69}
	pm.balSync.AddPeer(lightConn, &bal_impl.HistoryRange{BAL: bal_impl.NoHistory}, &bal_impl.HistoryRange{Header: 40, Body: 40, Receipt: 40, BAL: bal_impl.NoHistory})
	require.True(t, pm.holdsBlocksFrom(light, 50))
	require.Equal(t, Peer(light), pm.bestPeerHoldingBlocks(50))
}
