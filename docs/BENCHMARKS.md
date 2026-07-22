# Benchmarks

Performance of the core library, measured with Go's benchmark tooling. These are
tracked as regression targets (docs/SPEC.md §7), not marketing — the harness is
in-repo so anyone can reproduce them.

## Reproduce

```sh
make bench
# or:
go test -run '^$' -bench=. -benchmem ./pkg/orderbook/ ./pkg/matching/
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
| Order match (`Match`) | ≥ 200 K/s | ~2.8 M/s round-trip | ✅ |
| Cancel (dominant real op) | — | ~4 M/s, 0 allocs | ✅ |
| Best bid/ask read | < 1 µs | 6.3 ns | ✅ |
| Hot-path allocations | 0 on submit/cancel/match | 0 (via `Match`) | ✅ |

## Notes on the numbers

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
