package matching

import (
	"errors"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// DAY and GTD. The time-in-force most real order flow uses, and the one this engine
// did not have — a client wanting an order gone at the close had to remember to cancel
// it, which is a job the venue should be doing.

// captureSink records events so a test can assert what a consumer was told.
type captureSink struct{ events []Event }

func (c *captureSink) OnEvents(evs []Event) {
	for _, e := range evs {
		c.events = append(c.events, e)
	}
}

// fakeClock drives the engine's notion of now, so expiry is tested against a stated
// instant rather than against how long the test took to run.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func expiryEngine(t *testing.T, clk *fakeClock, close time.Time) *Engine {
	t.Helper()
	cfg := DefaultConfig("X")
	cfg.Clock = clk.now
	if !close.IsZero() {
		cfg.SessionClose = func() time.Time { return close }
	}
	return NewEngine(cfg)
}

func datedOrder(t *testing.T, user string, tif types.TimeInForce, price, qty int64, expires time.Time) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", types.SideBuy, types.OrderTypeLimit, price, qty, tif)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	o.ExpiresAt = expires
	return o
}

// TestDayOrderExpiresAtTheClose is the point of the feature.
func TestDayOrderExpiresAtTheClose(t *testing.T) {
	start := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	close := start.Add(8 * time.Hour)
	clk := &fakeClock{t: start}
	e := expiryEngine(t, clk, close)

	o := datedOrder(t, "mm", types.TIFDay, 100, 10, time.Time{})
	if res := e.Process(o); res.RejectionReason != nil {
		t.Fatalf("DAY order rejected: %v", res.RejectionReason)
	}
	if e.OrderCount() != 1 {
		t.Fatalf("book holds %d, want the resting DAY order", e.OrderCount())
	}
	if o.ExpiresAt != close.UTC() {
		t.Errorf("deadline is %v, want the session close %v", o.ExpiresAt, close.UTC())
	}

	// Still inside the session: it stays.
	clk.add(7 * time.Hour)
	e.ExpireDue()
	if e.OrderCount() != 1 {
		t.Fatalf("the order left before the close")
	}

	// Past the close: it goes.
	clk.add(2 * time.Hour)
	e.ExpireDue()
	if e.OrderCount() != 0 {
		t.Errorf("book still holds %d orders after the session closed", e.OrderCount())
	}
	if o.Status != types.OrderStatusCancelled {
		t.Errorf("expired order status is %v, want cancelled", o.Status)
	}
}

// TestDayOrderNeedsASession — refusing is the honest answer. Treating it as GTC would
// leave an order the client believes dies at the close resting forever.
func TestDayOrderNeedsASession(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)}
	e := expiryEngine(t, clk, time.Time{}) // no session close configured

	res := e.Process(datedOrder(t, "mm", types.TIFDay, 100, 10, time.Time{}))
	if !errors.Is(res.RejectionReason, types.ErrNoSessionClose) {
		t.Errorf("err = %v, want ErrNoSessionClose", res.RejectionReason)
	}
	if e.OrderCount() != 0 {
		t.Error("a DAY order rested on a venue with no session")
	}
}

// TestGTDExpiresAtItsOwnDeadline.
func TestGTDExpiresAtItsOwnDeadline(t *testing.T) {
	start := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: start}
	e := expiryEngine(t, clk, start.Add(8*time.Hour))

	deadline := start.Add(30 * time.Minute)
	o := datedOrder(t, "mm", types.TIFGoodTillDate, 100, 10, deadline)
	if res := e.Process(o); res.RejectionReason != nil {
		t.Fatalf("GTD order rejected: %v", res.RejectionReason)
	}

	clk.add(29 * time.Minute)
	e.ExpireDue()
	if e.OrderCount() != 1 {
		t.Fatal("the order left before its deadline")
	}
	clk.add(2 * time.Minute)
	e.ExpireDue()
	if e.OrderCount() != 0 {
		t.Errorf("book holds %d after the deadline passed", e.OrderCount())
	}
}

// TestExpiredDeadlinesAreRefused — accepting an order that is already dead and then
// cancelling it on the next command is a confusing accept-then-cancel for something
// that was never viable.
func TestExpiredDeadlinesAreRefused(t *testing.T) {
	start := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: start}
	e := expiryEngine(t, clk, start.Add(time.Hour))

	past := e.Process(datedOrder(t, "mm", types.TIFGoodTillDate, 100, 10, start.Add(-time.Minute)))
	if !errors.Is(past.RejectionReason, types.ErrInvalidExpiry) {
		t.Errorf("past deadline: err = %v, want ErrInvalidExpiry", past.RejectionReason)
	}
	missing := e.Process(datedOrder(t, "mm", types.TIFGoodTillDate, 100, 10, time.Time{}))
	if !errors.Is(missing.RejectionReason, types.ErrInvalidExpiry) {
		t.Errorf("missing deadline: err = %v, want ErrInvalidExpiry", missing.RejectionReason)
	}
	if e.OrderCount() != 0 {
		t.Error("a dead-on-arrival order rested")
	}
}

// TestExpiryEmitsAReason — a consumer must be able to tell an expiry from a cancel the
// client issued, or a client will think the venue pulled its order.
func TestExpiryEmitsAReason(t *testing.T) {
	start := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: start}
	cfg := DefaultConfig("X")
	cfg.Clock = clk.now
	cfg.SessionClose = func() time.Time { return start.Add(time.Hour) }
	sink := &captureSink{}
	cfg.EventSink = sink
	e := NewEngine(cfg)

	e.Process(datedOrder(t, "mm", types.TIFDay, 100, 10, time.Time{}))
	clk.add(2 * time.Hour)
	e.ExpireDue()

	var found bool
	for _, ev := range sink.events {
		if ev.Kind == EventCanceled && errors.Is(ev.Reason, types.ErrOrderExpired) {
			found = true
		}
	}
	if !found {
		t.Error("no Canceled carrying ErrOrderExpired; a client cannot tell this from a cancel it did not issue")
	}
}

// TestExpiryIgnoresMinRestingTime — the anti-spoofing floor stops a client withdrawing
// size early. It must not hold an order past its own stated lifetime, which would be
// the venue inventing liquidity the client never offered.
func TestExpiryIgnoresMinRestingTime(t *testing.T) {
	start := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: start}
	cfg := DefaultConfig("X")
	cfg.Clock = clk.now
	cfg.MinRestingTime = 24 * time.Hour // far longer than the order's life
	cfg.SessionClose = func() time.Time { return start.Add(time.Hour) }
	e := NewEngine(cfg)

	e.Process(datedOrder(t, "mm", types.TIFDay, 100, 10, time.Time{}))
	clk.add(2 * time.Hour)
	e.ExpireDue()
	if e.OrderCount() != 0 {
		t.Error("the resting floor held an order past its own expiry")
	}
}

// TestExpiryHappensBeforeMatching — an expired order must not trade. If it could, the
// venue would be filling against liquidity that had already gone.
func TestExpiryHappensBeforeMatching(t *testing.T) {
	start := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: start}
	e := expiryEngine(t, clk, start.Add(time.Hour))

	maker := datedOrder(t, "mm", types.TIFDay, 100, 10, time.Time{})
	maker.Side = types.SideSell
	e.Process(maker)

	// Past the close, then a buyer arrives that would otherwise cross.
	clk.add(2 * time.Hour)
	taker, err := types.NewOrder("tk", "X", types.SideBuy, types.OrderTypeLimit, 100, 10, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(taker)
	if len(res.Trades) != 0 {
		t.Errorf("%d trades against an expired order", len(res.Trades))
	}
}

// TestExpirySchedulesDoNotLeak — the heap holds entries for orders that have since
// filled or been cancelled, which is fine, but they must drain rather than accumulate.
func TestExpirySchedulesDoNotLeak(t *testing.T) {
	start := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: start}
	e := expiryEngine(t, clk, start.Add(time.Hour))

	var made []*types.Order
	for i := 0; i < 50; i++ {
		o := datedOrder(t, "mm", types.TIFDay, int64(100-i), 1, time.Time{})
		e.Process(o)
		made = append(made, o)
	}
	if got := e.PendingExpiries(); got != 50 {
		t.Fatalf("%d scheduled, want 50", got)
	}
	// Cancel half by hand; their schedule entries are now stale.
	for _, o := range made[:25] {
		if _, err := e.Cancel(o.ID, "mm"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
	}
	clk.add(2 * time.Hour)
	e.ExpireDue()
	if got := e.PendingExpiries(); got != 0 {
		t.Errorf("%d deadlines still scheduled after everything expired", got)
	}
	if e.OrderCount() != 0 {
		t.Errorf("book holds %d orders", e.OrderCount())
	}
}

// TestGTCIsUnaffected — the default must not acquire a deadline by accident.
func TestGTCIsUnaffected(t *testing.T) {
	start := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: start}
	e := expiryEngine(t, clk, start.Add(time.Hour))

	o := datedOrder(t, "mm", types.TIFGoodTillCancel, 100, 10, time.Time{})
	e.Process(o)
	clk.add(48 * time.Hour)
	e.ExpireDue()
	if e.OrderCount() != 1 {
		t.Error("a GTC order expired")
	}
	if e.PendingExpiries() != 0 {
		t.Error("a GTC order was scheduled for expiry")
	}
}
