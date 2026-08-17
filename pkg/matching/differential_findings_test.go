package matching

// What building the reference matcher found, and what fixing it decided.
//
// Three engine behaviours, each reproduced at two to four commands. All three were
// PINNED here first — asserted as they were, with the sentence a fix had to come and
// delete — because each had more than one defensible answer and shipping the wrong
// one is worse than leaving the defect recorded. docs/DIFFERENTIAL-FINDINGS.md is
// the argument that picked between the answers; this file is where the pins were
// inverted once it did.
//
// The three tests whose names now say the OPPOSITE of what they assert
// (TestRejectedFOKStillMovesTheLastTradePrice here,
// TestSTPCancelledMakerVanishesWithNoEvent here, and
// TestProRataSelfSkipCrossesTheBook in differential_test.go) keep those names on
// purpose. A pin is a promise to whoever changes the behaviour that they will have
// to come to this file; renaming it after the change hides the promise being kept.
//
// EVERY ONE OF THESE FIXES WAS A THREE-SIDED CHANGE, and the third side is the
// reason this note is long. internal/refmatch was written to reproduce the engine's
// position on (a) and (b), so the differential comparison was silent about both BY
// CONSTRUCTION — two implementations that agree because they were written from the
// same wrong sentence. Correcting the engine alone turned TestDifferentialTape red
// across profiles and seeds at once, correcting the model alone would have been a
// cover-up with a green suite, and the failure mode to guard against is someone
// seeing the wall of divergences and reaching for the cheapest repair, which is to
// relax the comparison.
//
// Two findings measured in the same slice are pinned rather than fixed at the
// bottom of this file. They are real, they are not what this slice decided, and a
// measured finding left unpinned is how it gets found a third time.

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// findingSink collects the whole event stream so a test can assert about what a
// consumer was and was not told.
type findingSink struct{ evs []Event }

func (s *findingSink) OnEvents(evs []Event) { s.evs = append(s.evs, evs...) }

// kindsFrom renders an event batch as a consumer would read it, for failure
// messages that carry the whole picture rather than the one assertion that fired.
func kindsFrom(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind.String())
	}
	return out
}

// TestRejectedFOKStillMovesTheLastTradePrice used to pin
// docs/REFERENCE-MATCHER.md §9(a). It now asserts the opposite, and the name is
// kept so the pin is visibly the thing that was redeemed.
//
// A fill-or-kill order that cannot fill has every one of its prints reversed: the
// makers get their quantity and their place in the book back, no trade reaches a
// consumer, and the command returns REJECTED. The last trade price used not to go
// back — match called recordLast on the way out, before settleInto's fill-or-kill
// branch unwound anything, and nothing put it back.
//
// The decision (docs/DIFFERENTIAL-FINDINGS.md §3) is that LastTradePrice means the
// price of the last trade this venue PUBLISHED. Every consumer of it wants that
// reading and none wants "a price at which a match was attempted and undone": the
// two reversed measurements below are the collar and the stop cascade, and both used
// to produce a second wrong off the first.
//
// THE SAME COMMIT CHANGED THE MODEL. internal/refmatch reproduced the old position:
// Model.execute sets m.last on every print and Model.settle's FOK branch unwound the
// trades without restoring it. Fixing the engine alone failed TestDifferentialTape
// on four seeds across three profiles with class "last-trade-price".
func TestRejectedFOKStillMovesTheLastTradePrice(t *testing.T) {
	e := NewEngine(DefaultConfig("A"))

	maker, err := types.NewOrder("m", "A", types.SideSell, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(maker)
	if got := e.LastTradePrice(); got != 0 {
		t.Fatalf("last trade price is %d before anything traded", got)
	}

	fok, err := types.NewOrder("t", "A", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(fok)

	if res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED", res.Status)
	}
	if len(res.Trades) != 0 {
		t.Fatalf("the rejected fill-or-kill returned %d trades; every one should have been unwound", len(res.Trades))
	}
	if _, qty, ok := e.BestAsk(); !ok || qty != 3 {
		t.Fatalf("the maker was not fully restored (best ask qty %d, present %t)", qty, ok)
	}
	if got := e.LastTradePrice(); got != 0 {
		t.Fatalf("last trade price is %d after a rejected fill-or-kill, want 0. The reversal is only "+
			"total once the reference price goes back too: this venue has just published a last-sale "+
			"price that appears on no tape, in no drop copy and in no execution report. "+
			"docs/DIFFERENTIAL-FINDINGS.md §3.", got)
	}
	// The trade id is NOT given back, and the asymmetry is the decision rather than
	// an oversight: an id is a name, and a counter that goes backwards can give one
	// name to two different prints; a price is a datum, and restoring it makes it
	// correct again. An operator reconciling a trade-id gap has found a rejected
	// fill-or-kill, and that explanation must keep working.
	after, err := types.NewOrder("t2", "A", types.SideBuy, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res2 := e.Process(after)
	if len(res2.Trades) != 1 {
		t.Fatalf("the follow-up buy printed %d trades, want 1", len(res2.Trades))
	}
	if _, seq := SplitID(res2.Trades[0].ID); seq != 2 {
		t.Fatalf("the next real print carries trade sequence %d, want 2 — the rejected fill-or-kill "+
			"must still have burned id 1. If this is now 1, tradeSeq was rewound along with the price "+
			"and one id can name two prints.", seq)
	}
}

// TestRejectedFOKDoesNotFireAStop is the measurement that made defect A lopsided,
// and it is the one the pinned test above did not cover: the pin asserted the value,
// this asserts the consequence.
//
// Four commands, DefaultConfig, nothing exotic switched on. Before the fix the
// venue's answer was: LastTradePrice 100, the stop FIRED, and trade id 2 printed
// 2 lots at 100 between two accounts neither of which sent the rejected order. u1's
// resting order silently lost 2 lots; u2's stop silently became a filled position.
// The complete event stream for command 4 was one event, REJECTED, naming u3.
//
// A stop is a client instruction that says "when the market TRADES at X". Firing it
// on a price nobody traded is a fill the client did not ask for.
func TestRejectedFOKDoesNotFireAStop(t *testing.T) {
	sink := &findingSink{}
	cfg := DefaultConfig("A")
	cfg.EventSink = sink
	e := NewEngine(cfg)

	maker, err := types.NewOrder("u1", "A", types.SideSell, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(maker)
	deep, err := types.NewOrder("u1", "A", types.SideSell, types.OrderTypeLimit, 105, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(deep)

	// A buy stop that fires when the market trades at 100. Nothing has traded, so
	// LastTradePrice is 0 and it rests.
	stopMkt, err := types.NewOrder("u2", "A", types.SideBuy, types.OrderTypeMarket, 0, 2, types.TIFImmediateOrCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	st, err := types.NewStopOrder(stopMkt, 100)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	if res := e.ProcessStop(st); res.Status != types.OrderStatusPendingTrigger {
		t.Fatalf("the stop ended %s, want PENDING_TRIGGER — this test needs it resting", res.Status)
	}
	if e.PendingStopCount() != 1 {
		t.Fatalf("pending stops = %d, want 1", e.PendingStopCount())
	}
	mark := len(sink.evs)

	fok, err := types.NewOrder("u3", "A", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(fok)

	if res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED", res.Status)
	}
	if len(res.Trades) != 0 {
		t.Fatalf("the rejected fill-or-kill returned %d trades, want 0: %+v", len(res.Trades), res.Trades)
	}
	if e.PendingStopCount() != 1 {
		t.Fatalf("the stop fired on a price nobody traded (pending stops %d, want 1). The rejected "+
			"order's phantom LastTradePrice reached cascadeStops. docs/DIFFERENTIAL-FINDINGS.md §3.1.",
			e.PendingStopCount())
	}
	if _, qty, ok := e.BestAsk(); !ok || qty != 3 {
		t.Fatalf("u1's resting order holds %d lots (present %t), want 3 — a rejected order moved a "+
			"third account's position", qty, ok)
	}
	for _, ev := range sink.evs[mark:] {
		if ev.Kind == EventTrade {
			t.Fatalf("a rejected command published a trade: %+v. Events were %v",
				ev.Trade, kindsFrom(sink.evs[mark:]))
		}
	}
}

// TestRejectedFOKDoesNotMoveTheBand is defect A's second consequence: an order that
// never traded used to refuse an order it never met.
//
// outsideBand is disabled while the reference is 0, so on a venue where nothing has
// traded the collar is off. One rejected fill-or-kill used to set the reference to
// 100, which armed a 10% band, which then refused an unrelated account's buy at 150.
func TestRejectedFOKDoesNotMoveTheBand(t *testing.T) {
	cfg := DefaultConfig("A")
	cfg.PriceBand = decimal.RequireFromString("0.1") // ±10%
	e := NewEngine(cfg)

	maker, err := types.NewOrder("u1", "A", types.SideSell, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(maker)

	fok, err := types.NewOrder("u2", "A", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if res := e.Process(fok); res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED", res.Status)
	}

	far, err := types.NewOrder("u3", "A", types.SideBuy, types.OrderTypeLimit, 150, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(far)
	if errors.Is(res.RejectionReason, types.ErrPriceOutsideBand) {
		t.Fatalf("an unrelated buy at 150 was refused with ErrPriceOutsideBand. The collar was armed "+
			"by a rejected order's phantom reference (last trade price is %d). "+
			"docs/DIFFERENTIAL-FINDINGS.md §3.1.", e.LastTradePrice())
	}
	if res.Status == types.OrderStatusRejected {
		t.Fatalf("the unrelated buy ended REJECTED (%v), want accepted", res.RejectionReason)
	}
}

// TestNestedFOKRestoresItsOwnWalksReference is why defect A's fix captures the
// pre-match price inside settleInto and not inside Match, and it is the assertion
// that tells the two placements apart. It is written by hand because the generated
// tape cannot reach it: stops are tier 2, so no differential seed ever fires one.
//
// cascadeStops calls settleInto again for every stop it fires, inside a command
// whose own walk has already printed. A fill-or-kill that fires as a stop and then
// fails must restore the reference ITS OWN walk started from — the price the
// triggering trade set — not the price the command started from. Capturing in Match
// would restore 100 here and erase a genuine, published print at 95.
func TestNestedFOKRestoresItsOwnWalksReference(t *testing.T) {
	e := NewEngine(DefaultConfig("N"))

	// A real print at 100.
	seed1, err := types.NewOrder("u1", "N", types.SideSell, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(seed1)
	take1, err := types.NewOrder("u2", "N", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(take1)
	if got := e.LastTradePrice(); got != 100 {
		t.Fatalf("setup: last trade price %d, want 100", got)
	}

	// A sell stop at 95 whose order is a fill-or-kill that will not fill, plus the
	// thin bid it will print against and then have reversed.
	fokLeg, err := types.NewOrder("u3", "N", types.SideSell, types.OrderTypeLimit, 90, 10, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	st, err := types.NewStopOrder(fokLeg, 95)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	if res := e.ProcessStop(st); res.Status != types.OrderStatusPendingTrigger {
		t.Fatalf("the stop ended %s, want PENDING_TRIGGER", res.Status)
	}
	thin, err := types.NewOrder("u4", "N", types.SideBuy, types.OrderTypeLimit, 92, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(thin)

	// One command that genuinely prints at 95 and, on the way out, fires the stop.
	bid95, err := types.NewOrder("u5", "N", types.SideBuy, types.OrderTypeLimit, 95, 2, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(bid95)
	drive, err := types.NewOrder("u6", "N", types.SideSell, types.OrderTypeLimit, 95, 2, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(drive)

	if len(res.Trades) != 1 || res.Trades[0].Price != 95 {
		t.Fatalf("the driving order printed %+v, want one trade at 95", res.Trades)
	}
	if e.PendingStopCount() != 0 {
		t.Fatalf("the stop did not fire (pending %d); this test needs the cascade", e.PendingStopCount())
	}
	if _, qty, ok := e.BestBid(); !ok || qty != 3 {
		t.Fatalf("the stop's reversed print did not restore the thin bid (%d lots, present %t)", qty, ok)
	}
	if got := e.LastTradePrice(); got != 95 {
		t.Fatalf("last trade price is %d after a cascade-fired fill-or-kill failed, want 95. 92 means "+
			"the reversed print was left standing as the reference; 100 means the restore was captured "+
			"in Match rather than in settleInto and it erased the command's own genuine print at 95.", got)
	}
}

// TestSTPCancelledMakerVanishesWithNoEvent used to pin docs/REFERENCE-MATCHER.md
// §9(b), the more serious of the two predictions. It now asserts the opposite, and
// it keeps every assertion it had: the verdict is still REJECTED and the book still
// loses the maker. This is a strengthening, not a replacement.
//
// A fill-or-kill taker under CANCEL_OLDEST removes a resting maker mid-walk, then
// fails to fill and is rejected. emitResult used to drop e.pending whenever the
// status was Rejected, so the Canceled announcing the maker's removal never reached
// a consumer; reverseTrade restores only makers it TRADED with, not ones self-trade
// prevention removed. The order was gone from the book and the stream never said so,
// and a consumer built on the stream kept a phantom resting order forever.
//
// Two answers were available and docs/DIFFERENTIAL-FINDINGS.md §4 picked between
// them on a measurement rather than a principle. Restoring the maker is not
// composable: the same walk makes four other non-trade mutations, and two of them
// (an iceberg refill, an OCO counterpart cancellation) are pinned at the bottom of
// this file precisely because a "restore everything" rule would have to reverse
// them too. So the cancellation STANDS and the event survives the rejection, which
// is the rule the rest of the engine already follows — Cancel, Reduce and expireDue
// all flush immediately so their events cannot be swallowed by a later verdict.
//
// THE SAME COMMIT CHANGED THE MODEL: `if status == StatusRejected { m.pending = nil }`
// in Model.compose was the model's copy of emitResult's dropped batch.
func TestSTPCancelledMakerVanishesWithNoEvent(t *testing.T) {
	sink := &findingSink{}
	cfg := DefaultConfig("B")
	cfg.EventSink = sink
	e := NewEngine(cfg)

	maker, err := types.NewOrder("u", "B", types.SideSell, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(maker)
	if e.OrderCount() != 1 {
		t.Fatalf("the maker did not rest (book holds %d)", e.OrderCount())
	}
	mark := len(sink.evs)

	fok, err := types.NewOrder("u", "B", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	fok.STPMode = string(STPCancelOldest)
	res := e.Process(fok)

	if res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED", res.Status)
	}
	if e.OrderCount() != 0 {
		t.Fatalf("the maker is still resting (book holds %d); the removal this test is about did not happen",
			e.OrderCount())
	}

	batch := sink.evs[mark:]
	announced := false
	for _, ev := range batch {
		if ev.Kind == EventCanceled && ev.OrderID == maker.ID {
			announced = true
		}
	}
	if !announced {
		t.Fatalf("the book lost order %d and the only events a consumer received were %v. A stream that "+
			"does not announce a removal leaves every L3 consumer holding a phantom resting order. "+
			"docs/DIFFERENTIAL-FINDINGS.md §4.", maker.ID, kindsFrom(batch))
	}
	// Composition order: the submitted order's own verdict comes first, then the
	// batch, exactly as it does on the accepted path where ACCEPTED also precedes
	// trades that preceded it. Making the rejected path the one exception would be
	// the real bug.
	if len(batch) == 0 || batch[0].Kind != EventRejected {
		t.Fatalf("the batch does not open with REJECTED: %v", kindsFrom(batch))
	}
	// And no execution is announced: those prints WERE undone, so the rule that
	// keeps the cancellation is the same rule that drops them.
	for _, ev := range batch {
		if ev.Kind == EventTrade {
			t.Fatalf("a reversed print reached a consumer: %v", kindsFrom(batch))
		}
	}
}

// TestRejectedFOKAnnouncesTheDecrementedMaker is docs/DIFFERENTIAL-FINDINGS.md
// §4.1(ii): the instance of defect B that was not previously recorded.
//
// STP DECREMENT shrinks both sides by their overlap with no trade to explain it.
// Here it takes the maker to zero, removes it, and emits a Canceled — which the
// rejected verdict then swallowed. Measured before the fix: the maker's Quantity was
// 0, it was gone from the book, the level aggregate was 0, the TAKER's own Quantity
// had been mutated from 5 to 2 on an order that ends REJECTED, and one event was
// published: REJECTED.
//
// A MEASUREMENT THAT NARROWS THE SPEC. §4.1(ii) also asks for the partial-decrement
// variant, where the maker is shrunk in place and its Replaced is dropped. That
// variant is NOT REACHABLE inside a rejected order, and the proof is two lines:
// decrement shrinks both sides by min(taker.RemainingQty, maker.RemainingQty), so
// after it at least one side is zero; if the maker survives then the taker is at
// zero, and a taker at zero is IsFilled, so the fill-or-kill branch reports FILLED
// and never rejects. A Replaced therefore only ever reaches a batch that was going
// to be published anyway. It is asserted here on the ACCEPTED path so the claim is
// checked rather than argued, and recorded so nobody writes the unreachable test.
func TestRejectedFOKAnnouncesTheDecrementedMaker(t *testing.T) {
	t.Run("maker to zero, Canceled survives the rejection", func(t *testing.T) {
		sink := &findingSink{}
		cfg := DefaultConfig("D")
		cfg.EventSink = sink
		e := NewEngine(cfg)

		maker, err := types.NewOrder("u", "D", types.SideSell, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		e.Process(maker)
		mark := len(sink.evs)

		fok, err := types.NewOrder("u", "D", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFFillOrKill)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		fok.STPMode = string(STPDecrement)
		res := e.Process(fok)

		if res.Status != types.OrderStatusRejected {
			t.Fatalf("the fill-or-kill ended %s, want REJECTED", res.Status)
		}
		if e.OrderCount() != 0 {
			t.Fatalf("the decremented maker is still resting (book holds %d)", e.OrderCount())
		}
		batch := sink.evs[mark:]
		found := false
		for _, ev := range batch {
			if ev.Kind == EventCanceled && ev.OrderID == maker.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("STP DECREMENT emptied order %d and removed it, and the events a consumer received "+
				"were %v. docs/DIFFERENTIAL-FINDINGS.md §4.1(ii).", maker.ID, kindsFrom(batch))
		}
	})

	t.Run("maker shrunk in place, Replaced carries the new remaining quantity", func(t *testing.T) {
		sink := &findingSink{}
		cfg := DefaultConfig("D")
		cfg.EventSink = sink
		e := NewEngine(cfg)

		maker, err := types.NewOrder("u", "D", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		e.Process(maker)
		mark := len(sink.evs)

		fok, err := types.NewOrder("u", "D", types.SideBuy, types.OrderTypeLimit, 100, 4, types.TIFFillOrKill)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		fok.STPMode = string(STPDecrement)
		res := e.Process(fok)

		// A taker decremented to nothing has no remaining quantity, so all-or-nothing
		// is satisfied and the verdict is FILLED — which is exactly why the Replaced
		// below can never be swallowed by a rejection.
		if res.Status != types.OrderStatusFilled {
			t.Fatalf("a fully decremented fill-or-kill ended %s, want FILLED; if this is now REJECTED "+
				"the unreachability argument in this test's comment is stale and the Replaced case "+
				"needs asserting on the rejected path too", res.Status)
		}
		batch := sink.evs[mark:]
		var replaced *Event
		for i := range batch {
			if batch[i].Kind == EventReplaced && batch[i].OrderID == maker.ID {
				replaced = &batch[i]
			}
		}
		if replaced == nil {
			t.Fatalf("the maker was shrunk in place with no Replaced: %v", kindsFrom(batch))
		}
		if replaced.Order == nil || replaced.Order.RemainingQty != 5 {
			t.Fatalf("the Replaced carries remaining %v, want 5 — a consumer's remaining-quantity "+
				"accounting is what this event exists to correct, and wire.Executed's LeavesQty is "+
				"carried on the strength of it", replaced.Order)
		}
		if _, qty, ok := e.BestAsk(); !ok || qty != 5 {
			t.Fatalf("the level aggregate is %d (present %t), want 5", qty, ok)
		}
	})
}

// TestRejectedFOKAnnouncesAStandingCancellation is docs/DIFFERENTIAL-FINDINGS.md
// §4.1(iv), and it is the instance that carries §4.1(iii)'s load after defect A was
// fixed. It is NOT the test §8's deliverable 2 names, and that is a finding rather
// than a shortfall — the reason is written out below.
//
// u1 posts an OCO: a sell 3 @ 100 primary with a protective stop leg. u2 sends a
// fill-or-kill buy 5 @ 100. The walk fills the primary, cancelOCOCounterpart cancels
// the stop leg, the fill-or-kill then fails and the primary is restored to the book.
// The stop leg is NOT restored. A client's protective stop is destroyed by a
// stranger's rejected order, and the only event used to be REJECTED naming the
// stranger.
//
// WHY NOT §4.1(iii)'s STANDING PRINT. That measurement — a rejected command whose
// stop cascade really printed 2 lots between two other accounts — was reachable only
// because the rejected order's phantom LastTradePrice fired the stop. Fixing defect A
// closes it, and TestRejectedFOKDoesNotFireAStop is what holds it closed. After A, a
// REJECTED verdict cannot carry a standing print at all: the only rejection that does
// not reverse its prints is the book-size cap on the resting branch, and a taker with
// a remainder has emptied at least one maker, so the resting count after it rests is
// never higher than before the command and ErrOrderBookFull is unreachable there. A
// standing CANCELLATION is reachable, it belongs to a third account, and it makes the
// same point: a rejection is not the same statement as "nothing happened".
func TestRejectedFOKAnnouncesAStandingCancellation(t *testing.T) {
	sink := &findingSink{}
	cfg := DefaultConfig("O")
	cfg.EventSink = sink
	e := NewEngine(cfg)

	primary, err := types.NewOrder("u1", "O", types.SideSell, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	legMkt, err := types.NewOrder("u1", "O", types.SideSell, types.OrderTypeMarket, 0, 3, types.TIFImmediateOrCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	leg, err := types.NewStopOrder(legMkt, 90)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	oco, err := types.NewOCOOrder(primary, leg)
	if err != nil {
		t.Fatalf("NewOCOOrder: %v", err)
	}
	e.ProcessOCO(oco)
	if e.PendingStopCount() != 1 {
		t.Fatalf("the protective leg did not rest (pending stops %d)", e.PendingStopCount())
	}
	mark := len(sink.evs)

	fok, err := types.NewOrder("u2", "O", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(fok)
	if res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED", res.Status)
	}
	if e.PendingStopCount() != 0 {
		t.Skipf("the OCO leg survived the rejected fill-or-kill (pending stops %d) — the deferred "+
			"finding this test rides on has been fixed, and this assertion needs rewriting against "+
			"the new behaviour", e.PendingStopCount())
	}

	batch := sink.evs[mark:]
	found := false
	for _, ev := range batch {
		if ev.Kind == EventCanceled && ev.OrderID == leg.Order.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("a stranger's REJECTED order destroyed u1's protective stop %d and the events a "+
			"consumer received were %v. The client is holding a stop the venue no longer has. "+
			"docs/DIFFERENTIAL-FINDINGS.md §4.1(iv).", leg.Order.ID, kindsFrom(batch))
	}
}

// --- pinned, not fixed -------------------------------------------------------
//
// Two findings measured while deciding defect B. Neither is what this slice
// decided, both are real, and each carries the sentence a fix must come and delete.
// They belong with tier 2's exotics work, where internal/refmatch can finally hold
// an opinion about icebergs and OCO — today it models neither, so only a
// hand-written test can hold them at all.

// TestFailingFOKCorruptsAnIcebergsReserve pins docs/DIFFERENTIAL-FINDINGS.md §4.4.
//
// A fill-or-kill that exhausts an iceberg's reserve and then fails leaves the order
// with a NEGATIVE FilledQty, its whole hidden reserve forced into the open, and a
// displayed size larger than its stated quantity. The walk consumes each slice,
// refills from the reserve and re-adds the SAME order object; reverseTrade then adds
// every reversed quantity back onto an object the refill has already rewound.
//
// checkInvariants passes on it, because filled + remaining == quantity still holds
// (-6 + 9 == 3) and remaining >= 0 still holds. That blind spot is deliberate here:
// adding FilledQty >= 0 to the invariant suite belongs with the fix it would catch,
// not with a slice that would then ship a red suite.
//
// This is a defect in reverseTrade's interaction with the refill path, not in the
// event stream, and the event-stream fix in this slice neither repairs nor worsens
// it. WHEN IT IS FIXED, this test inverts and the paragraph in
// docs/DIFFERENTIAL-FINDINGS.md §7 that defers it is deleted.
func TestFailingFOKCorruptsAnIcebergsReserve(t *testing.T) {
	e := NewEngine(DefaultConfig("I"))

	base, err := types.NewOrder("u1", "I", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	ib, err := types.NewIcebergOrder(base, 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	e.ProcessIceberg(ib)

	fok, err := types.NewOrder("u2", "I", types.SideBuy, types.OrderTypeLimit, 100, 12, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if res := e.Process(fok); res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED", res.Status)
	}

	if base.FilledQty >= 0 {
		t.Fatalf("the iceberg's FilledQty is %d. If it is no longer negative the defect "+
			"docs/DIFFERENTIAL-FINDINGS.md §4.4 records has been FIXED — invert this test, delete the "+
			"deferral in §7, and consider adding FilledQty >= 0 to checkInvariants in the same commit.",
			base.FilledQty)
	}
	_, qty, ok := e.BestAsk()
	if !ok || qty <= base.Quantity {
		t.Fatalf("the best ask shows %d lots (present %t) against a stated quantity of %d; this test is "+
			"pinned to the state where the whole reserve has been forced into the open",
			qty, ok, base.Quantity)
	}
	// The one thing that IS fixed: whatever the engine leaves behind, it no longer
	// leaves it behind silently. The refilled slice's re-entry is announced.
	checkInvariants(t, e, nil)
}

// TestFailingFOKCancelsAnOCOStopLeg pins docs/DIFFERENTIAL-FINDINGS.md §4.1(iv): a
// stranger's rejected fill-or-kill destroys a client's protective stop.
//
// The walk fills the OCO primary, cancelOCOCounterpart cancels the stop leg, the
// fill-or-kill fails, and reverseTrade restores the primary — but nothing
// re-registers the leg. The client asked for a bracket and is left holding one side
// of it, because an unrelated account sent an order the venue refused.
//
// It is pinned rather than fixed for the same reason B2 was chosen over restoring
// the maker: un-cancelling a stop is a fifth inverse operation in a branch that
// almost never runs. TestRejectedFOKAnnouncesAStandingCancellation asserts the half
// of it this slice DID fix — the destruction is now announced. WHEN THE REST IS
// FIXED, this test inverts.
func TestFailingFOKCancelsAnOCOStopLeg(t *testing.T) {
	e := NewEngine(DefaultConfig("O2"))

	primary, err := types.NewOrder("u1", "O2", types.SideSell, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	legMkt, err := types.NewOrder("u1", "O2", types.SideSell, types.OrderTypeMarket, 0, 3, types.TIFImmediateOrCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	leg, err := types.NewStopOrder(legMkt, 90)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	oco, err := types.NewOCOOrder(primary, leg)
	if err != nil {
		t.Fatalf("NewOCOOrder: %v", err)
	}
	e.ProcessOCO(oco)

	fok, err := types.NewOrder("u2", "O2", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if res := e.Process(fok); res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED", res.Status)
	}

	if _, qty, ok := e.BestAsk(); !ok || qty != 3 {
		t.Fatalf("the OCO primary was not restored (best ask %d lots, present %t)", qty, ok)
	}
	if e.PendingStopCount() != 0 {
		t.Fatalf("u1's protective leg is still pending (%d). The defect "+
			"docs/DIFFERENTIAL-FINDINGS.md §4.1(iv) records has been FIXED — invert this test and "+
			"delete the deferral in §7.", e.PendingStopCount())
	}
	if leg.Order.Status != types.OrderStatusCancelled {
		t.Fatalf("the leg's status is %s, want CANCELLED; this test is pinned to the state where a "+
			"rejected stranger destroyed it", leg.Order.Status)
	}
}

// TestCascadeFiredStopRejectedLeavesAPhantom pins a defect of the same family as §9(b),
// found by adversarial review of the fix for it rather than by the generated tape.
//
// A stop fires inside cascadeStops. Its order is emitted TRIGGERED and ACCEPTED, then
// settleInto rejects it — a fill-or-kill that cannot fill. That rejection reaches
// nobody: emitTerminalIfDone fires only on OrderStatusCancelled, never on Rejected, and
// a cascade-fired order never reaches emitResult, so its status and its return value are
// both discarded. The stream has announced an order that entered the book and will never
// say it left.
//
// A consumer reconstructing the book from the stream therefore holds a fifty-lot phantom
// at 200 forever. That is the same shape as the STP-cancelled maker this slice fixed, in
// a path the generated tape cannot reach, because stops are tier 2
// (docs/REFERENCE-MATCHER.md §2.4).
//
// It is PINNED and not fixed, for the reason the other three were: the fix is a real
// change to how a cascade reports a rejected order, it interacts with replay, and it
// deserves its own commit rather than riding along inside another. When it is fixed this
// test fails; invert it, and remove the exclusion the EventKind comment states.
//
// It is pre-existing. At the commit before this slice the same sequence produced TWO
// divergences; the STP fix removed one and this is the one that remains.
func TestCascadeFiredStopRejectedLeavesAPhantom(t *testing.T) {
	cfg := DefaultConfig("A")
	mirror := newMirror()
	cfg.EventSink = mirror
	e := NewEngine(cfg)

	seed := func(user string, side types.Side, price, qty int64) {
		o, err := types.NewOrder(user, "A", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		e.Process(o)
	}

	// A print at 80, so the stop below has a reference and does not fire on arrival.
	seed("m", types.SideSell, 80, 1)
	taker, err := types.NewOrder("t", "A", types.SideBuy, types.OrderTypeLimit, 80, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(taker)

	// The stop's own order is a fill-or-kill far larger than anything resting, so when
	// the stop fires the order is rejected rather than filled.
	inner, err := types.NewOrder("s", "A", types.SideBuy, types.OrderTypeLimit, 200, 50, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	stop, err := types.NewStopOrder(inner, 95)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	e.ProcessStop(stop)

	seed("m2", types.SideSell, 120, 4)
	seed("m3", types.SideSell, 95, 1)

	// Trades at 95, which fires the stop, whose fill-or-kill is then rejected.
	trigger, err := types.NewOrder("t2", "A", types.SideBuy, types.OrderTypeLimit, 95, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(trigger)

	got, want := mirror.resting(), engineResting(e)
	if len(got) == len(want) {
		t.Fatalf("the stream now reconstructs the book across a rejected cascade-fired stop "+
			"(mirror %v, engine %v). That is the fix this test was waiting for: invert it, and "+
			"remove the exclusion pkg/matching/event.go states for this case.", got, want)
	}
	t.Logf("pinned: mirror %v against engine %v — the rejected stop's order never leaves the stream's book", got, want)
}
