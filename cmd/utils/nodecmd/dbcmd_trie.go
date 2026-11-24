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

package nodecmd

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
)

// trieBuildStatus reports a state trie build, see buildTrie.
type trieBuildStatus struct {
	Number   uint64
	Root     common.Hash
	From     uint64 // block whose stored trie the build starts from
	Stage    string // lists, values, apply, commit or done
	Blocks   uint64
	Accounts int
	Slots    int
	Done     int // accounts whose values at Number were read
}

type trieBuild struct {
	c      *balTools
	number uint64
	root   common.Hash

	mu     sync.Mutex
	status trieBuildStatus
}

// buildTrie builds the state trie of a past block of a history node, logging
// its progress: from the stored trie of a later block, setting back every
// account and slot that the lists of the blocks in between changed to its
// value at the block, read from the BAL history. The root is checked against
// the header before the trie is written, and the trie is kept from live
// pruning, as it shares the nodes of the later trie. A trie that is stored in
// full is only kept.
func (c *balTools) buildTrie(number uint64) (trieBuildStatus, error) {
	if mode := c.bc.States().HistoryMode(); mode != database.HistoryModeBAL {
		return trieBuildStatus{}, fmt.Errorf("state tries are built on history nodes only, this one is %s", mode)
	}
	if !c.bc.States().BALs().Indexes() {
		return trieBuildStatus{}, errors.New("the BAL index is off")
	}
	if c.db.GetDomainsManager() != nil {
		return trieBuildStatus{}, errors.New("state tries are not built over flat tries")
	}
	header := c.bc.GetHeaderByNumber(number)
	if header == nil {
		return trieBuildStatus{}, fmt.Errorf("no header of block %d", number)
	}
	b := &trieBuild{c: c, number: number, root: header.Root, status: trieBuildStatus{Number: number, Root: header.Root}}
	started := time.Now()
	logger.Info("Started building a state trie", "number", number, "root", header.Root)
	err := b.build()
	if err == nil {
		b.update(func(s *trieBuildStatus) { s.Stage = "done" })
		logger.Info("Built a state trie", "number", number, "root", b.root, "from", b.snapshot().From, "elapsed", time.Since(started))
	}
	return b.snapshot(), err
}

func (b *trieBuild) snapshot() trieBuildStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *trieBuild) update(f func(s *trieBuildStatus)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(&b.status)
}

func (b *trieBuild) stopped() error {
	select {
	case <-b.c.quit:
		return errors.New("stopped")
	default:
		return nil
	}
}

// onDisk reports whether the root node of a state trie is stored.
func (b *trieBuild) onDisk(root common.Hash) bool {
	ok, _ := b.c.db.HasTrieNode(root.ExtendZero())
	return ok
}

func (b *trieBuild) build() error {
	c, number, root := b.c, b.number, b.root
	if k, ok := c.states.KeptTrie(number); ok && k.Root == root && b.onDisk(root) {
		b.update(func(s *trieBuildStatus) { s.From = k.Height })
		return nil
	}
	_, next, ok := c.bc.States().BALs().Indexed()
	if !ok || number >= next {
		return fmt.Errorf("block %d is not covered by the BAL index", number)
	}
	if c.states.TrieDB().DoesExistCachedNode(root.ExtendZero()) {
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
	counter := state.ExtCounter(common.Hash{}.Extend())
	pruned, err := c.states.KeepTrie(state.KeptTrie{Number: number, Root: root, Height: height, Counter: counter})
	if err != nil {
		return err
	}
	b.update(func(s *trieBuildStatus) { s.From = height })
	if height <= pruned {
		err = fmt.Errorf("the stored state trie of block %d is pruned", height)
	} else if height > number {
		err = b.apply(from.Root, height)
	}
	if err != nil {
		c.states.DropKeptTrie(number)
	}
	return err
}

// apply sets back the trie of block height to the block and writes it.
func (b *trieBuild) apply(fromRoot common.Hash, height uint64) error {
	c, number := b.c, b.number
	stage := func(stage string) {
		b.update(func(s *trieBuildStatus) { s.Stage = stage })
		logger.Info("Building a state trie", "number", number, "from", height, "stage", stage)
	}
	db := state.NewDatabaseWithExistingCache(c.db, c.states.TrieDB().TrieNodeCache())
	built, err := c.bc.States().BuildTrieBack(db, fromRoot, height, number, &state.TrieProgress{
		Stopped: b.stopped,
		Stage:   stage,
		Lists: func(blocks uint64, accounts, slots int) {
			b.update(func(s *trieBuildStatus) { s.Blocks, s.Accounts, s.Slots = blocks, accounts, slots })
		},
		Values: func(done int) { b.update(func(s *trieBuildStatus) { s.Done = done }) },
	})
	if err != nil {
		return err
	}
	if built != b.root {
		return fmt.Errorf("built state root %x, block %d has %x", built, number, b.root)
	}
	if err := b.stopped(); err != nil {
		return err
	}
	stage("commit")
	return db.TrieDB().Commit(built, true, number)
}
