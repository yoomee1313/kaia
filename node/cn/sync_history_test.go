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
	"testing"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/networks/p2p"
	"github.com/stretchr/testify/require"
)

// TestHistoryRangeHandshake exchanges the history ranges in the kaia/69
// handshake only.
func TestHistoryRangeHandshake(t *testing.T) {
	genesis, td, chainID := common.Hash{1}, big.NewInt(1), big.NewInt(1)
	for _, v := range []int{kaia68, kaia69} {
		rw1, rw2 := p2p.MsgPipe()
		p1, p2 := newPeer(v, p2pPeers[0], rw1), newPeer(v, p2pPeers[1], rw2)
		r1 := &bal.HistoryRange{Header: 5, Body: 6, Receipt: 7, BAL: 8, BALNext: 100}
		r2 := &bal.HistoryRange{BAL: bal.NoHistory}
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
	require.ErrorContains(t, p1.Handshake(1, chainID, td, common.Hash{2}, genesis, &bal.HistoryRange{BAL: bal.NoHistory}), "no history range")
}
