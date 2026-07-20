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

## 1. Order-Flow Imbalance (OFI)

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

**Source.** Kyle, *Continuous Auctions and Insider Trading* (Econometrica, 1985).

**Idea.** `ΔP = λ × (signed order flow)`. λ is price impact per unit of volume —
the slope converting trading pressure into price movement. Deep books → small λ;
thin books → large λ.

**Why it belongs here.** It explains, mechanically, several things retail
experiences as bad luck: stop-losses hit then reversing (a liquidity pocket with
known λ), and why large players slice orders (minimizing total λ × flow).

**Experiments** (best done in `pkg/sim`, where we know ground truth):
1. Estimate λ by regressing price change on signed flow across controlled
   simulated sessions; recover the λ we configured.
2. Show λ scales inversely with book depth.
3. **Execution study:** one large marketable order vs the same quantity sliced —
   measure realized impact and implementation shortfall. Reproduces why
   execution algos exist.

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

**Implementation** (`pkg/signals`): delta, CVD, absorption detector (high delta,
little price movement), and a "CVD divergence" flag.

**Experiments.**
1. In `pkg/sim`, where we *know* the true aggressor, quantify how badly the
   inferred-aggressor tick rule mislabels flow.
2. **Base-rate test:** does a "CVD divergence" precede a reversal more often than
   chance? Report the hit-rate with confidence intervals and after costs.
3. Reproduce an absorption → squeeze episode in simulation to show the mechanism
   is *possible*, while separating "possible" from "reliably tradable".

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
