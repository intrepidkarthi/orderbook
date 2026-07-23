package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

// TestMarkPriceBand: the band evaluates against the injected mark price, not the
// raw last trade — so a thin-book wick doesn't move the collar.
func TestMarkPriceBand(t *testing.T) {
	e := NewEngine(Config{Symbol: "X", PriceBand: decimal.RequireFromString("0.1")}) // ±10%
	// A thin wick prints last=200...
	e.Process(lim(t, "a", types.SideBuy, 200, 1))
	e.Process(lim(t, "b", types.SideSell, 200, 1)) // trades → last=200
	// ...but the risk layer marks fair value at 100.
	if err := e.SetMarkPrice(100); err != nil {
		t.Fatalf("SetMarkPrice(100): %v", err)
	}

	// 105 is within ±10% of the *mark* (100) even though last=200.
	if r := e.Process(lim(t, "c", types.SideBuy, 105, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("105 within band of mark 100 should be accepted, got %v", r.RejectionReason)
	}
	// 130 is outside ±10% of the mark.
	if r := e.Process(lim(t, "d", types.SideBuy, 130, 1)); r.Status != types.OrderStatusRejected {
		t.Errorf("130 outside band of mark 100 should be rejected, got %q", r.Status)
	}
	// Clearing the mark falls back to last trade (200), so 130 is now fine.
	if err := e.SetMarkPrice(0); err != nil {
		t.Fatalf("SetMarkPrice(0): %v", err)
	}
	if r := e.Process(lim(t, "e", types.SideBuy, 190, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("190 within band of last 200 should be accepted after clearing mark, got %v", r.RejectionReason)
	}
}

// TestForceTrade: the risk layer injects a forced (ADL/liquidation) trade between
// two orders at a bankruptcy price, bypassing the book and matching.
func TestForceTrade(t *testing.T) {
	e := newEngine()
	// The liquidatee's position-closing order and the deleveraged counterparty.
	liquidatee, _ := types.NewOrder("under", "BTC-USD", types.SideSell, types.OrderTypeMarket, 0, 5, types.TIFImmediateOrCancel)
	winner, _ := types.NewOrder("winner", "BTC-USD", types.SideBuy, types.OrderTypeLimit, 90, 5, types.TIFGoodTillCancel)

	tr, err := e.ForceTrade(liquidatee, winner, 90, 3) // force 3 @ bankruptcy price 90
	if err != nil {
		t.Fatalf("ForceTrade: %v", err)
	}
	if tr.Price != 90 || tr.Quantity != 3 {
		t.Errorf("forced trade = %d @ %d, want 3 @ 90", tr.Quantity, tr.Price)
	}
	if liquidatee.RemainingQty != 2 || winner.RemainingQty != 2 {
		t.Errorf("both sides should fill 3: liquidatee rem=%d winner rem=%d", liquidatee.RemainingQty, winner.RemainingQty)
	}
	if tr.SellerUserID != "under" || tr.BuyerUserID != "winner" {
		t.Errorf("forced trade parties wrong: buyer=%s seller=%s", tr.BuyerUserID, tr.SellerUserID)
	}
	// Over-fill is rejected.
	if _, err := e.ForceTrade(liquidatee, winner, 90, 99); err == nil {
		t.Error("ForceTrade beyond remaining quantity should error")
	}
	// It didn't touch the book.
	if e.OrderCount() != 0 {
		t.Errorf("ForceTrade must not rest anything on the book, count=%d", e.OrderCount())
	}
}
