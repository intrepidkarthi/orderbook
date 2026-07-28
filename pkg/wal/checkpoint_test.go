package wal

import (
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func cpCfg() matching.Config { return matching.DefaultConfig("X") }

func cpOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// bookFingerprint reduces an engine to the state recovery must reproduce.
func bookFingerprint(t *testing.T, e *matching.Engine) string {
	t.Helper()
	snap := e.TakeSnapshot()
	s := ""
	for _, o := range snap.Orders {
		s += string(rune('a'+int(o.ID%26))) + ":" +
			itoa(o.ID) + "/" + itoa(o.Price) + "/" + itoa(o.RemainingQty) + ";"
	}
	return s
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// drive appends each order to the log and applies it, returning the sequence of
// the last APPLIED command — the value a checkpoint must be stamped with.
func drive(t *testing.T, w *Writer, e *matching.Engine, orders []*types.Order) int64 {
	t.Helper()
	var lastApplied int64
	for _, o := range orders {
		seq, err := w.AppendSubmit(o)
		if err != nil {
			t.Fatalf("AppendSubmit: %v", err)
		}
		e.Process(o)
		lastApplied = seq
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return lastApplied
}

// TestRecoverMatchesUninterruptedRun is the property the whole recovery story
// rests on: snapshot partway, keep trading, then rebuild from snapshot plus log
// tail and land on the same book as a run that was never interrupted.
func TestRecoverMatchesUninterruptedRun(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")
	snapPath := filepath.Join(dir, "snap.json")

	first := []*types.Order{
		cpOrder(t, "a", types.SideBuy, 100, 5),
		cpOrder(t, "b", types.SideBuy, 99, 5),
		cpOrder(t, "c", types.SideSell, 105, 5),
	}
	second := []*types.Order{
		cpOrder(t, "d", types.SideBuy, 98, 7),
		cpOrder(t, "e", types.SideSell, 104, 3),
		cpOrder(t, "f", types.SideSell, 100, 2), // crosses the bid at 100
	}

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	live := matching.NewEngine(cpCfg())

	applied := drive(t, w, live, first)
	if err := Checkpoint(snapPath, live, applied); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	drive(t, w, live, second)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := bookFingerprint(t, live)

	got, err := Recover(cpCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if g := bookFingerprint(t, got); g != want {
		t.Errorf("recovered book\n got %s\nwant %s", g, want)
	}
}

// TestRecoverDoesNotDoubleApply is the failure mode that made the previously
// documented recipe silently wrong: without a recorded boundary the caller
// replays the whole log over a snapshot that already contains those commands.
func TestRecoverDoesNotDoubleApply(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")
	snapPath := filepath.Join(dir, "snap.json")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	live := matching.NewEngine(cpCfg())
	applied := drive(t, w, live, []*types.Order{
		cpOrder(t, "a", types.SideBuy, 100, 5),
		cpOrder(t, "b", types.SideBuy, 99, 5),
	})
	if err := Checkpoint(snapPath, live, applied); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wantCount := live.Book().Count()

	got, err := Recover(cpCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if c := got.Book().Count(); c != wantCount {
		t.Errorf("recovered book has %d orders, want %d — the snapshotted commands were replayed on top of the snapshot", c, wantCount)
	}

	// And prove the guard is the recorded boundary, not luck: replaying the whole
	// log over the same snapshot must visibly double the book.
	doubled, err := matching.RestoreEngine(cpCfg(), mustSnap(t, snapPath))
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}
	entries, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	Restore(doubled, entries) // deliberately ignores the boundary
	if c := doubled.Book().Count(); c != wantCount*2 {
		t.Errorf("sanity: replaying everything gave %d orders, expected %d — test no longer demonstrates the hazard", c, wantCount*2)
	}
}

// TestRecoverWithoutSnapshotReplaysEverything covers the cold-start path.
func TestRecoverWithoutSnapshotReplaysEverything(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	live := matching.NewEngine(cpCfg())
	drive(t, w, live, []*types.Order{
		cpOrder(t, "a", types.SideBuy, 100, 5),
		cpOrder(t, "b", types.SideSell, 105, 5),
	})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := bookFingerprint(t, live)

	got, err := Recover(cpCfg(), filepath.Join(dir, "absent.json"), walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if g := bookFingerprint(t, got); g != want {
		t.Errorf("cold recovery\n got %s\nwant %s", g, want)
	}
}

// TestRecoverEmpty covers neither file present.
func TestRecoverEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := Recover(cpCfg(), filepath.Join(dir, "s.json"), filepath.Join(dir, "w.log"))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Book().Count() != 0 {
		t.Errorf("fresh engine has %d resting orders, want 0", got.Book().Count())
	}
}

func mustSnap(t *testing.T, path string) *matching.EngineSnapshot {
	t.Helper()
	s, err := ReadSnapshot(path)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if s == nil {
		t.Fatal("snapshot missing")
	}
	return s
}
