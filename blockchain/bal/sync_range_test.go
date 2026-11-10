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
	"context"
	"crypto/ed25519"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

func rangeCacheConfig() *blockchain.CacheConfig {
	return &blockchain.CacheConfig{
		CacheSize:           512,
		BlockInterval:       blockchain.DefaultBlockInterval,
		TriesInMemory:       blockchain.DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
		BALRecord:           true,
		BALNoIndex:          true,
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
	first, next := Range(src)
	require.Equal(t, [2]uint64{1, uint64(n) + 1}, [2]uint64{first, next})
	dst, _ = open()
	return src, srcDB, dst, blocks, from
}

// TestBALSyncRanges syncs a node from genesis by applying the BALs of blocks
// fetched by segments from peers, up to the last block of the hash chain:
// nothing is applied without a chain, a peer whose BAL fails the state root
// is dropped, one whose BAL fails the chain is dropped before anything is
// applied, and the blocks after the chain are left to be executed.
func TestBALSyncRanges(t *testing.T) {
	defer func(d time.Duration) { balSyncNoPeer = d }(balSyncNoPeer)
	balSyncNoPeer = 300 * time.Millisecond
	src, srcDB, dst, blocks, from := newRangeChains(t, 40)
	head := uint64(len(blocks))
	ctx := context.Background()
	server := func() *Sync { return NewSync(srcDB, src, nil) }

	var dropped []string
	client := NewSync(database.NewMemoryDBManager(), dst, nil)
	client.drop = func(id string) { dropped = append(dropped, id); client.RemovePeer(id) }
	tamperBAL := func(n uint64, enc []byte) []byte {
		list, err := access.Decode(enc)
		require.NoError(t, err)
		for i := range list.Accounts {
			if a := &list.Accounts[i]; a.Address == (common.Address{0xaa, byte(n - 1)}) {
				c := a.BalanceChanges
				c[len(c)-1].Balance = new(big.Int).Add(c[len(c)-1].Balance, common.Big1)
			}
		}
		enc, err = list.Encode()
		require.NoError(t, err)
		return enc
	}
	tamper := func(n uint64) func(code uint64, data interface{}) interface{} {
		return func(code uint64, data interface{}) interface{} {
			if res, ok := data.(*blocksResponse); ok {
				for _, b := range res.Blocks {
					if b.Header.Number.Uint64() == n {
						b.BAL = tamperBAL(n, b.BAL)
					}
				}
			}
			return data
		}
	}
	hashChain := func(last, tampered uint64) *HashChain {
		chain, err := BuildHashChain(last, func(n uint64) (common.Hash, error) {
			if n > 0 && n == tampered {
				enc, err := ReadRaw(srcDB, n)
				require.NoError(t, err)
				return crypto.Keccak256Hash(tamperBAL(n, enc)), nil
			}
			return HashOf(srcDB, n)
		})
		require.NoError(t, err)
		return chain
	}

	// Without a hash chain nothing is applied.
	connect(client, server(), "a-bad", tamper(20))
	waitRanges(t, client, 1)
	n, err := client.SyncRanges(ctx, chainInserter{dst}, nil)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, dst.CurrentBlock().NumberU64())

	// A wrong BAL that the chain has fails the state root of the batch it is
	// applied in; the peer is dropped and the chain is back at the last
	// checked block.
	_, err = client.SyncRanges(ctx, chainInserter{dst}, hashChain(head, 20))
	require.Error(t, err)
	require.Equal(t, []string{"a-bad"}, dropped)
	require.Zero(t, dst.CurrentBlock().NumberU64(), "the batch is rolled back")

	// With the right chain, a wrong BAL is dropped on arrival, and the
	// blocks after the last block of the chain are not applied.
	last := head - 10
	chain := hashChain(last, 0)
	connect(client, server(), "b-bad", tamper(7))
	good := connect(client, server(), "c-good", nil)
	waitRanges(t, client, 2)
	n, err = client.SyncRanges(ctx, chainInserter{dst}, chain)
	require.NoError(t, err)
	require.Equal(t, int(last), n)
	require.Equal(t, []string{"a-bad", "b-bad"}, dropped)
	require.NotZero(t, good.requestCount())
	require.Equal(t, last, dst.CurrentBlock().NumberU64(), "no block waits for its child")
	for _, block := range blocks[:last] {
		hash, n := block.Hash(), block.NumberU64()
		require.Equal(t, hash, dst.GetHeaderByNumber(n).Hash())
		want, err := rlp.EncodeToBytes(storageReceipts(src.GetReceiptsByBlockHash(hash)))
		require.NoError(t, err)
		got, err := rlp.EncodeToBytes(storageReceipts(dst.GetReceiptsByBlockHash(hash)))
		require.NoError(t, err)
		require.Equal(t, want, got, "block %d", n)
		require.Equal(t, ByHash(src, hash, n), ByHash(dst, hash, n), "block %d", n)
	}
	sdb, err := dst.StateAt(dst.CurrentBlock().Root())
	require.NoError(t, err)
	require.Equal(t, common.BigToHash(common.Big1), sdb.GetState(crypto.CreateAddress(from, blocks[0].Transactions()[1].Nonce()), common.Hash{}), "the storage the deployment wrote")

	// Nothing more to apply: the rest is executed.
	n, err = client.SyncRanges(ctx, chainInserter{dst}, chain)
	require.NoError(t, err)
	require.Zero(t, n)
	client.applyBALs, client.hashChain = true, chain
	require.False(t, client.Synchronise(dst), "the downloader executes the blocks after the chain")
	_, err = dst.InsertChain(blocks[last:])
	require.NoError(t, err)
	require.Equal(t, head, dst.CurrentBlock().NumberU64())
}

// TestBALSyncConfigure loads the signed hash chain a node applying BALs
// needs, and fails to start the node without a valid one.
func TestBALSyncConfigure(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	defer func(k ed25519.PublicKey) { HashChainPublicKey = k }(HashChainPublicKey)
	HashChainPublicKey = pub
	genesis := common.HexToHash("0x9e")
	genesisOf := func() common.Hash { return genesis }
	chain, err := BuildHashChain(100, func(n uint64) (common.Hash, error) { return common.BigToHash(new(big.Int).SetUint64(n)), nil })
	require.NoError(t, err)
	root := t.TempDir()
	_, err = WriteHashChainPackage(filepath.Join(root, "node_modules", HashChainPackage), genesis, chain, key, "1.0.0")
	require.NoError(t, err)

	configure := func(hashChain string, apply bool) (*Sync, error) {
		s := NewSync(database.NewMemoryDBManager(), nil, nil)
		return s, s.Configure(hashChain, apply, genesisOf, nil)
	}
	s, err := configure(root, true)
	require.NoError(t, err)
	require.Equal(t, chain, s.hashChain)
	require.True(t, s.applyBALs)

	_, err = configure("", true)
	require.ErrorContains(t, err, "npm install "+HashChainPackage, "no package")
	require.ErrorContains(t, err, "--sync.apply-bals=false")
	_, err = configure(filepath.Join(root, "missing"), true)
	require.ErrorContains(t, err, "npm install", "a missing package")
	s, err = configure("", false)
	require.NoError(t, err, "a node executing blocks needs no chain")
	require.Nil(t, s.hashChain)

	other, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	HashChainPublicKey = other
	_, err = configure(root, true)
	require.ErrorIs(t, err, ErrHashChainSignature, "another key")
	_, err = configure(root, false)
	require.ErrorIs(t, err, ErrHashChainSignature, "a given chain is checked even if not applied")
}

func storageReceipts(receipts types.Receipts) []*types.ReceiptForStorage {
	out := make([]*types.ReceiptForStorage, len(receipts))
	for i, r := range receipts {
		out[i] = (*types.ReceiptForStorage)(r)
	}
	return out
}
