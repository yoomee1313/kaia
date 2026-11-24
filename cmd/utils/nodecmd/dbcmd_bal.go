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
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"syscall"

	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/blockchain/state"
	"github.com/kaiachain/kaia/blockchain/vm"
	"github.com/kaiachain/kaia/cmd/utils"
	"github.com/kaiachain/kaia/node"
	"github.com/kaiachain/kaia/node/cn"
	"github.com/kaiachain/kaia/storage/database"
	"github.com/urfave/cli/v2"
)

var (
	regenFromFlag = &cli.Uint64Flag{
		Name:  "from",
		Usage: "First block (0 = the block after the stored lists, or 1 from genesis)",
	}
	regenToFlag = &cli.Uint64Flag{
		Name:  "to",
		Usage: "Last block (0 = the head, or the block before the stored lists)",
	}
	regenSourceFlag = &cli.StringFlag{
		Name:  "source",
		Usage: "Where the lists come from: local (execute from the stored states), genesis (execute from the genesis state) or import (apply the lists of another chaindata, see --import)",
		Value: regenLocal,
	}
	regenModeFlag = &cli.StringFlag{
		Name:  "mode",
		Usage: "write (store the lists), verify (compare them with the stored ones) or reads (store the reads of stored lists)",
		Value: regenWrite,
	}
	regenImportFlag = &cli.PathFlag{
		Name:  "import",
		Usage: "Chaindata of another node to import the lists from, for the import source",
	}
	regenHashChainFlag = &cli.PathFlag{
		Name:  "hash-chain",
		Usage: "Signed pre-Amber BAL hash chain, a file or the directory of its package, that checks imported lists (default: --bal.hash-chain)",
	}
	regenMemoryFlag = &cli.Uint64Flag{
		Name:  "memory",
		Usage: "MiB of the heap the regeneration may grow by (0 = 4096)",
	}
	regenWorkersFlag = &cli.IntFlag{
		Name:  "workers",
		Usage: "Workers executing blocks (0 = from the memory and the cores)",
	}
	regenStepFlag = &cli.Uint64Flag{
		Name:  "step",
		Usage: "Blocks between stored states (0 = the trie commit interval)",
	}
	regenFlags = []cli.Flag{
		regenFromFlag, regenToFlag, regenSourceFlag, regenModeFlag, regenImportFlag,
		regenHashChainFlag, regenMemoryFlag, regenWorkersFlag, regenStepFlag,
	}
)

// balToolNetworkFlags name the network of the chain database, whose genesis
// the node is made with, mainnet unless set, like for the node itself.
var balToolNetworkFlags = []cli.Flag{utils.NetworkIdFlag, utils.MainnetFlag, utils.KairosFlag}

var (
	dbRegenCommand = &cli.Command{
		Name:   "regen",
		Usage:  "Regenerate the block access lists of past blocks",
		Action: utils.MigrateFlags(dbRegen),
		Flags:  append(append(regenFlags, balToolNetworkFlags...), utils.SnapshotFlags...),
		Description: `
db regen [--from N] [--to M] [--source local|genesis|import] [--mode write|verify|reads]
regenerates the block access lists of blocks N to M of a history node:
executing them again from the stored states (local) or from the genesis
state (genesis), or applying the lists in the chaindata of another node
(import, with --import DIR), whose every segment is checked against the
signed BAL hash chain. write stores the lists, verify compares them with
the stored ones, and reads stores the reads of stored lists that only have
their writes. Blocks are executed with the kaiax modules of the node.`,
	}
	dbBuildTrieCommand = &cli.Command{
		Name:      "build-trie",
		Usage:     "Build the state trie of a past block from the BAL history",
		ArgsUsage: "<number>",
		Action:    utils.MigrateFlags(dbBuildTrie),
		Flags:     append(balToolNetworkFlags, utils.SnapshotFlags...),
		Description: `
db build-trie N
builds the state trie of block N of a history node from the stored trie of a
later block and the BAL history, so that proofs, dumps and storage iteration
work at the block, and keeps it from live pruning.`,
	}
)

// balTools runs the BAL tools on a chain: the regeneration of block access
// lists and the build of past state tries. Closing quit stops them.
type balTools struct {
	bc       *blockchain.BlockChain
	states   *state.Manager
	db       database.DBManager
	vmConfig vm.Config
	quit     chan struct{}
}

func newBALTools(bc *blockchain.BlockChain, db database.DBManager, vmConfig vm.Config) *balTools {
	return &balTools{bc: bc, states: bc.States(), db: db, vmConfig: vmConfig, quit: make(chan struct{})}
}

// openBALTools makes the CN of the node, in history mode, without its network,
// to run the BAL tools on its chain, which an interrupt stops. The returned
// function stops the CN.
func openBALTools(ctx *cli.Context) (*balTools, *cn.Config, func(), error) {
	stack, cfg := utils.MakeConfigNode(ctx)
	cfg.CN.HistoryMode = database.HistoryModeBAL
	cfg.CN.WorkerDisable = true
	cfg.CN.TxPoolDisable = true
	sctx := node.NewServiceContext(&cfg.Node, make(map[reflect.Type]node.Service), stack.EventMux(), stack.AccountManager())
	full, err := cn.New(sctx, &cfg.CN)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := full.StartOffline(); err != nil {
		full.StopOffline()
		return nil, nil, nil, err
	}
	bc, ok := full.BlockChain().(*blockchain.BlockChain)
	if !ok {
		full.StopOffline()
		return nil, nil, nil, errors.New("the node has no block chain")
	}
	t := newBALTools(bc, full.ChainDB(), bc.VMConfig())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-sigs:
			logger.Info("Got interrupt, stopping")
			close(t.quit)
		case <-stopped:
		}
	}()
	return t, &cfg.CN, func() {
		signal.Stop(sigs)
		close(stopped)
		full.StopOffline()
	}, nil
}

func dbRegen(ctx *cli.Context) error {
	t, cnConfig, stop, err := openBALTools(ctx)
	if err != nil {
		return err
	}
	defer stop()
	cfg := regenConfig{
		From:          ctx.Uint64(regenFromFlag.Name),
		To:            ctx.Uint64(regenToFlag.Name),
		Source:        ctx.String(regenSourceFlag.Name),
		Mode:          ctx.String(regenModeFlag.Name),
		Memory:        ctx.Uint64(regenMemoryFlag.Name) << 20,
		Workers:       ctx.Int(regenWorkersFlag.Name),
		Step:          ctx.Uint64(regenStepFlag.Name),
		SkipReads:     cnConfig.BALSkipReads,
		ImportDir:     ctx.Path(regenImportFlag.Name),
		HashChainFile: ctx.Path(regenHashChainFlag.Name),
	}
	if cfg.Source == regenImport && cfg.HashChainFile == "" {
		cfg.HashChainFile = cnConfig.BALHashChain
	}
	s, err := t.regen(cfg)
	if err != nil {
		return err
	}
	if s.Error != "" {
		return errors.New(s.Error)
	}
	fmt.Printf("Regenerated the block access lists of blocks %d-%d (%s, %s): %d mismatches\n", s.From, s.Next-1, s.Source, s.Mode, s.Mismatches)
	return nil
}

func dbBuildTrie(ctx *cli.Context) error {
	if ctx.Args().Len() != 1 {
		return errors.New("usage: db build-trie <number>")
	}
	number, err := strconv.ParseUint(ctx.Args().First(), 10, 64)
	if err != nil {
		return fmt.Errorf("block number %q: %w", ctx.Args().First(), err)
	}
	t, _, stop, err := openBALTools(ctx)
	if err != nil {
		return err
	}
	defer stop()
	s, err := t.buildTrie(number)
	if err != nil {
		return err
	}
	fmt.Printf("Built the state trie of block %d, root %s, from the trie of block %d\n", s.Number, s.Root.Hex(), s.From)
	return nil
}
