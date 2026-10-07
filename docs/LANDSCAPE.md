# Landscape — Open-Source Order Books in Go, Rust and C++

Surveyed 2026-10-07 · Author: Karthikeyan NG

What else exists, what each project publishes about itself, and what that means for
this one. Everything here was read through the GitHub API, crates.io, pkg.go.dev and the
web. **No third-party code was cloned, built or run.** Every performance figure is the
project's own published claim, or an independent harness's measurement where that is
said. None was reproduced here. Figures from different hardware, workloads and methods
are **not comparable with each other or with this project's**, and no ranking is
implied by putting them in one table. Stars and dates move. Treat this as a dated
snapshot, not a scoreboard.

## 1. The short version

- **Nobody else combines** all of these in one project:
  - integer prices;
  - five self-trade-prevention modes, pro-rata, and call auctions;
  - iceberg, stop, pegged and OCO orders;
  - GTD/DAY expiry;
  - a write-ahead log with bounded recovery and replication drills;
  - a binary gateway with a market-data feed, and surveillance;
  - a second reference matcher with differential testing;
  - a versioned behaviour stamp, and a benchmark gate.
- The closest in **features** are OrderBook-rs (Rust) and 0x5487/matching-engine (Go).
  OrderBook-rs is a shared book behind locks, with a "lock-free" claim its own code
  contradicts. 0x5487 is GPL-3.0.
- The closest in **measured speed** among Go books publish single-operation figures in
  the same range as ours. geseq/orderbook reports a p50 cancel of 35 ns and a p50 add of
  170 ns. Nobody, including this project, has measured on a shared workload.
- **An independent harness exists**: flash1-dev/matching-engine-benchmark. It replays one
  command stream through a C ABI into about 247 engines, about 30 of them Go, and checks
  each engine's report stream against a consensus hash. **This project is not in it.**
  Several Go books are, and geseq runs it nightly in CI.
- **Adoption is driven by things this project lacks**:
  - a picture-driven quickstart;
  - percentile tables with the machine named;
  - real-data (NASDAQ ITCH) benchmarks;
  - Python bindings;
  - an order-type checklist up front;
  - a runnable whole exchange;
  - a place in someone else's comparison.

## 2. The independent harness

[flash1-dev/matching-engine-benchmark](https://github.com/flash1-dev/matching-engine-benchmark)
(v1.0.0, 2026-07-16):

- **Method:** a C ABI (`api/matching_engine_api.h`), about 2.0 M messages on one symbol,
  five scenarios (static, normal, swing-25, swing-40, flash-crash), seed 23, Graviton4 at
  `-O3 -march=native`, median of 10. It reports the worst case of the five, in M msg/s,
  and checks correctness with a SHA-256 of the report stream.
- **Findings:** 160 of 247 engines conform, and 181 issues were filed upstream.
- **Caveat:** it is published by a vendor whose own engine heads the table.
- **Go engines cross a cgo boundary per message**, so their figures include that cost.

| Engine | Lang | Worst case, M msg/s | Note |
|---|---|---:|---|
| CppTrader | C++ | 7.26 | as shipped |
| Kautenja/limit-order-book | C++ | 6.88 | with fix |
| fmstephe/matching_engine | Go | 2.48 | with fix; fastest Go |
| geseq/orderbook | Go | 1.81 | as shipped; its cross-through bug was found here |
| robaho/cpp_orderbook | C++ | 1.90 | self-claim 10–22 M/s |
| exchange-core | Java | 1.40 | batched through JNI |
| 0x5487/matching-engine | Go | 1.15 | self-claim ~2.6 M/s |
| i25959341/orderbook | Go | 0.72 | with fix |
| robaho/go-trader | Go | 0.56 | with fix |
| liquibook | C++ | 0.03 | with fix; deep static book |

Source: `CONSENSUS_CONFORMING_ENGINES.md` in that repository.

## 3. Go

| Project | ★ | License | Last commit | Prices | Cancel | Notable |
|---|---:|---|---|---|---|---|
| [i25959341/orderbook](https://github.com/i25959341/orderbook) | 556 | MIT | 2025-04 | shopspring decimal, string-keyed levels | O(1) + tree | Mindshare leader, awesome-go. Limit and market only. Unmaintained since 2019 in practice. README imports a fork path. |
| [0x5487/matching-engine](https://github.com/0x5487/matching-engine) | 24 | GPL-3.0 | 2026-04 | udecimal | skiplist | Disruptor actor, event sourcing, snapshots, iceberg, post-only, IOC/FOK, multi-market, versioned releases, design docs. No WAL, STP or stops. |
| [geseq/orderbook](https://github.com/geseq/orderbook) | 15 | MIT | 2026-07 | fixed 8 dp | O(1) | Best latency methodology in Go. No releases. Author says "You probably shouldn't use this as-is". |
| [robaho/go-trader](https://github.com/robaho/go-trader) | 520 | GPL-3.0 | 2026-06 | fixed 7 dp | O(1) in level | A full runnable exchange (FIX, gRPC, multicast, UIs). Not importable. |
| [fmstephe/matching_engine](https://github.com/fmstephe/matching_engine) | 473 | none | 2022-02 | uint64 | O(log n) | Invariant and differential testing, ITCH replay. No license, so not reusable. |
| [GOnevo/matchingo](https://github.com/GOnevo/matchingo) | 41 | MIT | 2026-06 | fpdecimal | **O(level)** | Friendliest single-call API; stop and OCO. |
| [alexey-ernest/go-hft-orderbook](https://github.com/alexey-ernest/go-hft-orderbook) | 280 | MIT | 2023-12 | float64 | O(1) | No matching at all. |

Published figures:

- **geseq** (README; 12th-gen i7 at 2.1 GHz, turbo and hyperthreading off, `GOMAXPROCS=2`):

  | Operation | p50 | p99 | p99.99 | max |
  |---|---:|---:|---:|---:|
  | AddOrder | 170 ns | 229 ns | 2.4 µs | 36 µs |
  | CancelOrder | 35 ns | 50 ns | 86 ns | 25 µs |

  It also claims "12.5 million Order Add/Cancel per second", or 21 M with turbo on.
- **0x5487** (`docs/benchmark.md`; Ryzen 7 PRO 4750U, go1.25.4): 385.3 ns/op,
  2.6 M orders/s, 2 allocs/op.
- **matchingo** (i7-8565U): 1755 ns/op, 14 allocs/op.
- **i25959341**: "above 300k trades per second", with no conditions stated.

## 4. Rust

| Project | ★ / downloads | License | Last commit | Prices | Notable |
|---|---|---|---|---|---|
| [OrderBook-rs](https://github.com/joaquinbejar/OrderBook-rs) | 541 / 72k | MIT | 2026-10 | u128 / u64 | Widest Rust feature set: STP, stops, iceberg, pegged, journal and replay, NATS, risk layer. Shared book behind `RwLock`s and `DashMap`. |
| [NautilusTrader](https://github.com/nautechsystems/nautilus_trader) `nautilus-model` | 29.7k | LGPL-3.0 | 2026-10 | fixed 9/16 dp | A data book inside a trading platform. Level delete is O(depth). |
| [hftbacktest](https://github.com/nkaz001/hftbacktest) | 4.9k | MIT | 2025-12 | f64 | Backtester with queue-position models, not a matching engine. Python is the draw. |
| [Phoenix v1](https://github.com/Ellipsis-Labs/phoenix-v1) | 278 | MIT (from BUSL) | — | ticks / lots | Audited on-chain CLOB with three STP modes. |
| [lobster](https://github.com/rubik/lobster) | 177 | ISC | 2022-09 | u64 | Simple; O(depth) cancel; dormant. |

Published figures:

- **OrderBook-rs** `BENCH.md`, the credible set (Apple M5 Max, unpinned, HDR histograms,
  closed loop, coordinated omission disclaimed):

  | Workload | p50 | p99 |
  |---|---:|---:|
  | cancel_only | 790 ns | 1,027 ns |
  | mixed_70_20_10 | 542 ns | 15.7 µs |

  The mixed workload costs 3.35 allocs/op. Its README also prints 31.6 M ops/s "hot spot"
  aggregate figures of the kind its sister crate `pricelevel` publicly withdrew.
- **AsthaMishra/matching-engine** (small, unlicensed) publishes the most careful table in
  Rust: 51 ns top-of-book match, 58 ns cancel at depth 1,000 (Criterion), an ITCH replay
  at p50 99 ns, and loopback order-to-ack at p50 10.4 µs. It says which is which.
- Headline TPS figures elsewhere, such as 7.2 M TPS or "8 ns per order", come with no
  hardware stated, or are loop-amortised means over fully crossing flows.

## 5. C++, and exchange-core

| Project | ★ | License | Last commit | Notable |
|---|---:|---|---|---|
| [liquibook](https://github.com/enewhuis/liquibook) | 1.5k | OCI custom | 2022-12 | `std::multimap` per side; cancel scans the price level linearly. AON/IOC/FOK, stop. Header-only. |
| [CppTrader](https://github.com/chronoxor/CppTrader) | 1.1k | MIT | 2026-09 | AVL levels plus a hash index; widest C++ order types. Cross-platform CI. |
| [exchange-core](https://github.com/exchange-core/exchange-core) (Java) | 2.6k | Apache-2.0 | 2022-05 | Adaptive radix tree, Disruptor pipeline, risk, journaling. The reference most people cite. |
| [Kautenja/limit-order-book](https://github.com/Kautenja/limit-order-book) | 311 | MIT | 2020-07 | Python on PyPI. |
| [itch-order-book](https://github.com/charles-cooper/itch-order-book) | 417 | BSD-3 | 2022-06 | Aggregate book only: 61 ns per tick. |

Published figures:

- **liquibook**: 2.06–2.49 M **inserts**/s (`PERFORMANCE.md`; a "2.4 GHZ i7" laptop, about
  14 prices, timed with `clock()`). **No cancel figure is published.**
- **CppTrader**: 102–309 ns mean per ITCH message (i7-4790K, Windows 8). That is book
  building with matching switched **off** (`performance/market_manager.cpp`).
- **exchange-core**: p50 0.5 µs and p99 4 µs at 1 M ops/s; 5 M ops/s with p99 42 µs
  (dual X5690, isolated tickless cores, mitigations off, Java 8). The workload is 82%
  moves, about 1,000 resting orders, open loop, excluding network and journal.

## 6. This project's own claims, in the same light

- **"Cancel ~3× faster than liquibook"** (CHANGELOG v0.13.0) cannot be re-run. Liquibook's
  cancel is O(log N + k), where k is the queue at that price, so the ratio depends on how
  many orders shared a level: about 50 in that preload. It measured queue depth as much
  as language. §1.6 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md) retires or qualifies it.
- **The same release measured geseq's cancel at 15–21 ns and OrderBook-rs's at 41 ns.**
  That drove the purpose-built order index. Those were local runs, made before this
  project's rule against running third-party code on the maintainer's machine.
  Comparisons from now on run on CI.
- **This project's published microbenchmarks** (README, `BENCHMARKS.md`; M4, go1.23.5):

  | Benchmark | Result |
  |---|---|
  | Cancel, 200K-order book | 65 ns |
  | Match into a caller buffer | 329 ns, 0 allocations |
  | Cancel-heavy tail | p50 83 / p99 167 / p999 250 ns |

  These are single-threaded and closed-loop, with no network or log in the path.

## 7. What brings users, as observed

1. A quickstart that draws the book before and after each call (i25959341).
2. Percentile tables with tuning stated (geseq, exchange-core), and a table of what each
   number does and does not measure (AsthaMishra, OrderBook-rs `BENCH.md`).
3. Real public data: NASDAQ ITCH replays (CppTrader, itch-order-book, several Rust books).
4. **Python bindings**: the largest driver in the Rust space (hftbacktest, NautilusTrader).
5. An order-type checklist at the top of the README.
6. A whole exchange you can run: go-trader, or DistributedATS on liquibook.
7. A permissive license. GPL and unlicensed projects are admired and not embedded.
8. Versioned releases with a changelog. Most Go competitors have none.
9. Appearing in someone else's comparison. flash1 lists many Go books and not this one.

What to avoid, also as observed: "lock-free" claims the code contradicts, and aggregate
throughput presented as latency. Both cost credibility, and one Rust crate withdrew its
numbers publicly.
