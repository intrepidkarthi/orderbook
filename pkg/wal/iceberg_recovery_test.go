package wal

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The round trip the defect never had, and the tape that says what it cost.
//
// docs/ICEBERG-DURABILITY.md deliverables 4 and 6. There was no test of iceberg WAL
// recovery at all before this file: the pin next door asserted the wrong behaviour
// deliberately, and the snapshot half asserted the path that was working. Nobody had
// ever written down what a correct log-only recovery of an iceberg looks like.

// TestIcebergRecoveredFromTheLogAloneKeepsItsReserve is that missing test.
//
// It asserts the four things a recovered iceberg has to get right, and they are four
// rather than one because three of them can be right while the fourth is wrong: the
// hidden reserve, the display size (a record that stated the total and lost the slice
// would show all nine lots, which is the defect inverted and worse), the REFILL —
// that consuming the visible slice reloads the next one from the reserve — and a
// subsequent fill that has to reach into the hidden size to be filled at all.
func TestIcebergRecoveredFromTheLogAloneKeepsItsReserve(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "iceberg-roundtrip.wal")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	base, err := types.NewOrder("u1", "X", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	ib, err := types.NewIcebergOrder(base, 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	if _, err := w.AppendIceberg(ib); err != nil {
		t.Fatalf("AppendIceberg: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eng, err := Recover(tapeCfg(), "", walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// 1. The reserve, and 2. the display size, read off the recovered engine itself.
	snap := eng.TakeSnapshot()
	if len(snap.Icebergs) != 1 {
		t.Fatalf("the recovered engine holds %d icebergs, want 1", len(snap.Icebergs))
	}
	if got := snap.Icebergs[0]; got.Hidden != 6 || got.DisplayQty != 3 {
		t.Fatalf("the recovered iceberg is {Hidden:%d DisplayQty:%d}, want {Hidden:6 DisplayQty:3} — "+
			"nine lots shown three. docs/ICEBERG-DURABILITY.md §3", got.Hidden, got.DisplayQty)
	}
	if _, qty, ok := eng.BestAsk(); !ok || qty != 3 {
		t.Fatalf("the recovered book shows %d lots at the offer (present=%v), want 3: the reserve is "+
			"HIDDEN, and a recovery that rested all nine would be the defect inverted", qty, ok)
	}

	// 3. The refill: taking the visible slice reloads the next one from the reserve.
	if n := printed(eng.Process(wrapperOrder(t, "u2", types.SideBuy, types.OrderTypeLimit, 100, 3))); n != 3 {
		t.Fatalf("a buy of 3 printed %d lots, want 3", n)
	}
	snap = eng.TakeSnapshot()
	if len(snap.Icebergs) != 1 {
		t.Fatalf("after one slice the recovered engine holds %d icebergs, want 1", len(snap.Icebergs))
	}
	if got := snap.Icebergs[0]; got.Hidden != 3 || got.Refills != 1 {
		t.Fatalf("after one slice the recovered iceberg is {Hidden:%d Refills:%d}, want {Hidden:3 Refills:1} — "+
			"the visible slice was consumed and the next one must have been loaded from the reserve",
			got.Hidden, got.Refills)
	}
	if _, qty, ok := eng.BestAsk(); !ok || qty != 3 {
		t.Fatalf("after the refill the book shows %d lots at the offer (present=%v), want 3", qty, ok)
	}

	// 4. A fill that only the hidden size can satisfy.
	if n := printed(eng.Process(wrapperOrder(t, "u3", types.SideBuy, types.OrderTypeLimit, 100, 6))); n != 6 {
		t.Fatalf("a buy of 6 against a recovered iceberg with 3 shown and 3 in reserve printed %d lots, "+
			"want 6 — the fill has to reach into the hidden size", n)
	}
	if _, _, ok := eng.BestAsk(); ok {
		t.Fatal("the iceberg is exhausted, so nothing should be left at the offer")
	}
}

// TestALostReserveInventsTradesAndFlipsTheBook is deliverable 6: the cost is not one
// order.
//
// docs/ICEBERG-DURABILITY.md's opening measurement, asserted. The tape is an iceberg
// of 9 shown 3, a buy of 9, then a sell of 5. Nothing is rejected, no fill-or-kill is
// involved. A venue recovering from its journal alone must print the same three
// trades and end holding the same book.
//
// Three assertions, and they are separate because a fix that repaired only the first
// would leave the venue printing trades nobody made:
//
//	the count and the quantity  — the two refilled slices that go missing
//	the invented print          — five lots at 100 the venue never printed
//	the final book              — SELL 5 @100 live, the OPPOSITE side after a lossy replay
func TestALostReserveInventsTradesAndFlipsTheBook(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "iceberg-tape.wal")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	liveSink := &printSink{}
	liveCfg := tapeCfg()
	liveCfg.EventSink = liveSink
	live := matching.NewEngine(liveCfg)

	ib, err := types.NewIcebergOrder(wrapperOrder(t, "u1", types.SideSell, types.OrderTypeLimit, 100, 9), 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	if _, err := w.AppendIceberg(ib); err != nil {
		t.Fatalf("AppendIceberg: %v", err)
	}
	live.ProcessIceberg(ib)
	logSubmit(t, w, live, wrapperOrder(t, "u2", types.SideBuy, types.OrderTypeLimit, 100, 9))
	logSubmit(t, w, live, wrapperOrder(t, "u3", types.SideSell, types.OrderTypeLimit, 100, 5))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recSink := &printSink{}
	recCfg := tapeCfg()
	recCfg.EventSink = recSink
	recovered, err := Recover(recCfg, "", walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if len(liveSink.trades) != 3 || liveSink.lots != 9 {
		t.Fatalf("the LIVE venue printed %d trades for %d lots, want 3 and 9 — the control has broken, "+
			"so this test no longer measures what it claims to: %v",
			len(liveSink.trades), liveSink.lots, liveSink.trades)
	}

	// Half one: the prints that go missing.
	if len(recSink.trades) != len(liveSink.trades) || recSink.lots != liveSink.lots {
		t.Errorf("a log-only recovery printed %d trades for %d lots; the venue printed %d for %d.\n"+
			" live: %v\n  log: %v\nThe two refilled slices are missing, because the reserve they came "+
			"from was never journalled. docs/ICEBERG-DURABILITY.md §2",
			len(recSink.trades), recSink.lots, len(liveSink.trades), liveSink.lots,
			liveSink.trades, recSink.trades)
	}

	// Half two: the print that is invented. It is asserted separately because it is a
	// different failure — a trade nobody made, against a resting order the live venue
	// never had — and a fix that restored the missing slices without removing this
	// would still put a print on the tape that no client sent an order for.
	remaining := map[string]int{}
	for _, tr := range liveSink.trades {
		remaining[tr]++
	}
	for _, tr := range recSink.trades {
		if remaining[tr] == 0 {
			t.Errorf("a log-only recovery printed %q, which the venue never printed. The buy of 9 was "+
				"filled 3 and RESTED on the recovered book, so the sell of 5 hit it. "+
				"docs/ICEBERG-DURABILITY.md §2", tr)
			continue
		}
		remaining[tr]--
	}

	// Half three: the book. The live venue ends holding the seller's residue; a venue
	// that lost the reserve ends holding the opposite side.
	if got, want := recovered.TakeSnapshot().Digest(), live.TakeSnapshot().Digest(); got != want {
		p, q, bidOK := recovered.BestBid()
		ap, aq, askOK := recovered.BestAsk()
		t.Errorf("a log-only recovery ends on a different book: recovered bid %d x %d (present=%v), "+
			"ask %d x %d (present=%v)\n got %s\nwant %s\ndocs/ICEBERG-DURABILITY.md §2",
			p, q, bidOK, ap, aq, askOK, got, want)
	}
}

// printSink records the prints an engine publishes, in order. It formats on the way
// in because the sink contract reuses the slice and its pointers after the call.
//
// Deliberately coarser than boundary_test.go's tapeSink, which carries trade ids: the
// comparison below is a MULTISET difference looking for a print nobody made, and a
// tape keyed by id would make every print unique and the difference meaningless.
type printSink struct {
	trades []string
	lots   int64
}

func (s *printSink) OnEvents(events []matching.Event) {
	for _, ev := range events {
		if ev.Kind != matching.EventTrade || ev.Trade == nil {
			continue
		}
		s.trades = append(s.trades, fmt.Sprintf("%s %d@%d", ev.Trade.TakerSide, ev.Trade.Quantity, ev.Trade.Price))
		s.lots += ev.Trade.Quantity
	}
}

func printed(res *matching.MatchResult) int64 {
	var n int64
	for _, tr := range res.Trades {
		n += tr.Quantity
	}
	return n
}
