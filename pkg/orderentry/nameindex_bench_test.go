package orderentry

import (
	"strconv"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The naming index runs on the matching goroutine, which makes its cost the venue's
// cost. These two benchmarks are the same workload with and without it, so the
// difference is what synchronous naming charges every accepted order.

func benchOrders(b *testing.B, n int) []*types.Order {
	b.Helper()
	out := make([]*types.Order, n)
	for i := range out {
		o, err := types.NewOrder("alice", "X", types.SideBuy, types.OrderTypeLimit,
			int64(100+i%50), 5, types.TIFGoodTillCancel)
		if err != nil {
			b.Fatalf("NewOrder: %v", err)
		}
		o.ClientOrderID = "c" + strconv.Itoa(i)
		out[i] = o
	}
	return out
}

func BenchmarkAcceptWithoutNaming(b *testing.B) {
	orders := benchOrders(b, b.N)
	e := matching.NewEngine(matching.DefaultConfig("X"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Process(orders[i])
	}
}

func BenchmarkAcceptWithNaming(b *testing.B) {
	orders := benchOrders(b, b.N)
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = NewNameIndex(NewRegistry("INC1", 4096))
	e := matching.NewEngine(cfg)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Process(orders[i])
	}
}

// BenchmarkName isolates the map write, against a naming table held at the size of a
// live book.
//
// The first version of this benchmark inserted b.N distinct keys into an
// ever-growing map and reported 450 ns/op — Go's map growth and the cache misses of a
// sixteen-million-entry table, not the cost of a write. A venue's naming table is
// bounded by its live book, which is thousands, and the number that matters is the
// one at that size.
func BenchmarkName(b *testing.B) {
	const live = 10_000
	reg := NewRegistry("INC1", 4096)
	orders := benchOrders(b, live)
	for i, o := range orders {
		o.ID = int64(i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reg.Name(orders[i%live])
	}
}

// BenchmarkNameAndForget is the pair, which is what actually happens: every named
// order is eventually unnamed. Measuring only the write would flatter the table by
// leaving out the half that keeps it bounded.
func BenchmarkNameAndForget(b *testing.B) {
	const live = 10_000
	reg := NewRegistry("INC1", 4096)
	orders := benchOrders(b, live)
	for i, o := range orders {
		o.ID = int64(i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o := orders[i%live]
		reg.Name(o)
		reg.forget(o)
	}
}

// BenchmarkOrderIDFor is the read a cancel does. It takes the read side of a lock the
// matcher writes, so contention here is contention with the venue.
func BenchmarkOrderIDFor(b *testing.B) {
	reg := NewRegistry("INC1", 4096)
	orders := benchOrders(b, 4096)
	for _, o := range orders {
		o.ID = 1
		reg.Name(o)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reg.OrderIDFor("alice", "c"+strconv.Itoa(i%4096))
	}
}
