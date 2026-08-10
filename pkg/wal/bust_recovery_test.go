package wal

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// A bust is a logged command, so it has to come back after a crash. It is also the
// command with the least tolerance for coming back wrong: a lost bust means the
// venue quietly reinstates a trade an operator annulled, and nothing about the
// restart says so. See docs/TRADE-BUST.md.

// bustTapeRunner starts a Runner over a real WAL and prints one trade, returning
// the runner, the writer and the trade id.
func bustTapeRunner(t *testing.T, dir string) (*matching.Runner, *Writer, int64) {
	t.Helper()
	w, err := Open(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), Log: w})

	mk := func(user string, side types.Side, price, qty int64) *types.Order {
		o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		return o
	}
	r.Process(mk("maker", types.SideSell, 100, 10))
	res := r.Process(mk("taker", types.SideBuy, 100, 4))
	if res == nil || len(res.Trades) == 0 {
		t.Fatal("setup: no trade printed")
	}
	return r, w, res.Trades[0].ID
}

// TestBustSurvivesRecoveryFromLogTail — no snapshot at all, so the bust can only
// come back by being replayed out of the log.
func TestBustSurvivesRecoveryFromLogTail(t *testing.T) {
	dir := t.TempDir()
	r, w, tradeID := bustTapeRunner(t, dir)

	if err := r.Bust(tradeID, "erroneous order entry"); err != nil {
		t.Fatalf("Bust: %v", err)
	}
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	eng, err := Recover(tapeCfg(), filepath.Join(dir, "absent.json"), filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !eng.IsBusted(tradeID) {
		t.Fatalf("trade %d is not busted after recovery; the annulment was lost", tradeID)
	}
	if err := eng.Bust(tradeID, "again"); !errors.Is(err, matching.ErrAlreadyBusted) {
		t.Errorf("re-bust after recovery = %v, want ErrAlreadyBusted", err)
	}
}

// TestBustSurvivesRecoveryAcrossCheckpoint covers both halves of the recovery
// path in one run: one bust is folded into the snapshot, the other is only in the
// tail after it. Getting either mechanism wrong loses exactly one of them.
func TestBustSurvivesRecoveryAcrossCheckpoint(t *testing.T) {
	dir := t.TempDir()
	snapPath := filepath.Join(dir, "snap.json")
	r, w, firstTrade := bustTapeRunner(t, dir)

	if err := r.Bust(firstTrade, "before the checkpoint"); err != nil {
		t.Fatalf("Bust: %v", err)
	}
	snap, err := r.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := WriteSnapshot(snapPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	// A second print, busted after the checkpoint.
	second, err := types.NewOrder("taker2", "X", types.SideBuy, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := r.Process(second)
	if res == nil || len(res.Trades) == 0 {
		t.Fatal("setup: second print did not trade")
	}
	secondTrade := res.Trades[0].ID
	if err := r.Bust(secondTrade, "after the checkpoint"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	live, err := r.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	eng, err := Recover(tapeCfg(), snapPath, filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	for _, id := range []int64{firstTrade, secondTrade} {
		if !eng.IsBusted(id) {
			t.Errorf("trade %d is not busted after recovery", id)
		}
	}
	if got, want := eng.TakeSnapshot().Digest(), live.Digest(); got != want {
		t.Errorf("recovered engine differs from the live one\n got %s\nwant %s", got, want)
	}
}

// TestBustReplayIsRefusedIdenticallyOutOfRange — the log records what was
// attempted, and a bust the live engine refused must be refused on replay too
// rather than silently succeeding against a different trade counter.
func TestBustReplayIsRefusedIdentically(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), Log: w})

	// No trades have printed, so every id is unknown. The command is journalled
	// write-ahead regardless — that is the point of write-ahead — so replay meets a
	// record for a bust that never took effect.
	if err := r.Bust(7, "nothing to bust"); !errors.Is(err, matching.ErrUnknownTrade) {
		t.Fatalf("live Bust = %v, want ErrUnknownTrade", err)
	}
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	eng, err := Recover(tapeCfg(), filepath.Join(dir, "absent.json"), walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if eng.BustCount() != 0 {
		t.Errorf("replay recorded %d busts; a refusal replayed as a success", eng.BustCount())
	}
}
