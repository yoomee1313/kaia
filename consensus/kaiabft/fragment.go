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
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus/bft"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
)

// Fragments of an Amber proposal.
//
// While the proposer executes its block it sends the txs it keeps in
// fragments, PRE-PREPAREs with a PartFragment, each with the block access list
// hint of its phases. It then sends the final PRE-PREPARE: the header of the
// block, a PartFinal that names the fragments by hash, and the hint of the
// whole block. A fragment changes no state of the consensus: a validator keeps
// it, prefetches the keys of its txs and of its hint, and relays it. The final
// message is a PRE-PREPARE as before once the block is assembled from the
// fragments; if one is missing it waits for it, until the round times out.

const (
	// fragmentTxs is the number of txs a proposer puts in a fragment.
	fragmentTxs = 512
	// maxFragmentBytes bounds the fragments a node keeps for one view.
	maxFragmentBytes = 2 * params.MaxProposalSize
	// fragmentPrefetchTimeout bounds the prefetch of the keys of a fragment.
	fragmentPrefetchTimeout = 2 * time.Second
)

var (
	errFragmentNotFromProposer = errors.New("fragment not from the proposer of its view")
	errFragmentView            = errors.New("fragment of a view this node does not keep")
	errFragmentsMissing        = errors.New("fragments of the proposal are missing")
	errFragmentsInvalid        = errors.New("fragments do not make the txs of the proposal")
)

// fragmentEvent carries a fragment a proposer made, for the machine to sign
// and send.
type fragmentEvent struct {
	Number uint64
	Parent common.Hash
	Part   bft.Part
}

type fragmentKey struct{ seq, round uint64 }

// fragmentSet holds the fragments received for one view, by the hash of their
// part data, and a final message waiting for some of them.
type fragmentSet struct {
	byHash  map[common.Hash]*bft.Fragment
	bytes   int
	pending *backlogEvent
}

// fragmentStub is the Proposal of a fragment message: only the number and the
// parent of the block, which proposalNumberMatchesView and the relay use.
func fragmentStub(number uint64, parent common.Hash) *types.Block {
	return types.NewBlockWithHeader(&types.Header{Number: new(big.Int).SetUint64(number), ParentHash: parent})
}

// ---------------------------------------------------------------------------
// Proposer side
// ---------------------------------------------------------------------------

// fragmentHinter makes the hints of the fragments of a proposal, as
// *blockchain.BlockChain does.
type fragmentHinter interface {
	NewFragmentHinter(header *types.Header) (*blockchain.FragmentHinter, error)
}

// commitHooker is an executor that tells the txs it keeps, as
// *work.DefaultExecutor does.
type commitHooker interface {
	SetCommitHook(hook func(txs []*types.Transaction))
}

// fragmenter cuts the txs a proposer keeps into fragments and posts them.
type fragmenter struct {
	b       *backend
	header  *types.Header
	statedb *state.StateDB
	hinter  *blockchain.FragmentHinter
	applied []*types.Transaction
	start   int // index of the first tx of the next fragment
	parts   []bft.Part
}

// newFragmenter returns a fragmenter for an Amber block the node proposes, or
// nil if the chain or the executor cannot make fragments.
func (b *backend) newFragmenter(header *types.Header, statedb *state.StateDB) *fragmenter {
	if !b.isAmberAt(header.Number.Uint64()) {
		return nil
	}
	fh, ok := b.parallel.(fragmentHinter)
	if !ok {
		return nil
	}
	hinter, err := fh.NewFragmentHinter(header)
	if err != nil {
		logger.Debug("Proposing without fragments", "number", header.Number, "err", err)
		return nil
	}
	return &fragmenter{b: b, header: header, statedb: statedb, hinter: hinter}
}

// commit takes the txs the executor kept and sends a fragment every
// fragmentTxs txs.
func (f *fragmenter) commit(txs []*types.Transaction) {
	f.applied = append(f.applied, txs...)
	if len(f.applied)-f.start >= fragmentTxs {
		f.flush()
	}
}

// flush sends the txs kept since the last fragment, if any.
func (f *fragmenter) flush() {
	if len(f.applied) == f.start {
		return
	}
	list := access.Recorded(f.statedb)
	if list == nil {
		return
	}
	hint, err := f.hinter.Next(list, f.applied)
	if err != nil {
		logger.Debug("Failed to make the hint of a fragment", "number", f.header.Number, "err", err)
		return
	}
	hintEnc, err := hint.Encode()
	if err != nil {
		return
	}
	data, err := rlp.EncodeToBytes(&bft.Fragment{Start: uint64(f.start), Txs: f.applied[f.start:], Hint: hintEnc})
	if err != nil {
		return
	}
	part := bft.Part{Kind: bft.PartFragment, Data: data}
	f.parts = append(f.parts, part)
	f.start = len(f.applied)
	// Posted before the proposal, so the machine sends the fragments first.
	f.b.eventMux.Post(fragmentEvent{Number: f.header.Number.Uint64(), Parent: f.header.ParentHash, Part: part})
}

// fragments returns the parts of the fragments sent, after sending the last
// txs, if they hold the txs of block in order; nil otherwise, and the
// proposal is then sent whole.
func (f *fragmenter) fragments(block *types.Block) []bft.Part {
	f.flush()
	txs := block.Transactions()
	if len(f.parts) == 0 || len(f.applied) != len(txs) {
		return nil
	}
	for i, tx := range txs {
		if f.applied[i].Hash() != tx.Hash() {
			return nil
		}
	}
	return f.parts
}

// sendFragment signs and sends a fragment of the proposal of this node for the
// current view. It changes no state.
func (m *machine) sendFragment(ev fragmentEvent) {
	if m.sequence == nil || m.sequence.Uint64() != ev.Number || !m.isProposer() || m.waitingForRoundChange {
		return
	}
	view := m.currentView()
	pp := &bft.Preprepare{View: view, Proposal: fragmentStub(ev.Number, ev.Parent), Parts: []bft.Part{ev.Part}}
	encoded, err := bft.Encode(pp)
	if err != nil {
		return
	}
	msg := &wireMessage{Hash: ev.Parent, Code: bft.MsgPreprepare, Msg: encoded}
	payload := m.signPayload(msg)
	if payload == nil {
		return
	}
	if m.sentFragmentsView == nil || m.sentFragmentsView.Cmp(view) != 0 {
		m.sentFragmentsView, m.sentFragments = view, nil
	}
	m.sentFragments = append(m.sentFragments, crypto.Keccak256Hash(ev.Part.Data))
	m.b.gossipSubPeer(msg.Hash, payload)
}

// finalParts returns the parts of the final PRE-PREPARE of a proposal sent in
// fragments in the current view: the Final and the hint of the block. It
// returns nil if the fragments were not all sent in this view, and the
// proposal is then sent whole.
func (m *machine) finalParts(request *bft.Request) []bft.Part {
	if len(request.Fragments) == 0 || m.sentFragmentsView == nil || m.sentFragmentsView.Cmp(m.currentView()) != 0 {
		return nil
	}
	hashes := make([]common.Hash, len(request.Fragments))
	for i, part := range request.Fragments {
		hashes[i] = crypto.Keccak256Hash(part.Data)
	}
	sent := make(map[common.Hash]struct{}, len(m.sentFragments))
	for _, h := range m.sentFragments {
		sent[h] = struct{}{}
	}
	for _, h := range hashes {
		if _, ok := sent[h]; !ok {
			return nil
		}
	}
	final, err := rlp.EncodeToBytes(&bft.Final{TxCount: uint64(len(request.Proposal.(*types.Block).Transactions())), Fragments: hashes})
	if err != nil {
		return nil
	}
	return bft.WithAccessList([]bft.Part{{Kind: bft.PartFinal, Data: final}}, request.AccessList)
}

// ---------------------------------------------------------------------------
// Validator side
// ---------------------------------------------------------------------------

// handleFragment keeps a fragment from the proposer of its view, for the
// current or the next sequence, and prefetches its keys. It changes no state
// of the consensus. A final message waiting for it is handled again.
func (m *machine) handleFragment(pp *bft.Preprepare, data []byte, src common.Address) error {
	seq, round := pp.View.Sequence.Uint64(), pp.View.Round.Uint64()
	if m.sequence == nil || (seq != m.sequence.Uint64() && seq != m.sequence.Uint64()+1) {
		return errFragmentView
	}
	proposer, err := m.b.valsetModule.GetProposer(seq, round)
	if err != nil {
		return err
	}
	if proposer != src {
		return errFragmentNotFromProposer
	}
	var frag bft.Fragment
	if err := rlp.DecodeBytes(data, &frag); err != nil {
		return bft.ErrInvalidMessage
	}
	key := fragmentKey{seq, round}
	set := m.fragments[key]
	if set == nil {
		set = &fragmentSet{byHash: make(map[common.Hash]*bft.Fragment)}
		m.fragments[key] = set
	}
	hash := crypto.Keccak256Hash(data)
	if _, ok := set.byHash[hash]; ok {
		return nil
	}
	if set.bytes+len(data) > maxFragmentBytes {
		return errFragmentView
	}
	set.byHash[hash] = &frag
	set.bytes += len(data)

	m.b.prefetchFragment(pp.Proposal.ParentHash(), seq, &frag)

	if pending := set.pending; pending != nil {
		set.pending = nil
		go m.b.eventMux.Post(*pending)
	}
	return nil
}

// assembleProposal returns the proposal of a final PRE-PREPARE with its txs
// from the fragments of its view, and the bytes and the entries of the hints of
// the fragments. It returns errFragmentsMissing, keeping the message to be
// handled again, if some fragment has not come yet.
func (m *machine) assembleProposal(pp *bft.Preprepare, data []byte, msg *wireMessage, src common.Address) (*types.Block, int, uint64, error) {
	var final bft.Final
	if err := rlp.DecodeBytes(data, &final); err != nil {
		return nil, 0, 0, bft.ErrInvalidMessage
	}
	key := fragmentKey{pp.View.Sequence.Uint64(), pp.View.Round.Uint64()}
	set := m.fragments[key]
	var (
		txs          types.Transactions
		hintBytes    int
		hintEntries  uint64
		missing      bool
		header, isOK = pp.Proposal.(*types.Block)
	)
	if !isOK {
		return nil, 0, 0, errors.New("kaiabft: invalid proposal type")
	}
	for _, hash := range final.Fragments {
		var frag *bft.Fragment
		if set != nil {
			frag = set.byHash[hash]
		}
		if frag == nil {
			missing = true
			continue
		}
		if frag.Start != uint64(len(txs)) {
			return nil, 0, 0, fmt.Errorf("%w: fragment starts at %d, not %d", errFragmentsInvalid, frag.Start, len(txs))
		}
		txs = append(txs, frag.Txs...)
		hintBytes += len(frag.Hint)
		if hint, err := access.Decode(frag.Hint); err == nil {
			hintEntries += hint.EntryCount()
		}
	}
	if missing {
		if set == nil {
			set = &fragmentSet{byHash: make(map[common.Hash]*bft.Fragment)}
			m.fragments[key] = set
		}
		set.pending = &backlogEvent{src: src, msg: msg, Hash: msg.Hash}
		return nil, 0, 0, errFragmentsMissing
	}
	if uint64(len(txs)) != final.TxCount {
		return nil, 0, 0, fmt.Errorf("%w: %d txs, the final message says %d", errFragmentsInvalid, len(txs), final.TxCount)
	}
	return types.NewBlockWithHeader(header.Header()).WithBody(txs), hintBytes, hintEntries, nil
}

// clearFragments drops the fragments of the sequences before the current one.
func (m *machine) clearFragments() {
	for key := range m.fragments {
		if m.sequence == nil || key.seq < m.sequence.Uint64() {
			delete(m.fragments, key)
		}
	}
	m.sentFragmentsView, m.sentFragments = nil, nil
}

// prefetchFragment reads the keys of the txs and of the hint of a fragment
// from the state of the parent of its block, if this node has it.
func (b *backend) prefetchFragment(parentHash common.Hash, number uint64, frag *bft.Fragment) {
	if number == 0 {
		return
	}
	parent := b.chain.GetHeader(parentHash, number-1)
	if parent == nil {
		return
	}
	hint, _ := access.Decode(frag.Hint)
	signer := types.MakeSigner(b.chain.Config(), new(big.Int).SetUint64(number))
	b.specWg.Add(1)
	go func() {
		defer b.specWg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), fragmentPrefetchTimeout)
		defer cancel()
		if p, ok := b.parallel.(interface {
			PrefetchAccessList(context.Context, common.Hash, *access.BlockAccessList, int)
		}); ok && hint != nil {
			p.PrefetchAccessList(ctx, parent.Root, hint, blockchain.ParallelExecutionWorkers)
		}
		blockchain.PrefetchBlockState(ctx, b.chain, parent.Root, number, frag.Txs, signer)
	}()
}
