# How real exchanges implement matching engines

Research notes grounding this library's roadmap in how production venues actually
work — centralized crypto (Binance, Coinbase), traditional/HFT (Nasdaq, LMAX,
CME, IEX), retail FX/CFD (MetaTrader), and on-chain/derivatives (dYdX,
Hyperliquid) — with the real incidents that taught the industry its hardest
lessons. Sourced from primary specs, regulator filings, engineering post-mortems,
and practitioner write-ups; vendor throughput claims are flagged as marketing.

---

## The one shape everyone converges on

Every serious venue runs the **same core**: a **single-threaded, deterministic
state machine per symbol**, with the book held entirely in RAM, fed by a
**sequenced command queue**. Parallelism lives *around* the core (I/O, risk,
market-data, sharding), never *inside* it. Per-symbol serialization is a
**correctness** requirement (strict price-time priority is inherently serial),
not just a performance choice.

- **LMAX** made this public: the Business Logic Processor does **6M+ ops/sec on
  one thread at sub-100ns**, using the Disruptor ring buffer and the
  single-writer principle. The rationale is mechanical sympathy — two cores
  writing one cache line ping-pong it between L1 caches (tens of ns each); one
  writer uses plain stores, so "the 99.99th percentile looks like the median."
- **Nasdaq INET, CME Globex, NYSE Pillar, LSE Millennium** are all variants:
  price-time (FIFO) matching with constant-time inline checks (size, price band,
  self-trade prevention), allocator-heavy work kept *off* the serial thread, and
  binary wire protocols (OUCH/ITCH, SBE) instead of text FIX.
- **Binance confirmed its own design** in the March 2023 post-mortem: hourly
  engine snapshots plus deterministic sequential replay, and multiple named
  engines (per-symbol/shard partitioning). Scale is **by sharding instruments**,
  never by locking one book across threads.

**This library's `single-writer Engine + MPSC Runner + int64 ticks + command-log
replay` is exactly this canonical shape.** The open-source reference is
[`exchange-core`](https://github.com/exchange-core/exchange-core) (Java,
Disruptor), which converges on the same pillars: integer price scaling, no
floating point, journaling + snapshots + full replay, and object pooling.

> **On throughput numbers:** treat all of them as marketing. Binance's "1.4M
> orders/sec," OKX's "~300k TPS," Hyperliquid's "200k orders/s" are aggregate,
> single-vendor, or un-benchmarked. No venue publishes reproducible *per-symbol*
> latency distributions. The credible practitioner target is **~1M orders/sec per
> core, p99 < 500µs, jitter < 50µs** — and even that is a rule of thumb. Any "X
> orders/sec" without p50/p99/p999 *and* allocation counts *and* book depth is
> noise. (This is why we publish tails and 0-allocs, not a headline TPS.)

---

## How each class of system really works

### Retail FX/CFD — MetaTrader (where "matching" usually happens nowhere)

MT5 is a licensed set of C++ server processes (Trade/History/Access servers) a
broker operates, extended via five approval-gated C++ APIs (Server/plugin,
Manager, Gateway, Report, Web). The data model is **Order → Deal(s) → Position**,
with **netting** (one position/symbol, exchange default) or **hedging** (a new
position per deal, FX default) accounting, and per-symbol execution modes
(Instant/Request/Market/**Exchange**) and fill policies (FOK/IOC/**BOC =
post-only**/Return).

The crux: in the **overwhelming common case there is no order book at all.**

- **B-book (dealing desk):** the broker is the counterparty; the dealer module
  fills the client at a reference price from an aggregated feed and warehouses the
  risk. "Execution" is a bookkeeping event, not a cross.
- **A-book (STP/DMA):** the order is passed over a **FIX bridge** (oneZero,
  PrimeXM XCore, Integral) to a liquidity provider; matching, if any, happens *at
  the LP*, mostly under **last-look** (the LP holds the request a few ms and can
  reject if the market moved against *it*).
- A true price-time book exists only in **Exchange mode** or when a broker builds
  an internal ECN via the **Gateway API** (which ships `SampleGateway`/
  `SampleExchange` and explicitly supports a "hybrid ECN engine").

**That internal-ECN slot is exactly where an engine like this belongs** — the
crossing venue a B-book broker lacks and Exchange-mode requires. Our honest
differentiator is the antithesis of last-look: *deterministic, replayable, real
price-time matching, verifiable by command-log replay.*

*Real cost of the alternative:* NYDFS fined **Barclays $150M (Nov 2015)** for a
last-look system that auto-rejected orders unprofitable to the bank; the NFA
fined **FXCM and Gain Capital** for asymmetric slippage. Asymmetry is the
liability — if an aggregation layer is ever added, reject/requote must be
symmetric and logged.

### Centralized crypto (Binance et al.)

Same single-threaded, in-memory, per-symbol core as everyone else; spot and
futures share the matching core but futures **bolts a risk engine around it**
(margin, mark price, funding, liquidation, ADL, insurance fund) — which is why
they fail independently. Self-trade prevention is universal, and the load-bearing
rule everywhere is **the taker's STP mode decides; the resting order's mode is
ignored.** Coinbase exposes dc (decrement, default)/co/cn/cb; Binance
EXPIRE_TAKER/MAKER/BOTH.

### Traditional / HFT (Nasdaq, LMAX, CME, IEX)

Deterministic serial core, binary protocols, and a deliberate design axis most
crypto ignores: **fairness**. CME offers per-instrument **FIFO or pro-rata**
(our FIFO/pro-rata switch mirrors this). A telling fork: **INET's OUCH acks on
receipt (before match)** while **CME iLink acks after the match** — a real
semantic choice with no industry consensus; a builder must pick one and document
it. **IEX** is the counter-design: a deterministic **350µs speed bump** so pegged
orders reprice against fresh data before latency arbitrage picks them off —
fairness as an explicit axis, not an accident of speed.

### On-chain / derivatives (dYdX v4, Hyperliquid)

The frontier of *deterministic replicated matching* — the same primitive as our
WAL replay, but load-bearing for consensus.

- **dYdX v4:** every validator runs an in-memory **memclob**; the block proposer
  matches against its local book and other validators re-derive and reject on
  divergence. Determinism = same start state + same ordered stream + deterministic
  logic. Order expiry is **Good-Til-Block height (a logical clock), never wall
  time.** Liquidation is a **privileged order** with a computed "Fillable Price"
  (spread from oracle, widening with bankruptcy) and up to a 1.5% penalty.
- **Hyperliquid:** the book *is* consensus state (HyperBFT); margin is re-checked
  against the oracle at match time.
- **Serum/OpenBook (Solana)** shows the cleanest core/settlement split: matching
  emits events onto a **request/event queue** that a permissionless "crank"
  consumes to move balances — matching decoupled from settlement.
- **Injective** uses a **frequent batch auction** (uniform-price clearing,
  randomized intra-batch order) to kill time-priority MEV — a batch alternative
  to continuous matching, and a natural sibling to our pro-rata mode.

The perp price triad the core never computes but everything depends on:
**index** (manipulation-resistant spot median), **mark** (index + clamped TWAP
basis; drives PnL/margin/liquidation), **last** (the CLOB output). Liquidations
trigger on **mark, never last** — precisely so a thin-book wick can't cascade.

---

## War stories, and what each one teaches

Ranked by how much money each actually cost someone.

| Incident | What happened | The lesson for a matching engine |
|---|---|---|
| **Knight Capital** — Aug 1 2012, **$440–460M in 45 min** | New code deployed to **7 of 8** servers **reused a flag bit** that reactivated 2003 dead code ("Power Peg") generating unbounded child orders; alert emails ignored; **no kill switch**; the fix *spread* the bug to all 8. Firm died. | **Guard the engine's *own* output**, not just inputs: per-session caps on child-order/notional/fill-rate that auto-halt. Never reuse flag bits; delete dead code. Verify fleet-wide deploy consistency. |
| **Flash Crash** — May 6 2010 | A $4.1B E-mini sell algo at 9% of volume with no price limit triggered a feedback loop; ~1000 DJIA points in minutes. | The regulatory legacy: **LULD price bands, market-wide circuit breakers (7/13/20%), SEC Rule 15c3-5** pre-trade capital/erroneous-order checks — codified "kill switches and capacity limits." |
| **Nasdaq Facebook IPO** — May 18 2012, $10M fine | A **race condition** in the opening-cross code: continuous modifications during cross calc re-triggered validation so it never converged; leadership started trading anyway; 30k orders hung 2+ hrs. | Auction/cross computations need a **bounded, convergent** algorithm and a fail-safe that **halts on ambiguity** rather than proceeding. |
| **TSE arrowhead** — Oct 1 2020, dark all day | Memory fault; **automatic failover didn't engage** (config still needed a manual reboot). | Failover must be **automatic, tested, and not itself a SPOF**. Kill-and-replay in staging. |
| **ASX** — Nov 16 2020 | A **multi-leg combination-order** bug produced bad market data on day one of a new platform; halted after 20 min. | **New/complex order types are the highest-risk surface.** Ship each behind a flag; validate their output; disable per-type without downing the venue. |
| **Binance spot** — Mar 24 2023, ~2 hr halt | A **trailing-stop** bug corrupted sequential engine state; because matching is sequential they had to halt, restore the hourly snapshot, and replay. | Exotic types (stop/iceberg/pegged/OCO/**trailing**) are the top real source of engine corruption — **fuzz them hardest against replay.** |
| **Binance futures** — May 19 2021 | During the BTC crash the engine degraded to ~20% capacity; users couldn't close positions or add margin and were liquidated anyway. | **Overload during volatility harms users exactly when they must de-risk.** Bound the ingress queue; keep cancel/reduce paths alive under shedding. |
| **Coinbase** — May 7 2026, ~8 hr | Matching ran on a **5-node Raft cluster in a single AWS AZ** for latency; a cooling failure killed 3/5 → lost quorum. Recovery went cancel-only → auction → full. | Colocating consensus in one AZ trades resilience for latency; **degraded modes (cancel-only, auction) must be first-class.** |
| **Hyperliquid JELLY** — Mar 26 2025, ~$13.5M | An attacker rammed a thin spot price +400%; the **manipulable oracle fed mark price**; the vault inherited a toxic short; validators voted to override the oracle. | **A mark/oracle price on shallow liquidity is the most dangerous input in the stack** — clamp it, cap how far a thin book moves it, drive circuit breakers off a clamped mark/index. |

---

## What this library already gets right

Mapped to the patterns above, we are already on the canonical path:

- **Single-writer, in-memory, per-symbol core** (Engine) with an MPSC command
  queue (Runner) — the LMAX/INET/exchange-core shape.
- **Integer int64 ticks/lots** with decimals only at the `Instrument` edge — the
  universal, non-negotiable "no float in the core" rule (the `100.20 + 0.30 ≠
  100.50` crossing bug is the canonical failure we avoid by construction).
- **O(1) cancel** via intrusive doubly-linked FIFO per level — the "single biggest
  performance win" the HFT practitioner measured (p999 13× lower).
- **Deterministic command-log replay + WAL recovery** — the same primitive dYdX
  bootstraps validators with and Binance recovered with in 2023.
- **Zero-allocation match path** (`Match` into a caller buffer) — the callback/
  visitor pattern that kills the "2M heap allocations from returning a trade
  slice" bug.
- **FIFO *and* pro-rata** allocation — mirrors CME's per-instrument choice.
- **Price-band circuit breaker, self-trade prevention, L1/L2/L3 snapshots** — the
  in-core O(1) guards real venues run inline.
- **Honest benchmarking** — tails (p50/p99/p999) and allocation counts, not a
  vendor TPS headline.

## Where we diverge — the gaps this research surfaces

These become the roadmap (see the accompanying plan). In priority order of
real-world risk: (1) the match path still reads `time.Now()` for timestamps — a
determinism leak that must be injected as a logical clock; (2) no self-output
kill switch (the Knight gap); (3) no replay-equivalence / zero-alloc **CI gate**
to weaponize determinism as a deploy check; (4) no typed, sequence-numbered
**event stream** as the integration seam (the linchpin for every recovery/
market-data/drop-copy adapter); (5) exotic order types aren't individually
feature-flagged; (6) no first-class **degraded modes** (cancel-only/halt) or
bounded backpressure; (7) snapshots aren't yet a resume-from-sequence recovery
primitive; (8) no mark/index-driven band, liquidation-injection hook, or
per-symbol shard manager.

---

## Sources

Primary specs & regulators: SEC Flash Crash report, SEC Knight Capital & Nasdaq
FB-IPO orders, 17 CFR 240.15c3-5, LULD plan, JPX arrowhead disclosure, NYDFS
Barclays order; Nasdaq OUCH/ITCH/SoupBinTCP/MoldUDP64, CME MDP 3.0/SBE, NYSE
Pillar & LSE drop-copy FIX specs; MetaQuotes MT5 docs; Coinbase/Binance matching
& STP docs; dYdX & Hyperliquid protocol docs. Practitioner: LMAX Disruptor paper,
Martin Fowler's LMAX architecture, the single-writer principle, Jane Street "How
to Build an Exchange," Proof Trading FIX-gateway engineering, exchange-core, and
"How I Built an HFT Matching Engine (And All The Things I Got Wrong)." Incident
analysis: Coinbase May-2026 post-mortem, Binance Mar-2023 post-mortem, Hyperliquid
JELLY coverage. Full URL list is preserved in the research working notes.
