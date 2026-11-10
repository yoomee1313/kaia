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

package bal

import (
	"errors"
	"fmt"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/types"
)

var errNoStateAnchor = errors.New("no stored state in the range")

// HasStateAt reports whether the state of block number is stored: the root
// node of its trie is in the database. State migration and live pruning drop
// the oldest states first, so a stored root means a whole trie.
func HasStateAt(bc *blockchain.BlockChain, number uint64) bool { return of(bc).hasStateAt(number) }

func (c *chain) hasStateAt(number uint64) bool {
	header := c.bc.GetHeaderByNumber(number)
	if header == nil {
		return false
	}
	if header.Root == types.EmptyRootHash {
		return true
	}
	node, err := c.bc.StateCache().TrieDB().Node(header.Root.ExtendZero())
	return err == nil && len(node) > 0
}

// StateAnchors describes where the stored states of a range are: the first
// one, found by binary search, and a sample of the later candidates, which are
// the multiples of Step after it.
type StateAnchors struct {
	First   uint64   `json:"first"`
	Step    uint64   `json:"step"`
	Sampled int      `json:"sampled"`
	Missing []uint64 `json:"missing"` // sampled candidates without a state
}

// FindStateAnchors finds the first block in [from, to] with a stored state,
// looking at the multiples of step, and checks samples of the later multiples.
// Blocks with a stored state are anchors: blocks after them can be executed
// again from there. Missing later anchors are allowed; an execution then
// starts from an earlier one.
func FindStateAnchors(bc *blockchain.BlockChain, from, to, step uint64, samples int) (*StateAnchors, error) {
	return of(bc).findStateAnchors(from, to, step, samples)
}

func (c *chain) findStateAnchors(from, to, step uint64, samples int) (*StateAnchors, error) {
	if step == 0 {
		return nil, errors.New("zero step")
	}
	lo := (from + step - 1) / step * step
	hi := to / step * step
	if lo > hi || !c.hasStateAt(hi) {
		return nil, fmt.Errorf("%w: [%d, %d] at multiples of %d", errNoStateAnchor, from, to, step)
	}
	// States are dropped from the oldest, so the multiples with a state are a
	// suffix of the range.
	for lo < hi {
		mid := lo + (hi-lo)/step/2*step
		if c.hasStateAt(mid) {
			hi = mid
		} else {
			lo = mid + step
		}
	}
	anchors := &StateAnchors{First: lo, Step: step}
	if count := (to/step*step-lo)/step + 1; samples > 0 && count > 1 {
		every := max(1, count/uint64(samples))
		for k := every; k < count; k += every {
			n := lo + k*step
			anchors.Sampled++
			if !c.hasStateAt(n) {
				anchors.Missing = append(anchors.Missing, n)
			}
		}
	}
	return anchors, nil
}

// stateAnchorAtOrBefore returns the last block with a stored state at or before
// number, among the multiples of step from first.
func (c *chain) stateAnchorAtOrBefore(number, first, step uint64) (uint64, error) {
	for n := (number-first)/step*step + first; ; n -= step {
		if c.hasStateAt(n) {
			return n, nil
		}
		if n < first+step {
			return 0, fmt.Errorf("%w: at or before block %d", errNoStateAnchor, number)
		}
	}
}
