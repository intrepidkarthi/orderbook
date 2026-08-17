# Benchmarks

Performance of the core library, measured with Go's benchmark tooling. Not marketing —
the harness is in-repo so anyone can reproduce them.

> **These are not tracked as regression targets, and this page used to say they were.**
> `.github/workflows/bench.yml` runs `go test -bench` and pipes the output into the run
> summary. There is no stored baseline, no `benchstat` comparison, and no condition that
> can fail a build. The only performance facts in this repository that a test can fail
> on are the three allocation ratios in `pkg/orderbook/alloc_test.go`
> (`TestCancelIsAllocationFree`, `TestCancelReplaceIsAllocationFree`,
> `TestAddAloneDoesAllocate`) — which is why those three are asserted as *ratios against
> a measured baseline* rather than as absolute numbers. Building the missing half — a
> committed tape, a recorded machine configuration and a comparison that can fail — is
> [`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) M10.

## Reproduce

```sh
make bench
# or:
go test -run '^$' -bench=. -benchmem ./pkg/orderbook/ ./pkg/matching/
# the durable path (Runner + EventSink + WAL):
go test -run '^$' -bench=. -benchmem ./pkg/wal/
```

CI also runs these on every push and publishes the numbers to the
[**Benchmarks** workflow](https://github.com/intrepidkarthi/orderbook/actions/workflows/bench.yml)
run summary (neutral GitHub-hosted hardware).

## Results

Apple M4, macOS 26.5, `go1.23.5 darwin/arm64`, single-threaded (`-benchmem`).
**Median of 5 runs** on an otherwise idle machine — a single run on a laptop is not
a number worth publishing, and the tail percentiles below are the most
load-sensitive figures here.

> **"Otherwise idle" is load-bearing, and here is what it costs to ignore it.** A
> re-measurement on 2026-08-11 ran on the same hardware with a load average near 6
> (a window server, a storage indexer and two browsers competing). Medians came out
> 10–25% above the figures below, in both directions across neighbouring rows, and
> an interleaved A/B against older code produced a 24% difference on one run that a
> second interleaved run put at 2%. **None of that was publishable, and none of it
> was evidence of anything** — the variance exceeded the effect being looked for.
> The allocation figures were unaffected, because counting allocations is not a
> timing. If you cannot quiesce the machine, measure allocations and leave the
> nanoseconds alone.

int64 ticks/lots, pooled book nodes/levels, single-writer engine:

| Benchmark | ns/op | ~ops/sec | B/op | allocs/op |
|-----------|------:|---------:|-----:|----------:|
| `OrderBook_BestBid` (top-of-book read) | 5.8 | ~170 M | 0 | 0 |
| `OrderBook_Cancel` (drain) | 273‡ | ~3.7 M | 46† | ~0† |
| `OrderBook_CancelReplace` (MM churn, 10 K book) | 172 | ~5.8 M | 1† | ~0† |
| `OrderBook_LevelChurn` (new price level) | 292 | ~3.4 M | 0 | 0 |
| `Engine_MatchInto` (`Match`, maker+taker+trade) | 329 | ~3.0 M | **0** | **0** |
| `Engine_Match` (`Process` convenience wrapper) | 419 | ~2.4 M | 296 | 4 |

Run-to-run spread is ±10% on most rows and wider on `CancelReplace`, which varied
120–212 ns across the five runs. Treat these as an order of magnitude with a shape,
not as constants.

‡ **That figure is a ten-million-order book, and the benchmark does not say so.**
`OrderBook_Cancel` inserts `b.N` orders and cancels all of them, so `b.N` is also
the book size — and Go chooses `b.N` by wall-clock, which on this machine lands
around 10 M. The number is therefore mostly cache behaviour at a depth no real
symbol reaches:

| resting orders | ns/op |
|---:|---:|
| 200,000 | 65 |
| 1,000,000 | 114 |
| 5,000,000 | 179 |
| 10,000,000 | 255–273 |

**At a plausible book depth, cancel is ~65 ns.** Both numbers are real; the 273 is
the pessimistic end of a scaling curve rather than a typical cost, and quoting it
without the book size — as this page did — understates the engine by 4×. The same
applies to `OrderBook_Add` (92 ns at 200 K, 206 ns at 10 M). It does not apply to
`CancelReplace` (fixed 10,000-order book) or `LevelChurn`, whose working sets are
constant, which is why those two barely move.

The general lesson, and the reason this is called out rather than quietly fixed:
for any book-level benchmark, **the book size is a parameter of the result.** A
library that benchmarks a 1,000-order book will look an order of magnitude faster
than one that benchmarks 10 M, with no difference in the code.

† **`0 allocs/op` in Go's output means "under 1.0", not "none".** The figure is
integer division, so a path allocating 0.99 objects per operation prints as `0` —
which is why `Cancel` can report 46 B/op and `0 allocs/op` on the same line. The
real ratios, measured against `runtime.MemStats` in `pkg/orderbook/alloc_test.go`:

| Path | allocs/op (measured directly) |
|---|---:|
| Cancel (`Remove`) | **0.0002** — 44 allocations across 200,000 cancels |
| Cancel + replace (MM steady state) | **0.0000** |
| `Add` into a growing book | **2.01** |

Two of those three were wrong on this page until 2026-08-11, and the `Add` row was
wrong in the flattering direction — it read **1.05** against a measured 2.01, so the
page understated the cost of growing a book by half. Cancel + replace read 0.009
against a measured 0.0000, understating the engine instead. Both are deterministic
allocation counts rather than timings, so neither was a machine artifact; they were
simply not re-read after the code moved. Checked against the pre-session tree to
confirm the numbers are stale rather than regressed, and both are asserted by
`pkg/orderbook/alloc_test.go`, which prints them on every run — the figures were
sitting in the test log the whole time.

So cancel and market-maker churn are allocation-free in substance, not just after
rounding — churn measures a flat zero. `Add` on its own is not, and at two
allocations per order rather than one it is further from free than this page used to
say. That is what "pooled" means rather than "allocation-free": the pool only pays
back once a release has put something in it, and a book that only grows never
releases anything.
Those three ratios are asserted by tests, so the claim can fail rather than merely
being printed.

**Tail latency** — `BenchmarkLatency_CancelHeavy`, a ~90%-cancel / 10%-new mix
against a warm book: **p50 83 ns · p99 167 ns · p999 250 ns**, 0 allocs/op. Five of
six runs reported exactly those three values; the sixth gave p99 208 / p999 334.
Under a concurrent test suite the same benchmark reports p999 417, which is worth
stating because it is what an unquiesced machine will show you.

### Tail latency by scenario

One scenario was published before — a cancel-heavy mix — and it is the *friendliest*
one. Each row below states its preload, because for a book-level operation the book
size is part of the result. Median of one run each, `-benchtime=200000x`, same machine.

| Scenario | preload | p50 | p99 | p99.9 | p99.99 |
|---|---:|---:|---:|---:|---:|
| `CancelOnly` — drain a preloaded book | 201 K | **83** | 209 | 833 | 3,541 |
| `CancelHeavy` — 90% cancel / 10% new | 20 K | **83** | 292 | 958 | — |
| `Mixed_70_20_10` — submit / cancel / aggress | 20 K | 125 | 875 | 2,834 | 12,750 |
| `ThinBook` — one order per level, 10-level walk | 5 K | 125 | 584 | 2,750 | 11,792 |
| `AddOnly` — passive insertion into a growing book | 20 K | 167 | 1,167 | 2,875 | 21,875 |
| `AggressiveWalk` — 5-lot takers sweeping levels | 20 K | 416 | 1,958 | 6,875 | 31,250 |

All figures ns, from one `-benchtime=200000x` run each so the rows are comparable to
each other. The `CancelHeavy` row therefore differs from the median-of-five figure
quoted above it (p99 167 / p999 250): more samples reach further into the tail, and a
fixed 200,000-iteration run is a different measurement from whatever Go's default
timing chooses. Both are real; neither is the other's correction.

**Publishing only the cancel-heavy scenario understated the tail by
roughly 30×**: its p99.9 is 958 ns, and an aggressive sweep's is 6,875 ns with a
p99.99 of 31 µs. Neither is alarming for a Go engine — but a venue writes its SLO on
p99.9, and the previously published number was the best of six.

Self-trade prevention, all five modes, never measured before and all cheap:

| STP mode | p50 | p99 | p99.9 |
|---|---:|---:|---:|
| `ALLOW` | 125 | 875 | 958 |
| `CANCEL_NEWEST` | 83 | 167 | 750 |
| `CANCEL_OLDEST` | 125 | 833 | 958 |
| `CANCEL_BOTH` | 83 | 208 | 834 |
| `DECREMENT` | 84 | 209 | 875 |

**The mass cancel is the one to know about.** Pulling a single account's 5,000 resting
orders takes **~872 µs at p50 and ~1.26 ms at p99** — and it runs on the matching
goroutine, so for that whole time no other participant's orders are processed. The
kill switch is a venue-wide pause proportional to the account's book: budget roughly
0.9 ms per 5,000 orders, and note that it scales, so a 100,000-order account is on the
order of 18 ms.

Re-measured after trading phases and DAY/GTD expiry were added to the engine: all five
p50 figures are identical and the upper quantiles are within run-to-run noise, so the
work those features added to the hot path — a length check and a field comparison, both
guarded at the call site — does not show up here.

`MassCancelBurst` is excluded from that refresh because each of its iterations rebuilds
a 5,000-order book: the timer excludes the rebuild so the figure is correct at any
`b.N`, but wall-clock is O(`b.N` × book) and a large `-benchtime` asks for a billion
insertions. Run it with `-benchtime=200x`.

`allocs/op` on these rows includes the harness building each `*types.Order` inside the
measured loop, which the zero-allocation rows above deliberately do not. Read them as
scenario cost, not as engine allocation.

### Recovery time — how long a restart takes

Previously unpublished, while the package doc claimed recovery was "bounded to
O(recent)". That claim is about the *replay* and it is true; it is also not what
dominates a restart.

| Operation | 1 K | 10 K | 100 K |
|---|---:|---:|---:|
| `ReadAll` — read, CRC-verify **and decode** every record | 3.7 ms | 21.4 ms | 214 ms |
| `ReplayTail` — full recovery, no snapshot | 4.3 ms | 25.1 ms | 234 ms |
| `WriteSnapshot` — the checkpoint pause | 10.9 ms | 15.2 ms | 82.4 ms |
| `RestoreSnapshot` — load a checkpoint, empty tail | 2.7 ms | 18.4 ms | 171 ms |

That first row used to be labelled "read + CRC-verify the log", and reading it that
way is how the covered-prefix work below was nearly mis-planned. `ReadAll` decodes
every record into an `Entry` as well, and the decode is the overwhelming majority of
it — see "Recovery behind a covered prefix", where reading and checksumming alone
comes out at ~112 ns a record against this row's ~2.1 µs.

Recovering a **100,000-order book**, which is what a real restart looks like:

| tail after the snapshot | total recovery |
|---:|---:|
| 0 records | 174 ms |
| 1,000 records | 177 ms |
| 10,000 records | 195 ms |

Three things fall out of this, and the third corrects a claim:

1. **A tail record costs ~2.1 µs**, so 10,000 of them add ~21 ms. Snapshotting really
   does bound the replay.
2. **Getting records off disk and into `Entry` values is ~90% of replay cost** —
   2.1 µs of the 2.3 µs per record — at ~15 allocations per record, because records
   are JSON. The next section splits that 2.1 µs: about 112 ns is the read and the
   CRC, and the rest is `json.Unmarshal`. This is where a binary record format would
   pay. It would *not* help write throughput, which is fsync-dominated; it would cut
   restart time.
3. **Restart is dominated by the snapshot, not the tail.** Loading a 100,000-order
   checkpoint is ~174 ms; the tail on top of it is tens of milliseconds. So recovery
   is O(book) for the snapshot **plus** O(tail) for the replay, and describing it as
   "bounded to O(recent)" understates it. The snapshot is what makes the *replay*
   cheap, not what makes the restart cheap.

### Recovery behind a covered prefix

A restart also pays for the part of the log the snapshot **already covers**, and none
of the tables above can see that: they build logs that are only the tail. These build
the prefix on purpose, holding the work constant at 1,000 records to apply while the
already-snapshotted prefix in front of them grows.

`Recover` now reads and CRC-verifies every record in the file and decodes and retains
only the ones past the snapshot's boundary ([BOUNDED-RECOVERY.md](BOUNDED-RECOVERY.md)).
"Before" is the previous behaviour — parse the whole file, then drop the covered
part. Same machine as everything above, `-benchtime 1x -count 5`, medians, file in
page cache.

**`BenchmarkRecoverBehindACoveredChurnPrefix`** writes submit/cancel pairs so the log
grows and the recovered book does not. It isolates the **log** term:

| covered prefix | log on disk | before | after | alloc before | alloc after |
|---:|---:|---:|---:|---:|---:|
| 1,000 | 0.35 MiB | 6.2 ms | 3.4 ms | 3.4 MiB | 2.0 MiB |
| 50,000 | 8.93 MiB | 161 ms | 11 ms | 70.6 MiB | 2.0 MiB |
| 200,000 | 35.4 MiB | 639 ms | 37 ms | 277 MiB | 2.0 MiB |
| 500,000 | 88.4 MiB | 1.66 s | 64 ms | 772 MiB | 2.0 MiB |

**`BenchmarkRecoverBehindACoveredPrefix`** places orders that all rest, so the log and
the recovered book grow together. It is what a real restart costs — O(book) **plus**
O(log) — and it is the reason the two tables exist separately:

| covered prefix | before | after | alloc before | alloc after |
|---:|---:|---:|---:|---:|
| 1,000 | 14.6 ms | 9.9 ms | 5.2 MiB | 3.4 MiB |
| 50,000 | 426 ms | 202 ms | 134 MiB | 51.1 MiB |
| 200,000 | 1.32 s | 435 ms | 435 MiB | 102 MiB |

**Do not add rows from the two tables together, and do not read the second table's
residual growth as a failed skip.** What still grows there is the snapshot restore,
which is O(book) and untouched by this change.

Three things fall out:

1. **Allocation is flat in the covered prefix.** 2.0 MiB whether the prefix is 50,000
   records or 500,000. Covered records are read into two reused buffers and never
   decoded, so they allocate nothing at all.
2. **Time fell by ~26× at half a million covered records**, to ~112 ns a record. The
   design that specified this predicted roughly half, from the mislabelled `ReadAll`
   row above. Taking the marginal cost between the 50,000- and 500,000-record rows,
   a covered record went from ~3.33 µs to ~106 ns: `json.Unmarshal` was ~97% of it.
   Measure, do not derive. And the saving is proportional, not a large-log effect —
   the 1,000-record row skips half its file and takes a little over half the time.
   The first version of that row was published as "6.0 ms" by transcription error and
   explained away as "barely improves"; see [BOUNDED-RECOVERY.md](BOUNDED-RECOVERY.md)
   §9.3 and §9.5.
3. **It is O(RETAINED log), which is O(total log) until retention is turned on.**
   88.4 MiB has to be read and checksummed however recent the snapshot is. Skipping
   the read as well would be faster still and would stop detecting corruption behind
   the snapshot — permanently, since each checkpoint buries it deeper — so it is not
   done. What bounds the read instead is deleting the file: see the next table.

### Restart cost against retention

`BenchmarkRestartWithRetention`, on `buildRetainedChurnLog`
(`pkg/wal/retention_bench_test.go`) — submit/cancel pairs, so the resting book stays
near empty and the log term is isolated from the book term. It is a different fixture
from slice 1's `buildCoveredChurnLog` (`restart_cost_test.go`), which does not rotate
and takes no retention parameters. "Total history" is what was written; "retained" is
what survived and therefore what the restart read.

1 MiB segments, a 4 MiB retained budget, 1,000 records to apply in every row, Apple M4,
`-benchtime 1x` — one pass over a log this process has just written, so the retained
rows are warm and the 1,068 MiB row is partly evicted and effectively cold. Single
samples, not medians: expect run-to-run spread of a few milliseconds on the small rows
(a repeat run put the 60,000-record retention-off row at 10.9 ms against the 18.1 ms
below). The spread does not touch the conclusion, which is a factor of a hundred.

| total history written | retention | retained on disk | segments | `Recover` |
|---:|---|---:|---:|---:|
| 60,000 records (11 MiB) | 4 MiB | 3.7 MiB | 4 | **5.95 ms** |
| 600,000 records (110 MiB) | 4 MiB | 4.1 MiB | 5 | **6.28 ms** |
| 6,000,000 records (1.1 GiB) | 4 MiB | 3.3 MiB | 4 | **5.66 ms** |
| 60,000 records (11 MiB) | off | 10.7 MiB | 11 | 18.1 ms |
| 600,000 records (110 MiB) | off | 106.1 MiB | 107 | 84.1 ms |
| 6,000,000 records (1.1 GiB) | off | 1,068 MiB | 1,069 | **2.21 s** |

**A hundred times the history, the same restart** — 5.95 ms against 5.66 ms, which is
noise. The retention-off rows are the control, they are what every release before this
one did, and they are what a venue with `-wal-retain` unset still does: the same 100×
of history costs 18.1 ms against 2.21 s, a factor of 122. Allocation follows the same
split — 2.1 MB retained against 9.7 MB unretained at the largest row — because what is
allocated is the tail, and what is read is the whole retained set.

**Two rates, and the difference is the page cache, not the segment count.** The 2.21 s
row is a single `-benchtime 1x` pass over a file that has just been written and partly
evicted — 2.07 s/GiB, and the closest thing here to what a restart after a reboot
costs. Re-reading the same 1,068 MiB warm, best of three, takes **767 ms — 0.74 s/GiB**,
which is the ~0.65 s/GiB slice 1 measured on a single file.

Segment count is not what separates them, and this was worth measuring rather than
assuming: the same 1,068 MiB in **1,069 segments of 1 MiB reads in 767 ms and in 9
segments of 128 MiB reads in 799 ms**. A thousand extra `open`/`close` pairs cost
nothing next to a gigabyte of I/O, so the argument for the 128 MiB default is the
rotation cost in the append path below, not the read cost here.

For sizing, use 2 s/GiB cold and 0.75 s/GiB warm: an operator who wants a
one-second cold restart budget picks about 500 MiB of `-wal-retain`, plus O(book) for
the snapshot and O(tail) for what is left to apply.

**`-wal-retain` is a budget, not a bound, and `-wal-retain-segments` is the floor under
it.** The byte budget is checked first and the segment floor second, so the floor wins:
the retained set never falls below `(-wal-retain-segments + 1) x -wal-segment-bytes`,
which at the shipped defaults of 4 and 128 MiB is **640 MiB**. Asking for 500 MiB
against the defaults therefore gets 640 MiB and about 1.3 s, not 1.0 s. Bring the
segment size down with the budget — 500 MiB against 16 MiB segments has an 80 MiB
floor — or raise the budget above the floor and let it do the deciding.

### What a rotation costs the append path

`BenchmarkRotationAppendTail`. A rotation is two fsyncs, a link, an unlink and a
directory fsync, and they land on the goroutine that assigns sequences — the matching
goroutine when the `Writer` is a `Runner`'s command log. Published rather than called
negligible, because a 10 ms hiccup every four minutes is a different product decision
from a 10 ms hiccup every four seconds.

`-benchtime 200000x`, Apple M4, `t.TempDir()` on APFS:

| segments | ns/op | p50 | p99 | p99.9 | rotations | mean rotation | worst rotation |
|---|---:|---:|---:|---:|---:|---:|---:|
| off | 2,526 | 1,625 ns | 9,959 ns | 52 µs | 0 | — | — |
| 128 MiB | 2,423 | 1,625 ns | 9,958 ns | 45 µs | 0 | — | — |
| 1 MiB | 6,001 | 1,625 ns | 9,959 ns | 76 µs | 58 | **12.4 ms** | **21.2 ms** |

**A rotation costs 12.4 ms on the appending goroutine**, and that is measured on the
appends that rotated rather than inferred from the difference between two runs. It is
two fsyncs, a `link`, an `unlink` and a directory fsync on a filesystem where fsync is
not cheap; a Linux server with a battery-backed controller will be faster and should
be measured rather than assumed.

Read it against the rotation RATE, which is what the segment size sets. At the 128 MiB
default and 2,500 msg/s a rotation happens about every four minutes, so 12 ms of
matching-goroutine pause every four minutes — smaller and rarer than a checkpoint's.
At 1 MiB it is every two seconds, which is a different product entirely: the median is
unmoved, the p99 is unmoved, and the mean per append triples. **Small segments are a
test fixture, not a configuration.** If a deployment needs both small segments and a
clean tail, the fix is to pre-create the next segment ahead of need, which changes the
crash matrix in [LOG-ROTATION.md](LOG-ROTATION.md) §3.3 and is not in this slice.

### Against the spec targets (§7)

| Metric | Target | Measured | |
|--------|-------:|---------:|:--|
| Order insert (resting) | ≥ 500 K/s | ~3.8 M/s (`Engine_RestingInsert`, 262 ns) | ✅ |
| Order match (`Match`) | ≥ 200 K/s | ~3.0 M/s maker+taker | ✅ |
| Cancel (dominant real op) | — | ~3.7 M/s, 0.0002 allocs/op | ✅ |
| Best bid/ask read | < 1 µs | 5.8 ns | ✅ |
| Hot-path allocations | 0 on submit/cancel/match | 0 via `Match`; 0.0002 on cancel; `Process` allocates 4 | ✅ |

## The durable path

The table above measures the `Engine` directly. Most embedders do not use the
`Engine` directly — they use the `Runner`, which is the concurrency-safe front,
and most add a write-ahead log and an event sink. That path was previously
unmeasured, so the only published numbers were for the fastest configuration the
library offers rather than the one the documentation recommends.

Allocation counts are deterministic and are the durable part of this comparison:

| Path | allocs/op | What it adds |
|---|---:|---|
| `Engine.Match` into a caller buffer | **0** | nothing — the zero-allocation claim applies here |
| `Engine.Process` (resting insert) | 2 | the `*MatchResult` |
| `Runner.Process` | 3 | the command hand-off and its reply channel |
| `Runner.Process` + `EventSink` | 3 | emission reuses the engine's event buffer |
| `Runner.Process` + `EventSink` + WAL | 9 | JSON-encoding the log record |

Relative cost, measured together on one machine (median of 3 runs, same hardware as
above):

| Path | ns/op | vs. bare `Runner` |
|---|---:|---:|
| `Runner.Process` | 613 | 1× |
| `Runner.Process` + `EventSink` | 619 | 1.0× |
| \+ WAL, group commit (one `Sync` per 256) | 18,260 | **~30×** |
| \+ WAL, `Sync` on every command | 3,810,000 | **~6,200×** |

- Adding an `EventSink` to the `Runner` is free — 619 ns against 613 ns is inside
  the noise.
- **Group commit costs ~30×**, dominated by the `fsync` rather than by encoding.
- **`Sync` on every command costs a further ~210×** on top of that. That is the
  storage device, not the engine. Choose the group size deliberately; it is the
  single biggest performance decision in a durable deployment.
- `Runner.Checkpoint` against a 5,000-order book takes **~0.3 ms** and allocates
  proportionally to book size (5,021 allocations, ~1.4 MB, for 5,000 orders).
  Checkpoint cadence trades recovery time against this pause.

These ratios were re-measured at v0.12.0, which added two entry kinds to the log and
a field to its record: the submit path is unchanged at 9 allocs/op and within 1% on
time, because a submit record encodes the same bytes it did before.

Absolute nanosecond figures for these are deliberately not published here: they
are dominated by the machine's storage and by whatever else it is doing. Run
`go test -bench=. ./pkg/wal/` on your own hardware, with your own group-commit
size, and use the ratios above to sanity-check the shape.

## What these numbers cannot tell you

Every figure on this page is a microbenchmark measured over seconds. They are the right
tool for "did this change make matching slower", and the wrong tool for "can this run a
venue".

Sustained load is now measured separately, by `cmd/obsoak`, and lives in
[SOAK.md](SOAK.md) rather than on this page — an hour at 2,500 messages a second
through a real socket, a real protocol and a durable log, with memory, goroutine and
file-descriptor counts sampled throughout. It found a correctness defect no benchmark
here could have, and it corrected a capacity figure this project had published.

Still not measured anywhere: **days rather than hours**, the gateway with hundreds of
concurrent connections rather than 25, and any workload but the harness's own maker/taker
mix. A capacity plan needs those and this project does not have them. See
[PRODUCTION-READINESS.md](PRODUCTION-READINESS.md), which is explicit about what that
means for the claim.

One warning that belongs here more than anywhere: **absolute throughput figures do not
survive a machine that is doing something else.** The first capacity numbers published
in SOAK.md failed to reproduce four hours later on the same code, because the host had
got busier and the measurement recorded nothing about that. The interleaved A/B against
a worktree that this page uses for every figure is the reason its numbers hold up, and
it was not applied there. Do not compare throughput across runs without it.

## Notes on the numbers

- **Scope: the matching core only.** Every figure here is an in-process call
  into the engine. `*types.Order` values are built before `b.ResetTimer()` and
  handed straight to the matcher, so the numbers exclude order allocation,
  decoding, boundary validation, network I/O, and the session and order-entry
  protocol layers. Those layers **do** exist in this repository as of v0.10.0
  (`internal/wire`, `pkg/orderentry`, `cmd/obgw`) and none of them is measured
  here — a gateway decodes a frame, checks a rate limit, resolves a client order
  id and hands off through a channel before the engine sees anything, and that
  work is not in any number on this page. These measure the matching algorithm and
  its data structures. They are a floor; any system built around this engine is
  strictly slower, and the gap is dominated by the layers that are not
  benchmarked here.
- **The hot path is allocation-free.** `Match(order, buf)` appends value trades
  into a caller-reused buffer and the book pools nodes/levels, so steady-state
  submit/cancel/match allocate nothing (docs/SPEC.md §6.1). `Process` is the
  ergonomic wrapper that builds a `*MatchResult` (4 allocs); use `Match` when
  latency matters. Decimals were removed from the hot path in v0.2.0.
- **Numbers vary by hardware.** GitHub-hosted runners are typically slower than
  an M-series laptop; use the CI run summary for a neutral baseline and your own
  machine for local comparison.
- **These are microbenchmarks.** They measure the core data structures and match
  loop in isolation, not end-to-end system throughput (which also involves
  persistence, networking, and risk checks that live in layers above the core).
