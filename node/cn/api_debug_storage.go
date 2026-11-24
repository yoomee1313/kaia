// Modifications Copyright 2024 The Kaia Authors
// Modifications Copyright 2018 The klaytn Authors
// Copyright 2015 The go-ethereum Authors
// This file is part of go-ethereum.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
//
// This file is derived from eth/api.go (2018/06/04).
// Modified and improved for the klaytn development.
// Modified and improved for the Kaia development.

package cn

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/hexutil"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/rlp"
	"github.com/kaiachain/kaia/storage/statedb"
)

// TODO-Kaia: Rearrange DebugCNAPI and DebugStorageCNAPI receivers
// StartWarmUp retrieves all state/storage tries of the latest committed state root and caches the tries.
func (api *DebugStorageCNAPI) StartWarmUp(minLoad uint) error {
	return api.cn.blockchain.States().StartWarmUp(minLoad)
}

// TODO-Kaia: Rearrange DebugCNAPI and DebugStorageCNAPI receivers
// StartContractWarmUp retrieves a storage trie of the latest state root and caches the trie
// corresponding to the given contract address.
func (api *DebugStorageCNAPI) StartContractWarmUp(contractAddr common.Address, minLoad uint) error {
	return api.cn.blockchain.States().StartContractWarmUp(contractAddr, minLoad)
}

// TODO-Kaia: Rearrange DebugCNAPI and DebugStorageCNAPI receivers
// StopWarmUp stops the warming up process.
func (api *DebugStorageCNAPI) StopWarmUp() error {
	return api.cn.blockchain.States().StopWarmUp()
}

// TODO-Kaia: Rearrange DebugCNAPI and DebugStorageCNAPI receivers
// StartCollectingTrieStats  collects state/storage trie statistics and print in the log.
func (api *DebugStorageCNAPI) StartCollectingTrieStats(contractAddr common.Address) error {
	return api.cn.blockchain.States().StartCollectingTrieStats(contractAddr)
}

// DebugStorageCNAPI is the collection of CN full node APIs exposed over
// the private debugging endpoint.
type DebugStorageCNAPI struct {
	config *params.ChainConfig
	cn     *CN
}

// NewDebugStorageCNAPI creates a new API definition for the full node-related
// private debug methods of the CN service.
func NewDebugStorageCNAPI(config *params.ChainConfig, cn *CN) *DebugStorageCNAPI {
	return &DebugStorageCNAPI{config: config, cn: cn}
}

// Preimage is a debug API function that returns the preimage for a sha3 hash, if known.
func (api *DebugStorageCNAPI) Preimage(ctx context.Context, hash common.Hash) (hexutil.Bytes, error) {
	if preimage := api.cn.ChainDB().ReadPreimage(hash); preimage != nil {
		return preimage, nil
	}
	return nil, errors.New("unknown preimage")
}

// StorageRangeResult is the result of a debug_storageRangeAt API call.
type StorageRangeResult struct {
	Storage storageMap   `json:"storage"`
	NextKey *common.Hash `json:"nextKey"` // nil if Storage includes the last key in the statedb.
}

type storageMap map[common.Hash]storageEntry

type storageEntry struct {
	Key   *common.Hash `json:"key"`
	Value common.Hash  `json:"value"`
}

// StorageRangeAt returns the storage at the given block height and transaction index.
func (api *DebugStorageCNAPI) StorageRangeAt(ctx context.Context, blockHash common.Hash, txIndex int, contractAddress common.Address, keyStart hexutil.Bytes, maxResult int) (StorageRangeResult, error) {
	// Retrieve the block
	block := api.cn.blockchain.GetBlockByHash(blockHash)
	if block == nil {
		return StorageRangeResult{}, fmt.Errorf("block %#x not found", blockHash)
	}
	_, _, _, statedb, release, err := api.cn.stateAtTransaction(block, txIndex, 0, nil, true, false)
	if err != nil {
		return StorageRangeResult{}, err
	}
	defer release()

	if statedb.ReadOnly() {
		return readOnlyStorageRangeAt(statedb, contractAddress, keyStart, maxResult)
	}
	st := statedb.StorageTrie(contractAddress)
	if st == nil {
		return StorageRangeResult{}, fmt.Errorf("account %x doesn't exist", contractAddress)
	}
	return storageRangeAt(st, keyStart, maxResult)
}

// readOnlyStorageRangeAt is storageRangeAt for a state without a state trie,
// like one read from the BAL history: its slots are listed in the order of
// their hashes, each with its key.
func readOnlyStorageRangeAt(statedb *state.StateDB, addr common.Address, start []byte, maxResult int) (StorageRangeResult, error) {
	if !statedb.Exist(addr) {
		return StorageRangeResult{}, fmt.Errorf("account %x doesn't exist", addr)
	}
	var from common.Hash // a key prefix, like the start of a trie iterator
	copy(from[:], start)
	result := StorageRangeResult{Storage: storageMap{}}
	statedb.ForEachStorage(addr, func(key, value common.Hash) bool {
		hash := crypto.Keccak256Hash(key[:])
		if bytes.Compare(hash[:], from[:]) < 0 {
			return true
		}
		if len(result.Storage) == maxResult {
			result.NextKey = &hash
			return false
		}
		result.Storage[hash] = storageEntry{Key: &key, Value: value}
		return true
	})
	if err := statedb.Error(); err != nil {
		return StorageRangeResult{}, err
	}
	return result, nil
}

func storageRangeAt(st state.Trie, start []byte, maxResult int) (StorageRangeResult, error) {
	it := statedb.NewIterator(st.NodeIterator(start))
	result := StorageRangeResult{Storage: storageMap{}}
	for i := 0; i < maxResult && it.Next(); i++ {
		_, content, _, err := rlp.Split(it.Value)
		if err != nil {
			return StorageRangeResult{}, err
		}
		e := storageEntry{Value: common.BytesToHash(content)}
		if preimage := st.GetKey(it.Key); preimage != nil {
			preimage := common.BytesToHash(preimage)
			e.Key = &preimage
		}
		result.Storage[common.BytesToHash(it.Key)] = e
	}
	// Add the 'next key' so clients can continue downloading.
	if it.Next() {
		next := common.BytesToHash(it.Key)
		result.NextKey = &next
	}
	if it.Err != nil {
		return StorageRangeResult{}, it.Err
	}
	return result, nil
}
