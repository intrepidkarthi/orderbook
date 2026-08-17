package wal

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/tape"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The auction half of the recovery contract, from docs/JOURNAL-COMPLETENESS.md.
//
// Runner.SetPhase runs the opening or closing uncross, prints its trades, and sets
// engine state the snapshot carries — and for three releases it reached
// logCommand's default branch, the branch whose comment claimed to hold only
// read-only commands. A venue that ran an auction and crashed before its next
// checkpoint recovered into the PRE-transition phase with an un-uncrossed book and
// no auction prints on its tape.
//
// The damage does not stop at the phase field, which is why these tests compare a
// book digest and a trade tape rather than an enum:
//
//   - a missing SetPhase(StatePreOpen) makes replay MATCH orders the live venue
//     rested, so the recovered tape holds trades that never happened and every
//     later trade id is shifted by the phantom prints;
//   - a missing SetPhase(StateOpen) skips the uncross, so the recovered book stays
//     crossed and the auction's prints are absent from a tape subscribers already
//     received;
//   - the price collar falls back to LastTradePrice, so a skipped uncross leaves
//     every later admission decision measured against a stale reference.

// auctionTape is a full session in the command alphabet the boundary sweep now
// speaks: pre-open accumulating a deliberately crossed book, an opening uncross, a
// stretch of continuous trading, a closing auction accumulating a second crossed
// book on top of a live one, its uncross, and a reopen.
//
// It contains one participant crossing ITSELF inside the auction on purpose.
// docs/JOURNAL-COMPLETENESS.md §7 names the self-trade-prevention branch of
// Uncross as the likeliest place for the uncross to turn out non-deterministic on
// replay — it stamps a wall-clock UpdatedAt on the victim — so the tape reaches it
// rather than leaving the prediction untested.
func auctionTape() []tape.Cmd {
	var out []tape.Cmd
	add := func(c tape.Cmd) {
		c.Pos = len(out)
		out = append(out, c)
	}
	buy := func(user string, price, qty int64) {
		add(tape.Cmd{Kind: tape.Submit, User: user, Price: price, Qty: qty})
	}
	sell := func(user string, price, qty int64) {
		add(tape.Cmd{Kind: tape.Submit, User: user, Sell: true, Price: price, Qty: qty})
	}
	phase := func(p tape.Phase) { add(tape.Cmd{Kind: tape.SetPhase, Phase: p}) }

	phase(tape.PhasePreOpen)
	buy("alice", 105, 6)
	sell("bob", 98, 4)
	buy("carol", 103, 5) // position 3, cancelled below
	sell("dave", 99, 7)
	buy("erin", 101, 3)
	sell("frank", 100, 2)
	buy("self", 104, 2)
	sell("self", 97, 2)
	phase(tape.PhaseOpen) // the opening uncross prints here
	sell("bob", 102, 5)
	buy("alice", 102, 5) // continuous, on the uncrossed book
	// Named by the POSITION of the submit that created it, so deleting that submit
	// would leave this cancelling a known-absent id rather than silently retargeting.
	add(tape.Cmd{Kind: tape.Cancel, User: "carol", Target: 3})
	buy("carol", 96, 4)
	phase(tape.PhaseClosingAuction)
	buy("dave", 108, 3)
	sell("erin", 94, 3)
	buy("frank", 107, 2)
	phase(tape.PhaseClosed) // the closing uncross prints here
	buy("alice", 100, 1)    // refused after the close, and journalled anyway
	phase(tape.PhasePreOpen)
	sell("bob", 99, 2)
	phase(tape.PhaseOpen)
	return out
}

// TestCrashAcrossAnAuction is deliverable 4 of docs/JOURNAL-COMPLETENESS.md, and it
// is TestCrashAtEveryBoundary's property over a tape whose whole point is the
// commands that tape could not reach: crash at every boundary of a session with two
// auctions in it, and require the replayed book digest AND the replayed trade tape
// to equal the live run's at every one of them.
//
// Against the code this document was written for it fails at the first assertion —
// the log holds four fewer records than the tape has commands, because four
// SetPhase commands were applied and never written down.
func TestCrashAcrossAnAuction(t *testing.T) {
	cmds := auctionTape()
	n := len(cmds)

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	live := &tapeSink{}
	cfg := tapeCfg()
	cfg.EventSink = live
	r := matching.NewRunner(matching.RunnerConfig{Engine: cfg, QueueSize: 64, Log: w})

	wantDigest := make([]string, n+1)
	wantPrints := make([]int, n+1)
	wantDigest[0] = digestRunner(t, r)

	ids := make(map[int]int64, len(cmds))
	for i, c := range cmds {
		applyTapeCmd(t, r, c, ids)
		wantDigest[i+1] = digestRunner(t, r)
		wantPrints[i+1] = len(live.prints)
	}
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	entries, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("log holds %d records for %d commands — a command was applied without being journalled, "+
			"so boundary k is no longer record k", len(entries), n)
	}
	if got := live.auctionPrints(); got < 2 {
		t.Fatalf("the live run produced %d auction prints; the tape is meant to run two uncrosses", got)
	}

	for k := 0; k <= n; k++ {
		replayed := &tapeSink{}
		rcfg := tapeCfg()
		rcfg.EventSink = replayed
		eng := matching.NewEngine(rcfg)
		Restore(eng, entries[:k])

		if got := snapDigest(t, eng.TakeSnapshot()); got != wantDigest[k] {
			t.Fatalf("boundary %d: recovered book differs from the live engine after %d commands\n got %s\nwant %s",
				k, k, got, wantDigest[k])
		}
		if got, want := strings.Join(replayed.prints, "\n"), live.tape(wantPrints[k]); got != want {
			t.Fatalf("boundary %d: replayed trade tape differs from the live one\n got %q\nwant %q", k, got, want)
		}
	}
}

// TestAuctionSurvivesRecovery is deliverable 3, stated the way an operator would
// state it: a venue that runs pre-open → open, crashes before its next checkpoint,
// and recovers must come back OPEN, with an UNCROSSED book, and with the auction's
// prints on its tape.
//
// The checkpoint is taken before the transition on purpose, so the snapshot cannot
// be the thing that carries it — the window between the last checkpoint and the
// crash is the window a write-ahead log exists to cover.
func TestAuctionSurvivesRecovery(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")
	snapPath := filepath.Join(dir, "snap.json")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	live := &tapeSink{}
	cfg := tapeCfg()
	cfg.EventSink = live
	r := matching.NewRunner(matching.RunnerConfig{Engine: cfg, QueueSize: 64, Log: w})

	r.SetPhase(matching.StatePreOpen)
	rest := func(user string, side types.Side, price, qty int64) {
		t.Helper()
		o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		r.Process(o)
	}
	rest("alice", types.SideBuy, 105, 5)
	rest("bob", types.SideSell, 99, 5)
	rest("carol", types.SideBuy, 101, 3)
	rest("dave", types.SideSell, 100, 3)

	// A crossed book is the intended state of a pre-open, and it is what makes the
	// assertion below meaningful: if the transition is lost, the recovered venue
	// opens onto a bid above an ask.
	bid, _, okB := r.BestBid()
	ask, _, okA := r.BestAsk()
	if !okB || !okA || bid <= ask {
		t.Fatalf("pre-open did not accumulate a crossed book: bid %d (%v), ask %d (%v)", bid, okB, ask, okA)
	}

	snap, err := r.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := WriteSnapshot(snapPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	trades := r.SetPhase(matching.StateOpen)
	if len(trades) == 0 {
		t.Fatal("the opening uncross printed nothing; there is no auction to lose")
	}
	liveTape := strings.Join(live.prints, "\n")
	liveDigest := digestRunner(t, r)
	r.Close()
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	replayed := &tapeSink{}
	rcfg := tapeCfg()
	rcfg.EventSink = replayed
	eng, err := Recover(rcfg, snapPath, walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if got := eng.State(); got != matching.StateOpen {
		t.Errorf("recovered venue is %s, want OPEN — the phase transition was applied and never journalled", got)
	}
	rb, _, okB := eng.BestBid()
	ra, _, okA := eng.BestAsk()
	if okB && okA && rb >= ra {
		t.Errorf("recovered book is still crossed (bid %d, ask %d) — the uncross never ran on replay", rb, ra)
	}
	if got := countAuctionPrints(replayed.prints); got == 0 {
		t.Error("the recovered tape holds no auction prints, but subscribers already received them")
	}
	if got := strings.Join(replayed.prints, "\n"); got != liveTape {
		t.Errorf("recovered tape differs from the live one\n got %q\nwant %q", got, liveTape)
	}
	if got := snapDigest(t, eng.TakeSnapshot()); got != liveDigest {
		t.Errorf("recovered digest differs from the live one\n got %s\nwant %s", got, liveDigest)
	}
}

// TestPreOpenRestsRatherThanMatchesOnReplay isolates §3.1: the cost of losing the
// transition INTO an accumulating phase, which is the direction that invents
// trades rather than losing them.
//
// A replay that arrives in StateOpen matches orders the live venue rested, so the
// recovered tape holds prints that never happened — and because trade ids come
// from a monotonic counter, every later id names a different trade than it did
// before the crash.
func TestPreOpenRestsRatherThanMatchesOnReplay(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	live := &tapeSink{}
	cfg := tapeCfg()
	cfg.EventSink = live
	r := matching.NewRunner(matching.RunnerConfig{Engine: cfg, QueueSize: 64, Log: w})

	r.SetPhase(matching.StatePreOpen)
	for _, o := range []struct {
		user  string
		side  types.Side
		price int64
	}{
		{"alice", types.SideSell, 100},
		{"bob", types.SideBuy, 104}, // would trade instantly in a continuous session
	} {
		ord, err := types.NewOrder(o.user, "X", o.side, types.OrderTypeLimit, o.price, 5, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		r.Process(ord)
	}
	if len(live.prints) != 0 {
		t.Fatalf("pre-open printed %d trades; it must rest orders, not match them", len(live.prints))
	}
	liveDigest := digestRunner(t, r)
	r.Close()
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	replayed := &tapeSink{}
	rcfg := tapeCfg()
	rcfg.EventSink = replayed
	eng, err := Recover(rcfg, "", walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := len(replayed.prints); got != 0 {
		t.Errorf("replay printed %d trades the live venue never printed — every later trade id is now shifted", got)
	}
	if got := eng.State(); got != matching.StatePreOpen {
		t.Errorf("recovered venue is %s, want PRE_OPEN", got)
	}
	if got := snapDigest(t, eng.TakeSnapshot()); got != liveDigest {
		t.Errorf("recovered digest differs from the live one\n got %s\nwant %s", got, liveDigest)
	}
}
