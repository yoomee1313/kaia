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
	"errors"

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

// SetSync hands the module the kaia/69 syncer of its node, which the peers
// source of a regeneration fetches from.
func (c *BALModule) SetSync(s *Sync) {
	c.sync = s
}

// AdminBALAPI serves the admin APIs of the BAL module.
type AdminBALAPI struct {
	c *BALModule
}

// StartBALRegen starts regenerating the block access lists of blocks from to to
// (experimental). Zero takes the block after the stored lists and the head,
// or, for the genesis, import and peers sources, the blocks before the stored
// lists; opts sets the source (local, genesis, import with importDir, peers),
// mode (write, verify, reads), memory, workers and skipReads. The import and
// peers sources stop at the last block of the signed BAL hash chain, the
// hashChain file or package directory or else the one of --bal.hash-chain.
func (api *AdminBALAPI) StartBALRegen(from, to uint64, opts *RegenConfig) error {
	cfg := RegenConfig{}
	if opts != nil {
		cfg = *opts
	}
	cfg.From, cfg.To = from, to
	if s := api.c.sync; s != nil {
		if cfg.Source == RegenPeers {
			cfg.Fetcher = s
		}
		if (cfg.Source == RegenImport || cfg.Source == RegenPeers) && cfg.HashChainFile == "" {
			cfg.HashChain = s.HashChain()
		}
	}
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

// BalPeers returns the history ranges kaia/69 peers announced: the oldest
// block whose header, body, receipts and block access list each one holds.
func (api *AdminBALAPI) BalPeers() (map[string]HistoryRange, error) {
	if api.c.sync == nil {
		return nil, errors.New("not supported by this node")
	}
	return api.c.sync.PeerRanges(), nil
}

// BuildStateTrie builds the state trie of a past block on a history node in
// the background, from a later stored trie and the BAL history, and keeps it
// from live pruning, so that proofs, dumps and storage iteration work at the
// block.
func (api *AdminBALAPI) BuildStateTrie(number uint64) error {
	return StartTrieBuild(api.c.bc, number)
}

// StopStateTrieBuild stops the running state trie build.
func (api *AdminBALAPI) StopStateTrieBuild() error {
	return StopTrieBuild(api.c.bc)
}

// StateTrieBuildStatus reports the last state trie build.
func (api *AdminBALAPI) StateTrieBuildStatus() TrieBuildStatus {
	return GetTrieBuildStatus(api.c.bc)
}
