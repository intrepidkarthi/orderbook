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

Apple M-series laptop, Go 1.23, single-threaded (`-benchmem`):

int64 ticks/lots, pooled book nodes/levels, single-writer engine:

| Benchmark | ns/op | ~ops/sec | B/op | allocs/op |
|-----------|------:|---------:|-----:|----------:|
| `OrderBook_BestBid` (top-of-book read) | 6.3 | ~160 M | 0 | 0 |
| `OrderBook_Cancel` (drain) | 253 | ~4 M | 0 | 0 |
| `OrderBook_CancelReplace` (MM churn) | 180 | ~5.5 M | 0 | 0 |
| `OrderBook_LevelChurn` (new price level) | 292 | ~3.4 M | 0 | 0 |
| `Engine_MatchInto` (`Match`, maker+taker+trade) | 352 | ~2.8 M | **0** | **0** |
| `Engine_Match` (`Process` convenience wrapper) | 491 | ~2 M | 296 | 4 |

**Tail latency** — `BenchmarkLatency_CancelHeavy`, a ~90%-cancel / 10%-new mix
against a warm book: **p50 83 ns · p99 167 ns · p999 292 ns**, 0 allocs/op.

### Against the spec targets (§7)

| Metric | Target | Measured | |
|--------|-------:|---------:|:--|
| Order match (`Match`) | ≥ 200 K/s | ~2.8 M/s maker+taker | ✅ |
| Cancel (dominant real op) | — | ~4 M/s, 0 allocs | ✅ |
| Best bid/ask read | < 1 µs | 6.3 ns | ✅ |
| Hot-path allocations | 0 on submit/cancel/match | 0 (via `Match`) | ✅ |

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
| `Runner.Process` | 4 | the command hand-off and its reply channel |
| `Runner.Process` + `EventSink` | 4 | emission reuses the engine's event buffer |
| `Runner.Process` + `EventSink` + WAL | 9 | JSON-encoding the log record |

Relative cost, measured together on one machine in one run:

- Adding an `EventSink` to the `Runner` is close to free — within run-to-run
  noise on the insert path.
- Adding a write-ahead log with **group commit** (one `Sync` per 256 commands)
  costs roughly an order of magnitude over the in-memory path, and is dominated
  by the `fsync`, not by encoding.
- `Sync` on **every** command costs three further orders of magnitude. That is
  the storage device, not the engine. Choose the group size deliberately; it is
  the single biggest performance decision in a durable deployment.
- `Runner.Checkpoint` against a 5,000-order book is on the order of a
  millisecond and allocates proportionally to book size. Checkpoint cadence
  trades recovery time against this pause.

Absolute nanosecond figures for these are deliberately not published here: they
are dominated by the machine's storage and by whatever else it is doing. Run
`go test -bench=. ./pkg/wal/` on your own hardware, with your own group-commit
size, and use the ratios above to sanity-check the shape.

## Notes on the numbers

- **Scope: the matching core only.** Every figure here is an in-process call
  into the engine. `*types.Order` values are built before `b.ResetTimer()` and
  handed straight to the matcher, so the numbers exclude order allocation,
  decoding, boundary validation, network I/O, and any session or order-entry
  protocol — none of which exist in this repository (see the Scope note in the
  README). These measure the matching algorithm and its data structures. They
  are a floor; any system built around this engine is strictly slower, and the
  gap is dominated by the layers that are not benchmarked here.
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
