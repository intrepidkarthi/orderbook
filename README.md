# orderbook

<p align="center">
  <a href="https://intrepidkarthi.github.io/orderbook/"><img src=".github/readme/demo.gif" alt="orderbook — a limit order book and matching engine in Go: a market order crosses the spread and trades at the maker's price" width="820"></a>
</p>

<p align="center"><b><a href="https://intrepidkarthi.github.io/orderbook/">▶ Live demo</a></b> — the real engine, compiled to WebAssembly, running in your browser · <b><a href="https://intrepidkarthi.github.io/orderbook/console.html">▶ Live console</a></b> — a running market with signals and surveillance, every panel titled by the call that produces it.</p>

[![Go Reference](https://pkg.go.dev/badge/github.com/intrepidkarthi/orderbook.svg)](https://pkg.go.dev/github.com/intrepidkarthi/orderbook)
[![CI](https://github.com/intrepidkarthi/orderbook/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/intrepidkarthi/orderbook/actions/workflows/ci.yml)
![Go 1.23+](https://img.shields.io/badge/go-1.23%2B-00ADD8?logo=go&logoColor=white)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Live demo](https://img.shields.io/badge/demo-WebAssembly-654FF0?logo=webassembly&logoColor=white)](https://intrepidkarthi.github.io/orderbook/)

An embeddable limit order book and matching engine in Go. Integer-exact `int64`
pricing with no floating point on the money path, a match path that allocates
**0 B/op**, a lock-free single-writer core (the LMAX model), deterministic crash
recovery gated in CI against a 2,000-command tape, and `cmd/obgw` — a reference
TCP gateway speaking a frozen binary protocol on both edges, order entry and
public market data. `go get` it into an exchange, a simulator, or a backtester:
the core owns the book, the matching algorithm, order lifecycle, sequencing and
market-data snapshots, while `pkg/wal` (durable persistence), `pkg/surveillance`
(market-abuse detection), `pkg/gateway` (pre-trade admission) and `pkg/auction`
(uniform-price call auction) cover the layers around it. It has never run a live
market, and [what that costs you](#experimental-use-at-your-own-risk) is written
down rather than implied.

---

## Installation

```sh
go get github.com/intrepidkarthi/orderbook/pkg/matching
```

Requires Go 1.23 or later.

---

## Quickstart

The engine works in integer ticks and lots. Pass them directly, or use an
`Instrument` to convert from decimals at the boundary.

```go
eng := matching.NewEngine(matching.DefaultConfig("BTC-USD"))

// A resting sell, then a crossing buy that trades against it at the maker price.
sell, _ := types.NewOrder("mm", "BTC-USD", types.SideSell, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
eng.Process(sell)

buy, _ := types.NewOrder("taker", "BTC-USD", types.SideBuy, types.OrderTypeLimit, 101, 3, types.TIFGoodTillCancel)
res := eng.Process(buy) // res.Trades, res.Status, res.RejectionReason

bid, qty, ok := eng.BestBid()
```

Decimals at the boundary, concurrent submission, and the zero-allocation path:

```go
// Decimals in, int64 ticks/lots out.
inst := types.NewInstrument("BTC-USD", decimal.RequireFromString("0.01"), decimal.RequireFromString("0.001"))
order, _ := inst.NewOrder("alice", types.SideBuy, types.OrderTypeLimit,
    decimal.RequireFromString("30000.50"), decimal.RequireFromString("0.25"), types.TIFGoodTillCancel)

// Many producers, one matching goroutine.
r := matching.NewRunner(matching.RunnerConfig{Engine: matching.DefaultConfig("BTC-USD")})
defer r.Close()
r.SubmitAsync(order) // enqueue without blocking; result arrives on the returned channel

// Zero-allocation hot path: reuse the trade buffer across calls.
buf := make([]types.Trade, 0, 8)
buf, status, _ := eng.Match(order, buf[:0])
```

Or run the reference gateway and talk to it over a socket:

```sh
go run ./cmd/obgw -addr 127.0.0.1:9000 -symbol BTC-USD -accounts alice:s3cret
```

`cmd/obgw`'s tests are a working client — login, enter, cancel, reduce, query,
resume — and are the most useful reference for writing another one. The protocol
helpers live in `server_test.go`.

Runnable, testable examples render on
[pkg.go.dev](https://pkg.go.dev/github.com/intrepidkarthi/orderbook/pkg/matching#pkg-examples).

---

## Experimental. Use at your own risk.

> **⚠ This is an experiment, not a product.** It has never run a live market. It has
> had no independent review. Its Go API and its wire protocol have each broken more
> than once in a single week, and the compatibility promise that now governs them
> ([docs/COMPATIBILITY.md](docs/COMPATIBILITY.md)) is days old. Known problems are
> documented rather than fixed — a venue left running with `-wal-retain` unset still
> becomes progressively harder to restart, because the command log then keeps every
> byte it has ever written and recovery reads all of it.
>
> **If you run this and something goes wrong — lost orders, a wrong book, money —
> that is yours, not mine.** The MIT licence's "without warranty of any kind" is the
> legal form of that sentence; this is the plain one. I am not responsible for what
> happens in your production, and nobody has run it in theirs.
>
> What this repository does offer is unusual specificity about its own limits. Every
> claim names the test or the measurement behind it, several sections exist only to
> correct earlier claims that were wrong, and
> [docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md) is a list of what
> would stop you. Read those before trusting any of this, not after.

**Scope.** *Embeddable* concedes the venue is not here — and no adjective in
this README claims what only a deployment can prove. Production readiness is a
property of a deployment, not of code:
[docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md) is the honest
account of what is solid, what is partial, and what is yours. Nobody runs this
in production today, and this paragraph will say so until someone does.

What ships: the matching core, durable recovery, an event stream proven to
reconstruct the book, an operator kill switch, a frozen binary protocol
([docs/PROTOCOL.md](docs/PROTOCOL.md)), and `cmd/obgw` — a reference TCP gateway
serving **both edges**: order entry with authentication, per-account outbound streams
and gap-free resume across a disconnect, and a public market-data feed with
snapshot-plus-delta recovery. Bounded backpressure at every stage. Around it:
`EngineSnapshot.Digest` (the book fingerprint replication and recovery agree on),
`examples/replication` (the drilled primary-backup reference), `cmd/obdash` (an
operator dashboard that is an ordinary subscriber of the venue's own feed), and a
[live console](https://intrepidkarthi.github.io/orderbook/console.html) running
the engine, signals and surveillance in the browser.

What does not, and is yours: **continuous operation** (the log rotates and a
covered prefix can be deleted, but retention is opt-in and unset by default, so a
venue left running without `-wal-retain` still becomes slower and hungrier to
restart every day it stays up — the first thing to set before running this for a
week), **credential lifecycle** (the reference speaks TLS and
holds secret digests, never plaintext — but rotation, revocation and expiry are
yours, and it says so), **multi-symbol routing** (order ids and
sequences are per-engine, so several symbols means several engines and a router
above them), **clearing and settlement**, and **any HA topology** — the library
ships the seams for primary-backup (deterministic apply, an ordered command log,
replay mode, snapshot bootstrap), proves them with a reference example and CI
drills ([docs/REPLICATION.md](docs/REPLICATION.md)), and deliberately not the
consensus, because bundling one forces a wrong answer on everybody. See
[docs/EXCHANGE-ARCHITECTURE.md](docs/EXCHANGE-ARCHITECTURE.md) for why, including
the venues that lost quorum getting it wrong.

What is offered here is that the pieces you build on are correct, tested, and
honest about their edges.
[docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md) is the checklist:
what ships, what is deliberately absent, what you would have to build — and which
gaps no library work can close, because they are properties of your deployment.

---

## Highlights

- **Integer-exact pricing.** The engine works in `int64` ticks and lots; a
  per-symbol `Instrument` converts decimals only at the API boundary. No
  floating-point on the money path.
- **Low latency, zero allocation.** O(1) cancel, pooled book nodes and price
  levels, and a caller-buffer match path that allocates **0 B/op**. A realistic
  cancel-heavy flow runs at **p50 83 ns · p99 167 ns · p999 250 ns** per
  operation.
- **Single-writer core.** One matching goroutine owns the book with no lock on
  the hot path (the LMAX model). A `Runner` fronts it with an MPSC command queue
  so many producers can submit concurrently.
- **Deterministic and recoverable.** The same ordered command stream produces
  byte-identical trades and book state — enabling command-log replay, durable
  WAL crash recovery (`pkg/wal`: write-ahead log + snapshots), and reproducible
  backtests. `Runner.Checkpoint` and `wal.Recover` join the snapshot to its log
  position, and the property is gated in CI against a 2,000-command tape:
  checkpoint anywhere, recover, and the book, all three sequence counters, the
  duplicate guard and the conditional-order state match the uninterrupted run. What
  a snapshot bounds is the *replay* and the *parse*, not the read: recovery reads and
  checksum-verifies every retained byte however recent the snapshot. What bounds the
  read is retention — the log rotates into segments and a prefix of them is deleted
  once a verified snapshot covers it, so restart cost is O(retained log) and the
  retained size is a byte budget you set. That budget is **not set by default**, and a
  venue that leaves it unset gets slower to restart every day it stays up. Measured in
  [docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md); the design and its
  costs are in [docs/LOG-ROTATION.md](docs/LOG-ROTATION.md) and
  [docs/BOUNDED-RECOVERY.md](docs/BOUNDED-RECOVERY.md).
- **An event stream that reconstructs the book.** `Accepted`/`Trade`/`Canceled`/
  `Replaced` replay into an L3 book identical to the engine's, asserted on every
  commit across 28 scenarios covering every order class — including iceberg
  refill, all five self-trade-prevention modes, FOK reversal, FOK against a
  resting iceberg, cascade-fired stops and a bust.
- **Full order surface.** Limit, market, stop / stop-limit, iceberg (hidden),
  post-only, pegged, OCO / bracket, and trailing-stop orders; GTC / IOC / FOK / DAY /
  GTD time-in-force, with the venue holding the deadline rather than the client
  remembering to cancel; self-trade prevention; a price-band circuit breaker; FIFO or
  pro-rata allocation.
- **A trading session.** Pre-open accepts orders without matching them, so the book
  accumulates and may legitimately cross; opening resolves it at a single clearing
  price by auction, in price-time priority, so a venue never opens onto a crossed book.
  The engine holds no calendar — it knows what each phase permits, not when phases
  change, because that is the venue's business.
- **Market integrity & safety.** Opt-in pre-trade risk controls — fat-finger and
  dust caps, per-account order limits, minimum resting time, client-order-id
  idempotency, mark-price step **and** depth bounds, a self-output guardrail, and
  timed band-breach pauses — plus a market-abuse surveillance suite (spoofing,
  order-to-trade ratio, marking-the-close, ramping, pinging, cross-book), an
  enforcing per-account rate gate, an operator kill switch, and a
  uniform-price call auction. Each maps to a real case in
  [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md). The taker speed bump is an
  observation hook, not enforcement — it reports, it does not delay.
- **A network edge that works.** `cmd/obgw` is a reference TCP order-entry
  gateway: authentication defaulting to deny, a frozen binary protocol
  ([docs/PROTOCOL.md](docs/PROTOCOL.md)), per-account outbound streams that
  outlive a connection, and gap-free resume — reconnect with your cursor and
  receive the fills that landed while you were disconnected. A client can never
  name another account's order, because the wire has no field for it.

  The full client lifecycle is on the wire: six order types (limit, market, stop /
  stop-limit, OCO, iceberg, pegged, trailing), **reduce in place keeping queue
  position**, **atomic cancel/replace**, **mass cancel**, cancel-on-disconnect, and
  **query your open orders** when resume is not available. Orders that outlive a
  restart stay nameable and keep reporting their fills.
- **An admin edge, and a soak harness that used it.** `cmd/obgw -admin` serves
  Prometheus metrics, `/healthz` and `/readyz` on a third port — counters off the
  engine's own event stream, so they count what the book saw rather than what the
  gateway believed it sent, and nothing a scrape touches goes through the command
  queue. `cmd/obsoak` drives the venue at a sustained rate and reports what grows.
  The longest run is four hours over three books — 14.4M messages, goroutines and
  descriptors flat across 240 samples, no orphans ([docs/SOAK.md](docs/SOAK.md) §1e).
  The first hour of ever running it found a defect that the whole test suite, two
  fuzzers and the race detector had not: under load the venue refused cancels for orders live in its
  own book, and filled to its order ceiling while reporting itself healthy. Fixed,
  regression-tested, and written up in [docs/SOAK.md](docs/SOAK.md).
- **A market-data edge.** `cmd/obgw -mdaddr` publishes the venue's public feed on its
  own listener: a snapshot naming the sequence it is consistent with, then incremental
  level changes, trade prints and venue-state changes in one dense, gap-free stream.
  A subscriber holding a cursor gets a gap-fill instead; one too far behind, or from
  another venue incarnation, is refused explicitly rather than quietly resynchronised.
  The guarantee — snapshot plus everything after its sequence equals the engine's book
  — is asserted both in-process and end to end over a socket.
- **Market data.** L1 / L2 / L3 (market-by-order) snapshots with sequence numbers,
  plus `marketdata.L2Feed` — incremental aggregated depth derived from the event
  stream, coalesced per command, with absolute quantities so a subscriber that misses
  an update recovers on the next one. Its test asserts the derived levels equal the
  engine's own snapshot after every command, which is how a long-standing depth bug
  was found.
- **Tested and benchmarked.** Race, fuzz, soak, and replay-recovery suites;
  microbenchmarks run in CI on every push.

---

## Concurrency model

| | `matching.Engine` | `matching.Runner` |
|---|---|---|
| Contract | single writer — drive from **one** goroutine | safe for **concurrent** producers |
| Mechanism | direct calls, no lock on the hot path | MPSC command queue → one matching goroutine |
| Submit | `Process` (result) · `Match` (zero-alloc, into a buffer) | `Process` (enqueue + wait) · `SubmitAsync` (non-blocking) |
| Reads | `BestBid` / `Snapshot` / … (book has its own RW-lock) | same, delegated to the engine |
| Use when | you own the sequencing loop; benchmarks | any multi-goroutine service |

The bare `Engine` has no internal mutex by design; calling its mutating methods
from multiple goroutines is a data race. Use a `Runner`, or serialize to one
goroutine. See [docs/INTEGRATION.md](docs/INTEGRATION.md).

---

## Performance

Core-library microbenchmarks (Apple M4, `go1.23.5 darwin/arm64`, single-threaded):

| Benchmark | ns/op | allocs/op | ~ops/sec |
|---|---:|---:|---:|
| Top-of-book read (`BestBid`) | 5.8 | 0 | ~170 M |
| Cancel (drain, 200 K-order book) | 65 | 0.0002 | ~15 M |
| Cancel (drain, 10 M-order book) | 273 | 0.0002 | ~3.7 M |
| Cancel / replace (10 K-order book) | 172 | 0.009 | ~5.8 M |
| New price level (churn) | 292 | 0 | ~3.4 M |
| Maker + taker match — `Match` (into caller buffer) | 329 | **0** | ~3.0 M |
| Maker + taker match — `Process` (convenience wrapper) | 419 | 4 | ~2.4 M |

Median of 5 runs on an idle machine; ±10% run to run. The fractional allocation
counts are deliberate: Go prints `allocs/op` as integer division, so "0" can mean
anything under 1.0. Measured against `runtime.MemStats`, cancel really does allocate
~0.0002 objects per operation — and `Add` into a growing book really does allocate
2.01, which is what "pooled" means rather than "allocation-free".

**Book size is part of the result**, so it is stated. Cancel is given at two depths
because the benchmark's iteration count doubles as the book size, and the 4× spread
between them is cache behaviour, not code. Any book-level benchmark quoted without
its depth — including, until now, this one — is not comparable to anything.

**What these numbers measure.** In-process calls into the matching core, and
nothing else. `*types.Order` values are constructed before `b.ResetTimer()` and
passed directly to the engine, so the figures exclude order allocation, decoding,
validation at the API boundary, network I/O, and the session and order-entry
protocol layers. Those layers exist here (`internal/wire`, `pkg/orderentry`,
`cmd/obgw`) and none of them is measured on this page. They are a measure of the
matching algorithm and its data structures, not of end-to-end order latency in a
venue. Read them as a floor, and treat any real system built on this as strictly
slower.

Tail latency on a realistic ~90%-cancel / 10%-new flow (`Match` / `Cancel`):
**p50 83 ns · p99 167 ns · p999 250 ns**, 0 allocs/op — the p999 stays within ~3×
of the median. Absolute figures include `time.Now` overhead; the median-to-tail
shape is the signal.

**These are the bare `Engine`.** Most embedders use the `Runner` (the
concurrency-safe front), usually with a write-ahead log and an event sink. That
path allocates 3 allocs/op rather than 0, and adding group-committed durability
costs ~30× — dominated by `fsync`, and syncing on every command instead costs a
further ~210×. The zero-allocation claim above is about `Match` into a caller
buffer, not about the concurrent API.
See [docs/BENCHMARKS.md](docs/BENCHMARKS.md#the-durable-path) for the comparison
and for why the durable figures are given as ratios rather than nanoseconds.

**Scaling across cores.** A book cannot be parallelised — it is a single writer by
design — so the only axis that scales is books. `BenchmarkShards_Scaling` drives N
shards through `matching.Shards`, one producer goroutine per shard, on a 70/20/10
rest / cancel / marketable mix holding ~2 K resting orders per book:

| books | ns/op (aggregate) | ~ops/sec | vs. 1 book |
|---:|---:|---:|---:|
| 1 | 1,141 | 876 K | 1.00× |
| 2 | 608 | 1.64 M | 1.88× |
| 4 | 508 | 1.97 M | 2.24× |
| 6 | 500 | 2.00 M | 2.28× |
| 8 | 502 | 1.99 M | 2.27× |

`GOMAXPROCS=4` on the same M4 (4 performance cores), median of 5 × 10 s runs,
~5.7 minutes of measurement:
`GOMAXPROCS=4 go test -run '^$' -bench BenchmarkShards_Scaling -benchtime=10s -count=5 ./pkg/matching/`.
4 allocs/op, 663 B/op — order construction is *inside*
the timed loop here, unlike the table above, because a producer that builds its own
orders is what a shard actually faces.

**It is not linear, and that is the finding.** Four books on four cores returns
2.24×, not 4×, and books 5 through 8 return nothing further. Each shard is a *pair*
of goroutines — a producer blocked on its reply, and the matching goroutine draining
the queue — so past core count the machine is spent on the handoff rather than on
matching. Adding books beyond the core count buys queue headroom, not throughput.
The per-book figure is also not comparable to the single-threaded table above: it is
a different workload, and both the queue handoff and order allocation are inside it.

Reproduce with `make bench`. CI runs the benchmarks on every push. Methodology
and full results: [docs/BENCHMARKS.md](docs/BENCHMARKS.md).

---

## Architecture

The core is a strictly downward-layered set of packages. Research, simulation,
and the web demo depend on the core; the core depends on nothing above it.

```
web/ (React + TS)  ──▶  cmd/obwasm (Go → WASM)  ─┐
                                                 │
   backtest ▸ strategy ▸ sim ▸ signals ▸ marketdata
                                                 │
        ═══════════════ CORE LIBRARY ════════════▼═══════
           surveillance ▸ matching ▸ orderbook ▸ types
```

| Package | Responsibility |
|---|---|
| `pkg/types` | `Order`, `Trade`, `Instrument` (decimal ⇄ tick boundary), order-type wrappers |
| `pkg/orderbook` | the CLOB data structure and L1/L2/L3 snapshots |
| `pkg/matching` | the single-writer `Engine` and the concurrent `Runner` |
| `pkg/marketdata` | record, replay, and digest — deterministic recovery primitives |
| `pkg/observability` | Prometheus counters, gauges and histograms off the event stream |
| `pkg/wal` | checksummed write-ahead log, snapshots, recovery, and the replication tail |
| `pkg/orderentry` | authentication, per-account streams with resume, and the naming index |
| `pkg/gateway` | enforcing pre-trade admission: rate gate, speed-bump hook, audit |
| `pkg/surveillance` | market-abuse detectors: spoofing, OTR, pinging, ramping, cross-book |
| `pkg/auction` | uniform-price call auction with randomized close |

---

## Documentation

| Document | Contents |
|---|---|
| [API reference](https://pkg.go.dev/github.com/intrepidkarthi/orderbook) | Generated Go documentation for every package, with runnable examples, on pkg.go.dev. |
| [INTEGRATION.md](docs/INTEGRATION.md) | Embedding the engine: reference architecture, single-writer vs concurrent, WAL and recovery, market-data fan-out, observability, multi-symbol scaling, and a production checklist. |
| [CONFIG.md](docs/CONFIG.md) | Every configuration knob — `Instrument`, engine policy, time-in-force, order types — with defaults, validation, and the core-vs-layer boundary. |
| [THREAT-MODEL.md](docs/THREAT-MODEL.md) | Attacks hackers and market manipulators run against order books — spoofing, wash trading, marking the close, quote stuffing, oracle manipulation, and more — each mapped to a real enforcement case and to what the engine does (and doesn't) defend. |
| [EXCHANGE-ARCHITECTURE.md](docs/EXCHANGE-ARCHITECTURE.md) | How real venues (MetaTrader, Binance, Coinbase, Nasdaq/LMAX/CME/IEX, dYdX/Hyperliquid) implement matching, and the incidents that shaped this design. |
| [SPEC.md](docs/SPEC.md) | Architecture, the order model, core design decisions, and performance targets. |
| [BENCHMARKS.md](docs/BENCHMARKS.md) | Performance results, methodology, and how to reproduce. |
| [LEARN.md](docs/LEARN.md) | Order books and market making from first principles. |
| [research-roadmap.md](docs/research-roadmap.md) | The microstructure research agenda: OFI, Kyle's λ, Avellaneda–Stoikov, delta/CVD — and [what data it runs on](docs/research-roadmap.md#0-data-and-scope). |
| [research/ofi.md](docs/research/ofi.md) | Does order-flow imbalance predict the next move? Contemporaneous R² ≈ 0.24, predictive R² ≈ 0.0004 — a ~577× gap, and the little that remains points the other way. |
| [research/kyle-lambda.md](docs/research/kyle-lambda.md) | Price impact measured end to end: the λ a real book produces, why it scales as 1/depth, and what a block order costs against working the same quantity. |
| [research/order-flow.md](docs/research/order-flow.md) | Delta, CVD, and absorption against ground truth: a 94.5%-accurate aggressor rule builds a CVD wrong by 169%, and CVD divergence loses to a price-only control. |
| [RUNBOOKS.md](docs/RUNBOOKS.md) | Procedures for the failures this venue can have — torn log, corrupt snapshot, stuck matcher, dropped publisher batches, evicted subscriber — each written from the code that produces it, with the alert thresholds and what makes each one worse. Plus a step-by-step guide to debugging a live venue when you do not yet know what is wrong, and what has no runbook. |
| [SOAK.md](docs/SOAK.md) | Sustained load: what `cmd/obsoak` measures, the methodology that took three wrong versions to get right, and the correctness defect the first hour of it found. |
| [PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md) | What a venue actually needs, with an honest status for each item — what ships, what is deliberately absent, and what you would have to build. Production readiness is a property of a deployment, not of a library, and this says so. |
| [PROTOCOL.md](docs/PROTOCOL.md) | The binary order-entry protocol `cmd/obgw` speaks: framing, session and resume, every message, the reason-code vocabulary, and what is deliberately absent from the wire. |
| [REPLICATION.md](docs/REPLICATION.md) | The primary-backup reference: what "committed" means, the incarnation fence, drills D1–D7 — and §8, what building it actually found versus what the spec predicted. |
| [TRADE-BUST.md](docs/TRADE-BUST.md) | Annulling a published print on an append-only stream: the four things a bust deliberately does *not* rewind, and §7 on the durability gap writing the spec uncovered. |
| [CONSOLE-SPEC.md](docs/CONSOLE-SPEC.md) | The live console and the operator dashboard: why a browser WASM market instead of a feed-connected desktop app, and every panel mapped to the call that produces it. |
| [COMPATIBILITY.md](docs/COMPATIBILITY.md) | What will not move under you: which packages are frozen, which changes are breaking even though they look additive, and the test that makes the promise checkable rather than stated. |
| [TESTING.md](docs/TESTING.md) | The one rule that keeps the rest honest: a test does not count until it has been run against code broken the way it claims to detect. Five case studies from this repository of tests that were green for the wrong reason — or red for none. |
| [CHANGELOG.md](CHANGELOG.md) | Release history with breaking-change notes. |

**Design records.** Each of the following was written as a spec *before* the code
existed, and each ends with a section recording what building it actually found —
including where the spec turned out to be wrong about its own design. They are the
working record rather than the reference manual, and they are listed here because a
document nothing links to is a document nobody reads.

| Document | Contents |
|---|---|
| [MULTI-SYMBOL.md](docs/MULTI-SYMBOL.md) | One venue, many books: partitioned ids, a durable manifest, one command log per shard — and why there is deliberately no venue-wide sequence and no venue-wide "as of" instant. |
| [LAG-AND-SHED.md](docs/LAG-AND-SHED.md) | Counting what the venue refused and timing what it waits on, so overload is legible in the metrics instead of silent. |
| [SEMANTICS-VERSION.md](docs/SEMANTICS-VERSION.md) | Making a replay refuse rather than lie when the matching semantics have changed under a log that outlived them. |
| [JOURNAL-COMPLETENESS.md](docs/JOURNAL-COMPLETENESS.md) | Turning "the journal records everything that mutates the book" from a claim into a test that fails when it stops being true. |
| [REFERENCE-MATCHER.md](docs/REFERENCE-MATCHER.md) | An oracle that shares no code with the thing it checks — two sorted slices, deliberately naive — and the three engine defects it found. |
| [DIFFERENTIAL-FINDINGS.md](docs/DIFFERENTIAL-FINDINGS.md) | Deciding those three defects before fixing them, and §11 on the three places the decision document was wrong about its own reachability. |
| [PINNED-DEFECTS.md](docs/PINNED-DEFECTS.md) | One rejection that fails to undo and one that fails to announce: the two defects that were pinned as tests before they were repaired. |
| [ICEBERG-ADMISSION.md](docs/ICEBERG-ADMISSION.md) | A fat-finger cap a client could switch off by choosing an order type, and the audit that found five admission checks measuring the wrong quantity. |
| [ICEBERG-DURABILITY.md](docs/ICEBERG-DURABILITY.md) | A log record that cannot state the order it records, what a recovery is allowed to do about it, and the operator flag that makes the trade-off explicit. |
| [PERFORMANCE-ROADMAP.md](docs/PERFORMANCE-ROADMAP.md) | The performance and production roadmap, with every milestone's status checked against the code rather than against what a spec intended. |
| [TUTORIAL-SPEC.md](docs/TUTORIAL-SPEC.md) | The hands-on course (`web/learn.html`): learn the book by being every player in it. |
| [DEMO-SPEC.md](docs/DEMO-SPEC.md) | The hosted explainer that runs the real Go engine in the browser via WebAssembly. |

---

## Beyond the engine

The same core powers two additional layers, kept strictly above the library:

- **Research harness** — order-flow-imbalance, price-impact, and delta/CVD
  signals, a deterministic exchange simulator, an Avellaneda–Stoikov
  market-making backtest, and three reproducible studies in
  [docs/research/](docs/research) that test the popular claims against ground
  truth — and mostly refute them.
  Market-abuse surveillance (spoofing/layering and rate limits) and call-auction
  uncrossing are included. The engine is its own data source — per-order (L3 /
  market-by-order) event streams plus simulator ground truth; see
  [data and scope](docs/research-roadmap.md#0-data-and-scope) for what that does
  and doesn't cover.
- **Interactive demo** — the engine compiled to WebAssembly, running live in the
  [browser](https://intrepidkarthi.github.io/orderbook/) to visualize matching
  and market making.

```sh
go run ./examples/basic         # place two orders and watch them match
go run ./examples/eventfeed     # consume the event stream as an exec-report + position feed
go run ./examples/gateway       # edge controls: enforcing rate gate, speed bump, audit trail
go run ./examples/marketmaker   # backtest an Avellaneda–Stoikov maker
go run ./cmd/obdemo             # end-to-end matching demonstration
go run ./cmd/ofistudy           # is order-flow imbalance predictive, or just contemporaneous?
go run ./cmd/lambdastudy        # Kyle's λ: price impact, depth, and the cost of a block order
go run ./cmd/flowstudy          # delta/CVD/absorption vs ground truth: what survives a control
go run ./cmd/l2capture          # live order-flow imbalance on Coinbase data
```

---

## Contributing

Contributions are welcome — bug fixes, order types, signals, strategies,
surveillance detectors, protocol codecs, or research write-ups. Start with
[CONTRIBUTING.md](CONTRIBUTING.md) (`make check` before a PR), and open an issue
or discussion if you want to talk through an idea first.

**Good places to start** (open an issue to claim one):

- **Render the markdown docs to HTML** in the Pages build so the hosted docs stay
  in sync with source automatically.
- **Protocol codecs** — a FIX / OUCH / SBE adapter package translating the wire ↔
  `types.Order` and the `EventSink` stream ↔ execution reports.
- **New `EventSink` kinds** — emit the reserved `Triggered` / `BookDelta` events.
- **Tracing and structured logging** — `pkg/observability` covers metrics; spans
  across the gateway → queue → matcher → publisher path are not there.
- **More surveillance** — a quote-fading detector, or a wash-trade detector keyed
  on beneficial-ownership groups (the cross-account case the core can't see).
- **More signals** — micro-price, VPIN, or a queue-position model in `pkg/signals`.
- **GTD / DAY time-in-force** with expiry, and richer `Instrument` validation.

If the library is useful to you, a ⭐ helps other developers find it.

## Status

Actively developed. Releases follow semantic versioning; the public API uses
integer ticks/lots and a single-writer engine as of `v0.3.0`, with a
threat-model-driven market-integrity layer as of `v0.6.0`. Breaking changes are
gated behind minor-version bumps until a `v1.0.0` API freeze. See the
[CHANGELOG](CHANGELOG.md).

## Provenance

The design is informed by the author's prior production matching engine —
price–time priority, the map-plus-ladder book structure, and the matching
algorithm. This repository is a clean, independent re-implementation for research
and education, not a copy of that stack.

## License

[MIT](LICENSE) © Karthikeyan NG

<sub>Topics: order-book · matching-engine · limit-order-book · clob · market-making · avellaneda-stoikov · order-flow-imbalance · market-microstructure · backtesting · algorithmic-trading · quantitative-finance · webassembly · golang · exchange · price-time-priority</sub>
