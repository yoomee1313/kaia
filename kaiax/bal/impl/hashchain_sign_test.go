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
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/stretchr/testify/require"
)

// TestSignedHashChain writes a signed package and loads it back: from the
// package directory, from node_modules and from the file, and fails with
// another key, another genesis, a tampered file or signature, the
// placeholder key and a missing package.
func TestSignedHashChain(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	genesis := common.HexToHash("0x1234")
	chain, err := BuildHashChain(SegmentSize+10, func(n uint64) (common.Hash, error) { return testHash(n), nil })
	require.NoError(t, err)

	root := t.TempDir()
	dir := filepath.Join(root, "node_modules", HashChainPackage)
	file, err := WriteHashChainPackage(dir, genesis, chain, key, "1.2.3")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, HashChainFileName(genesis)), file)
	manifest, err := os.ReadFile(filepath.Join(dir, "package.json"))
	require.NoError(t, err)
	var pkg map[string]interface{}
	require.NoError(t, json.Unmarshal(manifest, &pkg))
	require.Equal(t, HashChainPackage, pkg["name"])
	require.Equal(t, "1.2.3", pkg["version"])

	for _, path := range []string{dir, root, file} {
		got, err := LoadSignedHashChain(path, genesis, pub)
		require.NoError(t, err, path)
		require.Equal(t, chain, got, path)
	}

	otherPub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	_, err = LoadSignedHashChain(dir, genesis, otherPub)
	require.ErrorIs(t, err, ErrHashChainSignature, "another key")
	_, err = LoadSignedHashChain(file, common.HexToHash("0x5678"), pub)
	require.ErrorIs(t, err, ErrHashChainSignature, "another genesis")
	_, err = LoadSignedHashChain(dir, common.HexToHash("0x5678"), pub)
	require.ErrorContains(t, err, "no BAL hash chain of genesis", "no file of another genesis")
	_, err = LoadSignedHashChain(dir, genesis, HashChainPublicKey)
	require.ErrorContains(t, err, "no BAL hash chain signing key", "the placeholder key")

	enc, err := os.ReadFile(file)
	require.NoError(t, err)
	tampered := append([]byte{}, enc...)
	tampered[len(tampered)-1] ^= 1
	require.NoError(t, os.WriteFile(file, tampered, 0o644))
	_, err = LoadSignedHashChain(dir, genesis, pub)
	require.ErrorIs(t, err, ErrHashChainSignature, "a tampered file")
	require.NoError(t, os.WriteFile(file, enc, 0o644))

	sig, err := os.ReadFile(file + ".sig")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file+".sig", []byte("00"), 0o644))
	_, err = LoadSignedHashChain(dir, genesis, pub)
	require.ErrorIs(t, err, ErrHashChainSignature, "a bad signature file")
	require.NoError(t, os.Remove(file+".sig"))
	_, err = LoadSignedHashChain(dir, genesis, pub)
	require.ErrorIs(t, err, os.ErrNotExist, "no signature")
	require.NoError(t, os.WriteFile(file+".sig", sig, 0o644))

	_, err = LoadSignedHashChain(filepath.Join(root, "missing"), genesis, pub)
	require.ErrorIs(t, err, os.ErrNotExist, "no package")
}
