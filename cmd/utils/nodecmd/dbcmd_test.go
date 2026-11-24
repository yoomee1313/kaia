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
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/types"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/hashchain"
	"github.com/kaiachain/kaia/consensus/faker"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/kaiachain/kaia/storage/statedb"
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
	last := uint64(hashchain.SegmentSize + 300)
	hashOf := func(n uint64) common.Hash { return crypto.Keccak256Hash(big.NewInt(int64(n)).Bytes(), []byte("block")) }
	list := func(n uint64) *types.BlockAccessList {
		return &types.BlockAccessList{Accounts: []types.AccountChanges{{
			Address:        common.BigToAddress(big.NewInt(int64(n % 97))),
			BalanceChanges: []types.BalanceChange{{BlockAccessIndex: 1, PostBalance: big.NewInt(int64(n))}},
		}}}
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
	chain, err := state.BALHashChain.Load(file)
	require.NoError(t, err)
	want, err := hashchain.Build(last, func(n uint64) (common.Hash, error) {
		if n == 0 {
			return state.EmptyListHash, nil
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
	signedFile := filepath.Join(pkg, state.BALHashChain.FileName(genesis))
	require.FileExists(t, signedFile+".sig")
	require.FileExists(t, filepath.Join(pkg, "package.json"))
	pubKey, err := hex.DecodeString(pubHex)
	require.NoError(t, err)
	signed, err := state.BALHashChain.LoadSigned(pkg, genesis, pubKey)
	require.NoError(t, err)
	require.Equal(t, want, signed)
	require.NoError(t, checkHashChain(db, pkg, pubHex, -1, out))
	require.NoError(t, checkHashChain(db, signedFile, pubHex, 5, out))
	signedOut := filepath.Join(t.TempDir(), "signed.bin")
	require.NoError(t, buildHashChain(db, hashChainOptions{out: signedOut, keyFile: keyFile}, out))
	require.NoError(t, checkHashChain(db, signedOut, pubHex, -1, out))
	otherPub := writeSigningKey(t, filepath.Join(t.TempDir(), "other.pem"))
	require.ErrorIs(t, checkHashChain(db, pkg, otherPub, -1, out), hashchain.ErrSignature)

	enc, err := list(1).Encode()
	require.NoError(t, err)
	require.NoError(t, db.WriteBAL(hashOf(8300), 8300, enc))
	require.ErrorIs(t, checkHashChain(db, file, "", -1, out), hashchain.ErrMismatch)
	require.ErrorIs(t, checkHashChain(db, file, "", 8300, out), hashchain.ErrMismatch)
	require.NoError(t, checkHashChain(db, file, "", 5, out), "the other segment still holds")

	var stat bytes.Buffer
	require.NoError(t, balStat(db, 0, ^uint64(0), &stat))
	require.Contains(t, stat.String(), "blocks              8492")
}

// TestDBRebuild records the lists of a chain, rebuilds its state from them
// into a live pruned chain database, and opens the head state there.
func TestDBRebuild(t *testing.T) {
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	config := params.TestKaiaConfig("prague")
	gspec := &blockchain.Genesis{Config: config, Alloc: blockchain.GenesisAlloc{addr: {Balance: big.NewInt(params.KAIA)}}}
	signer := types.LatestSignerForChainID(config.ChainID)

	dir := filepath.Join(t.TempDir(), "chaindata")
	db := openTestChainDB(dir)
	genesis := gspec.MustCommit(db)
	chain, err := blockchain.NewBlockChain(db, &blockchain.CacheConfig{
		HistoryMode: database.HistoryModeBAL, CacheSize: 64, BlockInterval: 1,
		TriesInMemory: blockchain.DefaultTriesInMemory, TrieNodeCacheConfig: statedb.GetEmptyTrieNodeCacheConfig(),
	}, config, faker.NewFaker(), vm.Config{})
	require.NoError(t, err)
	const n = 40
	sent := new(big.Int)
	blocks, _ := blockchain.GenerateChain(config, genesis, faker.NewFaker(), db, n, func(i int, b *blockchain.BlockGen) {
		to := common.BigToAddress(big.NewInt(int64(0x10000 + i%7 + 1)))
		if i%7 == 0 {
			sent.Add(sent, big.NewInt(int64(i+1)))
		}
		tx, err := types.SignTx(types.NewTransaction(b.TxNonce(addr), to, big.NewInt(int64(i+1)), 21000, big.NewInt(25*params.Gkei), nil), signer, key)
		require.NoError(t, err)
		b.AddTx(tx)
	})
	_, err = chain.InsertChain(blocks)
	require.NoError(t, err)
	chain.Stop()
	first, next := db.ReadBALRange()
	require.Equal(t, [2]uint64{1, n + 1}, [2]uint64{first, next})

	out := filepath.Join(t.TempDir(), "rebuilt")
	o := rebuildOptions{
		out: out, commitEvery: 8, batch: 4, retention: 8, dirtyMiB: 64, cacheMiB: 64, workers: 2,
		stateShards: 1, dbType: database.LevelDB, genesis: gspec,
	}
	require.NoError(t, rebuildState(db, dir, o, io.Discard))
	require.NoDirExists(t, out+".rebuild")

	rebuilt := openTestChainDB(out)
	defer rebuilt.Close()
	require.True(t, rebuilt.ReadPruningEnabled())
	require.Equal(t, blocks[n-1].Hash(), rebuilt.ReadHeadBlockHash())
	sdb, err := state.New(blocks[n-1].Root(), state.NewDatabase(rebuilt), nil, nil)
	require.NoError(t, err)
	require.Equal(t, sent, sdb.GetBalance(common.BigToAddress(big.NewInt(0x10001))))
	_, err = state.New(blocks[10].Root(), state.NewDatabase(rebuilt), nil, nil)
	require.Error(t, err, "older states are pruned")
	require.Equal(t, uint64(n), rebuilt.ReadBlockByHash(blocks[n-1].Hash()).NumberU64())

	require.Error(t, rebuildState(openTestChainDB(dir), dir, o, io.Discard), "the output exists")
}

// TestSignedHashChain writes a signed package and loads it back: from the
// package directory, from node_modules and from the file, and fails with
// another key, another genesis, a tampered file or signature, the
// placeholder key and a missing package.
func TestSignedHashChain(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	genesis := common.HexToHash("0x1234")
	chain, err := hashchain.Build(hashchain.SegmentSize+10, func(n uint64) (common.Hash, error) { return common.BytesToHash([]byte{byte(n), byte(n >> 8), 1}), nil })
	require.NoError(t, err)

	root := t.TempDir()
	dir := filepath.Join(root, "node_modules", state.HashChainPackage)
	file, err := writeHashChainPackage(dir, genesis, chain, key, "1.2.3")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, state.BALHashChain.FileName(genesis)), file)
	manifest, err := os.ReadFile(filepath.Join(dir, "package.json"))
	require.NoError(t, err)
	var pkg map[string]interface{}
	require.NoError(t, json.Unmarshal(manifest, &pkg))
	require.Equal(t, state.HashChainPackage, pkg["name"])
	require.Equal(t, "1.2.3", pkg["version"])

	for _, path := range []string{dir, root, file} {
		got, err := state.BALHashChain.LoadSigned(path, genesis, pub)
		require.NoError(t, err, path)
		require.Equal(t, chain, got, path)
	}

	otherPub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	_, err = state.BALHashChain.LoadSigned(dir, genesis, otherPub)
	require.ErrorIs(t, err, hashchain.ErrSignature, "another key")
	_, err = state.BALHashChain.LoadSigned(file, common.HexToHash("0x5678"), pub)
	require.ErrorIs(t, err, hashchain.ErrSignature, "another genesis")
	_, err = state.BALHashChain.LoadSigned(dir, common.HexToHash("0x5678"), pub)
	require.ErrorContains(t, err, "no BAL hash chain of genesis", "no file of another genesis")
	_, err = state.BALHashChain.LoadSigned(dir, genesis, state.HashChainPublicKey)
	require.ErrorContains(t, err, "no BAL hash chain signing key", "the placeholder key")

	enc, err := os.ReadFile(file)
	require.NoError(t, err)
	tampered := append([]byte{}, enc...)
	tampered[len(tampered)-1] ^= 1
	require.NoError(t, os.WriteFile(file, tampered, 0o644))
	_, err = state.BALHashChain.LoadSigned(dir, genesis, pub)
	require.ErrorIs(t, err, hashchain.ErrSignature, "a tampered file")
	require.NoError(t, os.WriteFile(file, enc, 0o644))

	sig, err := os.ReadFile(file + ".sig")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file+".sig", []byte("00"), 0o644))
	_, err = state.BALHashChain.LoadSigned(dir, genesis, pub)
	require.ErrorIs(t, err, hashchain.ErrSignature, "a bad signature file")
	require.NoError(t, os.Remove(file+".sig"))
	_, err = state.BALHashChain.LoadSigned(dir, genesis, pub)
	require.ErrorIs(t, err, os.ErrNotExist, "no signature")
	require.NoError(t, os.WriteFile(file+".sig", sig, 0o644))

	_, err = state.BALHashChain.LoadSigned(filepath.Join(root, "missing"), genesis, pub)
	require.ErrorIs(t, err, os.ErrNotExist, "no package")
}
