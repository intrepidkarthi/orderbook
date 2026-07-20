// Package signals computes market-microstructure signals from order-book
// snapshots: book imbalance and order-flow imbalance (OFI) to start.
//
// These are the first entries in the research agenda (docs/research-roadmap.md).
// A standing caveat travels with them: the well-known strong relationship
// between OFI and price change is *contemporaneous* (it explains the move over
// the same interval), not a proven next-tick forecast. This package computes the
// signals; the experiments that test their predictive value live in the research
// and backtest layers.
//
// Signals are dimensionless and returned as float64, which is the natural type
// for feeding regressions and plots — distinct from the exact decimals used for
// money in the core library.
package signals

import (
	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/shopspring/decimal"
)

// DepthImbalance returns the depth-weighted order-book imbalance over the top
// `levels` levels of each side:
//
//	(ΣbidQty − ΣaskQty) / (ΣbidQty + ΣaskQty)  ∈ [−1, 1]
//
// Positive means more resting bid size than ask size (buy pressure); negative
// the reverse. Returns 0 only for an empty book (no size on either side) or
// non-positive levels; a fully one-sided book yields ±1 — all pressure on one
// side, which is a meaningful extreme rather than "no signal".
func DepthImbalance(snap *orderbook.Snapshot, levels int) float64 {
	if snap == nil || levels <= 0 {
		return 0
	}
	bid := sumTopQty(snap.Bids, levels)
	ask := sumTopQty(snap.Asks, levels)
	den := bid.Add(ask)
	if den.IsZero() {
		return 0
	}
	return bid.Sub(ask).Div(den).InexactFloat64()
}

// BestImbalance is DepthImbalance over only the best (top) level of each side.
func BestImbalance(snap *orderbook.Snapshot) float64 {
	return DepthImbalance(snap, 1)
}

func sumTopQty(levels []orderbook.SnapshotLevel, n int) decimal.Decimal {
	sum := decimal.Zero
	for i := 0; i < len(levels) && i < n; i++ {
		sum = sum.Add(levels[i].Quantity)
	}
	return sum
}
