package matching

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func bustOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// bustEngine returns an engine with an attached sink and one printed trade at 100.
func bustEngine(t *testing.T) (*Engine, *ocoSink, types.Trade) {
	t.Helper()
	sink := &ocoSink{}
	cfg := DefaultConfig("X")
	cfg.EventSink = sink
	e := NewEngine(cfg)

	e.Process(bustOrder(t, "maker", types.SideSell, 100, 10))
	res := e.Process(bustOrder(t, "taker", types.SideBuy, 100, 4))
	if len(res.Trades) != 1 {
		t.Fatalf("setup: got %d trades, want 1", len(res.Trades))
	}
	return e, sink, *res.Trades[0]
}

func TestBustRecordsAndAnnounces(t *testing.T) {
	e, sink, tr := bustEngine(t)

	if err := e.Bust(tr.ID, "erroneous order entry"); err != nil {
		t.Fatalf("Bust: %v", err)
	}
	if !e.IsBusted(tr.ID) {
		t.Error("IsBusted = false after a successful bust")
	}
	if got := e.BustCount(); got != 1 {
		t.Errorf("BustCount = %d, want 1", got)
	}

	last := sink.got[len(sink.got)-1]
	if last.Kind != EventBusted {
		t.Fatalf("last event kind = %v, want BUSTED", last.Kind)
	}
	if last.TradeID != tr.ID {
		t.Errorf("event TradeID = %d, want %d", last.TradeID, tr.ID)
	}
	if last.BustReason != "erroneous order entry" {
		t.Errorf("event BustReason = %q, want the operator's reason", last.BustReason)
	}
	// Appended, not substituted: the trade event that reported the print is still
	// in the stream, unchanged. A consumer replaying the tape must see both.
	var sawTrade bool
	for _, ev := range sink.got {
		if ev.Kind == EventTrade && ev.Trade != nil && ev.Trade.ID == tr.ID {
			sawTrade = true
		}
	}
	if !sawTrade {
		t.Error("the original trade event is gone; a bust must append, not rewrite")
	}
}

// TestBustDoesNotRewindTheBook is the load-bearing test of this feature, and the
// one that will look wrong to anybody who thinks of a bust as an undo. Each
// assertion here is a rule from docs/TRADE-BUST.md §2, and a change that "fixes"
// one of them is a change to the design, not to the code.
func TestBustDoesNotRewindTheBook(t *testing.T) {
	sink := &ocoSink{}
	cfg := DefaultConfig("X")
	cfg.EventSink = sink
	e := NewEngine(cfg)

	// A resting sell of 10, a buy of 4 against it, and a stop that the print fires.
	maker := bustOrder(t, "maker", types.SideSell, 100, 10)
	e.Process(maker)
	stop, err := types.NewStopOrder(bustOrder(t, "stopper", types.SideSell, 90, 1), 100)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	e.ProcessStop(stop)

	res := e.Process(bustOrder(t, "taker", types.SideBuy, 100, 4))
	if len(res.Trades) == 0 {
		t.Fatal("setup: no trade")
	}
	tr := res.Trades[0]

	// The stop must actually have fired, or the assertion below proves nothing.
	if n := len(e.stopBook.All()); n != 0 {
		t.Fatalf("setup: %d stops still pending; the print did not fire the stop, so this test is vacuous", n)
	}

	restingBefore := len(e.book.Orders())
	depthBefore := int64(0)
	if _, q, ok := e.BestAsk(); ok {
		depthBefore = q
	}
	lastBefore := e.book.LastTradePrice()
	stopsBefore := e.stopBook.All()

	if err := e.Bust(tr.ID, "off-market print"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	// 1. The orders are not put back.
	if got := len(e.book.Orders()); got != restingBefore {
		t.Errorf("resting order count %d -> %d: the bust re-rested the busted orders", restingBefore, got)
	}
	_, depthAfter, _ := e.BestAsk()
	if depthAfter != depthBefore {
		t.Errorf("ask depth %d -> %d: the bust returned the filled size to the book", depthBefore, depthAfter)
	}
	if maker.FilledQty != 4 {
		t.Errorf("maker filled = %d, want 4: the bust un-filled the maker", maker.FilledQty)
	}

	// 2. What the print caused stays caused: the stop it fired stays fired.
	if len(e.stopBook.All()) != len(stopsBefore) {
		t.Errorf("pending stop count %d -> %d: the bust un-fired a triggered stop",
			len(stopsBefore), len(e.stopBook.All()))
	}

	// 3. The reference price is not rewound.
	if got := e.book.LastTradePrice(); got != lastBefore {
		t.Errorf("LastTradePrice %d -> %d: the bust moved the band reference under live orders",
			lastBefore, got)
	}
}

func TestBustRejectsUnknownTrade(t *testing.T) {
	e, _, tr := bustEngine(t)

	for _, id := range []int64{0, -1, tr.ID + 1, 1 << 40} {
		if err := e.Bust(id, "typo"); !errors.Is(err, ErrUnknownTrade) {
			t.Errorf("Bust(%d) = %v, want ErrUnknownTrade", id, err)
		}
	}
	if e.BustCount() != 0 {
		t.Error("a refused bust was recorded anyway")
	}
}

// TestBustRejectsDuplicate — deliberately an error rather than a no-op. An
// operator issuing the same bust twice is usually an operator busting the wrong id
// twice, and this is not the layer to be relaxed about that.
func TestBustRejectsDuplicate(t *testing.T) {
	e, sink, tr := bustEngine(t)

	if err := e.Bust(tr.ID, "first"); err != nil {
		t.Fatalf("first Bust: %v", err)
	}
	before := len(sink.got)
	if err := e.Bust(tr.ID, "second"); !errors.Is(err, ErrAlreadyBusted) {
		t.Errorf("second Bust = %v, want ErrAlreadyBusted", err)
	}
	if len(sink.got) != before {
		t.Error("the refused duplicate published an event")
	}
	if r := e.busted[tr.ID]; r.Reason != "first" {
		t.Errorf("reason = %q, want the first one: a refused bust overwrote the record", r.Reason)
	}
}

// TestBustSurvivesSnapshotRoundTrip — the registry has to be part of the state a
// restored engine carries, or a restart quietly un-busts every annulled trade.
func TestBustSurvivesSnapshotRoundTrip(t *testing.T) {
	e, _, tr := bustEngine(t)
	if err := e.Bust(tr.ID, "erroneous"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	restored, err := RestoreEngine(DefaultConfig("X"), e.TakeSnapshot())
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}
	if !restored.IsBusted(tr.ID) {
		t.Fatal("the restored engine forgot the bust")
	}
	if got := restored.busted[tr.ID].Reason; got != "erroneous" {
		t.Errorf("restored reason = %q, want \"erroneous\"", got)
	}
	// And it stays refused: a restored registry that did not feed validation would
	// let the same trade be busted a second time after a restart.
	if err := restored.Bust(tr.ID, "again"); !errors.Is(err, ErrAlreadyBusted) {
		t.Errorf("re-bust after restore = %v, want ErrAlreadyBusted", err)
	}
}

// TestBustChangesTheDigest is what makes the registry replicable. Two engines that
// applied the same commands are equal only if they also agree on what settled, so
// a follower that lost a bust has to be a digest mismatch — otherwise a primary
// and its backup can disagree about which trades stand and still compare
// identical.
//
// The assertion isolates the registry on purpose. The obvious version of this test
// — bust on one engine, not the other, compare — passes even when the registry is
// left out of the snapshot entirely, because emitting EventBusted moves EventSeq
// and the digest covers that. It was written that way first and a sabotage run
// caught it. What follows compares two snapshots that differ in nothing else.
func TestBustChangesTheDigest(t *testing.T) {
	e, _, tr := bustEngine(t)
	if err := e.Bust(tr.ID, "erroneous"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	withBust := e.TakeSnapshot()
	if len(withBust.Busted) != 1 {
		t.Fatalf("snapshot carries %d bust records, want 1", len(withBust.Busted))
	}

	withoutBust := *withBust
	withoutBust.Busted = nil
	if withBust.Digest() == withoutBust.Digest() {
		t.Error("dropping the bust registry did not change the digest; a follower could lose a bust undetected")
	}

	// A different reason on the same trade is also a difference. The reason is
	// audit metadata, but it is metadata the record carries, so two engines that
	// disagree about it have not applied the same command.
	otherReason := *withBust
	otherReason.Busted = []BustRecord{{
		TradeID: tr.ID, Reason: "something else", At: withBust.Busted[0].At,
	}}
	if withBust.Digest() == otherReason.Digest() {
		t.Error("the bust reason is invisible to the digest")
	}

	// And two engines driven identically, both busting the same trade the same
	// way, still agree — the property that makes the digest usable as a
	// divergence alarm rather than a source of false ones.
	a, _, trA := bustEngine(t)
	b, _, trB := bustEngine(t)
	if err := a.Bust(trA.ID, "erroneous"); err != nil {
		t.Fatalf("Bust(a): %v", err)
	}
	if err := b.Bust(trB.ID, "erroneous"); err != nil {
		t.Fatalf("Bust(b): %v", err)
	}
	if a.TakeSnapshot().Digest() != b.TakeSnapshot().Digest() {
		t.Error("two engines that busted the same trade the same way have different digests")
	}
}

// TestBustDigestIgnoresWallClock — the At stamp differs between a bust restored
// from a snapshot (original instant) and one replayed from the log tail (replay
// instant). Both are the same bust, so the digest must not see the difference.
func TestBustDigestIgnoresWallClock(t *testing.T) {
	e, _, tr := bustEngine(t)
	if err := e.Bust(tr.ID, "erroneous"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	snap := e.TakeSnapshot()
	if len(snap.Busted) != 1 {
		t.Fatalf("snapshot carries %d bust records, want 1", len(snap.Busted))
	}
	want := snap.Digest()

	// The same registry, stamped a day later.
	shifted := *snap
	shifted.Busted = []BustRecord{{
		TradeID: tr.ID,
		Reason:  "erroneous",
		At:      snap.Busted[0].At.Add(24 * 60 * 60 * 1e9),
	}}
	if got := shifted.Digest(); got != want {
		t.Error("the digest changed with the bust timestamp; replayed and restored engines would never agree")
	}
}

// bustConformanceCase joins the 22 scenarios in TestEventStreamReconstructsBook.
// EventBusted carries no book change, so an L3 consumer must be able to ignore it
// and still rebuild the book exactly — and, because the mirror checks sequence
// continuity, the bust must not tear a hole in the stream either.
func bustConformanceCase(t *testing.T) conformanceCase {
	t.Helper()
	return conformanceCase{
		name: "a bust annuls a print without touching the book",
		run: func(t *testing.T, e *Engine) {
			e.Process(cOrd(t, "maker", types.SideSell, 100, 10))
			res := e.Process(cOrd(t, "taker", types.SideBuy, 100, 4))
			if len(res.Trades) == 0 {
				t.Fatal("setup: no trade to bust")
			}
			if err := e.Bust(res.Trades[0].ID, "erroneous"); err != nil {
				t.Fatalf("Bust: %v", err)
			}
			// Trading continues afterwards: the stream has to stay usable, not just
			// survive to the bust.
			e.Process(cOrd(t, "later", types.SideBuy, 99, 7))
			e.Process(cOrd(t, "later2", types.SideSell, 100, 2))
		},
	}
}
