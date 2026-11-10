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

package backend

import (
	"crypto/ecdsa"
	"math/big"
	"testing"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/statedb"
	"github.com/stretchr/testify/require"
)

// balSyncContexts returns a chain of n blocks sealed by 4 validators, executed
// by one node, and a second node of the same chain at genesis.
func balSyncContexts(t *testing.T, n int, cacheConfig *blockchain.CacheConfig, livePruning bool) (*testContext, *testContext, []*bal.Block) {
	keys := make([]*ecdsa.PrivateKey, 4)
	for i := range keys {
		keys[i], _ = crypto.GenerateKey()
	}
	genesisTime := uint64(time.Now().Unix()) - uint64(n) - 100
	sender, _ := crypto.GenerateKey()
	alloc := blockchain.GenesisAlloc{crypto.PubkeyToAddress(sender.PublicKey): {Balance: new(big.Int).Mul(big.NewInt(1e6), big.NewInt(params.KAIA))}}
	overrides := func(cc *blockchain.CacheConfig, livePruning bool) *testOverrides {
		return &testOverrides{node0Key: keys[0], nodeKeys: keys, genesisTime: genesisTime, cacheConfig: cc, alloc: alloc, livePruning: livePruning}
	}
	src := newTestContext(4, testKoreConfig, overrides(nil, false))
	t.Cleanup(src.Cleanup)
	parent := src.chain.Genesis()
	var blocks []*bal.Block
	for i := 0; i < n; i++ {
		block := makeSealedBlock(t, src, parent, sender, true)
		_, err := src.chain.InsertChain(types.Blocks{block})
		require.NoError(t, err)
		require.Equal(t, block.Hash(), src.chain.CurrentBlock().Hash(), "block %d", i+1)

		sdb, err := src.chain.StateAt(parent.Root())
		require.NoError(t, err)
		access.EnableRecorder(sdb)
		_, _, _, _, _, err = src.chain.Processor().Process(block, sdb, vm.Config{})
		require.NoError(t, err)
		enc, err := access.Recorded(sdb).Writes().Encode()
		require.NoError(t, err)
		blocks = append(blocks, &bal.Block{Block: block, Receipts: src.chain.GetReceiptsByBlockHash(block.Hash()), BAL: enc})
		parent = block
	}
	dst := newTestContext(4, testKoreConfig, overrides(cacheConfig, livePruning))
	t.Cleanup(dst.Cleanup)
	require.Equal(t, src.chain.Genesis().Hash(), dst.chain.Genesis().Hash())
	return src, dst, blocks
}

// makeSealedBlock makes a block one second after its parent, with a transfer
// and a contract deployment from sender if given, the proposer seal and, if
// committed, the committed seals of all validators.
func makeSealedBlock(t *testing.T, ctx *testContext, parent *types.Block, sender *ecdsa.PrivateKey, committed bool) *types.Block {
	header := ctx.MakeHeader(parent)
	require.NoError(t, ctx.chain.PrepareHeader(header))
	header.Time = new(big.Int).Add(parent.Time(), common.Big1)
	sdb, err := ctx.chain.StateAt(parent.Root())
	require.NoError(t, err)
	require.NoError(t, ctx.chain.Processor().InitializeState(header, sdb))
	var (
		txs      types.Transactions
		receipts types.Receipts
		usedGas  uint64
	)
	if sender != nil {
		signer := types.LatestSignerForChainID(ctx.config.ChainID)
		from := crypto.PubkeyToAddress(sender.PublicKey)
		to := common.Address{0xaa, byte(header.Number.Uint64())}
		code := []byte{0x60, 0x01, 0x60, 0x00, 0x55, 0x00} // SSTORE(0, 1)
		for i, tx := range []*types.Transaction{
			types.NewTransaction(sdb.GetNonce(from), to, big.NewInt(params.KAIA), 21000, big.NewInt(750*params.Gkei), nil),
			types.NewContractCreation(sdb.GetNonce(from)+1, common.Big0, 100000, big.NewInt(750*params.Gkei), code),
		} {
			tx, err := types.SignTx(tx, signer, sender)
			require.NoError(t, err)
			sdb.SetTxContext(tx.Hash(), header.Hash(), i)
			receipt, _, err := ctx.chain.ApplyTransaction(ctx.config, &header.Rewardbase, sdb, header, tx, &usedGas, &vm.Config{})
			require.NoError(t, err)
			txs, receipts = append(txs, tx), append(receipts, receipt)
		}
	}
	header.GasUsed = usedGas
	block, err := ctx.chain.Processor().FinalizeState(header, sdb, txs, receipts)
	require.NoError(t, err)
	block, err = ctx.engine.updateBlock(block)
	require.NoError(t, err)
	if !committed {
		return block
	}
	header = block.Header()
	seals, err := ctx.MakeCommittedSeals(header)
	require.NoError(t, err)
	require.NoError(t, ctx.engine.sealer.WriteCommittedSeals(header, seals))
	return block.WithSeal(header)
}

func balCacheConfig() *blockchain.CacheConfig {
	return &blockchain.CacheConfig{
		CacheSize:           512,
		BlockInterval:       blockchain.DefaultBlockInterval,
		TriesInMemory:       blockchain.DefaultTriesInMemory,
		TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
		BALRecord:           true,
	}
}

// TestInsertChainWithBALs syncs a node from genesis by applying the block
// access lists of a sealed chain: the committed seals are verified, and a
// block without them, a wrong list or a wrong receipt is rejected.
func TestInsertChainWithBALs(t *testing.T) {
	src, dst, blocks := balSyncContexts(t, 24, balCacheConfig(), false)
	head := len(blocks)

	// A block without committed seals is rejected before its list is applied.
	unsealed := makeSealedBlock(t, dst, dst.chain.Genesis(), nil, false)
	_, err := bal.InsertChain(dst.chain, []*bal.Block{{Block: unsealed, Receipts: blocks[0].Receipts, BAL: blocks[0].BAL}})
	require.Error(t, err)

	// A wrong list fails the state root, a wrong receipt the receipts root.
	list, err := access.Decode(blocks[0].BAL)
	require.NoError(t, err)
	tampered := false
	for i := range list.Accounts {
		if c := list.Accounts[i].BalanceChanges; len(c) > 0 {
			c[len(c)-1].Balance = new(big.Int).Add(c[len(c)-1].Balance, common.Big1)
			tampered = true
			break
		}
	}
	require.True(t, tampered, "the block mints a reward")
	wrongList, err := list.Encode()
	require.NoError(t, err)
	_, err = bal.InsertChain(dst.chain, []*bal.Block{{Block: blocks[0].Block, Receipts: blocks[0].Receipts, BAL: wrongList}})
	require.ErrorContains(t, err, "root")
	if len(blocks[3].Receipts) > 0 {
		receipts := types.Receipts{{Status: blocks[3].Receipts[0].Status, GasUsed: blocks[3].Receipts[0].GasUsed + 1}}
		_, err = bal.InsertChain(dst.chain, []*bal.Block{{Block: blocks[3].Block, Receipts: receipts, BAL: blocks[3].BAL}})
		require.Error(t, err)
	}
	require.Zero(t, dst.chain.CurrentBlock().NumberU64())

	// The sealed chain applies, in batches, to the same states and receipts.
	for i := 0; i < head; i += 10 {
		_, err := bal.InsertChain(dst.chain, blocks[i:min(i+10, head)])
		require.NoError(t, err)
	}
	require.Equal(t, blocks[head-1].Block.Hash(), dst.chain.CurrentBlock().Hash())
	for _, b := range blocks {
		hash, n := b.Block.Hash(), b.Block.NumberU64()
		want, err := rlp.EncodeToBytes(storageReceipts(src.chain.GetReceiptsByBlockHash(hash)))
		require.NoError(t, err)
		got, err := rlp.EncodeToBytes(storageReceipts(dst.chain.GetReceiptsByBlockHash(hash)))
		require.NoError(t, err)
		require.Equal(t, want, got, "block %d", n)
		require.Equal(t, b.BAL, bal.ByHash(dst.chain, hash, n), "block %d", n)
		require.True(t, dst.chain.HasState(b.Block.Root()), "block %d", n)
	}
	first, next := bal.Range(dst.chain)
	require.Equal(t, [2]uint64{1, uint64(head) + 1}, [2]uint64{first, next})
}

func storageReceipts(receipts types.Receipts) []*types.ReceiptForStorage {
	out := make([]*types.ReceiptForStorage, len(receipts))
	for i, r := range receipts {
		out[i] = (*types.ReceiptForStorage)(r)
	}
	return out
}

// TestLightSyncFromGenesis starts a light node from genesis: it applies the
// block access lists of a sealed chain with live pruning, verifying every
// committed seal and state root, deletes the blocks older than its retention,
// and keeps inserting blocks after that.
func TestLightSyncFromGenesis(t *testing.T) {
	defer func(n uint64) { bal.MinHistoryBlocks = n }(bal.MinHistoryBlocks)
	bal.MinHistoryBlocks = 4
	cacheConfig := balCacheConfig()
	cacheConfig.BALNoIndex = true
	cacheConfig.LivePruningRetention = 4
	cacheConfig.HistoryRetention = 10 * time.Second // blocks are one second apart
	src, dst, blocks := balSyncContexts(t, 40, cacheConfig, true)
	require.True(t, dst.chain.IsLivePruningRequired())

	_, err := bal.InsertChain(dst.chain, blocks[:30])
	require.NoError(t, err)
	require.NoError(t, bal.PruneHistory(dst.chain))
	first := bal.HistoryFirst(dst.chain)
	require.Equal(t, uint64(20), first, "blocks 20-30 are within 10 seconds of the head")
	for _, b := range blocks[:30] {
		n := b.Block.NumberU64()
		require.Equal(t, b.Block.Hash(), dst.chain.StateCache().TrieDB().DiskDB().ReadCanonicalHash(n), "block %d", n)
		if n < first {
			require.Nil(t, dst.chain.GetHeaderByNumber(n), "block %d", n)
			require.Nil(t, dst.chain.GetReceiptsByBlockHash(b.Block.Hash()), "block %d", n)
			require.Nil(t, bal.ByHash(dst.chain, b.Block.Hash(), n), "block %d", n)
			require.ErrorIs(t, bal.PrunedError(dst.chain, n), bal.ErrPrunedHistory)
		} else {
			require.NotNil(t, dst.chain.GetBlockByNumber(n), "block %d", n)
		}
	}
	balFirst, _ := bal.Range(dst.chain)
	require.Equal(t, first, balFirst)

	// The seals of later blocks still verify, and a wrong one still fails.
	unsealed := makeSealedBlock(t, src, blocks[29].Block, nil, false)
	_, err = bal.InsertChain(dst.chain, []*bal.Block{{Block: unsealed, BAL: blocks[30].BAL}})
	require.Error(t, err)
	_, err = bal.InsertChain(dst.chain, blocks[30:])
	require.NoError(t, err)
	head := dst.chain.CurrentBlock()
	require.Equal(t, blocks[39].Block.Hash(), head.Hash())
	require.Equal(t, src.chain.CurrentBlock().Root(), head.Root())
	require.True(t, dst.chain.HasState(head.Root()))
}
