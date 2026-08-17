package matching

// What building the reference matcher found.
//
// Three engine behaviours, each reproduced at two or three commands, each pinned
// here so that a fix has to come to this file and change a test that says what it is
// changing. None of them is fixed in this slice: each is a matching-semantics or
// event-stream change with consequences past the matcher, and this repository writes
// the spec before the code. docs/REFERENCE-MATCHER.md §10 carries the argument.
//
// Two of the three were PREDICTED in §9 of that document, from reading the code
// before any of this was built. The third — the pro-rata self-skip crossing the book,
// pinned in differential_test.go — was not predicted and was found by the harness on
// its first pro-rata seed.
//
// These are hand-written rather than left to the differential comparison on purpose.
// The model was written to reproduce the engine's position on both, so the comparison
// is silent about them by construction; a rule that lives only in two implementations
// has no oracle at all, it has two copies. Stating it as prose in a test is what gives
// a reviewer something to disagree with.
//
// FIXING ONE OF THESE IS A TWO-FILE CHANGE, and each test below names its second
// file. This is not a style note. Because the model canonises the engine's current
// position, correcting the engine makes the MODEL wrong, and TestDifferentialTape
// goes red across several profiles and seeds at once — measured, not predicted:
//
//   - restoring LastTradePrice in settleInto's FOK unwind fails fifo/seed=3,
//     fifo/seed=8, prorata-shard7/seed=4 and capped-shard3/seed=42, class
//     "last-trade-price";
//   - keeping the Canceled events on a rejected order fails fifo/seed=3,
//     fifo/seed=34 and capped-shard3/seed=42, class "event-count", shrinking to two
//     commands.
//
// Someone who fixes the engine, sees a wall of differential divergences and does not
// know why will reach for the cheapest-looking repair, which is to relax the
// comparison. That would trade a real defect for a permanently weaker oracle. The
// model edit is part of the fix, not fallout from it.

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// findingSink collects the whole event stream so a test can assert about what a
// consumer was and was not told.
type findingSink struct{ evs []Event }

func (s *findingSink) OnEvents(evs []Event) { s.evs = append(s.evs, evs...) }

// TestRejectedFOKStillMovesTheLastTradePrice pins docs/REFERENCE-MATCHER.md §9(a).
//
// A fill-or-kill order that cannot fill has every one of its prints reversed: the
// makers get their quantity and their place in the book back, no trade reaches a
// consumer, and the command returns REJECTED. The last trade price does not go back.
//
// match calls recordLast on the way out, before settleInto's fill-or-kill branch
// unwinds anything, and nothing puts it back. So a venue's published last trade price
// can be moved by an order that never traded — and because LastTradePrice is the
// price-collar reference when no mark is set (outsideBand), a rejected order can move
// the band that admits the next one.
//
// A fix belongs in settleInto's FOK branch, which knows the pre-match reference and
// is the only place that knows the prints are being undone.
//
// THE SAME COMMIT MUST CHANGE THE MODEL. internal/refmatch reproduces this position:
// Model.execute sets m.last on every print (refmatch.go), and the FOK branch of
// Model.settle (commands.go) unwinds the trades without restoring it. Fix the engine
// alone and TestDifferentialTape fails on four seeds across three profiles with
// class "last-trade-price".
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
	if got := e.LastTradePrice(); got != 100 {
		t.Fatalf("last trade price is %d after a rejected fill-or-kill. If it is now 0, the defect "+
			"docs/REFERENCE-MATCHER.md §9(a) predicted has been FIXED — invert this test and say so in §10.", got)
	}
}

// TestSTPCancelledMakerVanishesWithNoEvent pins docs/REFERENCE-MATCHER.md §9(b), and
// it is the more serious of the two.
//
// A fill-or-kill taker under CANCEL_OLDEST removes a resting maker mid-walk, then
// fails to fill and is rejected. emitResult drops e.pending when the status is
// Rejected, so the Canceled event announcing the maker's removal never reaches a
// consumer; and reverseTrade restores only makers it TRADED with, not ones
// self-trade prevention removed. The order is gone from the book and the event stream
// never says so.
//
// That contradicts the claim EventKind's doc comment makes and
// TestEventStreamReconstructsBook proves on hand-written scenarios: that the stream
// reconstructs the L3 book. It does, for every scenario that list contains. This is
// the combination it does not contain — fill-or-kill AND self-trade prevention in one
// order — which is exactly what a generated tape reaches.
//
// A consumer built on the stream keeps a phantom resting order forever. Either the
// maker must be restored like a traded one, or its cancellation must survive the
// rejection; both are real changes with consequences for replay, so neither is made
// here.
//
// THE SAME COMMIT MUST CHANGE THE MODEL. internal/refmatch reproduces this position
// in Model.compose (commands.go): `if status == StatusRejected { m.pending = nil }`
// is the model's copy of emitResult's dropped batch. Fix the engine alone and
// TestDifferentialTape fails on fifo/seed=3, fifo/seed=34 and capped-shard3/seed=42
// with class "event-count", shrinking to a two-command tape.
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

	for _, ev := range sink.evs[mark:] {
		if ev.Kind == EventCanceled && ev.OrderID == maker.ID {
			t.Fatalf("the maker's removal WAS announced. The defect docs/REFERENCE-MATCHER.md §9(b) " +
				"predicted has been fixed — invert this test and say so in §10.")
		}
	}
	// And state what a consumer was told instead, so the failure message of a future
	// change carries the whole picture.
	kinds := make([]string, 0, len(sink.evs)-mark)
	for _, ev := range sink.evs[mark:] {
		kinds = append(kinds, ev.Kind.String())
	}
	t.Logf("the book lost order %d and the only event a consumer received was %v", maker.ID, kinds)
}
