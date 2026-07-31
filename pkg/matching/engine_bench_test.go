package matching

import (
	"testing"
	"time"

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

// BenchmarkEngine_Match measures one maker/taker pair through the matching core:
// a resting sell followed by a crossing buy that trades against it. Orders are
// constructed before ResetTimer, so the figure excludes order allocation and
// covers the matching path only — not decoding, validation, or any I/O.
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

// BenchmarkEngine_MatchInto is one maker/taker pair through the zero-alloc path:
// a resting sell then a crossing buy, matched into a reused trade buffer.
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

// BenchmarkEngine_Cancel measures the ENGINE's cancel, not the book's.
//
// The published figures covered OrderBook.Remove, and every comparison against other
// libraries used that too. But a venue calls Engine.Cancel, which additionally checks
// ownership, enforces the minimum resting time, stamps the order, emits an event and —
// since DAY and GTD — services the expiry schedule. None of that was measured, and the
// gap hid a real regression: an unconditional clock read added to this path would have
// roughly doubled it, and no benchmark would have noticed.
func BenchmarkEngine_Cancel(b *testing.B) {
	e := NewEngine(Config{Symbol: "X", MaxOrders: b.N + 16})
	ids := make([]int64, 0, b.N)
	buf := make([]types.Trade, 0, 8)
	for i := 0; i < b.N; i++ {
		o := mkOrder("mm", types.SideBuy, int64(1000+i%4000), 1)
		buf, _, _ = e.Match(o, buf[:0])
		ids = append(ids, o.ID)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = e.Cancel(ids[i], "mm")
	}
}

// BenchmarkEngine_CancelWithExpiries is the same path on a venue that uses DAY orders,
// so the expiry schedule is non-empty and actually gets consulted. The difference
// between this and BenchmarkEngine_Cancel is what the feature costs a venue that uses
// it, which is the number worth knowing rather than the one that flatters it.
func BenchmarkEngine_CancelWithExpiries(b *testing.B) {
	close := time.Now().Add(24 * time.Hour)
	cfg := Config{Symbol: "X", MaxOrders: b.N + 16}
	cfg.SessionClose = func() time.Time { return close }
	e := NewEngine(cfg)
	ids := make([]int64, 0, b.N)
	buf := make([]types.Trade, 0, 8)
	for i := 0; i < b.N; i++ {
		o := mkOrder("mm", types.SideBuy, int64(1000+i%4000), 1)
		o.TimeInForce = types.TIFDay
		buf, _, _ = e.Match(o, buf[:0])
		ids = append(ids, o.ID)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = e.Cancel(ids[i], "mm")
	}
}
