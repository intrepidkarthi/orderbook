# Retail order flow — delta, CVD, absorption, and trapped traders

> **Reproduce:** `go run ./cmd/flowstudy`
> **Code:** `pkg/signals/flow.go`, `pkg/signals/stats.go`, `pkg/study/flow.go`, `pkg/study/squeeze.go`
> **Spec:** [research-roadmap.md §4](../research-roadmap.md#4-retail-order-flow--delta-cvd-absorption)

## The claim

Volume, delta, and open interest are said to reveal "trapped traders" who are
forced to exit, and that forced exit is said to drive price. CVD divergences mark
reversals. Absorption marks the moment a move is about to fail.

The primitives here are real. **Delta** (signed aggressor volume), **CVD**
(cumulative delta), and **absorption** (passive size eating aggressive flow
without price moving) are all well-defined and measurable. What follows tests the
claims built on top of them, and adds a question the genre rarely asks: whether
the numbers are even being *measured* correctly in the first place.

---

## 1. The aggressor side is guessed, and the guess compounds

No public feed publishes which side initiated a trade. Every delta and CVD figure
in every retail order-flow tool is built on an *inference* — usually the tick
rule, sometimes Lee-Ready. Here the matching engine assigns the true aggressor
(`Trade.TakerSide`), so the inference can be scored.

Ten seeds, ~9,400 trades each:

| seed | tick accuracy | tick CVD error | path R² | true CVD | tick CVD |
|---|---|---|---|---|---|
| 1 | 0.9445 | 521% | 0.7713 | −78 | −484 |
| 2 | 0.9484 | 71% | 0.9096 | 308 | 90 |
| 3 | 0.9395 | 41% | 0.7839 | −503 | −709 |
| 4 | 0.9462 | 36% | 0.9582 | −710 | −454 |
| 5 | 0.9454 | 631% | 0.2415 | −65 | −475 |
| 6 | 0.9425 | 177% | 0.5187 | **105** | **−81** |
| 7 | 0.9444 | 44% | 0.8091 | 365 | 203 |
| 8 | 0.9447 | 45% | 0.8142 | −125 | −181 |
| 9 | 0.9485 | 86% | 0.0310 | 377 | 53 |
| 10 | 0.9447 | 36% | 0.9529 | −478 | −652 |

**The tick rule is 94.5% accurate per trade — and that is not good enough.**
Mean CVD error is **169%** of the true series' magnitude. In seed 6 the inferred
CVD is **−81 while the truth is +105**: the sign is wrong, so the indicator says
"net selling" during a period of net buying. In seed 9 the inferred path tracks
the true one with an R² of **0.031** — no relationship at all — off 94.85%
per-trade accuracy.

The reason is simple and worth stating plainly: **classification errors do not
cancel.** A 5.5% error rate would be harmless if mistakes were symmetric noise,
but the tick rule fails in a correlated way — it misreads the same market
conditions the same way every time — so the errors accumulate into the running
sum instead of averaging out. Per-trade accuracy is the wrong statistic to quote.
Nobody trades a single label; they trade the cumulative series.

### What this simulator cannot tell you

**Lee-Ready scores 100% here, and that is an artifact, not a finding.** Every
aggressive order in this simulator is a market order that executes at the touch,
so a rule that classifies by comparing price to the midpoint cannot be wrong.
Real venues have hidden liquidity, mid-point executions, and quotes that are
stale relative to the print — the literature puts Lee-Ready nearer 85% on real
equity data. The honest reading of this section is about the *tick rule*, and
about the general mechanism of compounding error, not a league table of the two
methods. A test pins the 100% so that if the simulator ever gains those features,
it fails and this section gets revisited.

---

## 2. CVD divergences: the signal is real, the CVD part is not

A bearish divergence — price makes a higher high while cumulative delta does not
— is supposed to mark exhausted buyers and a coming reversal. Scored over a
20-step horizon, pooled across ten seeds:

| | hit rate | 95% interval | n |
|---|---|---|---|
| CVD divergence | 0.5056 | [0.4888, 0.5225] | 3,382 |
| **price extreme only** | **0.5208** | **[0.5096, 0.5319]** | **7,717** |
| base rate | 0.4582 | | 9,980 |

Read the first row alone and there is a result: divergences hit 50.6% against a
base rate of 45.8%, the intervals do not overlap, so the edge is statistically
real.

Read the second row and it evaporates. **"Price made a new extreme" — the same
directional call with the CVD condition deleted — scores 52.1%, higher than the
full divergence, and their intervals overlap.** Everything the divergence
detector achieves is achieved by the price half alone. The CVD condition
contributes nothing; it only fires less often.

What the detector is actually finding is **mean reversion after a new extreme**.
That is a real property of this market. It is not what the claim says it is, and
anyone attributing it to order-flow exhaustion has misread their own indicator.

This is why the control matters more than the base rate. Beating the base rate
proves a signal contains information; it does not prove the *interesting half*
of the signal contains any. A test that only ever compares against a base rate
will confirm compound indicators indefinitely.

---

## 3. Absorption predicts nothing

Absorption is heavy one-sided aggressive flow that fails to move the price. The
squeeze reading says the absorbed side is now trapped and price should travel
against them. Threshold set at the run's own 75th-percentile window delta, so it
adapts rather than depending on a magic constant:

| | hit rate | 95% interval | n |
|---|---|---|---|
| after absorption | 0.4816 | [0.4491, 0.5143] | 899 |
| base rate | 0.4939 | [0.4841, 0.5037] | 9,980 |

Edge: **−1.2 points**, intervals overlapping. Nothing. If anything the sign is
mildly against the claim, but at this sample size that is noise and should not be
read as a contrarian signal either.

---

## 4. The mechanism, though, is entirely real

None of the above means absorption is imaginary. Constructed deliberately — a
large passive bid placed one tick above the best bid, then hit with 600 lots of
aggressive selling:

| seed | absorbed | move with wall | move without wall | reversal leg |
|---|---|---|---|---|
| 1 | 600 | 0.0 | −53.5 | +13.5 |
| 2 | 600 | 0.0 | −64.5 | +12.0 |
| 3 | 600 | 0.0 | −55.0 | +100.5 |
| 4 | 600 | 0.0 | −58.0 | +7.0 |
| 5 | 600 | 0.0 | −37.0 | +18.0 |

The wall eats the entire order and **price does not move at all**. The identical
pressure without the wall drops price by 37 to 65 ticks. When buying then
arrives, price travels.

The control arm is what makes this evidence. "Price held while selling hit the
book" could just be a quiet market; only running the same pressure without the
wall shows what the wall prevented.

So the mechanism is real, reproducible, and mechanically obvious. **Section 3
says that spotting it tells you nothing about what happens next.** Both are true
at once, and the gap between them is where most order-flow content lives.

---

## Verdict

- **The primitives are sound.** Delta, CVD, and absorption are well-defined, and
  absorption demonstrably holds price against real pressure.
- **The predictive claims do not survive.** Absorption has no detectable edge.
  CVD divergence has one, but it belongs entirely to the price-extreme half —
  the CVD condition adds nothing and the effect is mean reversion.
- **And on real data, the inputs are not even measured correctly.** A 94.5%
  accurate aggressor rule yields a CVD wrong by 169% on average, occasionally
  with the opposite sign. Every conclusion drawn from an inferred CVD inherits
  that error, and no data vendor can sell you the fix, because the ground truth
  exists only inside the matching engine.

**What this does not establish.** These are uninformed noise traders in a
simulator with no hidden liquidity, no latency, and no informed flow. A market
with genuinely informed participants might well contain an order-flow signal this
one cannot. The claim here is narrower and sturdier: the specific patterns as
usually described do not work *even in a market simple enough to check them
exactly*, and the measurement problem underneath them is real everywhere.

## References

- Lee, C., Ready, M. (1991). *Inferring Trade Direction from Intraday Data*.
  Journal of Finance 46(2).
- Companion studies: [OFI](ofi.md) — contemporaneous versus predictive;
  [Kyle's λ](kyle-lambda.md) — price impact and execution cost.
