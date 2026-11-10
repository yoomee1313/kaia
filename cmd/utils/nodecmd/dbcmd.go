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
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kaiachain/kaia/blockchain/bal"
	"github.com/kaiachain/kaia/blockchain/bal/access"
	"github.com/kaiachain/kaia/cmd/utils"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/urfave/cli/v2"
)

var (
	dbOutFlag = &cli.PathFlag{
		Name:  "out",
		Usage: "File or directory to write",
	}
	dbToFlag = &cli.Uint64Flag{
		Name:  "to",
		Usage: "Last block (0 = the last stored block access list)",
	}
	hashChainPackageFlag = &cli.PathFlag{
		Name:  "package",
		Usage: "Directory of the " + bal.HashChainPackage + " npm package to write the signed chain to",
	}
	hashChainKeyFlag = &cli.PathFlag{
		Name:  "key",
		Usage: "PEM file of the ed25519 key to sign the chain with (openssl genpkey -algorithm ed25519 -out FILE)",
	}
	hashChainVersionFlag = &cli.StringFlag{
		Name:  "version",
		Usage: "Version of a new package.json",
		Value: "1.0.0",
	}
	hashChainCheckFlag = &cli.PathFlag{
		Name:  "check",
		Usage: "Check the stored block access lists against this chain file, or package directory with --pub, instead of building a chain",
	}
	hashChainPubFlag = &cli.StringFlag{
		Name:  "pub",
		Usage: "With --check, the hex ed25519 public key to check the signature of the chain with",
	}
	hashChainBlockFlag = &cli.Int64Flag{
		Name:  "block",
		Usage: "With --check, the block to prove and check alone (-1 = every stored segment)",
		Value: -1,
	}
)

var DBCommand = &cli.Command{
	Name:  "db",
	Usage: "Tools for the chain database (experimental)",
	Description: `
Run them on a stopped node, as they open its chain database.`,
	Subcommands: []*cli.Command{
		{
			Name:   "hashchain",
			Usage:  "Build, sign or check the pre-Amber BAL hash chain",
			Action: utils.MigrateFlags(dbHashChain),
			Flags: append([]cli.Flag{
				dbOutFlag, dbToFlag, hashChainPackageFlag, hashChainKeyFlag, hashChainVersionFlag,
				hashChainCheckFlag, hashChainPubFlag, hashChainBlockFlag,
			}, utils.SnapshotFlags...),
			Description: `
db hashchain --out FILE [--key PEM] [--to N]
db hashchain --package DIR --key PEM [--version V] [--to N]
builds the pre-Amber BAL hash chain from the block access lists stored from
the genesis block, writes the chain file and its root R_pre to FILE.root, and
with a key signs it, to FILE.sig. With --package the signed chain goes to the
directory of the npm package, named by the genesis hash, with a package.json.

db hashchain --check FILE|DIR [--pub KEY] [--block N]
checks the stored lists against a chain: every segment they hold in full, or
one block with a proof. With --pub the signature of the chain, a file or a
package directory, is checked too.`,
		},
	},
}

func openDBForTool(ctx *cli.Context) database.DBManager {
	return MakeFullNode(ctx).OpenDatabase(getConfig(ctx))
}

func dbHashChain(ctx *cli.Context) error {
	db := openDBForTool(ctx)
	defer db.Close()
	if path := ctx.String(hashChainCheckFlag.Name); path != "" {
		return checkHashChain(db, path, ctx.String(hashChainPubFlag.Name), ctx.Int64(hashChainBlockFlag.Name), os.Stdout)
	}
	return buildHashChain(db, hashChainOptions{
		out: ctx.String(dbOutFlag.Name), pkg: ctx.String(hashChainPackageFlag.Name),
		keyFile: ctx.String(hashChainKeyFlag.Name), version: ctx.String(hashChainVersionFlag.Name),
		to: ctx.Uint64(dbToFlag.Name),
	}, os.Stdout)
}

type hashChainOptions struct {
	out, pkg, keyFile, version string
	to                         uint64
}

// buildHashChain builds the pre-Amber BAL hash chain from the lists stored in
// db, from the genesis block to o.to, and writes it to o.out or to the package
// directory o.pkg, signed with the key in o.keyFile.
func buildHashChain(db database.DBManager, o hashChainOptions, w io.Writer) error {
	if (o.out == "") == (o.pkg == "") {
		return errors.New("one of --out and --package is required")
	}
	if o.pkg != "" && o.keyFile == "" {
		return errors.New("--package needs --key")
	}
	var key ed25519.PrivateKey
	if o.keyFile != "" {
		var err error
		if key, err = readSigningKey(o.keyFile); err != nil {
			return err
		}
	}

	first, next := db.ReadBALRange()
	if first != 1 || next < 2 {
		return fmt.Errorf("the stored lists cover blocks %d-%d, the chain starts at the genesis block", first, max(next, 1)-1)
	}
	last := next - 1
	if o.to != 0 {
		if o.to > last {
			return fmt.Errorf("block %d is after the last stored list %d", o.to, last)
		}
		last = o.to
	}
	genesis := db.ReadCanonicalHash(0)
	if key != nil && genesis == (common.Hash{}) {
		return errors.New("no genesis block in the chain database, which the signature is bound to")
	}

	start, lastLog := time.Now(), time.Now()
	chain, err := bal.BuildHashChain(last, func(n uint64) (common.Hash, error) {
		if time.Since(lastLog) > 30*time.Second {
			lastLog = time.Now()
			fmt.Fprintf(w, "hashed %d/%d blocks\n", n, last)
		}
		return bal.HashOf(db, n)
	})
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if _, err := chain.WriteTo(&buf); err != nil {
		return err
	}
	file := o.out
	if o.pkg != "" {
		if file, err = bal.WriteHashChainPackage(o.pkg, genesis, chain, key, o.version); err != nil {
			return err
		}
	} else {
		if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
			return err
		}
		if key != nil {
			sig := bal.SignHashChain(key, genesis, buf.Bytes())
			if err := os.WriteFile(file+".sig", []byte(hex.EncodeToString(sig)+"\n"), 0o644); err != nil {
				return err
			}
		}
	}
	root := chain.Root()
	if err := os.WriteFile(file+".root", []byte(root.Hex()+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(w, "hash chain of blocks 0-%d: %d segments, %d bytes, R_pre %s, in %s\n", last, len(chain.Roots), buf.Len(), root.Hex(), time.Since(start).Round(time.Second))
	if key != nil {
		fmt.Fprintf(w, "signed %s for genesis %s with public key %x\n", file, genesis.Hex(), []byte(key.Public().(ed25519.PublicKey)))
	}
	return nil
}

// readSigningKey reads an ed25519 private key from a PKCS #8 PEM file, as
// openssl genpkey -algorithm ed25519 writes it.
func readSigningKey(path string) (ed25519.PrivateKey, error) {
	enc, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(enc)
	if block == nil {
		return nil, fmt.Errorf("%s is not a PEM file", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an ed25519 key", path)
	}
	return ed, nil
}

// checkHashChain checks the lists stored in db against the chain in path:
// every segment the stored lists hold in full, or block alone with a proof.
// With pubHex the chain, a file or a package directory, is loaded checking its
// signature with that public key.
func checkHashChain(db database.DBManager, path, pubHex string, block int64, w io.Writer) error {
	var (
		chain *bal.HashChain
		err   error
	)
	if pubHex != "" {
		pub, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(pubHex), "0x"))
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return errors.New("--pub is not a hex ed25519 public key")
		}
		if chain, err = bal.LoadSignedHashChain(path, db.ReadCanonicalHash(0), pub); err != nil {
			return err
		}
		fmt.Fprintf(w, "signature verified for genesis %s\n", db.ReadCanonicalHash(0).Hex())
	} else if chain, err = bal.LoadHashChain(path); err != nil {
		return err
	}
	hash := func(n uint64) (common.Hash, error) { return bal.HashOf(db, n) }
	fmt.Fprintf(w, "hash chain of blocks 0-%d, R_pre %s\n", chain.Last, chain.Root().Hex())

	if block >= 0 {
		n := uint64(block)
		proof, err := bal.BlockProof(n, chain.Last, hash)
		if err != nil {
			return err
		}
		list := &access.BlockAccessList{}
		if n > 0 {
			if list, err = bal.Read(db, n); err != nil {
				return err
			}
		}
		if err := chain.VerifyBlock(n, list, proof); err != nil {
			return err
		}
		fmt.Fprintf(w, "block %d verified with a proof of %d bytes\n", n, len(proof)*common.HashLength)
		return nil
	}

	first, next := db.ReadBALRange()
	start, checked := time.Now(), 0
	for k := range uint64(len(chain.Roots)) {
		from, to := chain.SegmentBlocks(k)
		if max(from, 1) < first || to >= next {
			continue // not stored in full
		}
		hashes := make([]common.Hash, 0, to-from+1)
		for n := from; n <= to; n++ {
			h, err := hash(n)
			if err != nil {
				return fmt.Errorf("block %d: %w", n, err)
			}
			hashes = append(hashes, h)
		}
		if err := chain.VerifySegment(k, hashes); err != nil {
			return err
		}
		checked++
	}
	fmt.Fprintf(w, "checked %d of %d segments in %s\n", checked, len(chain.Roots), time.Since(start).Round(time.Millisecond))
	return nil
}
