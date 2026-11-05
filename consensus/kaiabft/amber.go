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
	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/bft"
	"github.com/kaiachain/kaia/params"
)

// From the Amber hardfork a PRE-PREPARE carries the block access list (BAL)
// of the proposal, reads included, and a validator sends its COMMIT only after
// it executed the proposal and found it valid: the state, the receipts and the
// BAL hash match the header, the block is within the Amber limits, and the BAL
// of its own execution is the one of the PRE-PREPARE. PREPARE is sent as before
// and execution starts on the PRE-PREPARE, so execution overlaps the PREPARE
// round.

var (
	errMissingAccessList    = errors.New("proposal without a block access list")
	errUnexpectedAccessList = errors.New("block access list before the Amber hardfork")
	errRejectedProposal     = errors.New("proposal found invalid by execution")
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
// writes hash of the header and within the entry limit.
func checkProposalAccessList(config *params.ChainConfig, pp *bft.Preprepare) error {
	number := pp.Proposal.Number()
	if !config.IsAmberForkEnabled(number) {
		if len(pp.AccessList) != 0 {
			return errUnexpectedAccessList
		}
		return nil
	}
	if len(pp.AccessList) == 0 {
		return errMissingAccessList
	}
	list, err := bal.Decode(pp.AccessList)
	if err != nil {
		return fmt.Errorf("block access list: %w", err)
	}
	if err := list.CheckHash(pp.Proposal.Header().BlockAccessListHash); err != nil {
		return err
	}
	return blockchain.CheckBlockAccessListEntries(config, number, list.EntryCount())
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
	list, err := bal.Decode(enc)
	if err != nil {
		return nil, true, err
	}
	if err := blockchain.CheckBlockAccessListEntries(config, block.Number(), list.EntryCount()); err != nil {
		return nil, false, err
	}
	return enc, false, nil
}

// encodeAccessList returns the RLP of the BAL recorded in statedb, reads
// included. The list is complete after IntermediateRoot of the block.
func encodeAccessList(statedb *state.StateDB) ([]byte, error) {
	list := statedb.BlockAccessList()
	if list == nil {
		return nil, errors.New("block access list not recorded")
	}
	return list.Encode()
}

// ---------------------------------------------------------------------------
// Machine side
// ---------------------------------------------------------------------------

// isRejected reports whether this node found the proposal invalid by
// executing it in this sequence. Such a proposal is never committed or kept
// locked, even if the block itself is not marked bad: the BAL entry limit is
// checked by consensus nodes only.
func (m *machine) isRejected(hash common.Hash) bool {
	_, ok := m.rejected[hash]
	return ok
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
		m.rejected[ev.Hash] = struct{}{}
		m.reproposeOnVerify = false
		if current {
			m.commitPending = false
			m.sendNextRoundChange("execution: invalid proposal")
		}
		return
	}
	m.verified[ev.Hash] = ev.AccessList

	if m.reproposeOnVerify && ev.Hash == m.lockedHash && m.isProposer() && m.preprepare != nil {
		m.reproposeOnVerify = false
		m.sendPreprepare(&bft.Request{Proposal: m.preprepare.Proposal, AccessList: ev.AccessList})
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

// reproposeLocked proposes the locked block again in a new round. From Amber
// it carries the BAL of this node's own execution; if this node has not
// verified the block yet, it executes it and proposes it once verified.
func (m *machine) reproposeLocked() {
	if !m.b.isAmberAt(m.sequence.Uint64()) {
		m.sendPreprepare(&bft.Request{Proposal: m.preprepare.Proposal})
		return
	}
	if accessList, ok := m.verified[m.lockedHash]; ok {
		m.sendPreprepare(&bft.Request{Proposal: m.preprepare.Proposal, AccessList: accessList})
		return
	}
	m.reproposeOnVerify = true
	m.b.startSpeculativeExecution(m.preprepare.Proposal)
}

// needsExecution reports whether this node must still execute an Amber
// proposal before it can commit it.
func (m *machine) needsExecution(hash common.Hash) bool {
	if !m.b.isAmberAt(m.sequence.Uint64()) {
		return false
	}
	_, ok := m.verified[hash]
	return !ok && !m.isRejected(hash)
}
