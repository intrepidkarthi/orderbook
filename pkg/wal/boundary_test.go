package wal

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
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
	tape := buildTape(n)

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

	var ids []int64
	for i, c := range tape {
		applyTapeCmd(t, r, c, i, &ids)
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
	tape := buildTape(n)
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

	var ids []int64
	for i, c := range tape {
		applyTapeCmd(t, r, c, i, &ids)
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
