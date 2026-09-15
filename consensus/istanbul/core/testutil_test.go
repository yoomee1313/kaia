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
package core

import (
	"crypto/ecdsa"
	"errors"
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

// This file provides the deterministic harness shared by the TestConsensus*
// suites in core_test.go: a queued network of real cores, the scheduler double
// that replaces EventMux and wall time, and a per-node backend double.

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

// scenarioConfig describes a fixture: the council size, how many of those
// validators sit on the committee, and whether the chain is permissionless.
type scenarioConfig struct {
	nodes          int
	committee      int
	permissionless bool
}

func newScenarioNetwork(t *testing.T, cfg scenarioConfig) *scenarioNetwork {
	t.Helper()
	require.GreaterOrEqual(t, cfg.nodes, cfg.committee)
	require.Positive(t, cfg.committee)
	net := &scenarioNetwork{t: t}
	keys, council := newScenarioIdentities(t, cfg.nodes)
	net.committee = council[:cfg.committee]
	installScenarioHeaderHashFn(t, keys[0])
	genesis := newScenarioGenesis(t, keys[0], council)
	for id, key := range keys {
		net.addNode(cfg, id, key, council, genesis)
	}
	return net
}

// newScenarioIdentities derives stable test identities so schedules and
// proposer rotation stay reproducible across runs.
func newScenarioIdentities(t *testing.T, count int) ([]*ecdsa.PrivateKey, []common.Address) {
	t.Helper()
	keys := make([]*ecdsa.PrivateKey, count)
	addresses := make([]common.Address, count)
	for i := range keys {
		raw := common.LeftPadBytes(big.NewInt(int64(i+1)).Bytes(), 32)
		key, err := crypto.ToECDSA(raw)
		require.NoError(t, err)
		keys[i], addresses[i] = key, crypto.PubkeyToAddress(key.PublicKey)
	}
	return keys, addresses
}

// installScenarioHeaderHashFn mirrors what real backend initialization does.
// Do not run these fixtures in parallel: HeaderHashFn is process-global.
// Without it committed seals would change Block.Hash and invalidate the next
// parent.
func installScenarioHeaderHashFn(t *testing.T, key *ecdsa.PrivateKey) {
	t.Helper()
	previous := types.HeaderHashFn
	types.SetHeaderHashFn(istanbul.NewSealerImpl(key).HeaderHash)
	t.Cleanup(func() { types.SetHeaderHashFn(previous) })
}

func newScenarioGenesis(t *testing.T, key *ecdsa.PrivateKey, council []common.Address) *types.Block {
	t.Helper()
	header := &types.Header{Number: new(big.Int), Time: big.NewInt(1), BlockScore: big.NewInt(1)}
	require.NoError(t, istanbul.NewSealerImpl(key).WriteValidators(header, council))
	return types.NewBlockWithHeader(header)
}

// addNode attaches one real core, driven by the scenario scheduler instead of
// the production event loop, to its own backend state.
func (net *scenarioNetwork) addNode(cfg scenarioConfig, id int, key *ecdsa.PrivateKey, council []common.Address, genesis *types.Block) {
	net.t.Helper()
	backend := &scenarioBackend{
		network: net, id: id, key: key, sealer: istanbul.NewSealerImpl(key),
		mux: new(event.TypeMux), head: genesis, blocks: map[uint64]*types.Block{0: genesis},
		permissionless: cfg.permissionless,
	}
	config := istanbul.DefaultConfig.Copy()
	config.ProposerPolicy = istanbul.RoundRobin
	c := New(backend, config).(*core)
	c.scheduler = &scenarioScheduler{network: net, node: id}
	net.nodes = append(net.nodes, &scenarioNode{core: c, backend: backend, active: true})
	net.registerKaiaxMocks(c, council, cfg.committee)
	c.startNewRound(common.Big0)
	require.NotNil(net.t, c.current)
	net.t.Cleanup(func() { c.stopTimer(); backend.mux.Stop() })
}

// registerKaiaxMocks pins a fixed council and committee with round-robin
// proposer rotation, so scenarios control faults rather than validator churn.
func (net *scenarioNetwork) registerKaiaxMocks(c *core, council []common.Address, committeeSize int) {
	ctrl := gomock.NewController(net.t)
	validators := valset_mock.NewMockValsetModule(ctrl)
	governance := mock_gov.NewMockGovModule(ctrl)
	validators.EXPECT().GetCouncil(gomock.Any()).Return(council, nil).AnyTimes()
	validators.EXPECT().GetDemotedValidators(gomock.Any()).Return([]common.Address{}, nil).AnyTimes()
	validators.EXPECT().GetCommittee(gomock.Any(), gomock.Any()).Return(net.committee, nil).AnyTimes()
	validators.EXPECT().GetProposer(gomock.Any(), gomock.Any()).DoAndReturn(func(height, round uint64) (common.Address, error) {
		return net.committee[(height-1+round)%uint64(committeeSize)], nil
	}).AnyTimes()
	governance.EXPECT().GetParamSet(gomock.Any()).Return(gov.ParamSet{CommitteeSize: uint64(committeeSize)}).AnyTimes()
	c.RegisterKaiaxModules(validators, governance)
}

// requestTimeout is the round timeout the production core is configured with.
func requestTimeout() time.Duration {
	return time.Duration(atomic.LoadUint64(&istanbul.DefaultConfig.Timeout)) * time.Millisecond
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
	var subject interface{}
	switch code {
	case bft.MsgPreprepare:
		subject = &bft.Preprepare{View: view, Proposal: proposal}
	case bft.MsgRoundChange:
		subject = &bft.Subject{View: view, PrevHash: proposal.ParentHash()}
	default:
		subject = &bft.Subject{View: view, Digest: proposal.Hash(), PrevHash: proposal.ParentHash()}
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

func (b *scenarioBackend) ProposalRound(hash common.Hash, number *big.Int) (byte, bool) {
	block := b.blocks[number.Uint64()]
	if block == nil || block.Hash() != hash {
		return 0, false
	}
	round, err := b.sealer.Round(block.Header())
	if err != nil {
		return 0, false
	}
	return round, true
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
