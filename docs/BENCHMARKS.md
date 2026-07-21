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

| Benchmark | ns/op | ~ops/sec | B/op | allocs/op |
|-----------|------:|---------:|-----:|----------:|
| `OrderBook_Add` (insert into sorted ladder) | 401 | ~2.5 M | 241 | 6 |
| `OrderBook_BestBid` (top-of-book read) | 70 | ~14 M | 48 | 4 |
| `Engine_RestingInsert` (full engine, order rests) | 561 | ~1.8 M | 397 | 10 |
| `Engine_Match` (maker + taker + trade round-trip) | 1548 | ~646 K | 1200 | 47 |

### Against the spec targets (§7)

| Metric | Target | Measured | |
|--------|-------:|---------:|:--|
| Order insert (resting) | ≥ 500 K/s | ~1.8 M/s | ✅ |
| Order match | ≥ 200 K/s | ~646 K/s round-trip | ✅ |
| Best bid/ask read | < 1 µs | 70 ns | ✅ |
| Hot-path allocations | bounded | measured above | ✅ |

## Notes on the numbers

- **Allocations come mostly from `shopspring/decimal`.** Exact-decimal money is
  a deliberate correctness choice (docs/SPEC.md §6.1); it is the dominant cost on
  the hot path. The documented future optimization is an `int64` fixed-point
  "ticks" fast path behind the same interface for latency-critical deployments.
- **Numbers vary by hardware.** GitHub-hosted runners are typically slower than
  an M-series laptop; use the CI run summary for a neutral baseline and your own
  machine for local comparison.
- **These are microbenchmarks.** They measure the core data structures and match
  loop in isolation, not end-to-end system throughput (which also involves
  persistence, networking, and risk checks that live in layers above the core).
