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
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kaiachain/kaia/common"
)

// The hash chains are distributed in the npm package HashChainPackage, which
// an operator installs with npm; the node does not fetch it. The package holds
// the chain of each network as <genesis hash in hex>.bin, in the format of
// WriteTo, and its ed25519 signature, in hex, as <genesis hash in hex>.bin.sig.
// The signature is over hashChainSigDomain || genesis hash || file, so the
// chain of one network does not verify as the chain of another.

const HashChainPackage = "@kaiachain/bal-hashchain"

// HashChainPublicKey is the ed25519 key the hash chain files are verified
// with.
//
// PLACEHOLDER: the Kaia Foundation signing key replaces this all-zero key at
// release. No chain verifies with the placeholder.
var HashChainPublicKey = ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))

var (
	ErrHashChainSignature = errors.New("BAL hash chain signature mismatch")

	hashChainSigDomain = []byte("kaia-bal-hashchain-v1")
)

// HashChainFileName returns the name of the chain file of the network with
// the genesis block genesis.
func HashChainFileName(genesis common.Hash) string {
	return hex.EncodeToString(genesis[:]) + ".bin"
}

func hashChainSigned(genesis common.Hash, file []byte) []byte {
	msg := make([]byte, 0, len(hashChainSigDomain)+common.HashLength+len(file))
	msg = append(msg, hashChainSigDomain...)
	msg = append(msg, genesis[:]...)
	return append(msg, file...)
}

// SignHashChain returns the signature of the chain file of the network with
// the genesis block genesis.
func SignHashChain(key ed25519.PrivateKey, genesis common.Hash, file []byte) []byte {
	return ed25519.Sign(key, hashChainSigned(genesis, file))
}

// VerifyHashChain checks the signature of the chain file of the network with
// the genesis block genesis, and returns the chain.
func VerifyHashChain(pub ed25519.PublicKey, genesis common.Hash, file, sig []byte) (*HashChain, error) {
	if len(pub) != ed25519.PublicKeySize || bytes.Equal(pub, make([]byte, ed25519.PublicKeySize)) {
		return nil, errors.New("no BAL hash chain signing key in this build")
	}
	if !ed25519.Verify(pub, hashChainSigned(genesis, file), sig) {
		return nil, fmt.Errorf("%w for genesis %s", ErrHashChainSignature, genesis.Hex())
	}
	return ReadHashChain(bytes.NewReader(file))
}

// HashChainPath returns the chain file of the network with the genesis block
// genesis: path itself if it is a file, or the file in the package directory
// path, or in path/node_modules/HashChainPackage.
func HashChainPath(path string, genesis common.Hash) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return path, nil
	}
	name := HashChainFileName(genesis)
	for _, dir := range []string{path, filepath.Join(path, "node_modules", HashChainPackage)} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return filepath.Join(dir, name), nil
		}
	}
	return "", fmt.Errorf("no BAL hash chain of genesis %s (%s) in %s", genesis.Hex(), name, path)
}

// LoadSignedHashChain reads the chain of the network with the genesis block
// genesis from path, a chain file or the directory of the package, and checks
// its signature, in the file next to it with the suffix .sig, with pub.
func LoadSignedHashChain(path string, genesis common.Hash, pub ed25519.PublicKey) (*HashChain, error) {
	file, err := HashChainPath(path, genesis)
	if err != nil {
		return nil, err
	}
	enc, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	sigHex, err := os.ReadFile(file + ".sig")
	if err != nil {
		return nil, fmt.Errorf("signature of the BAL hash chain: %w", err)
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(sigHex)), "0x"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: %s is not a hex ed25519 signature", ErrHashChainSignature, file+".sig")
	}
	return VerifyHashChain(pub, genesis, enc, sig)
}

// WriteHashChainPackage writes the chain of the network with the genesis
// block genesis and its signature with key to the package directory dir, with
// a package.json unless dir has one, and returns the path of the chain file.
func WriteHashChainPackage(dir string, genesis common.Hash, chain *HashChain, key ed25519.PrivateKey, version string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if _, err := chain.WriteTo(&buf); err != nil {
		return "", err
	}
	file := filepath.Join(dir, HashChainFileName(genesis))
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		return "", err
	}
	sig := SignHashChain(key, genesis, buf.Bytes())
	if err := os.WriteFile(file+".sig", []byte(hex.EncodeToString(sig)+"\n"), 0o644); err != nil {
		return "", err
	}
	manifest := filepath.Join(dir, "package.json")
	if _, err := os.Stat(manifest); errors.Is(err, os.ErrNotExist) {
		enc, err := json.MarshalIndent(map[string]interface{}{
			"name":        HashChainPackage,
			"version":     version,
			"description": "Pre-Amber BAL hash chains of the Kaia networks, signed by the Kaia Foundation",
			"files":       []string{"*.bin", "*.bin.sig"},
		}, "", "  ")
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(manifest, append(enc, '\n'), 0o644); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return file, nil
}
