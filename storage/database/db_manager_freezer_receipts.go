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
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/rlp"
)

// frozenReceipts returns the receipts item to append to the freezer. Items
// written in a slim format are moved as they are; items in the key-value
// format, written by an older version, are converted like blockReceiptsItem.
func frozenReceipts(hash common.Hash, number uint64, txs types.Transactions, data []byte) []byte {
	if len(data) > 0 && (data[0] == slimReceiptsPrefix || data[0] == derivedReceiptsPrefix) {
		return data
	}
	var receipts []*types.ReceiptForStorage
	if err := rlp.DecodeBytes(data, &receipts); err != nil {
		logger.Warn("Freezing undecodable receipts unchanged", "number", number, "err", err)
		return data
	}
	return slimReceiptsItem(hash, number, txs, receipts, data)
}
