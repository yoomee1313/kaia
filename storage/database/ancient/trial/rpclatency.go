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

//go:build ignore

// rpclatency measures the RPC read latency of old (frozen) and recent blocks
// for the block freezer trial. See KAIROS_TRIAL.md.
//
// It has two steps, so that the measured reads are not warmed up by the
// sampling:
//
//	go run rpclatency.go sample  -rpc URL -threshold 172800 -n 300 -out blocks.json
//	go run rpclatency.go measure -rpc URL -blocks blocks.json -out before.json
//	go run rpclatency.go measure -rpc URL -blocks blocks.json -compare before.json -out after.json
//
// sample picks random block numbers below and above the freezer boundary and
// records their hashes and first transaction. measure reads each block with
// kaia_getHeaderByNumber, kaia_getBlockByNumber, kaia_getBlockReceipts and
// kaia_getTransactionReceipt (in this order, so each call mostly measures the
// item it adds), and prints latency percentiles per range and method. With
// -compare it checks that every response equals the earlier run, e.g. the
// run before the blocks were frozen.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

const (
	rangeOld    = "old"
	rangeRecent = "recent"
)

var methods = []string{
	"kaia_getHeaderByNumber",
	"kaia_getBlockByNumber",
	"kaia_getBlockReceipts",
	"kaia_getTransactionReceipt",
}

// sampledBlock is a block picked by sample.
type sampledBlock struct {
	Range  string `json:"range"`
	Number uint64 `json:"number"`
	Hash   string `json:"hash"`
	TxHash string `json:"txHash,omitempty"`
}

type blockList struct {
	RPC      string         `json:"rpc"`
	Head     uint64         `json:"head"`
	Boundary uint64         `json:"boundary"` // Blocks [1, Boundary) are "old"
	Seed     int64          `json:"seed"`
	Created  time.Time      `json:"created"`
	Blocks   []sampledBlock `json:"blocks"`
}

// call is one timed RPC call made by measure.
type call struct {
	Range   string        `json:"range"`
	Number  uint64        `json:"number"`
	Method  string        `json:"method"`
	Latency time.Duration `json:"latencyNs"`
	Digest  string        `json:"digest,omitempty"` // sha256 of the result
	Err     string        `json:"err,omitempty"`
}

type run struct {
	RPC         string        `json:"rpc"`
	Started     time.Time     `json:"started"`
	Elapsed     time.Duration `json:"elapsedNs"`
	Concurrency int           `json:"concurrency"`
	Calls       []call        `json:"calls"`
}

type rpcClient struct {
	url  string
	http *http.Client
	id   int
	mu   sync.Mutex
}

func (c *rpcClient) call(method string, params ...interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	c.id++
	id := c.id
	c.mu.Unlock()

	body, err := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Post(c.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var res struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("%s: HTTP %d: %v", method, resp.StatusCode, err)
	}
	if res.Error != nil {
		return nil, fmt.Errorf("%s: %d %s", method, res.Error.Code, res.Error.Message)
	}
	if len(res.Result) == 0 || string(res.Result) == "null" {
		return nil, fmt.Errorf("%s: empty result", method)
	}
	return res.Result, nil
}

func hexNumber(n uint64) string { return "0x" + strconv.FormatUint(n, 16) }

func parseHexNumber(raw json.RawMessage) (uint64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rpclatency sample|measure [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "sample":
		err = sample(os.Args[2:])
	case "measure":
		err = measure(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newClient(url string) *rpcClient {
	return &rpcClient{url: url, http: &http.Client{Timeout: 60 * time.Second}}
}

func sample(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ExitOnError)
	var (
		url       = fs.String("rpc", "http://127.0.0.1:8551", "JSON-RPC endpoint")
		threshold = fs.Uint64("threshold", 172800, "freezer threshold; the boundary is head - threshold + 1")
		boundary  = fs.Uint64("boundary", 0, "first block that is not old (overrides -threshold)")
		n         = fs.Int("n", 300, "number of blocks per range")
		withTxs   = fs.Bool("with-txs", true, "only pick blocks with at least one transaction")
		tries     = fs.Int("tries", 50, "attempts per block to find one with a transaction")
		seed      = fs.Int64("seed", time.Now().UnixNano(), "random seed")
		out       = fs.String("out", "blocks.json", "output file")
	)
	fs.Parse(args)

	c := newClient(*url)
	raw, err := c.call("kaia_blockNumber")
	if err != nil {
		return err
	}
	head, err := parseHexNumber(raw)
	if err != nil {
		return err
	}
	b := *boundary
	if b == 0 {
		if head < *threshold+2 {
			return fmt.Errorf("head %d is not above the threshold %d", head, *threshold)
		}
		b = head - *threshold + 1
	}
	if b < 2 || b > head {
		return fmt.Errorf("boundary %d must be in [2, head %d]", b, head)
	}
	list := blockList{RPC: *url, Head: head, Boundary: b, Seed: *seed, Created: time.Now().UTC()}
	rnd := rand.New(rand.NewSource(*seed))
	seen := make(map[uint64]bool)
	for _, r := range []struct {
		name     string
		from, to uint64 // [from, to)
	}{{rangeOld, 1, b}, {rangeRecent, b, head + 1}} {
		picked, misses := 0, 0
		for picked < *n {
			blk, ok, err := pickBlock(c, rnd, r.from, r.to, *withTxs, *tries, seen)
			if err != nil {
				return err
			}
			if !ok {
				misses++
				if misses > *n {
					return fmt.Errorf("could not find enough %s blocks with transactions; retry with -with-txs=false", r.name)
				}
				continue
			}
			blk.Range = r.name
			list.Blocks = append(list.Blocks, blk)
			picked++
		}
		fmt.Printf("sampled %d %s blocks in [%d, %d)\n", picked, r.name, r.from, r.to)
	}
	fmt.Printf("head %d, boundary %d, seed %d\n", head, b, *seed)
	return writeJSON(*out, list)
}

// pickBlock picks a random block in [from, to) that was not picked before,
// with a transaction if withTxs is set.
func pickBlock(c *rpcClient, rnd *rand.Rand, from, to uint64, withTxs bool, tries int, seen map[uint64]bool) (sampledBlock, bool, error) {
	for i := 0; i < tries; i++ {
		number := from + uint64(rnd.Int63n(int64(to-from)))
		if seen[number] {
			continue
		}
		raw, err := c.call("kaia_getBlockByNumber", hexNumber(number), false)
		if err != nil {
			return sampledBlock{}, false, err
		}
		var block struct {
			Hash         string   `json:"hash"`
			Transactions []string `json:"transactions"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return sampledBlock{}, false, fmt.Errorf("block %d: %v", number, err)
		}
		if withTxs && len(block.Transactions) == 0 {
			continue
		}
		seen[number] = true
		blk := sampledBlock{Number: number, Hash: block.Hash}
		if len(block.Transactions) > 0 {
			blk.TxHash = block.Transactions[0]
		}
		return blk, true, nil
	}
	return sampledBlock{}, false, nil
}

func measure(args []string) error {
	fs := flag.NewFlagSet("measure", flag.ExitOnError)
	var (
		url         = fs.String("rpc", "http://127.0.0.1:8551", "JSON-RPC endpoint")
		blocksFile  = fs.String("blocks", "blocks.json", "block list written by sample")
		compareFile = fs.String("compare", "", "earlier measure output to compare responses and latency with")
		concurrency = fs.Int("concurrency", 1, "number of blocks read in parallel")
		out         = fs.String("out", "", "output file (optional)")
	)
	fs.Parse(args)

	var list blockList
	if err := readJSON(*blocksFile, &list); err != nil {
		return err
	}
	var before *run
	if *compareFile != "" {
		before = new(run)
		if err := readJSON(*compareFile, before); err != nil {
			return err
		}
	}

	c := newClient(*url)
	// Shuffle so that old and recent reads are interleaved and the disk cache
	// is not warmed by neighbouring reads.
	order := rand.New(rand.NewSource(list.Seed)).Perm(len(list.Blocks))
	jobs := make(chan sampledBlock)
	results := make(chan []call)
	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for blk := range jobs {
				results <- readBlock(c, blk)
			}
		}()
	}
	r := run{RPC: *url, Started: time.Now().UTC(), Concurrency: *concurrency}
	go func() {
		for _, i := range order {
			jobs <- list.Blocks[i]
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	done := 0
	for calls := range results {
		r.Calls = append(r.Calls, calls...)
		if done++; done%100 == 0 {
			fmt.Fprintf(os.Stderr, "read %d/%d blocks\n", done, len(list.Blocks))
		}
	}
	r.Elapsed = time.Since(r.Started)

	fmt.Printf("%d blocks (head %d, boundary %d), concurrency %d, %v, %.1f calls/s\n",
		len(list.Blocks), list.Head, list.Boundary, r.Concurrency, r.Elapsed.Round(time.Millisecond),
		float64(len(r.Calls))/r.Elapsed.Seconds())
	printSummary(&r, before)
	var mismatch error
	if before != nil {
		mismatch = compareDigests(&r, before)
	}
	if *out != "" {
		if err := writeJSON(*out, r); err != nil {
			return err
		}
	}
	return mismatch
}

// readBlock reads one block with every method and times each call.
func readBlock(c *rpcClient, blk sampledBlock) []call {
	calls := make([]call, 0, len(methods))
	for _, method := range methods {
		var params []interface{}
		switch method {
		case "kaia_getHeaderByNumber", "kaia_getBlockReceipts":
			params = []interface{}{hexNumber(blk.Number)}
		case "kaia_getBlockByNumber":
			params = []interface{}{hexNumber(blk.Number), true}
		case "kaia_getTransactionReceipt":
			if blk.TxHash == "" {
				continue
			}
			params = []interface{}{blk.TxHash}
		}
		start := time.Now()
		raw, err := c.call(method, params...)
		ca := call{Range: blk.Range, Number: blk.Number, Method: method, Latency: time.Since(start)}
		if err == nil && method == "kaia_getBlockByNumber" {
			err = checkHash(raw, blk.Hash)
		}
		if err != nil {
			ca.Err = err.Error()
		} else {
			sum := sha256.Sum256(raw)
			ca.Digest = hex.EncodeToString(sum[:])
		}
		calls = append(calls, ca)
	}
	return calls
}

// checkHash fails if the block is not the one that was sampled.
func checkHash(raw json.RawMessage, hash string) error {
	var block struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return err
	}
	if !strings.EqualFold(block.Hash, hash) {
		return fmt.Errorf("block hash %s, sampled %s", block.Hash, hash)
	}
	return nil
}

type statKey struct{ rng, method string }

type stat struct {
	lat  []time.Duration
	errs int
}

func collect(r *run) map[statKey]*stat {
	stats := make(map[statKey]*stat)
	for _, ca := range r.Calls {
		k := statKey{ca.Range, ca.Method}
		s := stats[k]
		if s == nil {
			s = new(stat)
			stats[k] = s
		}
		if ca.Err != "" {
			s.errs++
			continue
		}
		s.lat = append(s.lat, ca.Latency)
	}
	for _, s := range stats {
		sort.Slice(s.lat, func(i, j int) bool { return s.lat[i] < s.lat[j] })
	}
	return stats
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted))+0.5) - 1
	i = max(0, min(i, len(sorted)-1))
	return sorted[i]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d)/float64(time.Millisecond)) }

func printSummary(r, before *run) {
	stats := collect(r)
	var prev map[statKey]*stat
	if before != nil {
		prev = collect(before)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	header := "range\tmethod\tn\terr\tp50 ms\tp90 ms\tp99 ms\tmax ms\t"
	if prev != nil {
		header += "before p50\tbefore p99\t"
	}
	fmt.Fprintln(w, header)
	for _, rng := range []string{rangeOld, rangeRecent} {
		for _, method := range methods {
			s := stats[statKey{rng, method}]
			if s == nil {
				continue
			}
			line := fmt.Sprintf("%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t", rng, strings.TrimPrefix(method, "kaia_"), len(s.lat), s.errs,
				ms(percentile(s.lat, 0.5)), ms(percentile(s.lat, 0.9)), ms(percentile(s.lat, 0.99)), ms(percentile(s.lat, 1)))
			if prev != nil {
				if p := prev[statKey{rng, method}]; p != nil {
					line += fmt.Sprintf("%s\t%s\t", ms(percentile(p.lat, 0.5)), ms(percentile(p.lat, 0.99)))
				} else {
					line += "-\t-\t"
				}
			}
			fmt.Fprintln(w, line)
		}
	}
	w.Flush()
}

// compareDigests fails if a response differs from the earlier run.
func compareDigests(r, before *run) error {
	type key struct {
		number uint64
		method string
	}
	prev := make(map[key]string, len(before.Calls))
	for _, ca := range before.Calls {
		if ca.Err == "" {
			prev[key{ca.Number, ca.Method}] = ca.Digest
		}
	}
	same, differ, missing := 0, 0, 0
	for _, ca := range r.Calls {
		d, ok := prev[key{ca.Number, ca.Method}]
		switch {
		case !ok || ca.Err != "":
			missing++
		case d == ca.Digest:
			same++
		default:
			differ++
			fmt.Printf("MISMATCH block %d %s\n", ca.Number, ca.Method)
		}
	}
	fmt.Printf("responses: %d same, %d different, %d not comparable (error in either run)\n", same, differ, missing)
	if differ > 0 {
		return errors.New("some responses differ from the earlier run")
	}
	return nil
}

func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func readJSON(path string, v interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
