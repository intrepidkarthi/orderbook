package matching

import (
	"errors"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestCancelOnly checks the degraded state: cancels succeed, new liquidity is
// rejected with ErrNewOrdersHalted, and Resume restores normal trading — the
// Coinbase cancel-only → full recovery path.
func TestCancelOnly(t *testing.T) {
	e := newEngine()
	resting := lim(t, "mm", types.SideBuy, 100, 5)
	e.Process(resting)

	e.SetCancelOnly()
	if e.State() != StateCancelOnly {
		t.Fatalf("state = %v, want CancelOnly", e.State())
	}

	// New liquidity is rejected...
	r := e.Process(lim(t, "t", types.SideSell, 100, 1))
	if r.Status != types.OrderStatusRejected || !errors.Is(r.RejectionReason, types.ErrNewOrdersHalted) {
		t.Errorf("new order under cancel-only: status=%q reason=%v, want REJECTED / ErrNewOrdersHalted", r.Status, r.RejectionReason)
	}
	// ...but the resting order is still there and cancellable.
	if _, err := e.Cancel(resting.ID, "mm"); err != nil {
		t.Errorf("cancel under cancel-only should succeed, got %v", err)
	}

	e.Resume()
	if r := e.Process(lim(t, "t", types.SideBuy, 100, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("after resume, order should be accepted, got %v", r.RejectionReason)
	}
}

// TestGuardrailTrips checks the self-output tripwire: once trades in the window
// exceed the cap, the engine trips itself to Halted (the Knight lesson — guard
// the engine's own output).
func TestGuardrailTrips(t *testing.T) {
	fixed := time.Unix(0, 0).UTC()
	e := NewEngine(Config{
		Symbol:    "X",
		Clock:     func() time.Time { return fixed }, // frozen: everything is in one window
		Guardrail: Guardrail{MaxTrades: 3, Window: time.Minute},
	})
	// Rest asks, then take them one trade at a time.
	for i := 0; i < 6; i++ {
		e.Process(ord(t, "mm", types.SideSell, types.OrderTypeLimit, int64(100+i), 1, types.TIFGoodTillCancel))
	}
	tripped := -1
	for i := 0; i < 6; i++ {
		r := e.Process(ord(t, "t", types.SideBuy, types.OrderTypeMarket, 0, 1, types.TIFImmediateOrCancel))
		if r.Status == types.OrderStatusRejected && errors.Is(r.RejectionReason, types.ErrTradingHalted) {
			tripped = i
			break
		}
	}
	if !e.IsHalted() {
		t.Fatalf("engine should have tripped to Halted after >%d trades", 3)
	}
	// 3 trades allowed; the 4th trade trips, so the 5th taker (index 4) is rejected.
	if tripped != 4 {
		t.Errorf("tripped at taker %d, want 4 (after the cap of 3 trades)", tripped)
	}
}
