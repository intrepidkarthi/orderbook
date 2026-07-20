package orderbook

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func limit(t *testing.T, user string, side types.Side, price, qty string) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, dec(price), dec(qty), types.TIFGoodTillCancel)
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
	mustAdd(t, ob, limit(t, "a", types.SideBuy, "99", "1"))
	mustAdd(t, ob, limit(t, "b", types.SideBuy, "100", "2")) // better bid
	mustAdd(t, ob, limit(t, "c", types.SideSell, "102", "1"))
	mustAdd(t, ob, limit(t, "d", types.SideSell, "101", "3")) // better ask

	bid, bidQty, ok := ob.BestBid()
	if !ok || !bid.Equal(dec("100")) || !bidQty.Equal(dec("2")) {
		t.Errorf("best bid = %s (qty %s), want 100 (qty 2)", bid, bidQty)
	}
	ask, askQty, ok := ob.BestAsk()
	if !ok || !ask.Equal(dec("101")) || !askQty.Equal(dec("3")) {
		t.Errorf("best ask = %s (qty %s), want 101 (qty 3)", ask, askQty)
	}
	if sp, _ := ob.Spread(); !sp.Equal(dec("1")) {
		t.Errorf("spread = %s, want 1", sp)
	}
	if mid, _ := ob.MidPrice(); !mid.Equal(dec("100.5")) {
		t.Errorf("mid = %s, want 100.5", mid)
	}
}

func TestPriceOrdering(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	// Insert deliberately out of order.
	for _, p := range []string{"100", "103", "101", "104", "102"} {
		mustAdd(t, ob, limit(t, "u", types.SideBuy, p, "1"))
	}
	for _, p := range []string{"110", "107", "109", "106", "108"} {
		mustAdd(t, ob, limit(t, "u", types.SideSell, p, "1"))
	}

	// Bids descending (best first).
	wantBids := []string{"104", "103", "102", "101", "100"}
	for i, l := range ob.GetBidLevels(10) {
		if !l.Price.Equal(dec(wantBids[i])) {
			t.Errorf("bid level %d = %s, want %s", i, l.Price, wantBids[i])
		}
	}
	// Asks ascending (best first).
	wantAsks := []string{"106", "107", "108", "109", "110"}
	for i, l := range ob.GetAskLevels(10) {
		if !l.Price.Equal(dec(wantAsks[i])) {
			t.Errorf("ask level %d = %s, want %s", i, l.Price, wantAsks[i])
		}
	}
}

func TestFIFOWithinLevel(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	first := limit(t, "a", types.SideBuy, "100", "1")
	second := limit(t, "b", types.SideBuy, "100", "2")
	third := limit(t, "c", types.SideBuy, "100", "3")
	mustAdd(t, ob, first)
	mustAdd(t, ob, second)
	mustAdd(t, ob, third)

	orders := ob.GetOrdersAtPrice(types.SideBuy, dec("100"))
	if len(orders) != 3 {
		t.Fatalf("got %d orders, want 3", len(orders))
	}
	if orders[0].ID != first.ID || orders[1].ID != second.ID || orders[2].ID != third.ID {
		t.Error("orders not in FIFO (insertion) order")
	}
	// Aggregate quantity at the level.
	if _, qty, _ := ob.BestBid(); !qty.Equal(dec("6")) {
		t.Errorf("level qty = %s, want 6", qty)
	}
	// Peek returns the oldest.
	if ob.PeekBestBidOrder().ID != first.ID {
		t.Error("peek should return oldest order at best bid")
	}
}

func TestRemove_CleansLevel(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	best := limit(t, "a", types.SideSell, "101", "1")
	next := limit(t, "b", types.SideSell, "102", "1")
	mustAdd(t, ob, best)
	mustAdd(t, ob, next)

	if _, err := ob.Remove(best.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Best ask should now be the next level.
	if ask, _, ok := ob.BestAsk(); !ok || !ask.Equal(dec("102")) {
		t.Errorf("best ask after remove = %s, want 102", ask)
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
	o := limit(t, "a", types.SideBuy, "100", "10")
	mustAdd(t, ob, o)
	ob.UpdateOrderQuantity(o.ID, dec("4"))
	if _, qty, _ := ob.BestBid(); !qty.Equal(dec("6")) {
		t.Errorf("level qty after partial = %s, want 6", qty)
	}
}

func TestRestoreOrderQuantity(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	o := limit(t, "a", types.SideSell, "101", "10")
	mustAdd(t, ob, o)

	// Simulate a partial fill in place, then undo it.
	ob.UpdateOrderQuantity(o.ID, dec("4"))
	if _, qty, _ := ob.BestAsk(); !qty.Equal(dec("6")) {
		t.Fatalf("after partial: level qty = %s, want 6", qty)
	}
	ob.RestoreOrderQuantity(o.ID, dec("4"))
	if _, qty, _ := ob.BestAsk(); !qty.Equal(dec("10")) {
		t.Errorf("after restore: level qty = %s, want 10", qty)
	}
	// Restoring an unknown id is a no-op (no panic).
	ob.RestoreOrderQuantity("missing", dec("1"))
}

func TestDuplicateAddIgnored(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	o := limit(t, "a", types.SideBuy, "100", "1")
	mustAdd(t, ob, o)
	mustAdd(t, ob, o) // ignored, no error
	if ob.Count() != 1 {
		t.Errorf("count = %d, want 1 (duplicate ignored)", ob.Count())
	}
}

func TestOrderBookFull(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD", MaxOrders: 2})
	mustAdd(t, ob, limit(t, "a", types.SideBuy, "100", "1"))
	mustAdd(t, ob, limit(t, "b", types.SideBuy, "99", "1"))
	err := ob.Add(limit(t, "c", types.SideBuy, "98", "1"))
	if !errors.Is(err, types.ErrOrderBookFull) {
		t.Errorf("err = %v, want ErrOrderBookFull", err)
	}
}

func TestSnapshot(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	mustAdd(t, ob, limit(t, "a", types.SideBuy, "100", "2"))
	mustAdd(t, ob, limit(t, "b", types.SideBuy, "99", "1"))
	mustAdd(t, ob, limit(t, "c", types.SideSell, "101", "3"))
	ob.SetLastTradePrice(dec("100.5"))

	snap := ob.Snapshot(5)
	if snap.Symbol != "BTC-USD" {
		t.Errorf("symbol = %q", snap.Symbol)
	}
	if len(snap.Bids) != 2 || len(snap.Asks) != 1 {
		t.Fatalf("bids=%d asks=%d, want 2/1", len(snap.Bids), len(snap.Asks))
	}
	if !snap.Bids[0].Price.Equal(dec("100")) || !snap.Bids[0].Quantity.Equal(dec("2")) {
		t.Errorf("top bid = %s x %s, want 100 x 2", snap.Bids[0].Price, snap.Bids[0].Quantity)
	}
	if !snap.LastTradePrice.Equal(dec("100.5")) {
		t.Errorf("last trade = %s, want 100.5", snap.LastTradePrice)
	}
}

// TestLadderInvariant checks the sorted-ladder invariant holds after many
// out-of-order insertions (exercises binary-search insertion).
func TestLadderInvariant(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	// Pseudo-random-ish but deterministic price sequence.
	prices := []int{50, 12, 87, 33, 91, 5, 66, 42, 78, 21, 99, 3, 55, 60, 71}
	for _, p := range prices {
		mustAdd(t, ob, limit(t, "u", types.SideBuy, itoa(p), "1"))
		mustAdd(t, ob, limit(t, "u", types.SideSell, itoa(p+1000), "1"))
	}
	bids := ob.GetBidLevels(100)
	for i := 1; i < len(bids); i++ {
		if !bids[i-1].Price.GreaterThan(bids[i].Price) {
			t.Fatalf("bids not strictly descending at %d: %s !> %s", i, bids[i-1].Price, bids[i].Price)
		}
	}
	asks := ob.GetAskLevels(100)
	for i := 1; i < len(asks); i++ {
		if !asks[i-1].Price.LessThan(asks[i].Price) {
			t.Fatalf("asks not strictly ascending at %d: %s !< %s", i, asks[i-1].Price, asks[i].Price)
		}
	}
}

func itoa(n int) string { return decimal.NewFromInt(int64(n)).String() }
