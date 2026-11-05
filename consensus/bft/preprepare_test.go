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

package bft_test

import (
	"math/big"
	"testing"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/consensus/bft"
	"github.com/kaiachain/kaia/rlp"
	"github.com/stretchr/testify/require"
)

// A PRE-PREPARE without a block access list encodes as before Amber, and one
// with it round-trips the list.
func TestPreprepareAccessList(t *testing.T) {
	view := &bft.View{Round: big.NewInt(1), Sequence: big.NewInt(10)}
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(10), Time: big.NewInt(0)})

	legacy, err := rlp.EncodeToBytes([]any{view, block})
	require.NoError(t, err)
	enc, err := bft.Encode(&bft.Preprepare{View: view, Proposal: block})
	require.NoError(t, err)
	require.Equal(t, legacy, enc, "no access list: the pre-Amber encoding")
	var pp bft.Preprepare
	require.NoError(t, rlp.DecodeBytes(enc, &pp))
	require.Empty(t, pp.AccessList)
	require.Equal(t, block.Hash(), pp.Proposal.Hash())

	list := []byte{0xc1, 0xc0}
	enc, err = bft.Encode(&bft.Preprepare{View: view, Proposal: block, AccessList: list})
	require.NoError(t, err)
	require.NotEqual(t, legacy, enc)
	pp = bft.Preprepare{}
	require.NoError(t, rlp.DecodeBytes(enc, &pp))
	require.Equal(t, list, pp.AccessList)
	require.Equal(t, block.Hash(), pp.Proposal.Hash())
	require.Equal(t, 0, pp.View.Cmp(view))
}
