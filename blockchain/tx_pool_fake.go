// Modifications Copyright 2024 The Kaia Authors
// Copyright 2020 The klaytn Authors
// This file is part of the klaytn library.
//
// The klaytn library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The klaytn library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the klaytn library. If not, see <http://www.gnu.org/licenses/>.
package blockchain

import (
	"errors"
	"math/big"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/event"
	"github.com/kaiachain/kaia/kaiax"
)

var errTxPoolDisabled = errors.New("the transaction pool is disabled")

// FakeTxPool is a transaction pool that holds nothing, for a node made to
// run a tool on its chain: it runs no loop, subscribes to no chain event and
// writes no journal.
type FakeTxPool struct {
	txFeed event.Feed
}

// NewFakeTxPool returns a transaction pool that holds nothing.
func NewFakeTxPool() *FakeTxPool {
	return &FakeTxPool{}
}

func (*FakeTxPool) HandleTxMsg(types.Transactions) {}

func (*FakeTxPool) Pending() (map[common.Address]types.Transactions, error) {
	return map[common.Address]types.Transactions{}, nil
}

func (*FakeTxPool) CachedPendingTxsByCount(int) types.Transactions { return nil }

func (p *FakeTxPool) SubscribeNewTxsEvent(ch chan<- NewTxsEvent) event.Subscription {
	return p.txFeed.Subscribe(ch)
}

func (*FakeTxPool) GetPendingNonce(common.Address) uint64     { return 0 }
func (*FakeTxPool) AddLocal(*types.Transaction) error         { return errTxPoolDisabled }
func (*FakeTxPool) GasPrice() *big.Int                        { return new(big.Int) }
func (*FakeTxPool) SetGasPrice(*big.Int)                      {}
func (*FakeTxPool) Stop()                                     {}
func (*FakeTxPool) Get(common.Hash) *types.Transaction        { return nil }
func (*FakeTxPool) Stats() (int, int)                         { return 0, 0 }
func (*FakeTxPool) StartSpamThrottler(*ThrottlerConfig) error { return errTxPoolDisabled }
func (*FakeTxPool) StopSpamThrottler()                        {}

func (*FakeTxPool) Content() (map[common.Address]types.Transactions, map[common.Address]types.Transactions) {
	return map[common.Address]types.Transactions{}, map[common.Address]types.Transactions{}
}

func (*FakeTxPool) GetBlobSidecarFromStorage(*big.Int, int) (*types.BlobTxSidecar, error) {
	return nil, errTxPoolDisabled
}

func (*FakeTxPool) GetBlobSidecarFromPool(common.Hash) (*types.BlobTxSidecar, error) {
	return nil, errTxPoolDisabled
}

func (*FakeTxPool) SubscribeMissingBlobSidecars() <-chan *MissingBlobSidecar { return nil }

func (*FakeTxPool) SaveBlobSidecar(common.Hash, *types.BlobTxSidecar) error {
	return errTxPoolDisabled
}

func (*FakeTxPool) RegisterTxPoolModule(...kaiax.TxPoolModule) {}
