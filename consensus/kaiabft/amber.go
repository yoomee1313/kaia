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
	"bytes"
	"errors"
	"fmt"
	"math/big"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/bft"
	"github.com/kaiachain/kaia/params"
	"github.com/rcrowley/go-metrics"
)

// From the Amber hardfork a PRE-PREPARE carries the block access list (BAL)
// of the proposal, reads included, and a validator sends its COMMIT only after
// it executed the proposal and found it valid: the state, the receipts and the
// BAL hash match the header, the block is within the Amber limits, and the BAL
// of its own execution is the one of the PRE-PREPARE. PREPARE is sent as before
// and execution starts on the PRE-PREPARE, so execution overlaps the PREPARE
// round.

var (
	errMissingAccessList      = errors.New("proposal without a block access list")
	errUnexpectedAccessList   = errors.New("block access list before the Amber hardfork")
	errAccessListHashMismatch = errors.New("block access list hash mismatch")
	errRejectedProposal       = errors.New("proposal found invalid by execution")

	// invalidProposalCounter counts the proposals this node refused for their
	// sequence after executing them, which consensus no longer records as
	// bad blocks.
	invalidProposalCounter = metrics.NewRegisteredCounter("consensus/kaiabft/proposal/invalid", nil)
)

// execResultEvent is the outcome of the speculative execution of an Amber
// proposal, posted to the machine.
type execResultEvent struct {
	Number     uint64
	Hash       common.Hash
	AccessList []byte // RLP of the BAL of this node's execution, if valid
	Err        error  // why the proposal is invalid, or why execution failed
	Local      bool   // Err is a failure of this node, not of the proposal
}

func (b *backend) isAmberAt(num uint64) bool {
	return b.chain.Config().IsAmberForkEnabled(new(big.Int).SetUint64(num))
}

// checkProposalAccessList checks the BAL of a PRE-PREPARE without executing
// the proposal: present from Amber and absent before, well formed, with the
// writes hash of the header, within the entry limit, and within the size
// limit together with the block. It returns the list, nil before Amber.
func checkProposalAccessList(config *params.ChainConfig, pp *bft.Preprepare) (*access.BlockAccessList, error) {
	number := pp.Proposal.Number()
	if !config.IsAmberForkEnabled(number) {
		if len(pp.AccessList) != 0 {
			return nil, errUnexpectedAccessList
		}
		return nil, nil
	}
	if len(pp.AccessList) == 0 {
		return nil, errMissingAccessList
	}
	list, err := access.Decode(pp.AccessList)
	if err != nil {
		return nil, fmt.Errorf("block access list: %w", err)
	}
	want := pp.Proposal.Header().BlockAccessListHash
	if want == nil {
		return nil, errors.New("missing block access list hash")
	}
	if got, err := list.Hash(); err != nil {
		return nil, fmt.Errorf("block access list: %w", err)
	} else if got != *want {
		return nil, fmt.Errorf("%w: header %x, list %x", errAccessListHashMismatch, *want, got)
	}
	if err := blockchain.CheckBlockAccessListEntries(config, number, list.EntryCount()); err != nil {
		return nil, err
	}
	block, ok := pp.Proposal.(*types.Block)
	if !ok {
		return nil, errors.New("kaiabft: invalid proposal type")
	}
	if err := blockchain.CheckProposalSize(config, number, uint64(block.Size()), uint64(len(pp.AccessList))); err != nil {
		return nil, err
	}
	return list, nil
}

// verifyExecution checks the speculative result of an Amber proposal and
// returns the RLP of the BAL of the execution. local reports that the check
// failed for a reason of this node, a state access failure, which says nothing
// about the proposal.
func verifyExecution(config *params.ChainConfig, validator blockchain.Validator, block *types.Block, res *blockchain.SpeculativeResult) (accessList []byte, local bool, err error) {
	if err := res.State.Error(); err != nil {
		return nil, true, err
	}
	if validator != nil {
		if err := validator.ValidateStateWithCache(block, res); err != nil {
			if dbErr := res.State.Error(); dbErr != nil {
				return nil, true, fmt.Errorf("%w (state access failed: %v)", err, dbErr)
			}
			return nil, false, err
		}
	}
	if err := blockchain.CheckBlockComputationCost(config, block.Number(), res.ProcessStats.ComputationCost); err != nil {
		return nil, false, err
	}
	enc, err := encodeAccessList(res.State)
	if err != nil {
		return nil, true, err
	}
	list, err := access.Decode(enc)
	if err != nil {
		return nil, true, err
	}
	if err := blockchain.CheckBlockAccessListEntries(config, block.Number(), list.EntryCount()); err != nil {
		return nil, false, err
	}
	if err := blockchain.CheckProposalSize(config, block.Number(), uint64(block.Size()), uint64(len(enc))); err != nil {
		return nil, false, err
	}
	return enc, false, nil
}

// encodeAccessList returns the RLP of the BAL recorded in statedb, reads
// included. The list is complete after IntermediateRoot of the block.
func encodeAccessList(statedb *state.StateDB) ([]byte, error) {
	list := access.Recorded(statedb)
	if list == nil {
		return nil, errors.New("block access list not recorded")
	}
	return list.Encode()
}

// ---------------------------------------------------------------------------
// Machine side
// ---------------------------------------------------------------------------

// isInvalidProposal reports whether this node found the proposal invalid by
// executing it in this sequence, or found the block invalid on import, which
// records a bad block. Such a proposal is never committed, voted for or kept
// locked. A proposal is refused for its sequence only: consensus records no
// bad block, as the proposal is not committed.
func (m *machine) isInvalidProposal(hash common.Hash) bool {
	if _, ok := m.invalid[hash]; ok {
		return true
	}
	return m.b.hasBadProposal(hash)
}

// markInvalid refuses a proposal for this sequence for the given reason.
func (m *machine) markInvalid(hash common.Hash, err error) {
	if _, ok := m.invalid[hash]; !ok {
		invalidProposalCounter.Inc(1)
	}
	m.invalid[hash] = err
}

// trySendCommit sends the COMMIT of the accepted proposal. From Amber it waits
// until this node verified the proposal and the BAL of the PRE-PREPARE is the
// one of its execution; handleExecResult sends it then.
func (m *machine) trySendCommit() {
	if m.preprepare == nil || !m.b.isAmberAt(m.sequence.Uint64()) {
		m.sendCommit()
		return
	}
	verified, ok := m.verified[m.preprepare.Proposal.Hash()]
	if ok && bytes.Equal(verified, m.preprepare.AccessList) {
		m.commitPending = false
		m.sendCommit()
		return
	}
	m.commitPending = true
}

// handleExecResult takes the outcome of the speculative execution of an Amber
// proposal.
func (m *machine) handleExecResult(ev execResultEvent) {
	if m.sequence == nil || ev.Number != m.sequence.Uint64() {
		return
	}
	if ev.Local {
		// This node cannot tell; it does not vote, and catches up by sync if
		// the others commit.
		logger.Warn("Failed to verify the proposal by execution", "number", ev.Number, "hash", ev.Hash, "err", ev.Err)
		return
	}
	current := m.preprepare != nil && m.preprepare.Proposal.Hash() == ev.Hash
	if ev.Err != nil {
		logger.Warn("Proposal is invalid by execution", "number", ev.Number, "hash", ev.Hash, "err", ev.Err)
		m.markInvalid(ev.Hash, ev.Err)
		m.reproposal = nil
		if current {
			m.commitPending = false
			m.sendNextRoundChange("execution: invalid proposal")
		}
		return
	}
	m.verified[ev.Hash] = ev.AccessList

	if m.reproposal != nil && ev.Hash == m.reproposal.Hash() && m.isProposer() {
		proposal := m.reproposal
		m.reproposal = nil
		m.sendPreprepare(&bft.Request{Proposal: proposal, AccessList: ev.AccessList})
		return
	}
	if !current {
		return
	}
	if !bytes.Equal(m.preprepare.AccessList, ev.AccessList) {
		// The block is valid but its PRE-PREPARE carries another BAL. Keep the
		// lock: the block may be proposed again with the right BAL.
		logger.Warn("Block access list of the proposal differs from the execution", "number", ev.Number, "hash", ev.Hash)
		m.commitPending = false
		m.sendNextRoundChange("execution: block access list differs")
		return
	}
	if m.commitPending && m.state == statePrepared {
		m.trySendCommit()
	}
}

// reproposeLocked proposes the locked block again in a new round, see
// proposeVerified.
func (m *machine) reproposeLocked() {
	m.proposeVerified(m.preprepare.Proposal)
}

// proposeVerified proposes a block again in a new round: the locked block, or
// from the Permissionless hardfork the value a round-change certificate proves.
// From Amber it carries the BAL of this node's own execution; if this node has
// not executed the block yet, it executes it and proposes it when the result
// comes.
func (m *machine) proposeVerified(proposal bft.Proposal) {
	if !m.b.isAmberAt(m.sequence.Uint64()) {
		m.sendPreprepare(&bft.Request{Proposal: proposal})
		return
	}
	if accessList, ok := m.verified[proposal.Hash()]; ok {
		m.sendPreprepare(&bft.Request{Proposal: proposal, AccessList: accessList})
		return
	}
	m.reproposal = proposal
	m.b.startSpeculativeExecution(proposal, nil)
}

// needsExecution reports whether this node must still execute an Amber
// proposal before it can commit it.
func (m *machine) needsExecution(hash common.Hash) bool {
	if !m.b.isAmberAt(m.sequence.Uint64()) {
		return false
	}
	_, ok := m.verified[hash]
	return !ok && !m.isInvalidProposal(hash)
}
