<h1 align="center">📈 orderbook</h1>
<p align="center"><strong>A fast, embeddable limit-order-book &amp; matching engine in Go —<br/>plus a research harness and an animated, browser-native explainer.</strong></p>
<p align="center"><sub>The <b>same engine</b> you <code>go get</code> compiles to <b>WebAssembly</b> and runs live in the browser to teach how order books &amp; market making actually work.</sub></p>

<p align="center">
  <a href="https://intrepidkarthi.github.io/orderbook/"><img src="https://img.shields.io/badge/Live_Demo-GitHub_Pages-D4A547?style=for-the-badge&logo=webassembly&logoColor=white" alt="Live demo" /></a>
  <a href="docs/SPEC.md"><img src="https://img.shields.io/badge/Read_the-Spec-5B7C6B?style=for-the-badge&logo=readme&logoColor=white" alt="Spec" /></a>
  <a href="https://github.com/intrepidkarthi/orderbook/stargazers"><img src="https://img.shields.io/github/stars/intrepidkarthi/orderbook?style=for-the-badge&logo=github" alt="Stars" /></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go" />
  <img src="https://img.shields.io/badge/WebAssembly-ready-654FF0?style=flat-square&logo=webassembly&logoColor=white" alt="WebAssembly" />
  <img src="https://img.shields.io/badge/License-MIT-yellow?style=flat-square" alt="License MIT" />
  <img src="https://img.shields.io/badge/status-building_in_public-F59E0B?style=flat-square" alt="Status" />
  <a href="https://github.com/intrepidkarthi/orderbook/actions/workflows/ci.yml"><img src="https://github.com/intrepidkarthi/orderbook/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI" /></a>
  <a href="https://pkg.go.dev/github.com/intrepidkarthi/orderbook"><img src="https://pkg.go.dev/badge/github.com/intrepidkarthi/orderbook.svg" alt="Go Reference" /></a>
  <a href="https://goreportcard.com/report/github.com/intrepidkarthi/orderbook"><img src="https://goreportcard.com/badge/github.com/intrepidkarthi/orderbook?style=flat-square" alt="Go Report Card" /></a>
</p>

<p align="center">
  <a href="https://github.com/intrepidkarthi/orderbook/network/members"><img src="https://img.shields.io/github/forks/intrepidkarthi/orderbook?style=flat-square&logo=github&color=5B7C6B" alt="Forks" /></a>
  <a href="https://github.com/intrepidkarthi/orderbook/watchers"><img src="https://img.shields.io/github/watchers/intrepidkarthi/orderbook?style=flat-square&logo=github&color=D4A547" alt="Watchers" /></a>
  <a href="https://github.com/intrepidkarthi/orderbook/commits/main"><img src="https://img.shields.io/github/last-commit/intrepidkarthi/orderbook?style=flat-square&logo=git&color=22C55E" alt="Last commit" /></a>
  <a href="https://github.com/intrepidkarthi/orderbook/pulse"><img src="https://img.shields.io/github/commit-activity/m/intrepidkarthi/orderbook?style=flat-square&logo=git&label=Commits%2Fmo&color=F59E0B" alt="Commit activity" /></a>
  <a href="https://github.com/intrepidkarthi/orderbook"><img src="https://img.shields.io/github/repo-size/intrepidkarthi/orderbook?style=flat-square&logo=database&label=Repo%20Size&color=D97706" alt="Repo size" /></a>
  <a href="https://github.com/intrepidkarthi/orderbook/issues"><img src="https://img.shields.io/github/issues/intrepidkarthi/orderbook?style=flat-square&logo=github&color=F97316" alt="Open issues" /></a>
  <a href="https://github.com/intrepidkarthi/orderbook/graphs/contributors"><img src="https://img.shields.io/github/contributors/intrepidkarthi/orderbook?style=flat-square&logo=githubsponsors&logoColor=white&label=Contributors&color=EC4899" alt="Contributors" /></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go" />
  <img src="https://img.shields.io/badge/WebAssembly-654FF0?style=flat-square&logo=webassembly&logoColor=white" alt="WASM" />
  <img src="https://img.shields.io/badge/TypeScript-3178C6?style=flat-square&logo=typescript&logoColor=white" alt="TypeScript" />
  <img src="https://img.shields.io/badge/React-61DAFB?style=flat-square&logo=react&logoColor=black" alt="React" />
  <img src="https://img.shields.io/badge/Vite-646CFF?style=flat-square&logo=vite&logoColor=white" alt="Vite" />
  <img src="https://img.shields.io/badge/D3.js-F9A03C?style=flat-square&logo=d3dotjs&logoColor=white" alt="D3" />
  <img src="https://img.shields.io/badge/Tailwind_CSS-06B6D4?style=flat-square&logo=tailwindcss&logoColor=white" alt="Tailwind" />
</p>

> 🟢 **Live.** The engine, the research experiments, and an **animated
> [in-browser demo](https://intrepidkarthi.github.io/orderbook/)** (Go compiled to
> WASM) are all shipping. The whole roadmap is done: core engine (M1) →
> OFI/imbalance signals (M2) → deterministic simulator (M3) → Avellaneda–Stoikov +
> backtest (M4) → the OFI study (M5) → advanced order types (M6) → surveillance
> (M7) → auctions + circuit breakers + pro-rata (M8) → WASM demo + market-maker
> scene (M9/M10) → benchmarks + L3 (M11).
> See the [milestones](docs/SPEC.md#10-milestones-each--one-or-more-small-commits).

---

## What is this?

`orderbook` is one repository with **three reinforcing goals**:

| | | |
|:--|:--|:--|
| 🧱 **A library** | An efficient, embeddable **CLOB + matching engine** in Go. Decimal-exact, deterministic, price–time priority. `go get` and drop it into an exchange, a simulator, or a tool. | *suitable for many* |
| 🔬 **A research harness** | Build and **honestly backtest** microstructure ideas — order-flow imbalance, Kyle's λ, Avellaneda–Stoikov market making, delta/CVD — with reproducible experiments, not screenshots. | *contemporaneous ≠ predictive* |
| 🎬 **An animated explainer** | The **same engine compiled to WebAssembly**, running live in the browser, teaching how order books and market making work — one animated scene at a time. | *show, then explain* |

The three coexist through one rule: **strict downward layering** — the core
library never depends on the research, simulation, or presentation layers.

---

## Highlights

- 💰 **Money is never a float** — exact decimals throughout (fixing the classic bug in the frozen `legacy/` prototype).
- ⚡ **Fast** — targets ≥ 500k inserts/s and ≥ 200k matches/s per core, benchmarked in-repo.
- 🎯 **Deterministic** — same input stream → byte-identical trades & state; enables replay and honest backtests.
- 🧰 **Real-world order surface** — market, limit, stop/stop-limit, **iceberg/hidden**, **post-only**, **pegged**, **OCO/bracket**, trailing, auctions.
- 🛡️ **Market integrity built in** — self-trade prevention, **spoofing/layering** detection, **stop-cascade** protection, rate/velocity limits.
- 📊 **L1 / L2 / L3 (MBO)** market data — snapshots + incremental diffs with sequence numbers.
- 🤖 **Market making** — an Avellaneda–Stoikov quoter with production adaptations (rolling σ/k, inventory bounds, adverse-selection add-on).

See the full design in **[docs/SPEC.md](docs/SPEC.md)**.

---

## Performance

Core-library microbenchmarks (Apple M-series, Go 1.23, single-threaded) — all
meeting the [spec targets](docs/SPEC.md#7-performance-targets):

| Benchmark | ns/op | ~ops/sec |
|-----------|------:|---------:|
| top-of-book read (`BestBid`) | **70** | ~14 M |
| book insert (`Add`) | **401** | ~2.5 M |
| full-engine insert | **561** | ~1.8 M |
| match round-trip (maker+taker+trade) | **1548** | ~646 K |

Reproduce with `make bench`. CI runs them on every push and publishes the numbers
to the [Benchmarks workflow](https://github.com/intrepidkarthi/orderbook/actions/workflows/bench.yml)
run summary. Full methodology and notes: **[docs/BENCHMARKS.md](docs/BENCHMARKS.md)**.

---

## Architecture

```
web/ (React+TS)  ──▶  cmd/obwasm (Go→WASM)  ─┐
                                             │  strict downward layering
   backtest ▸ strategy ▸ sim ▸ signals ▸ marketdata
                                             │
            ══════════ CORE LIBRARY ═════════▼════════
              surveillance ▸ matching ▸ orderbook ▸ types
```

Research, simulation, and the web demo consume the core. The core depends on
nothing above it. Full diagram in the [spec](docs/SPEC.md#3-architecture).

---

## Quickstart

```sh
go get github.com/intrepidkarthi/orderbook/pkg/matching
```

```go
eng := matching.NewEngine(matching.DefaultConfig("BTC-USD"))

order, _ := types.NewOrder("alice", "BTC-USD", types.SideBuy,
    types.OrderTypeLimit, dec("30000"), dec("0.5"), types.TIFGoodTillCancel)

res := eng.Process(order)          // -> trades, status, remaining
bid, qty, ok := eng.BestBid()
```

### Runnable examples & tools

```sh
go run ./examples/basic         # place two orders, watch them match
go run ./examples/marketmaker   # backtest an Avellaneda–Stoikov maker
go run ./examples/signals       # compute book imbalance + OFI

go run ./cmd/obdemo             # end-to-end matching demo
go run ./cmd/obmm               # AS market-making backtest + scorecard
go run ./cmd/ofistudy           # the OFI contemporaneous-vs-predictive study
go run ./cmd/l2capture          # LIVE OFI on real Coinbase data
go run ./cmd/surveil            # spoofing / rate-limit surveillance
```

Advanced order types (stop, iceberg, post-only, OCO, pegged, trailing),
self-trade prevention, circuit breakers, and pro-rata matching all live in
`pkg/matching` — see [docs/SPEC.md §5](docs/SPEC.md#5-the-order-model-real-world-surface).

---

## Documentation

| Doc | What's inside |
|-----|---------------|
| 🎓 **[LEARN.md](docs/LEARN.md)** | Order books & market making from scratch — no finance background needed. |
| 📐 **[SPEC.md](docs/SPEC.md)** | Architecture, the real-world order model, core design decisions, performance targets, milestones. |
| 🔬 **[research-roadmap.md](docs/research-roadmap.md)** | OFI, Kyle's λ, Avellaneda–Stoikov, delta/CVD — each as implementation → experiment → honest write-up. |
| ⚡ **[BENCHMARKS.md](docs/BENCHMARKS.md)** | Core-library performance, methodology, and how to reproduce. |
| 🎬 **[DEMO-SPEC.md](docs/DEMO-SPEC.md)** | The animated, WASM-powered explainer: architecture, hosting, and the full scene storyboard. |

---

## Roadmap

`M0` spec ✅ · `M1` core engine ✅ · `M2` OFI signals ✅ · `M3` simulator + replay ✅ ·
`M4` Avellaneda–Stoikov + backtest ✅ · `M5` OFI study ✅ · `M6` advanced order
types ✅ · `M7` surveillance ✅ · `M8` auctions + circuit breakers + pro-rata ✅ ·
`M9` WASM demo + Pages ✅ · `M10` market-maker demo scene ✅ · `M11` benchmarks +
L3 ✅. The full roadmap is shipped (int-tick fast path noted as future). Details
in the [spec](docs/SPEC.md#10-milestones-each--one-or-more-small-commits).

---

## Provenance

The core design is informed by the author's prior production matching engine:
decimal pricing, price–time priority, the map+ladder book structure, and the
matching algorithm. This repo is a clean, research- and education-oriented
re-implementation — not a copy of that exchange stack.

## License

[MIT](LICENSE) © Karthikeyan NG

<sub>Topics: order-book · matching-engine · limit-order-book · clob · market-making · avellaneda-stoikov · order-flow-imbalance · market-microstructure · backtesting · algorithmic-trading · hft · quantitative-finance · webassembly · golang · exchange · price-time-priority</sub>
