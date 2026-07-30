# Order-Flow Imbalance — contemporaneous, not predictive

> **Reproduce:** `go run ./cmd/ofistudy`
> **Code:** `pkg/signals/ofi.go`, `pkg/study/ofi.go`
> **Spec:** [research-roadmap.md §1](../research-roadmap.md#1-order-flow-imbalance-ofi)

## The claim

A widely-shared version of the order-flow-imbalance result says the order book
**"predicts the next move ~62% of the time, R² ≈ 0.6."**

The underlying paper — Cont, Kukanov & Stoikov (2014) — says something different
and much narrower. It reports a strong **contemporaneous** linear relation
between OFI and price change *over the same interval*, with R² around 0.65–0.70.
Two things get lost in the retelling:

- **R² is variance explained, not a directional hit-rate.** "R² ≈ 0.6" and
  "right 62% of the time" are not the same statement, and neither implies the
  other.
- **Same-interval is not next-interval.** When the ask gets eaten, that *is* the
  move. Explaining a price change with the flow that caused it is close to
  mechanical.

This experiment separates the two.

## Method

5,000 intervals per seed against the real matching engine, with uninformed noise
traders supplying flow. Each interval contributes a best-level OFI value
(`signals.OFIStep`, following the paper's e_n construction) and a mid-price
change. Two regressions:

- **contemporaneous** — Δprice[i] on OFI[i]
- **predictive** — Δprice[i+1] on OFI[i], the actual forecasting question

## Results

Ten seeds, N = 4,999 intervals each:

| seed | contemporaneous R² | predictive R² | contemporaneous slope | predictive slope |
|---|---|---|---|---|
| 1 | 0.2697 | 0.0003 | +0.20506 | −0.00670 |
| 2 | 0.2941 | 0.0001 | +0.19483 | −0.00392 |
| 3 | 0.1968 | 0.0000 | +0.21388 | +0.00038 |
| 4 | 0.2406 | 0.0001 | +0.18006 | −0.00434 |
| 5 | 0.1621 | 0.0000 | +0.20644 | −0.00243 |
| 6 | 0.2457 | 0.0005 | +0.18167 | +0.00774 |
| 7 | 0.2770 | 0.0020 | +0.19458 | −0.01634 |
| 8 | 0.1193 | 0.0003 | +0.19767 | −0.01051 |
| 9 | 0.2813 | 0.0007 | +0.17843 | −0.00863 |
| 10 | 0.2700 | 0.0001 | +0.18671 | +0.00377 |
| **mean** | **0.2357** | **0.0004** | | |

Contemporaneous R² beats predictive R² by roughly **577×**.

> **Re-measured 2026-07-30.** These numbers changed when a bug was fixed in the
> engine's aggregated depth: a price level's total was not reduced when a resting
> order was fully consumed, so L2 depth was over-reported after every complete fill.
> OFI is computed from level depth, so the contemporaneous figure was affected —
> mean R² was previously reported as 0.1685 with a ~540× gap. The predictive figure
> is unchanged at ~0.0004, and the conclusion is *strengthened* rather than
> weakened: OFI explains more of the same-interval move than we thought, and still
> essentially none of the next one. `cmd/ofistudy` now computes the verdict from its
> own results instead of carrying them as literal text, which is what allowed the
> printed summary to drift from the table above it.

**The contemporaneous relation is real and correctly signed.** The slope is
positive in all ten seeds: net buying pressure at the touch coincides with the
price going up. Nobody should be surprised — that is what buying pressure *is*.

**The predictive relation is nothing.** Mean R² of 0.0003 means this-interval OFI
explains about three hundredths of one percent of next-interval price variance.
There is no forecast here.

**And the little that is there points the wrong way.** The predictive slope is
*negative* in nine of ten seeds — a whisper of mean reversion, not continuation.
The claim in the wild says a lopsided book tells you where price goes next; the
sign of the (vanishing) relationship leans mildly against that. At R² ≈ 0.0003
this is noise and should not be traded on in either direction, but it is the
opposite of the story being sold.

## Where this understates the effect

Honesty runs both ways, and this simulator is *not* generous to the
contemporaneous result. Our 0.17 is well below the 0.65–0.70 the paper reports on
real equity data.

The likely reason is the flow: our noise traders post limits up to 40 ticks off
the reference price, so much of their activity never touches the best level —
and best-level OFI, by construction, only sees the touch. Real order flow is far
more concentrated near the spread, which tightens the same-interval relation.

That gap is worth stating plainly, but it does not rescue the claim. The
predictive R² is 0.0003, three orders of magnitude below anything tradable. A
simulator that better reproduced the contemporaneous fit would move the number
the claim *already had right* and leave the number the claim depends on
untouched.

## Verdict

**Contemporaneous ≠ predictive.** OFI describes a price move as it happens; it
does not forecast the next one. The paper never claimed otherwise — the 62%
retelling invented a forecast the research does not contain, then converted a
variance-explained figure into a hit-rate it was never equivalent to.

This is the guiding principle of the whole research agenda, and it is here
because it is the single most common way microstructure results get oversold.

## References

- Cont, R., Kukanov, A., Stoikov, S. (2014). *The Price Impact of Order Book
  Events*. Journal of Financial Econometrics 12(1).
- Companion study: [Kyle's λ](kyle-lambda.md) — price impact, depth, and
  execution cost.
