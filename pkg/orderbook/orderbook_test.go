package orderbook

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func limit(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

func mustAdd(t *testing.T, ob *OrderBook, o *types.Order) {
	t.Helper()
	if err := ob.Add(o); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func TestEmptyBook(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	if _, _, ok := ob.BestBid(); ok {
		t.Error("empty book should have no best bid")
	}
	if _, _, ok := ob.BestAsk(); ok {
		t.Error("empty book should have no best ask")
	}
	if _, ok := ob.Spread(); ok {
		t.Error("empty book should have no spread")
	}
	if _, ok := ob.MidPrice(); ok {
		t.Error("empty book should have no mid")
	}
	if ob.Count() != 0 {
		t.Error("empty book count should be 0")
	}
}

func TestBestBidAsk_SpreadMid(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	mustAdd(t, ob, limit(t, "a", types.SideBuy, 99, 1))
	mustAdd(t, ob, limit(t, "b", types.SideBuy, 100, 2)) // better bid
	mustAdd(t, ob, limit(t, "c", types.SideSell, 102, 1))
	mustAdd(t, ob, limit(t, "d", types.SideSell, 101, 3)) // better ask

	bid, bidQty, ok := ob.BestBid()
	if !ok || bid != 100 || bidQty != 2 {
		t.Errorf("best bid = %d (qty %d), want 100 (qty 2)", bid, bidQty)
	}
	ask, askQty, ok := ob.BestAsk()
	if !ok || ask != 101 || askQty != 3 {
		t.Errorf("best ask = %d (qty %d), want 101 (qty 3)", ask, askQty)
	}
	if sp, _ := ob.Spread(); sp != 1 {
		t.Errorf("spread = %d, want 1", sp)
	}
	// Mid is floored integer ticks: (100+101)/2 = 100.
	if mid, _ := ob.MidPrice(); mid != 100 {
		t.Errorf("mid = %d, want 100 (floored)", mid)
	}
}

func TestPriceOrdering(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	// Insert deliberately out of order.
	for _, p := range []int64{100, 103, 101, 104, 102} {
		mustAdd(t, ob, limit(t, "u", types.SideBuy, p, 1))
	}
	for _, p := range []int64{110, 107, 109, 106, 108} {
		mustAdd(t, ob, limit(t, "u", types.SideSell, p, 1))
	}

	// Bids descending (best first).
	wantBids := []int64{104, 103, 102, 101, 100}
	for i, l := range ob.GetBidLevels(10) {
		if l.Price != wantBids[i] {
			t.Errorf("bid level %d = %d, want %d", i, l.Price, wantBids[i])
		}
	}
	// Asks ascending (best first).
	wantAsks := []int64{106, 107, 108, 109, 110}
	for i, l := range ob.GetAskLevels(10) {
		if l.Price != wantAsks[i] {
			t.Errorf("ask level %d = %d, want %d", i, l.Price, wantAsks[i])
		}
	}
}

func TestFIFOWithinLevel(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	first := limit(t, "a", types.SideBuy, 100, 1)
	second := limit(t, "b", types.SideBuy, 100, 2)
	third := limit(t, "c", types.SideBuy, 100, 3)
	mustAdd(t, ob, first)
	mustAdd(t, ob, second)
	mustAdd(t, ob, third)

	orders := ob.GetOrdersAtPrice(types.SideBuy, 100)
	if len(orders) != 3 {
		t.Fatalf("got %d orders, want 3", len(orders))
	}
	if orders[0].ID != first.ID || orders[1].ID != second.ID || orders[2].ID != third.ID {
		t.Error("orders not in FIFO (insertion) order")
	}
	// Aggregate quantity at the level.
	if _, qty, _ := ob.BestBid(); qty != 6 {
		t.Errorf("level qty = %d, want 6", qty)
	}
	// Peek returns the oldest.
	if ob.PeekBestBidOrder().ID != first.ID {
		t.Error("peek should return oldest order at best bid")
	}
}

func TestRemove_CleansLevel(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	best := limit(t, "a", types.SideSell, 101, 1)
	next := limit(t, "b", types.SideSell, 102, 1)
	mustAdd(t, ob, best)
	mustAdd(t, ob, next)

	if _, err := ob.Remove(best.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Best ask should now be the next level.
	if ask, _, ok := ob.BestAsk(); !ok || ask != 102 {
		t.Errorf("best ask after remove = %d, want 102", ask)
	}
	if ob.Count() != 1 {
		t.Errorf("count = %d, want 1", ob.Count())
	}
	// Removing again is not found.
	if _, err := ob.Remove(best.ID); !errors.Is(err, types.ErrOrderNotFound) {
		t.Errorf("remove missing err = %v, want ErrOrderNotFound", err)
	}
}

func TestUpdateOrderQuantity(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	o := limit(t, "a", types.SideBuy, 100, 10)
	mustAdd(t, ob, o)
	ob.UpdateOrderQuantity(o.ID, 4)
	if _, qty, _ := ob.BestBid(); qty != 6 {
		t.Errorf("level qty after partial = %d, want 6", qty)
	}
}

func TestRestoreOrderQuantity(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	o := limit(t, "a", types.SideSell, 101, 10)
	mustAdd(t, ob, o)

	// Simulate a partial fill in place, then undo it.
	ob.UpdateOrderQuantity(o.ID, 4)
	if _, qty, _ := ob.BestAsk(); qty != 6 {
		t.Fatalf("after partial: level qty = %d, want 6", qty)
	}
	ob.RestoreOrderQuantity(o.ID, 4)
	if _, qty, _ := ob.BestAsk(); qty != 10 {
		t.Errorf("after restore: level qty = %d, want 10", qty)
	}
	// Restoring an unknown id is a no-op (no panic).
	ob.RestoreOrderQuantity(999999, 1)
}

func TestDuplicateAddIgnored(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	o := limit(t, "a", types.SideBuy, 100, 1)
	mustAdd(t, ob, o)
	mustAdd(t, ob, o) // ignored, no error
	if ob.Count() != 1 {
		t.Errorf("count = %d, want 1 (duplicate ignored)", ob.Count())
	}
}

func TestOrderBookFull(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD", MaxOrders: 2})
	mustAdd(t, ob, limit(t, "a", types.SideBuy, 100, 1))
	mustAdd(t, ob, limit(t, "b", types.SideBuy, 99, 1))
	err := ob.Add(limit(t, "c", types.SideBuy, 98, 1))
	if !errors.Is(err, types.ErrOrderBookFull) {
		t.Errorf("err = %v, want ErrOrderBookFull", err)
	}
}

func TestSnapshot(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	mustAdd(t, ob, limit(t, "a", types.SideBuy, 100, 2))
	mustAdd(t, ob, limit(t, "b", types.SideBuy, 99, 1))
	mustAdd(t, ob, limit(t, "c", types.SideSell, 101, 3))
	ob.SetLastTradePrice(100)

	snap := ob.Snapshot(5)
	if snap.Symbol != "BTC-USD" {
		t.Errorf("symbol = %q", snap.Symbol)
	}
	if len(snap.Bids) != 2 || len(snap.Asks) != 1 {
		t.Fatalf("bids=%d asks=%d, want 2/1", len(snap.Bids), len(snap.Asks))
	}
	if snap.Bids[0].Price != 100 || snap.Bids[0].Quantity != 2 {
		t.Errorf("top bid = %d x %d, want 100 x 2", snap.Bids[0].Price, snap.Bids[0].Quantity)
	}
	if snap.LastTradePrice != 100 {
		t.Errorf("last trade = %d, want 100", snap.LastTradePrice)
	}
}

// TestLadderInvariant checks the sorted-ladder invariant holds after many
// out-of-order insertions (exercises binary-search insertion).
func TestLadderInvariant(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	// Pseudo-random-ish but deterministic price sequence.
	prices := []int64{50, 12, 87, 33, 91, 5, 66, 42, 78, 21, 99, 3, 55, 60, 71}
	for _, p := range prices {
		mustAdd(t, ob, limit(t, "u", types.SideBuy, p, 1))
		mustAdd(t, ob, limit(t, "u", types.SideSell, p+1000, 1))
	}
	bids := ob.GetBidLevels(100)
	for i := 1; i < len(bids); i++ {
		if bids[i-1].Price <= bids[i].Price {
			t.Fatalf("bids not strictly descending at %d: %d !> %d", i, bids[i-1].Price, bids[i].Price)
		}
	}
	asks := ob.GetAskLevels(100)
	for i := 1; i < len(asks); i++ {
		if asks[i-1].Price >= asks[i].Price {
			t.Fatalf("asks not strictly ascending at %d: %d !< %d", i, asks[i-1].Price, asks[i].Price)
		}
	}
}
