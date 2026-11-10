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

package access

import (
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

// TestBlockAccessRecorderProducer checks that the changes recorded by a block
// producer, which also runs txs it drops and restores bundles from a copy, hash
// the same as those of a node executing only the included txs.
// TestBlockAccessRecorderProducer checks that a block producer records the same
// list as a validator, reads included, although it reads state between txs,
// runs a tx it drops, and rolls back a bundle.
func TestBlockAccessRecorderProducer(t *testing.T) {
	var (
		a, b, c, d, e = common.HexToAddress("0xa"), common.HexToAddress("0xb"), common.HexToAddress("0xc"), common.HexToAddress("0xd"), common.HexToAddress("0xe")
		slot          = common.HexToHash("0x1")
	)
	db := state.NewDatabase(database.NewMemoryDBManager())
	base, err := state.New(common.Hash{}, db, nil, nil)
	require.NoError(t, err)
	for _, addr := range []common.Address{a, b, c, d, e} {
		base.AddBalance(addr, big.NewInt(100))
	}
	base.SetState(c, slot, common.HexToHash("0x2"))
	root, err := base.Commit(true)
	require.NoError(t, err)

	open := func(preload ...common.Address) *state.StateDB {
		s, err := state.New(root, db, nil, nil)
		require.NoError(t, err)
		for _, addr := range preload {
			s.GetBalance(addr)
		}
		EnableRecorder(s)
		return s
	}
	finish := func(s *state.StateDB) (common.Hash, []byte) {
		s.SetBlockAccessIndex(3)
		root := s.IntermediateRoot(true)
		enc, err := Recorded(s).Encode()
		require.NoError(t, err)
		return root, enc
	}

	// The producer has e loaded already. It runs tx 0, reads d between txs, runs
	// a tx that reads c and is dropped, a bundle that is rolled back, and then
	// tx 1, which reads c and e. The dropped tx loaded c first, so tx 1 finds it
	// cached.
	producer := open(e)
	producer.SetTxContext(common.Hash{1}, common.Hash{}, 0)
	producer.AddBalance(a, big.NewInt(1))
	producer.Finalise(true, true)

	producer.GetNonce(d)
	producer.SetTxContext(common.Hash{2}, common.Hash{}, 1)
	snap := producer.Snapshot()
	producer.GetState(c, slot)
	producer.GetBalance(d)
	producer.AddBalance(c, big.NewInt(5))
	producer.RevertToSnapshot(snap)

	producer.GetNonce(d)
	saved := producer.Copy()
	producer.SetTxContext(common.Hash{3}, common.Hash{}, 1)
	producer.GetBalance(d)
	producer.AddBalance(b, big.NewInt(2))
	producer.Finalise(true, true)
	producer.Set(saved)

	producer.SetTxContext(common.Hash{4}, common.Hash{}, 1)
	producer.GetState(c, slot)
	producer.GetBalance(e)
	producer.SetState(b, slot, common.HexToHash("0x3"))
	producer.Finalise(true, true)
	producer.GetNonce(d)
	producerRoot, producerList := finish(producer)

	// A validator runs the two included txs only.
	validator := open()
	validator.SetTxContext(common.Hash{1}, common.Hash{}, 0)
	validator.AddBalance(a, big.NewInt(1))
	validator.Finalise(true, true)
	validator.SetTxContext(common.Hash{4}, common.Hash{}, 1)
	validator.GetState(c, slot)
	validator.GetBalance(e)
	validator.SetState(b, slot, common.HexToHash("0x3"))
	validator.Finalise(true, true)
	validatorRoot, validatorList := finish(validator)

	require.Equal(t, validatorRoot, producerRoot)
	require.Equal(t, validatorList, producerList)

	list := Recorded(validator)
	addrs := make([]common.Address, len(list.Accounts))
	for i, acc := range list.Accounts {
		addrs[i] = acc.Address
	}
	require.Equal(t, []common.Address{a, b, c, e}, addrs, "c and e are read, d only outside of the included txs")
	require.Equal(t, []common.Hash{slot}, list.Accounts[2].StorageReads)
}
