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
	"github.com/kaiachain/kaia/fork"
	"github.com/kaiachain/kaia/kaiax/gov"
	mock_gov "github.com/kaiachain/kaia/kaiax/gov/mock"
	"github.com/kaiachain/kaia/kaiax/valset"
	valset_mock "github.com/kaiachain/kaia/kaiax/valset/mock"
	"github.com/kaiachain/kaia/params"
	"github.com/stretchr/testify/require"
)

// This file holds the multi-node fixtures shared by the consensus scenarios in
// core_test.go: the event queue that drives real cores one delivery at a time,
// and the backend/scheduler doubles each core runs against.

// maxScenarioSteps bounds phase advancement so a message loop fails the test
// instead of hanging it.
const maxScenarioSteps = 10000

// scenarioConfig is the complete preparation contract for one test case.
// Faults and message ordering are intentionally absent: those belong in the
// scenario's named steps rather than its initial conditions.
type scenarioConfig struct {
	validatorCount int
	committeeSize  int
	chainConfig    *params.ChainConfig
	istanbulConfig *istanbul.Config
}

type scenarioConfigOption func(*scenarioConfig)

func newScenarioConfig(options ...scenarioConfigOption) scenarioConfig {
	istanbulConfig := istanbul.DefaultConfig.Copy()
	istanbulConfig.ProposerPolicy = istanbul.RoundRobin
	config := scenarioConfig{
		validatorCount: 4,
		committeeSize:  4,
		chainConfig:    params.TestKaiaConfig("osaka"),
		istanbulConfig: istanbulConfig,
	}
	for _, option := range options {
		option(&config)
	}
	return config
}

func withValidatorCount(count int) scenarioConfigOption {
	return func(config *scenarioConfig) { config.validatorCount = count }
}

func withCommitteeSize(size int) scenarioConfigOption {
	return func(config *scenarioConfig) { config.committeeSize = size }
}

// runConsensusScenario prepares, runs, and tears down one isolated case.
func runConsensusScenario(t *testing.T, name string, config scenarioConfig, run func(*scenarioNet)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		net := newScenarioNet(t, config)
		run(net)
	})
}

// ----------------------------------------------------------------------------
// Scenario DSL
// ----------------------------------------------------------------------------

// scenarioEvent is one pending delivery, including self-delivery. Keeping
// envelopes in a queue lets a scenario delay or reorder them deterministically.
type scenarioEvent struct {
	from, to int
	data     interface{}
}

// scenarioNet is both the test-facing DSL and the owner of its deterministic
// network state. There is only one lifecycle and one source of truth per case.
type scenarioNet struct {
	t                         *testing.T
	config                    scenarioConfig
	validators                []*validator
	queue                     []scenarioEvent
	expectedCommittedProposal *types.Block
	now                       time.Duration
	mockControllers           []*gomock.Controller
}

// newScenarioNet wires council real cores against test backends that deliver
// through net.queue. Every core is left at sequence 1 round 0 with its
// round-change timer armed, so a scenario starts by submitting a proposal or by
// advancing virtual time.
func newScenarioNet(t *testing.T, config scenarioConfig) *scenarioNet {
	t.Helper()
	require.Positive(t, config.validatorCount)
	require.Positive(t, config.committeeSize)
	require.GreaterOrEqual(t, config.validatorCount, config.committeeSize)
	require.NotNil(t, config.chainConfig)
	require.NotNil(t, config.istanbulConfig)

	previousHeaderHashFn := types.HeaderHashFn
	net := &scenarioNet{t: t, config: config}
	t.Cleanup(func() {
		net.close()
		types.SetHeaderHashFn(previousHeaderHashFn)
	}) // also covers setup failures before this function returns

	fork.ClearHardForkBlockNumberConfig()
	require.NoError(t, fork.SetHardForkBlockNumberConfig(config.chainConfig))

	keys := make([]*ecdsa.PrivateKey, config.validatorCount)
	addresses := make([]common.Address, config.validatorCount)
	for i := range keys {
		raw := common.LeftPadBytes(big.NewInt(int64(i+1)).Bytes(), 32)
		var err error
		keys[i], err = crypto.ToECDSA(raw)
		require.NoError(t, err)
		addresses[i] = crypto.PubkeyToAddress(keys[i].PublicKey)
	}

	types.SetHeaderHashFn(istanbul.NewSealerImpl(keys[0]).HeaderHash)
	genesis := newScenarioGenesis(t, keys[0], addresses)
	for i, key := range keys {
		net.validators = append(net.validators, net.newValidator(i, key, genesis, addresses))
	}
	return net
}

// close releases per-scenario resources, including partially constructed ones.
func (net *scenarioNet) close() {
	for _, node := range net.validators {
		node.core.stopTimer()
		node.backend.mux.Stop()
		node.timers = nil
	}
	for _, ctrl := range net.mockControllers {
		ctrl.Finish()
	}
	fork.ClearHardForkBlockNumberConfig()

	net.queue = nil
}

func (net *scenarioNet) recipients(ids ...int) []*validator {
	net.t.Helper()
	require.NotEmpty(net.t, ids)
	selected := make([]*validator, len(ids))
	for i, id := range ids {
		require.GreaterOrEqual(net.t, id, 0)
		require.Less(net.t, id, len(net.validators))
		selected[i] = net.validators[id]
	}
	return selected
}

func (net *scenarioNet) proposal(node *validator, variant int64) *types.Block {
	net.requireNode(node)
	return net.buildProposal(node.id, variant)
}

func (net *scenarioNet) currentProposal() *types.Block {
	proposal := net.expectedCommittedProposal
	require.NotNil(net.t, proposal, "no proposal in flight")
	return proposal
}

func (net *scenarioNet) backlog(receiver, sender *validator) *prque.Prque {
	net.requireNode(receiver)
	net.requireNode(sender)
	return net.nodeBacklog(receiver.id, sender.id)
}

func (net *scenarioNet) pending() int {
	return len(net.queue)
}

// advance produces one protocol frontier and checks it. Each named message
// phase stops immediately after that message has been broadcast, before any
// recipient handles it. State transitions are named separately.
func (net *scenarioNet) advance(through string, allowed ...error) {
	net.advanceThrough(through, true, allowed...)
}

// advanceUnchecked is reserved for scenarios whose expected result
// intentionally violates the normal phase invariant and is asserted directly
// by the test.
func (net *scenarioNet) advanceUnchecked(through string, allowed ...error) {
	net.advanceThrough(through, false, allowed...)
}

// elapse moves virtual time forward, firing every timer that comes due in
// order. Callbacks only enqueue events, so no core transition runs until the
// scenario steps the queue.
func (net *scenarioNet) elapse(delta time.Duration) {
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

func newScenarioGenesis(t *testing.T, key *ecdsa.PrivateKey, council []common.Address) *types.Block {
	t.Helper()
	header := &types.Header{Number: new(big.Int), Time: big.NewInt(1), BlockScore: big.NewInt(1)}
	require.NoError(t, istanbul.NewSealerImpl(key).WriteValidators(header, council))
	return types.NewBlockWithHeader(header)
}

// newValidator builds one real core against its own backend and kaiax mocks. The
// scheduler is swapped in before startNewRound so that the first round-change
// timer lands on virtual time rather than the wall clock.
func (net *scenarioNet) newValidator(id int, key *ecdsa.PrivateKey, genesis *types.Block, council []common.Address) *validator {
	net.t.Helper()
	backend := &scenarioBackend{
		net: net, id: id, key: key, sealer: istanbul.NewSealerImpl(key),
		mux: new(event.TypeMux), head: genesis, blocks: map[uint64]*types.Block{0: genesis},
		chainConfig: net.config.chainConfig,
	}

	c := New(backend, net.config.istanbulConfig.Copy()).(*core)
	validator := &validator{id: id, core: c, backend: backend, active: true}
	c.scheduler = &scenarioScheduler{net: net, validator: validator}
	validators, governance := net.mockModules(council, net.config.committeeSize)
	c.RegisterKaiaxModules(validators, governance)
	c.startNewRound(common.Big0)
	require.NotNil(net.t, c.current)

	return validator
}

// mockModules serves a fixed council and committee with round-robin proposers,
// so a scenario exercises faults rather than validator-set churn.
func (net *scenarioNet) mockModules(council []common.Address, committeeSize int) (*valset_mock.MockValsetModule, *mock_gov.MockGovModule) {
	ctrl := gomock.NewController(net.t)
	net.mockControllers = append(net.mockControllers, ctrl)
	validators := valset_mock.NewMockValsetModule(ctrl)
	governance := mock_gov.NewMockGovModule(ctrl)
	committee := council[:committeeSize]
	validators.EXPECT().GetCouncil(gomock.Any()).Return(council, nil).AnyTimes()
	validators.EXPECT().GetDemotedValidators(gomock.Any()).Return([]common.Address{}, nil).AnyTimes()
	validators.EXPECT().GetCommittee(gomock.Any(), gomock.Any()).Return(committee, nil).AnyTimes()
	validators.EXPECT().GetProposer(gomock.Any(), gomock.Any()).DoAndReturn(func(height, round uint64) (common.Address, error) {
		return committee[(height-1+round)%uint64(committeeSize)], nil
	}).AnyTimes()
	governance.EXPECT().GetParamSet(gomock.Any()).Return(gov.ParamSet{CommitteeSize: uint64(committeeSize)}).AnyTimes()
	return validators, governance
}

// ----------------------------------------------------------------------------
// Proposals and faults
// ----------------------------------------------------------------------------

func (net *scenarioNet) validatorAddresses() []common.Address {
	addresses := make([]common.Address, len(net.validators))
	for i, validator := range net.validators {
		addresses[i] = validator.backend.Address()
	}
	return addresses
}

// buildProposal builds an empty block on top of the node's head. variant only
// shifts the timestamp so that two proposals at the same height hash
// differently, which is what equivocation scenarios need.
func (net *scenarioNet) buildProposal(node int, variant int64) *types.Block {
	net.t.Helper()
	backend := net.validators[node].backend
	header := &types.Header{
		ParentHash: backend.head.Hash(), Number: new(big.Int).Add(backend.head.Number(), common.Big1),
		Time: new(big.Int).Add(backend.head.Time(), big.NewInt(variant)), BlockScore: big.NewInt(1),
	}
	require.NoError(net.t, backend.sealer.WriteValidators(header, net.validatorAddresses()))
	seal, err := backend.sealer.MakeAuthorSeal(header)
	require.NoError(net.t, err)
	require.NoError(net.t, backend.sealer.WriteAuthorSeal(header, seal))
	return types.NewBlockWithHeader(header)
}

// submit queues a proposal request for the node's own core. Nothing runs until
// the scenario calls deliverRequest or advances a phase.
func (net *scenarioNet) submit(node int, proposal *types.Block) {
	net.expectedCommittedProposal = proposal
	net.queue = append(net.queue, scenarioEvent{node, node, istanbul.RequestEvent{Proposal: proposal}})
}

// ensureProposal creates the ordinary empty-block request for the proposer of
// the current view. Scenarios only construct proposals themselves when they
// need an intentionally conflicting Byzantine subject.
func (net *scenarioNet) ensureProposal() {
	net.t.Helper()
	running := net.active()
	require.NotEmpty(net.t, running, "no node available to propose")
	sequence := net.validators[running[0]].core.current.Sequence().Uint64()
	if net.expectedCommittedProposal != nil && net.expectedCommittedProposal.NumberU64() == sequence {
		return
	}
	id := net.proposer()
	require.True(net.t, net.validators[id].active, "current proposer node[%d] is shutdown", id)
	net.submit(id, net.buildProposal(id, 1))
}

func (net *scenarioNet) proposer() int {
	net.t.Helper()
	running := net.active()
	require.NotEmpty(net.t, running, "no node available to identify proposer")
	address := net.validators[running[0]].core.current.proposer
	for id, node := range net.validators {
		if node.backend.Address() == address {
			return id
		}
	}
	net.t.Fatalf("current proposer %s has no scenario node", address)
	return -1
}

// inject queues a message the sender's core would never produce. It is the only
// way a scenario can synthesize a message, and it is restricted to Byzantine
// nodes so honest traffic always comes from a real core handler.
func (net *scenarioNet) inject(from, to int, code uint64, proposal *types.Block, round uint64) {
	net.t.Helper()
	require.True(net.t, net.validators[from].byzantine, "honest messages must originate in core handlers")
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
	payload, err := net.validators[from].core.finalizeMessage(&bft.Message{Hash: proposal.ParentHash(), Code: code, Msg: encoded})
	require.NoError(net.t, err)
	net.queue = append(net.queue, scenarioEvent{from, to, istanbul.MessageEvent{Hash: proposal.ParentHash(), Payload: payload}})
}

// crash takes a node offline and disarms its timer. Already-queued messages
// remain selectable, but a delivery selected while the node is offline drops.
func (net *scenarioNet) crash(node int) {
	net.validators[node].active = false
	net.validators[node].core.stopTimer()
}

// makeByzantine crashes the node's core and lets inject speak for it instead.
// An attacker is therefore modelled purely as a message source: it never runs a
// consensus transition, so no scenario can accidentally rely on its state.
func (net *scenarioNet) makeByzantine(node int) {
	net.crash(node)
	net.validators[node].byzantine = true
}

// ----------------------------------------------------------------------------
// Driving the queue
// ----------------------------------------------------------------------------

// step executes the queued delivery at index and returns whatever the receiving
// core made of it. A delivery addressed to a crashed node is removed and
// dropped, as the network would drop it.
func (net *scenarioNet) step(index int) error {
	net.t.Helper()
	require.Less(net.t, index, len(net.queue), "missing queued event")
	queued := net.queue[index]
	net.queue = append(net.queue[:index], net.queue[index+1:]...)
	if !net.validators[queued.to].active {
		return nil
	}
	return net.validators[queued.to].core.handleEvent(queued.data)
}

// stepMatching executes the first queued delivery the predicate accepts, so a
// scenario can reorder deliveries without depending on queue positions.
func (net *scenarioNet) stepMatching(match func(scenarioEvent) bool) error {
	net.t.Helper()
	for i, queued := range net.queue {
		if match(queued) {
			return net.step(i)
		}
	}
	net.t.Fatalf("requested delivery not found in %d queued events", len(net.queue))
	return nil
}

// deliverOne selects the first queued envelope of this kind on an exact route.
// The envelope itself carries the proposal and view; send does not reconstruct
// either for an honest node.
func (net *scenarioNet) deliverOne(message string, code uint64, from, to int) error {
	net.t.Helper()
	for i, queued := range net.queue {
		ev, ok := queued.data.(istanbul.MessageEvent)
		if ok && queued.from == from && queued.to == to && net.messageCode(ev) == code {
			return net.step(i)
		}
	}
	net.t.Fatalf(
		"node[%d].send(%q, to node[%d]): matching message not found in %d queued events",
		from, message, to, len(net.queue),
	)
	return nil
}

func (net *scenarioNet) hasMessage(code uint64, from, to int) bool {
	for _, queued := range net.queue {
		ev, ok := queued.data.(istanbul.MessageEvent)
		if ok && queued.from == from && queued.to == to && net.messageCode(ev) == code {
			return true
		}
	}
	return false
}

// deliverRequest processes the RequestEvent that submit queued for the node,
// wherever it currently sits in the queue.
func (net *scenarioNet) deliverRequest(node int) error {
	net.t.Helper()
	return net.stepMatching(func(queued scenarioEvent) bool {
		_, ok := queued.data.(istanbul.RequestEvent)
		return ok && queued.to == node
	})
}

// stepTimeout fires the round-change timeout that advance already enqueued for
// the node, leaving every other node's timeout pending.
func (net *scenarioNet) stepTimeout(node int) error {
	net.t.Helper()
	return net.stepMatching(func(queued scenarioEvent) bool {
		_, ok := queued.data.(timeoutEvent)
		return ok && queued.to == node
	})
}

type scenarioMessageSpec struct {
	name          string
	label         string
	code          uint64
	deliveryPhase string
}

var scenarioMessageSpecs = [...]scenarioMessageSpec{
	{name: "preprepare", label: "PREPREPARE", code: bft.MsgPreprepare, deliveryPhase: "prepare"},
	{name: "prepare", label: "PREPARE", code: bft.MsgPrepare, deliveryPhase: "commit"},
	{name: "commit", label: "COMMIT", code: bft.MsgCommit, deliveryPhase: "new_sequence"},
	{name: "round_change", label: "ROUND CHANGE", code: bft.MsgRoundChange, deliveryPhase: "new_round"},
}

func messageCode(t *testing.T, message string) uint64 {
	t.Helper()
	for _, spec := range scenarioMessageSpecs {
		if spec.name == message {
			return spec.code
		}
	}
	t.Fatalf("unknown consensus message %q", message)
	return 0
}

func (net *scenarioNet) messageCode(ev istanbul.MessageEvent) uint64 {
	net.t.Helper()
	var msg bft.Message
	require.NoError(net.t, msg.FromPayload(ev.Payload, nil))
	return msg.Code
}

func (net *scenarioNet) advanceThrough(through string, check bool, allowed ...error) {
	net.t.Helper()
	validateAdvancePhase(net.t, through)
	if check && isConsensusPhase(through) {
		net.ensureProposal()
	}
	allowed = append(allowed, errFutureMessage, errOldMessage, errIgnored)
	for steps := 0; ; steps++ {
		require.Less(net.t, steps, maxScenarioSteps, "event budget exhausted (possible message loop)")
		index := net.nextEventThrough(through)
		if index < 0 {
			if check {
				net.assertPhase(through)
			}
			return
		}
		queued := net.queue[index]
		err := net.step(index)
		if err == nil || slices.ContainsFunc(allowed, func(want error) bool { return errors.Is(err, want) }) {
			continue
		}
		net.t.Fatalf("delivery %d -> %d (%T): %v", queued.from, queued.to, queued.data, err)
	}
}

func (net *scenarioNet) assertPhase(phase string) {
	net.t.Helper()
	running := net.active()
	require.NotEmpty(net.t, running, "no node left running to check %s phase", phase)

	switch phase {
	case "preprepare":
		require.NotNil(net.t, net.expectedCommittedProposal, "no submitted proposal")
		net.assertBroadcast(bft.MsgPreprepare, []int{net.proposer()})
	case "prepare":
		for _, id := range running {
			c := net.validators[id].core
			require.Equal(net.t, StatePreprepared, c.state, "node %d", id)
			require.NotNil(net.t, c.current.Preprepare, "node %d", id)
			require.Equal(net.t, net.expectedCommittedProposal.Hash(), c.current.Proposal().Hash(), "node %d", id)
			require.Zero(net.t, c.current.Prepares.Size(), "node %d: prepare delivered before boundary", id)
		}
		net.assertBroadcast(bft.MsgPrepare, net.activeCommittee())
	case "commit":
		quorum := (2*net.config.committeeSize + 2) / 3
		for _, id := range running {
			c := net.validators[id].core
			require.Equal(net.t, StatePrepared, c.state, "node %d", id)
			require.GreaterOrEqual(net.t, c.current.Prepares.Size(), quorum, "node %d", id)
			require.Zero(net.t, c.current.Commits.Size(), "node %d: commit delivered before boundary", id)
		}
		net.assertBroadcast(bft.MsgCommit, net.activeCommittee())
	case "new_sequence":
		net.assertNewSequence(running)
	case "round_change":
		view := net.validators[running[0]].core.currentView()
		for _, id := range running {
			c := net.validators[id].core
			require.Equal(net.t, view.Sequence, c.current.Sequence(), "node %d", id)
			require.Equal(net.t, view.Round, c.current.Round(), "node %d", id)
			require.True(net.t, c.waitingForRoundChange, "node %d", id)
		}
		net.assertBroadcast(bft.MsgRoundChange, running)
	case "new_round":
		view := net.validators[running[0]].core.currentView()
		for _, id := range running {
			c := net.validators[id].core
			require.Equal(net.t, view.Sequence, c.current.Sequence(), "node %d", id)
			require.Equal(net.t, view.Round, c.current.Round(), "node %d", id)
			require.False(net.t, c.waitingForRoundChange, "node %d", id)
			require.True(net.t, c.roundChangeTimer.Load().(*scenarioTimer).active, "node %d", id)
		}
	}
}

func (net *scenarioNet) assertBroadcast(code uint64, senders []int) {
	net.t.Helper()
	for _, from := range senders {
		for to := range net.validators {
			require.True(net.t, net.hasMessage(code, from, to), "node[%d] did not broadcast %s to node[%d]", from, messageName(net.t, code), to)
		}
	}
}

func (net *scenarioNet) activeCommittee() []int {
	var nodes []int
	for _, id := range net.active() {
		node := net.validators[id]
		if node.core.current.committee.Contains(node.backend.Address()) {
			nodes = append(nodes, id)
		}
	}
	return nodes
}

func (net *scenarioNet) assertNewSequence(running []int) {
	net.t.Helper()
	require.NotNil(net.t, net.expectedCommittedProposal, "no submitted proposal")
	first := net.validators[running[0]].backend
	height := net.expectedCommittedProposal.NumberU64()
	require.Len(net.t, first.committed, int(height), "node %d", running[0])
	round, err := first.sealer.Round(first.committed[height-1].Header())
	require.NoError(net.t, err)
	net.assertCommitted(net.expectedCommittedProposal, uint64(round))

	for _, id := range running {
		c := net.validators[id].core
		require.Equal(net.t, height+1, c.current.Sequence().Uint64(), "node %d", id)
		require.Equal(net.t, StateAcceptRequest, c.state, "node %d", id)
		require.False(net.t, c.current.IsHashLocked(), "node %d", id)
		require.Zero(net.t, c.current.Prepares.Size(), "node %d", id)
		require.Zero(net.t, c.current.Commits.Size(), "node %d", id)
	}
}

func (net *scenarioNet) nextEventThrough(through string) int {
	for i, queued := range net.queue {
		phase := net.eventPhase(queued.data)
		if isRoundPhase(through) {
			if isRoundPhase(phase) && roundPhaseOrder(phase) <= roundPhaseOrder(through) {
				return i
			}
			continue
		}
		if isConsensusPhase(phase) && consensusPhaseOrder(phase) <= consensusPhaseOrder(through) {
			return i
		}
	}
	return -1
}

func (net *scenarioNet) eventPhase(event interface{}) string {
	switch ev := event.(type) {
	case istanbul.RequestEvent:
		return "preprepare"
	case istanbul.MessageEvent:
		return phaseForCode(net.t, net.messageCode(ev))
	case backlogEvent:
		return phaseForCode(net.t, ev.msg.Code)
	case timeoutEvent:
		return "round_change"
	case istanbul.ChainHeadEvent:
		return "new_sequence"
	default:
		net.t.Fatalf("unknown scenario event %T", event)
		return ""
	}
}

func phaseForCode(t *testing.T, code uint64) string {
	t.Helper()
	for _, spec := range scenarioMessageSpecs {
		if spec.code == code {
			return spec.deliveryPhase
		}
	}
	t.Fatalf("unknown consensus message code %d", code)
	return ""
}

func messageName(t *testing.T, code uint64) string {
	t.Helper()
	for _, spec := range scenarioMessageSpecs {
		if spec.code == code {
			return spec.label
		}
	}
	t.Fatalf("unknown consensus message code %d", code)
	return ""
}

func validateAdvancePhase(t *testing.T, phase string) {
	t.Helper()
	if !isConsensusPhase(phase) && !isRoundPhase(phase) {
		t.Fatalf("unknown advance phase %q", phase)
	}
}

func isConsensusPhase(phase string) bool {
	return consensusPhaseOrder(phase) > 0
}

func consensusPhaseOrder(phase string) int {
	switch phase {
	case "preprepare":
		return 1
	case "prepare":
		return 2
	case "commit":
		return 3
	case "new_sequence":
		return 4
	default:
		return 0
	}
}

func isRoundPhase(phase string) bool {
	return roundPhaseOrder(phase) > 0
}

func roundPhaseOrder(phase string) int {
	switch phase {
	case "round_change":
		return 1
	case "new_round":
		return 2
	default:
		return 0
	}
}

// ----------------------------------------------------------------------------
// Virtual time
// ----------------------------------------------------------------------------

// earliestTimer returns the armed timer that comes due first at or before
// target, or nil when none is left.
func (net *scenarioNet) earliestTimer(target time.Duration) *scenarioTimer {
	var next *scenarioTimer
	for _, validator := range net.validators {
		for _, timer := range validator.timers {
			if timer.active && timer.due <= target && (next == nil || timer.due < next.due) {
				next = timer
			}
		}
	}
	return next
}

// roundTimeout is the configured round-change timeout for round 0.
func (net *scenarioNet) roundTimeout() time.Duration {
	return time.Duration(net.config.istanbulConfig.Timeout) * time.Millisecond
}

// ----------------------------------------------------------------------------
// Inspection and assertions
// ----------------------------------------------------------------------------

// active lists the validators whose cores are still running.
func (net *scenarioNet) active() []int {
	var nodes []int
	for i, node := range net.validators {
		if node.active {
			nodes = append(nodes, i)
		}
	}
	return nodes
}

func (net *scenarioNet) committeeAddresses() []common.Address {
	return net.validatorAddresses()[:net.config.committeeSize]
}

func (net *scenarioNet) requireNode(node *validator) {
	net.t.Helper()
	require.NotNil(net.t, node)
	require.Same(net.t, net, node.backend.net, "node[%d] belongs to another scenario", node.id)
}

// backlog returns the messages the receiver has parked from the sender.
func (net *scenarioNet) nodeBacklog(receiver, sender int) *prque.Prque {
	return net.validators[receiver].core.backlogs[net.validators[sender].backend.Address()]
}

// assertCommitted checks that every node still running committed exactly this
// proposal at this round.
func (net *scenarioNet) assertCommitted(proposal *types.Block, round uint64) {
	net.t.Helper()
	running := net.active()
	require.NotEmpty(net.t, running, "no node left running to assert on")
	for _, id := range running {
		net.assertNodeCommitted(id, proposal, round)
	}
}

func (net *scenarioNet) assertNodeCommitted(id int, proposal *types.Block, round uint64) {
	net.t.Helper()
	height := proposal.NumberU64()
	backend := net.validators[id].backend
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
func (net *scenarioNet) assertCommittedSeals(id int, block *types.Block) {
	net.t.Helper()
	backend := net.validators[id].backend
	var (
		committers []common.Address
		err        error
	)
	if backend.IsPermissionlessAt(block.NumberU64()) {
		committers, err = backend.sealer.CommittersWithRound(block.Header())
	} else {
		committers, err = backend.sealer.Committers(block.Header())
	}
	require.NoError(net.t, err)

	unique := valset.NewAddressSet(committers)
	require.Equal(net.t, len(committers), unique.Len(), "node %d: duplicate committed seals", id)
	// QBFT quorum, ceil(2N/3), restated here on purpose: reusing calcQuorumSize
	// would let a bug in it hide behind this assertion.
	require.GreaterOrEqual(net.t, unique.Len(), (2*net.config.committeeSize+2)/3, "node %d: seals below quorum", id)
	require.Zero(net.t, unique.Subtract(valset.NewAddressSet(net.committeeAddresses())).Len(), "node %d: sealer outside the committee", id)
}

// ----------------------------------------------------------------------------
// Validator DSL
// ----------------------------------------------------------------------------

type validator struct {
	id                int
	core              *core
	backend           *scenarioBackend
	syntheticProposal *types.Block
	timers            []*scenarioTimer
	active, byzantine bool
}

// send delivers this node's named consensus message to each recipient. An
// honest node can only select an envelope previously broadcast by its core, so
// its proposal and view remain implicit. A Byzantine node synthesizes the same
// kind of envelope from the proposal selected with useProposal.
func (node *validator) send(message string, recipients []*validator) sendResult {
	net := node.backend.net
	net.t.Helper()
	require.NotEmpty(net.t, recipients)
	require.True(net.t, node.active || node.byzantine, "shutdown node[%d] cannot send", node.id)
	code := messageCode(net.t, message)
	require.NotNil(net.t, recipients[0])
	if !node.byzantine && code == bft.MsgPreprepare && !net.hasMessage(code, node.id, recipients[0].id) {
		net.ensureProposal()
		require.Equal(net.t, node.id, net.proposer(), "only the current proposer can send PREPREPARE")
		require.NoError(net.t, net.deliverRequest(node.id))
	}

	result := sendResult{t: net.t, from: node.id, message: message}
	for _, recipient := range recipients {
		require.NotNil(net.t, recipient)
		require.Same(net.t, net, recipient.backend.net, "recipient node[%d] belongs to another scenario", recipient.id)
		if node.byzantine {
			require.NotNil(net.t, node.syntheticProposal, "Byzantine node[%d] has no proposal selected", node.id)
			round := node.core.current.Round().Uint64()
			if code == bft.MsgRoundChange {
				round++
			}
			net.inject(node.id, recipient.id, code, node.syntheticProposal, round)
		}
		result.deliveries = append(result.deliveries, deliveryResult{
			to:  recipient.id,
			err: net.deliverOne(message, code, node.id, recipient.id),
		})
	}
	return result
}

// useProposal chooses the subject of messages synthesized by a Byzantine node.
// Honest nodes get their subject from the core-produced envelope instead.
func (node *validator) useProposal(proposal *types.Block) {
	net := node.backend.net
	net.t.Helper()
	require.True(net.t, node.byzantine, "only Byzantine nodes choose a synthetic proposal")
	require.NotNil(net.t, proposal)
	node.syntheticProposal = proposal
}

func (node *validator) shutdown() {
	node.backend.net.crash(node.id)
}

func (node *validator) resume() {
	net := node.backend.net
	net.t.Helper()
	require.False(net.t, node.active, "node[%d] is already running", node.id)
	require.False(net.t, node.byzantine, "Byzantine node[%d] cannot resume as honest", node.id)
	node.active = true
	node.core.newRoundChangeTimer()
}

// timeout processes only this node's already-enqueued round-change timeout.
// Other nodes' timeout events remain pending, allowing asymmetric timeout tests.
func (node *validator) timeout() {
	net := node.backend.net
	net.t.Helper()
	require.NoError(net.t, net.stepTimeout(node.id))
}

func (node *validator) becomeByzantine() {
	node.backend.net.makeByzantine(node.id)
}

// ----------------------------------------------------------------------------
// Send result
// ----------------------------------------------------------------------------

type sendResult struct {
	t          *testing.T
	from       int
	message    string
	deliveries []deliveryResult
}

type deliveryResult struct {
	to int
	// err is the result returned by the recipient core's handleEvent path.
	// Protocol errors such as errFutureMessage are observed here unchanged.
	err error
}

func (result sendResult) isAccepted() {
	result.t.Helper()
	for _, delivery := range result.deliveries {
		require.NoError(result.t, delivery.err, "node[%d].send(%q, node[%d])", result.from, result.message, delivery.to)
	}
}

func (result sendResult) isRejectedWith(want error) {
	result.t.Helper()
	require.Len(result.t, result.deliveries, 1, "isRejectedWith requires exactly one recipient")
	delivery := result.deliveries[0]
	require.ErrorIs(result.t, delivery.err, want, "node[%d].send(%q, node[%d])", result.from, result.message, delivery.to)
}

// ----------------------------------------------------------------------------
// Test doubles
// ----------------------------------------------------------------------------

// scenarioScheduler replaces only when/where events execute, not the transition
// or timeout callback itself. Notifications for workers/VRank have no consumers.
type scenarioScheduler struct {
	net       *scenarioNet
	validator *validator
}

func (s *scenarioScheduler) Post(ev interface{}) {
	switch ev.(type) {
	case istanbul.NewSequenceEvent, istanbul.PrepreparedEvent:
		return
	}
	id := s.validator.id
	s.net.queue = append(s.net.queue, scenarioEvent{id, id, ev})
}

func (s *scenarioScheduler) PostAsync(ev interface{}) { s.Post(ev) }

func (s *scenarioScheduler) AfterFunc(delay time.Duration, fn func()) coreTimer {
	timer := &scenarioTimer{due: s.net.now + delay, fn: fn, active: true}
	s.validator.timers = append(s.validator.timers, timer)
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
	net         *scenarioNet
	id          int
	key         *ecdsa.PrivateKey
	sealer      *istanbul.IstanbulSealer
	mux         *event.TypeMux
	view        *bft.View
	head        *types.Block
	blocks      map[uint64]*types.Block
	committed   []*types.Block
	chainConfig *params.ChainConfig
}

var _ istanbul.Backend = (*scenarioBackend)(nil)

func (b *scenarioBackend) Address() common.Address          { return crypto.PubkeyToAddress(b.key.PublicKey) }
func (b *scenarioBackend) Sealer() *istanbul.IstanbulSealer { return b.sealer }
func (b *scenarioBackend) EventMux() *event.TypeMux         { return b.mux }
func (b *scenarioBackend) NodeType() common.ConnType        { return common.CONSENSUSNODE }
func (b *scenarioBackend) HasBadProposal(common.Hash) bool  { return false }

func (b *scenarioBackend) IsPermissionlessAt(number uint64) bool {
	return b.chainConfig.IsPermissionlessForkEnabled(new(big.Int).SetUint64(number))
}

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

// fanout queues the payload for every node, and for the sender too when self is
// set, mirroring how a real backend loops its own broadcast back.
func (b *scenarioBackend) fanout(hash common.Hash, payload []byte, self bool) error {
	for to := range b.net.validators {
		if !self && to == b.id {
			continue
		}
		ev := istanbul.MessageEvent{Hash: hash, Payload: append([]byte(nil), payload...)}
		b.net.queue = append(b.net.queue, scenarioEvent{b.id, to, ev})
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

func (b *scenarioBackend) HasPropsal(hash common.Hash, height *big.Int) bool {
	block := b.blocks[height.Uint64()]
	return block != nil && block.Hash() == hash
}

// Verify validates the empty-block fixture's parent, height and author.
// Transaction execution and timestamp/fork rules belong to backend integration
// tests, not here.
func (b *scenarioBackend) Verify(proposal bft.Proposal) (time.Duration, error) {
	block, ok := proposal.(*types.Block)
	if !ok || block.NumberU64() != b.head.NumberU64()+1 || block.ParentHash() != b.head.Hash() {
		return 0, istanbul.ErrInvalidProposal
	}
	author, err := b.sealer.Author(block.Header())
	if err != nil {
		return 0, err
	}
	if !valset.NewAddressSet(b.net.validatorAddresses()).Contains(author) {
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
	b.net.validators[b.id].core.scheduler.Post(istanbul.ChainHeadEvent{})
	return nil
}
