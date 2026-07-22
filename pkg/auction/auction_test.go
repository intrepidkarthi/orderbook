package auction

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func lv(price, qty int64) Level { return Level{Price: price, Qty: qty} }

func TestUncross_MaximisesVolume(t *testing.T) {
	// p=100 clears 20 (max); 99 and 101 clear only 10.
	bids := []Level{lv(101, 10), lv(100, 10)}
	asks := []Level{lv(99, 10), lv(100, 10)}
	r := Uncross(bids, asks)
	if !r.HasClearing {
		t.Fatal("expected a clearing price")
	}
	if r.ClearingPrice != 100 {
		t.Errorf("clearing price = %d, want 100", r.ClearingPrice)
	}
	if r.Volume != 20 {
		t.Errorf("volume = %d, want 20", r.Volume)
	}
}

func TestUncross_NoCross(t *testing.T) {
	r := Uncross([]Level{lv(99, 10)}, []Level{lv(101, 10)})
	if r.HasClearing {
		t.Errorf("disjoint book should not clear, got %+v", r)
	}
}

func TestUncross_TieBreakLowestPrice(t *testing.T) {
	// Both 99 and 100 clear 10 with zero imbalance ⇒ prefer the lower price.
	r := Uncross([]Level{lv(100, 10)}, []Level{lv(99, 10)})
	if !r.HasClearing || r.Volume != 10 {
		t.Fatalf("expected clearing of 10, got %+v", r)
	}
	if r.ClearingPrice != 99 {
		t.Errorf("clearing price = %d, want 99 (lowest on tie)", r.ClearingPrice)
	}
}

func TestFromSnapshot(t *testing.T) {
	// Build a crossed book snapshot and uncross it.
	ob := orderbook.New(orderbook.Config{Symbol: "X"})
	add := func(side types.Side, price, qty int64) {
		o, _ := types.NewOrder("u", "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		_ = ob.Add(o)
	}
	// Note: these rest without matching (Add doesn't match); the auction crosses them.
	add(types.SideBuy, 101, 10)
	add(types.SideBuy, 100, 10)
	add(types.SideSell, 99, 10)
	add(types.SideSell, 100, 10)

	r := FromSnapshot(ob.Snapshot(10))
	if !r.HasClearing || r.ClearingPrice != 100 || r.Volume != 20 {
		t.Errorf("snapshot uncross = %+v, want clearing 100 volume 20", r)
	}
}
