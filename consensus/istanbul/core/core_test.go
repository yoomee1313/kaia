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
	"time"

	"github.com/stretchr/testify/require"
)

// These scenarios run independent real cores one event at a time. Honest
// messages come from core.broadcast; only explicitly Byzantine nodes may
// forge votes. No core.Start goroutines or wall-clock sleeps are used. The
// scheduler drives the same handleEvent and timer callbacks used by the
// production event loop.
//
// Scope: fixed council/committee, rotating proposer, authenticated full-mesh
// delivery, and empty blocks. This does not model production gossip caches,
// transaction execution, fetcher insertion, or dynamic validator selection.
func TestConsensusNormal(t *testing.T) {
	runConsensusScenario(t, "ConsecutiveBlocks", newScenarioConfig(), func(s *scenarioNet) {
		for range int(4) {
			// Step: commit next block
			s.advance("new_sequence")
		}
	})
}

func TestConsensusNonByzantineFault(t *testing.T) {
	runConsensusScenario(t, "PrepareBeforeProposal", newScenarioConfig(), func(s *scenarioNet) {
		// Step: proposer creates proposal and prepare
		s.validators[0].send("preprepare", s.recipients(0)).isAccepted()

		// Step: prepare reaches lagging validator before proposal
		s.validators[0].send("prepare", s.recipients(1)).isRejectedWith(errFutureMessage)
		require.Zero(s.t, s.validators[1].core.current.Prepares.Size())
		require.False(s.t, s.backlog(s.validators[1], s.validators[0]).Empty())

		// Step: proposal reaches lagging validator later
		s.validators[0].send("preprepare", s.recipients(1)).isAccepted()
		s.advance("new_sequence")

		// Step: network commits and clears backlog
		require.True(s.t, s.backlog(s.validators[1], s.validators[0]).Empty())
	})

	runConsensusScenario(t, "ShutdownAndResume", newScenarioConfig(), func(s *scenarioNet) {
		// Step: one validator pauses before the proposal is broadcast
		s.validators[2].shutdown()
		require.False(s.t, s.validators[2].active)
		require.False(s.t, s.validators[2].core.roundChangeTimer.Load().(*scenarioTimer).active)

		// Step: proposer creates traffic while the validator is offline
		s.validators[0].send("preprepare", s.recipients(0)).isAccepted()
		require.Nil(s.t, s.validators[2].core.current.Preprepare)

		// Step: validator resumes, then receives the delayed proposal
		s.validators[2].resume()
		require.True(s.t, s.validators[2].active)
		require.True(s.t, s.validators[2].core.roundChangeTimer.Load().(*scenarioTimer).active)
		s.validators[0].send("preprepare", s.recipients(2)).isAccepted()
		s.advance("new_sequence")
	})

	runConsensusScenario(t, "SilentProposer", newScenarioConfig(), func(s *scenarioNet) {
		timeout := s.roundTimeout()

		// Step: round zero proposer goes offline
		s.validators[0].shutdown()

		// Step: network waits just before timeout
		s.elapse(timeout - time.Nanosecond)
		require.Zero(s.t, s.pending())
		for _, node := range s.validators[1:] {
			require.Zero(s.t, node.core.current.Round().Uint64())
		}

		// Step: timed-out validators broadcast ROUND CHANGE
		s.elapse(time.Nanosecond)
		require.Equal(s.t, 3, s.pending(), "timer callbacks only enqueue timeout events")
		s.advance("round_change")

		// Step: ROUND CHANGE quorum advances validators to round one
		s.advance("new_round")
		for _, node := range s.validators[1:] {
			timer := node.core.roundChangeTimer.Load().(*scenarioTimer)
			require.Equal(s.t, s.now+timeout+2*time.Second, timer.due)
		}

		// Step: round one proposer commits without failed node
		s.advance("new_sequence")
		require.Empty(s.t, s.validators[0].backend.committed)
	})
}

func TestConsensusByzantineFault(t *testing.T) {
	runConsensusScenario(t, "NonCommitteeSender", newScenarioConfig(
		withValidatorCount(5),
	), func(s *scenarioNet) {
		invalidProposal := s.proposal(s.validators[4], 1)

		// Step: qualified non-committee validator becomes Byzantine
		s.validators[4].becomeByzantine()

		// Step: reject its proposal
		s.validators[4].useProposal(invalidProposal)
		s.validators[4].send("preprepare", s.recipients(1)).isRejectedWith(errNotFromProposer)
		require.Nil(s.t, s.validators[1].core.current.Preprepare)

		// Step: accept proposal from actual proposer
		s.validators[0].send("preprepare", s.recipients(1, 0, 2)).isAccepted()
		require.Equal(s.t, s.currentProposal().Hash(), s.validators[1].core.current.Proposal().Hash())

		// Step: reject its prepare vote
		s.validators[4].useProposal(s.currentProposal())
		s.validators[4].send("prepare", s.recipients(1)).isRejectedWith(errNotFromCommittee)
		require.Zero(s.t, s.validators[1].core.current.Prepares.Size())
		s.validators[0].send("prepare", s.recipients(1)).isAccepted()
		require.Equal(s.t, 1, s.validators[1].core.current.Prepares.Size())

		// Step: reject its commit vote
		for _, sender := range []int{0, 1, 2} {
			s.validators[sender].send("prepare", s.recipients(0)).isAccepted()
		}
		s.validators[4].send("commit", s.recipients(1)).isRejectedWith(errNotFromCommittee)
		require.Zero(s.t, s.validators[1].core.current.Commits.Size())
		s.validators[0].send("commit", s.recipients(1)).isAccepted()
		require.Equal(s.t, 1, s.validators[1].core.current.Commits.Size())

		// Step: reject its round-change vote
		s.validators[4].send("round_change", s.recipients(1)).isRejectedWith(errNotFromCommittee)
		require.Nil(s.t, s.validators[1].core.roundChangeSet.roundChanges[1])
		s.elapse(s.roundTimeout())
		s.validators[2].timeout()
		s.validators[2].send("round_change", s.recipients(1)).isRejectedWith(errIgnored)
		require.Equal(s.t, 1, s.validators[1].core.roundChangeSet.roundChanges[1].Size())
	})

	for _, badCount := range []int{1, 3} {
		badCount := badCount
		runConsensusScenario(t, fmt.Sprintf("ConflictingVotes/%dByzantine", badCount), newScenarioConfig(), func(s *scenarioNet) {
			const validators = 4
			honestCount := validators - badCount
			conflicting := s.proposal(s.validators[0], 2)

			// Step: mark faulty voters Byzantine
			for id := honestCount; id < validators; id++ {
				s.validators[id].becomeByzantine()
				s.validators[id].useProposal(conflicting)
			}

			// Step: deliver valid proposal and conflicting votes
			for honest := 0; honest < honestCount; honest++ {
				s.validators[0].send("preprepare", s.recipients(honest)).isAccepted()
				for bad := honestCount; bad < validators; bad++ {
					for _, message := range []string{"prepare", "commit"} {
						s.validators[bad].send(message, s.recipients(honest)).isRejectedWith(errInconsistentSubject)
					}
				}
				require.Zero(s.t, s.validators[honest].core.current.GetPrepareOrCommitSize())
			}

			// Step: honest quorum determines outcome
			if badCount == 1 {
				s.advance("new_sequence")
				return
			}
			s.advanceUnchecked("commit")
			require.Empty(s.t, s.validators[0].backend.committed)
			require.Equal(s.t, StatePreprepared, s.validators[0].core.state)
			require.Equal(s.t, 1, s.validators[0].core.current.Prepares.Size())
		})
	}

	for _, validatorCount := range []int{5, 7} {
		validatorCount := validatorCount
		runConsensusScenario(t, fmt.Sprintf("EquivocatingProposer/%dValidators", validatorCount), newScenarioConfig(
			withValidatorCount(validatorCount),
			withCommitteeSize(validatorCount),
		), func(s *scenarioNet) {
			a, b := s.proposal(s.validators[0], 1), s.proposal(s.validators[0], 2)

			// Step: proposer becomes Byzantine
			s.validators[0].becomeByzantine()
			require.NotEqual(s.t, a.Hash(), b.Hash())

			// Step: proposer splits honest validators across proposals
			for id := 1; id < validatorCount; id++ {
				proposal := a
				if id > (validatorCount-1)/2 {
					proposal = b
				}
				s.validators[0].useProposal(proposal)
				s.validators[0].send("preprepare", s.recipients(id)).isAccepted()
				s.validators[0].send("prepare", s.recipients(id)).isAccepted()
			}

			// Step: cross-group votes fail to form quorum
			s.advanceUnchecked("commit", errInconsistentSubject)
			for _, node := range s.validators[1:] {
				require.Empty(s.t, node.backend.committed)
				require.Equal(s.t, StatePreprepared, node.core.state)
				require.Equal(s.t, (validatorCount-1)/2+1, node.core.current.Prepares.Size())
				require.False(s.t, node.core.current.IsHashLocked())
			}
		})
	}
}
