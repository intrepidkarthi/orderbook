package strategy

import (
	"math"
	"testing"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func mustAS(t *testing.T, p ASParams) *AvellanedaStoikov {
	t.Helper()
	a, err := NewAvellanedaStoikov(p)
	if err != nil {
		t.Fatalf("NewAvellanedaStoikov: %v", err)
	}
	return a
}

func TestValidate(t *testing.T) {
	if _, err := NewAvellanedaStoikov(ASParams{Gamma: 0, Kappa: 1, Sigma: 1}); err == nil {
		t.Error("gamma <= 0 should error")
	}
	if _, err := NewAvellanedaStoikov(ASParams{Gamma: 0.1, Kappa: 0, Sigma: 1}); err == nil {
		t.Error("kappa <= 0 should error")
	}
	if _, err := NewAvellanedaStoikov(ASParams{Gamma: 0.1, Kappa: 1, Sigma: -1}); err == nil {
		t.Error("sigma < 0 should error")
	}
}

func TestFlatInventory_SymmetricAroundMid(t *testing.T) {
	a := mustAS(t, ASParams{Gamma: 0.1, Kappa: 1.5, Sigma: 0.2})
	q := a.Quote(100, 0, 0.5)
	if !approx(q.Reservation, 100, 1e-9) {
		t.Errorf("reservation = %v, want mid=100 when flat", q.Reservation)
	}
	// Symmetric: mid − bid == ask − mid.
	if !approx(100-q.Bid, q.Ask-100, 1e-9) {
		t.Errorf("quotes not symmetric around mid: bid=%v ask=%v", q.Bid, q.Ask)
	}
	if !approx(q.Ask-q.Bid, q.Spread, 1e-9) {
		t.Errorf("ask-bid=%v != spread=%v", q.Ask-q.Bid, q.Spread)
	}
}

func TestInventorySkew(t *testing.T) {
	a := mustAS(t, ASParams{Gamma: 0.1, Kappa: 1.5, Sigma: 0.2})
	long := a.Quote(100, 5, 0.5)
	short := a.Quote(100, -5, 0.5)
	if !(long.Reservation < 100) {
		t.Errorf("long inventory should skew reservation below mid, got %v", long.Reservation)
	}
	if !(short.Reservation > 100) {
		t.Errorf("short inventory should skew reservation above mid, got %v", short.Reservation)
	}
	// Symmetric magnitude for ±q.
	if !approx(100-long.Reservation, short.Reservation-100, 1e-9) {
		t.Error("reservation skew should be symmetric in ±inventory")
	}
}

func TestReservation_NumericExample(t *testing.T) {
	// From the AS explainer: mid=100, q=5, γ=0.1, σ²=0.02, (T−t)=0.5
	// ⇒ reservation = 100 − 5·0.1·0.02·0.5 = 99.995
	a := mustAS(t, ASParams{Gamma: 0.1, Kappa: 1.5, Sigma: math.Sqrt(0.02)})
	q := a.Quote(100, 5, 0.5)
	if !approx(q.Reservation, 99.995, 1e-9) {
		t.Errorf("reservation = %v, want 99.995", q.Reservation)
	}
}

func TestSpread_ArrivalFloorAndGrowth(t *testing.T) {
	a := mustAS(t, ASParams{Gamma: 0.1, Kappa: 1.5, Sigma: 0.2})
	// At timeRemaining=0 the risk term vanishes; spread == (2/γ)·ln(1+γ/k).
	floor := (2.0 / 0.1) * math.Log(1.0+0.1/1.5)
	if got := a.Quote(100, 0, 0).Spread; !approx(got, floor, 1e-9) {
		t.Errorf("spread at t=0 = %v, want arrival floor %v", got, floor)
	}
	// Spread grows with time remaining ...
	if !(a.Quote(100, 0, 1).Spread > a.Quote(100, 0, 0.5).Spread) {
		t.Error("spread should increase with time remaining")
	}
	// ... and with volatility.
	hi := mustAS(t, ASParams{Gamma: 0.1, Kappa: 1.5, Sigma: 0.5})
	if !(hi.Quote(100, 0, 0.5).Spread > a.Quote(100, 0, 0.5).Spread) {
		t.Error("spread should increase with volatility")
	}
}
