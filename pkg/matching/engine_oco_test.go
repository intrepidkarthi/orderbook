package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func makeOCO(t *testing.T) *types.OCOOrder {
	t.Helper()
	// Bracket for a long: take-profit sell at 105, stop-loss sell stop at 95.
	primary := lim(t, "trader", types.SideSell, 105, 3)
	stop := stopOrder(t, "trader", types.SideSell, 3, 95)
	oco, err := types.NewOCOOrder(primary, stop)
	if err != nil {
		t.Fatalf("NewOCOOrder: %v", err)
	}
	return oco
}

func TestOCO_PrimaryFillCancelsStop(t *testing.T) {
	e := newEngine()
	seedWithLastPrice(t, e) // last=100
	oco := makeOCO(t)
	e.ProcessOCO(oco)

	// Primary rests as an ask at 105; stop rests pending.
	if ask, _, ok := e.BestAsk(); !ok || ask != 105 {
		t.Fatalf("primary should rest at 105, got %d (ok=%v)", ask, ok)
	}
	if e.PendingStopCount() != 1 {
		t.Fatalf("pending stops = %d, want 1", e.PendingStopCount())
	}

	// A buyer lifts the take-profit → primary fills → stop is cancelled.
	e.Process(lim(t, "buyer", types.SideBuy, 105, 3))
	if !oco.Primary.IsFilled() {
		t.Error("primary should be filled")
	}
	if e.PendingStopCount() != 0 {
		t.Errorf("stop should be cancelled after primary fill, pending = %d", e.PendingStopCount())
	}
}

func TestOCO_StopTriggerCancelsPrimary(t *testing.T) {
	e := newEngine()
	seedWithLastPrice(t, e) // last=100, bids 100/96/95
	oco := makeOCO(t)
	e.ProcessOCO(oco)

	// Drive price down to 95 → stop-loss fires → primary limit is cancelled.
	e.Process(marketOrder(t, "seller", types.SideSell, 11))

	if e.PendingStopCount() != 0 {
		t.Errorf("stop should have triggered, pending = %d", e.PendingStopCount())
	}
	if _, _, ok := e.BestAsk(); ok {
		t.Error("primary (105 ask) should have been cancelled → no asks remain")
	}
	if oco.Primary.Status != types.OrderStatusCancelled {
		t.Errorf("primary status = %q, want CANCELLED", oco.Primary.Status)
	}
	if oco.Stop.Order.FilledQty == 0 {
		t.Error("triggered stop should have filled")
	}
}
