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
	"math/big"
	"sync"
	"testing"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/hashchain"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/datasync/downloader"
	"github.com/kaiachain/kaia/event"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

// chainPeer serves the blocks of a chain to a downloader.
type chainPeer struct {
	id  string
	src *blockchain.BlockChain
	d   *downloader.Downloader
}

func (p *chainPeer) Head() (common.Hash, *big.Int) {
	head := p.src.CurrentBlock()
	return head.Hash(), p.src.GetTd(head.Hash(), head.NumberU64())
}

func (p *chainPeer) RequestHeadersByHash(origin common.Hash, amount, skip int, reverse bool) error {
	header := p.src.GetHeaderByHash(origin)
	if header == nil {
		go p.d.DeliverHeaders(p.id, nil)
		return nil
	}
	return p.RequestHeadersByNumber(header.Number.Uint64(), amount, skip, reverse)
}

func (p *chainPeer) RequestHeadersByNumber(origin uint64, amount, skip int, reverse bool) error {
	var headers []*types.Header
	for n, i := int64(origin), 0; i < amount && n >= 0; i++ {
		header := p.src.GetHeaderByNumber(uint64(n))
		if header == nil {
			break
		}
		headers = append(headers, header)
		if reverse {
			n -= int64(skip + 1)
		} else {
			n += int64(skip + 1)
		}
	}
	go p.d.DeliverHeaders(p.id, headers)
	return nil
}

func (p *chainPeer) RequestBodies(hashes []common.Hash) error {
	var txs [][]*types.Transaction
	for _, hash := range hashes {
		block := p.src.GetBlockByHash(hash)
		if block == nil {
			break
		}
		txs = append(txs, block.Transactions())
	}
	go p.d.DeliverBodies(p.id, txs)
	return nil
}

func (p *chainPeer) RequestReceipts(hashes []common.Hash) error {
	var receipts [][]*types.Receipt
	for _, hash := range hashes {
		receipts = append(receipts, p.src.GetReceiptsByBlockHash(hash))
	}
	go p.d.DeliverReceipts(p.id, receipts)
	return nil
}

func (p *chainPeer) RequestStakingInfo([]common.Hash) error { return nil }
func (p *chainPeer) RequestNodeData([]common.Hash) error    { return nil }

// downloadFixture syncs a node from genesis with the downloader, applying the
// BALs of the blocks up to the last block of the hash chain.
type downloadFixture struct {
	src     *blockchain.BlockChain
	dst     *countingChain
	blocks  []*types.Block
	client  *Sync
	d       *downloader.Downloader
	mu      sync.Mutex
	dropped []string
}

func newDownloadFixture(t *testing.T, n int, last uint64, tamper func(code uint64, data interface{}) interface{}) *downloadFixture {
	src, srcDB, dst, blocks, _ := newRangeChains(t, n)
	f := &downloadFixture{src: src, dst: &countingChain{BlockChain: dst}, blocks: blocks}
	db := dst.StateCache().TrieDB().DiskDB()
	drop := func(id string) {
		f.mu.Lock()
		f.dropped = append(f.dropped, id)
		f.mu.Unlock()
	}
	f.client = NewSync(db, f.dst, drop)
	chain, err := hashchain.Build(last, func(n uint64) (common.Hash, error) { return state.HashOf(srcDB, n) })
	require.NoError(t, err)
	f.client.applyBALs, f.client.hashChain = true, chain
	f.d = downloader.New(downloader.FullSync, db, nil, new(event.TypeMux), f.dst, nil, nil, drop, 0)
	t.Cleanup(f.d.Terminate)
	f.d.SetBALs(f.client)
	f.client.DeliverBALsTo(f.d.DeliverBALs)
	require.NoError(t, f.d.RegisterPeer("pn", SyncVersion, &chainPeer{id: "pn", src: src, d: f.d}))
	connect(f.client, NewSync(srcDB, src, nil), "pn", tamper)
	waitRanges(t, f.client, 1)
	return f
}

func (f *downloadFixture) sync() error {
	head := f.src.CurrentBlock()
	return f.d.Synchronise("pn", head.Hash(), f.src.GetTd(head.Hash(), head.NumberU64()), downloader.FullSync)
}

// TestBALDownload syncs a node from genesis with the downloader: the blocks up
// to the last block of the hash chain are inserted by applying their BALs,
// fetched with their receipts, and the blocks after it are executed.
func TestBALDownload(t *testing.T) {
	const n, last = 12, 8
	f := newDownloadFixture(t, n, last, nil)
	require.NoError(t, f.sync())

	require.Equal(t, uint64(n), f.dst.CurrentBlock().NumberU64())
	require.Equal(t, int64(last), f.dst.applied.Load())
	require.Equal(t, int64(n-last), f.dst.executed.Load())
	require.Empty(t, f.dropped)
	for _, b := range f.blocks {
		hash, n := b.Hash(), b.NumberU64()
		require.Equal(t, hash, f.dst.GetHeaderByNumber(n).Hash())
		want, err := rlp.EncodeToBytes(storageReceipts(f.src.GetReceiptsByBlockHash(hash)))
		require.NoError(t, err)
		got, err := rlp.EncodeToBytes(storageReceipts(f.dst.GetReceiptsByBlockHash(hash)))
		require.NoError(t, err)
		require.Equal(t, want, got, "block %d", n)
		require.Equal(t, f.src.StateCache().TrieDB().DiskDB().ReadBAL(hash, n), f.dst.StateCache().TrieDB().DiskDB().ReadBAL(hash, n), "block %d", n)
	}
	_, err := f.dst.StateAt(f.dst.CurrentBlock().Header())
	require.NoError(t, err)
}

// TestBALDownloadMismatch drops a peer whose BAL does not match the hash
// chain before any block is inserted with it.
func TestBALDownloadMismatch(t *testing.T) {
	swap := func(code uint64, data interface{}) interface{} {
		if lists, ok := data.([][]byte); ok && len(lists) > 1 {
			lists = append([][]byte{}, lists...)
			lists[0], lists[1] = lists[1], lists[0]
			return lists
		}
		return data
	}
	f := newDownloadFixture(t, 12, 8, swap)
	require.ErrorContains(t, f.sync(), "block access list")
	require.Contains(t, f.dropped, "pn")
	require.Zero(t, f.dst.applied.Load())
	require.Zero(t, f.dst.CurrentBlock().NumberU64())
}

func rangeCacheConfig() *blockchain.CacheConfig {
	return &blockchain.CacheConfig{
		CacheSize:           512,
		BlockInterval:       blockchain.DefaultBlockInterval,
		TriesInMemory:       blockchain.DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
		HistoryMode:         database.HistoryModeBAL,
	}
}

// newRangeChains executes a chain of n blocks with transfers and contract
// deployments on a node that records their BALs, and returns it with a node
// of the same genesis to sync.
func newRangeChains(t *testing.T, n int) (src *blockchain.BlockChain, srcDB database.DBManager, dst *blockchain.BlockChain, blocks []*types.Block, from common.Address) {
	config := params.TestChainConfig.Copy()
	key, _ := crypto.GenerateKey()
	from = crypto.PubkeyToAddress(key.PublicKey)
	gspec := &blockchain.Genesis{Config: config, Alloc: blockchain.GenesisAlloc{from: {Balance: new(big.Int).Mul(big.NewInt(1e6), big.NewInt(params.KAIA))}}}
	open := func() (*blockchain.BlockChain, database.DBManager) {
		db := database.NewMemoryDBManager()
		gspec.MustCommit(db)
		chain, err := blockchain.NewBlockChain(db, rangeCacheConfig(), config, faker.NewFaker(), vm.Config{})
		require.NoError(t, err)
		t.Cleanup(chain.Stop)
		return chain, db
	}
	src, srcDB = open()
	signer := types.LatestSignerForChainID(config.ChainID)
	blocks, _ = blockchain.GenerateChain(config, src.Genesis(), faker.NewFaker(), srcDB, n, func(i int, b *blockchain.BlockGen) {
		b.SetRewardbase(common.Address{0xbe})
		to := common.Address{0xaa, byte(i)}
		tx, err := types.SignTx(types.NewTransaction(b.TxNonce(from), to, big.NewInt(int64(i+1)), 21000, big.NewInt(params.Gkei*25), nil), signer, key)
		require.NoError(t, err)
		b.AddTx(tx)
		code := []byte{0x60, 0x01, 0x60, byte(i), 0x55, 0x00} // SSTORE(i, 1)
		tx, err = types.SignTx(types.NewContractCreation(b.TxNonce(from), common.Big0, 100000, big.NewInt(params.Gkei*25), code), signer, key)
		require.NoError(t, err)
		b.AddTx(tx)
	})
	_, err := src.InsertChain(blocks)
	require.NoError(t, err)
	first, next := src.States().BALs().Range()
	require.Equal(t, [2]uint64{1, uint64(n) + 1}, [2]uint64{first, next})
	dst, _ = open()
	return src, srcDB, dst, blocks, from
}

func storageReceipts(receipts types.Receipts) []*types.ReceiptForStorage {
	out := make([]*types.ReceiptForStorage, len(receipts))
	for i, r := range receipts {
		out[i] = (*types.ReceiptForStorage)(r)
	}
	return out
}

func TestBALSyncConfigure(t *testing.T) {
	genesis := func() common.Hash { return common.Hash{1} }
	s := NewSync(database.NewMemoryDBManager(), &balTestChain{}, nil)
	require.NoError(t, s.Configure("", false, genesis, nil))
	require.False(t, s.ApplyBALs())
	require.ErrorIs(t, s.Configure("", true, genesis, nil), errNoHashChain)
	require.ErrorIs(t, s.Configure(t.TempDir(), true, genesis, nil), errNoHashChain)
	require.Nil(t, s.HashChain())
}
