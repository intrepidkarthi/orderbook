package wal

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/tape"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// tapeSink records the trade tape as the engine emits it. Only trades: this is
// the tape a client reconciles against, and it is the half of the recovery
// contract that a book digest alone cannot see.
type tapeSink struct{ prints []string }

func (s *tapeSink) OnEvents(evs []matching.Event) {
	for _, e := range evs {
		if e.Kind != matching.EventTrade || e.Trade == nil {
			continue
		}
		t := e.Trade
		// Trade ids and the aggressor are part of the contract; wall-clock is
		// not, for the same reason snapDigest normalises it.
		//
		// Auction is in the line because it is part of the contract too: a print a
		// consumer was told came from an uncross must not replay as a continuous
		// fill, and a comparison that dropped the flag would call those two tapes
		// equal.
		s.prints = append(s.prints, fmt.Sprintf("%d|%d|%d|%s|%s|%s|auction=%t",
			t.ID, t.Price, t.Quantity, t.TakerSide, t.BuyerUserID, t.SellerUserID, t.Auction))
	}
}

func (s *tapeSink) tape(n int) string { return strings.Join(s.prints[:n], "\n") }

// auctionPrints counts the uncross prints on the tape. The boundary sweeps assert
// it is non-zero: without that, extending the tape's alphabet with phase
// transitions could silently degrade into transitions that uncross nothing, and
// the sweep would go back to proving what it proved before.
func (s *tapeSink) auctionPrints() int { return countAuctionPrints(s.prints) }

func countAuctionPrints(prints []string) int {
	var n int
	for _, p := range prints {
		if strings.HasSuffix(p, "|auction=true") {
			n++
		}
	}
	return n
}

// reasonSink records every rejection reason the venue published and every order id
// that appeared on a print, so a guard can assert what the tape REACHED rather than
// what it drew.
type reasonSink struct {
	reasons map[string]int
	printed map[int64]bool
}

func (s *reasonSink) OnEvents(evs []matching.Event) {
	if s.reasons == nil {
		s.reasons = map[string]int{}
		s.printed = map[int64]bool{}
	}
	for _, e := range evs {
		switch {
		case e.Kind == matching.EventRejected && e.Reason != nil:
			s.reasons[e.Reason.Error()]++
		case e.Kind == matching.EventTrade && e.Trade != nil:
			s.printed[e.Trade.BuyOrderID] = true
			s.printed[e.Trade.SellOrderID] = true
		}
	}
}

// TestRecoveryTapeSpeaksTheTierOneAlphabet is the guard that would have caught the
// hole this test file shipped with, and it is the reason the hole is worth writing
// down rather than quietly patching.
//
// tape.Recovery's comment claimed the profile was "deliberately a SUPERSET of
// Differential". It set Exotic:false, which made that true on the command-KIND axis
// (it adds SetPhase) and false on the order-PAYLOAD axis: measured on this exact
// 400-command tape, 287 submits, every one a plain GTC limit — no market orders, no
// IOC, no FOK, no post-only, no per-order STP, no trade groups, no privileged
// orders. So the replay oracle never crossed a crash boundary carrying a rejected
// FOK's reversed prints or an STP-cancelled maker: the two paths
// docs/REFERENCE-MATCHER.md §9 named IN ADVANCE as defect-bearing, and the two the
// differential harness then confirmed as live defects.
//
// Every assertion in the file passed throughout, because they count prints, auction
// prints and depth — none of which move if every submit is a plain limit. That is
// docs/JOURNAL-COMPLETENESS.md §1 exactly: an exhaustive check over an incomplete
// alphabet reporting completeness, in the sweep written to apply that lesson.
//
// So the alphabet is asserted here by OUTCOME wherever an outcome is observable
// from outside — a rejection reason a consumer was actually told — and by draw only
// where it is not. The two are labelled, because "drawn" and "reached" is the
// distinction the reduce incident in internal/tape cost a round to learn.
func TestRecoveryTapeSpeaksTheTierOneAlphabet(t *testing.T) {
	const n = 400
	cmds := recoveryTape(n)

	// --- the draw axis --------------------------------------------------------
	drawn := map[string]int{}
	stpModes := map[uint8]int{}
	for _, c := range cmds {
		if c.Kind != tape.Submit && c.Kind != tape.Replace {
			continue
		}
		if c.MarketOrd {
			drawn["market"]++
		}
		if c.PostOnly {
			drawn["post-only"]++
		}
		switch c.TIF {
		case 1:
			drawn["ioc"]++
		case 2:
			drawn["fok"]++
		}
		if c.STP != 0 {
			drawn["stp-mode"]++
			stpModes[c.STP]++
		}
		if c.TradeGroup != 0 {
			drawn["trade-group"]++
		}
		if c.Privileged {
			drawn["privileged"]++
		}
	}
	for _, what := range []string{"market", "post-only", "ioc", "fok", "stp-mode", "trade-group", "privileged"} {
		if drawn[what] == 0 {
			t.Errorf("the recovery tape draws %s zero times, so the crash-boundary sweep never replays one. "+
				"tape.Recovery claims to be a superset of tape.Differential; on the order-payload axis it is not", what)
		}
	}
	// All five per-order modes, not just the ones that happen to be common: STP is
	// where this engine is most likely to be self-consistently wrong, and a sweep
	// carrying three of five modes still looks like a sweep over STP.
	for mode := uint8(1); mode <= 5; mode++ {
		if stpModes[mode] == 0 {
			t.Errorf("no order on the recovery tape carries per-order STP mode %d", mode)
		}
	}

	// --- the outcome axis -----------------------------------------------------
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sink := &reasonSink{}
	cfg := tapeCfg()
	cfg.EventSink = sink
	r := matching.NewRunner(matching.RunnerConfig{Engine: cfg, QueueSize: 64, Log: w})
	ids := make(map[int]int64, len(cmds))
	for _, c := range cmds {
		applyTapeCmd(t, r, c, ids)
	}
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	// Each of these is a distinct settleInto branch, and each is a branch the sweep
	// could not reach at all before this tape carried the payload for it.
	for _, want := range []error{
		types.ErrFOKCannotFill,
		types.ErrPostOnlyWouldCross,
	} {
		if sink.reasons[want.Error()] == 0 {
			t.Errorf("no command on the recovery tape was refused with %q, so the crash-boundary sweep "+
				"never carries that path across a boundary", want)
		}
	}
	// Reachable in principle, NOT reached by this tape, listed rather than omitted
	// so the two sets together account for the whole tier-1 rejection enum — the
	// same idiom as TestDifferentialSweepReachesEveryOutcome. A reason that quietly
	// moves between the lists is a change somebody has to make deliberately.
	//
	// types.ErrMarketOrderNoLiquidity needs a market order to arrive at an EMPTY
	// opposite side. This tape keeps a peak resting depth in the thirties, so its
	// market orders always find a counterparty. Rather than assert a rejection that
	// does not happen, the market-order path is asserted by the outcome it DOES
	// reach: at least one market order printed.
	var marketPrinted int
	for _, c := range cmds {
		if !c.MarketOrd {
			continue
		}
		if id, ok := ids[c.Pos]; ok && sink.printed[id] {
			marketPrinted++
		}
	}
	if marketPrinted == 0 {
		t.Errorf("the recovery tape drew %d market orders and not one of them printed, so the market-order "+
			"branch of the walk is drawn but never reached", drawn["market"])
	}
	t.Logf("recovery tape reached: %v (%d market orders printed)", sink.reasons, marketPrinted)
	t.Logf("recovery tape drew: %v (stp modes %v)", drawn, stpModes)
}

// TestCrashAtEveryBoundary is the test a reader on r/highfreqtrading proposed
// when the recovery design came up, phrased almost exactly this way: kill the
// process at every write and emit boundary, then check that the replayed book
// AND the trade tape are identical to the uninterrupted run.
//
// It is strictly stronger than TestCrashRecoveryMatchesUninterrupted, which
// samples five checkpoints across a 2000-command tape and compares only the
// book. Sampling five points out of two thousand tests five points. A recovery
// bug that only shows up when the crash lands between a match and its emitted
// fill would sit in the 1,995 that were never tried.
//
// The property, at every one of the tape's boundaries:
//
//   - replaying the first k journal records reproduces the book the live engine
//     had after its k-th command, byte for byte; and
//   - the events that replay emits are the same prints, in the same order, that
//     the live run emitted over those k commands.
//
// The second half is what covers the emit boundary. A crash after matching but
// before the client saw the fill is survivable precisely because replay
// re-emits that fill rather than inventing a different one or skipping it.
func TestCrashAtEveryBoundary(t *testing.T) {
	// Every boundary means O(n²) applies, so n is chosen to keep this a test
	// people will actually run rather than a nightly job they will not.
	const n = 400
	cmds := recoveryTape(n)

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	// --- the uninterrupted run, recorded at every boundary -------------------
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
	wantPrints[0] = 0

	ids := make(map[int]int64, len(cmds))
	peakResting := 0
	for i, c := range cmds {
		applyTapeCmd(t, r, c, ids)
		if d := r.OrderCount(); d > peakResting {
			peakResting = d
		}
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
	// The premise: one journal record per command, so record k is boundary k.
	// Write-ahead logging records refused commands too, so this holds even for
	// the cancels in the tape that find nothing to cancel.
	if len(entries) != n {
		t.Fatalf("log holds %d records for %d commands; boundary k is no longer record k", len(entries), n)
	}
	if wantPrints[n] == 0 {
		t.Fatal("the tape produced no trades, so the emit half of this test asserts nothing")
	}
	if live.auctionPrints() == 0 {
		t.Fatal("the tape ran no auction, so this sweep is back to the alphabet docs/JOURNAL-COMPLETENESS.md §1 diagnosed")
	}
	// Floors, not just non-zero.
	//
	// When the recovery alphabet was widened to the shared internal/tape generator,
	// the sweep got WIDER and thinner at the same time: cancel-all, halt and
	// cancel-only refuse or remove exactly the liquidity the sweep is comparing, and
	// the measured effect on this 400-command tape was continuous prints 202 -> 117
	// and peak resting depth 72 -> 45, against a gain of 11 -> 16 auction prints and
	// five command kinds the old tape could not reach at all. That trade is
	// deliberate and it is recorded here as numbers, so the NEXT alphabet change has
	// to argue a number down rather than quietly hollow the sweep out while every
	// assertion still passes. A bare "> 0" would not have noticed.
	if got := len(live.prints); got < 100 {
		t.Fatalf("the tape produced only %d prints; the trade-tape half of this sweep is being hollowed out", got)
	}
	if got := live.auctionPrints(); got < 10 {
		t.Fatalf("the tape produced only %d auction prints, so the uncross is barely swept", got)
	}
	if peakResting < 30 {
		t.Fatalf("the book never held more than %d resting orders; a sweep over a near-empty book compares "+
			"a near-empty book", peakResting)
	}

	// --- crash at every boundary --------------------------------------------
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
			t.Fatalf("boundary %d: replayed trade tape differs from the live one (%d prints replayed, %d expected)",
				k, len(replayed.prints), wantPrints[k])
		}
	}
}

// TestCrashAtEveryBoundaryWithSnapshot is the same property across the join
// that recovery actually uses in production: a snapshot plus the log tail after
// it. The seam between the two is where a boundary bug hides, because it is the
// one place where "replay everything" and "replay the tail" disagree about who
// owns a record.
//
// The replay engine is configured with an event sink, exactly like the live one,
// and that detail is load-bearing. Engine.eventSeq only advances when a sink is
// attached, and EventSeq is part of the snapshot — so replaying without one
// reproduces the book perfectly and still fails the digest. Writing this test
// without the sink is how that was learned.
//
// It is also why cmd/obgw's recovered engine is legitimately behind the live
// engine's event sequence: obgw attaches its sink AFTER recovery on purpose, so
// replayed history is not re-published to clients. That is safe there because a
// restart mints a new incarnation and clients resync against it, but a consumer
// that persisted engine event sequence numbers across a restart would be
// surprised.
func TestCrashAtEveryBoundaryWithSnapshot(t *testing.T) {
	const n = 200
	cmds := recoveryTape(n)
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

	// The snapshot lands mid-tape, so every boundary after it exercises the join.
	const snapAt = 97
	wantDigest := make([]string, n+1)
	wantPrints := make([]int, n+1)
	wantDigest[0] = digestRunner(t, r)

	ids := make(map[int]int64, len(cmds))
	for i, c := range cmds {
		applyTapeCmd(t, r, c, ids)
		if i == snapAt {
			snap, err := r.Checkpoint()
			if err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			if err := WriteSnapshot(snapPath, snap); err != nil {
				t.Fatalf("WriteSnapshot: %v", err)
			}
		}
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
	base, err := ReadSnapshot(snapPath)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if base == nil {
		t.Fatal("no snapshot was written")
	}
	// The join is the point of this test, so the auction has to be in the TAIL:
	// an uncross that only ever ran before the checkpoint is carried by the
	// snapshot and proves nothing about the records replayed on top of it.
	if countAuctionPrints(live.prints[wantPrints[snapAt+1]:]) == 0 {
		t.Fatal("no auction print falls after the checkpoint, so the snapshot+tail join never replays one")
	}

	// Every boundary at or after the checkpoint: load the snapshot, apply only
	// the tail beyond its WALSeq, and land where the live engine was. The tail's
	// prints must be the live run's prints over exactly that span — a snapshot
	// that silently re-emits history, or drops it, fails here.
	for k := snapAt + 1; k <= n; k++ {
		replayed := &tapeSink{}
		rcfg := tapeCfg()
		rcfg.EventSink = replayed
		eng, err := matching.RestoreEngine(rcfg, base)
		if err != nil {
			t.Fatalf("boundary %d: RestoreEngine: %v", k, err)
		}
		RestoreAfter(eng, entries[:k], base.WALSeq)

		if got := snapDigest(t, eng.TakeSnapshot()); got != wantDigest[k] {
			t.Fatalf("boundary %d: snapshot+tail differs from the live engine after %d commands\n got %s\nwant %s",
				k, k, got, wantDigest[k])
		}
		want := strings.Join(live.prints[wantPrints[snapAt+1]:wantPrints[k]], "\n")
		if got := strings.Join(replayed.prints, "\n"); got != want {
			t.Fatalf("boundary %d: the tail replayed %d prints, want the live run's %d over the same span",
				k, len(replayed.prints), wantPrints[k]-wantPrints[snapAt+1])
		}
	}
}
