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
	"sort"
	"strings"
	"time"

	"github.com/kaiachain/kaia/cmd/utils"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/kaiax/bal"
	bal_impl "github.com/kaiachain/kaia/kaiax/bal/impl"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/klauspost/compress/zstd"
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
		Usage: "Directory of the " + bal_impl.HashChainPackage + " npm package to write the signed chain to",
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
	dbStatFromFlag = &cli.Uint64Flag{
		Name:  "from",
		Usage: "First block (0 = the first stored block access list)",
	}
	dbStatToFlag = &cli.Uint64Flag{
		Name:  "end",
		Usage: "End block, exclusive (0 = after the last stored block access list)",
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
		{
			Name:   "rebuild",
			Usage:  "Rebuild the state from the BALs, without executing, into a new live pruned chain database",
			Action: utils.MigrateFlags(dbRebuild),
			Flags:  append(rebuildFlags, utils.SnapshotFlags...),
			Description: `
db rebuild --out DIR
applies the stored block access lists from the genesis block, without
executing any block, checking the state root of the header at the end of
every batch, and writes a chain database of a live pruned node to DIR: the
block data of this node with the rebuilt state. It works in DIR.rebuild and
resumes from there after a restart.`,
		},
		{
			Name:   "stat",
			Usage:  "Summarize the stored block access lists",
			Action: utils.MigrateFlags(dbStat),
			Flags:  append([]cli.Flag{dbStatFromFlag, dbStatToFlag}, utils.SnapshotFlags...),
			Description: `
db stat [--from N] [--end M]
prints the sizes of the stored block access lists, raw and zstd, the sizes of
their reads stored apart, and per block the accounts, slots and changes.`,
		},
	},
}

func openDBForTool(ctx *cli.Context) (database.DBManager, string) {
	stack := MakeFullNode(ctx)
	dbc := getConfig(ctx)
	db := stack.OpenDatabase(dbc)
	return db, dbc.Dir
}

func dbHashChain(ctx *cli.Context) error {
	db, _ := openDBForTool(ctx)
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

	first, next := bal_impl.ReadBALRange(db)
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
	chain, err := bal_impl.BuildHashChain(last, func(n uint64) (common.Hash, error) {
		if time.Since(lastLog) > 30*time.Second {
			lastLog = time.Now()
			fmt.Fprintf(w, "hashed %d/%d blocks\n", n, last)
		}
		return bal_impl.HashOf(db, n)
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
		if file, err = bal_impl.WriteHashChainPackage(o.pkg, genesis, chain, key, o.version); err != nil {
			return err
		}
	} else {
		if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
			return err
		}
		if key != nil {
			sig := bal_impl.SignHashChain(key, genesis, buf.Bytes())
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
		chain *bal_impl.HashChain
		err   error
	)
	if pubHex != "" {
		pub, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(pubHex), "0x"))
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return errors.New("--pub is not a hex ed25519 public key")
		}
		if chain, err = bal_impl.LoadSignedHashChain(path, db.ReadCanonicalHash(0), pub); err != nil {
			return err
		}
		fmt.Fprintf(w, "signature verified for genesis %s\n", db.ReadCanonicalHash(0).Hex())
	} else if chain, err = bal_impl.LoadHashChain(path); err != nil {
		return err
	}
	hash := func(n uint64) (common.Hash, error) { return bal_impl.HashOf(db, n) }
	fmt.Fprintf(w, "hash chain of blocks 0-%d, R_pre %s\n", chain.Last, chain.Root().Hex())

	if block >= 0 {
		n := uint64(block)
		proof, err := bal_impl.BlockProof(n, chain.Last, hash)
		if err != nil {
			return err
		}
		list := &bal.BlockAccessList{}
		if n > 0 {
			if list, err = bal_impl.Read(db, n); err != nil {
				return err
			}
		}
		if err := chain.VerifyBlock(n, list, proof); err != nil {
			return err
		}
		fmt.Fprintf(w, "block %d verified with a proof of %d bytes\n", n, len(proof)*common.HashLength)
		return nil
	}

	first, next := bal_impl.ReadBALRange(db)
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

func dbStat(ctx *cli.Context) error {
	db, _ := openDBForTool(ctx)
	defer db.Close()
	end := ctx.Uint64(dbStatToFlag.Name)
	if end == 0 {
		end = ^uint64(0)
	}
	return balStat(db, ctx.Uint64(dbStatFromFlag.Name), end, os.Stdout)
}

type balStats struct {
	blocks, noNonce, raw, readBytes, noReads                       uint64
	accounts, changed, writes, slotReads, acctChanges, codeChanges uint64
	balances, nonces, deleted, cleared, accountBytes               uint64
	sizes                                                          []int
	all                                                            bytes.Buffer
}

// balStat prints the statistics of the lists stored in db for the blocks
// from to end-1.
func balStat(db database.DBManager, from, end uint64, w io.Writer) error {
	s := new(balStats)
	first, next := bal_impl.ReadBALRange(db)
	fmt.Fprintf(w, "stored lists        blocks %d-%d\n", first, max(next, 1)-1)
	for number := max(from, first); number < min(end, next); number++ {
		enc, err := bal_impl.ReadRaw(db, number)
		if err != nil {
			return err
		}
		list, err := bal.Decode(enc)
		if err != nil {
			return fmt.Errorf("block %d: %w", number, err)
		}
		readsEnc := bal_impl.ReadBALReads(db, db.ReadCanonicalHash(number), number)
		if readsEnc == nil {
			s.noReads++
		} else {
			reads, err := bal.DecodeReads(readsEnc)
			if err == nil {
				list, err = bal.Combine(list, reads)
			}
			if err != nil {
				return fmt.Errorf("block %d: reads: %w", number, err)
			}
		}
		s.add(enc, readsEnc, list)
	}
	return s.print(w)
}

func (s *balStats) add(enc, readsEnc []byte, list *bal.BlockAccessList) {
	s.blocks++
	s.raw += uint64(len(enc))
	s.readBytes += uint64(len(readsEnc))
	s.sizes = append(s.sizes, len(enc))
	s.all.Write(enc)

	nonceChanged := false
	for _, a := range list.Accounts {
		s.accounts++
		if a.Changed() {
			s.changed++
		}
		s.slotReads += uint64(len(a.StorageReads))
		for _, sw := range a.StorageWrites {
			s.writes += uint64(len(sw.Changes))
		}
		s.balances += uint64(len(a.BalanceChanges))
		s.nonces += uint64(len(a.NonceChanges))
		s.acctChanges += uint64(len(a.AccountChanges))
		for _, c := range a.AccountChanges {
			s.accountBytes += uint64(len(c.Account))
		}
		s.codeChanges += uint64(len(a.CodeChanges))
		s.deleted += uint64(len(a.Deleted))
		s.cleared += uint64(len(a.StorageCleared))
		if len(a.NonceChanges) > 0 {
			nonceChanged = true
		}
	}
	if !nonceChanged {
		s.noNonce++
	}
}

func (s *balStats) print(w io.Writer) error {
	if s.blocks == 0 {
		fmt.Fprintln(w, "no blocks")
		return nil
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return err
	}
	compressed := len(enc.EncodeAll(s.all.Bytes(), nil))
	sort.Ints(s.sizes)
	pct := func(p float64) int { return s.sizes[int(float64(len(s.sizes)-1)*p)] }
	per := func(n uint64) float64 { return float64(n) / float64(s.blocks) }

	fmt.Fprintf(w, "blocks              %d (no nonce change: %d)\n", s.blocks, s.noNonce)
	fmt.Fprintf(w, "raw bytes (writes)  %d (%.1f/block)\n", s.raw, per(s.raw))
	fmt.Fprintf(w, "reads bytes         %d (%.1f/block, %d blocks without reads)\n", s.readBytes, per(s.readBytes), s.noReads)
	fmt.Fprintf(w, "zstd (concatenated) %d (%.1f/block, ratio %.2f)\n", compressed, per(uint64(compressed)), float64(s.raw)/float64(compressed))
	fmt.Fprintf(w, "size p50/p90/p99/max %d / %d / %d / %d\n", pct(0.5), pct(0.9), pct(0.99), s.sizes[len(s.sizes)-1])
	fmt.Fprintf(w, "per block: accounts %.2f (changed %.2f), slot writes %.2f, slot reads %.2f\n",
		per(s.accounts), per(s.changed), per(s.writes), per(s.slotReads))
	fmt.Fprintf(w, "per block: balance %.2f, nonce %.2f, whole account %.3f (%.0f B avg), code %.3f, deleted %d, cleared %d\n",
		per(s.balances), per(s.nonces), per(s.acctChanges),
		float64(s.accountBytes)/float64(max(s.acctChanges, 1)), per(s.codeChanges), s.deleted, s.cleared)
	return nil
}
