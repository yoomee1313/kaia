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
	"math/big"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/prque"
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

// This file holds the multi-node fixtures shared by the consensus scenario
// suites in core_test.go: the event queue that drives real cores one delivery
// at a time, and the backend/scheduler doubles each core runs against.

// maxDrainSteps bounds drain so a message loop fails the test instead of
// hanging it.
const maxDrainSteps = 10000

// ----------------------------------------------------------------------------
// Network construction
// ----------------------------------------------------------------------------

// testEvent is a pending delivery, including self-delivery. Selecting an
// envelope permits delay/reordering without changing another core's state.
type testEvent struct {
	from, to int
	data     interface{}
}

type testNode struct {
	core              *core
	backend           *testBackend
	active, byzantine bool
}

type testNetwork struct {
	t         *testing.T
	nodes     []*testNode
	committee []common.Address
	queue     []testEvent
	now       time.Duration
	timers    []*testTimer
}

// testNetworkOpts tunes newTestNetwork. The zero value gives a network whose
// committee is the whole council, running under permissioned rules.
type testNetworkOpts struct {
	committeeSize  int // 0 means the whole council
	permissionless bool
}

// newTestNetwork wires council real cores against test backends that deliver
// through net.queue. Every core is left at sequence 1 round 0 with its
// round-change timer armed, so a scenario starts by submitting a proposal or by
// advancing virtual time.
func newTestNetwork(t *testing.T, council int, opts *testNetworkOpts) *testNetwork {
	t.Helper()
	if opts == nil {
		opts = &testNetworkOpts{}
	}
	if opts.committeeSize == 0 {
		opts.committeeSize = council
	}
	require.GreaterOrEqual(t, council, opts.committeeSize)
	require.Positive(t, opts.committeeSize)

	keys, addresses := newTestIdentities(t, council)
	installTestHeaderHashFn(t, keys[0])
	genesis := newTestGenesis(t, keys[0], addresses)

	net := &testNetwork{t: t, committee: addresses[:opts.committeeSize]}
	for i, key := range keys {
		net.nodes = append(net.nodes, net.newNode(i, key, genesis, addresses, opts))
	}
	return net
}

// newTestIdentities derives keys from small fixed scalars so that addresses,
// and therefore proposer rotation, are reproducible across runs.
func newTestIdentities(t *testing.T, count int) ([]*ecdsa.PrivateKey, []common.Address) {
	t.Helper()
	keys := make([]*ecdsa.PrivateKey, count)
	addresses := make([]common.Address, count)
	for i := range keys {
		raw := common.LeftPadBytes(big.NewInt(int64(i+1)).Bytes(), 32)
		var err error
		keys[i], err = crypto.ToECDSA(raw)
		require.NoError(t, err)
		addresses[i] = crypto.PubkeyToAddress(keys[i].PublicKey)
	}
	return keys, addresses
}

// installTestHeaderHashFn points the global header hasher at the Istanbul one,
// exactly as real backend initialization does. Without it, writing committed
// seals would change Block.Hash and invalidate the next block's parent. The
// hook is process-global, so these fixtures must not run in parallel.
func installTestHeaderHashFn(t *testing.T, key *ecdsa.PrivateKey) {
	t.Helper()
	previous := types.HeaderHashFn
	types.SetHeaderHashFn(istanbul.NewSealerImpl(key).HeaderHash)
	t.Cleanup(func() { types.SetHeaderHashFn(previous) })
}

func newTestGenesis(t *testing.T, key *ecdsa.PrivateKey, council []common.Address) *types.Block {
	t.Helper()
	header := &types.Header{Number: new(big.Int), Time: big.NewInt(1), BlockScore: big.NewInt(1)}
	require.NoError(t, istanbul.NewSealerImpl(key).WriteValidators(header, council))
	return types.NewBlockWithHeader(header)
}

// newNode builds one real core against its own backend and kaiax mocks. The
// scheduler is swapped in before startNewRound so that the first round-change
// timer lands on virtual time rather than the wall clock.
func (net *testNetwork) newNode(id int, key *ecdsa.PrivateKey, genesis *types.Block, council []common.Address, opts *testNetworkOpts) *testNode {
	net.t.Helper()
	backend := &testBackend{
		network: net, id: id, key: key, sealer: istanbul.NewSealerImpl(key),
		mux: new(event.TypeMux), head: genesis, blocks: map[uint64]*types.Block{0: genesis},
		permissionless: opts.permissionless,
	}
	config := istanbul.DefaultConfig.Copy()
	config.ProposerPolicy = istanbul.RoundRobin

	c := New(backend, config).(*core)
	c.scheduler = &testScheduler{network: net, node: id}
	validators, governance := net.mockModules(council, opts.committeeSize)
	c.RegisterKaiaxModules(validators, governance)
	c.startNewRound(common.Big0)
	require.NotNil(net.t, c.current)
	net.t.Cleanup(func() { c.stopTimer(); backend.mux.Stop() })

	return &testNode{core: c, backend: backend, active: true}
}

// mockModules serves a fixed council and committee with round-robin proposers,
// so a scenario exercises faults rather than validator-set churn.
func (net *testNetwork) mockModules(council []common.Address, committeeSize int) (*valset_mock.MockValsetModule, *mock_gov.MockGovModule) {
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
	return validators, governance
}

// ----------------------------------------------------------------------------
// Proposals and faults
// ----------------------------------------------------------------------------

// proposal builds an empty block on top of the node's head. variant only
// shifts the timestamp so that two proposals at the same height hash
// differently, which is what equivocation scenarios need.
func (net *testNetwork) proposal(node int, variant int64) *types.Block {
	net.t.Helper()
	backend := net.nodes[node].backend
	header := &types.Header{
		ParentHash: backend.head.Hash(), Number: new(big.Int).Add(backend.head.Number(), common.Big1),
		Time: new(big.Int).Add(backend.head.Time(), big.NewInt(variant)), BlockScore: big.NewInt(1),
	}
	require.NoError(net.t, backend.sealer.WriteValidators(header, net.committee))
	seal, err := backend.sealer.MakeAuthorSeal(header)
	require.NoError(net.t, err)
	require.NoError(net.t, backend.sealer.WriteAuthorSeal(header, seal))
	return types.NewBlockWithHeader(header)
}

// submit queues a proposal request for the node's own core. Nothing runs until
// the scenario calls deliverRequest or drain.
func (net *testNetwork) submit(node int, proposal *types.Block) {
	net.queue = append(net.queue, testEvent{node, node, istanbul.RequestEvent{Proposal: proposal}})
}

// inject queues a message the sender's core would never produce. It is the only
// way a scenario can put a message on the wire, and it is restricted to
// Byzantine nodes so honest traffic always comes from a real core handler.
func (net *testNetwork) inject(from, to int, code uint64, proposal *types.Block, round uint64) {
	net.t.Helper()
	require.True(net.t, net.nodes[from].byzantine, "honest messages must originate in core handlers")
	view := &bft.View{Sequence: proposal.Number(), Round: new(big.Int).SetUint64(round)}

	var subject interface{}
	switch code {
	case bft.MsgPreprepare:
		subject = &bft.Preprepare{View: view, Proposal: proposal}
	case bft.MsgRoundChange:
		// A round change carries no digest; it only names the view to move to.
		subject = &bft.Subject{View: view, PrevHash: proposal.ParentHash()}
	default:
		subject = &bft.Subject{View: view, Digest: proposal.Hash(), PrevHash: proposal.ParentHash()}
	}

	encoded, err := bft.Encode(subject)
	require.NoError(net.t, err)
	payload, err := net.nodes[from].core.finalizeMessage(&bft.Message{Hash: proposal.ParentHash(), Code: code, Msg: encoded})
	require.NoError(net.t, err)
	net.queue = append(net.queue, testEvent{from, to, istanbul.MessageEvent{Hash: proposal.ParentHash(), Payload: payload}})
}

// crash takes a node offline: its core stops processing deliveries and its
// timers stop firing. Messages addressed to it are dropped, not queued.
func (net *testNetwork) crash(node int) {
	net.nodes[node].active = false
	net.nodes[node].core.stopTimer()
}

// makeByzantine crashes the node's core and lets inject speak for it instead.
// An attacker is therefore modelled purely as a message source: it never runs a
// consensus transition, so no scenario can accidentally rely on its state.
func (net *testNetwork) makeByzantine(node int) {
	net.crash(node)
	net.nodes[node].byzantine = true
}

// ----------------------------------------------------------------------------
// Driving the queue
// ----------------------------------------------------------------------------

// step executes the queued delivery at index and returns whatever the receiving
// core made of it. A delivery addressed to a crashed node is removed and
// dropped, as the network would drop it.
func (net *testNetwork) step(index int) error {
	net.t.Helper()
	require.Less(net.t, index, len(net.queue), "missing queued event")
	queued := net.queue[index]
	net.queue = append(net.queue[:index], net.queue[index+1:]...)
	if !net.nodes[queued.to].active {
		return nil
	}
	return net.nodes[queued.to].core.handleEvent(queued.data)
}

// stepMatching executes the first queued delivery the predicate accepts, so a
// scenario can reorder deliveries without depending on queue positions.
func (net *testNetwork) stepMatching(match func(testEvent) bool) error {
	net.t.Helper()
	for i, queued := range net.queue {
		if match(queued) {
			return net.step(i)
		}
	}
	net.t.Fatalf("requested delivery not found in %d queued events", len(net.queue))
	return nil
}

// deliver processes one consensus message of the given code from one node to
// another.
func (net *testNetwork) deliver(from, to int, code uint64) error {
	net.t.Helper()
	return net.stepMatching(func(queued testEvent) bool {
		ev, ok := queued.data.(istanbul.MessageEvent)
		return ok && queued.from == from && queued.to == to && net.messageCode(ev) == code
	})
}

// deliverRequest processes the RequestEvent that submit queued for the node,
// wherever it currently sits in the queue.
func (net *testNetwork) deliverRequest(node int) error {
	net.t.Helper()
	return net.stepMatching(func(queued testEvent) bool {
		_, ok := queued.data.(istanbul.RequestEvent)
		return ok && queued.to == node
	})
}

// stepTimeout fires the round-change timeout that advance already enqueued for
// the node, leaving every other node's timeout pending.
func (net *testNetwork) stepTimeout(node int) error {
	net.t.Helper()
	return net.stepMatching(func(queued testEvent) bool {
		_, ok := queued.data.(timeoutEvent)
		return ok && queued.to == node
	})
}

// messageCode reads the code off the wire without checking the signature; the
// receiving core verifies it for real.
func (net *testNetwork) messageCode(ev istanbul.MessageEvent) uint64 {
	net.t.Helper()
	var msg bft.Message
	require.NoError(net.t, msg.FromPayload(ev.Payload, nil))
	return msg.Code
}

// drain runs the queue to exhaustion. Future/stale messages and insufficient
// round-change evidence are normal control-flow outcomes; any other rejection
// must be named by the scenario in allowed, otherwise the test fails.
func (net *testNetwork) drain(allowed ...error) {
	net.t.Helper()
	allowed = append(allowed, errFutureMessage, errOldMessage, errIgnored)
	for steps := 0; len(net.queue) > 0; steps++ {
		require.Less(net.t, steps, maxDrainSteps, "event budget exhausted (possible message loop)")
		queued := net.queue[0]
		err := net.step(0)
		if err == nil || slices.ContainsFunc(allowed, func(want error) bool { return errors.Is(err, want) }) {
			continue
		}
		net.t.Fatalf("delivery %d -> %d (%T): %v", queued.from, queued.to, queued.data, err)
	}
}

// ----------------------------------------------------------------------------
// Virtual time
// ----------------------------------------------------------------------------

// advance moves virtual time forward, firing every timer that comes due in
// order. Callbacks only enqueue events, so no core transition runs until the
// scenario steps the queue.
func (net *testNetwork) advance(delta time.Duration) {
	net.t.Helper()
	require.GreaterOrEqual(net.t, delta, time.Duration(0))
	target := net.now + delta
	for {
		next := net.earliestTimer(target)
		if next == nil {
			break
		}
		net.now = next.due
		next.active = false
		next.fn()
	}
	net.now = target
}

// earliestTimer returns the armed timer that comes due first at or before
// target, or nil when none is left.
func (net *testNetwork) earliestTimer(target time.Duration) *testTimer {
	var next *testTimer
	for _, timer := range net.timers {
		if timer.active && timer.due <= target && (next == nil || timer.due < next.due) {
			next = timer
		}
	}
	return next
}

// roundTimeout is the configured round-change timeout for round 0.
func (net *testNetwork) roundTimeout() time.Duration {
	return time.Duration(atomic.LoadUint64(&istanbul.DefaultConfig.Timeout)) * time.Millisecond
}

// ----------------------------------------------------------------------------
// Inspection and assertions
// ----------------------------------------------------------------------------

// active lists the nodes whose cores are still running.
func (net *testNetwork) active() []int {
	var nodes []int
	for i, node := range net.nodes {
		if node.active {
			nodes = append(nodes, i)
		}
	}
	return nodes
}

// backlog returns the messages the receiver has parked from the sender.
func (net *testNetwork) backlog(receiver, sender int) *prque.Prque {
	return net.nodes[receiver].core.backlogs[net.nodes[sender].backend.Address()]
}

// assertCommitted checks that every node still running committed exactly this
// proposal at this round.
func (net *testNetwork) assertCommitted(proposal *types.Block, round uint64) {
	net.t.Helper()
	running := net.active()
	require.NotEmpty(net.t, running, "no node left running to assert on")
	for _, id := range running {
		net.assertNodeCommitted(id, proposal, round)
	}
}

func (net *testNetwork) assertNodeCommitted(id int, proposal *types.Block, round uint64) {
	net.t.Helper()
	height := proposal.NumberU64()
	backend := net.nodes[id].backend
	require.Len(net.t, backend.committed, int(height), "node %d: exactly one commit per height", id)

	block := backend.committed[height-1]
	require.Equal(net.t, proposal.Hash(), block.Hash(), "node %d", id)
	require.Equal(net.t, proposal.ParentHash(), block.ParentHash(), "node %d", id)
	actualRound, err := backend.sealer.Round(block.Header())
	require.NoError(net.t, err)
	require.Equal(net.t, byte(round), actualRound, "node %d", id)
	net.assertCommittedSeals(id, block)
}

// assertCommittedSeals checks the seals attached at commit time form a quorum
// of distinct committee members.
func (net *testNetwork) assertCommittedSeals(id int, block *types.Block) {
	net.t.Helper()
	backend := net.nodes[id].backend
	var (
		committers []common.Address
		err        error
	)
	if backend.permissionless {
		committers, err = backend.sealer.CommittersWithRound(block.Header())
	} else {
		committers, err = backend.sealer.Committers(block.Header())
	}
	require.NoError(net.t, err)

	unique := valset.NewAddressSet(committers)
	require.Equal(net.t, len(committers), unique.Len(), "node %d: duplicate committed seals", id)
	// QBFT quorum, ceil(2N/3), restated here on purpose: reusing calcQuorumSize
	// would let a bug in it hide behind this assertion.
	require.GreaterOrEqual(net.t, unique.Len(), (2*len(net.committee)+2)/3, "node %d: seals below quorum", id)
	require.Zero(net.t, unique.Subtract(valset.NewAddressSet(net.committee)).Len(), "node %d: sealer outside the committee", id)
}

// ----------------------------------------------------------------------------
// Test doubles
// ----------------------------------------------------------------------------

// testScheduler replaces only when/where events execute, not the transition
// or timeout callback itself. Notifications for workers/VRank have no consumers.
type testScheduler struct {
	network *testNetwork
	node    int
}

func (s *testScheduler) Post(ev interface{}) {
	switch ev.(type) {
	case istanbul.NewSequenceEvent, istanbul.PrepreparedEvent:
		return
	}
	s.network.queue = append(s.network.queue, testEvent{s.node, s.node, ev})
}

func (s *testScheduler) PostAsync(ev interface{}) { s.Post(ev) }

func (s *testScheduler) AfterFunc(delay time.Duration, fn func()) coreTimer {
	timer := &testTimer{due: s.network.now + delay, fn: fn, active: true}
	s.network.timers = append(s.network.timers, timer)
	return timer
}

type testTimer struct {
	due    time.Duration
	fn     func()
	active bool
}

func (timer *testTimer) Stop() bool {
	active := timer.active
	timer.active = false
	return active
}

// testBackend supplies application state per node. It deliberately records
// committed blocks; assertions, rather than the backend, check consensus quorum
// and uniqueness so a core bug cannot be hidden by the test double.
type testBackend struct {
	network        *testNetwork
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

var _ istanbul.Backend = (*testBackend)(nil)

func (b *testBackend) Address() common.Address          { return crypto.PubkeyToAddress(b.key.PublicKey) }
func (b *testBackend) Sealer() *istanbul.IstanbulSealer { return b.sealer }
func (b *testBackend) EventMux() *event.TypeMux         { return b.mux }
func (b *testBackend) NodeType() common.ConnType        { return common.CONSENSUSNODE }
func (b *testBackend) IsPermissionlessAt(uint64) bool   { return b.permissionless }
func (b *testBackend) HasBadProposal(common.Hash) bool  { return false }

func (b *testBackend) Sign(data []byte) ([]byte, error) {
	return crypto.Sign(crypto.Keccak256(data), b.key)
}

func (b *testBackend) SetCurrentView(view *bft.View) {
	b.view = &bft.View{Sequence: new(big.Int).Set(view.Sequence), Round: new(big.Int).Set(view.Round)}
}

func (b *testBackend) Broadcast(hash common.Hash, payload []byte) error {
	return b.fanout(hash, payload, true)
}

func (b *testBackend) Gossip(payload []byte) error {
	return b.fanout(common.Hash{}, payload, false)
}

func (b *testBackend) GossipSubPeer(common.Hash, []byte) {
	// Broadcast already fans out to every node in this full-mesh model.
}

// fanout queues the payload for every node, and for the sender too when self is
// set, mirroring how a real backend loops its own broadcast back.
func (b *testBackend) fanout(hash common.Hash, payload []byte, self bool) error {
	for to := range b.network.nodes {
		if !self && to == b.id {
			continue
		}
		ev := istanbul.MessageEvent{Hash: hash, Payload: append([]byte(nil), payload...)}
		b.network.queue = append(b.network.queue, testEvent{b.id, to, ev})
	}
	return nil
}

func (b *testBackend) LastProposal() (bft.Proposal, common.Address) {
	author, _ := b.sealer.Author(b.head.Header()) // genesis has no proposer seal
	return b.head, author
}

func (b *testBackend) HasPropsal(hash common.Hash, height *big.Int) bool {
	block := b.blocks[height.Uint64()]
	return block != nil && block.Hash() == hash
}

// Verify validates the empty-block fixture's parent, height and author.
// Transaction execution and timestamp/fork rules belong to backend integration
// tests, not here.
func (b *testBackend) Verify(proposal bft.Proposal) (time.Duration, error) {
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

func (b *testBackend) Commit(proposal bft.Proposal, seals [][]byte) error {
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
