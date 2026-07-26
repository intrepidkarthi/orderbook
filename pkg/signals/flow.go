package signals

import (
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Delta — signed aggressor volume — is exactly SignedFlow, which the Kyle work
// already provides. It is deliberately not re-exported under a second name: the
// order-flow literature and the price-impact literature give the same quantity
// different labels, and having one function keeps the two studies measuring the
// identical thing.

// CVD accumulates cumulative volume delta: the running sum of signed aggressor
// volume. Feed it successive trade batches with Observe.
//
// CVD is the backbone of retail order-flow analysis, and on real data it is
// built from an *inferred* aggressor side (see TickRuleSide and LeeReadySide).
// Feeding it engine-assigned sides, as the simulator can, gives the true series
// that inference is trying to approximate. Not safe for concurrent use.
type CVD struct {
	cum int64
}

// NewCVD returns an empty accumulator.
func NewCVD() *CVD { return &CVD{} }

// Observe folds a batch of trades into the running total and returns the batch's
// own delta.
func (c *CVD) Observe(trades []*types.Trade) int64 {
	d := SignedFlow(trades)
	c.cum += d
	return d
}

// Add folds a pre-computed delta into the running total. Use it when the
// aggressor side came from inference rather than from the engine.
func (c *CVD) Add(delta int64) { c.cum += delta }

// Value returns the running cumulative delta in lots.
func (c *CVD) Value() int64 { return c.cum }

// Reset clears the accumulator.
func (c *CVD) Reset() { c.cum = 0 }

// TickRuleSide classifies a trade's aggressor from price movement alone: a trade
// above the previous price is buyer-initiated, below is seller-initiated, and an
// unchanged price repeats the previous classification.
//
// This is what anyone without an aggressor-tagged feed is reduced to, and it is
// the noisiest of the standard rules. prevSide seeds the zero-tick case; pass
// SideBuy for the first trade of a series (the choice washes out quickly).
func TickRuleSide(price, prevPrice int64, prevSide types.Side) types.Side {
	switch {
	case price > prevPrice:
		return types.SideBuy
	case price < prevPrice:
		return types.SideSell
	default:
		return normalizeSide(prevSide)
	}
}

// LeeReadySide classifies a trade against the prevailing quote midpoint: above
// the mid is buyer-initiated, below is seller-initiated. Trades exactly at the
// mid — which the tick rule alone must resolve — fall back to TickRuleSide.
//
// Lee & Ready (1991) is the standard improvement on the bare tick test, since a
// trade's position relative to the spread carries more information than its
// position relative to the last print. Pass a non-positive mid when no two-sided
// quote exists, and the tick rule handles it.
func LeeReadySide(price int64, mid float64, prevPrice int64, prevSide types.Side) types.Side {
	if mid <= 0 {
		return TickRuleSide(price, prevPrice, prevSide)
	}
	switch p := float64(price); {
	case p > mid:
		return types.SideBuy
	case p < mid:
		return types.SideSell
	default:
		return TickRuleSide(price, prevPrice, prevSide)
	}
}

func normalizeSide(s types.Side) types.Side {
	if s == types.SideSell {
		return types.SideSell
	}
	return types.SideBuy
}

// AbsorptionConfig parameterizes the absorption detector.
type AbsorptionConfig struct {
	MinDelta  int64 // minimum |delta| in the window to qualify as heavy flow
	MaxMove   int64 // maximum |price change| in ticks still counted as "absorbed"
	MinTrades int   // minimum executions in the window
}

// Absorbed reports whether a window shows absorption: heavy one-sided aggressive
// flow that failed to move the price, which is passive size soaking it up.
//
// Absorption is a real and observable mechanic. Whether it *predicts* anything is
// a separate question, and one the study answers separately — a detector firing
// is not evidence of a tradable edge.
func (c AbsorptionConfig) Absorbed(delta int64, priceMove int64, trades int) bool {
	if trades < c.MinTrades {
		return false
	}
	return abs64(delta) >= c.MinDelta && abs64(priceMove) <= c.MaxMove
}

// DivergenceKind labels a CVD/price divergence.
type DivergenceKind uint8

const (
	NoDivergence      DivergenceKind = iota
	BearishDivergence                // price made a higher high, CVD did not
	BullishDivergence                // price made a lower low, CVD did not
)

func (d DivergenceKind) String() string {
	switch d {
	case BearishDivergence:
		return "BEARISH"
	case BullishDivergence:
		return "BULLISH"
	default:
		return "NONE"
	}
}

// Extremes are the price and CVD extremes reached over one window.
type Extremes struct {
	PriceHigh, PriceLow float64
	CVDHigh, CVDLow     float64
}

// Divergence compares two consecutive windows and reports whether price and
// cumulative delta disagree about the direction of conviction.
//
// The retail reading is that a higher high on weakening cumulative delta means
// buyers are exhausted and a reversal is due. That is a hypothesis, not a fact,
// and it is worth exactly the base-rate test the study puts it through.
func Divergence(prev, cur Extremes) DivergenceKind {
	if cur.PriceHigh > prev.PriceHigh && cur.CVDHigh <= prev.CVDHigh {
		return BearishDivergence
	}
	if cur.PriceLow < prev.PriceLow && cur.CVDLow >= prev.CVDLow {
		return BullishDivergence
	}
	return NoDivergence
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
