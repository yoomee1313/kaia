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

package blockchain

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/blockchain/types/accountkey"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

type listSlice []*bal.BlockAccessList // item i is the list of block i+1

func (l listSlice) Read(number uint64) (*bal.BlockAccessList, error) {
	if number == 0 || number > uint64(len(l)) {
		return nil, fmt.Errorf("no list for block %d", number)
	}
	return l[number-1], nil
}

// accountFields encodes an account without its storage root, which History
// does not track.
func accountFields(t *testing.T, acc account.Account) []byte {
	if acc == nil {
		return nil
	}
	acc = acc.DeepCopy()
	if pa := account.GetProgramAccount(acc); pa != nil {
		pa.SetStorageRoot(common.ExtHash{})
	}
	enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(acc))
	require.NoError(t, err)
	return enc
}

// TestBALHistory indexes the lists of a chain with transfers, storage writes,
// key updates, EIP-7702 delegation (prague), self-destruction and recreation
// (shanghai, before EIP-6780), then checks every touched value at every block
// against the state.
func TestBALHistory(t *testing.T) {
	t.Run("shanghai", func(t *testing.T) { testBALHistory(t, "shanghai") })
	t.Run("prague", func(t *testing.T) { testBALHistory(t, "prague") })
}

func testBALHistory(t *testing.T, hardfork string) {
	env := newBALTestEnv(t, hardfork, 6)
	addrs := env.addrs
	var (
		newEOA   = common.HexToAddress("0x00000000000000000000000000000000000e0a02")
		untouch  = common.HexToAddress("0x00000000000000000000000000000000000e0a03")
		contract = crypto.CreateAddress(addrs[0], 0)
		initCode = balInitCode(balDestructibleCode())
		child    = crypto.CreateAddress2(balFactory, [32]byte{}, crypto.Keccak256(initCode))
		roleKeys = []*ecdsa.PrivateKey{mustKey(t), mustKey(t), mustKey(t)}
	)
	roleBased := accountkey.NewAccountKeyRoleBasedWithValues(accountkey.AccountKeyRoleBased{
		accountkey.NewAccountKeyPublicWithValue(&roleKeys[0].PublicKey),
		accountkey.NewAccountKeyPublicWithValue(&roleKeys[1].PublicKey),
		accountkey.NewAccountKeyPublicWithValue(&roleKeys[2].PublicKey),
	})

	chain, blocks := env.generate(9, func(i int, b *BlockGen) {
		b.SetRewardbase(common.Address{0xbe, byte(i % 3)})
		switch i {
		case 0:
			b.AddTx(env.legacyTx(b, 0, nil, common.Big0, balInitCode(balStoreCode())))
			b.AddTx(env.legacyTx(b, 1, &newEOA, big.NewInt(params.KAIA), nil))
		case 1:
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, balSlotData(1, 0)))
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, balSlotData(5, 0x55)))
			b.AddTx(env.legacyTx(b, 2, &balFactory, common.Big0, initCode))
		case 2:
			b.AddTx(env.kaiaTx(types.TxTypeAccountUpdate, map[types.TxValueKeyType]interface{}{
				types.TxValueKeyNonce:      b.TxNonce(addrs[3]),
				types.TxValueKeyFrom:       addrs[3],
				types.TxValueKeyAccountKey: roleBased,
			}, env.keys[3:4], nil))
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, balSlotData(3, 0x33)))
		case 3:
			// Destroy, recreate with fresh storage, write again.
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, nil))
			b.AddTx(env.legacyTx(b, 2, &balFactory, common.Big0, initCode))
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, balSlotData(4, 0x44)))
		case 4:
			if hardfork == "prague" {
				b.AddTx(env.setCodeTx(b, 4, balStoreAddr, nil))
			}
		case 5:
			// Destroy and leave it deleted.
			b.AddTx(env.legacyTx(b, 2, &child, common.Big0, nil))
			if hardfork == "prague" {
				b.AddTx(env.legacyTx(b, 4, &addrs[4], common.Big0, balSlotData(7, 0x77)))
			}
		case 7:
			// Recreate in a later block.
			b.AddTx(env.legacyTx(b, 2, &balFactory, common.Big0, initCode))
			b.AddTx(env.legacyTx(b, 0, &contract, common.Big0, balSlotData(5, 0)))
		}
		b.AddTx(env.legacyTx(b, 5, &addrs[1], big.NewInt(int64(i+1)), nil))
	})
	lists := checkBAL(t, chain, blocks)

	// Index with several flushes and a reopen, so keys span chunks.
	db := database.NewMemDB()
	builder, err := bal.NewIndexBuilder(db, 0, env.genesis.Root())
	require.NoError(t, err)
	for i, list := range lists {
		if i == 4 {
			builder, err = bal.NewIndexBuilder(db, 0, env.genesis.Root())
			require.NoError(t, err)
			require.Equal(t, uint64(5), builder.Next())
		}
		require.NoError(t, builder.Add(uint64(i+1), list))
		if i%2 == 1 {
			require.NoError(t, builder.Flush())
		}
	}
	require.NoError(t, builder.Flush())
	_, err = bal.NewIndexBuilder(db, 1, env.genesis.Root())
	require.Error(t, err)

	index, err := bal.OpenIndex(db)
	require.NoError(t, err)
	require.Equal(t, bal.IndexMeta{Base: 0, BaseRoot: env.genesis.Root(), Next: uint64(len(blocks) + 1)}, index.Meta())
	base, err := chain.StateAt(env.genesis.Root())
	require.NoError(t, err)
	history := bal.NewHistory(index, listSlice(lists), state.NewBALBase(base))

	// Everything touched, plus accounts never touched.
	accounts := map[common.Address]bool{untouch: true, balSystemAddr: true}
	slots := map[common.Address]map[common.Hash]bool{balSystemAddr: {{}: true}}
	for _, list := range lists {
		for _, a := range list.Accounts {
			accounts[a.Address] = true
			for _, w := range a.StorageWrites {
				if slots[a.Address] == nil {
					slots[a.Address] = make(map[common.Hash]bool)
				}
				slots[a.Address][w.Slot] = true
			}
		}
	}
	for _, addr := range []common.Address{contract, child} {
		require.True(t, accounts[addr], addr.Hex())
	}

	headers := []*types.Header{env.genesis.Header()}
	for _, block := range blocks {
		headers = append(headers, block.Header())
	}
	for number, header := range headers {
		n := uint64(number)
		statedb, err := chain.StateAt(header.Root)
		require.NoError(t, err)
		for addr := range accounts {
			msg := fmt.Sprintf("block %d account %s", n, addr.Hex())
			balance, err := history.Balance(addr, n)
			require.NoError(t, err, msg)
			require.Equal(t, statedb.GetBalance(addr), balance, msg)
			nonce, err := history.Nonce(addr, n)
			require.NoError(t, err, msg)
			require.Equal(t, statedb.GetNonce(addr), nonce, msg)
			code, err := history.Code(addr, n)
			require.NoError(t, err, msg)
			require.Equal(t, common.CopyBytes(statedb.GetCode(addr)), common.CopyBytes(code), msg)
			acc, err := history.Account(addr, n)
			require.NoError(t, err, msg)
			require.Equal(t, accountFields(t, statedb.GetAccount(addr)), accountFields(t, acc), msg)
			for slot := range slots[addr] {
				value, err := history.Storage(addr, slot, n)
				require.NoError(t, err, msg)
				require.Equal(t, statedb.GetState(addr, slot), value, "%s slot %x", msg, slot)
			}
		}
	}

	_, err = history.Balance(addrs[0], uint64(len(blocks)+1))
	require.ErrorIs(t, err, bal.ErrNotIndexed)
	if hardfork != "shanghai" {
		return
	}
	// The child was deleted at block 6 and recreated at block 8 with fresh storage.
	value, err := history.Storage(child, common.BigToHash(big.NewInt(4)), 7)
	require.NoError(t, err)
	require.Equal(t, common.Hash{}, value)
	acc, err := history.Account(child, 7)
	require.NoError(t, err)
	require.Nil(t, acc)
	value, err = history.Storage(child, common.BigToHash(big.NewInt(4)), 8)
	require.NoError(t, err)
	require.Equal(t, common.Hash{}, value)
	value, err = history.Storage(child, common.BigToHash(big.NewInt(1)), 8)
	require.NoError(t, err)
	require.Equal(t, common.BigToHash(big.NewInt(0x11)), value)
}
