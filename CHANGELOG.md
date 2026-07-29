# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (pre-1.0: minor
versions may include breaking changes).

## [Unreleased]

## [0.10.0] - 2026-07-29

Adds the network edge. v0.9.0 fixed what was broken; this release adds what was
absent — a client-facing order-entry protocol, a session layer, per-account
message sequencing with gap-free resume, and a reference gateway that speaks it
over TCP.

With this, *production-grade embeddable matching core with a demonstrated network
seam* is a claim the tests support. Both qualifiers are load-bearing:
production-grade describes the core, embeddable concedes the venue is not here.

### Added

- **`cmd/obgw`** — a reference TCP order-entry gateway. Authentication defaults to
  deny, so an unconfigured venue rejects everyone rather than admitting them.
  One goroutine per connection, admission control before enqueue, drain on
  SIGTERM.
- **`internal/wire`** — SoupBinTCP 3.00 framing carrying fixed-width big-endian
  payloads, no new dependencies. Frozen by 12 byte-exact golden vectors: a field
  that moves silently reinterprets every message a deployed client sends, so
  changing a layout means bumping `Version`, not editing a vector. Documented in
  [docs/PROTOCOL.md](docs/PROTOCOL.md).
- **`pkg/orderentry`** — per-account outbound streams that outlive connections.
  A `Session` is a socket; a `Stream` is an account's sequence. That separation
  is what makes resume possible: a maker whose resting order fills while its
  connection is down still has the execution waiting when it returns.
- **Gap-free resume**, scoped to a venue incarnation. A restart mints a new
  incarnation id, so a stale cursor is refused rather than served different
  content under numbers the client believes it already has. A cursor older than
  the retention ring is refused explicitly; the client is never told it is up to
  date when it is not.
- **`Engine.Reduce` / `Runner.Reduce`** — in-place down-size retaining queue
  position. The one order-entry operation a gateway provably cannot build
  outside the writer goroutine: cancel-then-new sends the order to the back of
  its level, which for a market maker managing size is a material loss. Size
  increases and price changes are rejected rather than silently reinterpreted,
  because an order that could grow in place would let a participant reserve a
  spot in the queue.
- **`matching.MultiSink`** and **`Runner.TryEnqueue` / `TryEnqueueCancel`**.
  Fire-and-forget submission never hands the engine-owned order back to a
  connection goroutine, which removes the whole read-after-submit race class.
- **`Gateway.Allow`** splits the admission decision from forwarding, so the gate
  can sit in front of `TryEnqueue`.

### Security

- The wire carries **no account and no engine order id**. Orders are named only
  by the client's own `ClOrdID`, scoped to the authenticated session. The engine
  cancels by `(orderID, userID)` and self-trade prevention lets one account
  observe another's resting orders, so a wire carrying either field would let a
  client name an order it does not own — there is no field in which to express
  it. Two accounts using the identical `ClOrdID` cannot reach each other's
  orders, and a test asserts it.
- `STPMode` and the privileged flag are absent from the wire: the first is venue
  policy, the second a liquidation capability that must never be client-settable.
- A reduce or cancel against another account's order is refused
  indistinguishably from a missing order, so a probe cannot learn that someone
  else's order exists.

### Fixed

- `Server.Close` deadlocked against its own connection handlers. Closing a
  listener stops new accepts and does nothing to established sockets, so the
  drain waited forever on handlers parked in a read. Found by the integration
  test on its first run, which is the reason the reference server exists.

### Changed

- README now claims *production-grade embeddable matching core with a
  demonstrated network seam*, and states plainly what is still yours: TLS,
  credential storage, multi-symbol routing, clearing, and any HA topology. The
  library ships the seams for primary-backup and deliberately not the consensus.
- **Production-readiness remains a property of a deployment, not of a library.**
  That sentence stays in the README permanently.

## [0.9.0] - 2026-07-29

A correctness release. A production-readiness audit of the recovery, event and
concurrency paths found that several controls the documentation marked as
shipped did not actually hold, and one execution path lost fills outright. Each
item below was reproduced with a failing test before it was fixed.

The headline is uncomfortable and worth stating plainly: the parts most likely
to hurt an embedder were not the missing pieces the README already disclosed,
but the pieces it claimed were finished.

### Fixed

- **Lost executions on OCO entry.** `ProcessOCO` called `submitStopInto` and
  discarded all three return values. A stop leg triggering on entry still
  settled through the book — filling and removing real makers and moving the
  last trade price — while reaching neither the event stream nor any
  `MatchResult`. The counterparty's fill was gone permanently. The same discard
  meant the stop leg was never announced, so its later fills referenced an order
  id no consumer had seen.
- **The event stream did not reconstruct the book**, despite `event.go` saying
  it did. Eight distinct defects: market and IOC remainders announced as
  `ACCEPTED` and terminated with no delete; iceberg refills re-adding a slice
  under the same id silently, so the owner of a live iceberg went dark after its
  first slice; the surviving OCO leg removed silently; STP `CANCEL_OLDEST` and
  `CANCEL_BOTH` removing the maker with no delete; STP `DECREMENT` shrinking
  both sides with no trade and no event at all; cascade-fired stops settling
  without ever being announced; and a rejected FOK publishing trades that
  `reverseTrade` had already undone. Emission is now composed in causal order
  per command, with sequence numbers assigned at publish time — numbering at
  record time produced a stream whose `Seq` ran backwards.
- **Snapshot restore was lossy in five ways.** Trailing stops vanished outright
  (they live only in the engine's map, never in either book); icebergs came back
  as a bare displayed slice with the reserve gone; OCO pairings were lost,
  leaving two independent orders either of which could fill; `markPrice` reset
  to zero, and both manipulation clamps skip when the current mark is zero, so
  the first post-recovery update was unconstrained; and the self-output
  guardrail's window reset, handing back a full budget immediately after a
  restart.
- **The client-order-id duplicate guard was empty after recovery**, on both the
  snapshot and the replay path. It stopped enforcing precisely when a client is
  most likely to resend — after the venue restarts — which is the FIX PossDup
  case it exists to cover.
- **Snapshot and log could not be joined.** `EngineSnapshot` carried no log
  position, and `INTEGRATION.md` told callers to replay entries after `Seq` —
  the engine's *order* sequence, unrelated to log positions. Following the
  documented recipe replayed an arbitrary slice of the log.
- **Replay bypassed the deterministic admission checks**, including the int64
  notional overflow guard. Because the log is write-ahead it records commands as
  submitted, not accepted, so an order the live engine rejected rested on the
  recovered book.
- **`WriteSnapshot` was atomic but not durable** — no fsync of either the file
  or the parent directory, so a crash could leave a correctly-named empty
  snapshot that `Recover` would load.
- **`Runner.Close` panicked in-flight producers** by closing the shared command
  queue from the consumer side. Correct shutdown required proving every producer
  had stopped, which a server with a goroutine per connection cannot do.
- **`pkg/gateway` data-raced** at the topology its own package doc recommended.

### Added

- `wal.Checkpoint`, `wal.Recover` and `wal.RestoreAfter` — the snapshot/log join
  expressed once, in code, instead of as prose for callers to reimplement.
- `Runner.Checkpoint` and `RunnerConfig.Log`: a write-ahead seam that logs each
  mutating command before applying it, tracking the sequence of the last command
  *applied* rather than appended.
- `Engine.CancelAllForUser` / `Runner.CancelAllForUser` — the operator kill
  switch. It pulls resting orders, pending stops and trailing stops, ignores
  `MinRestingTime`, and announces every removal.
- `matching.ErrShuttingDown` and an idempotent, fenced `Runner.Close`.
- `EventReplaced` now carries in-place size changes that keep queue position.
- Crash-recovery property test over a 2,000-command generated tape, replacing a
  determinism gate that ran on six hand-written orders.
- `TestEventStreamReconstructsBook`: 22 scenarios asserting the event stream
  rebuilds the engine's own L3 book, order-for-order and lot-for-lot.
- Benchmarks for the durable path (`Runner` + `EventSink` + WAL), which had no
  published number while the README advertised the bare engine's.

### Changed

- **Docs corrected where they overclaimed.** `THREAT-MODEL.md` row 16 no longer
  marks the taker speed bump as an enforcing control — it is an observation hook
  that reports and does not delay. The README no longer implies the
  zero-allocation figure applies to the concurrent API: `Match` into a caller
  buffer allocates nothing, `Runner.Process` allocates 4/op, and durability adds
  roughly an order of magnitude.

## [0.8.0] - 2026-07-26

Completes the microstructure research agenda. All four roadmap items are now
implemented, measured, and written up in [docs/research/](docs/research) — and
most of the popular claims they test do not survive contact with ground truth.

### Added

- **Order-flow study** (research-roadmap.md §4), completing the research agenda:
  `signals.CVD`, `signals.TickRuleSide` / `signals.LeeReadySide` (aggressor
  inference, there to be measured against ground truth rather than used in place
  of it), `signals.AbsorptionConfig`, `signals.Divergence`, and
  `signals.WilsonInterval`; `study.RunInference`, `study.RunDivergence` (with a
  price-only control arm), `study.RunAbsorption`, `study.RunSqueezeDemo`, and
  `study.PoolSignals`; `cmd/flowstudy` runs them all. Write-up:
  [order-flow.md](docs/research/order-flow.md) — a 94.5%-accurate tick rule
  builds a CVD wrong by 169% (sometimes with the opposite sign); CVD divergence
  beats its base rate but loses to a price-only control, so the CVD half adds
  nothing; absorption predicts nothing, though the mechanism is demonstrably
  real.

## [0.7.0] - 2026-07-26

The research release: the microstructure agenda's first two items measured,
written up, and checked in — plus an honest account of what data the harness
runs on.

### Added

- **Kyle's λ price-impact study** (research-roadmap.md §2): `signals.SignedFlow`
  and `signals.EstimateLambda` (fits `ΔP = λ·y`, reporting λ with R² and N);
  `study.RunKyleLambda` (the λ that emerges from a real book, with none
  configured), `study.RunKyleDepth` (λ ∝ 1/depth sweep), `study.RunExecution`
  (block vs sliced execution, scored on implementation shortfall plus realized
  and permanent impact), and `study.RunLambdaCalibration` (estimator validation);
  `cmd/lambdastudy` runs them all.
- **`docs/research/`**, the results directory the methodology has always
  required, with two write-ups.
  [kyle-lambda.md](docs/research/kyle-lambda.md): λ ∝ 1/depth holds, slicing is
  ~8% cheaper per lot and completes where a block leaves ~8% unfilled, and
  permanent impact is unchanged either way — slicing buys back the temporary
  component, not the permanent one. [ofi.md](docs/research/ofi.md): the §1
  result written up properly over ten seeds, including that the predictive slope
  is negative in nine of them.
- **`docs/research-roadmap.md` §0 "Data and scope"**: what the engine emits
  (per-order L3/MBO event streams), what simulator ground truth adds, what is
  missing (real capture is L2-only), and why "L4" is a vendor label rather than
  an exchange tier.

### Fixed

- **Overstated OFI figure in the docs.** `LEARN.md` claimed a contemporaneous
  R² ≈ 0.33 and `cmd/ofistudy` called it "~a third" of the same-interval move.
  The measured mean across ten seeds is **0.1685** (range 0.0704–0.2397); the
  three seeds the CLI printed were the joint highest of the first ten. Both
  numbers corrected and the CLI widened to ten seeds. The verdict is unchanged —
  predictive R² was always ~0.0003.

## [0.6.0] - 2026-07-23

The market-integrity release: a research-grounded threat model
([docs/THREAT-MODEL.md](docs/THREAT-MODEL.md)) and the defenses it prioritized,
plus durable persistence.

### Added

- **Durable WAL** (`pkg/wal`): append-only, length-prefixed command log written
  write-ahead, torn-tail-safe `Restore` into a fresh engine, and atomic snapshot
  write/read — the LMAX/Binance journal-plus-snapshot recovery model.
- **Threat model** (`docs/THREAT-MODEL.md`): a 27-attack catalogue, top-ten deep
  dives, a what-we-defend inventory, and a prioritized roadmap — every entry tied
  to a real enforcement action or incident.
- **In-core pre-trade risk & anti-manipulation controls** (`matching.Config`,
  opt-in, cold-path, `Privileged`-exempt, bypassed on deterministic replay):
  `MaxOrderQty` / `MaxOrderNotional` (fat-finger) and `MinOrderQty` /
  `MinOrderNotional` (dust) caps with an int64 notional-overflow guard;
  `MaxOrdersPerAccount`; `MinRestingTime` (anti-spoofing); `DedupClientOrderIDs`
  (idempotency); `MaxMarkStep` **and** `MinMarkDepth` / `MarkDepthBand` (anti
  oracle-pump — single jump and patient drag); `MaxForceTradeQty` (chunked
  liquidation); `BandBreachPause` (timed halt + auto-resume); `IcebergPeakJitter`
  (anti-sniffing). `HALTED` / `RESUMED` events on guardrail trips and pauses.
- **Surveillance detectors** (`pkg/surveillance`, alert-only): `OTRDetector`
  (order-to-trade ratio), `CloseMarkingDetector`, `RampingDetector`,
  `PingingDetector`, and `CrossBookMonitor` (cross-product correlation), alongside
  the existing spoof and rate detectors.
- **Call-auction session** (`pkg/auction`): `AuctionSession` for the open, close,
  and halt recovery, with a replay-safe `RandomizedClose` that defeats marking the
  close.
- **Edge gateway** (`pkg/gateway`): an enforcing token-bucket `RateGate` (rejects
  over-quota orders; cancels never gated) and an asymmetric taker speed bump, with
  `examples/gateway` demonstrating them plus a CAT-style audit export.
- `OrderBook.OrdersByUser` and `OrderBook.DepthWithin`; `Engine.SetReplaying`.
- Docs: `docs/INTEGRATION.md` "Market integrity & pre-trade risk" section; every
  new knob in `docs/CONFIG.md`; refreshed `docs/SPEC.md` package layout and
  market-integrity section; README highlights and docs table.

### Changed

- **BREAKING:** `Engine.SetMarkPrice(price int64)` now returns `error` (it rejects
  a mark update that violates `MaxMarkStep` / `MinMarkDepth`, or a negative price).
  `Runner.SetMarkPrice` is unchanged (async, fire-and-forget).
- `EngineSnapshot` gained `PausedUntil` so a mid-pause snapshot restores exactly.

## [0.5.0] - 2026-07-23

Phase C — real-world features. Self-trade prevention with taker-decides,
`DECREMENT` mode, cross-account `TradeGroupID`, and a `Privileged` exemption; a
mark/index-driven price band (`SetMarkPrice`) plus a `ForceTrade` liquidation/ADL
primitive; a per-symbol `Shards` router; an event-stream adapter example
(`examples/eventfeed`); and a uniform-price batch-auction mode (`auction.BatchAuction`).

## [0.4.0] - 2026-07-22

Determinism & integration seam. **Phase A:** an injectable `Clock` (byte-identical
replay), replay-equivalence and zero-allocation CI gates, feature-flagged exotic
order types (`DisabledClasses`), degraded states (`Open` / `CancelOnly` / `Halted`),
and a self-output `Guardrail`. **Phase B:** a monotonic `Event.Seq` + typed
`EventSink` event stream, `TakeSnapshot` / `RestoreEngine`, and bounded
backpressure (`TrySubmit` → `ErrQueueFull`).

## [0.3.0] - 2026-07-22

Production-grade low-µs core (P0–P6). O(1) cancel via intrusive linked lists, a
zero-allocation `Match` path (pooled nodes/levels + caller trade buffer), and a
single-writer `Runner` (MPSC command queue, lock-free hot path). Tail-latency,
fuzz, soak, and WAL-replay-recovery suites.

## [0.2.0] - 2026-07-22

**BREAKING:** integer-exact pricing. Prices are `int64` ticks and quantities
`int64` lots; a per-symbol `Instrument` converts decimals only at the boundary.
Engine-assigned monotonic `int64` ids replace UUIDs.

## [0.1.0] - 2026-07-21

Initial release: a decimal-first CLOB and matching engine with the full order
surface (limit, market, stop/stop-limit, iceberg, post-only, pegged, OCO,
trailing), GTC/IOC/FOK, self-trade prevention, a price-band circuit breaker, FIFO
and pro-rata allocation, L1/L2/L3 market data, a surveillance starter kit, and a
market-microstructure research harness with a WebAssembly demo.

[Unreleased]: https://github.com/intrepidkarthi/orderbook/compare/v0.8.0...HEAD
[0.8.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/intrepidkarthi/orderbook/releases/tag/v0.1.0
