package orderbook

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func benchOrders(n int) []*types.Order {
	orders := make([]*types.Order, n)
	one := decimal.NewFromInt(1)
	for i := range orders {
		// Spread across ~2000 price levels to exercise the sorted ladder.
		price := decimal.NewFromInt(int64(1000 + i%2000))
		o, _ := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, price, one, types.TIFGoodTillCancel)
		orders[i] = o
	}
	return orders
}

func BenchmarkOrderBook_Add(b *testing.B) {
	ob := New(Config{Symbol: "X", MaxOrders: b.N + 1})
	orders := benchOrders(b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ob.Add(orders[i])
	}
}

func BenchmarkOrderBook_BestBid(b *testing.B) {
	ob := New(Config{Symbol: "X", MaxOrders: 20000})
	for _, o := range benchOrders(10000) {
		_ = ob.Add(o)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = ob.BestBid()
	}
}
