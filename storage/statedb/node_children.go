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

package statedb

import (
	"github.com/kaiachain/kaia/common"
)

// NodeChild is a child of a trie node that is stored on its own, by hash.
type NodeChild struct {
	Path []byte // nibbles from the parent node to the child
	Hash common.ExtHash
}

// DecodeNodeChildren parses an encoded trie node as stored in the database and
// returns its children stored by hash, in path order. Embedded children are
// part of the node and are not returned. For a leaf, key holds the nibbles of
// the rest of the key and value the leaf value.
func DecodeNodeChildren(blob []byte) (children []NodeChild, key, value []byte, err error) {
	n, err := decodeNode(nil, blob)
	if err != nil {
		return nil, nil, nil, err
	}
	switch n := n.(type) {
	case *shortNode:
		if v, ok := n.Val.(valueNode); ok {
			return nil, n.Key[:len(n.Key)-1], v, nil
		}
		if h, ok := n.Val.(hashNode); ok {
			children = append(children, NodeChild{Path: n.Key, Hash: common.BytesToExtHash(h)})
		}
	case *fullNode:
		for i, child := range n.Children[:16] {
			if h, ok := child.(hashNode); ok {
				children = append(children, NodeChild{Path: []byte{byte(i)}, Hash: common.BytesToExtHash(h)})
			}
		}
		if v, ok := n.Children[16].(valueNode); ok {
			value = v
		}
	}
	return children, nil, value, nil
}

// KeybytesToHex returns the nibbles of a key, without the terminator.
func KeybytesToHex(key []byte) []byte {
	hex := keybytesToHex(key)
	return hex[:len(hex)-1]
}

// HexToKeybytes returns the key of an even number of nibbles.
func HexToKeybytes(hex []byte) []byte {
	return hexToKeybytes(hex)
}
