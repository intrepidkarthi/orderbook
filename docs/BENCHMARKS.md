# Benchmarks

Performance of the core library, measured with Go's benchmark tooling. These are
tracked as regression targets (docs/SPEC.md §7), not marketing — the harness is
in-repo so anyone can reproduce them.

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
| Cancel + replace (MM steady state) | **0.009** |
| `Add` into a growing book | **1.05** |

So cancel and market-maker churn are allocation-free in substance, not just after
rounding. `Add` on its own is not, and that is what "pooled" means rather than
"allocation-free": the pool only pays back once a release has put something in it.
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
| `ReadAll` — read + CRC-verify the log | 3.7 ms | 21.4 ms | 214 ms |
| `ReplayTail` — full recovery, no snapshot | 4.3 ms | 25.1 ms | 234 ms |
| `WriteSnapshot` — the checkpoint pause | 10.9 ms | 15.2 ms | 82.4 ms |
| `RestoreSnapshot` — load a checkpoint, empty tail | 2.7 ms | 18.4 ms | 171 ms |

Recovering a **100,000-order book**, which is what a real restart looks like:

| tail after the snapshot | total recovery |
|---:|---:|
| 0 records | 174 ms |
| 1,000 records | 177 ms |
| 10,000 records | 195 ms |

Three things fall out of this, and the third corrects a claim:

1. **A tail record costs ~2.1 µs**, so 10,000 of them add ~21 ms. Snapshotting really
   does bound the replay.
2. **Reading the log is ~90% of replay cost** — 2.1 µs of the 2.3 µs per record — at
   ~15 allocations per record, because records are JSON. This is where a binary record
   format would pay. It would *not* help write throughput, which is fsync-dominated;
   it would cut restart time.
3. **Restart is dominated by the snapshot, not the tail.** Loading a 100,000-order
   checkpoint is ~174 ms; the tail on top of it is tens of milliseconds. So recovery
   is O(book) for the snapshot **plus** O(tail) for the replay, and describing it as
   "bounded to O(recent)" understates it. The snapshot is what makes the *replay*
   cheap, not what makes the restart cheap.

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
