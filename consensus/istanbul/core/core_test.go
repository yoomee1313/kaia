// Copyright 2024 The Kaia Authors
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
package core

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/bft"
	"github.com/kaiachain/kaia/consensus/istanbul"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/event"
	"github.com/kaiachain/kaia/kaiax/gov"
	mock_gov "github.com/kaiachain/kaia/kaiax/gov/mock"
	"github.com/kaiachain/kaia/kaiax/valset"
	valset_mock "github.com/kaiachain/kaia/kaiax/valset/mock"
	"github.com/stretchr/testify/require"
)

// These suites run independent real cores one event at a time. Honest messages
// come from core.broadcast; only explicitly Byzantine nodes may inject votes.
// No core.Start goroutines or wall-clock sleeps are used. The scheduler drives
// the same handleEvent and timer callbacks used by the production event loop.
//
// Scope: fixed council/committee, rotating proposer, authenticated full-mesh
// delivery, and empty blocks. This does not model production gossip caches,
// transaction execution, fetcher insertion, or dynamic validator selection.
func TestConsensusNormal(t *testing.T) {
	t.Run("SingleBlock", func(t *testing.T) {
		for _, permissionless := range []bool{false, true} {
			t.Run(fmt.Sprintf("Permissionless=%t", permissionless), func(t *testing.T) {
				net := newScenarioNetwork(t, 4, 4, permissionless)
				proposal := net.proposal(0, 1)
				net.submit(0, proposal)
				require.Empty(t, net.nodes[0].backend.committed, "submission only queues an event")
				net.drain()
				net.assertCommitted(proposal, 0, 0, 1, 2, 3)
			})
		}
	})
	t.Run("ConsecutiveBlocks", func(t *testing.T) {
		net := newScenarioNetwork(t, 4, 4, false)
		for height := uint64(1); height <= 4; height++ {
			proposer := int(height-1) % 4
			proposal := net.proposal(proposer, 1)
			net.submit(proposer, proposal)
			net.drain()
			net.assertCommitted(proposal, 0, 0, 1, 2, 3)
			for _, node := range net.nodes {
				require.Equal(t, height+1, node.core.current.Sequence().Uint64())
				require.Equal(t, StateAcceptRequest, node.core.state)
				require.False(t, node.core.current.IsHashLocked())
				require.Zero(t, node.core.current.Prepares.Size())
				require.Zero(t, node.core.current.Commits.Size())
			}
		}
	})
}

func TestConsensusNonByzantineFault(t *testing.T) {
	t.Run("PrepareBeforeProposal", func(t *testing.T) {
		net := newScenarioNetwork(t, 4, 4, false)
		proposal := net.proposal(0, 1)
		net.submit(0, proposal)
		require.NoError(t, net.step(0)) // proposal request
		require.NoError(t, net.deliver(0, 0, bft.MsgPreprepare))
		require.ErrorIs(t, net.deliver(0, 1, bft.MsgPrepare), errFutureMessage)
		require.Zero(t, net.nodes[1].core.current.Prepares.Size())
		require.False(t, net.nodes[1].core.backlogs[net.nodes[0].backend.Address()].Empty())
		require.NoError(t, net.deliver(0, 1, bft.MsgPreprepare))
		net.drain()
		net.assertCommitted(proposal, 0, 0, 1, 2, 3)
		require.True(t, net.nodes[1].core.backlogs[net.nodes[0].backend.Address()].Empty())
	})
	t.Run("SilentProposer", func(t *testing.T) {
		net := newScenarioNetwork(t, 4, 4, false)
		net.stop(0)
		timeout := time.Duration(atomic.LoadUint64(&istanbul.DefaultConfig.Timeout)) * time.Millisecond
		net.advance(timeout - time.Nanosecond)
		require.Empty(t, net.queue)
		for _, node := range net.nodes[1:] {
			require.Zero(t, node.core.current.Round().Uint64())
		}
		net.advance(time.Nanosecond)
		// Timer callbacks enqueue timeout events; no core has processed them yet.
		require.Len(t, net.queue, 3)
		net.drain()
		for _, node := range net.nodes[1:] {
			require.Equal(t, uint64(1), node.core.current.Round().Uint64())
			require.False(t, node.core.waitingForRoundChange)
			timer := node.core.roundChangeTimer.Load().(*scenarioTimer)
			require.True(t, timer.active)
			require.Equal(t, net.now+timeout+2*time.Second, timer.due)
		}
		proposal := net.proposal(1, 1)
		net.submit(1, proposal)
		net.drain()
		net.assertCommitted(proposal, 1, 1, 2, 3)
		require.Empty(t, net.nodes[0].backend.committed)
	})
}

func TestConsensusByzantineFault(t *testing.T) {
	// A qualified validator outside the committee must not influence proposal
	// admission or vote counts. Valid inputs are generated by the other cores.
	t.Run("NonCommitteeSender", func(t *testing.T) {
		net := newScenarioNetwork(t, 5, 4, false)
		net.makeByzantine(4)
		target := net.nodes[1].core
		proposal := net.proposal(0, 1)
		net.inject(4, 1, bft.MsgPreprepare, net.proposal(4, 1), 0)
		require.ErrorIs(t, net.deliver(4, 1, bft.MsgPreprepare), errNotFromProposer)
		require.Nil(t, target.current.Preprepare)

		net.submit(0, proposal)
		require.NoError(t, net.step(0))
		for _, to := range []int{1, 0, 2} {
			require.NoError(t, net.deliver(0, to, bft.MsgPreprepare))
		}
		require.Equal(t, proposal.Hash(), target.current.Proposal().Hash())
		net.inject(4, 1, bft.MsgPrepare, proposal, 0)
		require.ErrorIs(t, net.deliver(4, 1, bft.MsgPrepare), errNotFromCommittee)
		require.Zero(t, target.current.Prepares.Size())
		require.NoError(t, net.deliver(0, 1, bft.MsgPrepare))
		require.Equal(t, 1, target.current.Prepares.Size())

		// Deliver a prepare quorum to node 0 so its core produces a real COMMIT.
		for _, from := range []int{0, 1, 2} {
			require.NoError(t, net.deliver(from, 0, bft.MsgPrepare))
		}
		net.inject(4, 1, bft.MsgCommit, proposal, 0)
		require.ErrorIs(t, net.deliver(4, 1, bft.MsgCommit), errNotFromCommittee)
		require.Zero(t, target.current.Commits.Size())
		require.NoError(t, net.deliver(0, 1, bft.MsgCommit))
		require.Equal(t, 1, target.current.Commits.Size())

		net.inject(4, 1, bft.MsgRoundChange, proposal, 1)
		require.ErrorIs(t, net.deliver(4, 1, bft.MsgRoundChange), errNotFromCommittee)
		require.Nil(t, target.roundChangeSet.roundChanges[1])
		net.advance(time.Duration(atomic.LoadUint64(&istanbul.DefaultConfig.Timeout)) * time.Millisecond)
		require.NoError(t, net.stepMatching(func(e scenarioEvent) bool {
			_, ok := e.data.(timeoutEvent)
			return e.to == 2 && ok
		}))
		require.ErrorIs(t, net.deliver(2, 1, bft.MsgRoundChange), errIgnored)
		require.Equal(t, 1, target.roundChangeSet.roundChanges[1].Size())
	})

	// Byzantine voters sign another digest, while
	// honest nodes produce their own PREPAREs and COMMITs through the network.
	t.Run("ConflictingVotes", func(t *testing.T) {
		for _, badCount := range []int{1, 3} {
			t.Run(fmt.Sprintf("Byzantine=%d", badCount), func(t *testing.T) {
				net := newScenarioNetwork(t, 4, 4, false)
				honestCount := 4 - badCount
				for i := honestCount; i < 4; i++ {
					net.makeByzantine(i)
				}
				proposal := net.proposal(0, 1)
				other := net.proposal(0, 2)
				net.submit(0, proposal)
				require.NoError(t, net.step(0))
				for i := 0; i < honestCount; i++ {
					require.NoError(t, net.deliver(0, i, bft.MsgPreprepare))
					for j := honestCount; j < 4; j++ {
						for _, code := range []uint64{bft.MsgPrepare, bft.MsgCommit} {
							net.inject(j, i, code, other, 0)
							require.ErrorIs(t, net.deliver(j, i, code), errInconsistentSubject)
						}
					}
					require.Zero(t, net.nodes[i].core.current.GetPrepareOrCommitSize())
				}
				net.drain()
				if badCount == 1 {
					net.assertCommitted(proposal, 0, 0, 1, 2)
				} else {
					require.Empty(t, net.nodes[0].backend.committed)
					require.Equal(t, StatePreprepared, net.nodes[0].core.state)
					require.Equal(t, 1, net.nodes[0].core.current.Prepares.Size())
				}
			})
		}
	})

	// Only the proposer equivocates; each honest
	// core independently votes for the proposal delivered to its group.
	t.Run("EquivocatingProposer", func(t *testing.T) {
		for _, count := range []int{5, 7} {
			t.Run(fmt.Sprintf("Validators=%d", count), func(t *testing.T) {
				net := newScenarioNetwork(t, count, count, false)
				net.makeByzantine(0)
				a, b := net.proposal(0, 1), net.proposal(0, 2)
				require.NotEqual(t, a.Hash(), b.Hash())
				for i := 1; i < count; i++ {
					proposal := a
					if i > (count-1)/2 {
						proposal = b
					}
					net.inject(0, i, bft.MsgPreprepare, proposal, 0)
					require.NoError(t, net.deliver(0, i, bft.MsgPreprepare))
					net.inject(0, i, bft.MsgPrepare, proposal, 0)
				}
				// Cross-group votes are delivered too; honest cores must reject
				// them instead of the harness concealing conflicting messages.
				net.drain(errInconsistentSubject)
				for _, node := range net.nodes[1:] {
					require.Empty(t, node.backend.committed)
					require.Equal(t, StatePreprepared, node.core.state)
					require.Equal(t, (count-1)/2+1, node.core.current.Prepares.Size())
					require.False(t, node.core.current.IsHashLocked())
				}
			})
		}
	})
}

// scenarioEvent is a pending delivery, including self-delivery. Selecting an
// envelope permits delay/reordering without changing another core's state.
type scenarioEvent struct {
	from, to int
	data     interface{}
}

type scenarioNode struct {
	core              *core
	backend           *scenarioBackend
	active, byzantine bool
}

type scenarioNetwork struct {
	t         *testing.T
	nodes     []*scenarioNode
	committee []common.Address
	queue     []scenarioEvent
	now       time.Duration
	timers    []*scenarioTimer
}

func newScenarioNetwork(t *testing.T, count, committeeSize int, permissionless bool) *scenarioNetwork {
	t.Helper()
	require.GreaterOrEqual(t, count, committeeSize)
	require.Positive(t, committeeSize)
	net := &scenarioNetwork{t: t}
	addresses := make([]common.Address, count)
	keys := make([]*ecdsa.PrivateKey, count)
	for i := range keys {
		// Stable test identities make schedules and proposer rotation reproducible.
		raw := common.LeftPadBytes(big.NewInt(int64(i+1)).Bytes(), 32)
		var err error
		keys[i], err = crypto.ToECDSA(raw)
		require.NoError(t, err)
		addresses[i] = crypto.PubkeyToAddress(keys[i].PublicKey)
	}
	// Do not run these fixtures in parallel: HeaderHashFn is process-global.
	// Real backend initialization installs this hash function too. Otherwise
	// committed seals would change Block.Hash and invalidate the next parent.
	previousHashFn := types.HeaderHashFn
	types.SetHeaderHashFn(istanbul.NewSealerImpl(keys[0]).HeaderHash)
	t.Cleanup(func() { types.SetHeaderHashFn(previousHashFn) })
	net.committee = addresses[:committeeSize]
	genesisHeader := &types.Header{Number: new(big.Int), Time: big.NewInt(1), BlockScore: big.NewInt(1)}
	require.NoError(t, istanbul.NewSealerImpl(keys[0]).WriteValidators(genesisHeader, addresses))
	genesis := types.NewBlockWithHeader(genesisHeader)
	for i, key := range keys {
		backend := &scenarioBackend{
			network: net, id: i, key: key, sealer: istanbul.NewSealerImpl(key),
			mux: new(event.TypeMux), head: genesis, blocks: map[uint64]*types.Block{0: genesis},
			permissionless: permissionless,
		}
		config := istanbul.DefaultConfig.Copy()
		config.ProposerPolicy = istanbul.RoundRobin
		c := New(backend, config).(*core)
		node := &scenarioNode{core: c, backend: backend, active: true}
		net.nodes = append(net.nodes, node)
		c.scheduler = &scenarioScheduler{network: net, node: i}
		ctrl := gomock.NewController(t)
		validators := valset_mock.NewMockValsetModule(ctrl)
		governance := mock_gov.NewMockGovModule(ctrl)
		validators.EXPECT().GetCouncil(gomock.Any()).Return(addresses, nil).AnyTimes()
		validators.EXPECT().GetDemotedValidators(gomock.Any()).Return([]common.Address{}, nil).AnyTimes()
		validators.EXPECT().GetCommittee(gomock.Any(), gomock.Any()).Return(net.committee, nil).AnyTimes()
		validators.EXPECT().GetProposer(gomock.Any(), gomock.Any()).DoAndReturn(func(height, round uint64) (common.Address, error) {
			return net.committee[(height-1+round)%uint64(committeeSize)], nil
		}).AnyTimes()
		governance.EXPECT().GetParamSet(gomock.Any()).Return(gov.ParamSet{CommitteeSize: uint64(committeeSize)}).AnyTimes()
		c.RegisterKaiaxModules(validators, governance)
		c.startNewRound(common.Big0)
		require.NotNil(t, c.current)
		t.Cleanup(func() { c.stopTimer(); backend.mux.Stop() })
	}
	return net
}

func (net *scenarioNetwork) submit(node int, proposal *types.Block) {
	net.queue = append(net.queue, scenarioEvent{node, node, istanbul.RequestEvent{Proposal: proposal}})
}

func (net *scenarioNetwork) proposal(node int, tag int64) *types.Block {
	net.t.Helper()
	backend := net.nodes[node].backend
	header := &types.Header{
		ParentHash: backend.head.Hash(), Number: new(big.Int).Add(backend.head.Number(), common.Big1),
		Time: new(big.Int).Add(backend.head.Time(), big.NewInt(tag)), BlockScore: big.NewInt(1),
	}
	require.NoError(net.t, backend.sealer.WriteValidators(header, net.committee))
	seal, err := backend.sealer.MakeAuthorSeal(header)
	require.NoError(net.t, err)
	require.NoError(net.t, backend.sealer.WriteAuthorSeal(header, seal))
	return types.NewBlockWithHeader(header)
}

func (net *scenarioNetwork) stop(node int) {
	net.nodes[node].active = false
	net.nodes[node].core.stopTimer()
}

func (net *scenarioNetwork) makeByzantine(node int) {
	net.stop(node)
	net.nodes[node].byzantine = true
}

func (net *scenarioNetwork) inject(from, to int, code uint64, proposal *types.Block, round uint64) {
	net.t.Helper()
	require.True(net.t, net.nodes[from].byzantine, "honest messages must originate in core handlers")
	view := &bft.View{Sequence: proposal.Number(), Round: new(big.Int).SetUint64(round)}
	var subject interface{} = &bft.Subject{View: view, Digest: proposal.Hash(), PrevHash: proposal.ParentHash()}
	if code == bft.MsgPreprepare {
		subject = &bft.Preprepare{View: view, Proposal: proposal}
	} else if code == bft.MsgRoundChange {
		subject = &bft.Subject{View: view, PrevHash: proposal.ParentHash()}
	}
	encoded, err := bft.Encode(subject)
	require.NoError(net.t, err)
	payload, err := net.nodes[from].core.finalizeMessage(&bft.Message{Hash: proposal.ParentHash(), Code: code, Msg: encoded})
	require.NoError(net.t, err)
	net.queue = append(net.queue, scenarioEvent{from, to, istanbul.MessageEvent{Hash: proposal.ParentHash(), Payload: payload}})
}

func (net *scenarioNetwork) step(index int) error {
	net.t.Helper()
	require.Less(net.t, index, len(net.queue), "missing queued event")
	queued := net.queue[index]
	net.queue = append(net.queue[:index], net.queue[index+1:]...)
	if !net.nodes[queued.to].active {
		return nil
	}
	return net.nodes[queued.to].core.handleEvent(queued.data)
}

func (net *scenarioNetwork) stepMatching(match func(scenarioEvent) bool) error {
	net.t.Helper()
	for i, queued := range net.queue {
		if match(queued) {
			return net.step(i)
		}
	}
	net.t.Fatalf("requested delivery not found in %d queued events", len(net.queue))
	return nil
}

func (net *scenarioNetwork) deliver(from, to int, code uint64) error {
	net.t.Helper()
	return net.stepMatching(func(queued scenarioEvent) bool {
		ev, ok := queued.data.(istanbul.MessageEvent)
		if !ok || queued.from != from || queued.to != to {
			return false
		}
		var msg bft.Message
		require.NoError(net.t, msg.FromPayload(ev.Payload, nil)) // inspection only; receiver verifies the signature
		return msg.Code == code
	})
}

func (net *scenarioNetwork) drain(allowed ...error) {
	net.t.Helper()
	// Future/stale messages and insufficient round-change evidence are normal
	// control-flow outcomes. Other rejections must be explicit in the scenario.
	allowed = append(allowed, errFutureMessage, errOldMessage, errIgnored)
	for steps := 0; len(net.queue) > 0; steps++ {
		require.Less(net.t, steps, 10000, "event budget exhausted (possible message loop)")
		queued := net.queue[0]
		err := net.step(0)
		if err == nil {
			continue
		}
		accepted := false
		for _, expected := range allowed {
			if errors.Is(err, expected) {
				accepted = true
				break
			}
		}
		require.True(net.t, accepted, "delivery %d -> %d (%T): %v", queued.from, queued.to, queued.data, err)
	}
}

func (net *scenarioNetwork) advance(delta time.Duration) {
	net.t.Helper()
	require.GreaterOrEqual(net.t, delta, time.Duration(0))
	target := net.now + delta
	for {
		var next *scenarioTimer
		for _, timer := range net.timers {
			if timer.active && timer.due <= target && (next == nil || timer.due < next.due) {
				next = timer
			}
		}
		if next == nil {
			break
		}
		net.now = next.due
		next.active = false
		next.fn() // timer callbacks enqueue events, never execute a core transition
	}
	net.now = target
}

func (net *scenarioNetwork) assertCommitted(proposal *types.Block, round uint64, nodes ...int) {
	net.t.Helper()
	height := proposal.NumberU64()
	for _, id := range nodes {
		backend := net.nodes[id].backend
		require.Len(net.t, backend.committed, int(height), "node %d: exactly one commit per height", id)
		block := backend.committed[height-1]
		require.Equal(net.t, proposal.Hash(), block.Hash(), "node %d", id)
		require.Equal(net.t, proposal.ParentHash(), block.ParentHash())
		actualRound, err := backend.sealer.Round(block.Header())
		require.NoError(net.t, err)
		require.Equal(net.t, byte(round), actualRound)
		var committers []common.Address
		if backend.permissionless {
			committers, err = backend.sealer.CommittersWithRound(block.Header())
		} else {
			committers, err = backend.sealer.Committers(block.Header())
		}
		require.NoError(net.t, err)
		unique := valset.NewAddressSet(committers)
		require.Equal(net.t, len(committers), unique.Len(), "duplicate committed seals")
		// Fixtures use 4, 5, or 7 committee members; expected quorums are explicit.
		quorum := map[int]int{4: 3, 5: 4, 7: 5}[len(net.committee)]
		require.Positive(net.t, quorum)
		require.GreaterOrEqual(net.t, unique.Len(), quorum)
		require.Zero(net.t, unique.Subtract(valset.NewAddressSet(net.committee)).Len())
	}
}

// scenarioScheduler replaces only when/where events execute, not the transition
// or timeout callback itself. Notifications for workers/VRank have no consumers.
type scenarioScheduler struct {
	network *scenarioNetwork
	node    int
}

func (s *scenarioScheduler) Post(ev interface{}) {
	switch ev.(type) {
	case istanbul.NewSequenceEvent, istanbul.PrepreparedEvent:
		return
	}
	s.network.queue = append(s.network.queue, scenarioEvent{s.node, s.node, ev})
}
func (s *scenarioScheduler) PostAsync(ev interface{}) { s.Post(ev) }
func (s *scenarioScheduler) AfterFunc(delay time.Duration, fn func()) coreTimer {
	timer := &scenarioTimer{due: s.network.now + delay, fn: fn, active: true}
	s.network.timers = append(s.network.timers, timer)
	return timer
}

type scenarioTimer struct {
	due    time.Duration
	fn     func()
	active bool
}

func (timer *scenarioTimer) Stop() bool {
	active := timer.active
	timer.active = false
	return active
}

// scenarioBackend supplies application state per node. It deliberately records
// committed blocks; assertions, rather than the backend, check consensus quorum
// and uniqueness so a core bug cannot be hidden by the test double.
type scenarioBackend struct {
	network        *scenarioNetwork
	id             int
	key            *ecdsa.PrivateKey
	sealer         *istanbul.IstanbulSealer
	mux            *event.TypeMux
	view           *bft.View
	head           *types.Block
	blocks         map[uint64]*types.Block
	committed      []*types.Block
	permissionless bool
}

var _ istanbul.Backend = (*scenarioBackend)(nil)

func (b *scenarioBackend) Address() common.Address          { return crypto.PubkeyToAddress(b.key.PublicKey) }
func (b *scenarioBackend) Sealer() *istanbul.IstanbulSealer { return b.sealer }
func (b *scenarioBackend) EventMux() *event.TypeMux         { return b.mux }
func (b *scenarioBackend) NodeType() common.ConnType        { return common.CONSENSUSNODE }
func (b *scenarioBackend) IsPermissionlessAt(uint64) bool   { return b.permissionless }
func (b *scenarioBackend) Sign(data []byte) ([]byte, error) {
	return crypto.Sign(crypto.Keccak256(data), b.key)
}
func (b *scenarioBackend) SetCurrentView(view *bft.View) {
	b.view = &bft.View{Sequence: new(big.Int).Set(view.Sequence), Round: new(big.Int).Set(view.Round)}
}
func (b *scenarioBackend) Broadcast(hash common.Hash, payload []byte) error {
	return b.fanout(hash, payload, true)
}
func (b *scenarioBackend) Gossip(payload []byte) error {
	return b.fanout(common.Hash{}, payload, false)
}
func (b *scenarioBackend) GossipSubPeer(common.Hash, []byte) {
	// Broadcast already fans out to every node in this full-mesh model.
}
func (b *scenarioBackend) fanout(hash common.Hash, payload []byte, self bool) error {
	for to := range b.network.nodes {
		if !self && to == b.id {
			continue
		}
		ev := istanbul.MessageEvent{Hash: hash, Payload: append([]byte(nil), payload...)}
		b.network.queue = append(b.network.queue, scenarioEvent{b.id, to, ev})
	}
	return nil
}
func (b *scenarioBackend) LastProposal() (bft.Proposal, common.Address) {
	author, _ := b.sealer.Author(b.head.Header()) // genesis has no proposer seal
	return b.head, author
}
func (b *scenarioBackend) HasPropsal(hash common.Hash, height *big.Int) bool {
	block := b.blocks[height.Uint64()]
	return block != nil && block.Hash() == hash
}
func (b *scenarioBackend) HasBadProposal(common.Hash) bool { return false }
func (b *scenarioBackend) Verify(proposal bft.Proposal) (time.Duration, error) {
	// Validate the empty-block fixture's parent, height and author. Transaction
	// execution and timestamp/fork rules belong to backend integration tests.
	block, ok := proposal.(*types.Block)
	if !ok || block.NumberU64() != b.head.NumberU64()+1 || block.ParentHash() != b.head.Hash() {
		return 0, istanbul.ErrInvalidProposal
	}
	author, err := b.sealer.Author(block.Header())
	if err != nil {
		return 0, err
	}
	if !valset.NewAddressSet(b.network.committee).Contains(author) {
		return 0, istanbul.ErrUnauthorizedAddress
	}
	return 0, nil
}
func (b *scenarioBackend) Commit(proposal bft.Proposal, seals [][]byte) error {
	block, ok := proposal.(*types.Block)
	if !ok {
		return istanbul.ErrInvalidProposal
	}
	header := block.Header()
	b.sealer.WriteRound(header, b.view.Round.Int64())
	if err := b.sealer.WriteCommittedSeals(header, seals); err != nil {
		return err
	}
	block = block.WithSeal(header)
	b.committed = append(b.committed, block)
	b.head = block
	b.blocks[block.NumberU64()] = block
	b.network.nodes[b.id].core.scheduler.Post(istanbul.ChainHeadEvent{})
	return nil
}
