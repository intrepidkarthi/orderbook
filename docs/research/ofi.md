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
| 1 | 0.2199 | 0.0003 | +0.15718 | −0.00549 |
| 2 | 0.2397 | 0.0005 | +0.14886 | −0.00708 |
| 3 | 0.1567 | 0.0000 | +0.15985 | −0.00155 |
| 4 | 0.1355 | 0.0000 | +0.09388 | −0.00147 |
| 5 | 0.1058 | 0.0008 | +0.12494 | −0.01054 |
| 6 | 0.1544 | 0.0004 | +0.10496 | +0.00545 |
| 7 | 0.2364 | 0.0007 | +0.15394 | −0.00844 |
| 8 | 0.0704 | 0.0003 | +0.10824 | −0.00655 |
| 9 | 0.1935 | 0.0001 | +0.11295 | −0.00246 |
| 10 | 0.1728 | 0.0000 | +0.10967 | −0.00029 |
| **mean** | **0.1685** | **0.0003** | | |

Contemporaneous R² beats predictive R² by roughly **540×**.

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
