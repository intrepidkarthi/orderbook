package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// digest fingerprints an engine's resting book independent of timestamps.
func digest(eng *matching.Engine) string {
	var b strings.Builder
	for _, o := range eng.Book().Orders() {
		fmt.Fprintf(&b, "%d|%s|%d|%d\n", o.ID, o.Side, o.Price, o.RemainingQty)
	}
	return b.String()
}

func newOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestWAL_FullReplayRecovery: a fresh engine replaying the durable command log
// rebuilds byte-identical book state — the crash-recovery contract.
func TestWAL_FullReplayRecovery(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "wal.log")
	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	live := matching.NewEngine(matching.DefaultConfig("X"))

	// Write-ahead: log then apply.
	submit := func(user string, side types.Side, price, qty int64) *types.Order {
		o := newOrder(t, user, side, price, qty)
		if _, err := w.AppendSubmit(o); err != nil {
			t.Fatalf("AppendSubmit: %v", err)
		}
		_ = w.Sync()
		live.Process(o)
		return o
	}
	cancel := func(id int64, user string) {
		_, _ = w.AppendCancel(id, user)
		_ = w.Sync()
		_, _ = live.Cancel(id, user)
	}

	submit("mm", types.SideSell, 100, 5)
	a2 := submit("mm", types.SideSell, 101, 5)
	submit("t", types.SideBuy, 100, 3) // partial fill of the 100 ask
	submit("b", types.SideBuy, 99, 10) // rests
	cancel(a2.ID, "mm")                // cancel the 101 ask
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// "Crash", then recover from the WAL into a fresh engine.
	entries, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	recovered := matching.NewEngine(matching.DefaultConfig("X"))
	Restore(recovered, entries)

	if d1, d2 := digest(live), digest(recovered); d1 != d2 {
		t.Errorf("recovered book != live book\n live=%q\n rec =%q", d1, d2)
	}
}

// TestWAL_SnapshotThenTail: snapshot bounds replay — recover from the snapshot,
// then replay only the WAL tail after it, to the same live state.
func TestWAL_SnapshotThenTail(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")
	snapPath := filepath.Join(dir, "snap.json")

	live := matching.NewEngine(matching.DefaultConfig("X"))
	live.Process(newOrder(t, "mm", types.SideSell, 100, 5))
	live.Process(newOrder(t, "mm", types.SideBuy, 99, 5))
	// Checkpoint after the prefix (captured by the snapshot, not the WAL).
	if err := WriteSnapshot(snapPath, live.TakeSnapshot()); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	// Tail, logged to the WAL.
	w, _ := Open(walPath)
	tail := func(side types.Side, price, qty int64) {
		o := newOrder(t, "x", side, price, qty)
		_, _ = w.AppendSubmit(o)
		_ = w.Sync()
		live.Process(o)
	}
	tail(types.SideSell, 101, 4)
	tail(types.SideBuy, 98, 3)
	_ = w.Close()

	// Recover: load snapshot, replay only the tail.
	snap, err := ReadSnapshot(snapPath)
	if err != nil || snap == nil {
		t.Fatalf("ReadSnapshot: %v (snap=%v)", err, snap)
	}
	recovered, err := matching.RestoreEngine(matching.DefaultConfig("X"), snap)
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}
	entries, _ := ReadAll(walPath)
	Restore(recovered, entries)

	if d1, d2 := digest(live), digest(recovered); d1 != d2 {
		t.Errorf("snapshot+tail recovery != live\n live=%q\n rec =%q", d1, d2)
	}
}

// TestWAL_ReplayBypassesAdmissionControls: recovery must reproduce the exact book
// even when the engine has live-ingress admission controls (minimum resting time,
// per-order caps) enabled. A cancel accepted live after the order rested long
// enough would be wrongly rejected on a fast replay if the check re-ran; Restore
// runs in replay mode so it doesn't, and the recovered book matches live.
func TestWAL_ReplayBypassesAdmissionControls(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "wal.log")
	now := time.Unix(0, 0).UTC()
	cfg := func() matching.Config {
		c := matching.DefaultConfig("X")
		c.Clock = func() time.Time { return now }
		c.MinRestingTime = time.Second
		c.MaxOrderQty = 1000
		return c
	}

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	live := matching.NewEngine(cfg())

	// Two resting asks placed at t=0, logged write-ahead.
	a1 := newOrder(t, "mm", types.SideSell, 100, 5)
	_, _ = w.AppendSubmit(a1)
	_ = w.Sync()
	live.Process(a1)
	a2 := newOrder(t, "mm", types.SideSell, 101, 5)
	_, _ = w.AppendSubmit(a2)
	_ = w.Sync()
	live.Process(a2)

	// Advance the clock past the minimum resting time, then cancel a2 — accepted
	// live because it has rested 2s (> 1s).
	now = time.Unix(2, 0).UTC()
	if _, err := live.Cancel(a2.ID, "mm"); err != nil {
		t.Fatalf("live cancel after min-rest should succeed: %v", err)
	}
	_, _ = w.AppendCancel(a2.ID, "mm")
	_ = w.Sync()
	_ = w.Close()

	// Recover into a fresh engine with the SAME controls enabled. Restore replays
	// fast (near-zero elapsed) but in replay mode, so the cancel still applies.
	entries, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	recovered := matching.NewEngine(cfg())
	Restore(recovered, entries)

	if d1, d2 := digest(live), digest(recovered); d1 != d2 {
		t.Errorf("recovery with admission controls diverged\n live=%q\n rec =%q", d1, d2)
	}
	// Sanity: only the 100 ask should remain.
	if got := digest(recovered); got != "1|SELL|100|5\n" {
		t.Errorf("recovered book = %q, want the single 100 ask", got)
	}
}

// TestWAL_TornTail: a crash mid-write leaves a partial record; the reader recovers
// every complete record before it and stops cleanly.
func TestWAL_TornTail(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "wal.log")
	w, _ := Open(walPath)
	_, _ = w.AppendSubmit(newOrder(t, "a", types.SideBuy, 100, 1))
	_, _ = w.AppendSubmit(newOrder(t, "b", types.SideSell, 101, 1))
	_ = w.Close()

	// Simulate a torn write: a length header claiming more bytes than follow.
	f, _ := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0o644)
	_, _ = f.Write([]byte{0, 0, 0, 50, 'g', 'a', 'r', 'b'}) // says 50 bytes, only 4 follow
	_ = f.Close()

	entries, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("torn tail: recovered %d entries, want 2 complete records", len(entries))
	}
}
