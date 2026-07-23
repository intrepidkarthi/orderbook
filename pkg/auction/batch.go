package auction

import (
	"sort"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// BatchAuction accumulates orders and clears them in a single uniform-price
// uncross — the frequent-batch-auction model (e.g. Injective) that replaces
// continuous-time priority with a periodic sealed batch, removing intra-batch
// front-running and latency races. Every executed order trades at one clearing
// price. It is an alternative to the continuous matching engine, built on the same
// Uncross clearing-price rule.
type BatchAuction struct {
	symbol string
	buys   []*types.Order
	sells  []*types.Order
}

// NewBatchAuction returns an empty batch for a symbol.
func NewBatchAuction(symbol string) *BatchAuction { return &BatchAuction{symbol: symbol} }

// Add accumulates a limit order into the batch (insertion order is the tie-break
// on allocation).
func (b *BatchAuction) Add(o *types.Order) {
	if o.Side == types.SideBuy {
		b.buys = append(b.buys, o)
	} else {
		b.sells = append(b.sells, o)
	}
}

// BatchResult is the outcome of a batch uncross.
type BatchResult struct {
	HasClearing   bool
	ClearingPrice int64
	Volume        int64
	Trades        []types.Trade
}

// Cross computes the single clearing price that maximises executed volume and
// fills every crossing order at that one price — aggressive prices first, then
// insertion order. Non-crossing orders are left unfilled. It mutates the filled
// orders' quantities.
func (b *BatchAuction) Cross() BatchResult {
	u := Uncross(aggregate(b.buys), aggregate(b.sells))
	res := BatchResult{HasClearing: u.HasClearing, ClearingPrice: u.ClearingPrice, Volume: u.Volume}
	if !u.HasClearing {
		return res
	}
	cp := u.ClearingPrice

	buys := eligible(b.buys, func(o *types.Order) bool { return o.Price >= cp })
	sells := eligible(b.sells, func(o *types.Order) bool { return o.Price <= cp })
	// Priority: better price first, insertion order (time) on ties.
	sort.SliceStable(buys, func(i, j int) bool { return buys[i].Price > buys[j].Price })
	sort.SliceStable(sells, func(i, j int) bool { return sells[i].Price < sells[j].Price })

	bi, si := 0, 0
	for bi < len(buys) && si < len(sells) {
		buy, sell := buys[bi], sells[si]
		q := min(buy.RemainingQty, sell.RemainingQty)
		if q > 0 {
			_ = buy.Fill(q)
			_ = sell.Fill(q)
			res.Trades = append(res.Trades, types.NewTradeValue(b.symbol, cp, q, buy, sell, types.SideBuy))
		}
		if buy.RemainingQty == 0 {
			bi++
		}
		if sell.RemainingQty == 0 {
			si++
		}
	}
	return res
}

// aggregate collapses orders into price levels (summing remaining quantity).
func aggregate(orders []*types.Order) []Level {
	byPrice := make(map[int64]int64, len(orders))
	for _, o := range orders {
		byPrice[o.Price] += o.RemainingQty
	}
	out := make([]Level, 0, len(byPrice))
	for p, q := range byPrice {
		out = append(out, Level{Price: p, Qty: q})
	}
	return out
}

func eligible(orders []*types.Order, ok func(*types.Order) bool) []*types.Order {
	out := make([]*types.Order, 0, len(orders))
	for _, o := range orders {
		if o.RemainingQty > 0 && ok(o) {
			out = append(out, o)
		}
	}
	return out
}
