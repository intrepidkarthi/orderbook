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

// TestMinMarkDepth: a mark move to a price the book does not back with enough
// resting depth is rejected; once depth exists near the price it is accepted.
func TestMinMarkDepth(t *testing.T) {
	e := NewEngine(Config{
		Symbol:        "BTC-USD",
		MinMarkDepth:  10,
		MarkDepthBand: decimal.RequireFromString("0.05"), // ±5%
	})
	// First mark is always accepted (no prior mark to protect).
	if err := e.SetMarkPrice(100); err != nil {
		t.Fatalf("first mark: %v", err)
	}
	// Moving the mark to 110 with an empty book near 110 is unbacked → rejected.
	if err := e.SetMarkPrice(110); !errors.Is(err, types.ErrMarkDepthTooThin) {
		t.Fatalf("unbacked mark move should be ErrMarkDepthTooThin, got %v", err)
	}
	if e.MarkPrice() != 100 {
		t.Errorf("rejected move must not change the mark, got %d", e.MarkPrice())
	}
	// Rest real depth near 110 (window ±5 ticks of 110 = [105,115]).
	e.Process(lim(t, "mm", types.SideSell, 110, 6))
	e.Process(lim(t, "mm2", types.SideBuy, 109, 6))
	if err := e.SetMarkPrice(110); err != nil {
		t.Fatalf("mark move backed by depth should be accepted, got %v", err)
	}
	// Clearing the mark to 0 is always allowed regardless of depth.
	if err := e.SetMarkPrice(0); err != nil {
		t.Errorf("clearing the mark should be accepted, got %v", err)
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

// TestDustFloors: orders below the min size / notional floor are rejected.
func TestDustFloors(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", MinOrderQty: 5, MinOrderNotional: 1000})

	if r := e.Process(lim(t, "a", types.SideBuy, 100, 4)); !errors.Is(r.RejectionReason, types.ErrOrderBelowMinQty) {
		t.Errorf("qty 4 < min 5 should be ErrOrderBelowMinQty, got %v", r.RejectionReason)
	}
	// qty 5 clears the size floor but 100*5=500 < 1000 notional floor.
	if r := e.Process(lim(t, "b", types.SideBuy, 100, 5)); !errors.Is(r.RejectionReason, types.ErrOrderBelowMinNotional) {
		t.Errorf("notional 500 < min 1000 should be ErrOrderBelowMinNotional, got %v", r.RejectionReason)
	}
	if r := e.Process(lim(t, "c", types.SideBuy, 100, 10)); r.Status == types.OrderStatusRejected {
		t.Errorf("qty 10 (notional 1000) should be accepted, got %v", r.RejectionReason)
	}
}

// TestMaxOrdersPerAccount: the per-account resting cap rejects the overflow order,
// and the count is released when an order leaves the book (cancel or fill) so the
// user can post again.
func TestMaxOrdersPerAccount(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", MaxOrdersPerAccount: 2})

	o1 := lim(t, "u", types.SideBuy, 90, 1)
	o2 := lim(t, "u", types.SideBuy, 91, 1)
	e.Process(o1)
	e.Process(o2)
	// Third resting order for the same user is rejected.
	if r := e.Process(lim(t, "u", types.SideBuy, 92, 1)); !errors.Is(r.RejectionReason, types.ErrTooManyOrders) {
		t.Fatalf("3rd resting order should be ErrTooManyOrders, got %v", r.RejectionReason)
	}
	// A different user is unaffected.
	if r := e.Process(lim(t, "v", types.SideBuy, 89, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("other user should not be capped, got %v", r.RejectionReason)
	}
	// Cancelling frees a slot; the user can post again.
	e.Cancel(o1.ID, "u")
	if r := e.Process(lim(t, "u", types.SideBuy, 92, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("after a cancel the user should be under the cap, got %v", r.RejectionReason)
	}
	// A fill also frees a slot: u now rests o2@91 and the new @92 (2 orders); a
	// seller consuming o2 removes it from the book, dropping u back to 1.
	e.Process(lim(t, "s", types.SideSell, 91, 1)) // fills o2 fully → leaves the book
	if got := e.Book().OrdersByUser("u"); got != 1 {
		t.Errorf("a fill should release a slot: user u should have 1 resting, got %d", got)
	}
	// ...so u can post again without hitting the cap.
	if r := e.Process(lim(t, "u", types.SideBuy, 88, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("after a fill freed a slot the user should be able to post, got %v", r.RejectionReason)
	}
}

// TestClientOrderIDDedup: a duplicate (user, client-id) submit is rejected; a
// rejected order stays resubmittable; the ring evicts old keys; empty ids skip.
func TestClientOrderIDDedup(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", DedupClientOrderIDs: 2})

	withCID := func(user, cid string, price, qty int64) *types.Order {
		o := lim(t, user, types.SideBuy, price, qty)
		o.ClientOrderID = cid
		return o
	}

	if r := e.Process(withCID("u", "c1", 90, 1)); r.Status == types.OrderStatusRejected {
		t.Fatalf("first submit should be accepted, got %v", r.RejectionReason)
	}
	// Same (user, client-id) → duplicate.
	if r := e.Process(withCID("u", "c1", 90, 1)); !errors.Is(r.RejectionReason, types.ErrDuplicateClientOrderID) {
		t.Fatalf("duplicate client id should be rejected, got %v", r.RejectionReason)
	}
	// Same client-id, different user → not a duplicate.
	if r := e.Process(withCID("v", "c1", 90, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("same client id under a different user should be allowed, got %v", r.RejectionReason)
	}
	// Empty client id is never deduped.
	e.Process(lim(t, "u", types.SideBuy, 90, 1))
	if r := e.Process(lim(t, "u", types.SideBuy, 90, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("empty client id must not be deduped, got %v", r.RejectionReason)
	}

	// Ring holds only the last 2 keys. Insert c2, c3 (for user "w") to evict c1's
	// nothing for w; then c-old should be resubmittable after eviction.
	e2 := NewEngine(Config{Symbol: "BTC-USD", DedupClientOrderIDs: 2})
	e2.Process(withCID("w", "a", 90, 1)) // ring: [a]
	e2.Process(withCID("w", "b", 90, 1)) // ring: [a,b]
	e2.Process(withCID("w", "c", 90, 1)) // evicts a → ring: [c,b]
	if r := e2.Process(withCID("w", "a", 90, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("client id 'a' should be resubmittable after eviction, got %v", r.RejectionReason)
	}

	// A rejected order (band) must stay resubmittable under the same id.
	e3 := NewEngine(Config{Symbol: "BTC-USD", DedupClientOrderIDs: 4, PriceBand: decimal.RequireFromString("0.1")})
	e3.Process(lim(t, "mmA", types.SideSell, 100, 1))
	e3.Process(lim(t, "mmB", types.SideBuy, 100, 1)) // trades → last=100, band ±10%
	rej := withCID("z", "k", 200, 1)                 // 200 is outside the band
	if r := e3.Process(rej); !errors.Is(r.RejectionReason, types.ErrPriceOutsideBand) {
		t.Fatalf("setup: expected band rejection, got %v", r.RejectionReason)
	}
	ok := withCID("z", "k", 105, 1) // same client id, now in-band
	if r := e3.Process(ok); errors.Is(r.RejectionReason, types.ErrDuplicateClientOrderID) {
		t.Error("a band-rejected order's client id must remain resubmittable")
	}
}

// TestForceTradeSizeCap: a forced print larger than the per-call cap is rejected,
// so liquidation must be chunked.
func TestForceTradeSizeCap(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", MaxForceTradeQty: 3})
	liq, _ := types.NewOrder("under", "BTC-USD", types.SideSell, types.OrderTypeMarket, 0, 10, types.TIFImmediateOrCancel)
	win, _ := types.NewOrder("winner", "BTC-USD", types.SideBuy, types.OrderTypeLimit, 90, 10, types.TIFGoodTillCancel)

	if _, err := e.ForceTrade(liq, win, 90, 4); !errors.Is(err, types.ErrForceTradeTooLarge) {
		t.Fatalf("forced qty 4 > cap 3 should be ErrForceTradeTooLarge, got %v", err)
	}
	// A chunk within the cap goes through.
	if _, err := e.ForceTrade(liq, win, 90, 3); err != nil {
		t.Fatalf("forced qty 3 (== cap) should be accepted, got %v", err)
	}
}

// TestBandBreachPause: a band breach halts trading for the configured duration and
// auto-resumes on the injected clock, emitting HALTED then RESUMED.
func TestBandBreachPause(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	sink := &recSink{}
	e := NewEngine(Config{
		Symbol:          "BTC-USD",
		Clock:           func() time.Time { return now },
		PriceBand:       decimal.RequireFromString("0.1"), // ±10%
		BandBreachPause: 30 * time.Second,
		EventSink:       sink,
	})
	// Establish a reference price of 100.
	e.Process(lim(t, "mm", types.SideSell, 100, 1))
	e.Process(lim(t, "mm2", types.SideBuy, 100, 1)) // trades → last=100

	// A breach (200 ≫ +10%) is rejected AND arms the pause.
	if r := e.Process(lim(t, "x", types.SideBuy, 200, 1)); !errors.Is(r.RejectionReason, types.ErrPriceOutsideBand) {
		t.Fatalf("breach should be rejected, got %v", r.RejectionReason)
	}
	if e.State() != StateHalted {
		t.Fatalf("band breach should halt trading, state=%v", e.State())
	}
	// During the pause even an in-band order is rejected as halted.
	if r := e.Process(lim(t, "y", types.SideBuy, 101, 1)); !errors.Is(r.RejectionReason, types.ErrTradingHalted) {
		t.Errorf("during the pause orders should be ErrTradingHalted, got %v", r.RejectionReason)
	}
	// Advance past the pause; the next order auto-resumes trading.
	now = time.Unix(31, 0).UTC()
	if r := e.Process(lim(t, "z", types.SideBuy, 101, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("after the pause elapses trading should resume, got %v", r.RejectionReason)
	}
	if e.State() != StateOpen {
		t.Errorf("engine should have auto-resumed to Open, state=%v", e.State())
	}
	if countKind(sink.got, EventHalted) != 1 || countKind(sink.got, EventResumed) != 1 {
		t.Errorf("expected one HALTED and one RESUMED event, got %d/%d",
			countKind(sink.got, EventHalted), countKind(sink.got, EventResumed))
	}
}

// TestGuardrailEmitsHalted: the self-output guardrail trip publishes a HALTED
// event so operators can page on it.
func TestGuardrailEmitsHalted(t *testing.T) {
	fixed := time.Unix(0, 0).UTC()
	sink := &recSink{}
	e := NewEngine(Config{
		Symbol:    "BTC-USD",
		Clock:     func() time.Time { return fixed },
		Guardrail: Guardrail{MaxTrades: 2, Window: time.Minute},
		EventSink: sink,
	})
	for i := range 5 {
		e.Process(ord(t, "mm", types.SideSell, types.OrderTypeLimit, int64(100+i), 1, types.TIFGoodTillCancel))
	}
	for range 5 {
		e.Process(ord(t, "t", types.SideBuy, types.OrderTypeMarket, 0, 1, types.TIFImmediateOrCancel))
	}
	if countKind(sink.got, EventHalted) != 1 {
		t.Errorf("guardrail trip should emit exactly one HALTED event, got %d", countKind(sink.got, EventHalted))
	}
}

// countKind counts recorded events of a kind (recEvent/recSink live in event_test.go).
func countKind(got []recEvent, k EventKind) int {
	n := 0
	for _, e := range got {
		if e.kind == k {
			n++
		}
	}
	return n
}
