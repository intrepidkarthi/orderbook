package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func redOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestReduceKeepsQueuePosition is the entire reason this operation exists. If it
// did not hold, a gateway could implement the same thing with cancel-then-new and
// the engine would not need to expose anything.
func TestReduceKeepsQueuePosition(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))

	first := redOrder(t, "mm", types.SideBuy, 100, 10)
	second := redOrder(t, "other", types.SideBuy, 100, 10)
	e.Process(first)
	e.Process(second)

	if _, err := e.Reduce(first.ID, 4, "mm"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}

	// A seller taking 4 must hit the reduced order first, not the one behind it.
	res := e.Process(redOrder(t, "taker", types.SideSell, 100, 4))
	if len(res.Trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(res.Trades))
	}
	if got := res.Trades[0].MakerOrderID; got != first.ID {
		t.Errorf("traded against order %d, want %d — the reduced order lost its place in the queue", got, first.ID)
	}
}

// TestReduceUpdatesAggregateDepth — level depth must equal the sum of its orders,
// or every market-data consumer sees size that is not there.
func TestReduceUpdatesAggregateDepth(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)

	if _, _, ok := e.BestBid(); !ok {
		t.Fatal("no bid")
	}
	if _, err := e.Reduce(o.ID, 3, "mm"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	_, qty, ok := e.BestBid()
	if !ok {
		t.Fatal("bid vanished after reduce")
	}
	if qty != 3 {
		t.Errorf("aggregate depth = %d, want 3", qty)
	}
	if o.RemainingQty != 3 {
		t.Errorf("order remaining = %d, want 3", o.RemainingQty)
	}
}

// TestReduceAfterPartialFill — newQty is the new total, so an order that has
// already filled 4 of 10 and reduces to 6 has 2 left.
func TestReduceAfterPartialFill(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)
	e.Process(redOrder(t, "taker", types.SideSell, 100, 4))

	if o.FilledQty != 4 {
		t.Fatalf("setup: filled %d, want 4", o.FilledQty)
	}
	if _, err := e.Reduce(o.ID, 6, "mm"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if o.RemainingQty != 2 {
		t.Errorf("remaining = %d, want 2 (6 total minus 4 already filled)", o.RemainingQty)
	}
}

// TestReduceRejectsIncreaseAndPriceGames — growing in place would let a
// participant reserve a place in the queue and then claim it.
func TestReduceRejectsIncrease(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)

	for _, q := range []int64{10, 11, 0, -1} {
		if _, err := e.Reduce(o.ID, q, "mm"); err != types.ErrInvalidQuantity {
			t.Errorf("Reduce to %d: err = %v, want ErrInvalidQuantity", q, err)
		}
	}
	if o.Quantity != 10 {
		t.Errorf("quantity changed to %d despite a rejected reduce", o.Quantity)
	}
}

// TestReduceBelowFilledIsRejected — clamping silently would leave the caller's
// model of the order wrong with no way to notice.
func TestReduceBelowFilledIsRejected(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)
	e.Process(redOrder(t, "taker", types.SideSell, 100, 6))

	if _, err := e.Reduce(o.ID, 5, "mm"); err != types.ErrInvalidQuantity {
		t.Errorf("reduce below filled quantity: err = %v, want ErrInvalidQuantity", err)
	}
}

// TestReduceRejectsAnotherAccount, and does so indistinguishably from a missing
// order: a probe must not be able to learn that someone else's order exists.
func TestReduceRejectsAnotherAccount(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)

	_, errOther := e.Reduce(o.ID, 5, "someone-else")
	_, errMissing := e.Reduce(999999, 5, "someone-else")
	if errOther != types.ErrOrderNotFound {
		t.Errorf("reducing another account's order: err = %v, want ErrOrderNotFound", errOther)
	}
	if errOther != errMissing {
		t.Errorf("a probe can distinguish an existing order (%v) from a missing one (%v)", errOther, errMissing)
	}
	if o.Quantity != 10 {
		t.Error("another account's reduce changed the order")
	}
}

// TestReduceAnnouncesReplaced — a consumer must learn the new size, since no
// trade explains the change.
func TestReduceAnnouncesReplaced(t *testing.T) {
	sink := &ocoSink{}
	cfg := DefaultConfig("X")
	cfg.EventSink = sink
	e := NewEngine(cfg)

	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)
	if _, err := e.Reduce(o.ID, 4, "mm"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if !sink.sawOrder(EventReplaced, o.ID) {
		t.Error("no Replaced event; a consumer's size accounting would stay wrong")
	}
}

// TestRunnerReduce drives it through the concurrency front.
func TestRunnerReduce(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	defer r.Close()

	res := r.Process(redOrder(t, "mm", types.SideBuy, 100, 10))
	if res == nil || res.Order == nil {
		t.Fatal("submit failed")
	}
	got, err := r.Reduce(res.Order.ID, 4, "mm")
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if got.Quantity != 4 {
		t.Errorf("quantity = %d, want 4", got.Quantity)
	}
	if _, qty, _ := r.BestBid(); qty != 4 {
		t.Errorf("aggregate depth = %d, want 4", qty)
	}
}
