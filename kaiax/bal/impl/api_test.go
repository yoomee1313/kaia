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
	"testing"

	"github.com/kaiachain/kaia/networks/rpc"
	"github.com/stretchr/testify/require"
)

// TestAdminBALAPI regenerates the lists through the admin APIs of the module.
func TestAdminBALAPI(t *testing.T) {
	f := newRegenFixture(t, "prague", 40)
	chain := f.open(t)
	server := rpc.NewServer()
	for _, api := range of(chain).APIs() {
		require.NoError(t, server.RegisterName(api.Namespace, api.Service))
	}
	client := rpc.DialInProc(server)
	defer client.Close()

	require.NoError(t, client.Call(nil, "admin_startBALRegen", 0, 0, map[string]interface{}{"workers": 2}))
	waitRegen(t, chain)
	var status RegenStatus
	require.NoError(t, client.Call(&status, "admin_balRegenStatus"))
	require.Empty(t, status.Error)
	require.Equal(t, uint64(len(f.blocks)+1), status.Next)
	f.requireLists(t, chain, 1, uint64(len(f.blocks)))

	var trie TrieBuildStatus
	require.NoError(t, client.Call(&trie, "admin_stateTrieBuildStatus"))
	require.False(t, trie.Running)

	err := client.Call(nil, "admin_balPeers")
	require.ErrorContains(t, err, "not supported by this node")
}
