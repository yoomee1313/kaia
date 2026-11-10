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

package blockchain

import (
	"bytes"
	"errors"
	"math/big"
	"slices"
	"sort"

	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
)

// The block access list hint of an Amber proposal.
//
// A PRE-PREPARE carries a hint of the block access list, not the list: a
// validator records its own list while executing the block and checks its
// writes against the header, so the hint only says what to prefetch and where
// each phase starts. The hint leaves out what a validator knows from the txs:
// the changes and reads of a tx that runs no EVM code, such as a value
// transfer, are predicted from the tx, the header and the state at the start
// of its phase.
//
// The rule is "leave out what the prediction gets right": the producer drops
// every change of its list that the prediction reproduces, and a validator
// adds the predicted changes the hint does not have, phase by phase, on the
// state the list gives at the start of each phase. By induction on the phases
// the validator rebuilds the list of the producer: before a phase both have the
// same state, so they predict the same, and a change the prediction gets wrong
// is in the hint. The producer checks this by rebuilding the list from its
// hint, and sends the whole list if it does not get it back.

var errNoParentHeader = errors.New("no parent header")

var emptyCodeHashBytes = crypto.Keccak256Hash(nil)

// predictedAccount is the state of an account at the end of a phase as a tx
// that runs no EVM code leaves it: nil fields are not predicted.
type predictedAccount struct {
	balance *big.Int
	nonce   *uint64
}

// hintPredictor predicts the changes of the txs of one block.
type hintPredictor struct {
	config      *params.ChainConfig
	header      *types.Header
	rules       params.Rules
	signer      types.Signer
	number      uint64
	deferredFee bool
	precompiles map[common.Address]struct{}
}

func newHintPredictor(config *params.ChainConfig, header *types.Header) *hintPredictor {
	rules := config.Rules(header.Number)
	p := &hintPredictor{
		config:      config,
		header:      header,
		rules:       rules,
		signer:      types.MakeSigner(config, header.Number),
		number:      header.Number.Uint64(),
		deferredFee: config.Governance != nil && config.Governance.DeferredTxFee(),
		precompiles: make(map[common.Address]struct{}),
	}
	for _, addr := range vm.ActivePrecompiles(rules) {
		p.precompiles[addr] = struct{}{}
	}
	return p
}

// predict returns the changes of a tx on s, the state at the start of its
// phase, and the accounts it reads, or nothing if the tx runs EVM code or its
// effect is not known without running it. It mirrors StateTransition for a
// value transfer: the fee of the gas used, which is the intrinsic gas with the
// signature validation gas and the floor data gas, is paid by the sender or the
// fee payer, the value goes from the sender to the recipient, the sender nonce
// is incremented and the fee is credited to the rewardbase, half of it burnt
// from the Permissionless hardfork.
func (p *hintPredictor) predict(tx *types.Transaction, s *state.StateDB) (map[common.Address]*predictedAccount, []common.Address) {
	switch tx.Type() {
	case types.TxTypeLegacyTransaction:
		if len(tx.Data()) != 0 {
			return nil, nil
		}
	case types.TxTypeValueTransfer, types.TxTypeValueTransferMemo,
		types.TxTypeFeeDelegatedValueTransfer, types.TxTypeFeeDelegatedValueTransferMemo:
	default:
		return nil, nil
	}
	if !p.rules.IsMagma || p.header.BaseFee == nil {
		return nil, nil
	}
	to := tx.To()
	if to == nil {
		return nil, nil
	}
	if _, ok := p.precompiles[*to]; ok {
		return nil, nil
	}
	// A new or empty recipient gets a whole account record or is deleted as
	// touched; a recipient with code runs it.
	if !s.Exist(*to) || s.Empty(*to) || s.GetCodeHash(*to) != emptyCodeHashBytes {
		return nil, nil
	}

	gasFrom, err := tx.ValidateSender(p.signer, s, p.number)
	if err != nil {
		return nil, nil
	}
	from := tx.ValidatedSender()
	feePayer, gasFeePayer := from, uint64(0)
	if tx.IsFeeDelegatedTransaction() {
		if gasFeePayer, err = tx.ValidateFeePayer(p.signer, s, p.number); err != nil {
			return nil, nil
		}
		feePayer = tx.ValidatedFeePayer()
	}
	intrinsic, err := tx.IntrinsicGas(p.number)
	if err != nil {
		return nil, nil
	}
	gas := intrinsic + gasFrom + gasFeePayer
	if p.rules.IsPrague {
		floor, err := FloorDataGas(tx.Type(), tx.Data(), gasFrom+gasFeePayer)
		if err != nil {
			return nil, nil
		}
		gas = max(gas, floor)
	}
	if gas > tx.Gas() {
		return nil, nil
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(gas), tx.EffectiveGasPrice(p.header, p.config))

	out := make(map[common.Address]*predictedAccount)
	balance := func(addr common.Address) *big.Int {
		a := out[addr]
		if a == nil {
			a = &predictedAccount{}
			out[addr] = a
		}
		if a.balance == nil {
			a.balance = new(big.Int).Set(s.GetBalance(addr))
		}
		return a.balance
	}
	balance(feePayer).Sub(balance(feePayer), fee)
	balance(from).Sub(balance(from), tx.Value())
	balance(*to).Add(balance(*to), tx.Value())
	reads := []common.Address{from, *to, feePayer}
	if !p.deferredFee {
		credited := fee
		if p.rules.IsPermissionless {
			credited = new(big.Int).Sub(fee, new(big.Int).Div(fee, big.NewInt(2)))
		}
		rewardbase := p.header.Rewardbase
		balance(rewardbase).Add(balance(rewardbase), credited)
		reads = append(reads, rewardbase)
	}
	nonce := s.GetNonce(from) + 1
	out[from].nonce = &nonce
	for _, a := range out {
		if a.balance != nil && a.balance.Sign() < 0 {
			return nil, nil // the tx could not have run
		}
	}
	return out, reads
}

// hintList is a block access list being edited: accounts by address and the
// accounts that change in each phase.
type hintList struct {
	accounts map[common.Address]*access.AccountAccess
	phases   map[uint32]map[common.Address]struct{}
}

func newHintList(list *access.BlockAccessList) *hintList {
	h := &hintList{
		accounts: make(map[common.Address]*access.AccountAccess, len(list.Accounts)),
		phases:   make(map[uint32]map[common.Address]struct{}),
	}
	for i := range list.Accounts {
		a := copyAccountAccess(&list.Accounts[i])
		h.accounts[a.Address] = a
		for _, k := range changeIndices(a) {
			h.mark(k, a.Address)
		}
	}
	return h
}

func (h *hintList) mark(k uint32, addr common.Address) {
	m := h.phases[k]
	if m == nil {
		m = make(map[common.Address]struct{})
		h.phases[k] = m
	}
	m[addr] = struct{}{}
}

func (h *hintList) account(addr common.Address) *access.AccountAccess {
	a := h.accounts[addr]
	if a == nil {
		a = &access.AccountAccess{Address: addr}
		h.accounts[addr] = a
	}
	return a
}

// apply writes the changes of phase k to s, which holds the start of phase k.
func (h *hintList) apply(s *state.StateDB, k uint32) error {
	var phase access.BlockAccessList
	for addr := range h.phases[k] {
		phase.Accounts = append(phase.Accounts, h.accounts[addr].At(k))
	}
	return access.ApplyPhase(s, &phase, int(k))
}

func (h *hintList) list() *access.BlockAccessList {
	out := &access.BlockAccessList{Accounts: make([]access.AccountAccess, 0, len(h.accounts))}
	for _, a := range h.accounts {
		out.Accounts = append(out.Accounts, *a)
	}
	slices.SortFunc(out.Accounts, func(x, y access.AccountAccess) int { return bytes.Compare(x.Address[:], y.Address[:]) })
	return out
}

func copyAccountAccess(a *access.AccountAccess) *access.AccountAccess {
	c := *a
	c.StorageWrites = slices.Clone(a.StorageWrites)
	c.StorageReads = slices.Clone(a.StorageReads)
	c.BalanceChanges = slices.Clone(a.BalanceChanges)
	c.NonceChanges = slices.Clone(a.NonceChanges)
	c.CodeChanges = slices.Clone(a.CodeChanges)
	c.AccountChanges = slices.Clone(a.AccountChanges)
	c.Deleted = slices.Clone(a.Deleted)
	c.StorageCleared = slices.Clone(a.StorageCleared)
	return &c
}

func changeIndices(a *access.AccountAccess) []uint32 {
	var ks []uint32
	for _, w := range a.StorageWrites {
		for _, c := range w.Changes {
			ks = append(ks, c.Index)
		}
	}
	for _, c := range a.BalanceChanges {
		ks = append(ks, c.Index)
	}
	for _, c := range a.NonceChanges {
		ks = append(ks, c.Index)
	}
	for _, c := range a.CodeChanges {
		ks = append(ks, c.Index)
	}
	for _, c := range a.AccountChanges {
		ks = append(ks, c.Index)
	}
	ks = append(ks, a.Deleted...)
	ks = append(ks, a.StorageCleared...)
	slices.Sort(ks)
	return slices.Compact(ks)
}

func balanceAt(a *access.AccountAccess, k uint32) (int, bool) {
	i := sort.Search(len(a.BalanceChanges), func(i int) bool { return a.BalanceChanges[i].Index >= k })
	return i, i < len(a.BalanceChanges) && a.BalanceChanges[i].Index == k
}

func nonceAt(a *access.AccountAccess, k uint32) (int, bool) {
	i := sort.Search(len(a.NonceChanges), func(i int) bool { return a.NonceChanges[i].Index >= k })
	return i, i < len(a.NonceChanges) && a.NonceChanges[i].Index == k
}

// isEmptyAccess reports whether an account entry holds nothing, so that it
// only says that the account was read.
func isEmptyAccess(a *access.AccountAccess) bool {
	return !a.Changed() && len(a.StorageReads) == 0
}

// stripAccessList returns the hint of list: list without the changes and the
// reads the prediction gets right. s is the parent state of the block.
func stripAccessList(config *params.ChainConfig, block *types.Block, s *state.StateDB, list *access.BlockAccessList) (*access.BlockAccessList, error) {
	h := newHintList(list)
	p := newHintPredictor(config, block.Header())
	if err := h.apply(s, 0); err != nil {
		return nil, err
	}
	type key struct {
		addr  common.Address
		nonce bool
		k     uint32
	}
	strip := make(map[key]struct{})
	predictedReads := make(map[common.Address]struct{})
	for i, tx := range block.Transactions() {
		k := uint32(i + 1)
		preds, reads := p.predict(tx, s)
		for addr, pa := range preds {
			a := h.accounts[addr]
			if a == nil {
				continue
			}
			if j, ok := balanceAt(a, k); ok && pa.balance != nil && a.BalanceChanges[j].Balance.Cmp(pa.balance) == 0 {
				strip[key{addr, false, k}] = struct{}{}
			}
			if j, ok := nonceAt(a, k); ok && pa.nonce != nil && a.NonceChanges[j].Nonce == *pa.nonce {
				strip[key{addr, true, k}] = struct{}{}
			}
		}
		for _, addr := range reads {
			predictedReads[addr] = struct{}{}
		}
		if err := h.apply(s, k); err != nil {
			return nil, err
		}
	}

	hint := &access.BlockAccessList{Accounts: make([]access.AccountAccess, 0, len(list.Accounts))}
	for i := range list.Accounts {
		a := copyAccountAccess(&list.Accounts[i])
		a.BalanceChanges = slices.DeleteFunc(a.BalanceChanges, func(c access.BalanceChange) bool {
			_, ok := strip[key{a.Address, false, c.Index}]
			return ok
		})
		a.NonceChanges = slices.DeleteFunc(a.NonceChanges, func(c access.NonceChange) bool {
			_, ok := strip[key{a.Address, true, c.Index}]
			return ok
		})
		if _, read := predictedReads[a.Address]; read && isEmptyAccess(a) {
			continue
		}
		hint.Accounts = append(hint.Accounts, *a)
	}
	return hint, nil
}

// rebuildAccessList returns the list a hint stands for: the hint with the
// changes and the reads the prediction gives where the hint has none. s is the
// parent state of the block.
func rebuildAccessList(config *params.ChainConfig, block *types.Block, s *state.StateDB, hint *access.BlockAccessList) (*access.BlockAccessList, error) {
	h := newHintList(hint)
	p := newHintPredictor(config, block.Header())
	if err := h.apply(s, 0); err != nil {
		return nil, err
	}
	for i, tx := range block.Transactions() {
		k := uint32(i + 1)
		preds, reads := p.predict(tx, s)
		for addr, pa := range preds {
			a := h.account(addr)
			if pa.balance != nil {
				if j, ok := balanceAt(a, k); !ok && pa.balance.Cmp(s.GetBalance(addr)) != 0 {
					a.BalanceChanges = slices.Insert(a.BalanceChanges, j, access.BalanceChange{Index: k, Balance: pa.balance})
					h.mark(k, addr)
				}
			}
			if pa.nonce != nil {
				if j, ok := nonceAt(a, k); !ok && *pa.nonce != s.GetNonce(addr) {
					a.NonceChanges = slices.Insert(a.NonceChanges, j, access.NonceChange{Index: k, Nonce: *pa.nonce})
					h.mark(k, addr)
				}
			}
		}
		for _, addr := range reads {
			h.account(addr)
		}
		if err := h.apply(s, k); err != nil {
			return nil, err
		}
	}
	return h.list(), nil
}

// AccessListHint returns the block access list hint that an Amber proposal
// carries for list, the list recorded while producing block: list without what
// a validator predicts from the txs. It returns list itself if a validator
// would not rebuild list from the hint.
func (bc *BlockChain) AccessListHint(block *types.Block, list *access.BlockAccessList) (*access.BlockAccessList, error) {
	parent := bc.GetHeaderByHash(block.ParentHash())
	if parent == nil {
		return list, errNoParentHeader
	}
	s, err := bc.StateAt(parent.Root)
	if err != nil {
		return list, err
	}
	hint, err := stripAccessList(bc.chainConfig, block, s, list)
	if err != nil {
		return list, err
	}
	rebuilt, err := bc.RebuildAccessList(block, parent, hint)
	if err != nil {
		return list, nil
	}
	want, err := list.Encode()
	if err != nil {
		return list, err
	}
	got, err := rebuilt.Encode()
	if err != nil || !bytes.Equal(got, want) {
		accessListHintFallbackCounter.Inc(1)
		return list, nil
	}
	return hint, nil
}

// RebuildAccessList returns the block access list that the hint of an Amber
// proposal stands for, see AccessListHint, from the state of parent.
func (bc *BlockChain) RebuildAccessList(block *types.Block, parent *types.Header, hint *access.BlockAccessList) (*access.BlockAccessList, error) {
	s, err := bc.StateAt(parent.Root)
	if err != nil {
		return nil, err
	}
	return rebuildAccessList(bc.chainConfig, block, s, hint)
}

// FragmentHinter makes the block access list hints of the fragments of an
// Amber proposal while the producer executes it, see bft.Fragment. The hint of
// a fragment holds the changes of its phases and the keys first read in them,
// without what the prediction gets right, as AccessListHint leaves them out.
// The hints of the fragments are for validators to prefetch: the final message
// of the proposal carries the hint of the whole block, which they execute
// with.
type FragmentHinter struct {
	p    *hintPredictor
	s    *state.StateDB
	done int // phases already given, from 0: InitializeState
	sent map[common.Address]map[common.Hash]struct{}
}

// NewFragmentHinter returns a FragmentHinter for a block with header, built on
// the state of its parent.
func (bc *BlockChain) NewFragmentHinter(header *types.Header) (*FragmentHinter, error) {
	parent := bc.GetHeaderByHash(header.ParentHash)
	if parent == nil {
		return nil, errNoParentHeader
	}
	s, err := bc.StateAt(parent.Root)
	if err != nil {
		return nil, err
	}
	return &FragmentHinter{
		p:    newHintPredictor(bc.chainConfig, header),
		s:    s,
		done: -1,
		sent: make(map[common.Address]map[common.Hash]struct{}),
	}, nil
}

// Next returns the hint of the txs applied after the last call, given list,
// the block access list recorded so far, and txs, every tx applied so far. The
// first call gives InitializeState too.
func (f *FragmentHinter) Next(list *access.BlockAccessList, txs []*types.Transaction) (*access.BlockAccessList, error) {
	h := newHintList(list)
	from := uint32(f.done + 1)
	type key struct {
		addr  common.Address
		nonce bool
		k     uint32
	}
	strip := make(map[key]struct{})
	predictedReads := make(map[common.Address]struct{})
	for k := f.done + 1; k <= len(txs); k++ {
		if k > 0 {
			preds, reads := f.p.predict(txs[k-1], f.s)
			for addr, pa := range preds {
				a := h.accounts[addr]
				if a == nil {
					continue
				}
				if j, ok := balanceAt(a, uint32(k)); ok && pa.balance != nil && a.BalanceChanges[j].Balance.Cmp(pa.balance) == 0 {
					strip[key{addr, false, uint32(k)}] = struct{}{}
				}
				if j, ok := nonceAt(a, uint32(k)); ok && pa.nonce != nil && a.NonceChanges[j].Nonce == *pa.nonce {
					strip[key{addr, true, uint32(k)}] = struct{}{}
				}
			}
			for _, addr := range reads {
				predictedReads[addr] = struct{}{}
			}
		}
		if err := h.apply(f.s, uint32(k)); err != nil {
			return nil, err
		}
	}
	to := uint32(len(txs))
	f.done = len(txs)

	inRange := func(k uint32) bool { return k >= from && k <= to }
	out := &access.BlockAccessList{}
	for i := range list.Accounts {
		a := &list.Accounts[i]
		frag := access.AccountAccess{Address: a.Address}
		for _, w := range a.StorageWrites {
			var changes []access.SlotChange
			for _, c := range w.Changes {
				if inRange(c.Index) {
					changes = append(changes, c)
				}
			}
			if len(changes) > 0 {
				frag.StorageWrites = append(frag.StorageWrites, access.SlotWrites{Slot: w.Slot, Changes: changes})
			}
		}
		for _, c := range a.BalanceChanges {
			if _, s := strip[key{a.Address, false, c.Index}]; inRange(c.Index) && !s {
				frag.BalanceChanges = append(frag.BalanceChanges, c)
			}
		}
		for _, c := range a.NonceChanges {
			if _, s := strip[key{a.Address, true, c.Index}]; inRange(c.Index) && !s {
				frag.NonceChanges = append(frag.NonceChanges, c)
			}
		}
		for _, c := range a.CodeChanges {
			if inRange(c.Index) {
				frag.CodeChanges = append(frag.CodeChanges, c)
			}
		}
		for _, c := range a.AccountChanges {
			if inRange(c.Index) {
				frag.AccountChanges = append(frag.AccountChanges, c)
			}
		}
		for _, k := range a.Deleted {
			if inRange(k) {
				frag.Deleted = append(frag.Deleted, k)
			}
		}
		for _, k := range a.StorageCleared {
			if inRange(k) {
				frag.StorageCleared = append(frag.StorageCleared, k)
			}
		}
		sent, known := f.sent[a.Address]
		for _, slot := range a.StorageReads {
			if _, ok := sent[slot]; !ok {
				frag.StorageReads = append(frag.StorageReads, slot)
			}
		}
		if !known {
			sent = make(map[common.Hash]struct{})
			f.sent[a.Address] = sent
		}
		for _, slot := range frag.StorageReads {
			sent[slot] = struct{}{}
		}
		if isEmptyAccess(&frag) {
			// A read of the account is given once, and not at all if the
			// prediction gives it.
			if _, predicted := predictedReads[a.Address]; known || predicted {
				continue
			}
		}
		out.Accounts = append(out.Accounts, frag)
	}
	return out, nil
}
