package matching

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func TestPriceBand_RejectsOutsideCollar(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", PriceBand: decimal.RequireFromString("0.1")}) // ±10%

	// Establish a reference price of 100 via a trade.
	e.Process(lim(t, "a", types.SideBuy, "100", "5"))
	e.Process(lim(t, "b", types.SideSell, "100", "5")) // trades at 100 → ref=100

	// Within the band [90,110] is accepted.
	if r := e.Process(lim(t, "c", types.SideBuy, "105", "1")); r.Status == types.OrderStatusRejected {
		t.Errorf("105 within band should be accepted, got %v", r.RejectionReason)
	}
	// Above the band is rejected.
	if r := e.Process(lim(t, "d", types.SideBuy, "111", "1")); !errors.Is(r.RejectionReason, types.ErrPriceOutsideBand) {
		t.Errorf("111 should be rejected outside band, got %q / %v", r.Status, r.RejectionReason)
	}
	// Below the band is rejected.
	if r := e.Process(lim(t, "e", types.SideSell, "89", "1")); !errors.Is(r.RejectionReason, types.ErrPriceOutsideBand) {
		t.Errorf("89 should be rejected outside band, got %q / %v", r.Status, r.RejectionReason)
	}
}

func TestPriceBand_DisabledByDefault(t *testing.T) {
	e := newEngine() // DefaultConfig ⇒ band 0
	e.Process(lim(t, "a", types.SideBuy, "100", "5"))
	e.Process(lim(t, "b", types.SideSell, "100", "5")) // ref=100
	// A wild price is fine with no band.
	if r := e.Process(lim(t, "c", types.SideBuy, "1000", "1")); r.Status == types.OrderStatusRejected {
		t.Errorf("no band ⇒ any price accepted, got %v", r.RejectionReason)
	}
}

func TestHalt_RejectsThenResumes(t *testing.T) {
	e := newEngine()
	e.Halt()
	if !e.IsHalted() {
		t.Fatal("engine should report halted")
	}
	r := e.Process(lim(t, "a", types.SideBuy, "100", "1"))
	if !errors.Is(r.RejectionReason, types.ErrTradingHalted) {
		t.Errorf("halted order should be rejected, got %q / %v", r.Status, r.RejectionReason)
	}

	e.Resume()
	if r := e.Process(lim(t, "a", types.SideBuy, "100", "1")); r.Status == types.OrderStatusRejected {
		t.Errorf("after resume, order should be accepted, got %v", r.RejectionReason)
	}
}
