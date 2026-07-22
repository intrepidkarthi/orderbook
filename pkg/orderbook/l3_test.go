package orderbook

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func TestSnapshotL3_OrderByOrder(t *testing.T) {
	ob := New(Config{Symbol: "BTC-USD"})
	// Two orders at the best bid (FIFO), one deeper.
	a := limit(t, "alice", types.SideBuy, 100, 2)
	b := limit(t, "bob", types.SideBuy, 100, 3)
	c := limit(t, "carol", types.SideBuy, 99, 5)
	mustAdd(t, ob, a)
	mustAdd(t, ob, b)
	mustAdd(t, ob, c)
	mustAdd(t, ob, limit(t, "dave", types.SideSell, 101, 4))

	snap := ob.SnapshotL3(10)
	if len(snap.Bids) != 3 {
		t.Fatalf("L3 bids = %d, want 3 individual orders", len(snap.Bids))
	}
	// Best price first, FIFO within the level: alice, bob (both @100), then carol.
	if snap.Bids[0].OrderID != a.ID || snap.Bids[0].UserID != "alice" {
		t.Errorf("first bid = %s/%d, want alice's order", snap.Bids[0].UserID, snap.Bids[0].OrderID)
	}
	if snap.Bids[1].OrderID != b.ID {
		t.Error("second bid should be bob's (time priority)")
	}
	if snap.Bids[2].OrderID != c.ID || snap.Bids[2].Price != 99 {
		t.Error("third bid should be carol's at 99")
	}
	if len(snap.Asks) != 1 || snap.Asks[0].UserID != "dave" {
		t.Errorf("asks = %+v, want one order from dave", snap.Asks)
	}
	// Quantity reflects remaining size.
	if snap.Bids[1].Quantity != 3 {
		t.Errorf("bob's L3 qty = %d, want 3", snap.Bids[1].Quantity)
	}
}
