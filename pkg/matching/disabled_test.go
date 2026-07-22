package matching

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestDisabledOrderClasses verifies each advanced order family can be
// feature-flagged off (rejected with ErrOrderTypeDisabled) without affecting the
// others or plain limit/market flow — so one buggy exotic type can be disabled
// in production without downing the venue.
func TestDisabledOrderClasses(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", DisabledClasses: []OrderClass{
		ClassStop, ClassTrailing, ClassOCO, ClassIceberg, ClassPegged,
	}})
	seedWithLastPrice(t, e) // resting book + last price 100

	disabled := func(name string, r *MatchResult) {
		t.Helper()
		if r.Status != types.OrderStatusRejected || !errors.Is(r.RejectionReason, types.ErrOrderTypeDisabled) {
			t.Errorf("%s: status=%q reason=%v, want REJECTED / ErrOrderTypeDisabled", name, r.Status, r.RejectionReason)
		}
	}
	disabled("stop", e.ProcessStop(stopOrder(t, "u", types.SideSell, 3, 95)))
	disabled("trailing", e.ProcessTrailingStop(trailingStop(t, "u", types.SideSell, 3, 5)))
	disabled("iceberg", e.ProcessIceberg(iceberg(t, "u", types.SideBuy, 99, 10, 3)))
	disabled("pegged", e.ProcessPegged(pegged(t, "u", types.SideBuy, 3, types.PegToBid, 0)))
	disabled("oco", e.ProcessOCO(makeOCO(t)))

	// Nothing from the rejects rested; plain limit/market still work.
	if e.PendingStopCount() != 0 || e.TrailingStopCount() != 0 {
		t.Errorf("rejected exotics must not rest: stops=%d trailing=%d", e.PendingStopCount(), e.TrailingStopCount())
	}
	if r := e.Process(lim(t, "u", types.SideBuy, 90, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("plain limit must never be gated, got %v", r.RejectionReason)
	}

	// Gating one class leaves the rest working.
	e2 := NewEngine(Config{Symbol: "BTC-USD", DisabledClasses: []OrderClass{ClassStop}})
	seedWithLastPrice(t, e2)
	if r := e2.ProcessIceberg(iceberg(t, "w", types.SideBuy, 98, 10, 3)); r.Status == types.OrderStatusRejected {
		t.Errorf("iceberg should work when only Stop is disabled, got %v", r.RejectionReason)
	}
	disabled("stop (only-stop engine)", e2.ProcessStop(stopOrder(t, "w", types.SideSell, 3, 95)))
}
