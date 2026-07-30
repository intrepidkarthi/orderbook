package wal

import (
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The log recorded submits and cancels and nothing else, while Reduce and
// CancelAllForUser both mutate the book. Neither is a control command a snapshot
// covers, so a restart undid them: an order reduced to 40 came back at 100, and a
// kill switch that pulled an account's book handed it all back. Nothing errored in
// either case — the recovered venue was simply wrong about what was resting.

func mutCfg() matching.Config {
	c := matching.DefaultConfig("X")
	c.DedupClientOrderIDs = 256
	return c
}

func mustOrder(t *testing.T, user string, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", types.SideBuy, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestReduceSurvivesRecovery — the size a client was told its order now has must
// be the size it has after a restart.
func TestReduceSurvivesRecovery(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: mutCfg(), QueueSize: 64, Log: w})

	res := r.Process(mustOrder(t, "u", 100, 100))
	if res == nil || res.Order == nil {
		t.Fatal("order not accepted")
	}
	id := res.Order.ID

	if _, err := r.Reduce(id, 40, "u"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	// The reduce must be a record of its own, not something inferred later.
	entries, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var reduces int
	for _, e := range entries {
		if e.Kind == KindReduce {
			reduces++
			if e.NewQty != 40 {
				t.Errorf("logged NewQty = %d, want 40", e.NewQty)
			}
			if e.CancelID != id {
				t.Errorf("logged order id = %d, want %d", e.CancelID, id)
			}
			if e.UserID != "u" {
				t.Errorf("logged user = %q, want \"u\"", e.UserID)
			}
		}
	}
	if reduces != 1 {
		t.Fatalf("log holds %d reduce records, want 1 — the reduce was applied without being logged", reduces)
	}

	eng, err := Recover(mutCfg(), "", walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	revived := eng.OpenOrdersFor("u")
	if len(revived) != 1 {
		t.Fatalf("recovered %d orders, want 1", len(revived))
	}
	if got := revived[0].Quantity; got != 40 {
		t.Errorf("recovered order has quantity %d, want 40 — the reduce did not survive the restart", got)
	}
	if got := revived[0].RemainingQty; got != 40 {
		t.Errorf("recovered order leaves %d, want 40", got)
	}
	// Depth must agree with the sum of the orders, or the book the venue quotes
	// is not the book it holds.
	if _, qty, ok := eng.BestBid(); !ok || qty != 40 {
		t.Errorf("recovered best bid shows %d (ok=%v), want 40", qty, ok)
	}
}

// TestReduceThenRecoverIsIdempotentUnderReplay — the log is written write-ahead,
// so a reduce the engine went on to refuse is on disk too. Replay must refuse it
// the same way rather than treating it as a command to force through.
func TestRefusedReduceReplaysAsRefused(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: mutCfg(), QueueSize: 64, Log: w})
	res := r.Process(mustOrder(t, "u", 100, 100))
	id := res.Order.ID

	// An increase, which Reduce refuses by design.
	if _, err := r.Reduce(id, 500, "u"); err == nil {
		t.Fatal("Reduce grew an order")
	}
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	eng, err := Recover(mutCfg(), "", walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	revived := eng.OpenOrdersFor("u")
	if len(revived) != 1 {
		t.Fatalf("recovered %d orders, want 1", len(revived))
	}
	if got := revived[0].Quantity; got != 100 {
		t.Errorf("recovered quantity %d, want 100 — replay applied a reduce the engine had refused", got)
	}
}

// TestCancelAllSurvivesRecovery — the operator kill switch is the one command that
// must never be quietly undone. A restart that hands a pulled book back is worse
// than one that never had the switch.
func TestCancelAllSurvivesRecovery(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: mutCfg(), QueueSize: 64, Log: w})

	for i := 0; i < 3; i++ {
		r.Process(mustOrder(t, "victim", int64(100-i), 5))
	}
	// A second account, to prove the sweep is scoped on the way back too.
	r.Process(mustOrder(t, "other", 80, 5))

	pulled, err := r.CancelAllForUser("victim")
	if err != nil {
		t.Fatalf("CancelAllForUser: %v", err)
	}
	if len(pulled) != 3 {
		t.Fatalf("pulled %d orders, want 3", len(pulled))
	}
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	eng, err := Recover(mutCfg(), "", walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := len(eng.OpenOrdersFor("victim")); got != 0 {
		t.Errorf("the pulled account has %d orders back after recovery, want 0", got)
	}
	if got := len(eng.OpenOrdersFor("other")); got != 1 {
		t.Errorf("the untouched account has %d orders, want 1 — replay swept more than the log recorded", got)
	}
}

// TestSnapshotPlusTailKeepsReduces — the tail after a checkpoint is where an
// unlogged mutation hid: the snapshot held the pre-reduce size and nothing in the
// tail corrected it.
func TestSnapshotPlusTailKeepsReduces(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")
	snapPath := filepath.Join(dir, "snap.json")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: mutCfg(), QueueSize: 64, Log: w})

	res := r.Process(mustOrder(t, "u", 100, 100))
	id := res.Order.ID

	// Checkpoint BEFORE the reduce, so only the log tail carries it.
	snap, err := r.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := WriteSnapshot(snapPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if _, err := r.Reduce(id, 25, "u"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	eng, err := Recover(mutCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	revived := eng.OpenOrdersFor("u")
	if len(revived) != 1 {
		t.Fatalf("recovered %d orders, want 1", len(revived))
	}
	if got := revived[0].Quantity; got != 25 {
		t.Errorf("recovered quantity %d, want 25 — the snapshot's pre-reduce size won", got)
	}
}
