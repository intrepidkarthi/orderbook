// Package auction implements a call-auction uncross: given resting buy and sell
// interest, it finds the single clearing price that maximises executed volume
// and reports how much trades there. This is the mechanism behind opening and
// closing auctions (docs/SPEC.md §5.2) — orders accumulate, then cross once at a
// common price instead of continuously.
package auction

import (
	"sort"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/shopspring/decimal"
)

// Level is aggregated size at a price.
type Level struct {
	Price decimal.Decimal
	Qty   decimal.Decimal
}

// Result is the outcome of an uncross.
type Result struct {
	HasClearing   bool
	ClearingPrice decimal.Decimal
	Volume        decimal.Decimal // quantity that trades at the clearing price
}

// Uncross computes the clearing price that maximises matched volume across bids
// and asks. Among prices with equal volume it prefers the one with the smallest
// buy/sell imbalance, then the lowest price (a deterministic tie-break). If
// nothing crosses, HasClearing is false.
func Uncross(bids, asks []Level) Result {
	best := Result{}
	bestImbalance := decimal.Zero
	first := true

	for _, p := range candidatePrices(bids, asks) {
		demand := sumQtyAtLeast(bids, p) // buyers willing to pay ≥ p
		supply := sumQtyAtMost(asks, p)  // sellers willing to accept ≤ p
		vol := decimal.Min(demand, supply)
		if vol.LessThanOrEqual(decimal.Zero) {
			continue
		}
		imbalance := demand.Sub(supply).Abs()

		better := !best.HasClearing || vol.GreaterThan(best.Volume) ||
			(vol.Equal(best.Volume) && imbalance.LessThan(bestImbalance))
		if first || better {
			best = Result{HasClearing: true, ClearingPrice: p, Volume: vol}
			bestImbalance = imbalance
			first = false
		}
	}
	return best
}

// FromSnapshot runs an uncross over an order-book snapshot's aggregated levels.
func FromSnapshot(s *orderbook.Snapshot) Result {
	bids := make([]Level, 0, len(s.Bids))
	for _, b := range s.Bids {
		bids = append(bids, Level{Price: b.Price, Qty: b.Quantity})
	}
	asks := make([]Level, 0, len(s.Asks))
	for _, a := range s.Asks {
		asks = append(asks, Level{Price: a.Price, Qty: a.Quantity})
	}
	return Uncross(bids, asks)
}

func sumQtyAtLeast(levels []Level, p decimal.Decimal) decimal.Decimal {
	sum := decimal.Zero
	for _, l := range levels {
		if l.Price.GreaterThanOrEqual(p) {
			sum = sum.Add(l.Qty)
		}
	}
	return sum
}

func sumQtyAtMost(levels []Level, p decimal.Decimal) decimal.Decimal {
	sum := decimal.Zero
	for _, l := range levels {
		if l.Price.LessThanOrEqual(p) {
			sum = sum.Add(l.Qty)
		}
	}
	return sum
}

// candidatePrices returns the distinct prices across both sides, ascending.
func candidatePrices(bids, asks []Level) []decimal.Decimal {
	seen := make(map[string]decimal.Decimal)
	for _, l := range bids {
		seen[l.Price.String()] = l.Price
	}
	for _, l := range asks {
		seen[l.Price.String()] = l.Price
	}
	out := make([]decimal.Decimal, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LessThan(out[j]) })
	return out
}
