# Order-Book Attacks & Defenses — A Threat Model

**Target:** `github.com/intrepidkarthi/orderbook` — a pure Go CLOB matching core:
deterministic single-writer, int64 ticks/lots (no float), monotonic sequence +
WAL + snapshot recovery, bounded backpressure. Identity, credit, position
limits, fees, and wire protocols are integration layers **above** the core (see
`docs/SPEC.md` §3 and `docs/INTEGRATION.md`).

**Audience:** integrators and the library maintainer.
**Method:** multi-agent research across five attack surfaces (market
manipulation, technical/security, predatory HFT, on-chain/MEV, and the
surveillance/regulatory playbook), an adversarial verification pass, and a
synthesis verified against the actual source (`pkg/matching`,
`pkg/surveillance`, `pkg/auction`). Every claim is grounded in a named
enforcement action, incident, or CVE.

> **How to read the verdicts.** Each attack is marked **defended ✓**,
> **partial**, or **gap**, and split into **core** (the matcher's job) vs
> **layer** (identity / credit / oracle / gateway / surveillance — deliberately
> out of the core). The boundary is the whole point: a matching core earns trust
> by being small, deterministic, and neutral. Defenses that need identity,
> credit, price-sourcing, timing policy, or off-venue context are delegated so
> the core stays bit-reproducible and audit-replayable.

---

## 1. Executive summary

The genuine, verifiable strength of this core is **structural** — it is
strongest exactly where the most-litigated matching-engine catastrophes live:

- **Runaway self-output** (Knight Capital, $440M in ~45 min, 2012) — the
  `Guardrail` self-output tripwire trips the engine to `Halted` on per-window
  trade/notional caps. Most engines guard only *inputs*; guarding the engine's
  own *output* is the Knight lesson, and it is present.
- **Race / TOCTOU** — the single-writer deterministic loop serializes every book
  mutation, so intra-book check-then-use races are structurally impossible.
- **Precision / overflow** — int64 ticks/lots eliminate float rounding entirely
  (no Bitcoin-CVE-2010-5139-class value leak from rounding).
- **Self-trade** (Coinbase $6.5M, 2021) — a rich STP suite (taker-decides,
  `CANCEL_NEWEST/OLDEST/BOTH`, `DECREMENT`, cross-account `TradeGroupID`,
  `Privileged` exemption) is in-core.
- **Complex-order-type wedge** (Binance trailing-stop halt 2023; ASX TMC outage
  2020) — `DisabledClasses` feature-flags let you kill one buggy exotic without
  downing the venue; plain limit/market cannot be disabled, so core liquidity
  survives.

The research surfaced a short, convergent gap list — since addressed. Each of the
three highest-leverage findings has shipped (see §5 for the full status):

1. **Minimum resting time** — the single highest-leverage missing primitive, on
   which four independent analyses converged — is implemented (`MinRestingTime`).
   It blunts spoofing, quote-fading, and flicker-quote games at the source.
2. **`pkg/auction`, the highest-leverage *unused* asset**, is now wired as a
   selectable open/close/clearing session (`AuctionSession` + `RandomizedClose`),
   structurally blunting marking-the-close, latency arbitrage, MEV/JIT, and
   momentum ignition at once.
3. **Surveillance now has an enforcing counterpart:** an OTR/cancel-ratio metric
   (`OTRDetector`) plus a rejecting token-bucket admission gate at the gateway
   layer (`examples/gateway`), so the venue can throttle, not merely alert.

**The roadmap is now fully implemented** (§5): the depth-backed mark bound
(`MinMarkDepth`), randomized iceberg peaks (`IcebergPeakJitter`), and the
cross-book correlator (`CrossBookMonitor`) that were the last open items have all
shipped, alongside a reusable `pkg/gateway` (enforcing rate gate + taker speed
bump). Every MUST/SHOULD/COULD row carries a ✅.

**What remains genuinely out of scope** is not a gap but the boundary of §6: a
matching core cannot know beneficial ownership, credit, off-venue conduct, oracle
sourcing, or ingress ordering — those are identity / clearing / oracle / gateway
concerns by design. On the **oracle / mark** posture specifically: `MaxMarkStep`
now bounds a single-jump pump *and* `MinMarkDepth` bounds a patient in-step drag
(a mark can only move to prices real resting depth supports), but the ultimate
trust in the mark's *source* (a manipulation-resistant index / TWAP) still lives
in the oracle layer above. The core refuses to act on an unbacked mark; it cannot
manufacture a good one.

### Attacker taxonomy

| Attacker | Goal | Primary surface | Where the defense lives |
|---|---|---|---|
| **Market manipulators** | Move / mark price, fake volume | Order placement & cancel patterns, the closing print | Core primitives (band, STP, auction) + surveillance detection |
| **Predatory HFT** | Pick off stale quotes, sniff hidden size, jump queues | Timing, ingress ordering, iceberg refill patterns | Core (min-rest, tick grid, randomized peaks) + gateway (speed bump) |
| **Technical attackers** | Crash / wedge / drain via protocol, arithmetic, resource, concurrency bugs | Order-type edge cases, integer bounds, queue depth, replay | Core hardening (bounds, dedup, caps) — determinism already immunizes races |
| **On-chain / MEV** | Reorder around victims, poison oracle marks, cascade liquidations | Ingress sequencing, mark-price feed, forced liquidation | Core supplies primitives (batch, mark bounds, `ForceTrade`); ordering / oracle = layer |

The core defends **market-microstructure mechanics and its own arithmetic and
state**. It cannot know identity, credit, beneficial ownership, off-venue
conduct, or who ordered the ingress stream — those are correctly layers above.

---

## 2. Master attack catalogue

| Attack | Category | How it works | Real example (name / date / $) | Detection signal | Defense | Core-or-layer |
|---|---|---|---|---|---|---|
| **Spoofing** | Manipulation | Large non-bona-fide order fakes pressure; small genuine fill opposite; cancel before the bait fills | JPMorgan $920.2M CFTC (2020); Coscia conviction (2015) | High OTR, short lifetime, displayed ≫ filled, cancel-on-reversal | `SpoofDetector` (have) + **min resting time** | Detect = surveillance ✓; **min-rest = core (add)** |
| **Layering** | Manipulation | Stacked spoof orders across multiple price levels | Sarao — Flash Crash (2010), ~$879K/day; Trillium $2.26M (2010) | Multi-level one-sided depth that vanishes on cancel | Aggregate per-side layering detector + OTR cap | Surveillance + gateway |
| **Wash / self-trade** | Manipulation | Trade against self / linked account; no change of ownership | Coinbase $6.5M CFTC (2021); CLS Global $428K (2024-25) | Maker owner = taker owner / group; matched equal size, same price | **STP + `TradeGroupID` + `Privileged`** (have) | **Core ✓**; ownership feed = layer |
| **Momentum ignition** | Manipulation | Aggressive burst baits chasers, then exit into the move | FINRA/SEC guidance (usually charged with spoofing) | Short-horizon impact then mean reversion | Impact/reversal detector; `PriceBand` caps excursion | Detector = surveillance (add); band = core ✓ |
| **Marking / banging the close** | Manipulation | Concentrate aggression into the closing / opening print to set a reference | Athena "Gravy" $1M SEC (2014), >70% of last-second volume; Optiver $14M | Last-N-second volume share; position reversal straddling the print | **Randomized-duration closing auction** (`pkg/auction`) + window detector | Auction = core (wire it); detector = surveillance |
| **Quote stuffing / flood DoS** | Technical + Manipulation | Flood orders/cancels to slow rivals and the tape | Citadel $800K Nasdaq (2014); Trillium $1M FINRA (2010) | Message-rate spike, extreme OTR | `RateLimiter` + **bounded backpressure** (have); **enforcing gate** | Backpressure = core ✓; **enforcing gate = gateway (add)** |
| **Resource exhaustion (dust)** | Technical | Millions of tiny resting orders bloat maps/levels, degrade latency | Binance order-pipeline congestion, err `-1008` (Oct 2025) | Open-order count / memory per account climbs | **Max-open-orders/account + min-notional/lot** | **Core (add)** |
| **Race / TOCTOU** | Technical | Concurrent check-then-use double-spend / double-cancel | CVE-2026-34368 (AVideo); parallel-withdrawal drains | Duplicate state transitions under concurrency | **Single-writer serialization** | **Core ✓ (immune)**; balances = layer |
| **Integer overflow / precision** | Technical | `price×qty` or windowed sums wrap int64; float rounding leaks value | Bitcoin CVE-2010-5139 — 184.467B BTC minted (2010) | Negative / absurd notional; sum discontinuity | int64 (no float) ✓ + **checked/saturating notional + ingress bounds** | Core: no-float ✓; **checked arithmetic = add** |
| **Complex order-type wedge** | Technical | Crafted exotic hits an unhandled path, wedges / crashes the venue | Binance trailing-stop halt (2023); ASX TMC outage (2020) | Exotic-type error rate; guardrail trip | **Feature-flag disable** (`DisabledClasses`); fuzz; guardrail | **Core ✓ (showcase)** |
| **Sequence / replay / injection** | Technical | Replay / inject a `NewOrder` → double-book | FIX PossDup / seq-34 reuse | Duplicate `ClientOrderID`; seq gaps | **`ClientOrderID` idempotency dedup** in-core; session seq = layer | Core (add) + session = layer |
| **Order-ID enumeration (IDOR)** | Technical | Walk sequential IDs to read others' orders | IDOR order-history bounties | Sequential-ID scans across accounts | Object-level authz; opaque public IDs | **Layer** (core int64 IDs are internal, fine) |
| **Auth / API-key theft** | Technical | Stolen keys place malicious counter-trades | 3Commas ~$20M, 100K keys (2022) | Orders from anomalous IP / session | Key scoping / MFA; **trust-gate the `Privileged` flag** | **Layer**; core must authenticate `Privileged` |
| **Latency arb / stale-quote picking** | Predatory HFT | Fast taker hits a maker quote gone stale vs a faster venue | *Flash Boys* / IEX 350µs speed bump (2016) | Aggressor hits just-stale price; maker cancel µs late | Asymmetric ingress **speed bump** on takers; or route to **batch auction** | Speed bump = gateway; auction = core |
| **Quote fading** | Predatory HFT | Makers yank displayed size on sensing toxic flow | TSX Alpha bump / liquidity-fade complaints | High cancel rate, ~0 fill on resting quotes | **Minimum quote life** + OTR fees | Min-life = core (add); fees = gateway |
| **Order anticipation / front-running** | Predatory HFT | Sniff a parent order, trade ahead of its children | Broadly documented (ASIC/SEC studies) | Trades consistently just ahead of large child orders | Iceberg + **randomized peaks** + non-display | Mechanics = core (add); detect = surveillance |
| **Iceberg detection / pinging** | Predatory HFT | Tiny IOC pings map hidden liquidity, then trade the whale | CME iceberg-prediction research (2019) | Bursts of tiny immediately-cancelled / IOC orders | **Randomized peaks + min order size** + ping detector | Core (add) + surveillance |
| **Stop-hunting** | Predatory HFT | Push price to trigger a stop cluster, then reverse | Recurrent FX/futures allegations | Price spike into a known stop zone then reversal | Hidden stops (don't leak pending-stop count); band / guardrail | Core (bands) ✓ + surveillance + market-data hygiene |
| **Queue jumping / sub-penny** | Predatory HFT | Step ahead of a resting queue by a trivial fraction | Reg NMS Rule 612 sub-penny ban | Quotes at sub-tick increments | **int64 tick grid — impossible by construction** | **Core ✓ (structural)** |
| **Oracle / mark pump → borrow** | On-chain / MEV | Pump a thin mark, borrow against the inflated value | Mango Markets $110M (2022, criminal conviction) | Mark moves without book depth; self-cross prints | **Min-liquidity mark bounds** + price band + STP | Band/STP = core ✓; **mark bounds = add**; oracle sourcing = layer |
| **Mark pump → forced short loss** | On-chain / MEV | Squeeze an illiquid perp onto a vault | Hyperliquid JELLY ~$13.5M (2025) | Mark vs depth divergence; cascade | Price band, `Halt`, `ForceTrade`, backpressure | **Core ✓**; ADL ranking = layer |
| **Liquidation cascade** | On-chain / MEV | Trigger mass liquidations, air-pocket the book | Hyperliquid HLP (2025); BitMEX Oct-11 flash crash | Guardrail trip; band breach | **Chunked `ForceTrade`** + insurance fund + ADL + `Halt` | ForceTrade/guardrail = core ✓; fund/ADL = layer |
| **Flash-loan governance** | On-chain / MEV | Borrow a quorum, pass a malicious proposal | Beanstalk $182M (2022) | Sudden supermajority; no timelock | Governance timelock; flash-resistant snapshot | **Layer** (core has no surface) |
| **Proposer / sequencer MEV** | On-chain / MEV | Block builder inserts ahead of a large taker | dYdX v4 (design risk) | Shadow-match divergence | Deterministic single-writer core | **Core ✓** intra-engine; ingress ordering = layer |
| **Cornering** | Manipulation | Control deliverable supply, force shorts | Hunt silver (1979-80), $134M liability | Position vs OI / float; basis dislocation | Position limits + `CancelOnly`/`Halted` + mark band | Limits = layer; **states / band = core ✓** |
| **Short squeeze** | Manipulation | Shorts forced to cover at your price | LME nickel — ~$3.9B trades busted (2022); GME (2021) | Short interest vs float; spot-future gap | **Halt states + trade-bust via WAL replay** | Policy = layer; **halt / replay = core ✓** |
| **Pump-and-dump / bear raid** | Manipulation | Off-venue promote, dump into the volume | SEC influencers ~$100M (2022) | Cross-market + social signals | Price band / halt limit intraday damage | **Layer / regulator** (off-venue conduct) |
| **Cross-product / cross-venue** | Manipulation | Spoof one product / venue, profit in a correlated one | Oystacher / 3Red $2.5M; JPMorgan (futures/cash) | Cross-book OTR / imbalance correlation | Fan multiple books into one `surveillance.Monitor` (SMARTS-style) | **Layer** (each `Engine` is one book) |

---

## 3. Deep dives — the top ten

### 3.1 Runaway / repurposed-path self-output — core ✓ (present)
A dormant or repurposed code path reactivates and sprays orders faster than any
human can react; the damage is done by the engine's *own* output, not a
malicious input. **Knight Capital lost $440M in ~45 minutes (Aug 1 2012)** when a
repurposed "Power Peg" flag reactivated dormant order-spraying logic; the SEC
fined them $12M under Rule 15c3-5. Most designs validate inputs and never watch
what they emit. **Our posture:** the `Guardrail` counts trades / notional per
window and trips the engine to `Halted` when either cap is exceeded; combined
with the deterministic single-writer, output is bit-reproducible, so the
guardrail catches the runaway regardless of *which* path emitted it. **To add:**
nothing structural — optionally surface the guardrail trip as an `EventSink`
event so operators page immediately.

### 3.2 Spoofing / layering — partial (detect ✓, prevent = gap)
Post large non-bona-fide orders on one side to fake pressure, fill a small
genuine order opposite, cancel the bait before it fills; layering stacks bait
across levels. The single most-litigated microstructure abuse: **JPMorgan
$920.2M CFTC (2020)**, **Coscia criminal conviction (2015)**, **Sarao's dynamic
layering** contributing to the **Flash Crash (May 6 2010)**. **Our posture:**
`SpoofDetector` flags large orders cancelled *unfilled* within a short lifetime —
the exact JPMorgan signature — but it keys off engine-sequence distance (not
wall-clock resting time), scores single orders (not aggregated per-side
layering), and has no order-to-trade / cancel-ratio metric (the strongest
spoofing signal). **To add — in core:** a **minimum resting time** (reject a
cancel arriving too soon after placement; only the single writer holds
deterministic placement timestamps, so this belongs in the engine).
**In surveillance:** an OTR / cancel-ratio detector with an *enforcing* mode and
per-level layering aggregation.

### 3.3 Wash / self-trade — core ✓ (present)
Buy and sell against yourself or a linked account — fake volume, painted prints,
harvested maker rebates, fee-tier climbing. **Coinbase $6.5M CFTC (2021)** (two
in-house bots matched each other); **CLS Global $428K (2024-25)**. **Our
posture:** fully defended in-core for the common case — STP is built into the
matcher (`Privileged` takers exempt, matching on a shared non-zero
`TradeGroupID`, per-order `STPMode` resolving `CANCEL_NEWEST/OLDEST/BOTH`,
`DECREMENT`, `ALLOW`) — a faithful, arguably richer CME-SMP (tag 2362)
implementation. **Boundary:** the core enforces the group IDs it is *told*;
mapping two unlinked accounts to one beneficial owner is an identity-layer job.

### 3.4 Oracle / mark manipulation → liquidation — partial → gap (the honest weak spot)
Pump a thin mark, then borrow against or force liquidation at the poisoned
price. **Mango Markets $110M (Oct 11 2022)** — the attacker self-crossed MNGO
perps while pumping thin spot ~2,300%, then borrowed $110M against the inflated
mark (criminal conviction). **Hyperliquid JELLY ~$13.5M (Mar 26 2025)** — an
illiquid perp was pushed ~0.0095 → 0.05 to squeeze a short onto the HLP vault.
**Our posture:** the price band keyed off a risk-clamped mark (`SetMarkPrice`,
`outsideBand`) is the *right primitive* — but it is only as good as the mark fed
in, and **nothing today rejects an unbacked mark update.** The STP suite already
blocks the self-cross wash-print *leg* within one venue, but the poisoned-mark
vector itself is open. **To add — in core:** **minimum-liquidity mark bounds** —
reject or clamp a `SetMarkPrice` update not backed by enough resting depth or
moving more than a per-window cap, plus a mark-step / rate guard. The mark's
*sourcing* (TWAP / EMA, multi-venue median, staleness) stays in the oracle layer
— but the core should refuse to *act* on an unclamped mark.

### 3.5 Complex order-type edge-case wedge — core ✓ (showcase)
A crafted exotic (trailing stop, combination, iceberg, pegged) hits an unhandled
path and wedges or crashes the engine, downing the whole venue. **Binance halted
spot trading ~2h (Mar 24 2023)** over a trailing-stop bug; **ASX closed 20 min
into its first day on ASX Trade (Nov 16 2020)** over a Tailor-Made Combination
bug, reopening with TMCs disabled. **Our posture:** `DisabledClasses` lets you
disable a buggy exotic without downing the venue — *exactly* the ASX remedy —
`fuzz_test.go` hardens edge paths, and the guardrail trips to `Halted` if a
malformed type runs away. Plain limit / market cannot be disabled, preserving
core liquidity. **To add:** nothing structural; maintain fuzz coverage as new
order classes land.

### 3.6 Quote stuffing / order-flood DoS — partial (backpressure ✓, enforcing gate = gap)
Mass place + cancel with near-zero fills — "a DDoS on the order book" — to
saturate the engine and slow rivals. **Citadel $800K (Nasdaq, 2014)**; **Trillium
$1M (FINRA, 2010)**. **Our posture:** the core is structurally resilient —
bounded backpressure (`TrySubmit` sheds new orders with `ErrQueueFull` while
`Cancel` always blocks through) means a stuffer cannot wedge the book; under
overload it drops *new* liquidity while cancels keep flowing, and `RateLimiter`
flags bursts. But surveillance only *alerts*. **To add — gateway:** an enforcing
admission gate (per-account msg/sec quota + OTR that *rejects*). **In core:** a
per-account in-flight / open-order cap so one session cannot exhaust queue depth.

### 3.7 Integer overflow / precision leak — partial (no-float ✓, checked-arith = gap)
Summing outputs or computing `price×qty` overflows a fixed-width int, or float
rounding leaks value. **Bitcoin value-overflow, CVE-2010-5139 (Aug 15 2010)** —
block 74638 minted **184.467 billion BTC** because output sums wrapped signed
int64. **Our posture:** int64 ticks / lots eliminate float rounding entirely — a
real, verifiable strength — but `price×qty` notional and the guardrail's windowed
sums can still overflow int64. **To add — in core:** checked / saturating
arithmetic on notional and windowed sums, plus ingress bounds validation on
price / qty / notional (reject absurd magnitudes before they enter the book).
Cheap, deterministic, cold-path.

### 3.8 Marking / banging the close — partial (auction exists, unwired)
Concentrate aggression into the closing / opening print to set a reference price
(NAV, options, position mark). **Athena "Gravy" $1M (SEC 2014)** — >70% of Nasdaq
volume in affected names in the last two seconds; **Optiver $14M**. **Our
posture:** not defended today (the core is time-of-session-neutral), but
`pkg/auction` already implements the uniform-price uncross that is the structural
counter. **To add — in core:** wire `pkg/auction` as a selectable open / close /
clearing mode with a **randomized duration** — you cannot aim for a print whose
timing you don't control. This is the highest-leverage unused asset; the same
wiring also blunts latency arbitrage, MEV/JIT, and momentum ignition.
**Surveillance:** a close-window volume-share detector.

### 3.9 Race / TOCTOU — core ✓ (immune)
Concurrent requests read stale state between check and use — double-spend /
double-cancel. **CVE-2026-34368** (a read-then-write with no row lock); exchange
parallel-withdrawal drains. **Our posture:** structurally immune inside the book
— the single-writer deterministic loop serializes every mutation, so there is no
interleaving, no data race, and intra-book TOCTOU cannot occur. This is a
property, not a mitigation. **Boundary:** TOCTOU on *balances / withdrawals* is a
credit-layer concern the core never touches — correctly out of scope.

### 3.10 Latency arbitrage / stale-quote picking — layer (+ core auction)
The canonical *Flash Boys* exploit: a fast taker sees the reference price move on
one venue and picks off a maker's now-stale quote before the maker can cancel.
**IEX's 350µs speed bump** (the SEC ruled sub-1ms delays de minimis, 2016) is the
named counter. **Our posture:** none in the matching core — it is pure continuous
price-time matching with no ingress delay, and the price band only stops
*grossly* off-market prints, not one-tick-stale picks. Correctly a *layer*
problem: the deterministic core must not carry timing policy. **To add —
gateway:** an asymmetric speed bump on liquidity-*taking* orders so makers can
reprice. **Alternatively, route to `pkg/auction`** — Budish-Cramton-Shim (QJE
2015) show frequent batch auctions turn the speed race into a price race,
defeating this at the source.

---

## 4. What this library already defends — the verified inventory

| Primitive (symbol) | Defends | Real attack it counters |
|---|---|---|
| **STP suite** — `isSelfMatch` / `takerSTP` / `decrement`: taker-decides, `CANCEL_NEWEST/OLDEST/BOTH`, `DECREMENT`, cross-account `TradeGroupID`, `Privileged` exemption | Wash / self-trade; the self-cross leg of oracle pumps | Coinbase $6.5M (2021); Mango leg (2022); CLS Global $428K |
| **PriceBand + risk-clamped mark** — `outsideBand`, `SetMarkPrice` | Off-market prints, ramping excursion, LULD-style collar | Banging the close (bounds achievable mark); oracle manipulation (band present) |
| **Guardrail self-output tripwire** — `Guardrail` → `Halted` on per-window trade / notional caps | Runaway / repurposed-path output | **Knight Capital $440M (2012)** |
| **Degraded states** — `EngineState` Open / CancelOnly / Halted, `SetCancelOnly` / `Halt` / `Resume` | Kill switch (Rule 15c3-5), venue-halt / liquidation-only | LME nickel halt (2022); Coinbase wind-down path; Hyperliquid delist |
| **DisabledClasses feature flags** — cold-path `rejectDisabled` | Buggy exotic order type without downing the venue | **ASX TMC (2020); Binance trailing-stop (2023)** |
| **Deterministic single-writer + int64 (no float)** | Races / TOCTOU (immune); precision / rounding leak (impossible); sub-penny queue-jump (impossible) | CVE-2026-34368; Bitcoin CVE-2010-5139 (rounding class); Reg NMS 612 |
| **Monotonic sequence + WAL + snapshot** (`pkg/wal`, `TakeSnapshot`) | Replayable audit spine; clean trade-bust / replay | CAT (Rule 613) event spine; LME / GME trade-bust |
| **Bounded backpressure** — `TrySubmit` sheds new, `Cancel` blocks through | Quote-stuffing / flood DoS (structural resilience) | Citadel $800K (2014); Trillium $1M (2010) |
| **SpoofDetector + RateLimiter** (`pkg/surveillance`) | Spoofing / layering detection; message-burst flagging | JPMorgan $920M signature; Trillium layering |
| **ForceTrade + Privileged exemption** | Liquidation / ADL print injection at a bankruptcy price | Hyperliquid force-settle (2025); BitMEX Oct-11 lineage |
| **pkg/auction (uniform-price uncross + `AuctionSession`)** — open/close/recovery call auction with a deterministic `RandomizedClose` | Marking-the-close, latency arb, MEV/JIT, momentum ignition | Athena (2014); Flash Boys; Injective / CoW model |
| **ProRata allocation** (`Config.ProRata`) | Alternative to strict time-priority allocation | — |
| **Iceberg / static peg** | Hides parent-order size (partial) | Order anticipation (mechanics present) |
| **Pre-trade risk caps** — `MinRestingTime`, `MaxOrderQty`/`MaxOrderNotional`, `MinOrderQty`/`MinOrderNotional`, `MaxOrdersPerAccount`, `DedupClientOrderIDs`, `MaxMarkStep` + `MinMarkDepth`, `MaxForceTradeQty`, `BandBreachPause`, `IcebergPeakJitter` + notional-overflow guard | Spoofing, dust/stuffing, replay double-book, oracle pump (jump + patient drag), liquidation cascade, iceberg sniffing, integer overflow | JPMorgan $920M; Binance `-1008`; FIX PossDup; Mango/JELLY; Bitcoin CVE-2010-5139 |
| **Surveillance detectors** (`pkg/surveillance`) — `SpoofDetector`, `RateLimiter`, `OTRDetector`, `CloseMarkingDetector`, `RampingDetector`, `PingingDetector`, `CrossBookMonitor` | Spoofing/layering, stuffing, OTR abuse, marking-the-close, ramping, pinging, cross-book manipulation | JPMorgan; Trillium; Athena; Oystacher $2.5M; MiFID II RTS 9 |
| **Gateway layer** (`pkg/gateway` + `examples/gateway`) — enforcing token-bucket `RateGate`, taker speed bump, CAT-style audit export | Flood DoS, latency arb, audit trail | Citadel/Trillium; IEX; Rule 613 |

---

## 5. Prioritized defense roadmap

Effort: **S** ≤ a day · **M** a few days · **L** a week+. Ranked by
real-world enforcement frequency × impact × matching-engine relevance.
**Status** tracks what has shipped: ✅ done · ◻ open.

### MUST — enforcement-backed, high-leverage

| # | Build | Why (a real attack it stops) | Effort | Layer | Status |
|---|---|---|---|---|---|
| 1 | **Minimum resting time** — reject a cancel arriving too soon after placement, using single-writer placement timestamps | Spoofing / quote-fading / JIT-flicker. JPMorgan $920M; Coscia. Four analyses converge here. | M | **Core** | ✅ `Config.MinRestingTime` |
| 2 | **Enforcing admission gate** — per-account msg/sec + OTR / cancel-ratio that *rejects* (promote `RateLimiter` from alert-only) | Quote stuffing / flood DoS. Citadel $800K; Trillium $1M. | M | **Gateway** | ✅ `gateway.RateGate` (+ `examples/gateway`) |
| 3 | **OTR / cancel-ratio metric** — count cancels-per-fill, not just placements (the strongest spoofing signal) | Spoofing / layering. Unanimous gap across analyses. | S | **Surveillance** | ✅ `surveillance.OTRDetector` |
| 4 | **Minimum-liquidity mark bounds + mark-step guard** — reject `SetMarkPrice` unbacked by depth or moving more than a per-window cap | Oracle / mark manipulation. **Mango $110M; Hyperliquid JELLY $13.5M.** The honest crypto gap. | M | **Core** | ✅ mark-step (`Config.MaxMarkStep`) + depth-backed bound (`Config.MinMarkDepth`) |
| 5 | **Checked / saturating notional arithmetic + ingress magnitude bounds** on price / qty / notional | Integer overflow. Bitcoin CVE-2010-5139. int64 alone isn't enough — `price×qty` still wraps. | S | **Core** | ✅ overflow-reject + saturating guardrail |
| 6 | **Per-order max size / notional (fat-finger) reject** in the cold path | Runaway / fat-finger. Complements `Guardrail` (which caps aggregate, not per-order). | S | **Core** | ✅ `Config.MaxOrderQty` / `MaxOrderNotional` |

### SHOULD — real gaps, clear defense

| # | Build | Why | Effort | Layer | Status |
|---|---|---|---|---|---|
| 7 | **Wire `pkg/auction` as a selectable clearing / open / close mode, randomized duration** | Marking-the-close (Athena $1M) + latency arb + MEV/JIT + momentum ignition — one change, four attacks. | L | **Core** | ✅ `auction.AuctionSession` + `RandomizedClose` |
| 8 | **Max-open-orders / account + min-notional / lot (dust) reject** before resting | Resource exhaustion. Binance congestion `-1008` (2025). Nothing caps total resting depth today. | M | **Core** | ✅ `MaxOrdersPerAccount`, `MinOrderQty`/`MinOrderNotional` |
| 9 | **`ClientOrderID` idempotency dedup** — reject a duplicate submit | Replay / duplicate-submit double-book. FIX PossDup abuse. | S | **Core** | ✅ `Config.DedupClientOrderIDs` |
| 10 | **Marking-the-close detector** — same-side aggression share in the final N seconds | Athena "Gravy" — nothing catches this today. | M | **Surveillance** | ✅ `surveillance.CloseMarkingDetector` |
| 11 | **Chunked / incremental `ForceTrade`** — size caps per call | Liquidation cascade. Hyperliquid HLP; BitMEX incremental-liquidation lesson. | M | **Core** | ✅ `Config.MaxForceTradeQty` |
| 12 | **Timed pause + reference recalc on band breach** (today the band rejects, doesn't pause) | LULD fidelity — breach → timed pause. | M | **Core** | ✅ `Config.BandBreachPause` (auto-resume) |

### COULD — lower enforcement weight or softer evidence

| # | Build | Why | Effort | Layer | Status |
|---|---|---|---|---|---|
| 13 | **Randomized iceberg peaks + true non-display** (deterministic refill is sniffable) | Order anticipation / pinging. Softer evidence. | M | **Core** | ✅ `Config.IcebergPeakJitter` |
| 14 | **Momentum-ignition / ramping detector** — impact-then-reversal | Usually charged bundled with spoofing. | M | **Surveillance** | ✅ `surveillance.RampingDetector` (directional push; reversal out of scope) |
| 15 | **Pinging detector** — bursts of tiny IOC / immediately-cancelled orders | Surgical pinging escapes `RateLimiter`. | M | **Surveillance** | ✅ `surveillance.PingingDetector` |
| 16 | **Asymmetric ingress speed bump on takers** | Latency arb (Flash Boys / IEX). Auction wiring (#7) covers it structurally. | L | **Gateway** | ✅ `gateway.Gateway` speed bump (+ `examples/gateway`) |
| 17 | **Cross-book `Monitor`** — fan multiple engines for cross-venue OTR / imbalance correlation | Cross-product manipulation (Oystacher $2.5M). | L | **Surveillance** | ✅ `surveillance.CrossBookMonitor` |
| 18 | **CAT-style export adapter** off the WAL event spine | Post-trade audit (Rule 613). WAL already gives the spine for free. | M | **Layer** | ✅ `examples/gateway` audit sink |
| 19 | **Guardrail-trip → `EventSink` alert** | Operator paging on the Knight tripwire. | S | **Core** | ✅ `EventHalted` / `EventResumed` |

---

## 6. What a pure matching core CANNOT defend — and why the boundary is correct

Everything below is out of scope **by design**, not by omission. Pushing these
into the matcher would compromise the determinism and neutrality that are its
core value.

- **Identity & beneficial ownership.** STP enforces the `TradeGroupID`s it is
  *told*. The core cannot know that two unlinked accounts share a beneficial
  owner (the collusive-wash and Mango-style cross-account case). Belongs to the
  **identity / risk layer** — ownership-clustering heuristics in the hot path
  would make matching non-deterministic and slow.
- **Credit, collateral, position limits, margin.** Cornering (Hunt silver
  $134M), short squeezes (LME nickel $3.9B), under-collateralization — all
  position-level and invisible to a single-book matcher. Belongs to the
  **credit / clearing layer**. The core contributes only *primitives*:
  `CancelOnly`/`Halted`, the mark-price band, `ForceTrade`, WAL-replay
  trade-bust.
- **Oracle sourcing.** The core can refuse to act on an *unclamped* mark
  (roadmap #4), but TWAP / EMA / multi-venue-median / staleness logic is an
  **oracle service** above. A matcher that sourced its own prices would couple
  matching to a data feed — the opposite of a neutral core.
- **Off-venue conduct.** Pump-and-dump / bear raids (SEC influencers ~$100M) and
  social-media ramping are disclosure / regulator concerns. No matching-engine
  defense exists; the band / halt only limit intraday damage.
- **Ingress ordering / MEV / sequencing fairness.** The core is deterministic
  *given the sequence it is handed* — this is what immunizes it against the
  dYdX-v4 proposer-reordering class. *Who orders the ingress stream* — encrypted
  mempool, commit-reveal, speed bump, private orderflow — is a **gateway /
  consensus** concern.
- **Auth, API keys, session, transport.** 3Commas (~$20M, 100K keys), IDOR order
  enumeration, FIX session-sequence validation, TLS — all **gateway / auth
  layer**. The core's one obligation here is a **trust boundary**: the
  `Privileged` / liquidation STP-exemption flag bypasses STP and widens band
  behavior, so the layer **must authenticate that flag** — a stolen privileged
  credential is worse than a normal one.
- **Governance.** Flash-loan governance (Beanstalk $182M) — timelocks and quorum
  snapshots are a **protocol-governance** concern with zero matching surface.

A matching core earns its trust by being *small, deterministic, and neutral*.
Every defense that requires identity, credit, price-sourcing, timing policy, or
off-venue context is deliberately delegated so the core stays bit-reproducible
and audit-replayable.

---

## 7. Sources

**Spoofing / layering / manipulation**
- CFTC — JPMorgan $920M (Release 8260-20); SEC 2020-233
- CFTC — Coscia (6649-13); the 2015 criminal conviction
- CFTC — Sarao (7156-15)
- FINRA — Trillium layering $2.26M (2010)
- CFTC — Oystacher / 3Red $2.5M (7504-16)

**Wash / self-trade**
- CFTC — Coinbase $6.5M (8369-21)
- SEC — CLS / ZM Quant / Gotbit (2024-166); DOJ CLS Global $428K
- CFTC — Shinhan (9125-25); CME wash-trade definition

**Marking the close / momentum**
- SEC — Athena "Gravy" $1M (2014-229); order 34-73369
- CFTC — Optiver banging the close (6239-12); Amaranth opinion

**Cornering / squeeze**
- Silver Thursday / Hunt brothers
- LME nickel 2022 — OFR working paper OFRwp-24-09
- SEC — GameStop / meme-stock staff report (2021)

**Technical / security**
- Bitcoin value-overflow CVE-2010-5139
- TOCTOU CVE-2026-34368
- Knight Capital $440M / SEC Rule 15c3-5 (34-63241); SEC 34-70694
- Binance trailing-stop halt (Mar 2023); Binance pipeline failure `-1008` (Oct 2025)
- ASX TMC outage (Nov 2020) — ASIC statement
- Citadel / Trillium quote stuffing
- 3Commas breach (Dec 2022)

**Predatory HFT**
- IEX speed bump / *Flash Boys* (SEC 2016)
- Budish-Cramton-Shim, frequent batch auctions (QJE 2015)
- Reg NMS Rule 612 sub-penny
- Minimum quote life (SEC / Schapiro); TSX Alpha speed bump
- Iceberg detection (CME, Zotikov 2019, arXiv:1909.09495)

**On-chain / MEV**
- CFTC v. Eisenberg / Mango $110M (8647-23)
- Hyperliquid JELLY (Mar 2025)
- bZx (Feb 2020); Harvest $34M (Oct 2020)
- Beanstalk $182M (Apr 2022)
- JIT liquidity (Uniswap v3); sandwich / MEV stats (arXiv:2411.03327)
- dYdX v4 proposer / MEV; Injective FBA
- Partial liquidation / ADL (BitMEX Oct-11 flash crash; BitMEX ADL docs)

**Surveillance / regulatory**
- Nasdaq SMARTS
- SEC Rule 15c3-5; LULD parameters
- CME Globex Self-Match Prevention (tag 2362)
- MiFID II RTS 6; ESMA algo-trading report
- Borsa Italiana OTR study
- Consolidated Audit Trail (Rule 613)

---

*Uncertainty notes: verdicts reference `pkg/matching`, `pkg/surveillance`, and
`pkg/auction` as of 2026-07-23. Roadmap rows 13-17 rest on softer evidence
(buy-side complaints, allegations) than the enforcement-backed MUST items —
weight accordingly. The crypto / oracle posture (§3.4) is the least-defended real
gap despite the band being present; do not read the band as a complete
oracle-manipulation defense.*
