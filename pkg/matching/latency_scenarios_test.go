package matching

import (
	"sort"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Per-scenario tail-latency benchmarks.
//
// docs/BENCHMARKS.md published p50/p99/p999 for exactly one scenario, a
// cancel-heavy mix. That is one shape of order flow, and the operations it does not
// cover are the ones most likely to have a bad tail: sweeping a thin book, pulling
// an account's whole book at once, and the self-trade-prevention paths. A venue
// writes its SLO on p99 and p99.9, so an unmeasured operation is an unpriced risk.
//
// The methodology is deliberately borrowed from joaquinbejar/OrderBook-rs, whose
// HDR bench suite does this properly and is the better practice: name each scenario,
// state its preload, discard a warmup, and report out to p99.99 rather than stopping
// at a median that hides everything interesting.
//
// Two honesty notes that belong next to the numbers:
//
//   - Each operation is timed with time.Now, whose overhead (~27ns here) is included
//     in every sample. The absolute figures are therefore an upper bound; the shape,
//     p99 and p99.9 against p50, is the signal.
//   - Every scenario states the book size it ran against, because for a book-level
//     operation the book size is part of the result — the lesson from the cancel
//     figure that silently meant a ten-million-order book.

// latencyRun accumulates samples for one scenario and reports its distribution.
type latencyRun struct {
	samples []time.Duration
}

func (r *latencyRun) time(fn func()) {
	start := time.Now()
	fn()
	r.samples = append(r.samples, time.Since(start))
}

// report publishes the quantiles a venue quotes. Below a few hundred samples the
// upper quantiles are not meaningful, so they are omitted rather than reported as
// though they were.
func (r *latencyRun) report(b *testing.B, preload int) {
	b.Helper()
	b.ReportMetric(float64(preload), "preload")
	if len(r.samples) < 500 {
		return
	}
	sort.Slice(r.samples, func(i, j int) bool { return r.samples[i] < r.samples[j] })
	b.ReportMetric(float64(pctl(r.samples, 0.50)), "p50-ns")
	b.ReportMetric(float64(pctl(r.samples, 0.99)), "p99-ns")
	b.ReportMetric(float64(pctl(r.samples, 0.999)), "p999-ns")
	b.ReportMetric(float64(pctl(r.samples, 0.9999)), "p9999-ns")
	b.ReportMetric(float64(r.samples[len(r.samples)-1].Nanoseconds()), "max-ns")
}

// warmBook preloads n non-crossing bids and returns the engine and their ids.
func warmBook(b *testing.B, n int, extra int) (*Engine, []int64, []types.Trade) {
	b.Helper()
	e := NewEngine(Config{Symbol: "X", MaxOrders: n + extra + 16})
	ids := make([]int64, 0, n)
	buf := make([]types.Trade, 0, 8)
	for i := 0; i < n; i++ {
		o := mkOrder("mm", types.SideBuy, int64(1000+i%4000), 1)
		buf, _, _ = e.Match(o, buf[:0])
		ids = append(ids, o.ID)
	}
	return e, ids, buf
}

// BenchmarkLatency_AddOnly — pure passive insertion into a growing book. The book
// grows across the measurement, so this is also where cache behaviour shows up.
func BenchmarkLatency_AddOnly(b *testing.B) {
	const preload = 20_000
	e, _, buf := warmBook(b, preload, b.N)
	var r latencyRun
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o := mkOrder("mm", types.SideBuy, int64(1000+(preload+i)%4000), 1)
		r.time(func() { buf, _, _ = e.Match(o, buf[:0]) })
	}
	b.StopTimer()
	r.report(b, preload)
}

// BenchmarkLatency_CancelOnly — a preloaded book drained in order. Cancel is the
// dominant operation in real order flow, so this is the one worth watching.
func BenchmarkLatency_CancelOnly(b *testing.B) {
	preload := b.N + 1000
	e, ids, _ := warmBook(b, preload, 0)
	var r latencyRun
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := ids[i]
		r.time(func() { _, _ = e.Cancel(id, "mm") })
	}
	b.StopTimer()
	r.report(b, preload)
}

// BenchmarkLatency_AggressiveWalk — takers sweeping across price levels, which is
// the worst case for the match loop: every fill removes a maker and may empty a
// level, and emptying a level touches the sorted price slice.
func BenchmarkLatency_AggressiveWalk(b *testing.B) {
	const preload = 20_000
	e := NewEngine(Config{Symbol: "X", MaxOrders: preload + b.N + 16})
	buf := make([]types.Trade, 0, 64)
	// A deep ask book, one lot per order, spread over 200 levels.
	for i := 0; i < preload; i++ {
		o := mkOrder("mm", types.SideSell, int64(2000+i%200), 1)
		buf, _, _ = e.Match(o, buf[:0])
	}
	var r latencyRun
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A 5-lot buy at the top of the range walks several orders and sometimes
		// several levels.
		o := mkOrder("tk", types.SideBuy, 2200, 5)
		r.time(func() { buf, _, _ = e.Match(o, buf[:0]) })
		if e.OrderCount() < 100 {
			b.StopTimer()
			for j := 0; j < 10_000; j++ {
				o := mkOrder("mm", types.SideSell, int64(2000+j%200), 1)
				buf, _, _ = e.Match(o, buf[:0])
			}
			b.StartTimer()
		}
	}
	b.StopTimer()
	r.report(b, preload)
}

// BenchmarkLatency_Mixed_70_20_10 — 70% passive submit, 20% cancel, 10% aggressive.
// A plausible venue mix rather than a single operation, which is what makes its p99
// the number closest to an SLO.
func BenchmarkLatency_Mixed_70_20_10(b *testing.B) {
	const preload = 20_000
	e, ids, buf := warmBook(b, preload, b.N)
	live := ids
	var r latencyRun
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		switch {
		case i%10 == 9: // 10% aggressive
			o := mkOrder("tk", types.SideSell, 1000, 1)
			r.time(func() { buf, _, _ = e.Match(o, buf[:0]) })
		case i%10 >= 7: // 20% cancel
			if len(live) == 0 {
				continue
			}
			id := live[len(live)-1]
			live = live[:len(live)-1]
			r.time(func() { _, _ = e.Cancel(id, "mm") })
		default: // 70% passive
			o := mkOrder("mm", types.SideBuy, int64(1000+(preload+i)%4000), 1)
			r.time(func() { buf, _, _ = e.Match(o, buf[:0]) })
			live = append(live, o.ID)
		}
	}
	b.StopTimer()
	r.report(b, preload)
}

// BenchmarkLatency_MassCancelBurst — the bulk-cancel worst case, measured end to end
// as one observation per burst rather than amortised per order.
//
// This is the operator kill switch and the market maker's panic button, it became
// durable in v0.13.0, and it has never been measured. Its cost is O(book), so the
// number that matters is how long the matching goroutine is unavailable to everyone
// else while it runs.
//
// RUN THIS WITH A SMALL -benchtime. Each iteration rebuilds a 5,000-order book, so
// wall-clock is O(b.N x book) even though the timer excludes the rebuild:
// -benchtime=200000x asks for a billion insertions and takes tens of minutes. The
// timing is correct at any b.N; only your patience scales. -benchtime=200x is plenty.
func BenchmarkLatency_MassCancelBurst(b *testing.B) {
	const perAccount = 5_000
	var r latencyRun
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		e := NewEngine(Config{Symbol: "X", MaxOrders: perAccount + 16})
		buf := make([]types.Trade, 0, 8)
		for j := 0; j < perAccount; j++ {
			o := mkOrder("victim", types.SideBuy, int64(1000+j%4000), 1)
			buf, _, _ = e.Match(o, buf[:0])
		}
		b.StartTimer()
		r.time(func() { e.CancelAllForUser("victim") })
	}
	b.StopTimer()
	// One sample per iteration, so the quantiles need many iterations to mean
	// anything; the mean ns/op is the useful figure here.
	r.report(b, perAccount)
}

// BenchmarkLatency_STPSweep — self-trade prevention on the hot path. Five modes ship
// and none was ever measured; DECREMENT in particular mutates a maker in place mid
// match, which is the kind of thing that hides a tail.
func BenchmarkLatency_STPSweep(b *testing.B) {
	for _, mode := range []struct {
		name string
		stp  SelfTradePrevention
	}{
		{"Allow", STPAllow},
		{"CancelNewest", STPCancelNewest},
		{"CancelOldest", STPCancelOldest},
		{"CancelBoth", STPCancelBoth},
		{"Decrement", STPDecrement},
	} {
		b.Run(mode.name, func(b *testing.B) {
			cfg := DefaultConfig("X")
			cfg.SelfTradePrevention = mode.stp
			cfg.MaxOrders = 2*b.N + 1000
			e := NewEngine(cfg)
			buf := make([]types.Trade, 0, 8)
			var r latencyRun
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Same account on both sides, so every taker meets its own maker and
				// the STP path runs on every iteration.
				maker := mkOrder("self", types.SideSell, 1500, 1)
				buf, _, _ = e.Match(maker, buf[:0])
				taker := mkOrder("self", types.SideBuy, 1500, 1)
				r.time(func() { buf, _, _ = e.Match(taker, buf[:0]) })
			}
			b.StopTimer()
			r.report(b, 0)
		})
	}
}

// BenchmarkLatency_ThinBook — sweeping a book with one order per level, so a taker
// walks the maximum number of levels per lot. The worst realistic case for the price
// slice, and the shape a venue sees in an illiquid instrument or a stressed market.
func BenchmarkLatency_ThinBook(b *testing.B) {
	const levels = 5_000
	e := NewEngine(Config{Symbol: "X", MaxOrders: levels + b.N + 16})
	buf := make([]types.Trade, 0, 64)
	for i := 0; i < levels; i++ {
		o := mkOrder("mm", types.SideSell, int64(3000+i), 1) // one lot per level
		buf, _, _ = e.Match(o, buf[:0])
	}
	var r latencyRun
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o := mkOrder("tk", types.SideBuy, 3020, 10) // walks ~10 levels
		r.time(func() { buf, _, _ = e.Match(o, buf[:0]) })
		if e.OrderCount() < 200 {
			b.StopTimer()
			for j := 0; j < 4_000; j++ {
				o := mkOrder("mm", types.SideSell, int64(3000+j), 1)
				buf, _, _ = e.Match(o, buf[:0])
			}
			b.StartTimer()
		}
	}
	b.StopTimer()
	r.report(b, levels)
}
