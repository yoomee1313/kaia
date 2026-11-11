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
	"github.com/kaiachain/kaia/networks/rpc"
)

func (c *BALModule) APIs() []rpc.API {
	return []rpc.API{
		{
			Namespace: "admin",
			Version:   "1.0",
			Service:   &AdminBALAPI{c},
		},
	}
}

// AdminBALAPI serves the admin APIs of the BAL module.
type AdminBALAPI struct {
	c *BALModule
}

// StartBALRegen starts regenerating the block access lists of blocks from to to
// (experimental). Zero takes the block after the stored lists and the head,
// or, for the genesis source, the blocks before the stored lists; opts sets
// the source (local, genesis), mode (write, verify, reads), memory, workers
// and skipReads.
func (api *AdminBALAPI) StartBALRegen(from, to uint64, opts *RegenConfig) error {
	cfg := RegenConfig{}
	if opts != nil {
		cfg = *opts
	}
	cfg.From, cfg.To = from, to
	return StartRegen(api.c.bc, cfg)
}

// StopBALRegen stops the running regeneration of block access lists.
func (api *AdminBALAPI) StopBALRegen() error {
	return StopRegen(api.c.bc)
}

// BalRegenStatus reports the last regeneration of block access lists.
func (api *AdminBALAPI) BalRegenStatus() RegenStatus {
	return GetRegenStatus(api.c.bc)
}
