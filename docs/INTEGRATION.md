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

The durable WAL storage backend (writing the event stream to disk/replication) is
yours to provide; the engine emits the sequenced stream and the snapshots.

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
3. Enable a WAL (downstream stage); verify snapshot + replay rebuilds identical
   state (`marketdata.Replay` + a book digest).
4. Bound every subscriber ring; conflate for slow consumers.
5. Add a `/metrics` endpoint (atomics only) + latency histogram off the hot path.
6. Seed idempotency on `ClientOrderID`; test duplicate/redelivery.
7. Golden-tape regression: capture a command log + trades, replay in CI, diff the
   `ValueDigest`.
8. Property/fuzz tests (quantity conservation, price-time priority) + soak/chaos
   (kill mid-run, recover, compare).
9. Load-test to worst-case burst; check p999 and queue depth under backpressure.
10. Graceful-drain shutdown wired to `SIGTERM`.

---

## What to build around the core

The engine is the matcher; a full venue needs these layers (intentionally *not* in
the core — see [CONFIG.md → what the core does not configure](CONFIG.md#what-the-core-deliberately-does-not-configure)):

- **Protocol adapters** — FIX/OUCH/SBE/WebSocket codecs translating the wire to
  `types.Order`, and translating the `EventSink` stream back to execution reports /
  market data. `examples/eventfeed` shows the pattern: consume the sequenced event
  stream into an exec-report feed and an order→deal→position projection (the
  MetaTrader-style lineage). A common deployment is as the **internal ECN behind an
  MT5 Gateway** — the real price-time crossing venue a B-book broker lacks.
- **Pre-trade risk** — credit, margin, buying power, position/notional caps, rate
  limits (in the gateway, before the sequencer).
- **Persistence** — durable WAL + snapshot store + replication.
- **Fees/clearing/settlement** — the core emits maker/taker + fill price; pricing
  and settlement live above.
- **Session/auction orchestration** — a controller driving halts, opens, and
  closing auctions.
