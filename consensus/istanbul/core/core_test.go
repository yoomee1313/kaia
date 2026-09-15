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

	"github.com/kaiachain/kaia/consensus/bft"
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
// Fixture conventions: node ids are indices into scenarioNetwork.nodes, and the
// tag passed to net.proposal only varies the block timestamp so that two
// proposals at the same height get different hashes.
const (
	round0 = 0
	round1 = 1

	tagA = 1
	tagB = 2
)

func TestConsensusNormal(t *testing.T) {
	t.Run("SingleBlock", func(t *testing.T) {
		const proposer = 0
		for _, permissionless := range []bool{false, true} {
			t.Run(fmt.Sprintf("Permissionless=%t", permissionless), func(t *testing.T) {
				net := newScenarioNetwork(t, scenarioConfig{nodes: 4, committee: 4, permissionless: permissionless})
				proposal := net.proposal(proposer, tagA)
				net.submit(proposer, proposal)
				require.Empty(t, net.nodes[proposer].backend.committed, "submission only queues an event")
				net.drain()
				net.assertCommitted(proposal, round0, 0, 1, 2, 3)
			})
		}
	})
	t.Run("ConsecutiveBlocks", func(t *testing.T) {
		const validators = 4
		net := newScenarioNetwork(t, scenarioConfig{nodes: validators, committee: validators})
		for height := uint64(1); height <= validators; height++ {
			proposer := int(height-1) % validators // round-robin rotation
			proposal := net.proposal(proposer, tagA)
			net.submit(proposer, proposal)
			net.drain()
			net.assertCommitted(proposal, round0, 0, 1, 2, 3)

			// Every core starts the next height from a clean slate.
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
	// A PREPARE that overtakes the PROPOSAL must be backlogged, then replayed
	// once the PROPOSAL arrives.
	t.Run("PrepareBeforeProposal", func(t *testing.T) {
		const (
			proposer = 0
			lagging  = 1 // receives the PREPARE before the PROPOSAL
		)
		net := newScenarioNetwork(t, scenarioConfig{nodes: 4, committee: 4})
		proposal := net.proposal(proposer, tagA)
		net.submit(proposer, proposal)
		require.NoError(t, net.step(0)) // the queued proposal request
		require.NoError(t, net.deliver(proposer, proposer, bft.MsgPreprepare))

		laggingCore, proposerAddr := net.nodes[lagging].core, net.nodes[proposer].backend.Address()
		require.ErrorIs(t, net.deliver(proposer, lagging, bft.MsgPrepare), errFutureMessage)
		require.Zero(t, laggingCore.current.Prepares.Size())
		require.False(t, laggingCore.backlogs[proposerAddr].Empty())

		require.NoError(t, net.deliver(proposer, lagging, bft.MsgPreprepare))
		net.drain()
		net.assertCommitted(proposal, round0, 0, 1, 2, 3)
		require.True(t, laggingCore.backlogs[proposerAddr].Empty())
	})

	// A proposer that never speaks must cost exactly one round timeout, after
	// which the next proposer takes over.
	t.Run("SilentProposer", func(t *testing.T) {
		const silent = 0
		net := newScenarioNetwork(t, scenarioConfig{nodes: 4, committee: 4})
		net.stop(silent)
		live := net.nodes[1:]

		timeout := requestTimeout()
		net.advance(timeout - time.Nanosecond)
		require.Empty(t, net.queue)
		for _, node := range live {
			require.Zero(t, node.core.current.Round().Uint64())
		}

		net.advance(time.Nanosecond)
		// Timer callbacks enqueue timeout events; no core has processed them yet.
		require.Len(t, net.queue, 3)
		net.drain()
		for _, node := range live {
			require.Equal(t, uint64(1), node.core.current.Round().Uint64())
			require.False(t, node.core.waitingForRoundChange)
			timer := node.core.roundChangeTimer.Load().(*scenarioTimer)
			require.True(t, timer.active)
			require.Equal(t, net.now+timeout+2*time.Second, timer.due)
		}

		// Round 1 belongs to node 1, which commits without node 0.
		const successor = 1
		proposal := net.proposal(successor, tagA)
		net.submit(successor, proposal)
		net.drain()
		net.assertCommitted(proposal, round1, 1, 2, 3)
		require.Empty(t, net.nodes[silent].backend.committed)
	})
}

func TestConsensusByzantineFault(t *testing.T) {
	// A qualified validator outside the committee must not influence proposal
	// admission or vote counts. Valid inputs are generated by the other cores.
	t.Run("NonCommitteeSender", func(t *testing.T) {
		const (
			proposer     = 0
			target       = 1 // the honest core whose state is inspected
			witness      = 2
			nonCommittee = 4 // qualified, but not seated on the committee
		)
		net := newScenarioNetwork(t, scenarioConfig{nodes: 5, committee: 4})
		net.makeByzantine(nonCommittee)
		targetCore := net.nodes[target].core
		proposal := net.proposal(proposer, tagA)

		// PROPOSAL: rejected because the sender is not the round's proposer.
		net.inject(nonCommittee, target, bft.MsgPreprepare, net.proposal(nonCommittee, tagA), round0)
		require.ErrorIs(t, net.deliver(nonCommittee, target, bft.MsgPreprepare), errNotFromProposer)
		require.Nil(t, targetCore.current.Preprepare)

		net.submit(proposer, proposal)
		require.NoError(t, net.step(0))
		for _, to := range []int{target, proposer, witness} {
			require.NoError(t, net.deliver(proposer, to, bft.MsgPreprepare))
		}
		require.Equal(t, proposal.Hash(), targetCore.current.Proposal().Hash())

		// PREPARE: not counted, while the honest one on the same subject is.
		net.inject(nonCommittee, target, bft.MsgPrepare, proposal, round0)
		require.ErrorIs(t, net.deliver(nonCommittee, target, bft.MsgPrepare), errNotFromCommittee)
		require.Zero(t, targetCore.current.Prepares.Size())
		require.NoError(t, net.deliver(proposer, target, bft.MsgPrepare))
		require.Equal(t, 1, targetCore.current.Prepares.Size())

		// COMMIT: deliver a prepare quorum to the proposer first, so that its
		// core produces a real COMMIT to compare against.
		for _, from := range []int{proposer, target, witness} {
			require.NoError(t, net.deliver(from, proposer, bft.MsgPrepare))
		}
		net.inject(nonCommittee, target, bft.MsgCommit, proposal, round0)
		require.ErrorIs(t, net.deliver(nonCommittee, target, bft.MsgCommit), errNotFromCommittee)
		require.Zero(t, targetCore.current.Commits.Size())
		require.NoError(t, net.deliver(proposer, target, bft.MsgCommit))
		require.Equal(t, 1, targetCore.current.Commits.Size())

		// ROUND CHANGE: same rule, with an honest one produced by a timeout.
		net.inject(nonCommittee, target, bft.MsgRoundChange, proposal, round1)
		require.ErrorIs(t, net.deliver(nonCommittee, target, bft.MsgRoundChange), errNotFromCommittee)
		require.Nil(t, targetCore.roundChangeSet.roundChanges[round1])
		net.advance(requestTimeout())
		require.NoError(t, net.stepMatching(func(e scenarioEvent) bool {
			_, ok := e.data.(timeoutEvent)
			return e.to == witness && ok
		}))
		require.ErrorIs(t, net.deliver(witness, target, bft.MsgRoundChange), errIgnored)
		require.Equal(t, 1, targetCore.roundChangeSet.roundChanges[round1].Size())
	})

	// Byzantine voters sign another digest, while
	// honest nodes produce their own PREPAREs and COMMITs through the network.
	t.Run("ConflictingVotes", func(t *testing.T) {
		const (
			validators = 4
			quorum     = 3
			proposer   = 0
		)
		for _, badCount := range []int{1, 3} {
			t.Run(fmt.Sprintf("Byzantine=%d", badCount), func(t *testing.T) {
				net := newScenarioNetwork(t, scenarioConfig{nodes: validators, committee: validators})
				honest := validators - badCount
				for id := honest; id < validators; id++ {
					net.makeByzantine(id)
				}
				proposal := net.proposal(proposer, tagA)
				conflicting := net.proposal(proposer, tagB)
				net.submit(proposer, proposal)
				require.NoError(t, net.step(0))

				for id := 0; id < honest; id++ {
					require.NoError(t, net.deliver(proposer, id, bft.MsgPreprepare))
					for bad := honest; bad < validators; bad++ {
						for _, code := range []uint64{bft.MsgPrepare, bft.MsgCommit} {
							net.inject(bad, id, code, conflicting, round0)
							require.ErrorIs(t, net.deliver(bad, id, code), errInconsistentSubject)
						}
					}
					require.Zero(t, net.nodes[id].core.current.GetPrepareOrCommitSize())
				}

				net.drain()
				if honest >= quorum {
					// The honest cores still reach quorum among themselves.
					net.assertCommitted(proposal, round0, 0, 1, 2)
				} else {
					// A lone honest core cannot advance past PREPREPARE.
					require.Empty(t, net.nodes[proposer].backend.committed)
					require.Equal(t, StatePreprepared, net.nodes[proposer].core.state)
					require.Equal(t, 1, net.nodes[proposer].core.current.Prepares.Size())
				}
			})
		}
	})

	// Only the proposer equivocates; each honest
	// core independently votes for the proposal delivered to its group.
	t.Run("EquivocatingProposer", func(t *testing.T) {
		const proposer = 0
		for _, count := range []int{5, 7} {
			t.Run(fmt.Sprintf("Validators=%d", count), func(t *testing.T) {
				net := newScenarioNetwork(t, scenarioConfig{nodes: count, committee: count})
				net.makeByzantine(proposer)
				proposalA, proposalB := net.proposal(proposer, tagA), net.proposal(proposer, tagB)
				require.NotEqual(t, proposalA.Hash(), proposalB.Hash())

				// Nodes 1..split see proposalA, the remaining honest nodes see proposalB.
				split := (count - 1) / 2
				for id := 1; id < count; id++ {
					proposal := proposalA
					if id > split {
						proposal = proposalB
					}
					net.inject(proposer, id, bft.MsgPreprepare, proposal, round0)
					require.NoError(t, net.deliver(proposer, id, bft.MsgPreprepare))
					net.inject(proposer, id, bft.MsgPrepare, proposal, round0)
				}

				// Cross-group votes are delivered too; honest cores must reject
				// them instead of the harness concealing conflicting messages.
				net.drain(errInconsistentSubject)
				for _, node := range net.nodes[1:] {
					require.Empty(t, node.backend.committed)
					require.Equal(t, StatePreprepared, node.core.state)
					// Its own PREPARE, its group peers', and the proposer's: short of quorum.
					require.Equal(t, split+1, node.core.current.Prepares.Size())
					require.False(t, node.core.current.IsHashLocked())
				}
			})
		}
	})
}
