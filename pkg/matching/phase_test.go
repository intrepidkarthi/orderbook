package matching

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Trading phases and the opening uncross.
//
// The engine held one crossed-book invariant everywhere: a bid above an ask is a bug.
// Pre-open is the single deliberate exception — orders accumulate unmatched and are
// resolved at one price — so these tests are mostly about that exception being
// contained: crossed while pre-open, never crossed once open.

func phaseOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestPreOpenRestsWithoutMatching is the behaviour that makes an auction possible.
func TestPreOpenRestsWithoutMatching(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	e.SetPhase(StatePreOpen)

	res := e.Process(phaseOrder(t, "a", types.SideSell, 100, 10))
	if res.RejectionReason != nil {
		t.Fatalf("pre-open rejected a limit order: %v", res.RejectionReason)
	}
	// A buy above the resting ask. In continuous trading this trades; in pre-open it
	// rests, and the book is legitimately crossed.
	res = e.Process(phaseOrder(t, "b", types.SideBuy, 110, 10))
	if res.RejectionReason != nil {
		t.Fatalf("pre-open rejected a crossing limit order: %v", res.RejectionReason)
	}
	if len(res.Trades) != 0 {
		t.Errorf("%d trades in pre-open; nothing should match", len(res.Trades))
	}
	if e.OrderCount() != 2 {
		t.Fatalf("book holds %d, want both orders resting", e.OrderCount())
	}
	bid, _, _ := e.BestBid()
	ask, _, _ := e.BestAsk()
	if bid <= ask {
		t.Errorf("book is not crossed (bid %d, ask %d); pre-open is supposed to allow it", bid, ask)
	}
}

// TestPreOpenRefusesMarketOrders — an unpriced order has nothing to rest at, and
// holding it to execute at whatever the auction decides is not what it asked for.
func TestPreOpenRefusesMarketOrders(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	e.SetPhase(StatePreOpen)

	o, err := types.NewOrder("a", "X", types.SideBuy, types.OrderTypeMarket, 0, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if res := e.Process(o); res.RejectionReason == nil {
		t.Error("pre-open accepted a market order")
	}
	if e.OrderCount() != 0 {
		t.Error("a market order rested in pre-open")
	}
}

// TestUncrossClearsAtOnePrice is the definition of a call auction: everyone who trades
// trades at the same price, whatever they were individually willing to pay.
func TestUncrossClearsAtOnePrice(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	e.SetPhase(StatePreOpen)

	e.Process(phaseOrder(t, "s1", types.SideSell, 100, 5))
	e.Process(phaseOrder(t, "s2", types.SideSell, 102, 5))
	e.Process(phaseOrder(t, "b1", types.SideBuy, 105, 5))
	e.Process(phaseOrder(t, "b2", types.SideBuy, 103, 5))

	trades := e.SetPhase(StateOpen)
	if len(trades) == 0 {
		t.Fatal("the uncross produced no trades from a crossed book")
	}
	price := trades[0].Price
	var volume int64
	for _, tr := range trades {
		if tr.Price != price {
			t.Errorf("trade at %d, want every print at the single clearing price %d", tr.Price, price)
		}
		if !tr.Auction {
			t.Error("an uncross print is not marked as an auction trade")
		}
		volume += tr.Quantity
	}
	if volume != 10 {
		t.Errorf("uncrossed %d lots, want 10 (both sells meet both buys)", volume)
	}
	// And the venue must not open onto a crossed book.
	bid, _, hasBid := e.BestBid()
	ask, _, hasAsk := e.BestAsk()
	if hasBid && hasAsk && bid >= ask {
		t.Errorf("the book is still crossed after opening: bid %d, ask %d", bid, ask)
	}
}

// TestUncrossRespectsPriceTimePriority — a venue that allocated its auction
// differently from its continuous session would be two venues.
func TestUncrossRespectsPriceTimePriority(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	e.SetPhase(StatePreOpen)

	// Two sells at the same price; the earlier one must fill first.
	first := phaseOrder(t, "s1", types.SideSell, 100, 5)
	second := phaseOrder(t, "s2", types.SideSell, 100, 5)
	e.Process(first)
	e.Process(second)
	// Only enough buying for one of them.
	e.Process(phaseOrder(t, "b1", types.SideBuy, 100, 5))

	trades := e.SetPhase(StateOpen)
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	if trades[0].SellOrderID != first.ID {
		t.Errorf("filled order %d, want the earlier %d — time priority was not respected",
			trades[0].SellOrderID, first.ID)
	}
	if second.RemainingQty != 5 {
		t.Errorf("the later order filled %d lots", 5-second.RemainingQty)
	}
}

// TestUncrossOnAnUncrossedBookDoesNothing — the transition must be safe to make when
// nobody happened to cross.
func TestUncrossOnAnUncrossedBookDoesNothing(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	e.SetPhase(StatePreOpen)
	e.Process(phaseOrder(t, "s", types.SideSell, 110, 5))
	e.Process(phaseOrder(t, "b", types.SideBuy, 100, 5))

	if trades := e.SetPhase(StateOpen); len(trades) != 0 {
		t.Errorf("%d trades from an uncrossed book", len(trades))
	}
	if e.OrderCount() != 2 {
		t.Errorf("book holds %d, want both orders still resting", e.OrderCount())
	}
}

// TestUncrossKeepsLevelTotalsHonest — the level aggregate is separate bookkeeping from
// the orders, and an auction fills orders through a different path than continuous
// matching. If that path forgets the level, Snapshot lies, which is the bug v0.14.0
// fixed on the continuous side.
func TestUncrossKeepsLevelTotalsHonest(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	e.SetPhase(StatePreOpen)

	e.Process(phaseOrder(t, "s1", types.SideSell, 100, 5))
	e.Process(phaseOrder(t, "s2", types.SideSell, 100, 5))
	e.Process(phaseOrder(t, "s3", types.SideSell, 100, 5))
	e.Process(phaseOrder(t, "b1", types.SideBuy, 100, 7)) // partial across the queue

	e.SetPhase(StateOpen)

	snap := e.Snapshot(1 << 20)
	var levelTotal int64
	for _, l := range snap.Asks {
		levelTotal += l.Quantity
	}
	var orderTotal int64
	for _, o := range e.RestingOrders() {
		if o.Side == types.SideSell {
			orderTotal += o.RemainingQty
		}
	}
	if levelTotal != orderTotal {
		t.Errorf("ask levels total %d, resting sell orders total %d — the uncross did not keep the level in step",
			levelTotal, orderTotal)
	}
}

// TestClosedRefusesNewLiquidityButAllowsCancels — a participant must be able to clear
// its book after the session, or it carries the position to the next one unwillingly.
func TestClosedRefusesNewLiquidityButAllowsCancels(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	resting := phaseOrder(t, "a", types.SideBuy, 100, 5)
	e.Process(resting)

	e.SetPhase(StateClosed)
	if res := e.Process(phaseOrder(t, "a", types.SideBuy, 99, 5)); !errors.Is(res.RejectionReason, types.ErrNewOrdersHalted) {
		t.Errorf("closed venue: err = %v, want ErrNewOrdersHalted", res.RejectionReason)
	}
	if _, err := e.Cancel(resting.ID, "a"); err != nil {
		t.Errorf("a closed venue refused a cancel: %v", err)
	}
}

// TestPhaseTransitionsAnnounceThemselves — a phase change decides whether anyone can
// trade, so it cannot be something a consumer has to poll for.
func TestPhaseTransitionsAnnounceThemselves(t *testing.T) {
	cfg := DefaultConfig("X")
	sink := &captureSink{}
	cfg.EventSink = sink
	e := NewEngine(cfg)

	e.SetPhase(StatePreOpen)
	e.SetPhase(StateOpen)
	e.SetPhase(StateClosed)

	var states int
	for _, ev := range sink.events {
		if ev.Kind == EventHalted || ev.Kind == EventResumed || ev.Kind == EventCancelOnly {
			states++
		}
	}
	if states < 3 {
		t.Errorf("saw %d state events across three transitions", states)
	}
	// Re-asserting the same phase must announce nothing: an event describing no change
	// is worse than none.
	before := len(sink.events)
	e.SetPhase(StateClosed)
	if len(sink.events) != before {
		t.Error("re-asserting the current phase produced an event")
	}
}

// TestSetPhaseThroughTheRunner — the transition is ordered against the order flow, so
// an order submitted just before a pre-open closes is either in the auction or after
// it, never ambiguous.
func TestSetPhaseThroughTheRunner(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	defer r.Close()

	r.SetPhase(StatePreOpen)
	if r.Phase() != StatePreOpen {
		t.Fatalf("phase is %v, want pre-open", r.Phase())
	}
	r.Process(phaseOrder(t, "s", types.SideSell, 100, 5))
	r.Process(phaseOrder(t, "b", types.SideBuy, 100, 5))

	trades := r.SetPhase(StateOpen)
	if len(trades) != 1 {
		t.Fatalf("got %d trades from the opening uncross, want 1", len(trades))
	}
	if !trades[0].Auction {
		t.Error("the print is not marked as an auction trade")
	}
	if r.Phase() != StateOpen {
		t.Errorf("phase is %v, want open", r.Phase())
	}
	if r.OrderCount() != 0 {
		t.Errorf("book holds %d after a full uncross", r.OrderCount())
	}
}

// TestContinuousTradingIsUnchanged — the default phase must behave exactly as before,
// or every existing venue changes behaviour on upgrade.
func TestContinuousTradingIsUnchanged(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	if e.State() != StateOpen {
		t.Fatalf("a fresh engine starts in %v, want open", e.State())
	}
	e.Process(phaseOrder(t, "s", types.SideSell, 100, 5))
	res := e.Process(phaseOrder(t, "b", types.SideBuy, 100, 5))
	if len(res.Trades) != 1 {
		t.Fatalf("got %d trades, want 1 — continuous matching changed", len(res.Trades))
	}
	if res.Trades[0].Auction {
		t.Error("a continuous trade is marked as an auction print")
	}
}
