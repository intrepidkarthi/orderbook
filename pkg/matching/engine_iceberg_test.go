package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func iceberg(t *testing.T, user string, side types.Side, price, total, display int64) *types.IcebergOrder {
	t.Helper()
	ib, err := types.NewIcebergOrder(lim(t, user, side, price, total), display)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	return ib
}

// TestIceberg_JitterConfigConserves: with Config.IcebergPeakJitter set, the reload
// sizes vary but the full hidden quantity still trades — jitter changes the peak
// size, never the total.
func TestIceberg_JitterConfigConserves(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.IcebergPeakJitter = decimal.RequireFromString("0.3") // ±30%
	e := NewEngine(cfg)
	e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 99, 100, 10))

	var traded int64
	// Drain the whole iceberg with small sell orders; count everything that fills.
	for range 200 {
		r := e.Process(marketOrder(t, "taker", types.SideSell, 1))
		for _, tr := range r.Trades {
			traded += tr.Quantity
		}
		if _, _, ok := e.BestBid(); !ok {
			break
		}
	}
	if traded != 100 {
		t.Errorf("jittered iceberg should still trade its full size: traded %d want 100", traded)
	}
}

func TestIceberg_ShowsOnlyDisplaySlice(t *testing.T) {
	e := newEngine()
	e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 99, 10, 3))

	// The book shows only the display slice (3), not the full 10.
	if _, qty, ok := e.BestBid(); !ok || qty != 3 {
		t.Errorf("visible best bid qty = %d, want 3 (rest hidden)", qty)
	}
}

func TestIceberg_RefillsAsConsumed(t *testing.T) {
	e := newEngine()
	e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 99, 10, 3))

	// Consume three display slices (9 total); each refill re-shows 3, 3, then 1.
	for range 3 {
		e.Process(marketOrder(t, "taker", types.SideSell, 3))
	}
	// After 9 consumed, only the final hidden unit (1) remains visible.
	if _, qty, ok := e.BestBid(); !ok || qty != 1 {
		t.Errorf("visible qty after 9 consumed = %d, want 1", qty)
	}
	// Consume the last unit; the iceberg is now fully worked off.
	e.Process(marketOrder(t, "taker", types.SideSell, 1))
	if _, _, ok := e.BestBid(); ok {
		t.Error("book should be empty after the iceberg is fully consumed")
	}
}

func TestIceberg_ImmediateCrossRefills(t *testing.T) {
	e := newEngine()
	// Resting asks: 99×3, 100×3, 101×3 (9 available).
	e.Process(lim(t, "a99", types.SideSell, 99, 3))
	e.Process(lim(t, "a100", types.SideSell, 100, 3))
	e.Process(lim(t, "a101", types.SideSell, 101, 3))

	// An aggressive iceberg buy for 8 (display 3) sweeps 99, 100, and 2 of 101.
	res := e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 101, 8, 3))
	if res.Status != types.OrderStatusFilled {
		t.Fatalf("status = %q, want FILLED", res.Status)
	}
	var total int64
	for _, tr := range res.Trades {
		total += tr.Quantity
	}
	if total != 8 {
		t.Errorf("iceberg bought %d, want 8", total)
	}
	// 1 unit of the 101 ask remains.
	if ask, qty, ok := e.BestAsk(); !ok || ask != 101 || qty != 1 {
		t.Errorf("remaining ask = %d x %d, want 101 x 1", ask, qty)
	}
}

// TestIceberg_RefillGoesToTheBackOfTheQueue is the queue-fairness property of the
// iceberg: a refilled slice is NEW liquidity and joins its price level behind
// everything already resting there. engine.go says so in a comment at the refill
// site ("the refilled slice re-enters at the back of its price level"), and until
// this test that sentence was the only thing asserting it.
//
// That gap mattered because docs/REFERENCE-MATCHER.md §3 credited the differential
// harness's ranked L3 comparison with catching "an iceberg refill that lands in the
// wrong place". It cannot: icebergs are tier 2 (see the commandTier table), so the
// generator never emits one, and the ranked comparison never sees a refill. The
// claim was checked by mutation — re-adding the refilled slice at the HEAD of its
// level, using only the book's existing exported API — and `go test ./...` passed
// across all 23 packages with the iceberg silently jumping the queue. The spec
// sentence has been corrected and this test is what actually holds the property.
//
// It is a fairness rule with money attached. A hidden order that kept its place
// through every refill would take priority it never queued for, which is the
// advantage displaying liquidity is supposed to buy.
func TestIceberg_RefillGoesToTheBackOfTheQueue(t *testing.T) {
	e := newEngine()

	// A 4-lot iceberg showing 2, then a plain 3-lot order joining behind it.
	e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 100, 4, 2))
	e.Process(lim(t, "joe", types.SideBuy, 100, 3))

	queue := func() []string {
		var users []string
		for _, o := range e.Book().GetOrdersAtPrice(types.SideBuy, 100) {
			users = append(users, o.UserID)
		}
		return users
	}
	if got, want := queue(), []string{"whale", "joe"}; !slicesEqual(got, want) {
		t.Fatalf("queue at 100 before the refill = %v, want %v", got, want)
	}

	// Consume the whole visible slice, which forces a refill of the hidden 2.
	e.Process(marketOrder(t, "taker", types.SideSell, 2))

	// The refilled slice is behind joe, who was already waiting. If this reads
	// [whale joe], the refill kept a queue position it did not earn.
	if got, want := queue(), []string{"joe", "whale"}; !slicesEqual(got, want) {
		t.Fatalf("queue at 100 after the refill = %v, want %v — the refilled slice must join at the BACK "+
			"of its price level, not keep the consumed slice's priority", got, want)
	}

	// And the refill is real liquidity, not just a repositioned marker.
	if _, qty, ok := e.BestBid(); !ok || qty != 5 {
		t.Fatalf("visible depth at the touch = %d, want 5 (joe's 3 plus the refilled 2)", qty)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
