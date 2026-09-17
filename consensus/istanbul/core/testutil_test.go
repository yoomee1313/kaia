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
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/bft"
	"github.com/kaiachain/kaia/consensus/istanbul"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/event"
	"github.com/kaiachain/kaia/fork"
	"github.com/kaiachain/kaia/kaiax/gov"
	mock_gov "github.com/kaiachain/kaia/kaiax/gov/mock"
	valset_mock "github.com/kaiachain/kaia/kaiax/valset/mock"
	"github.com/kaiachain/kaia/params"
	"github.com/stretchr/testify/require"
)

// This file holds the multi-node fixtures shared by the consensus scenarios in
// consensus_test.go: the pending deliveries that drive real cores one at a time, and
// the backend/scheduler doubles each core runs against.

// maxScenarioSteps bounds event execution so a message loop fails the test
// instead of hanging it.
const maxScenarioSteps = 10000

// scenarioConfig is the complete preparation contract for one test case.
// Faults and message ordering are intentionally absent: those belong in
// runConsensus callbacks rather than the initial conditions.
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

func withChainConfig(chainConfig *params.ChainConfig) scenarioConfigOption {
	return func(config *scenarioConfig) { config.chainConfig = chainConfig }
}

// runConsensusScenario prepares, runs, and tears down one isolated case.
func runConsensusScenario(t *testing.T, name string, config scenarioConfig, run func(*scenarioNet)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		run(newScenarioNet(t, config))
	})
}

// scenarioNet is both the test-facing DSL and the owner of its deterministic
// network state. There is only one lifecycle and one source of truth per case.
type scenarioNet struct {
	t                         *testing.T
	config                    scenarioConfig
	validators                []*validator
	pendingDeliveries         []scenarioEvent
	rules                     []*messageRule
	injections                []func()
	expectedCommittedProposal *types.Block
}

// newScenarioNet wires council real cores against test backends that record
// pending deliveries. Every core is left at sequence 1 round 0 with its
// round-change timer armed. Scenarios deliver messages or explicitly fire a timeout.
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
	header := &types.Header{Number: new(big.Int), Time: big.NewInt(1), BlockScore: big.NewInt(1)}
	require.NoError(t, istanbul.NewSealerImpl(keys[0]).WriteValidators(header, addresses))
	genesis := types.NewBlockWithHeader(header)
	for i, key := range keys {
		net.validators = append(net.validators, newValidator(net, i, key, genesis, addresses))
	}
	return net
}

// close releases per-scenario resources, including partially constructed ones.
func (net *scenarioNet) close() {
	for _, node := range net.validators {
		node.core.stopTimer()
		node.backend.mux.Stop()
	}
	fork.ClearHardForkBlockNumberConfig()

	net.pendingDeliveries = nil
	net.rules = nil
	net.injections = nil
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

func (net *scenarioNet) currentProposal() *types.Block {
	net.t.Helper()
	proposal := net.expectedCommittedProposal
	require.NotNil(net.t, proposal, "no proposal in flight")
	return proposal
}

func (net *scenarioNet) validatorAddresses() []common.Address {
	addresses := make([]common.Address, len(net.validators))
	for i, validator := range net.validators {
		addresses[i] = validator.backend.Address()
	}
	return addresses
}

func (net *scenarioNet) requireNode(node *validator) {
	net.t.Helper()
	require.NotNil(net.t, node)
	require.Same(net.t, net, node.backend.net, "node[%d] belongs to another scenario", node.id)
}

// Rules affect actual broadcasts for the remainder of this scenario.
// Injected messages bypass these rules, so a test can drop normal traffic and
// independently inject a conflicting envelope.
func (net *scenarioNet) miss(code uint64, from *validator, recipients []*validator) {
	net.t.Helper()
	newMessageRule(net, code, from, recipients).drop = true
}

// delay holds each recipient's deliveries until an explicit release action.
func (net *scenarioNet) delay(code uint64, from *validator, recipients []*validator) {
	net.t.Helper()
	require.NotEmpty(net.t, recipients)
	for _, recipient := range recipients {
		newMessageRule(net, code, from, []*validator{recipient}).hold = true
	}
}

// release queues the original held payloads and disables the selected delays.
// Recipients can be released separately; their per-recipient message order is
// preserved even if the sender has moved on. runConsensus delivers the payloads.
func (net *scenarioNet) release(code uint64, from *validator, recipients []*validator) {
	net.t.Helper()
	net.requireNode(from)
	require.NotEmpty(net.t, recipients)
	var delayed []*messageRule
	for _, recipient := range recipients {
		net.requireNode(recipient)
		index := slices.IndexFunc(net.rules, func(rule *messageRule) bool {
			return rule.hold && rule.code == code && rule.from == from && slices.Contains(rule.recipients, recipient)
		})
		require.NotEqual(net.t, -1, index, "no active delay for code %d from node %d to node %d", code, from.id, recipient.id)
		rule := net.rules[index]
		require.NotContains(net.t, delayed, rule, "duplicate release recipient: node %d", recipient.id)
		require.NotEmpty(net.t, rule.held, "no held messages for code %d from node %d to node %d", code, from.id, recipient.id)
		delayed = append(delayed, rule)
	}
	for _, rule := range delayed {
		net.pendingDeliveries = append(net.pendingDeliveries, rule.held...)
		rule.held = nil
		rule.hold = false
	}
}

// modify replaces the proposal/subject of an actual outgoing message and signs
// it again as its sender. This models a Byzantine sender, not damaged wire bytes.
func (net *scenarioNet) modify(code uint64, from *validator, recipients []*validator, proposal *types.Block) {
	net.t.Helper()
	require.NotNil(net.t, proposal)
	require.True(net.t, code != bft.MsgRoundChange, "ROUND CHANGE has no proposal digest")
	rule := newMessageRule(net, code, from, recipients)
	rule.proposal = proposal
	from.byzantine = true
	net.t.Cleanup(func() {
		require.True(net.t, rule.applied, "modify did not match code %d from node %d", code, from.id)
	})
}

// inject registers one extra signed message per recipient for runConsensus.
// A nil proposal uses the ordinary proposal submitted for that sequence.
func (net *scenarioNet) inject(code uint64, from *validator, recipients []*validator, proposal *types.Block) {
	net.t.Helper()
	net.requireNode(from)
	require.NotEmpty(net.t, recipients)
	for _, recipient := range recipients {
		net.requireNode(recipient)
	}
	from.byzantine = true
	net.injections = append(net.injections, func() {
		if proposal == nil {
			proposal = net.currentProposal()
		}
		round := from.core.current.Round().Uint64()
		if code == bft.MsgRoundChange {
			round++
		}
		message := from.message(code, proposal, round)
		for _, recipient := range recipients {
			net.pendingDeliveries = append(net.pendingDeliveries, scenarioEvent{from.id, recipient.id, message})
		}
	})
}

// runConsensus applies actions once, drives real cores until no events remain
// deliverable, then checks the expected outcomes. The default is that every
// validator commits in round 0 and becomes ready for the next sequence.
// Explicit expectations replace that default: expectUncommitted alone checks
// only the selected validators, without requiring the others to commit.
//
// The target is the lowest uncommitted height among the validators expected to
// commit, or among all selected validators when only no-commit is expected.
// This resumes an incomplete height without letting an intentionally excluded
// validator prevent the participating validators from starting their next block.
// Expectations select what to check, never which nodes receive messages.
//
// At most one height is driven per call. Timers fire only through explicit
// timeout actions; message rules persist across calls until scenario teardown
// (a delay can be released earlier).
func (net *scenarioNet) runConsensus(actions func(), expectations ...consensusExpectation) {
	net.t.Helper()
	if len(expectations) == 0 {
		expectations = []consensusExpectation{expectCommit(net.validators, 0)}
	}
	var height, commitHeight uint64
	selected := make(map[*validator]bool)
	for _, expectation := range expectations {
		require.NotEmpty(net.t, expectation.validators)
		require.LessOrEqual(net.t, expectation.round, uint64(255), "commit round must fit the block header")
		for _, node := range expectation.validators {
			net.requireNode(node)
			require.NotContains(net.t, selected, node, "node %d: duplicate consensus expectation", node.id)
			selected[node] = true
			next := node.backend.head.NumberU64() + 1
			if height == 0 || next < height {
				height = next
			}
			if expectation.commit && (commitHeight == 0 || next < commitHeight) {
				commitHeight = next
			}
		}
	}
	if commitHeight != 0 {
		height = commitHeight
	}
	if actions != nil {
		actions()
	}
	if len(net.pendingDeliveries) == 0 {
		net.requestProposal(height)
	}
	for _, inject := range net.injections {
		inject()
	}
	net.injections = nil

	for steps := 0; ; steps++ {
		require.Less(net.t, steps, maxScenarioSteps, "event budget exhausted (possible message loop)")
		if len(net.pendingDeliveries) == 0 && !net.requestProposal(height) {
			break
		}
		queued := net.pendingDeliveries[0]
		net.pendingDeliveries = net.pendingDeliveries[1:]
		node := net.validators[queued.to]
		before := node.backend.view
		err := node.core.handleEvent(queued.data)
		// Just like the real event loop, rejected/deferred messages do not stop
		// execution. Consensus outcomes, not handler errors, are the contract.
		switch queued.data.(type) {
		case istanbul.MessageEvent, backlogEvent:
		default:
			require.NoError(net.t, err)
		}
		after := node.backend.view
		if after.Sequence.Cmp(before.Sequence) == 0 && after.Round.Cmp(before.Round) != 0 {
			require.Positive(net.t, after.Round.Cmp(before.Round), "node %d: round regressed", node.id)
			proposer := net.validators[(after.Sequence.Uint64()-1+after.Round.Uint64())%uint64(net.config.committeeSize)]
			require.Equal(net.t, after.Sequence, node.core.current.Sequence(), "node %d", node.id)
			require.Equal(net.t, after.Round, node.core.current.Round(), "node %d", node.id)
			require.Equal(net.t, proposer.backend.Address(), node.core.current.proposer, "node %d", node.id)
			require.Equal(net.t, StateAcceptRequest, node.core.state, "node %d", node.id)
			require.False(net.t, node.core.waitingForRoundChange, "node %d", node.id)
		}
	}

	for _, expectation := range expectations {
		for _, node := range expectation.validators {
			id := node.id
			if !expectation.commit {
				require.Less(net.t, node.backend.head.NumberU64(), height,
					"node %d: unexpected commit at height %d", id, height)
				continue
			}

			proposal := net.currentProposal()
			require.Equal(net.t, height, proposal.NumberU64(), "expected proposal must match the target height")
			require.Len(net.t, node.backend.committed, int(height), "node %d: exactly one commit per height", id)
			block := node.backend.committed[height-1]
			require.Equal(net.t, proposal.Hash(), block.Hash(), "node %d", id)
			require.Equal(net.t, proposal.ParentHash(), block.ParentHash(), "node %d", id)
			actualRound, err := node.backend.sealer.Round(block.Header())
			require.NoError(net.t, err)
			require.Equal(net.t, byte(expectation.round), actualRound, "node %d", id)
			author, err := node.backend.sealer.Author(block.Header())
			require.NoError(net.t, err)
			proposer := net.validators[(height-1+expectation.round)%uint64(net.config.committeeSize)]
			require.Equal(net.t, proposer.backend.Address(), author, "node %d: unexpected block author", id)
			require.Equal(net.t, height+1, node.core.current.Sequence().Uint64(), "node %d", id)
			require.Equal(net.t, StateAcceptRequest, node.core.state, "node %d", id)
		}
	}
}

// requestProposal supplies the worker's empty-block request when the elected
// proposer is ready. After a round change this also lets the new proposer work.
// It never creates votes or advances a core state directly.
func (net *scenarioNet) requestProposal(height uint64) bool {
	net.t.Helper()
	for _, node := range net.validators {
		c := node.core
		if c.current.Sequence().Uint64() != height || !c.isProposer() ||
			c.waitingForRoundChange || c.state != StateAcceptRequest || c.current.pendingRequest != nil {
			continue
		}
		proposal := node.proposal(1)
		net.expectedCommittedProposal = proposal
		net.pendingDeliveries = append(net.pendingDeliveries, scenarioEvent{node.id, node.id, istanbul.RequestEvent{Proposal: proposal}})
		return true
	}
	return false
}

// assertCommitSafety observes every backend commit, including those produced
// by injected messages and schedules that are not expected to make progress.
// The backend records the result first; these assertions never turn an unsafe
// commit into a rejected input that the core could recover from.
func (net *scenarioNet) assertCommitSafety(id int) {
	net.t.Helper()
	node := net.validators[id]
	backend := node.backend
	block := backend.committed[len(backend.committed)-1]
	height := block.NumberU64()
	require.Equal(net.t, backend.head.NumberU64()+1, height, "node %d: non-consecutive commit", id)
	require.Equal(net.t, backend.head.Hash(), block.ParentHash(), "node %d: wrong committed parent", id)
	require.Len(net.t, backend.committed, int(height), "node %d: exactly one commit per height", id)
	net.assertCommittedSeals(id, block)

	if node.byzantine {
		return
	}
	for _, other := range net.validators {
		if other.byzantine || other.id == id {
			continue
		}
		if committed := other.backend.blocks[height]; committed != nil {
			require.Equal(net.t, committed.Hash(), block.Hash(),
				"nodes %d and %d committed different blocks at height %d", other.id, id, height)
		}
	}
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

	committee := net.validatorAddresses()[:net.config.committeeSize]
	unique := make(map[common.Address]struct{}, len(committers))
	for _, committer := range committers {
		require.NotContains(net.t, unique, committer, "node %d: duplicate committed seals", id)
		require.Contains(net.t, committee, committer, "node %d: sealer outside the committee", id)
		unique[committer] = struct{}{}
	}
	// QBFT quorum, ceil(2N/3), restated here on purpose: reusing calcQuorumSize
	// would let a bug in it hide behind this assertion.
	require.GreaterOrEqual(net.t, len(unique), (2*net.config.committeeSize+2)/3, "node %d: seals below quorum", id)
}

// consensusExpectation describes the outcome at runConsensus's target height.
// Explicit expectations replace the default; unlisted validators still execute
// normally and every commit remains subject to the common safety checks.
type consensusExpectation struct {
	validators []*validator
	commit     bool
	round      uint64
}

// expectCommit requires the selected validators to commit the expected proposal
// in the given round and become ready for the next sequence.
func expectCommit(validators []*validator, round uint64) consensusExpectation {
	return consensusExpectation{validators: slices.Clone(validators), commit: true, round: round}
}

// expectUncommitted permits earlier blocks, but no commit at the target height.
// It describes this explicit event schedule, not all possible future rounds.
func expectUncommitted(validators []*validator) consensusExpectation {
	return consensusExpectation{validators: slices.Clone(validators)}
}

type validator struct {
	id        int
	core      *core
	backend   *scenarioBackend
	byzantine bool
}

// newValidator builds one real core against its own backend and kaiax mocks. The
// scheduler queues events and holds timer callbacks for explicit delivery.
func newValidator(net *scenarioNet, id int, key *ecdsa.PrivateKey, genesis *types.Block, council []common.Address) *validator {
	net.t.Helper()
	backend := &scenarioBackend{
		net: net, id: id, key: key, sealer: istanbul.NewSealerImpl(key),
		mux: new(event.TypeMux), head: genesis, blocks: map[uint64]*types.Block{0: genesis},
		chainConfig: net.config.chainConfig,
	}

	c := New(backend, net.config.istanbulConfig.Copy()).(*core)
	validator := &validator{id: id, core: c, backend: backend}
	c.scheduler = &scenarioScheduler{net: net, validator: validator}
	// Fixed committee with round-robin proposers; no validator-set churn.
	committeeSize := net.config.committeeSize
	ctrl := gomock.NewController(net.t)
	net.t.Cleanup(ctrl.Finish)
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
	c.RegisterKaiaxModules(validators, governance)
	c.startNewRound(common.Big0)
	require.NotNil(net.t, c.current)

	return validator
}

// proposal builds an empty block on top of this validator's head. variant only
// shifts the timestamp so proposals at the same height can intentionally have
// different hashes in equivocation scenarios.
func (node *validator) proposal(variant int64) *types.Block {
	net := node.backend.net
	net.t.Helper()
	header := &types.Header{
		ParentHash: node.backend.head.Hash(), Number: new(big.Int).Add(node.backend.head.Number(), common.Big1),
		Time: new(big.Int).Add(node.backend.head.Time(), big.NewInt(variant)), BlockScore: big.NewInt(1),
	}
	require.NoError(net.t, node.backend.sealer.WriteValidators(header, net.validatorAddresses()))
	seal, err := node.backend.sealer.MakeAuthorSeal(header)
	require.NoError(net.t, err)
	require.NoError(net.t, node.backend.sealer.WriteAuthorSeal(header, seal))
	return types.NewBlockWithHeader(header)
}

// message signs a synthetic or modified envelope using only this sender's key.
// Normal broadcasts never pass through this helper.
func (node *validator) message(code uint64, proposal *types.Block, round uint64) istanbul.MessageEvent {
	net := node.backend.net
	net.t.Helper()
	require.True(net.t, node.byzantine, "only faulty senders synthesize messages")
	view := &bft.View{Sequence: proposal.Number(), Round: new(big.Int).SetUint64(round)}
	var subject interface{}
	switch code {
	case bft.MsgPreprepare:
		subject = &bft.Preprepare{View: view, Proposal: proposal}
	case bft.MsgPrepare, bft.MsgCommit:
		subject = &bft.Subject{View: view, Digest: proposal.Hash(), PrevHash: proposal.ParentHash()}
	case bft.MsgRoundChange:
		subject = &bft.Subject{View: view, PrevHash: proposal.ParentHash()}
	default:
		net.t.Fatalf("unknown consensus message code %d", code)
	}
	encoded, err := bft.Encode(subject)
	require.NoError(net.t, err)
	payload, err := node.core.finalizeMessage(&bft.Message{Hash: proposal.ParentHash(), Code: code, Msg: encoded})
	require.NoError(net.t, err)
	return istanbul.MessageEvent{Hash: proposal.ParentHash(), Payload: payload}
}

// timeout fires this validator's real timer callback; run delivers its event.
func (node *validator) timeout() {
	net := node.backend.net
	net.t.Helper()
	timer := node.core.roundChangeTimer.Load().(*scenarioTimer)
	require.True(net.t, timer.active, "validator[%d] has no armed round-change timer", node.id)
	timer.active = false
	timer.fn()
}

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
	s.net.pendingDeliveries = append(s.net.pendingDeliveries, scenarioEvent{id, id, ev})
}

func (s *scenarioScheduler) PostAsync(ev interface{}) { s.Post(ev) }

// AfterFunc holds the callback without running wall-clock time. Scenarios only
// trigger round-change expiry explicitly; timer deadlines are not modeled.
func (s *scenarioScheduler) AfterFunc(_ time.Duration, fn func()) coreTimer {
	return &scenarioTimer{fn: fn, active: true}
}

type scenarioTimer struct {
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

// fanout applies rules to actual core-produced broadcasts, per recipient.
// Payloads remain signed wire messages; recipients execute the normal handler.
func (b *scenarioBackend) fanout(hash common.Hash, payload []byte, self bool) error {
	var msg bft.Message
	require.NoError(b.net.t, msg.FromPayload(payload, nil))
	for to, recipient := range b.net.validators {
		if !self && to == b.id {
			continue
		}
		ev := scenarioEvent{b.id, to, istanbul.MessageEvent{Hash: hash, Payload: append([]byte(nil), payload...)}}
		deliver := true
		for _, rule := range b.net.rules {
			if rule.code != msg.Code || rule.from.id != b.id || !slices.Contains(rule.recipients, recipient) {
				continue
			}
			rule.applied = true
			switch {
			case rule.drop:
				deliver = false
			case rule.hold:
				rule.held = append(rule.held, ev)
				deliver = false
			case rule.proposal != nil:
				var view *bft.View
				if msg.Code == bft.MsgPreprepare {
					var pp *bft.Preprepare
					require.NoError(b.net.t, msg.Decode(&pp))
					view = pp.View
				} else {
					var subject *bft.Subject
					require.NoError(b.net.t, msg.Decode(&subject))
					view = subject.View
				}
				require.Equal(b.net.t, view.Sequence, rule.proposal.Number(), "modified proposal must keep the message's sequence")
				ev.data = rule.from.message(msg.Code, rule.proposal, view.Round.Uint64())
			}
		}
		if deliver {
			b.net.pendingDeliveries = append(b.net.pendingDeliveries, ev)
		}
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
	if !slices.Contains(b.net.validatorAddresses(), author) {
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
	b.net.assertCommitSafety(b.id)
	b.head = block
	b.blocks[block.NumberU64()] = block
	b.net.validators[b.id].core.scheduler.Post(istanbul.ChainHeadEvent{})
	return nil
}

// scenarioEvent is one pending delivery, including self-delivery. Keeping
// envelopes pending lets a scenario delay or reorder them deterministically.
type scenarioEvent struct {
	from, to int
	data     interface{}
}

// messageRule matches one sender, message kind and explicit recipients.
// At most one active rule may affect a given route, avoiding ordering ambiguity.
type messageRule struct {
	code       uint64
	from       *validator
	recipients []*validator
	drop       bool
	hold       bool
	proposal   *types.Block
	held       []scenarioEvent
	applied    bool
}

func newMessageRule(net *scenarioNet, code uint64, from *validator, recipients []*validator) *messageRule {
	net.t.Helper()
	net.requireNode(from)
	require.Contains(net.t, []uint64{bft.MsgPreprepare, bft.MsgPrepare, bft.MsgCommit, bft.MsgRoundChange}, code)
	require.NotEmpty(net.t, recipients)
	for _, recipient := range recipients {
		net.requireNode(recipient)
		for _, existing := range net.rules {
			if existing.code == code && existing.from == from &&
				(existing.drop || existing.hold || existing.proposal != nil) {
				require.NotContains(net.t, existing.recipients, recipient, "overlapping message rules")
			}
		}
	}
	rule := &messageRule{code: code, from: from, recipients: append([]*validator(nil), recipients...)}
	net.rules = append(net.rules, rule)
	return rule
}
