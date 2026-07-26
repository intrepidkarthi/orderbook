# Research Roadmap — Market Microstructure

> The research layer exists to convert microstructure *claims* into
> *reproducible experiments*. Every item ships as **implementation → runnable
> experiment → honest write-up**, and the write-up must answer one question:
> **does the effect survive out-of-sample data and realistic trading costs?**

This agenda grew out of evaluating four widely-shared trading posts (order-flow
imbalance "predicts 62% of the next move", order-flow "trapped trader"
narratives, and the Avellaneda–Stoikov market-making model). The goal is not to
debunk or to hype — it is to *measure*, using an engine where we control ground
truth.

A guiding principle throughout:

> **Contemporaneous ≠ predictive.** A relationship that explains price moves over
> the *same* window is often near-mechanical. The money question is always
> whether it forecasts the *next* window, out-of-sample, after fees.

---

## 0. Data and scope

A standing objection to any microstructure work is "your data isn't deep enough."
It deserves a precise answer, because the answer here is unusual: this repository
does not *consume* order-book data, it *produces* it.

**What the engine emits.** Every order carries a persistent `int64` id for its
entire life. `matching.EventSink` streams the per-order lifecycle — `Accepted`,
`Rejected`, `Trade`, `Canceled`, `Triggered`, `Halted`, `Resumed` — each stamped
with a monotonic `Event.Seq`, and `OrderBook.SnapshotL3(depth)` returns the
order-by-order (market-by-order) book. That is L3/MBO granularity *at the
source*, with no inference and no gaps: the event sequence is the matching
engine's own history, not a reconstruction of it.

**What the simulator adds.** Experiments run in `pkg/sim` against something no
commercial feed can sell: **ground truth**. We know the true aggressor of every
trade, the λ we configured, and the informed-flow fraction. On real data the
aggressor side is *inferred* — the Lee-Ready / tick rule — and that inference is
noisy. Measuring how noisy is itself an experiment here (§4), and it is only
possible because there is a ground truth to check the inference against.

**What we do not have.** Real-market capture is thin. `cmd/l2capture` pulls live
**L2** from Coinbase, and the one study that has touched real data (§1) ran on
replayed L2. Redoing OFI on real market-by-order data is not possible with what
ships today. That is a genuine gap rather than a considered choice, and a
real-MBO capture path is a welcome contribution.

**On "L4".** Some crypto vendors market a *Level 4* tier: per-order events with
persistent ids, tracked from submission until the order leaves the matching
engine. Traditional venues have no such tier — both
[CME](https://www.cmegroup.com/articles/faqs/market-by-order-mbo.html) and
[Databento](https://databento.com/microstructure/mbo) treat market-by-order as
the most granular product that exists, and an ITCH-style MBO feed already carries
per-order add / modify / cancel / execute under stable ids. The label is newer
than the capability it describes.

**Why granularity is not the crux.** Deeper data raises *contemporaneous* R²: it
resolves the mechanism by which a move happened. It does not manufacture forecast
power, which is the claim §1 exists to test. The point cuts both ways, and the
honest version is this — at sub-second horizons, queue position from MBO carries
real edge, and serious market making depends on it. But that is latency-sensitive
execution, a different question from whether a book-imbalance number predicts the
next bar. No feed upgrade converts one into the other.

---

## 1. Order-Flow Imbalance (OFI)

> **Status: done.** Results in [research/ofi.md](research/ofi.md); reproduce with
> `go run ./cmd/ofistudy`. Headline: contemporaneous R² ≈ 0.17 against predictive
> R² ≈ 0.0003 — a ~540× gap, and the predictive slope is negative in nine seeds
> out of ten.

**Source.** Cont, Kukanov & Stoikov, *The Price Impact of Order Book Events*
(Journal of Financial Econometrics, 2014).

**The claim in the wild.** "The order book predicts the next move ~62% of the
time (R² ≈ 0.6)."

**What the paper actually shows.** A strong **contemporaneous** linear relation
between OFI and price change *over the same interval* — R² around 0.65–0.70. It
explains moves that are already happening (when the ask is eaten, that *is* the
move). It is not a next-tick forecast, and R² is variance-explained, not a
directional hit-rate.

**Definitions to implement** (`pkg/signals`):
- Best-level order-book imbalance: `(bidQty − askQty) / (bidQty + askQty)`.
- Depth-weighted imbalance over *k* levels.
- OFI proper: signed sum of best-level size changes driven by book events
  (adds/cancels/trades on bid vs ask) over a window.

**Experiments.**
1. Reproduce the **contemporaneous** regression on replayed L2 data → expect a
   high R². (Sanity check that our OFI matches the literature.)
2. Make it **predictive**: regress *next*-interval return on *this*-interval OFI,
   strictly out-of-sample → expect R² to collapse toward the 0.01–0.05 range.
3. Cost the gap: even where predictive R² is positive, subtract fees + latency +
   queue position and report net.

**Honest verdict criterion.** Report both R² values side by side. If predictive
edge does not survive costs, say so plainly.

---

## 2. Price Impact — Kyle's Lambda

> **Status: done.** Results in [research/kyle-lambda.md](research/kyle-lambda.md);
> reproduce with `go run ./cmd/lambdastudy`. Headline: λ ∝ 1/depth holds, and
> slicing a large order buys back the *temporary* impact but not the permanent
> one.

**Source.** Kyle, *Continuous Auctions and Insider Trading* (Econometrica, 1985).

**Idea.** `ΔP = λ × (signed order flow)`. λ is price impact per unit of volume —
the slope converting trading pressure into price movement. Deep books → small λ;
thin books → large λ.

**Why it belongs here.** It explains, mechanically, several things retail
experiences as bad luck: stop-losses hit then reversing (a liquidity pocket with
known λ), and why large players slice orders (minimizing total λ × flow).

**Ground truth.** `types.Trade.TakerSide` records which side crossed the spread,
assigned by the matching engine itself. Signed order flow is therefore *exact*
here — no Lee-Ready inference, no tick rule, no misclassification to correct for.
That is the whole reason λ is worth measuring in `pkg/sim` first (§0).

**Estimator** (`pkg/signals`):

- `SignedFlow(trades []*types.Trade) int64` — Σ `+qty` for buyer-initiated and
  `−qty` for seller-initiated trades, read off `TakerSide`.
- `EstimateLambda(flow, dMid []float64) LambdaFit` — OLS fit of `ΔP = λ·y + c`,
  returning `LambdaFit{Lambda, Intercept, R2, N}`. Thin Kyle-semantics wrapper
  over the existing `LinReg`; λ is in ticks per lot.

**Experiments** (`pkg/study`, runnable via `cmd/lambdastudy`):

1. **Estimator validation.** Generate a synthetic path `ΔP = λ·y + ε` with a
   *known* λ and confirm the estimator recovers it, with R² degrading as ε grows.
   This is a unit test of the instrument, not a claim about markets — recovering
   a λ you injected is circular by construction, and the write-up must say so
   rather than present it as a finding. It earns its place only because every
   number below depends on the estimator being right.
2. **Emergent λ.** Run the real engine with no λ parameter anywhere: λ is not
   configured, it *falls out* of the book's depth and the matching rules.
   Regress Δmid on signed flow per interval and report λ with its R².
3. **λ versus depth.** Sweep resting depth and re-estimate. Kyle predicts
   λ ∝ 1/depth; report the fitted relationship and whether it actually holds on
   an integer-tick book, where quantization bites at thin depths.
4. **Execution study.** One block marketable order of size *Q* against the same
   *Q* sliced into *n* children released over time, from an identical book and
   seed. Report, in ticks: arrival mid, execution VWAP, **implementation
   shortfall** (`(VWAP − arrival) × Q`, signed by side), **realized impact**
   (mid immediately after completion − arrival), and **permanent impact** (mid
   after a recovery window − arrival). The block-versus-slice gap, and the
   temporary component that decays once the pressure stops, is the mechanical
   reason execution algorithms exist.

**Honest verdict criterion.** Report λ with R² and interval count, never λ alone
— a slope fitted through noise is still a slope. If slicing does not beat the
block in this simulator, say so and explain what the simulator is missing
(most likely: noise traders that replenish too fast, making liquidity recovery
unrealistically generous).

**Write-up.** [research/kyle-lambda.md](research/kyle-lambda.md), establishing
the `docs/research/` results directory the methodology section (§5) has always
required.

---

## 3. Market Making — Avellaneda–Stoikov

**Source.** Avellaneda & Stoikov, *High-frequency trading in a limit order book*
(Quantitative Finance, 2008). Extensions: Guéant–Lehalle–Fernández-Tapia (2013,
inventory bounds + closed form), Cartea–Jaimungal (adverse selection), and the
ergodic treatment of Cao–Šiška–Szpruch–Treetanthiploet (2024).

**The two equations.**
- Reservation (indifference) price: `r = s − q·γ·σ²·(T − t)`.
- Optimal half-spread sum: `δ = γ·σ²·(T − t) + (2/γ)·ln(1 + γ/k)`.

Where `s` = mid, `q` = inventory, `γ` = risk aversion, `σ²` = variance,
`(T − t)` = time remaining, `k` = order-arrival decay.

**Implementation** (`pkg/strategy`): an `AvellanedaStoikov` quoter that, each
step, reads mid + inventory + clock and emits bid/ask around the
inventory-skewed reservation price. Plus the production adaptations the base
model omits, each toggleable so their effect is measurable:
- rolling **σ** estimation (constant-vol assumption is false in practice);
- rolling **k** estimation (Hummingbot-style);
- **inventory bounds** (GLT) — stop quoting the exposed side at a cap;
- an **adverse-selection** spread add-on;
- a **circuit breaker** for discontinuous moves.

**Experiments** (`pkg/backtest` against `pkg/sim`):
1. Baseline AS vs a naive fixed-spread quoter → PnL, inventory path, Sharpe.
2. Parameter sweeps over `γ, k, σ`; show inventory-skew and spread behavior.
3. **Adverse-selection stress:** inject informed flow; show where naive AS loses
   and which adaptation recovers it.
4. Honest accounting: the formula is necessary structure, **not** sufficient
   edge — quantify how much of real MM P&L comes from fees/rebates and queue
   position vs the model itself.

**Note.** A widely-shared AS explainer labeled its code "Rust" while showing
Python, and cited (accurately) papers as recent as a March 2026 HSBC FX
preprint. We keep the citations, fix the label, and — crucially — *measure*
rather than assert.

---

## 4. Retail Order Flow — Delta, CVD, Absorption

> **Status: done.** Results in [research/order-flow.md](research/order-flow.md);
> reproduce with `go run ./cmd/flowstudy`. Headline: the tick rule is 94.5%
> accurate per trade and still builds a CVD wrong by 169%; CVD divergence beats
> its base rate but loses to a price-only control, so the CVD half adds nothing;
> absorption predicts nothing, though the mechanism itself is real.

**The claim in the wild.** Volume + delta + open interest reveal "trapped
traders" who are forced to exit, and that forced exit drives price; CVD
divergences mark reversals.

**What's real vs what's narrative.** The primitives are real: **delta** (signed
aggressor volume), **CVD** (cumulative delta), open-interest mechanics
(futures are zero-sum; OI up = opening, down = closing), and **absorption**
(passive limits eating aggressive flow without price moving). The *storytelling*
— every move re-explained after the fact as a squeeze — is largely unfalsifiable
and measurement-dependent (aggressor side is usually *inferred*, e.g. via the
tick / Lee-Ready rule, which is noisy).

**Why this one matters most.** Every retail order-flow tool is built on *inferred*
aggressor side, because no public feed publishes it. Here the engine assigns it
(`Trade.TakerSide`), so we can measure the inference error itself — the one
experiment on this agenda that cannot be run on purchased data at any
granularity, no matter how deep (§0).

**Signals** (`pkg/signals/flow.go`):

- Delta is already implemented: it *is* `SignedFlow` from the Kyle work. Reuse
  it; do not add a second name for the same quantity.
- `CVD` — a streaming cumulative-delta accumulator shaped like the existing `OFI`
  type (`Observe`/`Value`/`Reset`).
- `TickRuleSide(price, prevPrice, prevSide)` — the classic tick test: uptick is a
  buy, downtick a sell, zero-tick repeats the previous classification.
- `LeeReadySide(price, mid, prevPrice, prevSide)` — the quote rule (above the mid
  is a buy, below a sell), falling back to the tick test exactly at the mid.
- `AbsorptionDetector` — flags a window carrying large |delta| with little price
  movement: aggressive flow being eaten by passive size.
- `CVDDivergence` — flags price making a higher high while CVD makes a lower
  high, and the mirror case.

**Stats** (`pkg/signals/stats.go`): `WilsonInterval(successes, n, z)`. Any
hit-rate claim below is reported with it — a bare percentage over a few hundred
episodes is not a result.

**Experiments** (`pkg/study/flow.go`, runnable via `cmd/flowstudy`):

1. **How wrong is the inferred aggressor?** Classify every simulated trade with
   the tick rule and with Lee-Ready, compare against the engine's truth, and
   report per-trade accuracy. Then report what practitioners actually care about:
   the error in the *derived* series — inferred CVD versus true CVD, as terminal
   drift and as correlation over the path. Per-trade accuracy can look
   respectable while the cumulative series walks away, because misclassifications
   do not cancel; that gap is the finding.
2. **Do CVD divergences precede reversals?** Detect divergences, then measure how
   often a reversal follows within a fixed horizon. Report the hit-rate **beside
   the unconditional base rate** with Wilson intervals on both. A 55% hit-rate
   against a 54% base rate is nothing, and reporting it without the base rate is
   the single most common way this genre oversells itself. Then subtract costs.
3. **Absorption → squeeze.** Construct the episode deliberately in `pkg/sim`: a
   large passive wall eats aggressive flow, then pulls, and price gaps. This
   shows the mechanism is *real*. Separately, measure how often absorption is
   followed by a directional move across ordinary runs — the gap between "this
   can happen" and "this is tradable" is the entire point, and both numbers must
   appear together.

**Honest verdict criterion.** Report the base rate next to every hit-rate, and
the inference error next to every inferred-aggressor result. If divergences do
not beat their base rate, publish that — §5 says a rigorous null is a result, and
this is the item most likely to produce one.

**Write-up.** `docs/research/order-flow.md`.

---

## 5. Methodology & guardrails

- **Ground truth first.** Wherever possible, validate a signal in `pkg/sim`
  (known aggressor, known λ, known informed-flow fraction) before touching noisy
  real data.
- **Out-of-sample or it didn't happen.** Train/estimate on one slice, evaluate on
  another. No in-sample victory laps.
- **Costs are not optional.** Every "edge" is reported gross *and* net of fees,
  spread, and a latency/queue assumption.
- **Reproducibility.** Each experiment is a runnable `cmd/` binary or notebook
  with a fixed seed; results checked in as a short markdown write-up.
- **Publish nulls.** A signal that doesn't work, shown rigorously, is a result —
  and more useful than most trading content.

---

## 6. References

- Cont, Kukanov, Stoikov (2014), *The Price Impact of Order Book Events*, JFE.
- Kyle (1985), *Continuous Auctions and Insider Trading*, Econometrica.
- Avellaneda, Stoikov (2008), *High-frequency trading in a limit order book*, QF.
- Guéant, Lehalle, Fernández-Tapia (2013), inventory-constrained market making.
- Cartea, Jaimungal, et al., adverse-selection extensions.
- Cao, Šiška, Szpruch, Treetanthiploet (2024), *Logarithmic regret in the
  ergodic Avellaneda–Stoikov market making model* (arXiv:2409.02025).
