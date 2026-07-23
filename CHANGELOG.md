# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (pre-1.0: minor
versions may include breaking changes).

## [Unreleased]

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
and a self-output `Guardrail`. **Phase B:** a monotonic `EngineSeq` + typed
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

[Unreleased]: https://github.com/intrepidkarthi/orderbook/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/intrepidkarthi/orderbook/releases/tag/v0.1.0
