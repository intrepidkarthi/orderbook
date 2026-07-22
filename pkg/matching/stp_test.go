package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

// TestSTP_TakerDecides: the taker's per-order STPMode governs, overriding the
// engine default — even when the engine allows self-trades.
func TestSTP_TakerDecides(t *testing.T) {
	e := NewEngine(Config{Symbol: "X", SelfTradePrevention: STPAllow})
	e.Process(lim(t, "same", types.SideSell, 100, 5))

	taker := lim(t, "same", types.SideBuy, 100, 3)
	taker.STPMode = string(STPCancelNewest) // taker overrides the ALLOW default
	res := e.Process(taker)

	if len(res.Trades) != 0 {
		t.Errorf("taker STP mode should prevent the self-trade, got %d trades", len(res.Trades))
	}
	if _, qty, _ := e.BestAsk(); qty != 5 {
		t.Errorf("maker should be untouched at qty 5, got %d", qty)
	}
}

// TestSTP_Decrement: DECREMENT shrinks both sides by the overlap with no trade.
func TestSTP_Decrement(t *testing.T) {
	e := NewEngine(Config{Symbol: "X", SelfTradePrevention: STPDecrement})
	e.Process(lim(t, "same", types.SideSell, 100, 5)) // maker
	taker := lim(t, "same", types.SideBuy, 100, 3)
	res := e.Process(taker)

	if len(res.Trades) != 0 {
		t.Errorf("DECREMENT prints no trade, got %d", len(res.Trades))
	}
	if taker.RemainingQty != 0 || taker.Status != types.OrderStatusCancelled {
		t.Errorf("smaller (taker) side should fully cancel: rem=%d status=%s", taker.RemainingQty, taker.Status)
	}
	// Maker shrinks 5 → 2 and keeps resting.
	if _, qty, ok := e.BestAsk(); !ok || qty != 2 {
		t.Errorf("maker should shrink to 2, got qty=%d ok=%v", qty, ok)
	}
}

// TestSTP_TradeGroup: a shared non-zero trade group prevents self-trades across
// different users.
func TestSTP_TradeGroup(t *testing.T) {
	e := NewEngine(Config{Symbol: "X", SelfTradePrevention: STPCancelNewest})
	mk := lim(t, "accountA", types.SideSell, 100, 5)
	mk.TradeGroupID = 7
	e.Process(mk)

	taker := lim(t, "accountB", types.SideBuy, 100, 3) // different user...
	taker.TradeGroupID = 7                             // ...same group
	res := e.Process(taker)

	if len(res.Trades) != 0 {
		t.Errorf("same trade group should be self-prevented, got %d trades", len(res.Trades))
	}
	// A genuinely different account (no shared group) trades normally.
	other := lim(t, "accountC", types.SideBuy, 100, 2)
	if r := e.Process(other); len(r.Trades) != 1 {
		t.Errorf("cross-account order should trade, got %d", len(r.Trades))
	}
}

// TestSTP_PrivilegedExempt: a privileged (liquidation) order is exempt from STP
// and the price band.
func TestSTP_PrivilegedExempt(t *testing.T) {
	e := NewEngine(Config{Symbol: "X", SelfTradePrevention: STPCancelBoth,
		PriceBand: decimal.RequireFromString("0.1")})
	e.Process(lim(t, "same", types.SideBuy, 100, 5))
	e.Process(lim(t, "x", types.SideSell, 100, 5)) // trade → last=100, ref set

	// Re-seed a self maker and hit it with a privileged taker: STP is skipped.
	e.Process(lim(t, "same", types.SideSell, 100, 4))
	taker := lim(t, "same", types.SideBuy, 100, 3)
	taker.Privileged = true
	if r := e.Process(taker); len(r.Trades) != 1 {
		t.Errorf("privileged order should bypass STP and trade, got %d", len(r.Trades))
	}
	// Privileged also bypasses the price band (200 is outside ±10% of 100).
	priv := lim(t, "liq", types.SideBuy, 200, 1)
	priv.Privileged = true
	if r := e.Process(priv); r.Status == types.OrderStatusRejected {
		t.Errorf("privileged order should bypass the band, got %v", r.RejectionReason)
	}
}
