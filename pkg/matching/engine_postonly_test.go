package matching

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func TestPostOnly_RestsWhenPassive(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "mm", types.SideSell, "101", "5")) // best ask 101

	// A post-only buy at 100 does not cross → rests as a maker.
	res := e.Process(lim(t, "pm", types.SideBuy, "100", "3").AsPostOnly())
	if res.Status == types.OrderStatusRejected {
		t.Fatalf("passive post-only should rest, got %q (%v)", res.Status, res.RejectionReason)
	}
	if bid, qty, ok := e.BestBid(); !ok || !bid.Equal(dec("100")) || !qty.Equal(dec("3")) {
		t.Errorf("post-only should rest at 100 x 3, got %s x %s", bid, qty)
	}
}

func TestPostOnly_RejectedWhenCrossing(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "mm", types.SideSell, "101", "5")) // best ask 101

	// A post-only buy at 101 would take → rejected, nothing rests, no trade.
	res := e.Process(lim(t, "pm", types.SideBuy, "101", "3").AsPostOnly())
	if res.Status != types.OrderStatusRejected {
		t.Fatalf("crossing post-only should be rejected, got %q", res.Status)
	}
	if !errors.Is(res.RejectionReason, types.ErrPostOnlyWouldCross) {
		t.Errorf("reason = %v, want ErrPostOnlyWouldCross", res.RejectionReason)
	}
	if len(res.Trades) != 0 {
		t.Errorf("post-only reject should produce no trades, got %d", len(res.Trades))
	}
	// The resting ask is untouched; no bid was added.
	if _, _, ok := e.BestBid(); ok {
		t.Error("rejected post-only must not rest on the book")
	}
	if _, qty, _ := e.BestAsk(); !qty.Equal(dec("5")) {
		t.Errorf("resting ask should be untouched (5), got %s", qty)
	}
}

func TestPostOnly_LockingPriceCrosses(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "mm", types.SideBuy, "100", "5")) // best bid 100

	// A post-only sell at 100 locks the book (sell price <= best bid) → rejected.
	res := e.Process(lim(t, "pm", types.SideSell, "100", "3").AsPostOnly())
	if res.Status != types.OrderStatusRejected {
		t.Errorf("post-only sell at the bid should be rejected, got %q", res.Status)
	}
}
