package matching

import (
	"sort"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// BenchmarkLatency_CancelHeavy reports the latency *distribution* (p50/p99/p999)
// of a realistic ~90%-cancel / 10%-new order flow against a warm book, not just
// the mean. Tail latency is what a production venue cares about; a low mean can
// still hide GC- or rehash-induced spikes.
//
// Each op is timed with time.Now, whose ~tens-of-ns overhead is added uniformly,
// so treat the absolute numbers as an upper bound and the p99/p999-vs-p50 *shape*
// as the signal.
func BenchmarkLatency_CancelHeavy(b *testing.B) {
	const warm = 20000
	e := NewEngine(Config{Symbol: "X", MaxOrders: warm + b.N + 16})
	live := make([]int64, 0, warm) // resting order ids we can cancel
	buf := make([]types.Trade, 0, 8)

	price := func(i int) int64 { return int64(1000 + i%4000) } // ±2000-tick book, no crossing

	for i := 0; i < warm; i++ {
		o := mkOrder("mm", types.SideBuy, price(i), 1)
		buf, _, _ = e.Match(o, buf[:0])
		live = append(live, o.ID)
	}

	lat := make([]time.Duration, b.N)
	next := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%10 == 0 || len(live) == 0 {
			// 10%: post a new resting order.
			o := mkOrder("mm", types.SideBuy, price(warm+next), 1)
			next++
			start := time.Now()
			buf, _, _ = e.Match(o, buf[:0])
			lat[i] = time.Since(start)
			live = append(live, o.ID)
		} else {
			// 90%: cancel a resting order (drawn round-robin).
			id := live[len(live)-1]
			live = live[:len(live)-1]
			start := time.Now()
			_, _ = e.Cancel(id, "mm")
			lat[i] = time.Since(start)
		}
	}
	b.StopTimer()

	if b.N < 100 {
		return // too few samples for stable percentiles
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	b.ReportMetric(float64(pctl(lat, 0.50)), "p50-ns")
	b.ReportMetric(float64(pctl(lat, 0.99)), "p99-ns")
	b.ReportMetric(float64(pctl(lat, 0.999)), "p999-ns")
	b.ReportMetric(float64(lat[len(lat)-1].Nanoseconds()), "max-ns")
}

// pctl returns the q-quantile (0<q<1) of a sorted duration slice, in nanoseconds.
func pctl(sorted []time.Duration, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx].Nanoseconds()
}
