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
	"errors"
	"math/big"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/bft"
	"github.com/kaiachain/kaia/consensus/istanbul"
	"github.com/kaiachain/kaia/crypto"
	valset_mock "github.com/kaiachain/kaia/kaiax/valset/mock"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

func amberConfig() *params.ChainConfig {
	return &params.ChainConfig{PermissionlessCompatibleBlock: big.NewInt(0), AmberCompatibleBlock: big.NewInt(0)}
}

// amberBlock returns a block at number whose header commits the writes hash of
// a BAL, and the RLP of the BAL with a read.
func amberBlock(t *testing.T, number int64) (*types.Block, []byte) {
	b := access.NewBuilder()
	b.ReadSlot(common.HexToAddress("0x01"), common.HexToHash("0x11"))
	b.SetBalance(common.HexToAddress("0x02"), 1, big.NewInt(5))
	b.SetBalance(common.HexToAddress("0x03"), 1, big.NewInt(7))
	list := b.Build()
	hash, err := list.Hash()
	require.NoError(t, err)
	enc, err := list.Encode()
	require.NoError(t, err)
	// An istanbul extra without committed seals, as a proposal has.
	extra, err := rlp.EncodeToBytes(&istanbul.IstanbulExtra{})
	require.NoError(t, err)
	header := &types.Header{
		Number: big.NewInt(number), ParentHash: common.Hash{1}, Time: big.NewInt(0), BlockAccessListHash: &hash,
		Extra: append(make([]byte, istanbul.IstanbulExtraVanity), extra...),
	}
	return types.NewBlockWithHeader(header), enc
}

// newAmberMachine returns a machine of this node at (10, round) in the
// Prepared state with block accepted from a PRE-PREPARE carrying accessList.
func newAmberMachine(t *testing.T, cfg *params.ChainConfig, block *types.Block, accessList []byte) (*machine, common.Address) {
	chain := &fakeChain{cfg: cfg, current: newEmptyBlock(0, common.Hash{})}
	b, key := newTestBackend(t, chain)
	self := crypto.PubkeyToAddress(key.PublicKey)
	ctrl := gomock.NewController(t)
	mValset := valset_mock.NewMockValsetModule(ctrl)
	other := common.HexToAddress("0xbeef")
	mValset.EXPECT().GetProposer(gomock.Any(), gomock.Any()).Return(other, nil).AnyTimes()
	b.valsetModule = mValset
	m := newTestMachine(b, 10, 0, block, []common.Address{self})
	m.preprepare.Parts = bft.WithAccessList(nil, accessList)
	m.state = statePrepared
	m.proposer = other
	return m, self
}

func requireCode(t *testing.T, msg *wireMessage, code uint64) {
	t.Helper()
	require.Equal(t, code, msg.Code)
}

func TestCheckProposalAccessList(t *testing.T) {
	block, enc := amberBlock(t, 10)
	view := &bft.View{Sequence: big.NewInt(10), Round: big.NewInt(0)}
	pp := func(b *types.Block, list []byte) *bft.Preprepare {
		return &bft.Preprepare{View: view, Proposal: b, Parts: bft.WithAccessList(nil, list)}
	}
	check := func(cfg *params.ChainConfig, pp *bft.Preprepare) error {
		_, err := checkProposalAccessList(cfg, pp, 0, 0)
		return err
	}

	list, err := checkProposalAccessList(amberConfig(), pp(block, enc), 0, 0)
	require.NoError(t, err)
	got, err := list.Encode()
	require.NoError(t, err)
	require.Equal(t, enc, got)
	list, err = checkProposalAccessList(permissionlessConfig(), pp(newEmptyBlock(10, common.Hash{1}), nil), 0, 0)
	require.NoError(t, err)
	require.Nil(t, list)

	require.NoError(t, check(amberConfig(), pp(block, enc)))
	require.ErrorIs(t, check(amberConfig(), pp(block, nil)), errMissingAccessList)
	require.ErrorIs(t, check(permissionlessConfig(), pp(block, enc)), errUnexpectedAccessList)
	require.NoError(t, check(permissionlessConfig(), pp(newEmptyBlock(10, common.Hash{1}), nil)))
	require.Error(t, check(amberConfig(), pp(block, []byte{0x01})), "not a list")

	// A hint is not checked against the header: it leaves out what the
	// validators predict, and they check their own list.
	other, otherEnc := amberBlock(t, 10)
	header := other.Header()
	header.BlockAccessListHash = &common.Hash{9}
	require.NoError(t, check(amberConfig(), pp(types.NewBlockWithHeader(header), otherEnc)))

	// The consensus limit of a hint counts reads and writes: 3 accounts and
	// one read slot.
	defer func(limit uint64) { accessListHintEntryLimit = limit }(accessListHintEntryLimit)
	accessListHintEntryLimit = 4
	require.NoError(t, check(amberConfig(), pp(block, enc)))
	accessListHintEntryLimit = 3
	require.ErrorIs(t, check(amberConfig(), pp(block, enc)), errAccessListHintEntryLimit)
	accessListHintEntryLimit = uint64(params.DefaultAmberAccessListHintEntryLimit)

	// The size limit of the block and the list together.
	limit := uint64(params.MaxProposalSize - len(enc))
	require.NoError(t, check(amberConfig(), pp(withSize(t, block.Header(), limit), enc)))
	require.ErrorIs(t, check(amberConfig(), pp(withSize(t, block.Header(), limit+1), enc)), blockchain.ErrProposalSizeLimit)
}

// withSize returns a block with header whose RLP is size bytes, padded with
// extra data.
func withSize(t *testing.T, header *types.Header, size uint64) *types.Block {
	header = types.CopyHeader(header)
	for range 5 {
		block := types.NewBlockWithHeader(header)
		diff := int64(size) - int64(block.Size())
		if diff == 0 {
			return block
		}
		n := int64(len(header.Extra)) + diff
		require.GreaterOrEqual(t, n, int64(0))
		header.Extra = make([]byte, n)
	}
	t.Fatalf("no block of %d bytes", size)
	return nil
}

// From Amber the COMMIT waits for the execution, and goes out once the
// execution verified the proposal with the BAL of the PRE-PREPARE.
func TestAmberCommitWaitsForExecution(t *testing.T) {
	block, enc := amberBlock(t, 10)
	m, _ := newAmberMachine(t, amberConfig(), block, enc)
	msgs, stop := captureMessages(m.b)
	defer stop()

	m.trySendCommit()
	expectNoMessage(t, msgs)
	require.True(t, m.commitPending)

	// An outcome of another height is ignored.
	m.handleExecResult(execResultEvent{Number: 11, Hash: block.Hash(), AccessList: enc})
	expectNoMessage(t, msgs)

	m.handleExecResult(execResultEvent{Number: 10, Hash: block.Hash(), AccessList: enc})
	requireCode(t, recvMessage(t, msgs), bft.MsgCommit)
	require.False(t, m.commitPending)

	// A later PREPARE quorum path sends it at once.
	m.trySendCommit()
	requireCode(t, recvMessage(t, msgs), bft.MsgCommit)
}

// Before Amber the COMMIT is sent at once, as before.
func TestCommitBeforeAmberDoesNotWait(t *testing.T) {
	block := newEmptyBlock(10, common.Hash{1})
	m, _ := newAmberMachine(t, permissionlessConfig(), block, nil)
	msgs, stop := captureMessages(m.b)
	defer stop()
	m.trySendCommit()
	requireCode(t, recvMessage(t, msgs), bft.MsgCommit)
}

// A valid block whose PRE-PREPARE carries another hint than the one of this
// node is committed: the hint says nothing about the block.
func TestAmberAccessListHintMismatchCommits(t *testing.T) {
	block, enc := amberBlock(t, 10)
	wrong := append([]byte{}, enc...)
	wrong[len(wrong)-1] ^= 1
	m, _ := newAmberMachine(t, amberConfig(), block, wrong)
	m.lockedHash = block.Hash()
	msgs, stop := captureMessages(m.b)
	defer stop()

	m.trySendCommit()
	m.handleExecResult(execResultEvent{Number: 10, Hash: block.Hash(), AccessList: enc})
	requireCode(t, recvMessage(t, msgs), bft.MsgCommit)
	require.True(t, m.isHashLocked())
	require.False(t, m.needsExecution(block.Hash()))
}

// A proposal found invalid by execution is not committed, releases the lock
// and is rejected when proposed again in the sequence.
func TestAmberInvalidExecutionReleasesLock(t *testing.T) {
	block, enc := amberBlock(t, 10)
	m, _ := newAmberMachine(t, amberConfig(), block, enc)
	m.lockedHash = block.Hash()
	msgs, stop := captureMessages(m.b)
	defer stop()

	m.trySendCommit()
	m.handleExecResult(execResultEvent{Number: 10, Hash: block.Hash(), Err: errors.New("invalid merkle root")})
	requireCode(t, recvMessage(t, msgs), bft.MsgRoundChange)
	expectNoMessage(t, msgs)
	require.False(t, m.isHashLocked())
	require.True(t, m.isInvalidProposal(block.Hash()))
	require.False(t, m.needsExecution(block.Hash()))

	// The next round, whose proposer proposes it again.
	m.state, m.waitingForRoundChange = stateAcceptRequest, false
	m.proposer = m.b.address
	pp := &bft.Preprepare{View: m.currentView(), Proposal: block, Parts: bft.WithAccessList(nil, enc)}
	require.ErrorIs(t, m.handlePreprepare(&wireMessage{Code: bft.MsgPreprepare, Msg: mustEncode(t, pp)}, m.b.address), errRejectedProposal)
}

// A failure of this node, such as a state read error, is neither a vote nor a
// rejection.
func TestAmberLocalFailureDoesNotVote(t *testing.T) {
	block, enc := amberBlock(t, 10)
	m, _ := newAmberMachine(t, amberConfig(), block, enc)
	m.lockedHash = block.Hash()
	msgs, stop := captureMessages(m.b)
	defer stop()

	m.trySendCommit()
	m.handleExecResult(execResultEvent{Number: 10, Hash: block.Hash(), Err: errors.New("missing trie node"), Local: true})
	expectNoMessage(t, msgs)
	require.True(t, m.isHashLocked())
	require.False(t, m.isInvalidProposal(block.Hash()))
	require.True(t, m.commitPending)
}

// A locked block proposed again carries the BAL of this node's execution; if
// the node has not verified it yet, it proposes it once verified.
func TestAmberReproposeLockedWithOwnAccessList(t *testing.T) {
	block, enc := amberBlock(t, 10)
	wrong := append([]byte{}, enc...)
	wrong[len(wrong)-1] ^= 1
	m, self := newAmberMachine(t, amberConfig(), block, wrong)
	m.lockedHash = block.Hash()
	m.proposer = self
	m.state = stateAcceptRequest
	msgs, stop := captureMessages(m.b)
	defer stop()

	m.reproposeLocked()
	expectNoMessage(t, msgs)
	require.NotNil(t, m.reproposal)

	m.handleExecResult(execResultEvent{Number: 10, Hash: block.Hash(), AccessList: enc})
	msg := recvMessage(t, msgs)
	requireCode(t, msg, bft.MsgPreprepare)
	var pp *bft.Preprepare
	require.NoError(t, msg.Decode(&pp))
	require.Equal(t, enc, pp.AccessList(), "its own BAL, not the one it received")
	require.Nil(t, m.reproposal)

	// Verified already: proposed at once.
	m.reproposeLocked()
	msg = recvMessage(t, msgs)
	require.NoError(t, msg.Decode(&pp))
	require.Equal(t, enc, pp.AccessList())
}

// A round change keeps the execution of the locked block only.
func TestCancelSpeculativeExecutionExcept(t *testing.T) {
	b, _ := newTestBackend(t, &fakeChain{cfg: amberConfig()})
	cancelled := false
	b.specHash, b.specCancel = common.Hash{1}, func() { cancelled = true }
	b.cancelSpeculativeExecutionExcept(common.Hash{1})
	require.False(t, cancelled)
	b.cancelSpeculativeExecutionExcept(common.Hash{2})
	require.True(t, cancelled)
}

// verifyExecution returns the BAL of the execution and checks the Amber limits.
func TestVerifyExecution(t *testing.T) {
	statedb, err := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil, nil)
	require.NoError(t, err)
	access.EnableRecorder(statedb)
	statedb.GetState(common.HexToAddress("0x01"), common.HexToHash("0x11"))
	statedb.AddBalance(common.HexToAddress("0x02"), big.NewInt(5))
	statedb.AddBalance(common.HexToAddress("0x03"), big.NewInt(7))
	statedb.IntermediateRoot(true)
	block := newEmptyBlock(10, common.Hash{1})
	res := &blockchain.SpeculativeResult{State: statedb, ProcessStats: blockchain.ProcessStats{ComputationCost: 1000}}

	cfg := amberConfig()
	cfg.AmberBlockComputationCostLimit = 1000
	enc, local, err := verifyExecution(cfg, nil, block, res)
	require.NoError(t, err)
	require.False(t, local)
	want, err := access.Recorded(statedb).Encode()
	require.NoError(t, err)
	require.Equal(t, want, enc)

	cfg.AmberBlockComputationCostLimit = 999
	_, local, err = verifyExecution(cfg, nil, block, res)
	require.ErrorIs(t, err, blockchain.ErrBlockComputationCostLimit)
	require.False(t, local, "the proposal is invalid")

	cfg.AmberBlockComputationCostLimit = 0
	cfg.AmberBlockAccessListEntryLimit = access.Recorded(statedb).WriteEntryCount() - 1
	_, local, err = verifyExecution(cfg, nil, block, res)
	require.ErrorIs(t, err, blockchain.ErrBlockAccessListEntryLimit)
	require.False(t, local)

	cfg.AmberBlockAccessListEntryLimit = 0
	limit := uint64(params.MaxProposalSize - len(want))
	_, _, err = verifyExecution(cfg, nil, withSize(t, block.Header(), limit), res)
	require.NoError(t, err)
	_, local, err = verifyExecution(cfg, nil, withSize(t, block.Header(), limit+1), res)
	require.ErrorIs(t, err, blockchain.ErrProposalSizeLimit)
	require.False(t, local)
}
