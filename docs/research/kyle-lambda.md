# Kyle's λ — price impact, depth, and the cost of trading all at once

> **Reproduce:** `go run ./cmd/lambdastudy`
> **Code:** `pkg/signals/kyle.go`, `pkg/study/kyle.go`, `pkg/study/execution.go`
> **Spec:** [research-roadmap.md §2](../research-roadmap.md#2-price-impact--kyles-lambda)

Kyle (1985) models price impact as a straight line:

```
ΔP = λ · y
```

where `y` is signed order flow — aggressive buy volume minus aggressive sell
volume — and λ is the price move per unit of that flow. λ is a property of
*liquidity*, not of the asset. A deep book absorbs flow with barely a flicker; a
thin one gets shoved.

Everything below runs in `pkg/sim` against the real matching engine, where the
aggressor of every trade is known exactly (`Trade.TakerSide`) rather than
inferred with a tick rule. See [§0 Data and scope](../research-roadmap.md#0-data-and-scope)
for why that matters and what it does not buy us.

---

## 1. Does the ruler work?

Before measuring anything, check the instrument. Build a synthetic path
`ΔP = λ·y + ε` with λ = 0.25 and re-estimate it:

| true λ | recovered | R² | noise σ |
|---|---|---|---|
| 0.2500 | 0.2506 | 0.9744 | 2 |
| 0.2500 | 0.2516 | 0.8601 | 5 |
| 0.2500 | 0.2565 | 0.2853 | 20 |

λ survives noise; R² is what degrades. That is the behaviour you want — an
estimator whose slope drifted under noise would corrupt every number below.

**This is not a finding.** Recovering a λ that was injected by construction is
circular. It earns its place only as a calibration step.

---

## 2. The λ a real book produces

Now with no λ configured anywhere. Order flow is uninformed noise traders; λ is
whatever emerges from the book's depth and the matching rules. 5,000 intervals
per seed:

| seed | λ (ticks/lot) | R² | depth (lots) | trades |
|---|---|---|---|---|
| 1 | 0.15508 | 0.1733 | 333.7 | 2479 |
| 2 | 0.15406 | 0.1808 | 424.4 | 2468 |
| 3 | 0.15332 | 0.1764 | 336.2 | 2412 |
| 4 | 0.14560 | 0.1782 | 475.7 | 2395 |
| 5 | 0.15154 | 0.1725 | 408.3 | 2371 |

λ lands near 0.15 ticks per lot and is stable across seeds. R² ≈ 0.17: signed
flow explains about a sixth of the variance in mid-price change.

That R² deserves a plain reading. It is *contemporaneous* — flow and price move
measured over the same interval — so it is largely mechanical, exactly the
caveat the [OFI study](../research-roadmap.md#1-order-flow-imbalance-ofi)
insists on. Nothing here forecasts anything.

---

## 3. λ versus depth

Kyle's real content is that impact is the inverse of liquidity. Scaling the
noise traders' order sizes deepens the book; depth is then *measured* rather
than assumed, so the x-axis is observed liquidity, not the knob:

| scale | depth (lots) | λ (ticks/lot) | R² | λ·depth |
|---|---|---|---|---|
| 1 | 333.7 | 0.15508 | 0.1733 | 51.7 |
| 2 | 867.4 | 0.07321 | 0.1751 | 63.5 |
| 4 | 1361.4 | 0.03825 | 0.1822 | 52.1 |
| 8 | 2430.7 | 0.02065 | 0.1865 | 50.2 |

Depth rises 7.3×; λ falls 7.5×. **λ ∝ 1/depth holds.** λ·depth shows no
systematic trend across the sweep, and λ falls monotonically with depth in every
seed tested.

The honest caveat: λ·depth is not *constant*, it wanders between roughly 42 and
69 across seeds and rungs — about ±25%. On an integer-tick book, small price
moves are quantized away, which biases λ at the deep end where the true moves
are smallest. The proportionality is real; the constant is approximate.

---

## 4. What impact costs: one block versus ten slices

400 lots, executed two ways from a byte-identical book and seed — one marketable
block, or ten children released 20 simulator steps apart. Averaged over 50 seeds,
in ticks:

| | block | sliced |
|---|---|---|
| slippage per lot | 28.42 | 26.16 |
| realized impact | 33.79 | 27.92 |
| permanent impact | 23.42 | 24.47 |
| unfilled lots (50 runs) | 1506 | 32 |

**Slicing is cheaper.** 7.9% less slippage per lot, and cheaper in 42 of 50 runs.

**The block often cannot finish.** It left 1,506 lots unexecuted across 50 runs —
roughly 8% of everything it tried to trade — against 32 for the schedule. The
sliced arm filled at least as much as the block in 50 of 50 runs. A market order
large enough to matter can simply run out of book, and that failure never shows
up in a slippage number.

**Permanent impact is the same either way** — 23.42 versus 24.47, a difference
well inside the run-to-run noise. This is the result worth sitting with, and it
is what the theory predicts: the same quantity ultimately traded, so the same
information reached the price. **Slicing does not reduce your permanent footprint.
It buys back the temporary one.**

### Where this result is weaker than it looks

The realized-impact row is a claim about the *mean*, not about a typical run. Per
seed, the sliced arm's immediate impact is lower only 25 times out of 50 — a coin
flip. The average gap is carried by the minority of runs where the block chews
deep into the book. Reported as "block orders move the price more on average,
driven by tail events," it holds; reported as "block orders always move the price
more," it does not.

---

## Verdict

λ is a real, measurable property of a book, it scales as 1/depth, and it is the
mechanical reason large orders are worked rather than dumped. The execution
result reproduces why execution algorithms exist — with the sharper point that
what slicing buys is the *temporary* component, not the permanent one.

**What this does not establish.** Every number here comes from a simulator whose
liquidity replenishes on a schedule we chose. Slicing wins partly because the
noise traders refill the book between children, and a real venue under stress
refills more slowly and less reliably — which would flatter slicing less. The
result demonstrates the mechanism is real and measurable; it is not a calibration
of what slicing is worth on any actual venue.

## References

- Kyle, A. (1985). *Continuous Auctions and Insider Trading*. Econometrica 53(6).
- Cont, R., Kukanov, A., Stoikov, S. (2014). *The Price Impact of Order Book
  Events*. Journal of Financial Econometrics 12(1).
