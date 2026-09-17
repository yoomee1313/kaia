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
	"fmt"
	"testing"

	"github.com/kaiachain/kaia/consensus/bft"
	"github.com/kaiachain/kaia/params"
	"github.com/stretchr/testify/require"
)

// Scenarios declare exceptional actions and expected consensus outcomes in
// runConsensus. Real cores run until no events remain deliverable, then the
// outcomes are checked (by default, everyone commits in round 0). No handler
// states or message-store sizes are scripted. Every commit is checked for
// quorum, valid seals and agreement, regardless of the expected outcomes.
//
// The fixture uses a fixed committee, rotating proposer and empty blocks.
// Timers fire only when explicitly requested. Gossip caches, transaction
// execution, chain sync and process restarts are outside this model.
// A no-commit result applies to this schedule, not every possible future round.
func TestConsensus(t *testing.T) {
	for _, fork := range []struct {
		name   string
		config *params.ChainConfig
	}{
		{"Legacy", &params.ChainConfig{}},
		{"Permissionless", params.TestKaiaConfig("permissionless")},
	} {
		t.Run(fork.name, func(t *testing.T) {
			runConsensusScenario(t, "ConsecutiveBlocks", newScenarioConfig(withChainConfig(fork.config)), func(s *scenarioNet) {
				for range int(4) {
					s.runConsensus(nil)
				}
			})

			runConsensusScenario(t, "CommitsWithReorderedMessages", newScenarioConfig(withChainConfig(fork.config)), func(s *scenarioNet) {
				// Validator 1 receives votes while its PREPREPARE is delayed.
				s.runConsensus(func() {
					s.delay(bft.MsgPreprepare, s.validators[0], s.recipients(1))
				}, expectUncommitted(s.recipients(1)))

				// Deliver the original payload after the other validators have progressed.
				s.runConsensus(func() {
					s.release(bft.MsgPreprepare, s.validators[0], s.recipients(1))
				})
			})

			runConsensusScenario(t, "CommitsAfterSilentProposer", newScenarioConfig(withChainConfig(fork.config)), func(s *scenarioNet) {
				// No consensus traffic reaches or leaves validator 0.
				s.runConsensus(func() {
					for _, code := range []uint64{bft.MsgPreprepare, bft.MsgPrepare, bft.MsgCommit, bft.MsgRoundChange} {
						s.miss(code, s.validators[0], s.validators)
						for _, node := range s.validators[1:] {
							s.miss(code, node, s.recipients(0))
						}
					}
				}, expectUncommitted(s.validators))

				// Real timeout handlers broadcast ROUNDCHANGE and elect the next proposer.
				s.runConsensus(func() {
					for _, node := range s.validators[1:] {
						node.timeout()
					}
				}, expectCommit(s.recipients(1, 2, 3), 1), expectUncommitted(s.recipients(0)))

				// The participating validators can start another block while 0 stays isolated.
				s.runConsensus(nil, expectCommit(s.recipients(1, 2, 3), 0), expectUncommitted(s.recipients(0)))
			})

			runConsensusScenario(t, "CommitsDespiteNonCommitteeTraffic", newScenarioConfig(
				withChainConfig(fork.config), withValidatorCount(5), withCommitteeSize(4),
			), func(s *scenarioNet) {
				s.runConsensus(func() {
					s.inject(bft.MsgPreprepare, s.validators[4], s.recipients(0, 1, 2, 3), s.validators[4].proposal(1))
					for _, code := range []uint64{bft.MsgPrepare, bft.MsgCommit, bft.MsgRoundChange} {
						// Use the ordinary proposal, but sign as the non-committee node.
						s.inject(code, s.validators[4], s.recipients(0, 1, 2, 3), nil)
					}
				}, expectCommit(s.recipients(0, 1, 2, 3), 0))
			})

			runConsensusScenario(t, "CommitsWithOneConflictingVoter", newScenarioConfig(withChainConfig(fork.config)), func(s *scenarioNet) {
				s.runConsensus(func() {
					for _, code := range []uint64{bft.MsgPrepare, bft.MsgCommit} {
						s.modify(code, s.validators[3], s.recipients(0, 1, 2), s.validators[0].proposal(2))
					}
				}, expectCommit(s.recipients(0, 1, 2), 0))
			})

			runConsensusScenario(t, "DoesNotCommitWithoutQuorum", newScenarioConfig(withChainConfig(fork.config)), func(s *scenarioNet) {
				// An earlier successful block must not satisfy or invalidate the next outcome.
				s.runConsensus(nil)

				// Every other voter sends validator 0 a conflicting proposal digest.
				s.runConsensus(func() {
					for _, node := range s.validators[1:] {
						for _, code := range []uint64{bft.MsgPrepare, bft.MsgCommit} {
							s.modify(code, node, s.recipients(0), s.validators[0].proposal(2))
						}
					}
				}, expectUncommitted(s.recipients(0)))
			})

			for _, count := range []int{5, 7} {
				runConsensusScenario(t, fmt.Sprintf("SplitVotesDoNotCommit/%dValidators", count), newScenarioConfig(
					withChainConfig(fork.config), withValidatorCount(count), withCommitteeSize(count),
				), func(s *scenarioNet) {
					s.runConsensus(func() {
						a, b := s.validators[0].proposal(1), s.validators[0].proposal(2)
						require.NotEqual(s.t, a.Hash(), b.Hash())
						middle := 1 + (count-1)/2

						// Proposer 0 sends proposal A to one group and B to the other.
						for _, code := range []uint64{bft.MsgPreprepare, bft.MsgPrepare} {
							s.modify(code, s.validators[0], s.validators[middle:], b)
						}
						// It also equivocates with COMMITs that its core cannot produce.
						s.inject(bft.MsgCommit, s.validators[0], s.validators[1:middle], a)
						s.inject(bft.MsgCommit, s.validators[0], s.validators[middle:], b)
						// Cross-group traffic is delivered too.
						// N=5: 3 votes per group, quorum 4. N=7: 4 votes, quorum 5.
					}, expectUncommitted(s.validators[1:]))
				})
			}
		})
	}
}
