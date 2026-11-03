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

	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
)

// slimReceiptsPrefix starts a frozen receipts item that is stored without the
// per-receipt bloom filters. An item in the key-value format is an RLP list,
// whose first byte is at least 0xc0, so the two formats cannot be confused.
const slimReceiptsPrefix = 0x01

// slimReceiptRLP is the frozen form of a receipt: the key-value storage form
// (receiptStorageRLP) without the bloom, which is derived from the logs.
type slimReceiptRLP struct {
	Status          uint
	TxHash          common.Hash
	ContractAddress common.Address
	Logs            []*types.LogForStorage
	GasUsed         uint64
}

var errSlimReceiptsBloom = errors.New("receipt bloom does not match its logs")

// encodeFrozenReceipts converts a receipts item from the key-value format into
// the slim frozen format. It fails if a stored bloom does not match the bloom
// derived from the logs, because the slim format could not restore it; the
// caller then freezes the item unchanged.
func encodeFrozenReceipts(data []byte) ([]byte, error) {
	var receipts []*types.ReceiptForStorage
	if err := rlp.DecodeBytes(data, &receipts); err != nil {
		return nil, err
	}
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

// frozenReceipts returns the receipts item to append to the freezer: the slim
// form if it can restore the item exactly, otherwise the item unchanged.
func frozenReceipts(number uint64, data []byte) []byte {
	slim, err := encodeFrozenReceipts(data)
	if err != nil {
		logger.Warn("Freezing receipts without slimming", "number", number, "err", err)
		return data
	}
	return slim
}

// decodeReceipts decodes a receipts item in either the key-value format or the
// slim frozen format. The blooms of slim receipts are derived from their logs.
func decodeReceipts(data []byte) (types.Receipts, error) {
	if len(data) > 0 && data[0] == slimReceiptsPrefix {
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
