package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func trailingStop(t *testing.T, user string, side types.Side, qty, trail string) *types.TrailingStop {
	t.Helper()
	ts, err := types.NewTrailingStop(marketOrder(t, user, side, qty), dec(trail))
	if err != nil {
		t.Fatalf("NewTrailingStop: %v", err)
	}
	return ts
}

func TestTrailingStop_RestsThenTriggers(t *testing.T) {
	e := newEngine()
	seedWithLastPrice(t, e) // last=100, bids 100/96/95

	// Sell trailing stop, trail 5 → stop starts at 95, rests.
	ts := trailingStop(t, "trader", types.SideSell, "3", "5")
	res := e.ProcessTrailingStop(ts)
	if res.Status != types.OrderStatusPendingTrigger {
		t.Fatalf("status = %q, want PENDING_TRIGGER", res.Status)
	}
	if e.TrailingStopCount() != 1 {
		t.Fatalf("trailing stops = %d, want 1", e.TrailingStopCount())
	}

	// Drive price down to 95 → the trail (95) is reached → fires.
	e.Process(marketOrder(t, "seller", types.SideSell, "11"))
	if e.TrailingStopCount() != 0 {
		t.Errorf("trailing stop should have fired, count = %d", e.TrailingStopCount())
	}
	if ts.Order.FilledQty.IsZero() {
		t.Error("fired trailing stop should have filled")
	}
}

func TestTrailingStop_Cancel(t *testing.T) {
	e := newEngine()
	seedWithLastPrice(t, e)
	ts := trailingStop(t, "trader", types.SideSell, "3", "5")
	e.ProcessTrailingStop(ts)
	if _, err := e.Cancel(ts.Order.ID, "trader"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if e.TrailingStopCount() != 0 {
		t.Errorf("count = %d, want 0 after cancel", e.TrailingStopCount())
	}
}
