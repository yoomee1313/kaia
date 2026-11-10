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

package database

import (
	"errors"
	"fmt"

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
)

// Receipts are stored in one of three formats. An item in the key-value format
// is an RLP list, whose first byte is at least 0xc0, so a prefix byte below
// that tells the slim formats apart. New items are written in the smallest
// format that restores them, see blockReceiptsItem; the key-value format is
// the one of older versions.
const (
	// slimReceiptsPrefix starts an item without the per-receipt blooms, which
	// are derived from the logs.
	slimReceiptsPrefix = 0x01
	// derivedReceiptsPrefix starts an item that also leaves out every field
	// derived from the block and its transactions: the tx hash of a receipt,
	// and the block number, block hash, tx hash, tx index and log index of a
	// log. It is read together with the block body.
	derivedReceiptsPrefix = 0x02
)

// slimReceiptRLP is a receipt in the slim format: the key-value storage form
// (receiptStorageRLP) without the bloom.
type slimReceiptRLP struct {
	Status          uint
	TxHash          common.Hash
	ContractAddress common.Address
	Logs            []*types.LogForStorage
	GasUsed         uint64
}

// derivedReceiptRLP is a receipt in the derived format. The contract address
// is empty unless the transaction created a contract; deriving it would need
// the sender, i.e. a signature recovery.
type derivedReceiptRLP struct {
	Status          uint
	ContractAddress []byte
	Logs            []derivedLogRLP
	GasUsed         uint64
}

// derivedLogRLP holds the consensus fields of a log.
type derivedLogRLP struct {
	Address common.Address
	Topics  []common.Hash
	Data    []byte
}

var (
	errSlimReceiptsBloom    = errors.New("receipt bloom does not match its logs")
	errDerivedReceiptsField = errors.New("receipt field does not match the block")
)

// encodeSlimReceipts converts a receipts item from the key-value format into
// the slim format. It fails if a stored bloom does not match the bloom derived
// from the logs, because the slim format could not restore it.
func encodeSlimReceipts(receipts []*types.ReceiptForStorage) ([]byte, error) {
	slim := make([]slimReceiptRLP, len(receipts))
	for i, r := range receipts {
		if types.CreateBloom(types.Receipts{(*types.Receipt)(r)}) != r.Bloom {
			return nil, errSlimReceiptsBloom
		}
		slim[i] = slimReceiptRLP{
			Status:          r.Status,
			TxHash:          r.TxHash,
			ContractAddress: r.ContractAddress,
			Logs:            make([]*types.LogForStorage, len(r.Logs)),
			GasUsed:         r.GasUsed,
		}
		for j, log := range r.Logs {
			slim[i].Logs[j] = (*types.LogForStorage)(log)
		}
	}
	enc, err := rlp.EncodeToBytes(slim)
	if err != nil {
		return nil, err
	}
	return append([]byte{slimReceiptsPrefix}, enc...), nil
}

// encodeDerivedReceipts converts receipts into the derived format. It fails
// if a stored field differs from the value derived from the block, because
// the derived format could not restore it.
func encodeDerivedReceipts(receipts []*types.ReceiptForStorage, hash common.Hash, number uint64, txs types.Transactions) ([]byte, error) {
	if len(receipts) != len(txs) {
		return nil, fmt.Errorf("%w: %d receipts, %d transactions", errDerivedReceiptsField, len(receipts), len(txs))
	}
	derived := make([]derivedReceiptRLP, len(receipts))
	logIndex := uint(0)
	for i, r := range receipts {
		txHash := txs[i].Hash()
		if r.TxHash != txHash {
			return nil, fmt.Errorf("%w: tx hash of receipt %d", errDerivedReceiptsField, i)
		}
		if types.CreateBloom(types.Receipts{(*types.Receipt)(r)}) != r.Bloom {
			return nil, errSlimReceiptsBloom
		}
		derived[i] = derivedReceiptRLP{
			Status:  r.Status,
			Logs:    make([]derivedLogRLP, len(r.Logs)),
			GasUsed: r.GasUsed,
		}
		if r.ContractAddress != (common.Address{}) {
			derived[i].ContractAddress = r.ContractAddress.Bytes()
		}
		for j, log := range r.Logs {
			if log.BlockNumber != number || log.BlockHash != hash || log.TxHash != txHash || log.TxIndex != uint(i) || log.Index != logIndex || log.Removed {
				return nil, fmt.Errorf("%w: log %d of receipt %d", errDerivedReceiptsField, j, i)
			}
			derived[i].Logs[j] = derivedLogRLP{Address: log.Address, Topics: log.Topics, Data: log.Data}
			logIndex++
		}
	}
	enc, err := rlp.EncodeToBytes(derived)
	if err != nil {
		return nil, err
	}
	return append([]byte{derivedReceiptsPrefix}, enc...), nil
}

// blockReceiptsItem returns the receipts item of a block: the derived format
// if it restores the receipts exactly, else the slim format if that does, else
// the key-value format.
func blockReceiptsItem(hash common.Hash, number uint64, txs types.Transactions, receipts types.Receipts) []byte {
	storage := make([]*types.ReceiptForStorage, len(receipts))
	for i, r := range receipts {
		storage[i] = (*types.ReceiptForStorage)(r)
	}
	return slimReceiptsItem(hash, number, txs, storage, nil)
}

// slimReceiptsItem converts receipts in the key-value format into the derived
// format if it restores them exactly, else into the slim format if that does,
// else returns data, or their key-value encoding if data is nil.
func slimReceiptsItem(hash common.Hash, number uint64, txs types.Transactions, receipts []*types.ReceiptForStorage, data []byte) []byte {
	enc, err := encodeDerivedReceipts(receipts, hash, number, txs)
	if err == nil {
		return enc
	}
	logger.Warn("Storing receipts without deriving their fields", "number", number, "err", err)
	if enc, err := encodeSlimReceipts(receipts); err == nil {
		return enc
	}
	logger.Warn("Storing receipts without slimming", "number", number, "err", err)
	if data == nil {
		if data, err = rlp.EncodeToBytes(receipts); err != nil {
			logger.Crit("Failed to encode block receipts", "err", err)
		}
	}
	return data
}

// decodeReceipts decodes a receipts item in any of the formats. The body is
// needed for the derived format only and is read through the given function.
func decodeReceipts(data []byte, hash common.Hash, number uint64, body func() *types.Body) (types.Receipts, error) {
	if len(data) == 0 {
		return nil, errors.New("empty receipts item")
	}
	switch data[0] {
	case derivedReceiptsPrefix:
		return decodeDerivedReceipts(data[1:], hash, number, body)
	case slimReceiptsPrefix:
		var slim []slimReceiptRLP
		if err := rlp.DecodeBytes(data[1:], &slim); err != nil {
			return nil, err
		}
		receipts := make(types.Receipts, len(slim))
		for i, s := range slim {
			r := &types.Receipt{
				Status:          s.Status,
				Logs:            make([]*types.Log, len(s.Logs)),
				TxHash:          s.TxHash,
				ContractAddress: s.ContractAddress,
				GasUsed:         s.GasUsed,
			}
			for j, log := range s.Logs {
				r.Logs[j] = (*types.Log)(log)
			}
			r.Bloom = types.CreateBloom(types.Receipts{r})
			receipts[i] = r
		}
		return receipts, nil
	}
	var storageReceipts []*types.ReceiptForStorage
	if err := rlp.DecodeBytes(data, &storageReceipts); err != nil {
		return nil, err
	}
	receipts := make(types.Receipts, len(storageReceipts))
	for i, receipt := range storageReceipts {
		receipts[i] = (*types.Receipt)(receipt)
	}
	return receipts, nil
}

// decodeDerivedReceipts restores receipts in the derived format from the block.
func decodeDerivedReceipts(data []byte, hash common.Hash, number uint64, body func() *types.Body) (types.Receipts, error) {
	var derived []derivedReceiptRLP
	if err := rlp.DecodeBytes(data, &derived); err != nil {
		return nil, err
	}
	b := body()
	if b == nil {
		return nil, errors.New("block body missing")
	}
	if len(b.Transactions) != len(derived) {
		return nil, fmt.Errorf("%d receipts, %d transactions", len(derived), len(b.Transactions))
	}
	receipts := make(types.Receipts, len(derived))
	logIndex := uint(0)
	for i, d := range derived {
		txHash := b.Transactions[i].Hash()
		r := &types.Receipt{
			Status:  d.Status,
			Logs:    make([]*types.Log, len(d.Logs)),
			TxHash:  txHash,
			GasUsed: d.GasUsed,
		}
		if len(d.ContractAddress) != 0 {
			r.ContractAddress = common.BytesToAddress(d.ContractAddress)
		}
		for j, l := range d.Logs {
			r.Logs[j] = &types.Log{
				Address:     l.Address,
				Topics:      l.Topics,
				Data:        l.Data,
				BlockNumber: number,
				TxHash:      txHash,
				TxIndex:     uint(i),
				BlockHash:   hash,
				Index:       logIndex,
			}
			logIndex++
		}
		r.Bloom = types.CreateBloom(types.Receipts{r})
		receipts[i] = r
	}
	return receipts, nil
}
