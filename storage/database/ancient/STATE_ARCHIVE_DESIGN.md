# State archive in the freezer (design draft)

Goal: keep historical state readable (archive node behaviour) while the
key-value state DB on SSD holds only the live state and a recent window, as a
live-pruning node does today. Old trie nodes go to append-only freezer files on
HDD. Consensus is not affected.

Status: Phase 1 PoC implemented (section 7 lists where the code differs from
this design). Numbers marked *(estimate)* must still be measured (see
"Phase 0").

## 1. What live pruning already gives us

Live pruning (KIP-111) is the basis. It changes how trie nodes are stored:

- **Every node has a unique key.** With pruning enabled, the hasher attaches a
  fresh 7-byte counter to every node hash except the account trie root
  (`storage/statedb/hasher.go` `hashData`). The KV key is
  `ExtHash = hash(32) || counter(7)`. Two equal nodes in different places are
  stored twice, so a node is never shared and can be deleted without reference
  counting.
- **A node's content never changes.** A changed node gets a new ExtHash. The old
  ExtHash is never used again by a newer trie version.
- **The counter grows with time.** `extHashLastCounter` starts at
  `UnixNano() >> 8` when the process starts and is incremented atomically
  (`common/types.go` `nextExtHashCounter`). So a counter value identifies a node
  and also roughly when it was created. Children are hashed before their parent,
  so a child's counter is lower than its parent's.
- **Parents store the full ExtHash of their children** (`unextendNode` with
  `preserveExtHash`). A storage root inside an account leaf is an ExtHash too.
  A reader always holds the ExtHash it needs.
- **Pruning marks** (`Pruning-` + block + ExtHash in MiscDB) record the block at
  which each node was replaced. `pruneTrieNodeLoop` deletes nodes whose mark is
  older than `head - LivePruningRetention` (default 172,800). This works in
  archive mode too: `writeStateTrie` sends every block to the prune loop.
- All state reads that miss memory go through one call:
  `Database.node` / `Database.Node` → `DBManager.ReadTrieNode(ExtHash)`.

Live pruning can only be turned on before block 1 (`node/cn/backend.go`). An
existing hash-keyed archive DB (zero counters, shared nodes) cannot use any of
the designs below without a resync.

## 2. Where to copy nodes into the freezer

Three placements were considered.

| | A. Move at prune time | B. Append at commit time (recommended) | C. Move to a cold KV at prune time |
|---|---|---|---|
| What the freezer holds | Dead nodes only | Every node ever committed (live ones too) | Dead nodes in an LSM on HDD |
| Item order | By death block | By counter (= creation order) | n/a |
| Lookup ExtHash → location | Needs a per-node index: ~15 B × all dead nodes on SSD *(estimate: ~2 TB for a 35 TB state archive)* | Binary search on counters: small sparse index *(estimate: a few GB on disk, a few MB in RAM)* | LSM key lookup, 2–3 HDD seeks |
| Extra write work | Prune loop must read each node back from KV before deleting it | One sequential append per commit | LSM compaction on HDD, write amplification 10–30× |
| Crash ordering | Append + fsync, write index, then delete from KV | Append + fsync before the KV batch | Same as A |
| SetHead below the prune window | Revived nodes sit in the freezer; needs care | Nothing to do (append-only history) | Same as A |
| History expiry (drop states older than X) | Easy: truncate the tail | Not possible without rewriting | Range delete |
| Uses existing pruning code | Changes the prune step | Prune loop unchanged (still deletes) | Changes the prune step |

**Recommendation: B.** A is the literal "move instead of delete", but because
nodes die in a different order from the one they are created in, a dead node's
location is arbitrary and needs its own index entry. That index alone would be
several percent of the archive and must sit on SSD. B stores nodes in counter
order, so the counter in the ExtHash already says where the node is. The prune
loop stays as it is. The cost is a second copy of the live state on HDD
(a full node's live state, small next to the history). C was the earlier idea
in the proposal doc; it fails on HDD during catch-up sync because of compaction
traffic.

A stays a fallback if history expiry turns out to be a requirement.

## 3. Design B

### 3.1 Freezer instance

A second freezer instance at `<AncientDir>/state`, next to the block freezer
(`<AncientDir>/chain`), using the exported table config (commit f55b35da6). All
tables advance together, one item per **chunk**:

| Table | Compression | Item |
|---|---|---|
| `nodes` | snappy | A chunk: nodes sorted by counter (format below) |
| `first` | raw, 8 bytes | Counter of the first node in the chunk (big endian) |
| `block` | raw, 8 bytes | Block number of the commit that produced the chunk |

Chunk format (target ~64 KB before compression *(to tune)*):

```
count(uvarint)
repeat count times:  counter(7) | hash(32) | len(uvarint) | rlp
```

The full ExtHash is stored so that a lookup can check the hash, and so that a
chunk can be rebuilt or verified without other data.

No change to the freezer package API is needed, so `feat/bas-style-diff` (which
imports the package) is not affected.

### 3.2 Write path

Hook in `statedb.Database` where nodes leave memory for the KV store:
`writeBatchNodes` (Commit) and `Cap`.

1. Collect the `(ExtHash, rlp)` pairs that the call will write. Today
   `writeBatchNodes` streams them into the KV batch and may flush part of the
   batch early (`WriteBatchesOverThreshold`). It has to collect first.
2. Sort the pairs by counter. Pairs with a zero counter (account trie roots), and
   pairs whose counter is not above the last archived counter, go to the
   **exception index** (3.4). The rest are cut into chunks.
3. Append the chunks to the state freezer and fsync.
4. Write the KV batch as today.

Order 3 → 4 makes sure a node is never in KV without being in the archive. A
crash between 3 and 4 leaves archived nodes that are never referenced (the block
is processed again with new counters). They waste space and do no harm.

Other node writers (`WriteTrieNode`, state sync with zero counters, state
migration) are not covered. The node refuses to start with the archive when
state migration is configured. Snap sync is already disabled.

In archive mode (`--gcmode archive`) every block is committed, so every block's
state is readable later. In full mode only the committed states (every
`--state.block-interval` blocks) are complete. A state between them is rebuilt by
re-execution as today, or later from BAL.

### 3.3 Read path

`DBManager.ReadTrieNodeFromOld` falls back to the archive after a KV miss, as
the block freezer does for blocks:

1. KV `Get` (unchanged). A hit returns.
2. Unless the counter is zero, find the chunk: binary search on an in-memory
   sample of `first` (every 1024th chunk *(to tune)*), then on the matching
   8 KB slice of the `first` table. Read the chunk, find the counter, check the
   32-byte hash, return the RLP.
3. Otherwise (zero counter, or not in that chunk), exception index `Get`, then
   the same chunk read.

The chunk search comes first because exceptions are rare, so the common read
needs no extra KV lookup.

Expected cost on HDD *(estimate)*: 2–3 seeks per node, about 10 nodes per
account lookup, so ~0.2–0.3 s for a cold historical `getBalance`. Putting the
small freezer index files and `first` on SSD cuts this to about one seek per
node. Archive reads go into the clean trie cache like KV reads. If history
queries push recent nodes out of that cache, archive reads may need their own
cache.

`HasState(root)` and `StateAt(root)` work for old roots without change, because
the root is found through the exception index.

### 3.4 Exception index

KV entries `StateArchiveNode-` + ExtHash → chunk number (8 bytes), in MiscDB or
a dedicated small DB. It holds:

- account trie roots (zero counter): one entry per committed block *(estimate:
  ~10 GB at 200 M blocks)*. Keying roots by block number instead would remove
  most of this but needs the block number in `ReadTrieNode`.
- nodes that arrive out of counter order. Within one commit there are none,
  because the batch is sorted. Across commits there should be none when blocks
  are committed one after another. A clock going backwards between restarts
  could cause some, and so can `Cap`, which flushes nodes in insertion order
  rather than counter order. Phase 0 measures the rate
  (`statearchive/exception`).

Exceptions are stored in the same chunks as the in-order nodes of their
append; they sort first because their counters are lower. The `first` value
of a chunk is the counter of its first in-order node. A chunk that holds
exceptions only gets the highest archived counter plus one. So `first` never
decreases, and an in-order node is always in the last chunk whose `first` is
not above its counter. An exception that is already indexed (e.g. the root of
a block that changed no state) is not stored again.

### 3.5 Metadata and checks (MiscDB)

- `StateArchiveDir`: the directory, recorded when the archive is first opened.
  Opening a DB that has one without the archive fails, the same rule as the
  block freezer. Offline tools (`FreezerReadOnly`) open it read-only.
- The archive can only be turned on before the genesis block is written
  (`ken init --state.archive`, or the first start). A node that was committed
  before would never be archived, and a later state that still uses it would
  lose it to pruning. So there is no `StateArchiveStart`: the archive holds
  every state from the genesis.
- `StateArchiveChunks`: the chunk count, written with the exception index
  after each append. An archive with fewer chunks (an older copy, or an empty
  directory) is refused. More chunks are fine (a crash after the fsync).
- `StateArchiveSample-` + chunk number: the first counter of every
  `archiveSampleInterval`-th chunk, written with the exception index. On open,
  the samples are loaded from there as far as they go, the last one is checked
  against the freezer, and the rest are read from the freezer and recorded.
  The last counter is rebuilt from the last chunk.
- The exception index, the samples and the chunk count are written to MiscDB
  without a sync of their own, so an OS crash can lose them while the nodes
  stay in KV. On open, the chunks from the recorded count on are reindexed: an
  exception is a node whose counter is zero or below its chunk's `first`.
- On open, the freezer repairs torn appends itself. Nothing else has to be
  repaired, because KV writes come after the archive append.

### 3.6 Rewind (SetHead)

Nothing is truncated. A rewind to a block inside the prune window works as
today. A rewind below it now finds the old state in the archive, and the
blockchain continues from there. Nodes that come back to life are read from the
archive. When they die again, the prune loop deletes a KV key that does not
exist, which is a no-op. Chunks written for the abandoned blocks stay as unused
data.

### 3.7 Flags

| Flag | Meaning |
|---|---|
| `--state.archive` | Enable the state archive. Requires a live-pruning DB. |
| `--datadir.ancient` | Shared with the block freezer. Tables go into `<root>/state`. |

The block freezer threshold and `--state.live-pruning-retention` remain
separate settings. Both default to 172,800.

### 3.8 Metrics

`klay/db/statearchive/`: `append/time`, `append/bytes`, `read/time`,
`read/miss`, `exception/size`, `chunks`.

## 4. Not handled / open points

- **History expiry**: in B, an old chunk still holds nodes that are alive, so
  the tail cannot be dropped. If expiry is required, use A, or rewrite the log
  from time to time (copy live nodes forward).
- **ExtHash overhead**: a live-pruning archive is larger than a hash-keyed
  archive. Each child reference is 7 bytes longer (up to 112 bytes per full
  node), and identical subtrees are no longer shared. Phase 0 measures the ratio.
  Shortening the counters at freeze time is possible later, as a change to the
  storage format only.
- **Existing mainnet/Kairos archives** are hash-keyed, so they need a resync
  (or a block replay through `ken import`) with live pruning and the archive on.
- **Hot-path latency**: one fsync per committed block on HDD *(estimate 5–20
  ms)*. If this is too slow: append without waiting and fsync in the
  background. On open, walk the tries of blocks after the last durable chunk and
  re-append nodes whose counter is above the last archived one. Because a child's
  counter is lower than its parent's, the walk can skip any subtree whose counter
  is at or below that point.
- **Clock skew**: correctness does not depend on counter order (exceptions and
  the hash check cover it). Only the size of the exception index does.

## 5. Relation to BAL (`feat/bas-style-diff`)

BAL plus a per-key index gives a much smaller history and fast sync, but it
cannot answer historical proofs (`eth_getProof`) or any read that walks a trie.
It also needs a new historical reader. The node archive keeps the existing trie
read path exactly (proofs included) at about the size of an archive, on HDD.
The two do not exclude each other: the node archive at a checkpoint interval
(full mode, interval K) plus BAL for the blocks in between is the
"checkpoint + BAL" path from the proposal doc.

## 6. Plan

**Phase 0: measure** (local chain under load, then a Kairos block replay with
`ken import` into a fresh live-pruning datadir, first N million blocks):

- nodes and bytes committed per block, average node size;
- out-of-order rate across commits (exception index growth);
- size of the live-pruning archive vs the hash-keyed archive over the same range;
- fsync cost per commit on the target HDD.

**Phase 1: PoC** (on this branch):

- state freezer instance and chunk codec;
- write hook in `writeBatchNodes` / `Cap` (collect, sort, append, then KV);
- read fallback in `ReadTrieNodeFromOld`, exception index, MiscDB metadata,
  misuse checks;
- `--state.archive` flag;
- tests:
  - a state read at a block whose nodes were pruned from KV returns the same
    result as before pruning, for account and storage tries;
  - `eth_getProof` at an archived block;
  - a crash between append and KV write;
  - reopen after a crash;
  - SetHead below the prune window;
  - a counter that goes backwards (forced with `ResetExtHashCounterForTest`).

**Phase 2**: background fsync with repair walk, freezer index files on SSD,
an archive read cache, and the `ken import` trial on Kairos.

## 7. Phase 1 PoC

Code: `storage/database/state_archive.go` (chunks, append, lookup),
`storage/database/db_manager_state_archive.go` (open, checks, DBManager
methods), the hook in `storage/statedb/database.go` (`writeArchivedNodes`,
used by `writeBatchNodes` and `Cap`), and the `--state.archive` flag
(`ken` and `ken init`). The node refuses to start with the archive on a
database without live pruning. State migration already refuses live pruning.

Where it differs from the design above:

- `db-migration` copies the archive files into `<dst datadir>/ancient/state`
  and records that directory in the destination MiscDB, which already holds
  the exception index, the samples and the chunk count. A single-DB, DynamoDB
  or memory destination is refused up front.

- Read order and exception storage: see 3.3 and 3.4.
- `block` holds the block of the `Commit`, or, for a `Cap` flush, the block of
  the last `Commit` before it.
- SetHead below the prune window is allowed when the archive is open
  (`BlockChain.SetHead`). Pruning marks of the abandoned blocks may later
  delete nodes the new chain still uses from KV; the archive still has them.

Tests: `storage/database/state_archive_test.go` (chunk format, in-order and
out-of-order appends over many chunks and sample groups, reopen, a crash
between the archive append and the KV write, a MiscDB that lost the index,
recorded samples, migration, misuse checks) and
`blockchain/blockchain_state_archive_test.go` (live pruning with retention 3
over 20 blocks; every state, account and storage trie, is read back after
its roots were deleted from KV; a merkle proof at a pruned block; a counter
reset backwards in the middle of the chain; SetHead below the prune window
and new blocks on top).

Not handled yet:

- One fsync per append is on the commit path (every block in archive mode).
- No read cache for chunks, and no placement of `first` on SSD.
- Other node writers (`WriteTrieNode`, the stack trie used by sync) bypass the
  archive. The node refuses to start with the archive unless `--syncmode` is
  `full`, because fast sync writes the pivot state without it. Snap sync is
  disabled.
- Found on the way, not caused by the archive: with live pruning,
  `Trie.Prove` writes the nodes below the root under their ExtHash, so
  `VerifyProof` with that proof DB fails (eth_getProof only returns the node
  list, so RPC is not affected). `doGetProof` opens the storage trie with the
  unextended storage root, which looks wrong for a live-pruning DB.
