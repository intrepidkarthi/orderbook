package orderbook

import (
	"runtime"
	"testing"
)

// The zero-allocation claim in docs/BENCHMARKS.md cannot be checked from the
// benchmark table it is printed in. Go computes allocs/op as integer division —
// `int64(MemAllocs) / int64(N)` — so anything below 1.0 prints as "0". A path that
// allocated 0.99 objects per operation would be published as allocation-free.
//
// BenchmarkOrderBook_Cancel reports "0 allocs/op" and, in the same line, 41 B/op.
// Those two are only consistent if a handful of large allocations are being
// amortised across the run, and reading the table alone cannot tell that from
// three quarters of an allocation on every cancel.
//
// These tests measure the ratio directly, so the claim is pinned by something that
// can fail rather than by a rounded-down column.

// allocsPerOp runs fn n times and reports allocations and bytes per operation.
func allocsPerOp(n int, fn func(i int)) (allocs, bytes float64) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < n; i++ {
		fn(i)
	}
	runtime.ReadMemStats(&after)
	return float64(after.Mallocs-before.Mallocs) / float64(n),
		float64(after.TotalAlloc-before.TotalAlloc) / float64(n)
}

// TestCancelIsAllocationFree — cancel is the dominant operation in real order flow,
// so it is the one the claim most needs to hold for.
func TestCancelIsAllocationFree(t *testing.T) {
	const n = 200_000
	ob := New(Config{Symbol: "X", MaxOrders: n + 1})
	orders := benchOrders(n)
	for _, o := range orders {
		_ = ob.Add(o)
	}

	allocs, bytes := allocsPerOp(n, func(i int) { _, _ = ob.Remove(orders[i].ID) })
	t.Logf("cancel: %.4f allocs/op, %.2f B/op", allocs, bytes)
	// Measured ~0.0002 (a few dozen allocations across 200k cancels). The bound is
	// loose enough not to be flaky and tight enough that one allocation per cancel
	// would break it by two orders of magnitude.
	if allocs > 0.01 {
		t.Errorf("cancel allocates %.4f objects/op; the published claim is that it allocates none", allocs)
	}
}

// TestCancelReplaceIsAllocationFree is the market-maker steady state, and the
// pattern the node pooling exists for: the node a cancel releases is the node the
// next post takes.
func TestCancelReplaceIsAllocationFree(t *testing.T) {
	const k, n = 10_000, 200_000
	ob := New(Config{Symbol: "X", MaxOrders: k + 10})
	live := benchOrders(k)
	for _, o := range live {
		_ = ob.Add(o)
	}
	repl := benchOrders(n)

	allocs, bytes := allocsPerOp(n, func(i int) {
		j := i % k
		_, _ = ob.Remove(live[j].ID)
		_ = ob.Add(repl[i])
		live[j] = repl[i]
	})
	t.Logf("cancel/replace: %.4f allocs/op, %.2f B/op", allocs, bytes)
	if allocs > 0.05 {
		t.Errorf("cancel/replace allocates %.4f objects/op; pooling should make this ~0", allocs)
	}
}

// TestAddAloneDoesAllocate is the honest other half, and the reason the docs say
// pooled rather than allocation-free without qualification. Adding orders to a
// growing book has nothing to reuse — the pool only pays back once something has
// been released into it. Asserting a floor here stops the pooling claim from being
// quietly widened into one the code does not make.
func TestAddAloneDoesAllocate(t *testing.T) {
	const n = 200_000
	ob := New(Config{Symbol: "X", MaxOrders: n + 1})
	orders := benchOrders(n)

	allocs, bytes := allocsPerOp(n, func(i int) { _ = ob.Add(orders[i]) })
	t.Logf("add (growing book): %.4f allocs/op, %.2f B/op", allocs, bytes)
	if allocs < 0.5 {
		t.Errorf("add allocates only %.4f objects/op — if this became free, the docs understate the engine and should be corrected", allocs)
	}
}
