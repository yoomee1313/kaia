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

// Package checkpoint writes the state trie of a block into incremental
// checkpoint segments and rebuilds the state of a later block from a
// checkpoint and the block access lists.
//
// Nodes are keyed by their path, not their hash: 0x00 || nibbles for the
// account trie, 0x01 || account hash || nibbles for a storage trie, one byte
// per nibble. Contract code is keyed by 0x02 || code hash, once per chain. A segment holds the nodes that differ from its base segment at
// the same path, and an empty value (a tombstone) for every path whose node is
// gone. So a lookup through the chain of a segment returns exactly the node
// that the trie of its block has at that path.
package checkpoint

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/types/account"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/database/checkpoint"
	"github.com/kaiachain/kaia/storage/statedb"
)

const (
	prefixAccount byte = 0x00
	prefixStorage byte = 0x01
	prefixCode    byte = 0x02
)

var (
	errMissingNode = errors.New("checkpoint chain misses a node")
	ErrStopped     = errors.New("checkpoint export stopped")
)

// NodeReader returns a stored trie node by its hash, see statedb.Database.Node.
type NodeReader func(hash common.ExtHash) ([]byte, error)

// CodeReader returns contract code by its hash, see state.Database.ContractCode.
type CodeReader func(hash common.Hash) ([]byte, error)

func codeKey(hash common.Hash) []byte {
	return append([]byte{prefixCode}, hash[:]...)
}

func accountPathKey(path []byte) []byte {
	return append([]byte{prefixAccount}, path...)
}

func storagePathKey(owner common.Hash, path []byte) []byte {
	key := make([]byte, 0, 1+common.HashLength+len(path))
	return append(append(append(key, prefixStorage), owner[:]...), path...)
}

// ref is a stored node at a path. hash is nil for the root of the old trie
// when there is none.
type ref struct {
	path []byte
	hash common.ExtHash
}

var emptyCodeHash = crypto.Keccak256Hash(nil)

func canonical(h common.ExtHash) common.Hash { return common.BytesToHash(h[:common.HashLength]) }

// leaf is an account found while walking the account trie.
type leaf struct {
	storageRoot common.ExtHash
	codeHash    common.Hash // zero if the account has no code
}

// differ walks a new trie against the old trie of the base segment and emits
// what the segment must hold.
type differ struct {
	nodes  NodeReader
	code   CodeReader
	base   *checkpoint.Store
	baseID uint64 // 0 if there is no base
	w      *checkpoint.Writer
	quit   <-chan struct{}

	newLeaves, oldLeaves map[common.Hash]leaf // collected in the account trie
	leafErr              error
	stats                ExportStats
}

// ExportStats counts what a segment holds.
type ExportStats struct {
	Nodes, Tombstones, Codes, Bytes uint64
}

func (d *differ) newNode(r ref) ([]byte, error) {
	blob, err := d.nodes(r.hash)
	if err != nil {
		return nil, fmt.Errorf("node %x at path %x: %w", r.hash, r.path, err)
	}
	return blob, nil
}

func (d *differ) oldNode(key []byte) ([]byte, error) {
	blob, err := d.base.Get(d.baseID, key)
	if err != nil || len(blob) == 0 {
		return nil, fmt.Errorf("%w: key %x: %v", errMissingNode, key, err)
	}
	return blob, nil
}

func children(parent []byte, blob []byte) ([]ref, []byte, []byte, error) {
	cs, key, value, err := statedb.DecodeNodeChildren(blob)
	if err != nil {
		return nil, nil, nil, err
	}
	refs := make([]ref, len(cs))
	for i, c := range cs {
		refs[i] = ref{path: append(common.CopyBytes(parent), c.Path...), hash: c.Hash}
	}
	return refs, key, value, nil
}

func hasPrefix(path, prefix []byte) bool { return bytes.HasPrefix(path, prefix) }

// take returns the leading refs whose path starts with prefix.
func take(refs []ref, prefix []byte) (taken, rest []ref) {
	i := 0
	for i < len(refs) && hasPrefix(refs[i].path, prefix) {
		i++
	}
	return refs[:i], refs[i:]
}

// merge walks two frontiers of nodes under the same prefix, both sorted by
// path. A node is emitted if the old trie has no node with the same hash at
// its path, and an old node gets a tombstone if the new trie has none at its
// path. Equal nodes at the same path have equal subtrees, which are skipped.
// Keys come out in order: a node is written before its subtree, and the next
// sibling sorts after the whole subtree.
func (d *differ) merge(news, olds []ref, key func([]byte) []byte, onLeaf func(path, key, value []byte, isNew bool)) error {
	for len(news) > 0 || len(olds) > 0 {
		switch {
		case len(olds) == 0 || (len(news) > 0 && bytes.Compare(news[0].path, olds[0].path) < 0):
			n := news[0]
			var under []ref
			under, olds = take(olds, n.path)
			if err := d.emit(n, under, key, onLeaf); err != nil {
				return err
			}
			news = news[1:]
		case len(news) == 0 || bytes.Compare(olds[0].path, news[0].path) < 0:
			o := olds[0]
			var under []ref
			under, news = take(news, o.path)
			if err := d.drop(o, under, key, onLeaf); err != nil {
				return err
			}
			olds = olds[1:]
		default: // same path
			n, o := news[0], olds[0]
			news, olds = news[1:], olds[1:]
			if canonical(n.hash) == canonical(o.hash) {
				continue
			}
			if err := d.emit(n, []ref{o}, key, onLeaf); err != nil {
				return err
			}
		}
	}
	return nil
}

// emit writes the new node n and merges its children with the old nodes under
// its path. If the first of them is at the path of n, its children are used.
func (d *differ) emit(n ref, olds []ref, key func([]byte) []byte, onLeaf func(path, key, value []byte, isNew bool)) error {
	if d.stats.Nodes%4096 == 0 {
		select {
		case <-d.quit:
			return ErrStopped
		default:
		}
	}
	blob, err := d.newNode(n)
	if err != nil {
		return err
	}
	if err := d.w.Put(key(n.path), blob); err != nil {
		return err
	}
	d.stats.Nodes++
	d.stats.Bytes += uint64(len(blob))
	news, lkey, value, err := children(n.path, blob)
	if err != nil {
		return err
	}
	if value != nil {
		onLeaf(n.path, lkey, value, true)
	}
	if len(olds) > 0 && bytes.Equal(olds[0].path, n.path) {
		oldBlob, err := d.oldNode(key(n.path))
		if err != nil {
			return err
		}
		oldChildren, okey, ovalue, err := children(n.path, oldBlob)
		if err != nil {
			return err
		}
		if ovalue != nil {
			onLeaf(n.path, okey, ovalue, false)
		}
		olds = append(oldChildren, olds[1:]...)
	}
	return d.merge(news, olds, key, onLeaf)
}

// drop writes a tombstone for the old node o and merges its children with the
// new nodes under its path.
func (d *differ) drop(o ref, news []ref, key func([]byte) []byte, onLeaf func(path, key, value []byte, isNew bool)) error {
	if err := d.w.Put(key(o.path), nil); err != nil {
		return err
	}
	d.stats.Tombstones++
	oldBlob, err := d.oldNode(key(o.path))
	if err != nil {
		return err
	}
	olds, okey, ovalue, err := children(o.path, oldBlob)
	if err != nil {
		return err
	}
	if ovalue != nil {
		onLeaf(o.path, okey, ovalue, false)
	}
	return d.merge(news, olds, key, onLeaf)
}

func rootRefs(root common.Hash) []ref {
	if root == (common.Hash{}) || root == types.EmptyRootHash {
		return nil
	}
	return []ref{{path: []byte{}, hash: root.ExtendZero()}}
}

// decodeLeaf returns the storage root and code hash of an encoded account.
func decodeLeaf(value []byte) (leaf, error) {
	ser := account.NewAccountSerializer()
	if err := rlp.DecodeBytes(value, ser); err != nil {
		return leaf{}, err
	}
	l := leaf{storageRoot: types.EmptyRootHash.ExtendZero()}
	if pa := account.GetProgramAccount(ser.GetAccount()); pa != nil {
		l.storageRoot = pa.GetStorageRoot()
		if h := common.BytesToHash(pa.GetCodeHash()); h != emptyCodeHash {
			l.codeHash = h
		}
	}
	return l, nil
}

func storageRefs(root common.ExtHash) []ref {
	if canonical(root) == types.EmptyRootHash || canonical(root) == (common.Hash{}) {
		return nil
	}
	return []ref{{path: []byte{}, hash: root}}
}

// Export writes the state trie of meta.StateRoot into a new segment of store,
// as the difference from the segment meta.Base (0 for a full segment). Nodes
// of the new trie are read through nodes and code. Closing quit stops it and
// discards the segment.
func Export(store *checkpoint.Store, meta checkpoint.Meta, nodes NodeReader, code CodeReader, quit <-chan struct{}) (checkpoint.Meta, ExportStats, error) {
	var oldRoot common.Hash
	if meta.Base != 0 {
		base, ok := store.Meta(meta.Base)
		if !ok {
			return checkpoint.Meta{}, ExportStats{}, fmt.Errorf("%w: %d", checkpoint.ErrUnknownSegment, meta.Base)
		}
		oldRoot = base.StateRoot
	}
	w, err := store.NewWriter(meta)
	if err != nil {
		return checkpoint.Meta{}, ExportStats{}, err
	}
	d := &differ{
		nodes: nodes, code: code, base: store, baseID: meta.Base, w: w, quit: quit,
		newLeaves: make(map[common.Hash]leaf), oldLeaves: make(map[common.Hash]leaf),
	}
	stats, err := d.run(meta.StateRoot, oldRoot)
	if err != nil {
		w.Abort()
		return checkpoint.Meta{}, stats, err
	}
	out, err := w.Finish()
	return out, stats, err
}

func (d *differ) run(newRoot, oldRoot common.Hash) (ExportStats, error) {
	news, olds := rootRefs(newRoot), rootRefs(oldRoot)
	if len(news) > 0 && len(olds) > 0 && newRoot == oldRoot {
		return d.stats, nil
	}
	onAccount := func(path, key, value []byte, isNew bool) {
		owner := common.BytesToHash(statedb.HexToKeybytes(append(common.CopyBytes(path), key...)))
		l, err := decodeLeaf(value)
		if err != nil && d.leafErr == nil {
			d.leafErr = fmt.Errorf("account %x: %w", owner, err)
		}
		if isNew {
			d.newLeaves[owner] = l
		} else {
			d.oldLeaves[owner] = l
		}
	}
	if err := d.merge(news, olds, accountPathKey, onAccount); err != nil {
		return d.stats, err
	}
	if d.leafErr != nil {
		return d.stats, d.leafErr
	}

	// Storage tries of the accounts whose leaf changed, in account hash order.
	owners := make([]common.Hash, 0, len(d.newLeaves)+len(d.oldLeaves))
	for owner := range d.newLeaves {
		owners = append(owners, owner)
	}
	for owner := range d.oldLeaves {
		if _, ok := d.newLeaves[owner]; !ok {
			owners = append(owners, owner)
		}
	}
	sort.Slice(owners, func(i, j int) bool { return bytes.Compare(owners[i][:], owners[j][:]) < 0 })
	noLeaf := func(path, key, value []byte, isNew bool) {}
	for _, owner := range owners {
		var news, olds []ref
		if l, ok := d.newLeaves[owner]; ok {
			news = storageRefs(l.storageRoot)
		}
		if l, ok := d.oldLeaves[owner]; ok {
			olds = storageRefs(l.storageRoot)
		}
		key := func(path []byte) []byte { return storagePathKey(owner, path) }
		if err := d.merge(news, olds, key, noLeaf); err != nil {
			return d.stats, fmt.Errorf("storage of %x: %w", owner, err)
		}
	}

	// Code of the changed accounts that the chain does not hold yet.
	codes := make(map[common.Hash]bool)
	for _, l := range d.newLeaves {
		if l.codeHash != (common.Hash{}) {
			codes[l.codeHash] = true
		}
	}
	hashes := make([]common.Hash, 0, len(codes))
	for h := range codes {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })
	for _, h := range hashes {
		if d.baseID != 0 {
			if _, err := d.base.Get(d.baseID, codeKey(h)); err == nil {
				continue
			} else if !errors.Is(err, checkpoint.ErrNotFound) {
				return d.stats, err
			}
		}
		code, err := d.code(h)
		if err != nil {
			return d.stats, fmt.Errorf("code %x: %w", h, err)
		}
		if err := d.w.Put(codeKey(h), code); err != nil {
			return d.stats, err
		}
		d.stats.Codes++
		d.stats.Bytes += uint64(len(code))
	}
	return d.stats, nil
}
