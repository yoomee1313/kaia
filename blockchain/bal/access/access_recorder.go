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

package access

import (
	"bytes"
	"maps"
	"math/big"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/rlp"
)

var (
	stateLogger   = log.NewModuleLogger(log.BlockchainState)
	emptyCodeHash = crypto.Keccak256Hash(nil)
)

// accessRecorder records the block access list while a block is executed. It
// only observes the StateDB and never changes state or gas.
//
// Reads are recorded when an account is accessed and when a slot is read
// through GetState or GetCommittedState. Writes are recorded as post values
// whenever the journal is flushed: at every Finalise, which marks the end of a
// tx, and when the phase index changes, so that changes of InitializeState that
// were not finalised are not attributed to the first tx.
//
// Reads are held as pending until the phase they belong to is kept: they are
// committed at Finalise and at the end of InitializeState, and dropped when a
// tx starts and when a whole tx is reverted. So a block producer that reads
// state between txs, or runs a tx it then drops, records the same reads as a
// validator that runs only the included txs.
type accessRecorder struct {
	index   uint32
	builder *Builder

	pendingAccounts map[common.Address]struct{}
	pendingSlots    map[common.Address]map[common.Hash]struct{}

	// seen holds, per account, the values as of the last recorded change, or
	// as loaded from the parent state. A change is recorded only if the value
	// differs from it.
	seen map[common.Address]*accessView

	// journalOffset is where the next flush starts reading journal entries.
	journalOffset int

	// merged, if set, is the list of the whole block that Recorded returns,
	// see SetMerged.
	merged *BlockAccessList
}

type accessView struct {
	exists   bool
	balance  *big.Int
	nonce    uint64
	codeHash common.Hash
	fields   []byte // Kaia-specific fields, see kaiaFields
}

func newAccessView(acc account.Account) *accessView {
	if acc == nil {
		return &accessView{balance: new(big.Int), codeHash: emptyCodeHash}
	}
	codeHash := emptyCodeHash
	if pa := account.GetProgramAccount(acc); pa != nil {
		codeHash = common.BytesToHash(pa.GetCodeHash())
	}
	return &accessView{
		exists:   true,
		balance:  new(big.Int).Set(acc.GetBalance()),
		nonce:    acc.GetNonce(),
		codeHash: codeHash,
		fields:   kaiaFields(acc),
	}
}

// kaiaFields encodes the account without balance, nonce, code hash and storage
// root, leaving the Kaia-specific fields: account type, humanReadable, key and
// code info.
func kaiaFields(acc account.Account) []byte {
	c := acc.DeepCopy()
	c.SetBalance(new(big.Int))
	c.SetNonce(0)
	if pa := account.GetProgramAccount(c); pa != nil {
		pa.SetStorageRoot(common.ExtHash{})
		pa.SetCodeHash(emptyCodeHash.Bytes())
	}
	enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(c))
	if err != nil {
		stateLogger.Error("Failed to encode account fields for the block access list", "err", err)
	}
	return enc
}

func newAccessRecorder(idx int) *accessRecorder {
	return &accessRecorder{
		index:           uint32(idx),
		builder:         NewBuilder(),
		seen:            make(map[common.Address]*accessView),
		pendingAccounts: make(map[common.Address]struct{}),
		pendingSlots:    make(map[common.Address]map[common.Hash]struct{}),
	}
}

// EnableRecorder starts recording the block access list. It must be called
// on a StateDB opened at the parent root, before the block is executed.
// Accounts already loaded hold parent values and are taken as the base of
// their changes. The phase index starts at 0 (InitializeState).
func EnableRecorder(s *state.StateDB) {
	if s.Modified() {
		stateLogger.Error("Block access recorder enabled on a modified state, the list will be wrong")
	}
	r := newAccessRecorder(0)
	s.LoadedAccounts(func(addr common.Address, acc account.Account, deleted bool) {
		r.seen[addr] = newAccessView(acc)
	})
	s.SetAccessRecorder(r)
}

// EnableRecorderAt starts recording at phase idx on a state that holds the
// start of that phase: the current values are the base of the changes, so the
// list holds the changes of the phases from idx on.
func EnableRecorderAt(s *state.StateDB, idx int) {
	r := newAccessRecorder(idx)
	s.LoadedAccounts(func(addr common.Address, acc account.Account, deleted bool) {
		if deleted {
			acc = nil
		}
		r.seen[addr] = newAccessView(acc)
	})
	s.SetAccessRecorder(r)
}

// Recording reports whether the block access recorder is on.
func Recording(s *state.StateDB) bool {
	_, ok := s.AccessRecorder().(*accessRecorder)
	return ok
}

// Recorded returns the recorded block access list, or nil if the recorder is
// not enabled. Changes are recorded at Finalise, so it is complete after
// IntermediateRoot of the block. Storage roots are not recorded.
func Recorded(s *state.StateDB) *BlockAccessList {
	r, ok := s.AccessRecorder().(*accessRecorder)
	if !ok {
		return nil
	}
	if r.merged != nil {
		return r.merged
	}
	return r.builder.Build()
}

// SetMerged makes Recorded return list for s, the list of a whole block whose
// phases were executed on states of their own and merged. s must have a
// recorder.
func SetMerged(s *state.StateDB, list *BlockAccessList) {
	if r, ok := s.AccessRecorder().(*accessRecorder); ok {
		r.merged = list
	}
}

// Copy returns a deep copy of the recorder for a copy of the StateDB, which
// starts with an empty journal. Views in seen are replaced, never modified, so
// they are shared.
func (r *accessRecorder) Copy() state.AccessRecorder {
	slots := make(map[common.Address]map[common.Hash]struct{}, len(r.pendingSlots))
	for addr, m := range r.pendingSlots {
		slots[addr] = maps.Clone(m)
	}
	return &accessRecorder{
		index:           r.index,
		builder:         r.builder.Copy(),
		seen:            maps.Clone(r.seen),
		pendingAccounts: maps.Clone(r.pendingAccounts),
		pendingSlots:    slots,
		merged:          r.merged,
	}
}

// SetIndex starts a phase: 0 for InitializeState, i+1 for the i-th tx, and
// len(txs)+1 for FinalizeState. Changes not yet finalised are recorded under
// the old index. Pending reads are dropped: they were made after the last
// Finalise, outside of any tx, as a block producer does.
func (r *accessRecorder) SetIndex(s *state.StateDB, idx int) {
	r.dropReads()
	if r.index == uint32(idx) {
		return
	}
	r.capture(s, false, false)
	r.index = uint32(idx)
}

// EndInitialization keeps the changes and reads of InitializeState, which may
// not be finalised.
func (r *accessRecorder) EndInitialization(s *state.StateDB) {
	r.capture(s, false, false)
	r.commitReads()
}

func (r *accessRecorder) Finalise(s *state.StateDB, deleteEmptyObjects bool) {
	r.capture(s, true, deleteEmptyObjects)
}

// RevertedTx drops the reads of a whole tx that is dropped, as a block
// producer does with a tx it cannot include.
func (r *accessRecorder) RevertedTx() { r.dropReads() }

// AccountLoaded takes the values of a newly loaded account as the base of its
// changes, and records the read.
func (r *accessRecorder) AccountLoaded(addr common.Address, acc account.Account) {
	if _, ok := r.seen[addr]; !ok {
		r.seen[addr] = newAccessView(acc)
	}
	r.AccountRead(addr)
}

// AccountRead records a read of an account, loaded or not. It is recorded on
// every access, not on load only: a tx that is dropped may be the one that
// loaded the account.
func (r *accessRecorder) AccountRead(addr common.Address) {
	r.pendingAccounts[addr] = struct{}{}
}

func (r *accessRecorder) SlotRead(addr common.Address, slot common.Hash) {
	m := r.pendingSlots[addr]
	if m == nil {
		m = make(map[common.Hash]struct{})
		r.pendingSlots[addr] = m
	}
	m[slot] = struct{}{}
}

func (r *accessRecorder) commitReads() {
	for addr := range r.pendingAccounts {
		r.builder.ReadAccount(addr)
	}
	for addr, slots := range r.pendingSlots {
		for slot := range slots {
			r.builder.ReadSlot(addr, slot)
		}
	}
	r.dropReads()
}

func (r *accessRecorder) dropReads() {
	if len(r.pendingAccounts) > 0 {
		r.pendingAccounts = make(map[common.Address]struct{})
	}
	if len(r.pendingSlots) > 0 {
		r.pendingSlots = make(map[common.Address]map[common.Hash]struct{})
	}
}

// capture records the post values of the accounts changed since the last
// flush. If final is set, it runs at Finalise and records the deletions that
// Finalise is about to apply.
func (r *accessRecorder) capture(s *state.StateDB, final, deleteEmptyObjects bool) {
	changes, next := s.AccessChanges(r.journalOffset, final, deleteEmptyObjects)
	r.journalOffset = next
	if final {
		// Finalise clears the journal and ends the tx, which keeps its reads.
		r.journalOffset = 0
		r.commitReads()
	}
	for i := range changes {
		r.captureAccount(&changes[i])
	}
}

func (r *accessRecorder) captureAccount(c *state.AccessChange) {
	addr, idx := c.Address, r.index
	// prev is nil if the account was loaded before the recorder was enabled.
	// Then everything is recorded.
	prev := r.seen[addr]
	if c.Deleted {
		if prev == nil || prev.exists {
			r.builder.Delete(addr, idx)
		}
		r.seen[addr] = newAccessView(nil)
		return
	}

	acc := c.Account()
	cur := newAccessView(acc)
	// Only an account that existed has a storage to drop. One that comes into
	// existence after being touched and removed as empty earlier in the block
	// has none, and the list must not depend on that history: a phase executed
	// from the list alone does not see it.
	if c.Recreated && (prev == nil || prev.exists) {
		r.builder.ClearStorage(addr, idx)
	}
	if prev == nil || !prev.exists || c.Recreated || !bytes.Equal(prev.fields, cur.fields) {
		enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(acc))
		if err != nil {
			stateLogger.Error("Failed to encode account for the block access list", "addr", addr, "err", err)
		}
		r.builder.SetAccount(addr, idx, enc)
	}
	if prev == nil || prev.balance.Cmp(cur.balance) != 0 {
		r.builder.SetBalance(addr, idx, cur.balance)
	}
	if prev == nil || prev.nonce != cur.nonce {
		r.builder.SetNonce(addr, idx, cur.nonce)
	}
	if prev == nil || prev.codeHash != cur.codeHash {
		r.builder.SetCode(addr, idx, c.Code())
	}
	for slot, pre := range c.Slots {
		// After a recreation the storage starts empty, so every written slot
		// is recorded even if it went back to the value before the first write.
		if value := c.State(slot); value != pre || c.Recreated {
			r.builder.WriteSlot(addr, idx, slot, value)
		} else {
			r.builder.ReadSlot(addr, slot)
		}
	}
	r.seen[addr] = cur
}

// wholeAccountExtraSize bounds what a whole account adds to its Kaia fields: a
// balance, a nonce, a code hash and a storage root with their headers.
const wholeAccountExtraSize = 33 + 9 + 33 + 40 + 4*9

// Stats returns the entries of the block access list recorded so far on s, as
// BlockAccessList.EntryCount counts them, and an upper bound of its encoded
// size. It includes the accesses of the running tx that are not finalised yet,
// so a block producer can check a tx before it keeps it. The changes not
// recorded yet are bounded by a change of every field of each changed account
// and of each written slot. It returns zeros if s has no recorder.
func Stats(s *state.StateDB) (entries, size uint64) {
	r, ok := s.AccessRecorder().(*accessRecorder)
	if !ok {
		return 0, 0
	}
	entries, size = r.builder.Stats()
	newAccounts := make(map[common.Address]struct{})
	newSlots := make(map[common.Address]map[common.Hash]struct{})
	addAccount := func(addr common.Address) {
		if _, ok := newAccounts[addr]; ok {
			return
		}
		if has, _ := r.builder.Has(addr, nil); !has {
			newAccounts[addr] = struct{}{}
			entries++
			size += MaxAccountSize
		}
	}
	addSlot := func(addr common.Address, slot common.Hash) {
		addAccount(addr)
		if _, ok := newSlots[addr][slot]; ok {
			return
		}
		if _, has := r.builder.Has(addr, &slot); !has {
			if newSlots[addr] == nil {
				newSlots[addr] = make(map[common.Hash]struct{})
			}
			newSlots[addr][slot] = struct{}{}
			entries++
			size += MaxSlotSize
		}
	}
	for addr := range r.pendingAccounts {
		addAccount(addr)
	}
	for addr, slots := range r.pendingSlots {
		for slot := range slots {
			addSlot(addr, slot)
		}
	}
	changes, _ := s.AccessChanges(r.journalOffset, false, false)
	for i := range changes {
		c := &changes[i]
		addAccount(c.Address)
		for slot := range c.Slots {
			addSlot(c.Address, slot)
			size += MaxChangeSize
		}
		// A balance, a nonce and the whole account, which adds a balance, a
		// nonce, a code hash and a storage root to the Kaia fields.
		acc := c.Account()
		size += 2*MaxChangeSize + MaxBytesChangeSize(len(kaiaFields(acc))+wholeAccountExtraSize)
		if prev := r.seen[c.Address]; prev == nil || prev.codeHash != newAccessView(acc).codeHash {
			size += MaxBytesChangeSize(len(c.Code()))
		}
	}
	return entries, size
}
