package orderbook

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func benchOrders(n int) []*types.Order {
	orders := make([]*types.Order, n)
	for i := range orders {
		// Spread across ~2000 price levels to exercise the sorted ladder.
		price := int64(1000 + i%2000)
		o, _ := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, price, 1, types.TIFGoodTillCancel)
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

// --- P0: benchmarks that stress the weak paths (cancel, level churn) ---

// BenchmarkOrderBook_Cancel drains a large book by cancelling every order —
// exercises the O(1) node unlink + O(log P) price-slice removal.
func BenchmarkOrderBook_Cancel(b *testing.B) {
	ob := New(Config{Symbol: "X", MaxOrders: b.N + 1})
	orders := benchOrders(b.N)
	for _, o := range orders {
		_ = ob.Add(o)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ob.Remove(orders[i].ID)
	}
}

// BenchmarkOrderBook_CancelReplace is the market-maker steady state: a book of
// ~K resting orders, cancel one and re-post another each iteration.
func BenchmarkOrderBook_CancelReplace(b *testing.B) {
	const K = 10000
	ob := New(Config{Symbol: "X", MaxOrders: K + 10})
	live := benchOrders(K)
	for _, o := range live {
		_ = ob.Add(o)
	}
	repl := benchOrders(b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % K
		_, _ = ob.Remove(live[j].ID)
		_ = ob.Add(repl[i])
		live[j] = repl[i]
	}
}

// BenchmarkOrderBook_LevelChurn adds and removes a brand-new price level against
// a dense book — exercises the O(P) sorted-price-slice shift. The base book sits
// on even ticks and the churn on the interleaved odd ticks so every insert lands
// mid-slice.
func BenchmarkOrderBook_LevelChurn(b *testing.B) {
	ob := New(Config{Symbol: "X", MaxOrders: 5000})
	for i := 0; i < 2000; i++ {
		price := int64(1000 + 2*i) // even ticks
		o, _ := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, price, 1, types.TIFGoodTillCancel)
		_ = ob.Add(o)
	}
	churn := make([]*types.Order, b.N)
	for i := range churn {
		price := int64(1001 + 2*(i%2000)) // interleaved odd ticks (new levels)
		o, _ := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, price, 1, types.TIFGoodTillCancel)
		churn[i] = o
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ob.Add(churn[i])
		_, _ = ob.Remove(churn[i].ID)
	}
}
