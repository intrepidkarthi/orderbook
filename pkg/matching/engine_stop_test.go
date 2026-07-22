package matching

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func marketOrder(t *testing.T, user string, side types.Side, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeMarket, 0, qty, types.TIFImmediateOrCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

func stopOrder(t *testing.T, user string, side types.Side, qty, stopPrice int64) *types.StopOrder {
	t.Helper()
	s, err := types.NewStopOrder(marketOrder(t, user, side, qty), stopPrice)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	return s
}

// seedWithLastPrice builds a book and prints one trade so LastTradePrice is set
// to 100, leaving descending resting bids at 100, 96, 95 for a stop to sell into.
func seedWithLastPrice(t *testing.T, e *Engine) {
	e.Process(lim(t, "b100", types.SideBuy, 100, 10))
	e.Process(lim(t, "s100", types.SideSell, 100, 5)) // trades 5 @ 100 -> last=100, bid 100 has 5 left
	e.Process(lim(t, "b96", types.SideBuy, 96, 5))
	e.Process(lim(t, "b95", types.SideBuy, 95, 5))
}

func TestStop_RestsThenTriggers(t *testing.T) {
	e := newEngine()
	seedWithLastPrice(t, e)

	// A sell stop at 95: market is 100, so it should rest (not yet reached).
	so := stopOrder(t, "trader", types.SideSell, 3, 95)
	res := e.ProcessStop(so)
	if res.Status != types.OrderStatusPendingTrigger {
		t.Fatalf("status = %q, want PENDING_TRIGGER", res.Status)
	}
	if e.PendingStopCount() != 1 {
		t.Fatalf("pending stops = %d, want 1", e.PendingStopCount())
	}

	// Drive the price down to 95 with a market sell that sweeps 100, 96, then 95.
	drive := e.Process(marketOrder(t, "seller", types.SideSell, 11))
	if len(drive.Trades) == 0 {
		t.Fatal("driving sell should produce trades")
	}
	// The stop fired and executed.
	if e.PendingStopCount() != 0 {
		t.Errorf("pending stops = %d, want 0 after trigger", e.PendingStopCount())
	}
	if so.Order.FilledQty == 0 {
		t.Error("triggered stop order should have filled")
	}
	// Its fills are included in the driving order's result (cascade).
	filledFromStop := false
	for _, tr := range drive.Trades {
		if tr.SellerUserID == "trader" {
			filledFromStop = true
		}
	}
	if !filledFromStop {
		t.Error("stop order's trades should appear in the triggering result")
	}
}

func TestStop_ImmediateTrigger(t *testing.T) {
	e := newEngine()
	seedWithLastPrice(t, e) // last price 100

	// A sell stop at 100: 100 <= 100 triggers immediately and sells into bids.
	so := stopOrder(t, "trader", types.SideSell, 2, 100)
	res := e.ProcessStop(so)
	if res.Status == types.OrderStatusPendingTrigger {
		t.Fatal("stop at/above market should trigger immediately, not rest")
	}
	if e.PendingStopCount() != 0 {
		t.Errorf("pending stops = %d, want 0", e.PendingStopCount())
	}
	if so.Order.FilledQty == 0 {
		t.Error("immediately-triggered stop should fill")
	}
}

func TestStop_Cancel(t *testing.T) {
	e := newEngine()
	seedWithLastPrice(t, e)

	so := stopOrder(t, "trader", types.SideSell, 3, 90) // rests (90 < 100)
	e.ProcessStop(so)
	if e.PendingStopCount() != 1 {
		t.Fatalf("pending = %d, want 1", e.PendingStopCount())
	}
	// Wrong user can't cancel.
	if _, err := e.Cancel(so.Order.ID, "someone"); !errors.Is(err, types.ErrOrderNotFound) {
		t.Errorf("wrong-user cancel err = %v, want ErrOrderNotFound", err)
	}
	if _, err := e.Cancel(so.Order.ID, "trader"); err != nil {
		t.Fatalf("owner cancel: %v", err)
	}
	if e.PendingStopCount() != 0 {
		t.Errorf("pending = %d, want 0 after cancel", e.PendingStopCount())
	}
}

func TestStopLimit_RestsAsLimitOnTrigger(t *testing.T) {
	e := newEngine()
	seedWithLastPrice(t, e)

	// Stop-limit: underlying is a limit sell at 97; stop at 95. When triggered it
	// sells what it can at ≥97 and rests the rest as a limit.
	underlying, _ := types.NewOrder("trader", "BTC-USD", types.SideSell, types.OrderTypeLimit, 97, 4, types.TIFGoodTillCancel)
	so, _ := types.NewStopOrder(underlying, 95)
	e.ProcessStop(so)

	// Drive price to 95.
	e.Process(marketOrder(t, "seller", types.SideSell, 11))
	if e.PendingStopCount() != 0 {
		t.Errorf("stop should have triggered, pending = %d", e.PendingStopCount())
	}
	// The limit (97) can't fully fill into remaining low bids, so a remainder
	// rests as an ask at 97.
	if ask, _, ok := e.BestAsk(); !ok || ask != 97 {
		t.Errorf("expected a resting ask at 97 from the stop-limit, got %d (ok=%v)", ask, ok)
	}
}
