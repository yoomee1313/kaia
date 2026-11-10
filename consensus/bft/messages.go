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

// Package bft hosts wire-level BFT consensus types shared by Istanbul and
// KaiaBFT engines. Any change to these types is a wire-protocol change and must
// be considered against backward compatibility with deployed nodes.
package bft

import (
	"fmt"
	"io"
	"math/big"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
)

// Proposal supports retrieving height and serialized block to be used during BFT consensus.
type Proposal interface {
	// Number retrieves the sequence number of this proposal.
	Number() *big.Int

	// Hash retrieves the hash of this proposal.
	Hash() common.Hash

	EncodeRLP(w io.Writer) error

	DecodeRLP(s *rlp.Stream) error

	String() string

	ParentHash() common.Hash

	Header() *types.Header

	WithSeal(header *types.Header) *types.Block
}

// Request wraps a proposal to be processed by a BFT engine. From the Amber
// hardfork a block producer adds the block access list hint of the proposal,
// and the fragments of its txs it already sent, if any.
type Request struct {
	Proposal   Proposal
	AccessList []byte
	Fragments  []Part
}

// View includes a round number and a sequence number.
//
// Sequence is the block number we'd like to commit.
// Each round has a number and is composed by 3 steps: preprepare, prepare and commit.
//
// If the given block is not accepted by validators, a round change will occur
// and the validators start a new round with round+1.
type View struct {
	Round    *big.Int
	Sequence *big.Int
}

// EncodeRLP serializes a View into the Kaia RLP format.
func (v *View) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []any{v.Round, v.Sequence})
}

// DecodeRLP deserializes a View from a Kaia RLP stream.
func (v *View) DecodeRLP(s *rlp.Stream) error {
	var view struct {
		Round    *big.Int
		Sequence *big.Int
	}
	if err := s.Decode(&view); err != nil {
		return err
	}
	v.Round, v.Sequence = view.Round, view.Sequence
	return nil
}

func (v *View) String() string {
	return fmt.Sprintf("{Round: %d, Sequence: %d}", v.Round.Uint64(), v.Sequence.Uint64())
}

// Cmp compares v and y and returns:
//
//	-1 if v < y
//	 0 if v == y
//	+1 if v > y
func (v *View) Cmp(y *View) int {
	sdiff := v.Sequence.Cmp(y.Sequence)
	if sdiff != 0 {
		return sdiff
	}
	rdiff := v.Round.Cmp(y.Round)
	if rdiff != 0 {
		return rdiff
	}
	return 0
}

// ConsensusMsg is the envelope used by p2p for forwarded consensus messages.
type ConsensusMsg struct {
	PrevHash common.Hash
	Payload  []byte
}

// Preprepare is the message sent by the proposer to propose a new block.
//
// A PRE-PREPARE above round 0 is justified by RoundChangeCertificate, a quorum
// of signed ROUND CHANGE messages for its view stripped of their Evidence. When
// any of them claims a prepared value, PreparedMessages are the PREPARE/COMMIT
// votes proving the highest claim for Proposal. The prepared block is therefore
// sent once, as the Proposal itself, instead of once per ROUND CHANGE.
//
// From the Amber hardfork it carries Parts, typed payloads that new kinds of
// content can be added to without a new message: the block access list hint
// of the proposal (PartHint), a fragment of its txs sent ahead while the
// proposer executes them (PartFragment), and the final message that names the
// fragments of the block (PartFinal). The parts are covered by the signature
// of the message, so the proposer answers for them; they are not part of the
// block, whose header commits the hash of the writes of its block access list.
// A node ignores the parts of a kind it does not know: kinds that a node needs
// to get the block are enabled by a hardfork. Before Amber there are none and
// they are not encoded, so the message is the same as before.
type Preprepare struct {
	View                   *View
	Proposal               Proposal
	RoundChangeCertificate []*Message
	PreparedMessages       []*Message
	Parts                  []Part
}

// Kinds of the parts of a PRE-PREPARE.
const (
	// PartHint is the RLP of the block access list hint of the proposal.
	PartHint uint64 = 1
	// PartFragment is the RLP of a Fragment. A PRE-PREPARE that has one is a
	// fragment message: its Proposal holds only the number and the parent of
	// the block, and it changes no state of the consensus.
	PartFragment uint64 = 2
	// PartFinal is the RLP of a Final. A PRE-PREPARE that has one has the
	// header of the block as its Proposal, without the txs.
	PartFinal uint64 = 3
)

// Part is a typed payload of a PRE-PREPARE.
type Part struct {
	Kind uint64
	Data []byte
}

// Fragment holds the txs of a proposal from index Start on, which the proposer
// sends while it executes them, with the block access list hint of their
// phases.
type Fragment struct {
	Start uint64
	Txs   types.Transactions
	Hint  []byte
}

// Final names the fragments of a proposal by the hashes of their part data,
// in the order of their txs, and the number of its txs.
type Final struct {
	TxCount   uint64
	Fragments []common.Hash
}

// Part returns the data of the first part of the given kind, nil if none.
func (b *Preprepare) Part(kind uint64) []byte {
	for _, p := range b.Parts {
		if p.Kind == kind {
			return p.Data
		}
	}
	return nil
}

// AccessList returns the block access list hint of the PRE-PREPARE.
func (b *Preprepare) AccessList() []byte {
	return b.Part(PartHint)
}

// WithAccessList returns the parts with the hint set to hint, nil if hint is
// empty.
func WithAccessList(parts []Part, hint []byte) []Part {
	out := make([]Part, 0, len(parts)+1)
	for _, p := range parts {
		if p.Kind != PartHint {
			out = append(out, p)
		}
	}
	if len(hint) != 0 {
		out = append(out, Part{Kind: PartHint, Data: hint})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EncodeRLP serializes a Preprepare into the Kaia RLP format.
func (b *Preprepare) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, struct {
		View                   *View
		Proposal               Proposal
		RoundChangeCertificate []*Message `rlp:"optional"`
		PreparedMessages       []*Message `rlp:"optional"`
		Parts                  []Part     `rlp:"optional"`
	}{b.View, b.Proposal, b.RoundChangeCertificate, b.PreparedMessages, b.Parts})
}

// DecodeRLP deserializes a Preprepare from a Kaia RLP stream.
// Proposal is decoded as a *types.Block, which is the only concrete Proposal
// implementation exchanged on the wire.
func (b *Preprepare) DecodeRLP(s *rlp.Stream) error {
	var preprepare struct {
		View                   *View
		Proposal               *types.Block
		RoundChangeCertificate []*Message `rlp:"optional"`
		PreparedMessages       []*Message `rlp:"optional"`
		Parts                  []Part     `rlp:"optional"`
	}
	if err := s.Decode(&preprepare); err != nil {
		return err
	}
	b.View, b.Proposal = preprepare.View, preprepare.Proposal
	b.RoundChangeCertificate, b.PreparedMessages = preprepare.RoundChangeCertificate, preprepare.PreparedMessages
	b.Parts = preprepare.Parts
	return nil
}

// PreparedCertificate proves that a proposal reached the prepared state in a
// prior round. Messages are the signed PREPARE/COMMIT envelopes that form the
// quorum; carrying the complete envelopes lets receivers authenticate every
// voter without persisting the certificate in the block header. It travels as
// ROUND CHANGE Evidence and is never signed by the ROUND CHANGE sender.
type PreparedCertificate struct {
	View     *View
	Proposal *types.Block
	Messages []*Message
}

// PreparedClaim is the signed summary of the PreparedCertificate a ROUND
// CHANGE sender holds: the round in which it prepared, the proposal hash, and
// the Keccak-256 hash of the encoded certificate attached as Evidence. Evidence
// stays outside the signature so that a PRE-PREPARE can embed the ROUND CHANGE
// without it; EvidenceHash binds the Evidence to its signer, so a relay cannot
// substitute different Evidence for a signed claim.
type PreparedClaim struct {
	Round        *big.Int
	Digest       common.Hash
	EvidenceHash common.Hash
}

// Prepare is the PREPARE payload.
type Prepare struct {
	View   *View
	Digest common.Hash
}

// Commit is the COMMIT payload. Its committed seal is part of Msg and is
// therefore authenticated by the outer Message signature.
type Commit struct {
	View          *View
	Digest        common.Hash
	CommittedSeal []byte
}

// RoundChange is the ROUND-CHANGE payload. The certificate proving Prepared is
// carried once as the outer Message Evidence.
type RoundChange struct {
	View     *View
	Prepared *PreparedClaim `rlp:"optional,nilList"`
}
