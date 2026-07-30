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
| `OrderBook_Cancel` (drain) | 273 | ~3.7 M | 46† | ~0† |
| `OrderBook_CancelReplace` (MM churn) | 172 | ~5.8 M | 1† | ~0† |
| `OrderBook_LevelChurn` (new price level) | 292 | ~3.4 M | 0 | 0 |
| `Engine_MatchInto` (`Match`, maker+taker+trade) | 329 | ~3.0 M | **0** | **0** |
| `Engine_Match` (`Process` convenience wrapper) | 419 | ~2.4 M | 296 | 4 |

Run-to-run spread is ±10% on most rows and wider on `CancelReplace`, which varied
120–212 ns across the five runs. Treat these as an order of magnitude with a shape,
not as constants.

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
