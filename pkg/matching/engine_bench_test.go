package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func mkOrder(user string, side types.Side, price int64, qty int64) *types.Order {
	o, _ := types.NewOrder(user, "X", side, types.OrderTypeLimit,
		decimal.NewFromInt(price), decimal.NewFromInt(qty), types.TIFGoodTillCancel)
	return o
}

// BenchmarkEngine_RestingInsert measures the cost of processing limit orders that
// rest (no cross) — the insert hot path through the full engine.
func BenchmarkEngine_RestingInsert(b *testing.B) {
	e := NewEngine(Config{Symbol: "X", MaxOrders: b.N + 1})
	orders := make([]*types.Order, b.N)
	for i := range orders {
		// Bids well below any ask so nothing crosses.
		orders[i] = mkOrder("u", types.SideBuy, int64(1000+i%2000), 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Process(orders[i])
	}
}

// BenchmarkEngine_Match measures a full match round-trip: a resting sell followed
// by a crossing buy that trades against it.
func BenchmarkEngine_Match(b *testing.B) {
	makers := make([]*types.Order, b.N)
	takers := make([]*types.Order, b.N)
	for i := range makers {
		makers[i] = mkOrder("maker", types.SideSell, 1000, 1)
		takers[i] = mkOrder("taker", types.SideBuy, 1000, 1)
	}
	e := NewEngine(DefaultConfig("X"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Process(makers[i])
		e.Process(takers[i])
	}
}
