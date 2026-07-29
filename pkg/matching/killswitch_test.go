package matching

import (
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func ksOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestCancelAllForUserPullsEveryOrderClass — an operator pulling a participant
// must pull all of it. A trailing stop lives only in the engine's map and a
// pending stop only in the stop book, so a kill switch that walks the order book
// alone leaves live exposure behind.
func TestCancelAllForUserPullsEveryOrderClass(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	seedWithLastPrice(t, e) // other participants' liquidity

	e.Process(ksOrder(t, "bad", types.SideBuy, 94, 5))
	e.Process(ksOrder(t, "bad", types.SideSell, 130, 5))
	e.ProcessStop(stopOrder(t, "bad", types.SideSell, 3, 80))
	ts, err := types.NewTrailingStop(marketOrder(t, "bad", types.SideSell, 4), 50)
	if err != nil {
		t.Fatalf("NewTrailingStop: %v", err)
	}
	e.ProcessTrailingStop(ts)

	otherBefore := 0
	for _, o := range e.Book().Orders() {
		if o.UserID != "bad" {
			otherBefore++
		}
	}

	got := e.CancelAllForUser("bad")
	if len(got) != 4 {
		t.Errorf("cancelled %d orders, want 4 (two resting, one stop, one trailing)", len(got))
	}
	for _, o := range e.Book().Orders() {
		if o.UserID == "bad" {
			t.Errorf("order %d for the killed account is still resting", o.ID)
		}
	}
	if e.PendingStopCount() != 0 {
		t.Errorf("pending stops = %d, want 0", e.PendingStopCount())
	}
	if e.TrailingStopCount() != 0 {
		t.Errorf("trailing stops = %d, want 0", e.TrailingStopCount())
	}

	otherAfter := 0
	for _, o := range e.Book().Orders() {
		if o.UserID != "bad" {
			otherAfter++
		}
	}
	if otherAfter != otherBefore {
		t.Errorf("other participants' orders changed from %d to %d", otherBefore, otherAfter)
	}
}

// TestCancelAllForUserIgnoresMinRestingTime — an anti-spoofing floor that blocked
// an operator from pulling a book would be a liability, not a control.
func TestCancelAllForUserIgnoresMinRestingTime(t *testing.T) {
	cfg := DefaultConfig("X")
	cfg.MinRestingTime = time.Hour
	e := NewEngine(cfg)

	o := ksOrder(t, "bad", types.SideBuy, 100, 5)
	e.Process(o)

	// A normal cancel is refused this soon.
	if _, err := e.Cancel(o.ID, "bad"); err != types.ErrCancelTooSoon {
		t.Fatalf("ordinary cancel = %v, want ErrCancelTooSoon (setup no longer exercises the case)", err)
	}
	if got := e.CancelAllForUser("bad"); len(got) != 1 {
		t.Errorf("kill switch cancelled %d orders, want 1 — it must not honour MinRestingTime", len(got))
	}
	if e.Book().Count() != 0 {
		t.Errorf("book still holds %d orders", e.Book().Count())
	}
}

// TestCancelAllForUserAnnouncesEveryRemoval keeps the kill switch consistent with
// the L3 contract: a consumer that is not told cannot reconcile.
func TestCancelAllForUserAnnouncesEveryRemoval(t *testing.T) {
	sink := &ocoSink{}
	cfg := DefaultConfig("X")
	cfg.EventSink = sink
	e := NewEngine(cfg)

	a := ksOrder(t, "bad", types.SideBuy, 100, 5)
	b := ksOrder(t, "bad", types.SideBuy, 99, 5)
	e.Process(a)
	e.Process(b)

	e.CancelAllForUser("bad")

	for _, id := range []int64{a.ID, b.ID} {
		if !sink.sawOrder(EventCanceled, id) {
			t.Errorf("no Canceled event for order %d", id)
		}
	}
}

// TestRunnerCancelAllForUser drives it through the concurrency front, which is
// where an operator actually reaches it.
func TestRunnerCancelAllForUser(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	defer r.Close()

	r.Process(ksOrder(t, "bad", types.SideBuy, 100, 5))
	r.Process(ksOrder(t, "bad", types.SideBuy, 99, 5))
	r.Process(ksOrder(t, "good", types.SideBuy, 98, 5))

	got, err := r.CancelAllForUser("bad")
	if err != nil {
		t.Fatalf("CancelAllForUser: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("cancelled %d, want 2", len(got))
	}
	if r.OrderCount() != 1 {
		t.Errorf("book holds %d orders, want 1 (the untouched account)", r.OrderCount())
	}
}
