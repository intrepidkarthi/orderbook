package matching

import (
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// capsCfg enables a max-quantity cap so a replayed oversize order has something
// to be rejected by.
func capsCfg() Config {
	c := DefaultConfig("X")
	c.MaxOrderQty = 100
	return c
}

// TestReplayStillRejectsOversizeOrders is the regression for a divergence that
// produced no error at all. The command log is written write-ahead, so it records
// commands as submitted — including ones the live engine rejected. Replay used to
// bypass the caps wholesale, so a live-rejected order rested on the recovered
// book and recovery silently produced a different venue.
func TestReplayStillRejectsOversizeOrders(t *testing.T) {
	oversize := func() *types.Order {
		o, err := types.NewOrder("u1", "X", types.SideBuy, types.OrderTypeLimit, 100, 500, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		return o
	}

	live := NewEngine(capsCfg())
	if got := live.Process(oversize()).RejectionReason; got != types.ErrOrderExceedsMaxQty {
		t.Fatalf("live rejection = %v, want %v", got, types.ErrOrderExceedsMaxQty)
	}

	replayed := NewEngine(capsCfg())
	replayed.SetReplaying(true)
	got := replayed.Process(oversize()).RejectionReason
	replayed.SetReplaying(false)

	if got != types.ErrOrderExceedsMaxQty {
		t.Errorf("replay rejection = %v, want %v", got, types.ErrOrderExceedsMaxQty)
	}
	if c := replayed.Book().Count(); c != live.Book().Count() {
		t.Errorf("recovered book has %d resting orders, live had %d — replay diverged", c, live.Book().Count())
	}
}

// TestReplayStillRejectsNotionalOverflow pins the arithmetic invariant. Unlike
// the caps this is not ingress policy: a corrupt or hand-edited log entry must
// not be able to replay a notional that wraps int64 into the book.
func TestReplayStillRejectsNotionalOverflow(t *testing.T) {
	huge := func() *types.Order {
		o, err := types.NewOrder("u1", "X", types.SideBuy, types.OrderTypeLimit,
			1<<40, 1<<40, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		return o
	}

	e := NewEngine(DefaultConfig("X"))
	e.SetReplaying(true)
	got := e.Process(huge()).RejectionReason
	e.SetReplaying(false)

	if got != types.ErrNotionalOverflow {
		t.Errorf("replay rejection = %v, want %v", got, types.ErrNotionalOverflow)
	}
	if c := e.Book().Count(); c != 0 {
		t.Errorf("an overflowing order rested during replay (%d resting)", c)
	}
}

// TestReplayStillRejectsDuplicates ties this to the guard restored in T1.1: the
// duplicate check is deterministic, so replay must reach the same verdict the
// live engine did.
func TestReplayStillRejectsDuplicates(t *testing.T) {
	cfg := DefaultConfig("X")
	cfg.DedupClientOrderIDs = 64

	mk := func() *types.Order {
		o, err := types.NewOrder("u1", "X", types.SideBuy, types.OrderTypeLimit, 100, 1, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		o.ClientOrderID = "cid-1"
		return o
	}

	e := NewEngine(cfg)
	e.SetReplaying(true)
	e.Process(mk())
	got := e.Process(mk()).RejectionReason
	e.SetReplaying(false)

	if got != types.ErrDuplicateClientOrderID {
		t.Errorf("duplicate during replay: rejection = %v, want %v", got, types.ErrDuplicateClientOrderID)
	}
	if c := e.Book().Count(); c != 1 {
		t.Errorf("book has %d orders, want 1 — a duplicate double-booked during replay", c)
	}
}

// TestReplayStillSuppressesMinRestingTime is the other half of the split: the
// time-dependent controls must STAY suppressed, or replaying a cancel against
// replay-time timestamps rejects a cancel the live engine accepted.
func TestReplayStillSuppressesMinRestingTime(t *testing.T) {
	cfg := DefaultConfig("X")
	cfg.MinRestingTime = time.Hour

	e := NewEngine(cfg)
	e.SetReplaying(true)
	defer e.SetReplaying(false)

	o, err := types.NewOrder("u1", "X", types.SideBuy, types.OrderTypeLimit, 100, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(o)

	if _, err := e.Cancel(o.ID, "u1"); err != nil {
		t.Errorf("cancel during replay returned %v; minimum resting time must stay suppressed", err)
	}
}
