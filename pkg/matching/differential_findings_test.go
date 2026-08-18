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
// The five tests whose names now say the OPPOSITE of what they assert
// (TestRejectedFOKStillMovesTheLastTradePrice here,
// TestSTPCancelledMakerVanishesWithNoEvent here,
// TestFailingFOKCorruptsAnIcebergsReserve here,
// TestCascadeFiredStopRejectedLeavesAPhantom here, and
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
// Findings measured but not decided are pinned rather than fixed at the bottom of
// this file. They are real, they are not what the slice that measured them decided,
// and a measured finding left unpinned is how it gets found a third time. Two of
// the original four have since been redeemed (docs/PINNED-DEFECTS.md); the OCO leg
// and the rejected taker's own fill counters are still pinned.

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
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

// --- one finding still pinned ------------------------------------------------
//
// TestFailingFOKCancelsAnOCOStopLeg below is what is left of the pair measured
// while deciding defect B. The iceberg half of it is fixed, above; the OCO half is
// real, is not what this slice decided, and carries the sentence a fix must come
// and delete. It belongs with tier 2's exotics work, where internal/refmatch can
// finally hold an opinion about OCO — today it models neither icebergs nor OCO
// (docs/REFERENCE-MATCHER.md §2.4), so only a hand-written test can hold it at all.

// TestFailingFOKCorruptsAnIcebergsReserve used to pin
// docs/DIFFERENTIAL-FINDINGS.md §4.4. It now asserts the opposite, and it keeps
// every assertion it had — the FilledQty check and the displayed-size check are
// both here, reversed — plus the fields the old test never looked at. The name is
// kept so the pin is visibly the thing that was redeemed.
//
// A fill-or-kill that exhausted an iceberg's reserve and then failed used to leave
// the order with FilledQty -6, its whole hidden reserve forced into the open, a
// displayed size three times its stated quantity, and no registration left to refill
// from. The walk consumes each visible slice and refills from the reserve, which
// resets Quantity, FilledQty and RemainingQty on the SAME order object; reverseTrade
// then added every reversed quantity back onto counters the refill had already
// rewound.
//
// The decision (docs/PINNED-DEFECTS.md §3.3) is that THE REFILL PATH OWNS THE
// REWIND. The walk saves an iceberg's whole state the first time it is about to
// trade against it and the failure branch restores it whole — slice, counters,
// status, reserve, refill counter and registry entry — rather than inverting
// anything, and the prints against a restored iceberg never reach reverseTrade at
// all. reverseTrade is unchanged: it is the shared reversal primitive, and a special
// case for one order type in it is a special case every future reversal path
// inherits.
//
// The rejected command must leave NO TRACE. That is why this asserts the exact event
// batch and the reserve's continued operation as well as the numbers: a restore that
// repairs the counters and leaves the order out of e.icebergOrders passes every
// arithmetic assertion and never refills again.
func TestFailingFOKCorruptsAnIcebergsReserve(t *testing.T) {
	sink := &findingSink{}
	cfg := DefaultConfig("I")
	cfg.EventSink = sink
	e := NewEngine(cfg)

	base, err := types.NewOrder("u1", "I", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	ib, err := types.NewIcebergOrder(base, 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	e.ProcessIceberg(ib)
	mark := len(sink.evs)

	fok, err := types.NewOrder("u2", "I", types.SideBuy, types.OrderTypeLimit, 100, 12, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if res := e.Process(fok); res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED", res.Status)
	}

	if base.FilledQty != 0 {
		t.Fatalf("the iceberg's FilledQty is %d, want 0. A negative value is the defect "+
			"docs/DIFFERENTIAL-FINDINGS.md §4.4 recorded: reverseTrade adding reversed quantity back onto "+
			"counters the refill already rewound. docs/PINNED-DEFECTS.md §3.", base.FilledQty)
	}
	if base.Quantity != 3 || base.RemainingQty != 3 {
		t.Fatalf("the restored slice is quantity %d / remaining %d, want 3 / 3", base.Quantity, base.RemainingQty)
	}
	if base.Status != types.OrderStatusNew {
		t.Fatalf("the restored slice's status is %s, want NEW — the state it was in before the "+
			"rejected command", base.Status)
	}
	if ib.Hidden != 6 {
		t.Fatalf("the hidden reserve holds %d lots, want 6. An iceberg exists to hide size, and a "+
			"stranger's rejected order emptying the reserve into the open is the whole defect", ib.Hidden)
	}
	if ib.Refills() != 0 {
		t.Fatalf("the refill counter reads %d, want 0. Under IcebergPeakJitter the counter seeds the "+
			"next slice's size, so a restore that leaves it advanced re-derives a size a watcher has "+
			"already seen", ib.Refills())
	}
	if _, registered := e.icebergOrders[base.ID]; !registered {
		t.Fatalf("order %d is no longer registered as an iceberg; the walk deleted it when the reserve "+
			"ran dry and the restore did not put it back, so it can never refill again", base.ID)
	}
	price, qty, ok := e.BestAsk()
	if !ok || price != 100 || qty != base.Quantity {
		t.Fatalf("the best ask shows %d:%d (present %t) against a stated quantity of %d; the level must "+
			"publish exactly what it published before the rejected command", price, qty, ok, base.Quantity)
	}
	if e.OrderCount() != 1 {
		t.Fatalf("the book holds %d orders, want 1", e.OrderCount())
	}
	if got := e.LastTradePrice(); got != 0 {
		t.Fatalf("last trade price is %d after a rejected fill-or-kill, want 0", got)
	}
	batch := sink.evs[mark:]
	if len(batch) != 1 || batch[0].Kind != EventRejected {
		t.Fatalf("the rejected command published %v, want exactly [REJECTED]. The two extra ACCEPTEDs "+
			"are the refills, and a refill that has been undone must not be announced: they carry the "+
			"restored quantity, so a mirror agrees with the engine and nothing else in the tree "+
			"notices. docs/PINNED-DEFECTS.md §11 row 4.", kindsFrom(batch))
	}
	checkInvariants(t, e, nil)

	// And the reserve must still WORK. This is the assertion a restore that repairs
	// the numbers and leaves the order deregistered fails, and it is the only one.
	after, err := types.NewOrder("u3", "I", types.SideBuy, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(after)
	if len(res.Trades) != 1 || res.Trades[0].Quantity != 3 {
		t.Fatalf("the follow-up buy printed %+v, want one trade of 3", res.Trades)
	}
	if ib.Hidden != 3 || ib.Refills() != 1 {
		t.Fatalf("after the restored slice was consumed the reserve holds %d with %d refills, want 3 "+
			"and 1 — the restored iceberg did not reload", ib.Hidden, ib.Refills())
	}
	if _, qty, ok := e.BestAsk(); !ok || qty != 3 {
		t.Fatalf("the reloaded slice shows %d lots (present %t), want 3", qty, ok)
	}
	checkInvariants(t, e, nil)
}

// TestFailingFOKRestoresAPartiallyConsumedIcebergSlice is docs/PINNED-DEFECTS.md
// §3.1's second reproduction, and it is the one that moves the LEVEL AGGREGATE
// independently of the order.
//
// The walk here inherits a slice somebody else has already taken a lot out of, and
// it is stopped by self-trade prevention rather than by an empty book. That makes
// the restore a remove-then-add rather than a write: the node resting under the
// iceberg's id contributes its own quantity to its level's total (the book's
// `contributed` field, deliberately not order.RemainingQty), so a restore that
// writes the saved counters onto the order in place leaves the order reading 2 lots
// and its level publishing 3.
//
// That half-fix passes every other assertion in this tree — measured — which is what
// makes this test load-bearing rather than a second copy of the one above.
func TestFailingFOKRestoresAPartiallyConsumedIcebergSlice(t *testing.T) {
	e := NewEngine(DefaultConfig("I2"))

	base, err := types.NewOrder("u1", "I2", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	ib, err := types.NewIcebergOrder(base, 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	e.ProcessIceberg(ib)

	// One lot out of the displayed slice, so the state the walk saves is a PARTIALLY
	// consumed one that the walk itself did not create.
	nibble, err := types.NewOrder("u3", "I2", types.SideBuy, types.OrderTypeLimit, 100, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(nibble)

	// u2's own liquidity behind the iceberg, so u2's fill-or-kill is stopped by
	// self-trade prevention with the refilled slice still resting.
	behind, err := types.NewOrder("u2", "I2", types.SideSell, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(behind)

	if base.Quantity != 3 || base.FilledQty != 1 || base.RemainingQty != 2 {
		t.Fatalf("setup: the slice is %d/%d/%d, want quantity 3 filled 1 remaining 2",
			base.Quantity, base.FilledQty, base.RemainingQty)
	}
	if _, qty, ok := e.BestAsk(); !ok || qty != 7 {
		t.Fatalf("setup: the ask level publishes %d (present %t), want 7", qty, ok)
	}

	fok, err := types.NewOrder("u2", "I2", types.SideBuy, types.OrderTypeLimit, 100, 20, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	fok.STPMode = string(STPCancelNewest)
	if res := e.Process(fok); res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED — this test needs the rejected path", res.Status)
	}

	if base.Quantity != 3 || base.FilledQty != 1 || base.RemainingQty != 2 {
		t.Fatalf("the restored slice is %d/%d/%d, want quantity 3 filled 1 remaining 2 — exactly what it "+
			"held before the rejected command", base.Quantity, base.FilledQty, base.RemainingQty)
	}
	if base.Status != types.OrderStatusPartiallyFilled {
		t.Fatalf("the restored slice's status is %s, want PARTIALLY_FILLED", base.Status)
	}
	if ib.Hidden != 6 || ib.Refills() != 0 {
		t.Fatalf("the reserve holds %d with %d refills, want 6 and 0", ib.Hidden, ib.Refills())
	}
	if _, qty, ok := e.BestAsk(); !ok || qty != 7 {
		t.Fatalf("the ask level publishes %d lots (present %t), want 7. If the order reads 2 and the "+
			"level publishes one more than it should, the restore wrote the saved counters onto the "+
			"resting node instead of removing it first. docs/PINNED-DEFECTS.md §5.2(3).", qty, ok)
	}
	checkInvariants(t, e, nil)
}

// levelIDs renders a price level as a consumer of queue priority reads it: the
// resting order ids, in the order they will trade.
func levelIDs(e *Engine, side types.Side, price int64) []int64 {
	orders := e.Book().GetOrdersAtPrice(side, price)
	out := make([]int64, 0, len(orders))
	for _, o := range orders {
		out = append(out, o.ID)
	}
	return out
}

// TestFailingFOKRestoresAnIcebergAtItsOwnPlaceInTheQueue is docs/PINNED-DEFECTS.md
// §13.6, and it is the one assertion the two tests above cannot make: their iceberg
// is ALONE at its price, so any restore order at all looks right.
//
// The reversal walks a failed fill-or-kill's prints in the order they were made, and
// re-adding each consumed maker at its own print is what puts a level back in the
// order it was in. A restore that runs before that loop instead re-enters every
// iceberg it touched AHEAD of makers that were resting in FRONT of it — a client who
// sent nothing gains priority over a client who was there first, paid for by a third
// party's rejected order. It is not visible in fill counters, in level aggregates, in
// the event batch or in the fingerprint corpus, because the id SET is right and only
// the ORDER is wrong; the next print names the wrong maker.
//
// Measured before the fix at docs/PINNED-DEFECTS.md §13.6: the level came back
// [iceberg, older maker] and a one-lot buy printed against the iceberg. The engine
// before EITHER change put it back [older maker, iceberg], so this is a property the
// fix had to preserve rather than one it introduced.
func TestFailingFOKRestoresAnIcebergAtItsOwnPlaceInTheQueue(t *testing.T) {
	e := NewEngine(DefaultConfig("I3"))

	// The older maker rests FIRST, so the iceberg behind it has no claim on priority.
	first, err := types.NewOrder("u3", "I3", types.SideSell, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(first)

	base, err := types.NewOrder("u1", "I3", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	ib, err := types.NewIcebergOrder(base, 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	e.ProcessIceberg(ib)

	before := levelIDs(e, types.SideSell, 100)
	if len(before) != 2 || before[0] != first.ID || before[1] != base.ID {
		t.Fatalf("setup: the level is %v, want [%d %d]", before, first.ID, base.ID)
	}

	// 5 + 3 + 3 + 3 = 14 lots against a 20-lot fill-or-kill: everything at the level
	// is consumed, the reserve runs dry, and the order still cannot fill.
	fok, err := types.NewOrder("u2", "I3", types.SideBuy, types.OrderTypeLimit, 100, 20, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if res := e.Process(fok); res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED — this test needs the rejected path", res.Status)
	}

	after := levelIDs(e, types.SideSell, 100)
	if len(after) != 2 || after[0] != first.ID || after[1] != base.ID {
		t.Fatalf("the level came back %v, want %v. A rejected order rewrote queue priority: the "+
			"restored iceberg (%d) is now ahead of a maker (%d) that was resting in front of it, so "+
			"the next print at this price names the wrong maker. The restore must happen at the "+
			"iceberg's FIRST print inside the reversal loop, not before it. "+
			"docs/PINNED-DEFECTS.md §13.6.", after, before, base.ID, first.ID)
	}
	// The whole point of the order type, asserted in the same test as the ordering:
	// a fix that gets the queue right and leaves the reserve in the open has fixed
	// nothing. 5 + 3, not 5 + 9.
	if _, qty, ok := e.BestAsk(); !ok || qty != 8 {
		t.Fatalf("the ask level publishes %d lots (present %t), want 8 — the older maker's 5 and the "+
			"iceberg's 3-lot slice. If it reads 14 the reserve is standing in the open.", qty, ok)
	}
	if ib.Hidden != 6 {
		t.Fatalf("the hidden reserve holds %d lots, want 6", ib.Hidden)
	}
	checkInvariants(t, e, nil)

	// The consequence, stated as the venue states it: who does the next lot trade
	// against.
	next, err := types.NewOrder("u4", "I3", types.SideBuy, types.OrderTypeLimit, 100, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(next)
	if len(res.Trades) != 1 {
		t.Fatalf("the follow-up buy printed %d trades, want 1", len(res.Trades))
	}
	if res.Trades[0].MakerOrderID != first.ID {
		t.Fatalf("the next lot printed against maker %d, want %d — the order that was first in the "+
			"queue before a stranger's rejected fill-or-kill", res.Trades[0].MakerOrderID, first.ID)
	}
}

// TestRejectedFOKPreservesEveryLevelsQueueOrder is the generated half of the test
// above: the hand-written scenario names one shape, and this one says the property
// holds over shapes nobody chose.
//
// The property: after a fill-or-kill the venue REFUSED, every price level holds the
// same order ids in the same order as before the command. That is queue priority, and
// it is the one part of a reversal that no aggregate, no counter and no event batch
// can witness — the id set stays right while the order goes wrong.
//
// The generator is deliberately narrow, and each exclusion is a case where a rejected
// command legitimately changes a level: every account is distinct, so no self-trade
// prevention fires (a DECREMENT leaves an untouched maker resting mid-level while the
// reversal re-adds behind it); no OCO, whose leg a rejected fill-or-kill does cancel
// by design (TestFailingFOKCancelsAnOCOStopLeg); and FIFO allocation, because
// pro-rata prints within a level in allocation order rather than arrival order.
// Inside those bounds a refusal must be invisible.
func TestRejectedFOKPreservesEveryLevelsQueueOrder(t *testing.T) {
	// Deterministic and committed: the same 60 tapes run on every `go test`, so this
	// is a regression test and not a lottery.
	for seed := int64(1); seed <= 60; seed++ {
		rng := rand.New(rand.NewSource(seed))
		e := NewEngine(DefaultConfig("QP"))
		var refusals int

		for step := range 40 {
			price := int64(95 + rng.Intn(11))
			qty := int64(1 + rng.Intn(12))
			side := types.SideBuy
			if rng.Intn(2) == 0 {
				side = types.SideSell
			}
			user := fmt.Sprintf("m%d", step) // a distinct account per command

			switch rng.Intn(4) {
			case 0: // an iceberg, the order type whose restore this is about
				total := qty + int64(1+rng.Intn(9))
				o, err := types.NewOrder(user, "QP", side, types.OrderTypeLimit, price, total, types.TIFGoodTillCancel)
				if err != nil {
					continue
				}
				ib, err := types.NewIcebergOrder(o, qty)
				if err != nil {
					continue
				}
				e.ProcessIceberg(ib)
			case 1, 2: // plain resting liquidity
				o, err := types.NewOrder(user, "QP", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
				if err != nil {
					continue
				}
				e.Process(o)
			default: // the taker under test
				// Oversized on purpose: most of these cannot fill, which is the
				// path this test exists for.
				o, err := types.NewOrder(user, "QP", side, types.OrderTypeLimit, price, qty*4, types.TIFFillOrKill)
				if err != nil {
					continue
				}
				bidsBefore := make(map[int64][]int64)
				asksBefore := make(map[int64][]int64)
				for p := int64(95); p <= 105; p++ {
					bidsBefore[p] = levelIDs(e, types.SideBuy, p)
					asksBefore[p] = levelIDs(e, types.SideSell, p)
				}

				if res := e.Process(o); res.Status != types.OrderStatusRejected {
					continue
				}
				refusals++

				for p := int64(95); p <= 105; p++ {
					for _, c := range []struct {
						side   types.Side
						before []int64
					}{{types.SideBuy, bidsBefore[p]}, {types.SideSell, asksBefore[p]}} {
						got := levelIDs(e, c.side, p)
						if !slices.Equal(got, c.before) {
							t.Fatalf("seed %d, command %d: the refused fill-or-kill rewrote level %s %d "+
								"from %v to %v. A rejected command must leave queue priority exactly as it "+
								"found it. docs/PINNED-DEFECTS.md §13.6.", seed, step, c.side, p, c.before, got)
						}
					}
				}
				checkInvariants(t, e, nil)
			}
		}
		if refusals == 0 {
			t.Fatalf("seed %d produced no refused fill-or-kill, so it asserted nothing. The generator "+
				"has drifted away from the path this test exists for.", seed)
		}
	}
}

// TestRejectedFOKKeepsItsOwnFillCounters pins docs/PINNED-DEFECTS.md §9's third
// bullet: the family's remaining member, measured in the same slice that fixed two
// of them and deliberately not fixed here.
//
// A fill-or-kill for 12 prints 9 against three makers, cannot fill, and has all nine
// prints reversed. Every maker gets its quantity and its place back and no trade
// reaches a consumer — and the TAKER walks away holding FilledQty 9 and
// RemainingQty 3, on an order that ended REJECTED, with those numbers published on
// its REJECTED event for every consumer to read.
//
// It is pinned rather than fixed because it is a much wider consumer-visible change
// than either fix in this slice: it moves the payload of EVERY rejected fill-or-kill
// that printed, on a path internal/semcheck's corpus reaches forty times. WHEN IT IS
// FIXED, this test fails; invert it, and delete this paragraph's claim from
// docs/PINNED-DEFECTS.md §9.
func TestRejectedFOKKeepsItsOwnFillCounters(t *testing.T) {
	sink := &findingSink{}
	cfg := DefaultConfig("K")
	cfg.EventSink = sink
	e := NewEngine(cfg)

	for _, user := range []string{"m1", "m2", "m3"} {
		maker, err := types.NewOrder(user, "K", types.SideSell, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		e.Process(maker)
	}
	mark := len(sink.evs)

	fok, err := types.NewOrder("t", "K", types.SideBuy, types.OrderTypeLimit, 100, 12, types.TIFFillOrKill)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(fok)
	if res.Status != types.OrderStatusRejected {
		t.Fatalf("the fill-or-kill ended %s, want REJECTED", res.Status)
	}
	if _, qty, ok := e.BestAsk(); !ok || qty != 9 {
		t.Fatalf("the makers were not fully restored (best ask %d lots, present %t)", qty, ok)
	}

	if fok.FilledQty != 9 || fok.RemainingQty != 3 {
		t.Fatalf("the rejected taker holds filled %d / remaining %d. If it now reads 0 / 12 the finding "+
			"docs/PINNED-DEFECTS.md §9 records has been FIXED — invert this test and delete that bullet.",
			fok.FilledQty, fok.RemainingQty)
	}
	batch := sink.evs[mark:]
	if len(batch) != 1 || batch[0].Kind != EventRejected || batch[0].Order == nil {
		t.Fatalf("the rejected command published %v, want exactly [REJECTED]", kindsFrom(batch))
	}
	if batch[0].Order.FilledQty != 9 {
		t.Fatalf("the REJECTED event carries FilledQty %d; this test is pinned to the state where a "+
			"consumer is told a refused order filled nine lots it did not keep", batch[0].Order.FilledQty)
	}
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

// TestCascadeFiredStopRejectedLeavesAPhantom used to pin the defect of the same
// family as §9(b) that adversarial review of the fix for it turned up. It now
// asserts the opposite, and the name is kept so the pin is visibly the thing that
// was redeemed.
//
// A stop fires inside cascadeStops. Its order is emitted TRIGGERED and ACCEPTED,
// then settleInto rejects it — a fill-or-kill that cannot fill. That rejection used
// to reach nobody: emitTerminalIfDone fired only on OrderStatusCancelled, and a
// cascade-fired order never reaches emitResult, so its status and its return value
// were both discarded. The stream said an order entered the book and never said it
// left, so a consumer reconstructing the book held a fifty-lot phantom at 200
// forever — and pkg/marketdata's L2 feed published forty-six lots the book did not
// have.
//
// The decision (docs/PINNED-DEFECTS.md §4.3) widens emitTerminalIfDone to fire on
// Rejected as well as Cancelled, carrying the reason settleInto returned. Routing
// cascade-fired orders through emitResult instead would announce a filled one twice
// and publish a batch mid-command; delaying the ACCEPTED until after settleInto
// would put the stop's own trades before the event that introduces the order, which
// is the exact failure emitAdd's comment records.
//
// The kind stays CANCELED rather than REJECTED, and the assertions below are about
// that choice: a mirror deletes on CANCELED and not on REJECTED, the CANCELED must
// come AFTER the ACCEPTED or the mirror deletes an order it has not yet added, and
// the reason must survive so a client is told why a contingent order was refused.
func TestCascadeFiredStopRejectedLeavesAPhantom(t *testing.T) {
	cfg := DefaultConfig("A")
	mirror := newMirror()
	sink := &findingSink{}
	cfg.EventSink = MultiSink{mirror, sink}
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
	mark := len(sink.evs)

	// Trades at 95, which fires the stop, whose fill-or-kill is then rejected.
	trigger, err := types.NewOrder("t2", "A", types.SideBuy, types.OrderTypeLimit, 95, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(trigger)

	got, want := mirror.resting(), engineResting(e)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the stream does not reconstruct the book across a rejected cascade-fired stop\n"+
			"  mirror: %v\n  engine: %v\nA consumer holding an order the venue refused is the defect "+
			"docs/PINNED-DEFECTS.md §4 records.", got, want)
	}

	batch := sink.evs[mark:]
	accepted, canceled := -1, -1
	for i, ev := range batch {
		if ev.OrderID != inner.ID {
			continue
		}
		switch ev.Kind {
		case EventAccepted:
			accepted = i
		case EventCanceled:
			canceled = i
		}
	}
	if canceled < 0 {
		t.Fatalf("nothing in the batch says the refused stop order %d left the book: %v",
			inner.ID, kindsFrom(batch))
	}
	if accepted < 0 || canceled < accepted {
		t.Fatalf("the CANCELED for order %d is at %d and its ACCEPTED at %d: %v. Before the ACCEPTED, a "+
			"mirror deletes an order it has not yet added and then adds it, and the phantom comes back.",
			inner.ID, canceled, accepted, kindsFrom(batch))
	}
	if !errors.Is(batch[canceled].Reason, types.ErrFOKCannotFill) {
		t.Fatalf("the CANCELED carries reason %v, want ErrFOKCannotFill — the client whose contingent "+
			"order was refused is told a cancellation happened and not why", batch[canceled].Reason)
	}
	if batch[canceled].Order == nil || batch[canceled].Order.Status != types.OrderStatusRejected {
		t.Fatalf("the CANCELED carries order %+v; the kind means REMOVED and the status is what says the "+
			"venue refused it. docs/PINNED-DEFECTS.md §4.3.", batch[canceled].Order)
	}
}
