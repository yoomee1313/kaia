# Block freezer PoC

This proof of concept moves old canonical block data (headers, bodies, receipts)
out of the key-value databases into append-only flat files that can live on a
cheaper disk (HDD). It is a port of go-ethereum's chain freezer
(`core/rawdb`, master as of 2026-10-21). It does not affect consensus.

## Design

**Package `storage/database/ancient`**
- `freezer.go`, `freezer_table.go`, `freezer_batch.go`, `freezer_meta.go` and
  `freezer_utils*.go` are geth's files with small changes: the `ethdb` ancient
  interfaces are defined locally (`AncientReaderOp`, `AncientWriteOp`), logging
  uses the Kaia module logger, and metrics use `rcrowley/go-metrics` (the size
  gauge is a `Counter`). The LGPL headers are kept and each file names its
  geth source.
- The geth table tests are ported. The BAL tests and the `ancienttest` suite are
  left out because Kaia has no BAL tables and no memory freezer.
- The chain freezer has four tables, all keyed by block number:
  `hashes` (raw, no compression), `headers`, `bodies` and `receipts` (snappy).
  Items are stored exactly as they are in the key-value store (header RLP, body
  RLP, `[]*ReceiptForStorage` RLP), so moving them needs no re-encoding.
  Total difficulty is not frozen.

**DBManager integration** (`storage/database/db_manager_freezer.go`)
- `NewDBManager` opens the freezer when `DBConfig.EnableFreezer` is set, at
  `<AncientDir>/chain` (default `<chaindata>/ancient/chain`).
- Reads check the key-value store first and the freezer only after a miss, so
  recent blocks cost one extra branch and nothing else. The existing caches stay
  in front. Changed reads: `ReadCanonicalHash`, `HasHeader`, `ReadHeader`,
  `ReadHeaderRLP`, `HasBody`, `ReadBody`, `ReadBodyRLP`, `ReadBodyRLPByHash`,
  `ReadReceipts`, `ReadReceiptsByBlockHash` (and everything built on them, such
  as `ReadBlock` and `ReadReceipt`).
- Hash-keyed reads use geth's `isCanon` check: a frozen item is returned only
  if the frozen canonical hash at that number equals the requested hash.
- Numbers at or above the freezer head never touch the freezer. A failed read
  of a frozen item (e.g. the freezer disk is gone) is logged as an error and
  counted in `ancient/readerror`, and the item is reported as missing.
- Writes are unchanged; new blocks still go to the key-value store.
- Kept in the key-value store: the genesis block, the `H`+hash to number
  mapping (needed by `ReadHeaderNumber`), total difficulty, and tx lookup
  entries.
- Read-only mode (`DBConfig.FreezerReadOnly`) is for offline tools: if blocks
  were frozen, the freezer is opened read-only from `--datadir.ancient` or the
  directory recorded in MiscDB, without `--db.freezer` and without a freeze
  loop. `ken init` and the `snapshot` subcommands use it, and accept
  `--datadir.ancient` for a moved freezer.
- `db-migration` opens the source freezer read-only and, after the key-value
  copy, copies the freezer files into `<dst datadir>/ancient/chain` and records
  that directory in the destination MiscDB. A single-DB, DynamoDB or memory
  destination is refused up front when the source has frozen blocks.
- Chain export reads frozen blocks through the DBManager. Import into a new
  node writes to the key-value store and the loop freezes the blocks later;
  import into a node that froze them skips them, because `HasBlock` sees the
  freezer (`TestFreezer_ExportImport`).
- `DBManager.FrozenBlocks()` returns how many blocks are frozen (0 when the
  freezer is disabled).

**Freeze loop** (a goroutine started by `NewDBManager`, stopped first in `Close`)
- Every minute it freezes canonical blocks up to `head - threshold`, at most
  30,000 per round. When a round is full it starts the next one right away,
  like geth.
- Order per round: append to the freezer, fsync the freezer, record the freezer
  directory in MiscDB, delete the blocks from the key-value stores in one batch
  per DB, then delete any side chain blocks at the frozen heights. Every 30,000
  frozen blocks (outside the freezer lock) the frozen key ranges of the
  header, body and receipts DBs are compacted, so the space is freed. A range
  compaction flushes the memtable, so it is not run after every small round.
  In a local test with 3,000 small blocks the key-value size went from 3.8 MB
  to 1.7 MB with the compaction, and grew to 4.2 MB without it (tombstones).
- A missing header, body or receipts entry stops the round with an error log.
  Nothing is deleted in that case.
- Metrics, under `klay/db/chaindata/ancient/`: `frozen` (gauge, frozen
  blocks), `lag` (gauge, blocks below the threshold that are not frozen yet),
  `freeze/time` (timer, one sample per round), `compact/time` (timer, one
  sample per compaction), `readerror` (counter), and the freezer's own `read`,
  `write` (meters) and `size` (counter).

**Crash safety and misuse checks**
- On open, a batch that was fsynced but not yet deleted from the key-value
  store (a crash between the two steps) is found by walking down from the
  freezer head, checked against the frozen hashes, and deleted. A torn or
  unsynced freezer write is repaired by the freezer tables themselves (geth
  logic), and the key-value data is still there because it is deleted only
  after the fsync.
- The genesis hash in the freezer must match the key-value genesis.
- Once blocks have been frozen, MiscDB stores the freezer directory
  (`AncientDir` key). Opening the database without the freezer, or with an empty
  freezer at another path, fails. Otherwise the node would start with a hole in
  its chain. A freezer moved to a new path (e.g. to an HDD) is accepted if its
  genesis matches and the key-value store continues where it ends: block
  `frozen` must be in the key-value store and its parent must be the last frozen
  block. An older copy of the freezer fails that check instead of leaving a
  hole in the chain.

**Rewind (SetHead)**: rewinding below the freezer head is **refused** with an
error (`Cannot rewind below the frozen blocks`), in the same way the existing
live-pruning guard refuses rewinds. `SetHead` pauses the freeze loop
(`DBManager.PauseFreezer`) from the check until the rewind is done, so a
running batch cannot freeze the blocks the rewind deletes. The rewind is also
refused when the target block has no state and the search for a block with
state would go below the freezer head; without that check `HeaderChain.SetHead`
would follow the search down and delete the total difficulty and other
key-value data of frozen blocks. Truncating the freezer would also work, but
then a mistaken `debug_setHead` could silently throw away immutable history,
and the threshold already covers realistic recovery rewinds. Repair rewinds at
startup (`setHeadBeyondRoot(..., repair=true)`) only move head markers and
delete nothing, so they are not affected.

**Threshold default: 172,800 blocks (2 days).** With instant BFT finality the
threshold does not need to cover reorg depth, as geth's 90,000 blocks
(about 12.5 days at 12 s) do. Instead it sets (1) how far `SetHead` may rewind
and (2) which recent blocks stay on the fast disk, where most RPC reads
(receipts, logs, recent blocks) land. Two days covers state-recovery rewinds,
which are bounded by the trie commit interval (128 blocks by default), with
plenty of margin, and gives an operator a weekend to react before blocks
leave the key-value store. geth's ~25-hour equivalent felt too tight for that.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--db.freezer` | off | Enable the freezer and the freeze loop (experimental). |
| `--db.freezer.threshold` | `172800` | Number of recent blocks kept in the key-value DBs. At least 128 and twice `--state.block-interval`, so that a crash recovery never has to rewind into the freezer; the node refuses to start otherwise. |
| `--datadir.ancient` | `<chaindata>/ancient` | Freezer root. A relative path is resolved against the chaindata directory. Tables go into `<root>/chain`. |

Environment variables: `KAIA_DB_FREEZER`, `KAIA_DB_FREEZER_THRESHOLD`,
`KAIA_DATADIR_ANCIENT`. The flags are also available in the TOML config through
`altsrc`.

## Trying it on a Kairos chaindata copy

Use a **copy**. Once blocks are frozen, the chaindata cannot be opened without
the freezer.

```bash
make ken    # or: go build -o build/bin/ken ./cmd/ken
```

1. Record the sizes and some reference reads before you start:
   ```bash
   du -sh /data/kairos/chaindata/{header,body,receipts}
   ```
   With the node running, save `kaia.getBlock(N)`, `kaia.getBlockReceipts(hash)`
   and a few `kaia.getTransactionReceipt(tx)` results for old blocks
   (e.g. N = 1, 1000000, head - 200000).
2. Start the node with the freezer and, ideally, the ancient root on the HDD.
   Peers are not needed to freeze the local chain:
   ```bash
   ./build/bin/ken --kairos --datadir /data/kairos \
     --db.freezer --datadir.ancient /hdd/kairos-ancient \
     --nodiscover --maxconnections 0
   ```
3. Watch the logs for `Opened block freezer` and then one
   `Froze chain segment from=… to=… elapsed=…` line per 30,000-block batch.
   The first catch-up freezes everything from genesis up to head - 172,800, so
   it runs thousands of back-to-back batches. The elapsed time per batch gives
   the migration speed. A shorter `--db.freezer.threshold` freezes more for a
   quick trial.
4. Check reads: repeat the reference reads from step 1 and compare. Also try
   `debug.setHead` to a frozen block, which should return an error.
5. Restart with the same flags. The freezer resumes where it stopped. Restarting
   without `--db.freezer` should fail with
   `old blocks were moved to the freezer at …`.
6. Compare `du -sh chaindata/{header,body,receipts}` with
   `du -sh /hdd/kairos-ancient`. The loop compacts every 30,000 frozen blocks,
   so no manual `debug.chaindbCompact` is needed.

To try an interrupted batch, kill the process (`kill -9`) during a
`Froze chain segment` round and restart. Look for
`Deleting leftover frozen blocks from key-value store` if the kill happened
between the fsync and the delete.

## Not handled yet

- **`--db.single`**, **DynamoDB**, **MemoryDB**, **secondary RocksDB**: the
  freezer is disabled with a warning. Secondary RocksDB is excluded because the
  primary holds the freezer's exclusive file lock.
- **Freezer truncation**: SetHead below the freezer head is refused, not
  supported.
- **Dangling side chains** above the freezer head (geth deletes them) are not
  followed. Kaia's BFT finality makes side chains essentially non-existent.
- **Tail pruning / history expiry and era files**: the tail-group code is
  ported but unused.
- **Per-type disk placement**: `--datadir.ancient` is a stop-gap until the
  shared tiered-storage module provides placement.
- **Observed, unrelated to the freezer**: `ReadBodyRLP` and `ReadBodyRLPByHash`
  have an inverted `err != nil` check when re-encoding a cached body, and the
  header cache ignores the block number.
