# Integration guide

How to embed the matching engine in a Go service and take it to production. This
guide covers the reference architecture, how to drive the engine (single-threaded
vs concurrent), determinism/recovery, market-data fan-out, observability, scaling,
and the pitfalls of a single-writer engine. For the exhaustive knob list see
[CONFIG.md](CONFIG.md); for the design rationale see [SPEC.md](SPEC.md); for the
generated API reference (with runnable examples) see
[pkg.go.dev](https://pkg.go.dev/github.com/intrepidkarthi/orderbook).

The library is a **pure matching core**: it owns the order book, the matching
algorithm, order lifecycle, deterministic sequencing, and market-data snapshots.
It deliberately leaves protocol codecs, authentication, pre-trade risk, fees,
credit, and the persistence backend to the layers around it — that boundary is
the same one Nasdaq INET, LMAX, and Coinbase draw.

---

## Install

```sh
go get github.com/intrepidkarthi/orderbook/pkg/matching
```

The packages you'll touch:

| Package | Role |
|---------|------|
| `pkg/types` | `Order`, `Trade`, `Instrument` (the decimal⇄tick boundary), order-type wrappers |
| `pkg/orderbook` | the CLOB data structure + L1/L2/L3 snapshots (usually via the engine) |
| `pkg/matching` | the `Engine` (single-writer core) and `Runner` (concurrency front) |
| `pkg/marketdata` | record / replay / digest — deterministic recovery primitives |

---

## Two ways to drive the engine

### 1. `Engine` — single-writer core

`matching.NewEngine` gives you the raw engine. It is a **single writer**: its
mutating methods own the book with no internal lock, so you must call them from
**one goroutine** (or serialise externally). This is the fastest path and the one
to use inside your own sequencer loop.

```go
e := matching.NewEngine(matching.DefaultConfig("BTC-USD"))

sell, _ := types.NewOrder("mm", "BTC-USD", types.SideSell, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
e.Process(sell)                                   // rests at 100

buy, _ := types.NewOrder("t", "BTC-USD", types.SideBuy, types.OrderTypeLimit, 101, 3, types.TIFGoodTillCancel)
res := e.Process(buy)                             // res.Trades, res.Status
```

For the **zero-allocation** hot path, use `Match` with a reused buffer — trades
are appended as values, nothing is heap-allocated per order:

```go
buf := make([]types.Trade, 0, 8)
buf, status, reason := e.Match(order, buf[:0])    // 0 allocs/op steady state
```

`Process` is the ergonomic wrapper that returns a `*MatchResult`; `Match` is the
low-latency path. Both go through the identical matching logic.

### 2. `Runner` — concurrency front (recommended for services)

`matching.NewRunner` wraps an engine with a single matching goroutine fed by an
MPSC command queue. **Many producers submit concurrently**; the writer applies
commands in FIFO order. This is the LMAX single-writer model — the only
synchronisation on the submit path is the queue hand-off, not a lock.

```go
r := matching.NewRunner(matching.RunnerConfig{
    Engine:    matching.DefaultConfig("BTC-USD"),
    QueueSize: 4096,
})
defer r.Close()

// From any goroutine, safely:
res := r.Process(order)                           // enqueue + wait
ch := r.SubmitAsync(order)                         // enqueue, don't block; result on ch

// Market-data reads delegate to the book's own lock — safe without the queue:
bid, qty, ok := r.BestBid()
snap := r.Snapshot(10)
```

**Rule of thumb:** reach for `Runner` in any multi-goroutine service. Use the bare
`Engine` only when you own the sequencing loop yourself.

---

## Reference architecture

A production matching path is a staged pipeline. Parallelism is applied *around*
the matching core, never *inside* it — the hot loop touches nothing but the book.

```
 producers        ┌───────────┐   ┌────────────┐   ┌──────────────┐   ┌───────────┐
 (gateways)  ───▶ │  Gateway  ├──▶│ Sequencer  ├──▶│   Matcher    ├──▶│ Publisher │
                  │  N gorout.│   │ 1 MPSC queue│   │ 1 gorout./sym│   │ fan-out   │
                  └───────────┘   └────────────┘   └──────────────┘   └───────────┘
   decode / auth /                 assign seq +      Engine owns the    drain events,
   pre-trade risk /                append to WAL     book, matches      publish snap +
   → int64 ticks                   (before matching) with no locks      deltas to subs
```

Mapped onto this library:

| Stage | Your code | This library |
|-------|-----------|--------------|
| **Gateway** | protocol decode, auth, pre-trade risk, `Instrument` conversion | `types.Instrument`, `types.NewOrder` |
| **Sequencer** | assign a monotonic seq, append to your WAL | `matching.Runner`'s command queue |
| **Matcher** | — | `matching.Engine` (single writer) |
| **Publisher** | fan out to subscribers off the hot path | `Engine.Snapshot` / `Book().SnapshotL3` + your own event pump |

Everything in "Your code" is an integration seam you own; the library is the
Sequencer+Matcher core plus the snapshot primitives.

---

## Backpressure & queue sizing

Use a **bounded** queue (`RunnerConfig.QueueSize`). Size it to cover your
worst-case burst × drain latency, and round to a power of two. Under sustained
overload, submit new liquidity with **`TrySubmit`** / **`TrySubmitAsync`**: they
enqueue non-blocking and return **`ErrQueueFull`** when the queue is full, so you
shed the order instead of growing memory unboundedly and triggering GC pauses. The
blocking `Cancel` still waits for space — so under overload you shed new orders but
**never drop cancels**, letting users de-risk (the Binance May-2021 lesson). Gauge
pressure with `Runner.QueueLen()`/`QueueCap()`, and use degraded states
(`SetCancelOnly`, `Halt`) to wind down. Reserve synchronous `Process` for tests and
low-rate control calls.

---

## Determinism, replay & recovery

The engine is **deterministic**: the same ordered command stream produces
byte-identical trades and book state. That is what makes replay, WAL recovery, and
honest backtests possible.

`pkg/marketdata` provides the primitives:

```go
// Record the order flow as it's processed.
rec := marketdata.NewRecorder(engine)
rec.Process(order)                    // forwards to the engine, captures the input
log := rec.Stream()                   // a replayable command log
tape := rec.Trades()

// Recover: a fresh engine replaying the same log rebuilds identical state.
fresh := matching.NewEngine(cfg)
marketdata.Replay(fresh, log)         // → identical book & trades

// Fingerprint a trade tape for golden-file / regression tests.
digest := marketdata.ValueDigest(tape)  // id-independent outcome hash
```

**WAL pattern for production:** append every accepted command to a durable log
*before* the matcher applies it, on a **downstream stage** so `fsync` never blocks
the matching goroutine. Recover by loading your newest **snapshot** and replaying
only the WAL entries after it — checkpoint on a cadence to bound replay time.
Idempotency comes from `ClientOrderID` + the sequence number: a duplicate seq on
redelivery is ignored, so trades are never double-emitted.

The library provides the primitives:

- **Ordered event stream.** Set `Config.EventSink` to receive every engine event
  (`Accepted`/`Rejected`/`Trade`/`Canceled`) with a **monotonic `Seq`** — the seam
  to feed a WAL writer, a market-data publisher, and drop copy. The sink must
  return fast (push to a ring/channel); it never back-pressures the matcher.
- **Snapshot + restore.** `Engine.TakeSnapshot()` captures a sequence-keyed copy
  of the resting book, pending stops, and counters; `RestoreEngine` / `LoadSnapshot`
  rebuild it. Recover by loading the newest snapshot and replaying the command log
  after its `Seq` — bounding replay to O(recent).

**`pkg/wal`** is the durable backend: an append-only, fsync'd command log
(`wal.Open`/`AppendSubmit`/`AppendCancel`/`Sync`, written write-ahead so no
acknowledged order is lost), snapshot persistence (`WriteSnapshot`/`ReadSnapshot`,
atomic), and replay-based recovery (`ReadAll` + `Restore` into a fresh engine).
It stops cleanly at a torn tail from a crash mid-write. Recover by loading the
newest snapshot then replaying the WAL entries after its `Seq`. Cross-datacenter
replication of the log is the only piece left to the operator.

---

## Market data

Read the book with the snapshot API (safe to call concurrently — the book has its
own RW-lock):

| View | Method | Shape |
|------|--------|-------|
| L1 (top of book) | `BestBid()`, `BestAsk()`, `Spread()`, `MidPrice()` | best price + aggregate size |
| L2 (price-aggregated) | `Snapshot(depth)` | `[]SnapshotLevel{Price, Quantity}` per side + `SequenceNum` |
| L3 (market-by-order) | `Book().SnapshotL3(depth)` | every resting order individually |

Every snapshot carries a `SequenceNum` (book version). For a production feed,
publish **one snapshot + a stream of incremental deltas** keyed to the same
sequence, so subscribers detect gaps and resync. Never let the matcher write to
subscriber sockets — hand events to a publisher goroutine with a bounded ring, and
**conflate** (collapse to the latest per level) for slow consumers rather than
blocking.

---

## Observability

Record latency into a lock-free, zero-allocation histogram on the critical path
and compute **p50/p99/p999 on a separate goroutine** — averaging pre-computed
percentiles across shards hides the tail, and not correcting for coordinated
omission makes your tail numbers lie. Expose via a scraped `/metrics` endpoint
reading atomics the matcher only increments:

- latency percentiles (p50/p99/p999) for submit / cancel / match
- command-queue depth, book depth (order count)
- match rate, reject rate (by reason)
- WAL fsync latency, snapshot duration

The repo's `BenchmarkLatency_CancelHeavy` shows the shape to target: **p50 ~83 ns,
p99 ~167 ns, p999 ~292 ns** on a cancel-heavy mix.

---

## Multi-symbol scaling

Keep **one engine goroutine per symbol** (or per shard of symbols). A router
hashes `symbol → shard`; shards share no mutable state, so they scale linearly
across cores. Pin hot shards to cores for cache locality. Scale *out* to multiple
processes only when a single box's cores or NIC saturate — but keep a symbol
wholly within one writer.

```go
runners := map[string]*matching.Runner{}
for _, sym := range symbols {
    runners[sym] = matching.NewRunner(matching.RunnerConfig{Engine: matching.DefaultConfig(sym)})
}
// route: runners[order.Symbol].SubmitAsync(order)
```

---

## Graceful shutdown

1. Stop the gateways (no new commands).
2. Let the `Runner` drain its queue into the matcher — call `Close()` (it drains,
   then stops the goroutine).
3. `fsync` the WAL and take a final snapshot.
4. Flush the publisher ring.

Thread a `context.Context` through your stages for clean cancellation.

---

## Pitfalls (single-writer engines)

- **Don't call the bare `Engine` from multiple goroutines.** It has no internal
  lock by design — use a `Runner`, or serialise to one goroutine.
- **Don't block the matcher** on I/O, logging, or a slow subscriber. Everything
  slow is a downstream stage.
- **Don't use an unbounded queue.** Bound it and define a shed/reject policy.
- **Watch GC.** Use the zero-alloc `Match` path with a reused buffer on the hot
  loop; avoid per-order allocation in your gateway too.
- **`SubmitAsync` errors are enqueue failures**, not order rejections — treat them
  distinctly (a rejected order comes back as a `MatchResult` with a status/reason).

---

## Getting started → production checklist

1. `go get`; build an `Instrument` (tick/lot) and one engine per symbol.
2. Wire producers through a `Runner`; bound `QueueSize`; define a shed policy.
3. Enable the durable WAL (`pkg/wal`: `AppendSubmit`/`AppendCancel` write-ahead,
   `Sync` at the durability point); verify snapshot + `Restore` rebuilds identical
   state (a book digest).
4. Turn on the pre-trade risk controls that fit your market (see *Market integrity
   & pre-trade risk* above) and wire a surveillance `Monitor`.
5. Bound every subscriber ring; conflate for slow consumers.
6. Add a `/metrics` endpoint (atomics only) + latency histogram off the hot path.
7. Seed idempotency: set `Config.DedupClientOrderIDs` (and/or dedup in your
   gateway); test duplicate/redelivery.
8. Golden-tape regression: capture a command log + trades, replay in CI, diff the
   `ValueDigest`.
9. Property/fuzz tests (quantity conservation, price-time priority) + soak/chaos
   (kill mid-run, recover, compare).
10. Load-test to worst-case burst; check p999 and queue depth under backpressure.
11. Graceful-drain shutdown wired to `SIGTERM`.

---

## Market integrity & pre-trade risk

The engine ships a set of **opt-in, zero-cost-when-off** defences against the
attacks real venues face — spoofing, quote stuffing, oracle-mark manipulation,
fat-finger blowups, liquidation cascades, and more. Each is grounded in a named
enforcement case in [THREAT-MODEL.md](THREAT-MODEL.md); each knob is documented in
[CONFIG.md](CONFIG.md). They split across four layers, matching where a real
venue puts them.

**1. In-core pre-trade controls (`matching.Config`).** Enforced on the cold reject
path, so the zero-alloc hot path is untouched; `Privileged` (liquidation/ADL)
orders are exempt from the caps; the time-based checks are bypassed on
deterministic replay.

```go
cfg := matching.DefaultConfig("BTC-USD")
cfg.MaxOrderQty = 10_000            // fat-finger size cap (ErrOrderExceedsMaxQty)
cfg.MaxOrderNotional = 50_000_000   // fat-finger notional cap; int64 overflow always rejected
cfg.MinOrderQty = 1                 // dust floor
cfg.MaxOrdersPerAccount = 500       // per-account resting-order cap (anti-stuffing)
cfg.MinRestingTime = 20 * time.Millisecond // anti-spoofing: no instant cancel
cfg.DedupClientOrderIDs = 4096      // reject a replayed (user, ClientOrderID)
cfg.MaxMarkStep = decimal.RequireFromString("0.10")   // mark can't jump >10%…
cfg.MinMarkDepth = 50               // …nor move to a price the book doesn't back
cfg.MaxForceTradeQty = 100          // liquidations must be chunked
cfg.BandBreachPause = 30 * time.Second // band breach → timed halt + auto-resume
cfg.Guardrail = matching.Guardrail{MaxTrades: 10_000, Window: time.Second} // Knight tripwire
eng := matching.NewEngine(cfg)
```

**2. Surveillance (`pkg/surveillance`) — detect, alert.** Feed the engine's
`EventSink` stream (translated to `surveillance.Event`) into a `Monitor` of
detectors: `SpoofDetector`, `RateLimiter`, `OTRDetector` (order-to-trade ratio),
`CloseMarkingDetector`, `RampingDetector`, `PingingDetector`. For a multi-symbol
venue, a `CrossBookMonitor` correlates manipulation across books (cross-product
abuse). Detectors *alert*; they never touch the match path.

**3. Gateway (`pkg/gateway`) — enforce at the edge.** `gateway.New(runner, cfg)`
wraps a `Runner` with an enforcing token-bucket `RateGate` (rejects an over-quota
account with `ErrThrottled` — cancels are never gated) and an asymmetric speed
bump on liquidity-taking orders (latency-arbitrage defence). `examples/gateway`
also shows a CAT-style audit export off the event stream.

**4. Auction (`pkg/auction`) — open, close, recover.** `auction.AuctionSession`
runs a uniform-price call auction for the open, the close, or halt recovery
(cancel-only → auction → continuous). `RandomizedClose` jitters the uncross time
deterministically (replay-safe) to defeat marking-the-close.

---

## What to build around the core

Beyond the controls above, a full venue still needs these layers — intentionally
*not* in the core (see [CONFIG.md → what the core does not
configure](CONFIG.md#what-the-core-deliberately-does-not-configure) and
THREAT-MODEL.md §6 for why the boundary is drawn here):

- **Protocol adapters** — FIX/OUCH/SBE/WebSocket codecs translating the wire to
  `types.Order`, and translating the `EventSink` stream back to execution reports /
  market data. The `pkg/fix` example demonstrates this boundary with FIX
  NewOrderSingle decoding, cancel handling, and execution-report encoding.
  `examples/eventfeed` shows the event translation pattern: consume the sequenced
  event stream into an exec-report feed and an order→deal→position projection.
  A common deployment is as the **internal ECN behind an MT5 Gateway** — the real
  price-time crossing venue a B-book broker lacks.