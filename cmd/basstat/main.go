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

// basstat summarizes the block access set files written by --bas.record-dir:
// sizes (raw and zstd) and per-block counts of accounts, slots and changes.
// It is an experimental tool.
//
//	basstat [-from N] [-to M] <bas-record-dir>
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/bas"
	"github.com/kaiachain/kaia/rlp"
	"github.com/klauspost/compress/zstd"
)

type stats struct {
	blocks, noNonce, raw                                           uint64
	accounts, changed, writes, slotReads, acctChanges, codeChanges uint64
	balances, nonces, deleted, cleared, accountBytes               uint64
	sizes                                                          []int
	all                                                            bytes.Buffer
}

func main() {
	from := flag.Uint64("from", 0, "first block")
	to := flag.Uint64("to", ^uint64(0), "end block (exclusive)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: basstat [-from N] [-to M] <bas-record-dir>")
		os.Exit(2)
	}
	files, err := filepath.Glob(filepath.Join(flag.Arg(0), "bas-*.rlp"))
	if err != nil {
		fatal(err)
	}
	sort.Strings(files)

	s := new(stats)
	for _, f := range files {
		if err := s.readFile(f, *from, *to); err != nil {
			fatal(err)
		}
	}
	s.print()
}

func (s *stats) readFile(path string, from, to uint64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	stream := rlp.NewStream(file, 0)
	for {
		var rec blockchain.BASFileRecord
		if err := stream.Decode(&rec); err == io.EOF {
			return nil
		} else if err != nil {
			// The file may be appended to by a running node.
			fmt.Fprintln(os.Stderr, "stop at partial record:", path, err)
			return nil
		}
		if rec.Number < from || rec.Number >= to {
			continue
		}
		set, err := bas.Decode(rec.Set)
		if err != nil {
			return fmt.Errorf("block %d: %w", rec.Number, err)
		}
		s.add(rec.Set, set)
	}
}

func (s *stats) add(enc []byte, set *bas.BlockAccessSet) {
	s.blocks++
	s.raw += uint64(len(enc))
	s.sizes = append(s.sizes, len(enc))
	s.all.Write(enc)

	nonceChanged := false
	for _, a := range set.Accounts {
		s.accounts++
		if a.Changed() {
			s.changed++
		}
		s.slotReads += uint64(len(a.StorageReads))
		for _, w := range a.StorageWrites {
			s.writes += uint64(len(w.Changes))
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

func (s *stats) print() {
	if s.blocks == 0 {
		fmt.Println("no blocks")
		return
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		fatal(err)
	}
	compressed := len(enc.EncodeAll(s.all.Bytes(), nil))
	sort.Ints(s.sizes)
	pct := func(p float64) int { return s.sizes[int(float64(len(s.sizes)-1)*p)] }
	per := func(n uint64) float64 { return float64(n) / float64(s.blocks) }

	fmt.Printf("blocks              %d (no nonce change: %d)\n", s.blocks, s.noNonce)
	fmt.Printf("raw bytes           %d (%.1f/block)\n", s.raw, per(s.raw))
	fmt.Printf("zstd (concatenated) %d (%.1f/block, ratio %.2f)\n", compressed, per(uint64(compressed)), float64(s.raw)/float64(compressed))
	fmt.Printf("size p50/p90/p99/max %d / %d / %d / %d\n", pct(0.5), pct(0.9), pct(0.99), s.sizes[len(s.sizes)-1])
	fmt.Printf("per block: accounts %.2f (changed %.2f), slot writes %.2f, slot reads %.2f\n",
		per(s.accounts), per(s.changed), per(s.writes), per(s.slotReads))
	fmt.Printf("per block: balance %.2f, nonce %.2f, whole account %.3f (%.0f B avg), code %.3f, deleted %d, cleared %d\n",
		per(s.balances), per(s.nonces), per(s.acctChanges),
		float64(s.accountBytes)/float64(max(s.acctChanges, 1)), per(s.codeChanges), s.deleted, s.cleared)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
