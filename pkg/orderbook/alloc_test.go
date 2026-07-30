package orderbook

import (
	"runtime"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
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

// --- the level-total invariant ---

// A price level's TotalQty must always equal the sum of the RemainingQty of the
// orders resting at it. That is what makes Snapshot — the read model for signals, the
// WASM demo, and any L2 feed — mean anything.
//
// Nothing checked it, and it was false. The matching engine fills an order, which
// drops RemainingQty to zero, and only then removes it; unlink subtracted
// RemainingQty, so it subtracted nothing and left the maker's whole original size in
// the level. Depth was over-reported after every full fill, permanently, and no test
// noticed because every existing assertion checked either the orders or the level,
// never that the two agreed.
//
// The fix was to make the total the book's own property (node.contributed) instead of
// a discipline every removal site had to remember. These tests are the invariant.

// levelSums returns the sum of resting RemainingQty per (side, price).
func levelSums(ob *OrderBook) (bids, asks map[int64]int64) {
	bids, asks = map[int64]int64{}, map[int64]int64{}
	for _, o := range ob.Orders() {
		m := bids
		if o.Side == types.SideSell {
			m = asks
		}
		m[o.Price] += o.RemainingQty
	}
	return bids, asks
}

// assertLevelsMatchOrders fails if any level disagrees with the orders resting at it.
func assertLevelsMatchOrders(t *testing.T, ob *OrderBook, when string) {
	t.Helper()
	wantBids, wantAsks := levelSums(ob)
	snap := ob.Snapshot(1 << 20)
	for _, l := range snap.Bids {
		if l.Quantity != wantBids[l.Price] {
			t.Fatalf("%s: bid %d level says %d, orders sum to %d", when, l.Price, l.Quantity, wantBids[l.Price])
		}
		delete(wantBids, l.Price)
	}
	for _, l := range snap.Asks {
		if l.Quantity != wantAsks[l.Price] {
			t.Fatalf("%s: ask %d level says %d, orders sum to %d", when, l.Price, l.Quantity, wantAsks[l.Price])
		}
		delete(wantAsks, l.Price)
	}
	// Anything left is a level with resting orders that the snapshot did not report.
	for price, q := range wantBids {
		t.Fatalf("%s: bid %d holds %d lots of orders but no level", when, price, q)
	}
	for price, q := range wantAsks {
		t.Fatalf("%s: ask %d holds %d lots of orders but no level", when, price, q)
	}
}

// TestLevelTotalSurvivesAFullFill is the exact case that was broken: an order removed
// after being filled, so its RemainingQty is zero by the time the book sees it go.
func TestLevelTotalSurvivesAFullFill(t *testing.T) {
	ob := New(Config{Symbol: "X", MaxOrders: 100})
	var made []*types.Order
	for i := 0; i < 3; i++ {
		o, err := types.NewOrder("mm", "X", types.SideSell, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		if err := ob.Add(o); err != nil {
			t.Fatalf("Add: %v", err)
		}
		made = append(made, o)
	}
	if _, qty, _ := ob.BestAsk(); qty != 15 {
		t.Fatalf("level starts at %d, want 15", qty)
	}

	// Exactly what the engine does for a FULLY filled maker: fill it, then remove
	// it, with no UpdateOrderQuantity call — executeTrade applies the fill and the
	// match loop only calls UpdateOrderQuantity on the partial case. Two of the three
	// go, so the level survives and a stale total stays visible; when the last order
	// leaves, the level is deleted and any error in it disappears with it, which is
	// why this needs more than one order to catch anything.
	for _, o := range made[:2] {
		_ = o.Fill(5)
		if _, err := ob.Remove(o.ID); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	}
	// And a partial fill on the third.
	ob.UpdateOrderQuantity(made[2].ID, 2)
	_ = made[2].Fill(2)

	if _, qty, _ := ob.BestAsk(); qty != 3 {
		t.Errorf("level says %d after 12 of 15 lots traded, want 3", qty)
	}
	assertLevelsMatchOrders(t, ob, "after two full fills and a partial")
}

// TestLevelTotalSurvivesRemovalOfAFilledOrderWithoutAnUpdate is the harsher case: a
// caller that removes a filled order WITHOUT telling the book about the fill first.
// The old code left the whole quantity behind; the invariant must now hold even so,
// because relying on the caller is what produced the bug.
func TestLevelTotalSurvivesRemovalOfAFilledOrderWithoutAnUpdate(t *testing.T) {
	ob := New(Config{Symbol: "X", MaxOrders: 10})
	o, err := types.NewOrder("mm", "X", types.SideBuy, types.OrderTypeLimit, 100, 8, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if err := ob.Add(o); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Fill it behind the book's back, then remove it.
	_ = o.Fill(8)
	if _, err := ob.Remove(o.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, qty, ok := ob.BestBid(); ok || qty != 0 {
		t.Errorf("level still reports %d lots (present=%v) after its only order left", qty, ok)
	}
	assertLevelsMatchOrders(t, ob, "after removing a filled order with no prior update")
}

// TestLevelTotalHoldsUnderChurn — the invariant over a mixed stream, since the bug
// only showed up in a specific interleaving and a single case proves little.
func TestLevelTotalHoldsUnderChurn(t *testing.T) {
	ob := New(Config{Symbol: "X", MaxOrders: 5000})
	var resting []*types.Order
	for i := 0; i < 2000; i++ {
		switch i % 5 {
		case 0, 1: // add
			side := types.SideBuy
			price := int64(90 + i%10)
			if i%2 == 0 {
				side = types.SideSell
				price = int64(100 + i%10)
			}
			o, err := types.NewOrder("u", "X", side, types.OrderTypeLimit, price, 1+int64(i%9), types.TIFGoodTillCancel)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			if err := ob.Add(o); err != nil {
				continue
			}
			resting = append(resting, o)
		case 2: // partial fill
			if len(resting) == 0 {
				continue
			}
			o := resting[i%len(resting)]
			if o.RemainingQty > 1 {
				ob.UpdateOrderQuantity(o.ID, 1)
				_ = o.Fill(1)
			}
		case 3: // full fill then remove — the engine's path, and the broken one
			if len(resting) == 0 {
				continue
			}
			j := i % len(resting)
			o := resting[j]
			_ = o.Fill(o.RemainingQty) // no UpdateOrderQuantity, as the engine does
			_, _ = ob.Remove(o.ID)
			resting = append(resting[:j], resting[j+1:]...)
		case 4: // plain cancel of a live order
			if len(resting) == 0 {
				continue
			}
			j := i % len(resting)
			_, _ = ob.Remove(resting[j].ID)
			resting = append(resting[:j], resting[j+1:]...)
		}
		assertLevelsMatchOrders(t, ob, "churn step")
	}
}
