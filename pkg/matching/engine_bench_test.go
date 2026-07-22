package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func mkOrder(user string, side types.Side, price int64, qty int64) *types.Order {
	o, _ := types.NewOrder(user, "X", side, types.OrderTypeLimit,
		price, qty, types.TIFGoodTillCancel)
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

// BenchmarkEngine_CancelReplace measures market-maker churn through the full
// engine: a book of ~K resting orders, cancel one and re-post another each step.
func BenchmarkEngine_CancelReplace(b *testing.B) {
	const K = 5000
	e := NewEngine(Config{Symbol: "X", MaxOrders: K + 10})
	live := make([]*types.Order, K)
	for i := range live {
		live[i] = mkOrder("mm", types.SideBuy, int64(1000+i%2000), 1)
		e.Process(live[i])
	}
	repl := make([]*types.Order, b.N)
	for i := range repl {
		repl[i] = mkOrder("mm", types.SideBuy, int64(1000+i%2000), 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % K
		_, _ = e.Cancel(live[j].ID, "mm")
		e.Process(repl[i])
		live[j] = repl[i]
	}
}

// --- P4: zero-allocation hot path via Match(order, buf) ---

// BenchmarkEngine_MatchInto is the full match round-trip through the zero-alloc
// path: a resting sell then a crossing buy, matched into a reused trade buffer.
// Steady state, the consumed maker's node recycles for the next insert.
func BenchmarkEngine_MatchInto(b *testing.B) {
	makers := make([]*types.Order, b.N)
	takers := make([]*types.Order, b.N)
	for i := range makers {
		makers[i] = mkOrder("maker", types.SideSell, 1000, 1)
		takers[i] = mkOrder("taker", types.SideBuy, 1000, 1)
	}
	e := NewEngine(DefaultConfig("X"))
	buf := make([]types.Trade, 0, 8)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, _, _ = e.Match(makers[i], buf[:0])
		buf, _, _ = e.Match(takers[i], buf[:0])
	}
}

// BenchmarkEngine_CancelReplaceInto is market-maker churn through the zero-alloc
// path: a book of ~K resting orders, cancel one and re-post another each step.
func BenchmarkEngine_CancelReplaceInto(b *testing.B) {
	const K = 5000
	e := NewEngine(Config{Symbol: "X", MaxOrders: K + 10})
	buf := make([]types.Trade, 0, 8)
	live := make([]*types.Order, K)
	for i := range live {
		live[i] = mkOrder("mm", types.SideBuy, int64(1000+i%2000), 1)
		buf, _, _ = e.Match(live[i], buf[:0])
	}
	repl := make([]*types.Order, b.N)
	for i := range repl {
		repl[i] = mkOrder("mm", types.SideBuy, int64(1000+i%2000), 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % K
		_, _ = e.Cancel(live[j].ID, "mm")
		buf, _, _ = e.Match(repl[i], buf[:0])
		live[j] = repl[i]
	}
}
