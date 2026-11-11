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

package impl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/kaiax/bal"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database"
)

// A history node keeps its state with live pruning: a block replacing a trie
// node marks it with the number of its parent block, and the node is deleted
// once the head is the retention period past the mark. Every node but the root
// of the account trie gets a fresh nonce from a counter that only grows, so a
// node is never written twice and the trie of a past block can be built back
// into the trie database next to the live one: it takes the stored trie of a
// later block and sets back what the lists in between changed. The built trie
// shares the nodes the lists left alone with the trie it was built from, and
// those are kept from pruning: a node marked at that block or later whose
// nonce is not above the counter when the build started was in its trie. The
// nodes without a nonce, of the genesis state, are kept too, but for the roots
// of later blocks.

var keptTriesKey = []byte("bal-kept-tries")

// TrieBuildStatus reports a state trie build, see StartTrieBuild.
type TrieBuildStatus struct {
	Running  bool        `json:"running"`
	Number   uint64      `json:"number"`
	Root     common.Hash `json:"root"`
	From     uint64      `json:"from"`  // block whose stored trie the build starts from
	Stage    string      `json:"stage"` // lists, values, apply, commit or done
	Blocks   uint64      `json:"blocks"`
	Accounts int         `json:"accounts"`
	Slots    int         `json:"slots"`
	Done     int         `json:"done"` // accounts whose values at Number were read
	Elapsed  string      `json:"elapsed"`
	Error    string      `json:"error,omitempty"`
}

// keptTrie is the state trie of block Number, which shares the nodes of the
// stored trie of block Height that have a nonce up to Counter.
type keptTrie struct {
	Number  uint64
	Root    common.Hash
	Height  uint64
	Counter uint64
}

// tries keeps built state tries from live pruning and runs builds.
type tries struct {
	mu     sync.Mutex // serialises pruning with keeping tries
	loaded bool
	kept   []keptTrie
	pruned uint64 // last block whose marked nodes were deleted

	buildMu sync.Mutex
	build   *trieBuild
}

func (t *tries) load(db database.DBManager) {
	if t.loaded {
		return
	}
	t.loaded = true
	t.pruned, _ = db.ReadLastPrunedBlockNumber()
	enc, err := db.GetMiscDB().Get(keptTriesKey)
	if err != nil || len(enc) == 0 {
		return
	}
	if err := rlp.DecodeBytes(enc, &t.kept); err != nil {
		logger.Error("Failed to read the kept state tries, live pruning may delete their nodes", "err", err)
	}
}

func (t *tries) store(db database.DBManager) error {
	enc, err := rlp.EncodeToBytes(t.kept)
	if err != nil {
		return err
	}
	return db.GetMiscDB().Put(keptTriesKey, enc)
}

// keep starts keeping k, replacing a kept trie of the same block, and returns
// the last block whose marked nodes were deleted.
func (t *tries) keep(db database.DBManager, k keptTrie) (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.load(db)
	kept := []keptTrie{k}
	for _, o := range t.kept {
		if o.Number != k.Number {
			kept = append(kept, o)
		}
	}
	t.kept = kept
	return t.pruned, t.store(db)
}

func (t *tries) drop(db database.DBManager, number uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.load(db)
	kept := t.kept[:0:0]
	for _, o := range t.kept {
		if o.Number != number {
			kept = append(kept, o)
		}
	}
	t.kept = kept
	if err := t.store(db); err != nil {
		logger.Error("Failed to store the kept state tries", "err", err)
	}
}

func (t *tries) get(db database.DBManager, number uint64) (keptTrie, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.load(db)
	for _, k := range t.kept {
		if k.Number == number {
			return k, true
		}
	}
	return keptTrie{}, false
}

func extCounter(h common.ExtHash) uint64 {
	var b [8]byte
	c := h.Counter()
	copy(b[1:], c[:])
	return binary.BigEndian.Uint64(b[:])
}

// PruneTrieNodes deletes the trie nodes of marks of blocks up to limit, but
// those of kept tries.
func (c *BALModule) PruneTrieNodes(marks []database.PruningMark, limit uint64) {
	t := &c.tries
	t.mu.Lock()
	defer t.mu.Unlock()
	t.load(c.db)
	if len(t.kept) > 0 {
		marks = t.unkept(c, marks)
	}
	c.db.PruneTrieNodes(marks)
	t.pruned = limit
}

// unkept returns the marks whose nodes no kept trie holds.
func (t *tries) unkept(c *BALModule, marks []database.PruningMark) []database.PruningMark {
	roots := make(map[common.Hash]bool, len(t.kept))
	for _, k := range t.kept {
		roots[k.Root] = true
	}
	var (
		out        = make([]database.PruningMark, 0, len(marks))
		rootNumber = ^uint64(0)
		blockRoot  common.Hash
		keptNodes  int
	)
	for _, m := range marks {
		if m.Hash.IsZeroExtended() && roots[m.Hash.Unextend()] {
			keptNodes++
			continue
		}
		var (
			counter uint64
			shared  bool
		)
		for _, k := range t.kept {
			if m.Number >= k.Height {
				shared, counter = true, max(counter, k.Counter)
			}
		}
		keep := false
		switch {
		case !shared:
		case m.Hash.IsZeroExtended():
			if rootNumber != m.Number {
				rootNumber, blockRoot = m.Number, common.Hash{}
				if h := c.bc.GetHeaderByNumber(m.Number); h != nil {
					blockRoot = h.Root
				}
			}
			keep = m.Hash.Unextend() != blockRoot
		default:
			keep = extCounter(m.Hash) <= counter
		}
		if keep {
			keptNodes++
		} else {
			out = append(out, m)
		}
	}
	if keptNodes > 0 {
		logger.Debug("Kept trie nodes of built state tries from pruning", "nodes", keptNodes)
	}
	return out
}

type trieBuild struct {
	c      *BALModule
	number uint64
	root   common.Hash
	quit   chan struct{}
	done   chan struct{}

	mu      sync.Mutex
	status  TrieBuildStatus
	started time.Time
}

// StartTrieBuild builds the state trie of a past block of a history node
// in the background, see TrieBuildStatus: from the stored trie of a later
// block, setting back every account and slot that the lists of the blocks in
// between changed to its value at the block, read from the BAL history. The
// root is checked against the header before the trie is written, and the trie
// is kept from live pruning. A trie that is stored in full is only kept.
func StartTrieBuild(bc *blockchain.BlockChain, number uint64) error {
	return of(bc).startTrieBuild(number)
}

func (c *BALModule) startTrieBuild(number uint64) error {
	if mode := ReadHistoryMode(c.db); mode != HistoryModeHistory {
		return fmt.Errorf("state tries are built on history nodes only, this one is %s", mode)
	}
	if c.archive == nil {
		return errors.New("the BAL index is off")
	}
	if c.db.GetDomainsManager() != nil {
		return errors.New("state tries are not built over flat tries")
	}
	header := c.bc.GetHeaderByNumber(number)
	if header == nil {
		return fmt.Errorf("no header of block %d", number)
	}
	c.tries.buildMu.Lock()
	defer c.tries.buildMu.Unlock()
	if b := c.tries.build; b != nil {
		select {
		case <-b.done:
		default:
			return fmt.Errorf("the state trie of block %d is being built", b.number)
		}
	}
	b := &trieBuild{
		c: c, number: number, root: header.Root,
		quit: make(chan struct{}), done: make(chan struct{}),
		started: time.Now(),
		status:  TrieBuildStatus{Running: true, Number: number, Root: header.Root},
	}
	c.tries.build = b
	logger.Info("Started building a state trie", "number", number, "root", header.Root)
	c.wg.Go(b.run)
	return nil
}

// StopTrieBuild stops the running state trie build, which writes nothing.
func StopTrieBuild(bc *blockchain.BlockChain) error { return of(bc).stopTrieBuild() }

func (c *BALModule) stopTrieBuild() error {
	c.tries.buildMu.Lock()
	b := c.tries.build
	c.tries.buildMu.Unlock()
	if b == nil {
		return errors.New("no state trie build")
	}
	select {
	case <-b.quit:
	default:
		close(b.quit)
	}
	<-b.done
	return nil
}

// GetTrieBuildStatus returns the status of the last state trie build.
func GetTrieBuildStatus(bc *blockchain.BlockChain) TrieBuildStatus { return of(bc).trieBuildStatus() }

func (c *BALModule) trieBuildStatus() TrieBuildStatus {
	c.tries.buildMu.Lock()
	b := c.tries.build
	c.tries.buildMu.Unlock()
	if b == nil {
		return TrieBuildStatus{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.status
	s.Elapsed = time.Since(b.started).Round(time.Millisecond).String()
	return s
}

func (b *trieBuild) update(f func(s *TrieBuildStatus)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(&b.status)
}

func (b *trieBuild) stopped() error {
	select {
	case <-b.quit:
		return errors.New("stopped")
	case <-b.c.quit:
		return errors.New("the chain stopped")
	default:
		return nil
	}
}

func (b *trieBuild) run() {
	defer close(b.done)
	err := b.buildTrie()
	b.update(func(s *TrieBuildStatus) {
		s.Running = false
		if err != nil {
			s.Error = err.Error()
		} else {
			s.Stage = "done"
		}
	})
	if err != nil {
		logger.Warn("Failed to build a state trie", "number", b.number, "err", err)
	} else {
		logger.Info("Built a state trie", "number", b.number, "root", b.root, "elapsed", time.Since(b.started))
	}
}

// onDisk reports whether the root node of a state trie is stored.
func (b *trieBuild) onDisk(root common.Hash) bool {
	ok, _ := b.c.db.HasTrieNode(root.ExtendZero())
	return ok
}

func (b *trieBuild) buildTrie() error {
	c, number, root := b.c, b.number, b.root
	if k, ok := c.tries.get(c.db, number); ok && k.Root == root && b.onDisk(root) {
		b.update(func(s *TrieBuildStatus) { s.From = k.Height })
		return nil
	}
	_, next, ok := c.archive.Covered()
	if !ok || number >= next {
		return fmt.Errorf("block %d is not covered by the BAL index", number)
	}
	if c.bc.StateCache().TrieDB().DoesExistCachedNode(root.ExtendZero()) {
		return fmt.Errorf("the state trie of block %d is still in memory, build it once the node dropped it", number)
	}

	// The last trie stored in full at or before the head and the index, which
	// is the trie of the block itself if that was the last one stored.
	top := min(c.bc.CurrentBlock().NumberU64(), next-1)
	var from *types.Header
	for n := top; n >= number; n-- {
		if err := b.stopped(); err != nil {
			return err
		}
		if h := c.bc.GetHeaderByNumber(n); h != nil && b.onDisk(h.Root) {
			from = h
			break
		}
		if n == 0 {
			break
		}
	}
	if from == nil {
		return fmt.Errorf("no stored state trie from block %d to block %d", number, top)
	}
	height := from.Number.Uint64()
	counter := extCounter(common.Hash{}.Extend())
	pruned, err := c.tries.keep(c.db, keptTrie{Number: number, Root: root, Height: height, Counter: counter})
	if err != nil {
		return err
	}
	b.update(func(s *TrieBuildStatus) { s.From = height })
	if height <= pruned {
		err = fmt.Errorf("the stored state trie of block %d is pruned", height)
	} else if height > number {
		err = b.apply(from.Root, height)
	}
	if err != nil {
		c.tries.drop(c.db, number)
	}
	return err
}

// trieChange is what the lists after the block changed in an account.
type trieChange struct {
	reset bool // deleted or recreated
	slots map[common.Hash]struct{}
}

// apply sets back the trie of block height to the block and writes it.
func (b *trieBuild) apply(fromRoot common.Hash, height uint64) error {
	c, number := b.c, b.number
	b.update(func(s *TrieBuildStatus) { s.Stage = "lists" })

	changes := make(map[common.Address]*trieChange)
	slots := 0
	for n := number + 1; n <= height; n++ {
		if err := b.stopped(); err != nil {
			return err
		}
		list, err := Read(c.db, n)
		if err != nil {
			return fmt.Errorf("block %d: %w", n, err)
		}
		for i := range list.Accounts {
			a := &list.Accounts[i]
			c := changes[a.Address]
			if c == nil {
				c = &trieChange{slots: make(map[common.Hash]struct{})}
				changes[a.Address] = c
			}
			c.reset = c.reset || len(a.Deleted) > 0 || len(a.StorageCleared) > 0
			for _, w := range a.StorageWrites {
				if _, ok := c.slots[w.Slot]; !ok {
					c.slots[w.Slot] = struct{}{}
					slots++
				}
			}
		}
		b.update(func(s *TrieBuildStatus) { s.Blocks, s.Accounts, s.Slots = n-number, len(changes), slots })
	}

	history, valid, err := c.archive.History(1024)
	if err != nil {
		return err
	}
	if meta := history.Index().Meta(); number < meta.Base || height >= meta.Next {
		return fmt.Errorf("blocks %d-%d are not covered by the BAL index", number, height)
	}
	db := state.NewDatabaseWithExistingCache(c.db, c.bc.StateCache().TrieDB().TrieNodeCache())
	sdb, err := state.New(fromRoot, db, nil, nil)
	if err != nil {
		return err
	}
	addrs := make([]common.Address, 0, len(changes))
	for addr := range changes {
		addrs = append(addrs, addr)
	}
	sort.Slice(addrs, func(i, j int) bool { return bytes.Compare(addrs[i][:], addrs[j][:]) < 0 })

	b.update(func(s *TrieBuildStatus) { s.Stage = "values" })
	back := &bal.BlockAccessList{Accounts: make([]bal.AccountAccess, 0, len(addrs))}
	for i, addr := range addrs {
		if err := b.stopped(); err != nil {
			return err
		}
		a, err := backAccess(history, sdb, addr, number, changes[addr])
		if err != nil {
			return fmt.Errorf("account %s: %w", addr.Hex(), err)
		}
		back.Accounts = append(back.Accounts, *a)
		if i%1024 == 0 {
			b.update(func(s *TrieBuildStatus) { s.Done = i })
		}
	}
	if !valid() {
		return errors.New("the BAL index was rebuilt")
	}
	b.update(func(s *TrieBuildStatus) { s.Done, s.Stage = len(addrs), "apply" })
	if err := bal.Apply(sdb, back); err != nil {
		return err
	}
	built, err := sdb.Commit(true)
	if err != nil {
		return err
	}
	if built != b.root {
		return fmt.Errorf("built state root %x, block %d has %x", built, number, b.root)
	}
	if err := b.stopped(); err != nil {
		return err
	}
	b.update(func(s *TrieBuildStatus) { s.Stage = "commit" })
	return db.TrieDB().Commit(built, true, number)
}

// backAccess returns the changes that set addr in the state of a later block
// back to its value at the end of block number.
func backAccess(h *History, later *state.StateDB, addr common.Address, number uint64, c *trieChange) (*bal.AccountAccess, error) {
	acc, err := h.Account(addr, number)
	if err != nil {
		return nil, err
	}
	a := &bal.AccountAccess{Address: addr}
	if acc == nil {
		a.Deleted = []uint32{0}
		return a, nil
	}
	enc, err := rlp.EncodeToBytes(account.NewAccountSerializerWithAccount(acc))
	if err != nil {
		return nil, err
	}
	a.AccountChanges = []bal.AccountChange{{Index: 0, Account: enc}}
	pa := account.GetProgramAccount(acc)
	laterProgram := later.IsProgramAccount(addr)
	if pa == nil {
		if laterProgram {
			a.StorageCleared = []uint32{0}
		}
		return a, nil
	}
	if !laterProgram || !bytes.Equal(later.GetCodeHash(addr).Bytes(), pa.GetCodeHash()) {
		code, err := h.Code(addr, number)
		if err != nil {
			return nil, err
		}
		a.CodeChanges = []bal.CodeChange{{Index: 0, Code: code}}
	}
	var slots []common.Hash
	if !laterProgram || c.reset {
		a.StorageCleared = []uint32{0}
		if slots, err = h.Slots(addr, number); err != nil {
			return nil, err
		}
	} else {
		for slot := range c.slots {
			slots = append(slots, slot)
		}
	}
	sort.Slice(slots, func(i, j int) bool { return bytes.Compare(slots[i][:], slots[j][:]) < 0 })
	for i, slot := range slots {
		if i > 0 && slots[i-1] == slot {
			continue
		}
		value, err := h.Storage(addr, slot, number)
		if err != nil {
			return nil, err
		}
		if value == (common.Hash{}) && len(a.StorageCleared) > 0 {
			continue
		}
		a.StorageWrites = append(a.StorageWrites, bal.SlotWrites{Slot: slot, Changes: []bal.SlotChange{{Index: 0, Value: value}}})
	}
	return a, nil
}
