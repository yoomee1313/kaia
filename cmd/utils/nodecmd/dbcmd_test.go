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

package nodecmd

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/stretchr/testify/require"
)

func openTestChainDB(dir string) database.DBManager {
	return database.NewDBManager(&database.DBConfig{Dir: dir, DBType: database.LevelDB, NumStateTrieShards: 1, OpenFilesLimit: 256})
}

// writeSigningKey writes a new ed25519 key as openssl genpkey does and
// returns its public key in hex.
func writeSigningKey(t *testing.T, path string) string {
	pub, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	return hex.EncodeToString(pub)
}

// TestDBHashChain writes lists to a chain database, builds the chain file and
// checks it, signs a package, then tampers with a list.
func TestDBHashChain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chaindata")
	last := uint64(bal.SegmentSize + 300)
	hashOf := func(n uint64) common.Hash { return crypto.Keccak256Hash(big.NewInt(int64(n)).Bytes(), []byte("block")) }
	list := func(n uint64) *access.BlockAccessList {
		builder := access.NewBuilder()
		builder.SetBalance(common.BigToAddress(big.NewInt(int64(n%97))), 1, big.NewInt(int64(n)))
		return builder.Build()
	}
	db := openTestChainDB(dir)
	defer db.Close()
	genesis := hashOf(0)
	db.WriteCanonicalHash(genesis, 0)
	for n := uint64(1); n <= last; n++ {
		enc, err := list(n).Encode()
		require.NoError(t, err)
		db.WriteCanonicalHash(hashOf(n), n)
		require.NoError(t, db.WriteBAL(hashOf(n), n, enc))
	}
	require.NoError(t, db.WriteBALRange(1, last+1))
	out := io.Discard

	file := filepath.Join(t.TempDir(), "bal.hashchain")
	require.NoError(t, buildHashChain(db, hashChainOptions{out: file}, out))
	chain, err := bal.LoadHashChain(file)
	require.NoError(t, err)
	want, err := bal.BuildHashChain(last, func(n uint64) (common.Hash, error) {
		if n == 0 {
			return bal.EmptyListHash, nil
		}
		return list(n).Hash()
	})
	require.NoError(t, err)
	require.Equal(t, want, chain)
	root, err := os.ReadFile(file + ".root")
	require.NoError(t, err)
	require.Equal(t, want.Root().Hex(), strings.TrimSpace(string(root)))

	require.NoError(t, checkHashChain(db, file, "", -1, out))
	for _, n := range []int64{0, 5, 8192, 8492} {
		require.NoError(t, checkHashChain(db, file, "", n, out), "block %d", n)
	}

	// The signed package, checked with the public key of the signing key.
	keyFile := filepath.Join(t.TempDir(), "signing.pem")
	pubHex := writeSigningKey(t, keyFile)
	pkg := filepath.Join(t.TempDir(), "bal-hashchain")
	require.Error(t, buildHashChain(db, hashChainOptions{pkg: pkg}, out), "a package is signed")
	require.NoError(t, buildHashChain(db, hashChainOptions{pkg: pkg, keyFile: keyFile, version: "3.0.2"}, out))
	signedFile := filepath.Join(pkg, bal.HashChainFileName(genesis))
	require.FileExists(t, signedFile+".sig")
	require.FileExists(t, filepath.Join(pkg, "package.json"))
	pubKey, err := hex.DecodeString(pubHex)
	require.NoError(t, err)
	signed, err := bal.LoadSignedHashChain(pkg, genesis, pubKey)
	require.NoError(t, err)
	require.Equal(t, want, signed)
	require.NoError(t, checkHashChain(db, pkg, pubHex, -1, out))
	require.NoError(t, checkHashChain(db, signedFile, pubHex, 5, out))
	signedOut := filepath.Join(t.TempDir(), "signed.bin")
	require.NoError(t, buildHashChain(db, hashChainOptions{out: signedOut, keyFile: keyFile}, out))
	require.NoError(t, checkHashChain(db, signedOut, pubHex, -1, out))
	otherPub := writeSigningKey(t, filepath.Join(t.TempDir(), "other.pem"))
	require.ErrorIs(t, checkHashChain(db, pkg, otherPub, -1, out), bal.ErrHashChainSignature)

	enc, err := list(1).Encode()
	require.NoError(t, err)
	require.NoError(t, db.WriteBAL(hashOf(8300), 8300, enc))
	require.ErrorIs(t, checkHashChain(db, file, "", -1, out), bal.ErrHashChain)
	require.ErrorIs(t, checkHashChain(db, file, "", 8300, out), bal.ErrHashChain)
	require.NoError(t, checkHashChain(db, file, "", 5, out), "the other segment still holds")
}
