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

package kaiabft

import (
	"math/big"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/bft"
	"github.com/kaiachain/kaia/crypto"
	valset_mock "github.com/kaiachain/kaia/kaiax/valset/mock"
	"github.com/kaiachain/kaia/rlp"
	"github.com/stretchr/testify/require"
)

// fragmentEnv is a validator at (10, 0) whose proposer sends block in
// fragments.
type fragmentEnv struct {
	m        *machine
	self     common.Address
	proposer common.Address
	block    *types.Block
	hint     []byte
}

func newFragmentEnv(t *testing.T) *fragmentEnv {
	proposal, hint := amberBlock(t, 10)
	parent := newEmptyBlock(9, common.Hash{})
	header := proposal.Header()
	header.ParentHash = parent.Hash()
	var txs types.Transactions
	txKey, err := crypto.GenerateKey()
	require.NoError(t, err)
	signer := types.LatestSignerForChainID(big.NewInt(1))
	for i := range 5 {
		tx, err := types.SignTx(types.NewTransaction(uint64(i), common.HexToAddress("0x0a"), common.Big1, 21000, common.Big1, nil), signer, txKey)
		require.NoError(t, err)
		txs = append(txs, tx)
	}
	block := types.NewBlock(header, txs, nil)

	chain := &fakeChain{cfg: amberConfig(), current: parent, headers: map[common.Hash]*types.Header{parent.Hash(): parent.Header()}}
	b, key := newTestBackend(t, chain)
	b.parallel = &fakeParallel{}
	self := crypto.PubkeyToAddress(key.PublicKey)
	proposerKey, err := crypto.GenerateKey()
	require.NoError(t, err)
	proposer := crypto.PubkeyToAddress(proposerKey.PublicKey)
	mValset := valset_mock.NewMockValsetModule(gomock.NewController(t))
	mValset.EXPECT().GetProposer(gomock.Any(), gomock.Any()).Return(proposer, nil).AnyTimes()
	mValset.EXPECT().GetCommittee(gomock.Any(), gomock.Any()).Return([]common.Address{self, proposer}, nil).AnyTimes()
	b.valsetModule = mValset

	m := newTestMachine(b, 10, 0, nil, []common.Address{self, proposer})
	t.Cleanup(m.stopTimer)
	m.state = stateAcceptRequest
	m.proposer = proposer
	return &fragmentEnv{m: m, self: self, proposer: proposer, block: block, hint: hint}
}

// fragment returns the part of a fragment of the txs [start, end) of the block.
func (e *fragmentEnv) fragment(t *testing.T, start, end int) bft.Part {
	empty, err := (&access.BlockAccessList{}).Encode()
	require.NoError(t, err)
	data, err := rlp.EncodeToBytes(&bft.Fragment{Start: uint64(start), Txs: e.block.Transactions()[start:end], Hint: empty})
	require.NoError(t, err)
	return bft.Part{Kind: bft.PartFragment, Data: data}
}

func (e *fragmentEnv) fragmentMsg(t *testing.T, seq int64, part bft.Part) *wireMessage {
	pp := &bft.Preprepare{View: &bft.View{Sequence: big.NewInt(seq), Round: big.NewInt(0)}, Proposal: fragmentStub(uint64(seq), e.block.ParentHash()), Parts: []bft.Part{part}}
	return &wireMessage{Code: bft.MsgPreprepare, Msg: mustEncode(t, pp)}
}

func (e *fragmentEnv) finalMsg(t *testing.T, txCount int, parts ...bft.Part) *wireMessage {
	hashes := make([]common.Hash, len(parts))
	for i, p := range parts {
		hashes[i] = crypto.Keccak256Hash(p.Data)
	}
	final, err := rlp.EncodeToBytes(&bft.Final{TxCount: uint64(txCount), Fragments: hashes})
	require.NoError(t, err)
	pp := &bft.Preprepare{
		View:     &bft.View{Sequence: big.NewInt(10), Round: big.NewInt(0)},
		Proposal: types.NewBlockWithHeader(e.block.Header()),
		Parts:    bft.WithAccessList([]bft.Part{{Kind: bft.PartFinal, Data: final}}, e.hint),
	}
	return &wireMessage{Code: bft.MsgPreprepare, Msg: mustEncode(t, pp)}
}

// A fragment is kept and changes no state; one of another sender, or of a
// sequence too far, is refused.
func TestFragmentChangesNoState(t *testing.T) {
	e := newFragmentEnv(t)
	msgs, stop := captureMessages(e.m.b)
	defer stop()

	part := e.fragment(t, 0, 2)
	require.NoError(t, e.m.handlePreprepare(e.fragmentMsg(t, 10, part), e.proposer))
	require.Equal(t, stateAcceptRequest, e.m.state)
	require.Nil(t, e.m.preprepare)
	require.Len(t, e.m.fragments[fragmentKey{10, 0}].byHash, 1)
	expectNoMessage(t, msgs)

	// The next sequence, before this node gets there.
	require.NoError(t, e.m.handlePreprepare(e.fragmentMsg(t, 11, part), e.proposer))
	require.Len(t, e.m.fragments[fragmentKey{11, 0}].byHash, 1)

	require.ErrorIs(t, e.m.handlePreprepare(e.fragmentMsg(t, 12, part), e.proposer), errFragmentView)
	require.ErrorIs(t, e.m.handlePreprepare(e.fragmentMsg(t, 10, part), e.self), errFragmentNotFromProposer)

	// Fragments of the past sequences go with a new sequence.
	e.m.sequence = big.NewInt(11)
	e.m.clearFragments()
	require.NotContains(t, e.m.fragments, fragmentKey{10, 0})
	require.Contains(t, e.m.fragments, fragmentKey{11, 0})
}

// The final message is accepted as the PRE-PREPARE of the block assembled from
// its fragments.
func TestFinalAssemblesProposal(t *testing.T) {
	e := newFragmentEnv(t)
	first, second := e.fragment(t, 0, 2), e.fragment(t, 2, 5)
	require.NoError(t, e.m.handlePreprepare(e.fragmentMsg(t, 10, second), e.proposer))
	require.NoError(t, e.m.handlePreprepare(e.fragmentMsg(t, 10, first), e.proposer))

	require.NoError(t, e.m.handlePreprepare(e.finalMsg(t, 5, first, second), e.proposer))
	require.Equal(t, statePreprepared, e.m.state)
	require.Equal(t, e.block.Hash(), e.m.preprepare.Proposal.Hash())
	require.Len(t, e.m.preprepare.Proposal.(*types.Block).Transactions(), 5)
	require.Equal(t, e.hint, e.m.preprepare.AccessList())
}

// A final message waits for a missing fragment, and is handled again when it
// comes.
func TestFinalWaitsForMissingFragment(t *testing.T) {
	e := newFragmentEnv(t)
	first, second := e.fragment(t, 0, 2), e.fragment(t, 2, 5)
	require.NoError(t, e.m.handlePreprepare(e.fragmentMsg(t, 10, first), e.proposer))

	final := e.finalMsg(t, 5, first, second)
	require.ErrorIs(t, e.m.handlePreprepare(final, e.proposer), errFragmentsMissing)
	require.Equal(t, stateAcceptRequest, e.m.state)
	require.NotNil(t, e.m.fragments[fragmentKey{10, 0}].pending)

	require.NoError(t, e.m.handlePreprepare(e.fragmentMsg(t, 10, second), e.proposer))
	require.Nil(t, e.m.fragments[fragmentKey{10, 0}].pending, "the final message is posted again")
	require.NoError(t, e.m.handlePreprepare(final, e.proposer))
	require.Equal(t, statePreprepared, e.m.state)
}

// Fragments that do not make the txs of the final message are refused.
func TestFinalWithWrongFragments(t *testing.T) {
	e := newFragmentEnv(t)
	first, overlap := e.fragment(t, 0, 2), e.fragment(t, 1, 5)
	require.NoError(t, e.m.handlePreprepare(e.fragmentMsg(t, 10, first), e.proposer))
	require.NoError(t, e.m.handlePreprepare(e.fragmentMsg(t, 10, overlap), e.proposer))
	require.ErrorIs(t, e.m.handlePreprepare(e.finalMsg(t, 5, first, overlap), e.proposer), errFragmentsInvalid)

	e2 := newFragmentEnv(t)
	all := e2.fragment(t, 0, 5)
	require.NoError(t, e2.m.handlePreprepare(e2.fragmentMsg(t, 10, all), e2.proposer))
	require.ErrorIs(t, e2.m.handlePreprepare(e2.finalMsg(t, 4, all), e2.proposer), errFragmentsInvalid)
	require.Equal(t, stateAcceptRequest, e2.m.state)
}

// The hints of the fragments count against the consensus limit of the hint.
func TestFragmentHintsCountAgainstLimit(t *testing.T) {
	defer func(limit uint64) { accessListHintEntryLimit = limit }(accessListHintEntryLimit)
	e := newFragmentEnv(t)
	hint, err := access.Decode(e.hint)
	require.NoError(t, err)
	b := access.NewBuilder()
	b.ReadAccount(common.HexToAddress("0xf1"))
	fragHint, err := b.Build().Encode()
	require.NoError(t, err)
	data, err := rlp.EncodeToBytes(&bft.Fragment{Start: 0, Txs: e.block.Transactions(), Hint: fragHint})
	require.NoError(t, err)
	part := bft.Part{Kind: bft.PartFragment, Data: data}
	require.NoError(t, e.m.handlePreprepare(e.fragmentMsg(t, 10, part), e.proposer))

	accessListHintEntryLimit = hint.EntryCount() // the fragment adds one
	require.ErrorIs(t, e.m.handlePreprepare(e.finalMsg(t, 5, part), e.proposer), errAccessListHintEntryLimit)
	require.Equal(t, stateAcceptRequest, e.m.state)
}

// A proposer sends its proposal as a final message only if it sent all its
// fragments in the current view.
func TestFinalPartsNeedSentFragments(t *testing.T) {
	e := newFragmentEnv(t)
	e.m.proposer = e.self
	first, second := e.fragment(t, 0, 2), e.fragment(t, 2, 5)
	request := &bft.Request{Proposal: e.block, AccessList: e.hint, Fragments: []bft.Part{first, second}}
	require.Nil(t, e.m.finalParts(request))

	e.m.sendFragment(fragmentEvent{Number: 10, Parent: e.block.ParentHash(), Part: first})
	require.Nil(t, e.m.finalParts(request), "the second fragment is not sent")
	e.m.sendFragment(fragmentEvent{Number: 10, Parent: e.block.ParentHash(), Part: second})
	parts := e.m.finalParts(request)
	require.NotNil(t, parts)
	pp := &bft.Preprepare{Parts: parts}
	require.Equal(t, e.hint, pp.AccessList())
	var final bft.Final
	require.NoError(t, rlp.DecodeBytes(pp.Part(bft.PartFinal), &final))
	require.Equal(t, uint64(5), final.TxCount)
	require.Equal(t, []common.Hash{crypto.Keccak256Hash(first.Data), crypto.Keccak256Hash(second.Data)}, final.Fragments)

	// A fragment of another sequence is not sent.
	e.m.sendFragment(fragmentEvent{Number: 11, Parent: e.block.ParentHash(), Part: first})
	require.Len(t, e.m.sentFragments, 2)
}
