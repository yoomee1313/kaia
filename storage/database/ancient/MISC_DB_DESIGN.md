# Misc DB: contents, size and a recommended layout

Status: analysis and proposal only, nothing here is implemented. Written on
2026-11-05 against `feat/freezer-poc` at `80a366353`.

The misc DB (`chaindata/misc`, `MiscDB`) holds everything that is not a
header, body, receipt, tx lookup, trie node or snapshot. On a Kairos copy it is
10.7 GB (10 GiB), and 99.7% of that is two per-block items: the total blockscore (TD) and
the bloom bits. Both can be made almost free. Everything else is a few MB.

## 1. Measurement

Copy: `/Volumes/SEAN/chaindata/kairos/klay/chaindata/misc`, a non-pruned full
sync that finished on 2026-11-05, copied before reading. Head 143,190,971 (the
TD count). Counted with `trial/miscstat.go` (one sequential pass, 38 s).
"Disk" is LevelDB's `SizeOf` for the key range of the class.

| Class | Keys | Key bytes | Value bytes | Disk | Disk per block |
|---|---:|---:|---:|---:|---:|
| TD `h`+num+hash+`t` | 143,190,972 | 6.01 GB | 0.70 GB | **6.71 GB** (63%) | 46.8 B |
| Bloom bits `B`+bit+section+hash | 71,593,984 | 3.08 GB | 1.00 GB | **4.02 GB** (37%) | 28.0 B |
| `supplyCheckpoint` | 1,118,680 | 27 MB | 30 MB | 24 MB | |
| `shead` (bloom section heads) | 34,958 | 0.5 MB | 1.1 MB | 1.6 MB | |
| `stakingInfo` | 1,657 | | 3.1 MB | 1.4 MB | |
| `council`+num, vote and gov arrays, migration markers, `klay-config-`, `DatabaseVersion`, `count` | ~300 | | < 0.2 MB | < 0.3 MB | |
| Total | 215,940,529 | 9.12 GB | 1.73 GB | 10.7 GB | 75 B |

`du` reports 10 GiB (= 10.7 GB) for the directory.

Observations:

- **Both large items are dominated by a 32-byte block hash in the key**, not by
  their values. The hash is random, so neither LevelDB's key prefix compression
  nor snappy reduces it: disk ≈ raw bytes. The TD value is 1 to 5 bytes.
- **Absent from this copy:**
  - Pruning marks: the copy is not live-pruned.
  - `vrankCheckpoint`: no Permissionless blocks.
  - Legacy istanbul `snapshot`+hash and legacy `governance`+num: the node synced
    with current code, which migrates from headers.
  - `SenderTxHash`: indexing is off.
- **The 23 GB archive copy (GCP `dev-archive-kairos`) is not explained by this
  copy.** TD and bloom bits for its height account for about 11 GB. The rest is
  most likely legacy istanbul snapshots (one JSON blob per 1024 blocks, no live
  writer any more) and legacy governance keys, which an old node keeps forever.
  To check, run `go run trial/miscstat.go <copy of misc>` on a copy of that DB.
- **Mainnet:** TD and bloom bits grow at about 75 B per block, so misc is about
  75 B × height, e.g. 15 GB at 200M blocks, plus any legacy keys.

## 2. What is in it

### 2.1 Written per block or per period

| Item | Key → value | Cadence | Readers | Deleted | SetHead |
|---|---|---|---|---|---|
| TD | `h`+BE8(num)+hash+`t` → RLP big.Int | every block | insert (parent TD), p2p status, downloader, RPC `totalBlockScore` | only by the freezer for frozen blocks (if TD = num+1) | deleted for rewound blocks |
| Bloom bits | `B`+BE2(bit)+BE8(section)+hash → compressed bitset | 2048 keys per 4096-block section, 256 confirmations behind | `eth_getLogs` | never; after a rewind the old section's keys stay as garbage under the old head hash | only `count` and `shead` roll back |
| `shead` | `shead`+BE8(section) → hash | per section | chain indexer | on reorg | rolled back |
| `supplyCheckpoint` | +BE8(num) → RLP {minted, burnt} | every 128 blocks | `kaia_getTotalSupply` | once by the blob-fee fixup | deleted if num%128 = 0 |
| `vrankCheckpoint` | +BE8(num) → RLP {scores, candidates × reporters matrix} | VRankEpoch/8 blocks, after Permissionless | cold-start score restore | never, though only valid in its own epoch | deleted |
| `stakingInfo` | +LE8(num) → JSON | per staking interval, before Kaia only | reward, RPC, p2p staking-info | never | deleted |
| `council` | +LE8(num) → JSON addresses | per council-changing vote, before Permissionless | council lookup | never | deleted |
| Pruning marks (live pruning) | `Pruning-`+BE8(num)+ExtHash(39) → 0x01 | every trie commit, one per replaced node | prune loop | after retention (172,800 blocks) | prune below last pruned refused |
| `SenderTxHash` (opt-in) | +hash → tx hash | per fee-delegated tx | RPC | never | not rewound |

### 2.2 Single keys (metadata)

All small and rewritten in place:

- `DatabaseVersion`
- `klay-config-`+hash
- `AncientDir`
- `TrieSync`
- `lastPrunedBlockNumber`
- `PruningEnabled`
- `count`
- `lastSupplyCheckpointNumber`
- `supplyBlobFeeFixupDone`
- `vrankLastCheckpoint`
- `governanceVoteDataBlockNums`, `governanceDataBlockNums`, `validatorVoteBlockNums`. Each is one JSON array rewritten whole per insert, and grows with the number of vote blocks, not with height.
- Migration markers `governanceLowestVoteScannedEpochIdx`, `lowestScannedValidatorVoteNum`
- `governanceState`
- `InvalidBlock`
- `chaindatafetcherCheckpoint`
- `databaseDirectory`+n, `migrationStatus`, `migrationOldDBPath`

### 2.3 Legacy, no live writer

- **`snapshot`+hash:** istanbul snapshots. Read only by the valset migration and its fallback; deleted by SetHead.
- **`governance`+num, `governanceIdxHistory`:** read once to seed `governanceDataBlockNums`.

Not in misc: the bridge service keys (`bridgeservice` DB), the state snapshot
journal and generator (snapshot DB), and the chaindatafetcher checkpoint (Kafka
or KAS). The randao, reward, contractgov, system, gasless and auction modules
write nothing.

## 3. Recommendation

| Item | Now | Recommended | Why | Saving on Kairos |
|---|---|---|---|---|
| **TD** | KV, every block | **Compute, don't store**, except the genesis TD and the last `threshold` blocks | Every non-genesis header must have BlockScore 1 (`validateHeader`), so TD(n) = TD(0) + n. Kairos TD(0) is 1. | **−6.7 GB** (keeps ~8 MB) |
| **Bloom bits** | KV, 43-byte key with the section head hash | **KV with an 11-byte key** `B`+bit+section | Kaia has no reorgs, so the hash only separates sections that a rewind abandoned. Without it, a recomputed section overwrites the old one, which also fixes today's garbage after SetHead. | **−2.6 GB** (4.0 → ~1.4) |
| Pruning marks | KV, write and delete one key per node | **Append-only, one item per block, tail-truncated after retention** (freezer table with `TruncateTail`) | Marks are written and consumed strictly in block order. Today each one is a KV put plus a tombstone, and the prune loop has to scan tombstones (the comment at `blockchain.go:1601`). | live-pruned nodes only; not measured here. Size is 56 B × marks per block × retention. |
| Legacy `snapshot`, `governance` | KV, kept forever | **Delete in the background** once the valset and headergov migrations report done | Fully derivable from headers, which the migrations already did | 0 on this copy; probably most of the 23 GB archive gap |
| `vrankCheckpoint` | KV, never pruned | KV, **delete checkpoints of past epochs** | Only valid inside their epoch | small; grows with candidates × reporters |
| `supplyCheckpoint`, `stakingInfo`, `council`, gov and valset arrays, `shead`, metadata | KV | **Keep in KV** | Small; random or rewrite access; module-owned | — |
| Total misc | 10.7 GB | **~1.5 GB** | | **−86%** |

I don't recommend moving the kaiax modules to their own DB. Their data is under
30 MB and their rewind hooks already work. The two items worth taking out of the
LevelDB write path, pruning marks and frozen block data, are block-ordered, and
the freezer already provides that shape. The 11-byte bloom keys were also
weighed against a freezer table of one item per section; I rejected that because
`eth_getLogs` reads 3 to 6 of a section's 2048 vectors, so whole-section items
would read about 300× more data per section.

### 3.1 With BAL, checkpoints and EN history

- **Pruning marks shrink.** Retention currently has to cover the historical state
  that RPC must serve. With past state rebuilt from a checkpoint plus BALs,
  retention can drop to what crash recovery and reorg-free SetHead need
  (thousands of blocks instead of 172,800). The marks shrink by the same factor.
- **`stakingInfo` (before Kaia) and `supplyCheckpoint`** become derivable from
  historical state via BAL. They are too small to be worth dropping.
- **Bloom bits are still needed.** BALs carry state changes, not logs.
  `eth_getLogs` on an EN with history keeps using bloom bits, then frozen
  receipts.
- **TD is not affected.**

### 3.2 Freezer, live pruning and SetHead

- **TD**
  - Computing TD works for frozen and non-frozen blocks alike. The freezer's
    current rule (delete if TD = num+1, rebuild for frozen canonical blocks)
    becomes a special case.
  - SetHead needs nothing.
- **Bloom bits**
  - SetHead already rolls back `count` and `shead`. With the short key, the
    recomputed section overwrites the old keys.
- **Pruning marks**
  - SetHead below the last pruned block is refused today. Above it, the table
    is truncated at the new head, as `TruncateFrozen` does for blocks.

## 4. Migration and compatibility

Each change can be read in both formats and is migrated by a background loop,
like the freeze loop. An item is deleted only after its replacement is
verified.

1. **TD**
   - Read path: try the key; if it is missing and the header exists, return
     TD(0) + n.
   - Write path: keep writing TD for new blocks.
   - Background loop: walk old TD keys and delete each one whose value equals
     TD(0) + n, keeping the last `threshold` blocks.
   - Downgrade: an older binary needs only the TD of its head's parent to keep
     inserting, so it still runs. Its RPC shows no `totalBlockScore` for old
     blocks.
2. **Bloom bits**
   - Read the short key first, then the long one.
   - Background loop, one section at a time: read the 2048 long keys of the
     canonical section, write the short keys, delete the long ones, and record
     the progress section in a misc key. Then compact.
   - Downgrade: an older binary would find no long keys and silently return
     fewer logs. So this change must make the DB one-way, as the freezer does:
     bump `DatabaseVersion`, or reuse the freezer's one-way warning. An
     alternative for downgrade is to make the chain indexer rebuild from
     headers (reset `count`); that is slow but correct.
3. **Pruning marks**
   - New marks go to the append-only table.
   - The prune loop drains the remaining KV marks with the current code for one
     retention window. After that, the KV range is empty and is compacted.
     Nothing is copied.
   - Downgrade: an older binary does not see the table's marks, so those nodes
     are never pruned. That is a space leak, not corruption.
4. **Legacy keys**
   - Delete in the background after the markers show the migrations are done
     (`lowestScannedValidatorVoteNum` = 0, `governanceLowestVoteScannedEpochIdx`
     = 0).
   - Downgrade: an older binary that still reads istanbul snapshots rebuilds
     them from headers.
5. **vrank:** delete past-epoch checkpoints in `PostInsertBlock` at each epoch
   start.

Suggested order, by saving per unit of risk:

1. TD
2. Bloom bits short key
3. Pruning marks table (needs a live-pruned measurement first)
4. Legacy cleanup (needs the archive copy measured first)

## 5. Open items

- Measure pruning marks on a live-pruned Kairos node. The BAL session's
  pruning sync on the internal SSD copy will have one when it finishes; it holds
  the LevelDB lock until then. Also measure the 23 GB archive misc.
- Check every TD reader for an implicit "TD exists ⇒ block is known" use, e.g.
  `HasBlockAndState`-style checks and the downloader's local TD, before computing
  TD for any header.
- `SenderTxHash` grows per fee-delegated tx when enabled. It is a hash lookup and
  belongs in KV, but probably in the tx lookup DB rather than misc.
