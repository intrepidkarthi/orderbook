# orderbook — Specification & Design

> A fast, embeddable limit-order-book and matching engine in Go — and a research
> harness plus an animated, hosted explainer for market microstructure, market
> making, and strategy backtesting.

Status: **draft v0.1** · Author: Karthikeyan NG · Last updated: 2026-07-20

Companion documents:
- [`research-roadmap.md`](research-roadmap.md) — the microstructure research agenda.
- [`DEMO-SPEC.md`](DEMO-SPEC.md) — the animated web demo and hosting spec.

---

## 1. Vision

This repository has a deliberate **three-part mandate**:

1. **A library others can use.** A clean, efficient, well-tested, embeddable
   central limit order book (CLOB) and matching engine — something you can
   `go get` and drop into an exchange, a simulator, a teaching tool, or a
   trading application.

2. **A research harness.** A place to *build*, *backtest*, and *honestly
   evaluate* market-microstructure ideas — order-flow imbalance, market making
   (Avellaneda–Stoikov), price impact (Kyle's lambda), delta/CVD, adverse
   selection — with reproducible experiments rather than screenshots and
   anecdotes.

3. **A hosted, animated explainer.** A web demo that runs the *real* engine in
   the browser (via WebAssembly) and teaches — with animation — how an order
   book works, how matching works, and how market making works. Useful to
   anyone trying to understand these systems.

These goals pull in slightly different directions: a reusable core wants to be
lean and dependency-light; a research harness and a rich UI want heavier
tooling. The design reconciles them with one rule, enforced everywhere:

> **Strict downward layering.** The core library never imports the research,
> simulation, strategy, or presentation layers. Dependencies flow one way only.

That single constraint lets one repository be a credible engineering artifact, a
research platform, *and* an educational product without any of the three
compromising the others.

---

## 2. Goals & non-goals

### Goals
- Correct, **deterministic** matching with **price–time priority** (and
  optional pro-rata) — money is **never** a float.
- Embeddable: a small, stable public API with minimal dependencies.
- Fast enough to be interesting: hundreds of thousands of ops/sec/core (§7),
  with benchmarks tracked in-repo.
- A full, real-world **order-type and market-integrity** surface (§5) — the set
  popular exchanges actually ship, not just market/limit.
- A research layer that turns microstructure claims into falsifiable experiments.
- An animated demo that compiles the core to WASM and runs it live in-browser.

### Non-goals (for now)
- **Not** a nanosecond-latency colocated HFT engine. This is Go, not C++ with
  kernel bypass; we compete on clarity, correctness, and throughput.
- **Not** a full exchange (no auth, custody, KYC, settlement) — though the core
  is designed to be embeddable inside one.
- **Not** financial advice or a "profitable strategy" generator. The research
  tooling exists to *understand and stress-test* ideas — including proving some
  don't survive costs.

---

## 3. Architecture

Layered; dependencies flow downward only. The **presentation** column is a
sibling that consumes the core through the WASM boundary.

```
┌───────────────────────────────┐        ┌──────────────────────────────┐
│  apps / cmd                    │        │  web/ (React + TS)           │
│  demos, tools, experiment      │        │  animated educational demo   │
│  runners                       │        │  ── consumes ──▶ obwasm      │
├───────────────────────────────┤        └──────────────┬───────────────┘
│  backtest   PnL, inventory,    │                       │ (WASM boundary)
│             Sharpe, adverse    │                       │
│             selection, reports │        ┌──────────────▼───────────────┐
├───────────────────────────────┤        │  cmd/obwasm                  │
│  strategy   market making      │        │  Go→WASM bindings over core  │
│             (Avellaneda–Stoikov)        └──────────────┬───────────────┘
├───────────────────────────────┤                       │
│  sim        exchange simulator,│                       │
│             synthetic agents   │                       │
├───────────────────────────────┤                       │
│  signals    OFI, imbalance,    │                       │
│             delta/CVD, lambda  │                       │
├───────────────────────────────┤                       │
│  marketdata feeds, L2/L3       │                       │
│             replay, capture    │                       │
╞═══════════════════════════════╪═══════════════════════╡
│  CORE LIBRARY (standalone, importable, dependency-light)               │
│    surveillance  spoofing/layering, cascade, rate/velocity limits      │
│    matching      price-time (+ pro-rata), TIF, STP, auctions           │
│    orderbook     CLOB structure, L2/L3, depth, snapshots               │
│    types         Order, Trade, Side, errors (decimal)                  │
└───────────────────────────────────────────────────────────────────────┘
```

Everything above the double line is research/tooling/presentation and may depend
on the core. Nothing below it may depend on anything above it.

---

## 4. Package layout

```
orderbook/
├── go.mod                      module github.com/intrepidkarthi/orderbook
├── README.md
├── LICENSE                     MIT
├── docs/
│   ├── SPEC.md                 this document
│   ├── research-roadmap.md     microstructure research agenda
│   └── DEMO-SPEC.md            animated demo + hosting spec
├── legacy/
│   └── orderbook_v0.go         original float64 prototype (frozen, build-ignored)
├── pkg/
│   ├── types/                  Order, Trade, Side, OrderType, TIF, errors
│   ├── orderbook/              CLOB: price levels, ladder, depth, L2/L3, snapshot
│   ├── matching/               matching engine (price-time, pro-rata, auctions)
│   ├── surveillance/           STP, spoofing/layering, cascade, rate/velocity limits
│   ├── marketdata/             feed interfaces, L2/L3 replay, live capture
│   ├── signals/                OFI, imbalance, delta, CVD, Kyle's lambda
│   ├── strategy/               market-making strategies (AS, …)
│   ├── sim/                    exchange simulator + synthetic agents
│   └── backtest/               harness + performance metrics
├── cmd/
│   ├── obdemo/                 minimal end-to-end matching demo (CLI)
│   └── obwasm/                 Go→WASM entrypoint binding the core for web/
└── web/                        React + TypeScript animated demo (see DEMO-SPEC.md)
```

---

## 5. The order model (real-world surface)

The set of order types, matching modes, and market-integrity controls that
production venues (Binance, Coinbase, Kraken, CME, Nasdaq, LMAX, dYdX,
Hyperliquid) actually ship. Not all land day one — see the milestones (§10) —
but the core is designed so each slots in without redesign.

### 5.1 Order types
| Type | Notes |
|---|---|
| **Market** | Takes liquidity immediately; may sweep multiple levels. |
| **Limit** | Rests at a price or better. |
| **Stop / Stop-Limit** | Triggers a market/limit order when the market touches a stop price (stop-loss, take-profit). |
| **Iceberg / Reserve** | Shows only a *display* quantity; the hidden remainder auto-refills as the tip fills. |
| **Hidden / Dark** | Fully non-displayed resting liquidity. |
| **Post-Only** | Maker-only; rejected (or repriced) if it would cross and take. |
| **Pegged** | Price tracks a reference (mid / bid / ask; primary & market peg). |
| **OCO** | One-cancels-other (e.g., take-profit + stop-loss bracket). |
| **OTO / Bracket** | One-triggers-other; entry that arms exits on fill. |
| **Trailing stop** | Stop that follows the market by an offset. |
| **Auction orders** | Market/Limit-on-open, Market/Limit-on-close. |
| **Reduce-only / Min-qty** | Derivatives & execution constraints. |

### 5.2 Matching & execution
- **Price–time priority (FIFO)** as the default; **pro-rata** and price-time/
  pro-rata **hybrid** allocation as a per-symbol mode (common in futures).
- **Time-in-force:** GTC, GTD, DAY, IOC, FOK.
- Trades print at the **maker's** (resting) price.
- **Self-trade prevention:** cancel-newest / cancel-oldest / cancel-both / decrement.
- **Fees:** maker–taker with tiers and rebates (modeled for backtests).
- **Auctions:** opening / closing / **volatility** auctions with single-price
  uncrossing.
- **Guards:** tick size, lot size, min notional, **price bands / LULD** circuit
  breakers.

### 5.3 Market integrity & surveillance
A first-class layer, because "how manipulation looks and gets caught" is part of
what this project teaches.
- **Spoofing / layering** detection: large orders placed away from the touch and
  cancelled before execution; order-book pressure that evaporates.
- **Stop-hunt / liquidity-pocket** dynamics: clustered stops swept in a cascade
  then snapping back, plus **stop-cascade protection** (halt when triggers
  chain past a threshold).
- **Quote stuffing, momentum ignition, wash trading** heuristics.
- **Rate & ratio limits:** message rate, **order-to-trade / cancel ratio**,
  velocity limits, position limits.
- **Controls:** kill switch / mass-cancel, cancel-on-disconnect, trading halts,
  fat-finger / price-collar checks.

### 5.4 Market data
- **L1** (top of book), **L2** (aggregated per price), **L3 / MBO** (full
  order-by-order).
- Snapshots + **incremental diffs**, monotonic **sequence numbers**, gap
  detection.
- Trade tape (time & sales), depth heatmap, OHLCV aggregation.

---

## 6. Core design decisions

Each records the choice, the rationale, and the alternative we deferred.

### 6.1 Price & quantity — **decimal first**
- **Choice:** `shopspring/decimal` for all prices and quantities.
- **Why:** money must not accumulate binary floating-point error. The `legacy/`
  prototype used `float64` — precisely the bug this project fixes.
- **Deferred:** an `int64` fixed-point "ticks" fast path behind the same
  interface once benchmarks justify it. Correctness before speed.

### 6.2 Book structure — **map + sorted ladder**
- **Choice:** `map[price]→*PriceLevel` (O(1) lookup) + a price-sorted slice with
  **binary-search insertion** (O(log n) new-level insert, O(1) best access).
  Each level is a **FIFO queue** → price–time priority.
- **Why:** best bid/ask is read constantly; it must be O(1). New price levels are
  comparatively rare.
- **Deferred:** heap / balanced BST / skip-list / radix-bucketed ladders — noted
  as benchmark alternatives.

### 6.3 Concurrency — **single writer per book**
- One mutex-guarded writer per symbol; readers use snapshots. Scale-out is
  *across symbols* (one book/goroutine each), not by parallelizing one book.
- **Deferred:** sharding, lock-free structures, single-writer ring buffers.

### 6.4 Determinism — **a hard requirement**
Given the same ordered input stream, the engine produces byte-identical trades
and state. No wall-clock or RNG in the matching path; anything that must be
deterministic (timestamps, IDs) is injected. Determinism is what makes replay,
golden-file tests, and honest backtests possible.

### 6.5 Identifiers
- **UUIDv7** (time-ordered) at the boundary; monotonic **sequence numbers**
  internally for deterministic ordering and replay.

---

## 7. Performance targets

Aspirational baselines from the author's prior matching engine on an Apple M4;
treated as regression targets once benchmarks exist — not marketing.

| Metric | Target (per core) |
|---|---|
| Order insert (resting) | ≥ 500k / sec |
| Order match | ≥ 200k / sec |
| Best bid/ask read | < 1 µs |
| Hot-path allocations | bounded, measured |

Benchmarks live in-repo (`go test -bench`), tracked over time, run under the
race detector in CI.

---

## 8. Testing & quality

- **Unit tests** per package.
- **Invariant/property tests:** book never crosses (best bid < best ask); total
  quantity conserved across a match; no negative sizes; FIFO preserved per level.
- **Fuzzing** on random order streams (no panics; invariants hold).
- **Golden-file replay:** a recorded stream reproduces identical trades/state —
  the determinism guarantee.
- **Race detector** and **benchmarks** in CI (GitHub Actions).

---

## 9. Research agenda (summary)

Detailed in [`research-roadmap.md`](research-roadmap.md). Each item ships as
**implementation → runnable experiment → honest write-up** (does it survive
out-of-sample data and trading costs?):

1. **Order-flow imbalance** (Cont, Kukanov & Stoikov, 2014) — reproduce the
   strong *contemporaneous* R²; then test whether it *predicts* the next
   interval. That distinction is the whole ballgame.
2. **Kyle's lambda** (1985) — estimate price impact per unit of flow inside the
   simulator, where ground truth is known.
3. **Avellaneda–Stoikov** (2008) — reservation price + optimal spread market
   making; backtest and measure inventory, adverse selection, PnL, Sharpe.
4. **Delta / CVD / absorption** — implement the retail order-flow primitives and
   stress-test the "trapped trader" narratives statistically.

---

## 10. Milestones (each = one or more small commits)

| # | Milestone |
|---|---|
| **M0** | **Spec** (this doc) + research roadmap + demo spec. |
| M1 | Core: `types` + `orderbook` + lean `matching` (market/limit, GTC/IOC/FOK, STP) + tests + `cmd/obdemo` + CI (build/vet/test-race). |
| M2 | `signals`: book imbalance + OFI, with tests. |
| M3 | `sim` + `marketdata` replay: synthetic order flow to trade against. |
| M4 | `strategy` Avellaneda–Stoikov + `backtest` harness + metrics. |
| M5 | Live L2 capture (crypto WS) + OFI contemporaneous-vs-predictive study. |
| M6 | Advanced order types: Stop/Stop-Limit, Iceberg/Hidden, Post-Only, Pegged, OCO/Bracket, Trailing. |
| M7 | `surveillance`: STP modes, spoofing/layering, stop-cascade, rate/ratio limits. |
| M8 | Auctions + circuit breakers (LULD); pro-rata matching mode. |
| M9 | `cmd/obwasm` WASM bindings + `web/` scaffolding + demo scenes 1–4 (mechanics). |
| M10 | Demo scenes 5–8 (signals, market making, surveillance) + GitHub Pages deploy. (CI landed early in M1.) |
| M11 | Perf pass (int-tick fast path, allocation audit); L3/MBO feed; benchmark dashboard. |

---

## 11. Provenance & license

The core design is informed by the author's prior production matching engine
(`alef/matching-engine`): decimal pricing, price–time priority, the map+ladder
book structure, and the matching algorithm. This repository is a clean,
research- and education-oriented re-implementation — not a copy of that exchange
stack.

License: **MIT**.
