# Block freezer trial on a Kairos archive copy

This runbook measures the block freezer (`--db.freezer`, see `../POC_NOTES.md`)
on a copy of the `dev-archive-kairos` chaindata (11.1 TB pd-balanced,
`asia-northeast3-b`). It answers three questions:

1. **Size**: how much of the key-value store moves to the freezer disk.
2. **Freeze speed**: how long the first catch-up takes (about 231 M Kairos
   blocks at the time of writing), and whether the HDD-class disk is the limit.
3. **Read latency**: RPC reads of frozen blocks on Hyperdisk Throughput,
   compared with the same reads from the key-value store on pd-balanced.

It also checks that frozen reads return the same data and that the misuse checks
work.

**Always work on a copy.** Once blocks are frozen, the chaindata cannot be
opened without the freezer, and an older binary cannot open it at all. The
trial node runs isolated (no peers), so its head does not move and the freezer
boundary is known in advance.

## 0. Plan and cost

| Item | Choice |
|---|---|
| Trial VM | N2, same zone, same memory as the dev node or close to it. N2 supports both pd-balanced and Hyperdisk Throughput. The VM's own disk limits also cap Hyperdisk performance; check them for the shape you pick. |
| KV disk | Clone of the dev node's data disk, pd-balanced, same size (11.1 TB). |
| Freezer disk | Hyperdisk Throughput. Size it from the header, body and receipts sizes measured in step 3, plus 20 % (min 2 TiB, max 32 TiB). |
| Freezer throughput | Start at 90 MiB/s per TiB (the maximum). Hyperdisk Throughput has no IOPS setting: it gives 4 IOPS per provisioned MiB/s, and random RPC reads are IOPS-bound. Step 7 repeats the reads at a lower throughput. |

On list prices (`$0.006/GiB-month` plus `$0.285` per MiB/s-month), a 4 TiB disk at
360 MiB/s costs about $25 + $103 a month. The trial runs for a few days, so
delete the disks when you are done (step 9).

## 1. Prepare the disks and the VM

```bash
export ZONE=asia-northeast3-b
export SRC_VM=dev-archive-kairos
export TRIAL_VM=kairos-freezer-trial

# Find the data disk of the dev node (the non-boot disk).
gcloud compute instances describe $SRC_VM --zone $ZONE \
  --format='table(disks[].deviceName, disks[].source.basename(), disks[].boot)'
export SRC_DISK=<data disk name>
```

Record the dev node's command line and config. The trial must use the same
database flags (`--dbtype`, `--db.num-statetrie-shards`, LevelDB compression,
`--gcmode`, `--state.block-interval`, ...), or the copy will not open the
same way:

```bash
gcloud compute ssh $SRC_VM --zone $ZONE -- 'ps -o args= -C ken; cat /etc/kend/conf/kend.conf'
```

For a clean copy, stop ken on the dev node for the clone (a few minutes). A
clone of a running disk is crash-consistent, which LevelDB recovers from too:

```bash
gcloud compute disks create kairos-trial-kv --zone $ZONE \
  --source-disk $SRC_DISK --type pd-balanced
# Size: header + body + receipts from step 3 plus 20 %, at least 2 TiB.
gcloud compute disks create kairos-trial-ancient --zone $ZONE \
  --type hyperdisk-throughput --size 4TB --provisioned-throughput 360
gcloud compute instances create $TRIAL_VM --zone $ZONE \
  --machine-type n2-highmem-16 --image-family ubuntu-2404-lts-amd64 --image-project ubuntu-os-cloud \
  --boot-disk-size 200GB \
  --disk name=kairos-trial-kv,device-name=kv \
  --disk name=kairos-trial-ancient,device-name=ancient
```

You can also skip step 3's sizing and create the freezer disk after step 3,
then attach it with `gcloud compute instances attach-disk`.

On the VM, mount the clone at the same path as on the dev node. MiscDB keeps a
few absolute paths, so this avoids surprises. Format and mount the freezer disk:

```bash
sudo mkdir -p /var/kend/data /hdd
sudo mount /dev/disk/by-id/google-kv /var/kend/data      # use the dev node's mount point
sudo mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0 /dev/disk/by-id/google-ancient
sudo mount -o noatime /dev/disk/by-id/google-ancient /hdd
sudo mkdir -p /hdd/ancient && sudo chown -R $USER /hdd/ancient
```

The freezer refuses a symlinked directory. Pass the real path with
`--datadir.ancient`; do not symlink `chaindata/ancient` to the HDD.

## 2. Build

```bash
sudo apt-get install -y build-essential git sysstat
# Go version from go.mod (1.25.3 at the time of writing).
curl -sL https://go.dev/dl/go1.25.3.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
export PATH=$PATH:/usr/local/go/bin
git clone -b feat/freezer-poc https://github.com/yoomee1313/kaia.git && cd kaia
make ken
go build -o build/bin/rpclatency storage/database/ancient/trial/rpclatency.go
```

## 3. Baseline (freezer off)

Set the common flags once. Copy the database flags from the dev node and
add the isolation, RPC and metrics flags:

```bash
export DATADIR=/var/kend/data
export CHAIN=$DATADIR/klay/chaindata
export DBFLAGS="<database flags of the dev node>"
export KEN="./build/bin/ken --kairos --datadir $DATADIR $DBFLAGS \
  --nodiscover --maxconnections 0 \
  --rpc --rpcaddr 127.0.0.1 --rpcport 8551 --rpcapi kaia,debug \
  --metrics --prometheus --prometheusport 61001"

# Start and stop by PID, so that nothing else named ken is touched.
start() { log=$1; shift; nohup $KEN "$@" > ~/ken-$log.log 2>&1 & echo $! > ~/ken.pid; }
stop() { kill -INT $(cat ~/ken.pid); while kill -0 $(cat ~/ken.pid) 2>/dev/null; do sleep 1; done; }
```

If another service on the VM already uses 8551, 61001 or the P2P port 32323,
pick other ports (`--rpcport`, `--prometheusport`, `--port`). A node that
cannot bind the metrics port still runs, but exports no metrics.

Record the sizes:

```bash
sudo du -s --block-size=1M $CHAIN/* | tee ~/size-before.txt
df -BG $DATADIR | tee -a ~/size-before.txt
```

A disk made from another disk can be slower on the first read of each block.
Read the block data once so that the baseline is not slowed down by that:

```bash
sudo find $CHAIN/header $CHAIN/body $CHAIN/receipts -type f -exec cat {} + > /dev/null
```

Start the node and pick the blocks. The boundary is `head - threshold + 1`,
which is where the freezer will stop because the head does not move:

```bash
start baseline
./build/bin/rpclatency sample -threshold 172800 -n 300 -seed 1 -out ~/blocks.json
```

Measure cold (node restarted and page cache dropped), then warm:

```bash
cold() { stop; sudo sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches'; start "$@"; sleep 60; }
cold baseline-cold
./build/bin/rpclatency measure -blocks ~/blocks.json -out ~/before-cold.json | tee ~/before-cold.txt
./build/bin/rpclatency measure -blocks ~/blocks.json -out ~/before-warm.json | tee ~/before-warm.txt
stop
```

Compare responses only against the same node. A load-balanced endpoint such
as the public Kairos RPC returns the committed seals in `extraData` in a
different order on each backend, so the digests differ there.

## 4. Freeze

```bash
start freeze --db.freezer --datadir.ancient /hdd/ancient
grep -m1 'Opened block freezer' ~/ken-freeze.log
iostat -dxm 60 > ~/iostat-freeze.txt &
```

Each round freezes 30,000 blocks and logs
`Froze chain segment from=… to=… elapsed=…`. Every 30,000 blocks the frozen
key ranges are compacted. Sample the metrics now and then. The freeze rate is
the difference in `ancient_frozen` over the elapsed time:

```bash
m() { curl -s localhost:61001/metrics | grep -E '^klaytn_klay_db_chaindata_ancient_(frozen|lag|size|freeze_time|freeze_time_0_5|freeze_time_0_99|compact_time|readerror|write) '; }
while sleep 600; do date -u +%FT%TZ; m; done > ~/metrics-freeze.txt &
```

The metrics appear a few seconds after the start. Timers are in nanoseconds; `ancient_write` is bytes per second (1-minute rate).
In `iostat-freeze.txt`, `%util` and `wMB/s` of the freezer disk against the
`rMB/s` of the KV disk tell which side limits the speed. Record the
`elapsed` of the first rounds early on: at 10 s per round the catch-up of
about 7,700 rounds takes about 21 hours.

The catch-up is done when `ancient_lag` is 0 and no new `Froze chain segment`
lines appear.

**Interrupted batch (optional, once).** Run `kill -9 $(cat ~/ken.pid)` during a round,
restart with the same `start` command and look for
`Deleting leftover frozen blocks from key-value store` (if the kill came between
the fsync and the delete) and then normal rounds again.

## 5. After the freeze

```bash
sudo du -s --block-size=1M $CHAIN/* | tee ~/size-after.txt
du -s --block-size=1M /hdd/ancient/chain | tee -a ~/size-after.txt
ls -la /hdd/ancient/chain | tee -a ~/size-after.txt
```

The compaction in the loop frees most of the space. If `header`, `body` or
`receipts` are still much larger than the blocks that stay in the key-value
store, compact once and measure again, so that the result shows what the
loop leaves behind as well as what a full compaction gives:

```bash
# "allbutstate" compacts header, body and receipts only. The default preset
# would also compact the archive state trie, which takes far longer.
curl -s -H 'content-type: application/json' localhost:8551 \
  --data '{"jsonrpc":"2.0","id":1,"method":"debug_chaindbCompact","params":[{"preset":"allbutstate"}]}'
```

If curl returns a timeout, the compaction may still be running. Watch `du`
until the sizes stop changing.

Measure reads with the freezer enabled, cold and warm, and compare with the
baseline. `measure` exits non-zero and prints `MISMATCH` if a response
changed:

```bash
KEN="$KEN --db.freezer --datadir.ancient /hdd/ancient"
cold after
./build/bin/rpclatency measure -blocks ~/blocks.json -compare ~/before-cold.json -out ~/after-cold.json | tee ~/after-cold.txt
./build/bin/rpclatency measure -blocks ~/blocks.json -compare ~/before-warm.json -out ~/after-warm.json | tee ~/after-warm.txt
./build/bin/rpclatency measure -blocks ~/blocks.json -concurrency 16 | tee ~/after-c16.txt
```

The `old` rows are frozen blocks and the `recent` rows stay in the key-value
store, so `recent` should not change. Within a block, the calls run in the
order header, block, block receipts, transaction receipt. Each call is
served partly from what the previous one cached, so it mostly measures the
item it adds: hash and header, then body, then receipts.

Also note the restart time: the time from start to `Opened block freezer` in
`ken-after.log`.

## 6. Misuse checks

Run these with the node from step 5 (freezer enabled):

```bash
stop

# Starting without the freezer must fail with
# "old blocks were moved to the freezer at /hdd/ancient/chain".
./build/bin/ken --kairos --datadir $DATADIR $DBFLAGS --nodiscover --maxconnections 0 2>&1 | tail -3

# Moving the freezer (e.g. to another disk) must work: the freezer at the new
# path is accepted because the key-value store continues where it ends.
sudo mkdir -p /hdd/moved && sudo chown $USER /hdd/moved && mv /hdd/ancient /hdd/moved/ancient
start moved --db.freezer --datadir.ancient /hdd/moved/ancient
grep -m1 'Opened block freezer' ~/ken-moved.log   # want: frozen=<boundary>
./build/bin/rpclatency measure -blocks ~/blocks.json -compare ~/before-warm.json | tail -1
stop
```

## 7. Lower throughput (optional)

Read latency of frozen blocks depends on the provisioned throughput, because
IOPS do too. Lower it, wait until the change is applied, and repeat the cold
measurement from step 5 (GCP limits how often performance can be changed;
check the current Hyperdisk docs):

```bash
gcloud compute disks update kairos-trial-ancient --zone $ZONE --provisioned-throughput 100
gcloud compute disks describe kairos-trial-ancient --zone $ZONE --format='value(provisionedThroughput)'
```

## 8. Rewind into the freezer (last)

A rewind below the freezer head truncates the freezer and deletes the blocks
above the target, so run it after every other measurement. Pick a target about
100,000 blocks below the boundary:

```bash
start rewind --db.freezer --datadir.ancient /hdd/moved/ancient   # the path from step 6
curl -s -H 'content-type: application/json' localhost:8551 \
  --data '{"jsonrpc":"2.0","id":1,"method":"debug_setHead","params":["<target as hex>"]}'
grep -E 'Rewinding blockchain|Truncated block freezer' ~/ken-rewind.log   # want: to=<target + 1>
stop; start rewind2 --db.freezer --datadir.ancient /hdd/moved/ancient
grep -m1 'Opened block freezer' ~/ken-rewind2.log   # want: frozen=<target + 1>
./build/bin/rpclatency measure -blocks ~/blocks.json -compare ~/before-warm.json | tail -1
stop
```

The compare must show no `MISMATCH`. Blocks above the target are gone and
count as not comparable. Record how long the rewind took.

## 9. Results to report

| Metric | Value |
|---|---|
| Head, boundary, frozen blocks | |
| `header` / `body` / `receipts` before → after (MiB), with and without the manual compaction | |
| Freezer size (`/hdd/ancient/chain`) and per table | |
| Catch-up time, blocks/s, mean and p99 `freeze_time`, mean `compact_time` | |
| Bottleneck (`iostat`: freezer `%util`/`wMB/s`, KV `rMB/s`) | |
| Read p50 / p99 per method, `old` and `recent`, cold and warm, before and after | |
| Same at concurrency 16, and at the lower throughput | |
| `MISMATCH` count (want 0), `ancient_readerror` (want 0) | |
| Restart time with the freezer | |
| Start without the freezer refused? `debug_setHead` into the freezer: time, `frozen` = target + 1 after a restart, 0 mismatches? | |
| Monthly cost: KV pd-balanced at the new size + freezer disk (capacity + throughput) vs. 11.1 TB pd-balanced today | |

The KV savings show up in the bill only after the data moves to a smaller
pd-balanced disk; a disk cannot be shrunk. The size after the freeze is the
input for that.

## 10. Clean up

```bash
gcloud compute instances delete $TRIAL_VM --zone $ZONE
gcloud compute disks delete kairos-trial-kv kairos-trial-ancient --zone $ZONE
```

## Local dry run

Steps 3 to 6 and 8 were run on 2026-10-28 and 2026-10-29 on a laptop, using a one-validator local
chain (homi, 908 blocks, about 3,400 transfers) copied to a `ken` datadir
with `--db.freezer.threshold 300`. The page cache was not dropped (macOS):
- Freezing worked in two stages: threshold 600, then `kill -9`, then a
  restart with threshold 300. The rounds froze blocks 0-308 and 309-608 in
  about 64 ms each.
- All 1,200 responses were equal before and after the freeze. Frozen-block
  reads were within 0.05 ms of the key-value reads at p50.
- `frozen`=609, `lag`=0 and `readerror`=0, and `size` matched `du`.
- A start without `--db.freezer` and a start with an empty freezer at
  another path were refused with the documented errors.
- `debug_setHead` to block 256 (2026-10-29, after SetHead support was added)
  truncated the freezer from 609 to 257 blocks. After a restart, `frozen` was
  257, block 257 was gone, and the 252 responses for blocks up to 256 were
  unchanged.
- A freezer moved to another directory opened and served the same data.

The loop's range compaction (every 30,000 frozen blocks) is not reached at
that size. `TestFreezer_Compaction` covers it with a lowered interval
(1,000 blocks: 1.26 MB before, 76 KB after, 1.40 MB without the compaction).
Step 4 is its first run at scale, so check `ancient_compact_time` and the KV
sizes there.
