package matching

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

// TestMaxOrderQty: a single order larger than the cap is rejected; one within it
// is accepted; a Privileged (liquidation) order bypasses the cap.
func TestMaxOrderQty(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", MaxOrderQty: 100})

	if r := e.Process(lim(t, "a", types.SideBuy, 100, 100)); r.Status == types.OrderStatusRejected {
		t.Errorf("qty at the cap should be accepted, got %v", r.RejectionReason)
	}
	r := e.Process(lim(t, "b", types.SideBuy, 100, 101))
	if r.Status != types.OrderStatusRejected || !errors.Is(r.RejectionReason, types.ErrOrderExceedsMaxQty) {
		t.Errorf("qty over the cap should be ErrOrderExceedsMaxQty, got %q / %v", r.Status, r.RejectionReason)
	}

	// Privileged orders (liquidation/ADL) are exempt.
	priv := lim(t, "liq", types.SideBuy, 100, 1_000_000)
	priv.Privileged = true
	if r := e.Process(priv); r.Status == types.OrderStatusRejected && errors.Is(r.RejectionReason, types.ErrOrderExceedsMaxQty) {
		t.Errorf("privileged order should bypass the size cap, got %v", r.RejectionReason)
	}
}

// TestMaxOrderNotional: a limit order whose price×qty exceeds the cap is rejected;
// one at the cap is accepted.
func TestMaxOrderNotional(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", MaxOrderNotional: 10_000})

	if r := e.Process(lim(t, "a", types.SideBuy, 100, 100)); r.Status == types.OrderStatusRejected {
		t.Errorf("notional at the cap (100*100=10000) should be accepted, got %v", r.RejectionReason)
	}
	r := e.Process(lim(t, "b", types.SideBuy, 100, 101)) // 10100 > 10000
	if r.Status != types.OrderStatusRejected || !errors.Is(r.RejectionReason, types.ErrOrderExceedsMaxNotional) {
		t.Errorf("notional over the cap should be ErrOrderExceedsMaxNotional, got %q / %v", r.Status, r.RejectionReason)
	}
}

// TestNotionalOverflowRejected: an order whose price×qty overflows int64 is
// rejected even with no notional cap configured — the value can never enter the
// book to wrap a sum later (the Bitcoin CVE-2010-5139 class).
func TestNotionalOverflowRejected(t *testing.T) {
	e := NewEngine(DefaultConfig("BTC-USD")) // no caps set
	o := lim(t, "a", types.SideBuy, math.MaxInt64/2, 4)
	r := e.Process(o)
	if r.Status != types.OrderStatusRejected || !errors.Is(r.RejectionReason, types.ErrNotionalOverflow) {
		t.Errorf("overflowing notional should be ErrNotionalOverflow, got %q / %v", r.Status, r.RejectionReason)
	}
}

// TestMinRestingTime: a cancel arriving before the minimum resting time is
// rejected; once enough time elapses on the injected clock, the cancel succeeds.
// A privileged order is exempt.
func TestMinRestingTime(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	e := NewEngine(Config{
		Symbol:         "BTC-USD",
		Clock:          func() time.Time { return now },
		MinRestingTime: time.Second,
	})

	o := lim(t, "mm", types.SideSell, 100, 5)
	e.Process(o) // placed at t=0

	// Cancel at t=0.5s — too soon.
	now = time.Unix(0, 500*int64(time.Millisecond)).UTC()
	if _, err := e.Cancel(o.ID, "mm"); !errors.Is(err, types.ErrCancelTooSoon) {
		t.Fatalf("cancel before min resting should be ErrCancelTooSoon, got %v", err)
	}
	// The order must still be live and cancellable later.
	if _, ok := e.Book().Get(o.ID); !ok {
		t.Fatal("order should remain resting after a too-soon cancel")
	}

	// Cancel at t=1.2s — allowed.
	now = time.Unix(0, 1200*int64(time.Millisecond)).UTC()
	if _, err := e.Cancel(o.ID, "mm"); err != nil {
		t.Fatalf("cancel after min resting should succeed, got %v", err)
	}

	// Privileged order: exempt from the minimum resting time.
	now = time.Unix(0, 0).UTC()
	p := lim(t, "liq", types.SideSell, 101, 5)
	p.Privileged = true
	e.Process(p)
	now = time.Unix(0, int64(time.Millisecond)).UTC() // 1ms later, well under 1s
	if _, err := e.Cancel(p.ID, "liq"); err != nil {
		t.Fatalf("privileged cancel should bypass min resting, got %v", err)
	}
}

// TestMarkStepGuard: an oversized single mark update is rejected and leaves the
// mark unchanged; a within-step update, the first mark, and clearing all succeed.
func TestMarkStepGuard(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", MaxMarkStep: decimal.RequireFromString("0.20")}) // ±20%

	// First mark (from unset 0) is always accepted, however large.
	if err := e.SetMarkPrice(1000); err != nil {
		t.Fatalf("first mark should be accepted: %v", err)
	}
	// +20% exactly (to 1200) is within the step.
	if err := e.SetMarkPrice(1200); err != nil {
		t.Fatalf("+20%% step should be accepted: %v", err)
	}
	// A jump to 2000 (+66%) is rejected and the mark is left unchanged.
	if err := e.SetMarkPrice(2000); !errors.Is(err, types.ErrMarkStepTooLarge) {
		t.Fatalf("oversized mark step should be ErrMarkStepTooLarge, got %v", err)
	}
	if got := e.MarkPrice(); got != 1200 {
		t.Errorf("rejected mark update must not change the mark, got %d want 1200", got)
	}
	// Clearing the mark to 0 is always allowed.
	if err := e.SetMarkPrice(0); err != nil {
		t.Fatalf("clearing the mark should be accepted: %v", err)
	}
	// A negative mark is rejected.
	if err := e.SetMarkPrice(-1); !errors.Is(err, types.ErrInvalidPrice) {
		t.Errorf("negative mark should be ErrInvalidPrice, got %v", err)
	}
}

func TestCheckedMul(t *testing.T) {
	cases := []struct {
		a, b   int64
		want   int64
		wantOK bool
	}{
		{0, math.MaxInt64, 0, true},
		{100, 100, 10_000, true},
		{math.MaxInt64, 1, math.MaxInt64, true},
		{math.MaxInt64 / 2, 4, 0, false},
		{math.MaxInt64, 2, 0, false},
	}
	for _, c := range cases {
		got, ok := checkedMul(c.a, c.b)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("checkedMul(%d,%d) = (%d,%v), want (%d,%v)", c.a, c.b, got, ok, c.want, c.wantOK)
		}
	}
}

func TestSaturatingAdd(t *testing.T) {
	if got := saturatingAdd(5, 7); got != 12 {
		t.Errorf("saturatingAdd(5,7) = %d, want 12", got)
	}
	if got := saturatingAdd(math.MaxInt64-1, 10); got != math.MaxInt64 {
		t.Errorf("saturatingAdd overflow should clamp to MaxInt64, got %d", got)
	}
}
