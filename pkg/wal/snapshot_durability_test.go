package wal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func snapWithOneOrder(t *testing.T) *matching.EngineSnapshot {
	t.Helper()
	e := matching.NewEngine(matching.DefaultConfig("X"))
	o, err := types.NewOrder("u1", "X", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(o)
	return e.TakeSnapshot()
}

// TestWriteSnapshotLeavesNoTempFile pins the cleanup half of the atomic swap. A
// stray .tmp beside the snapshot is not merely untidy — it is the artefact an
// operator would find after a crash and have to reason about.
func TestWriteSnapshotLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")
	if err := WriteSnapshot(path, snapWithOneOrder(t)); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file survived a successful write (stat err = %v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want exactly the snapshot", len(entries))
	}
}

// TestWriteSnapshotFailureKeepsPreviousGood is the property that matters when a
// checkpoint fails mid-flight: the last good snapshot must still be loadable. A
// write that destroys the previous snapshot before failing turns a recoverable
// incident into an unrecoverable one.
func TestWriteSnapshotFailureKeepsPreviousGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")

	good := snapWithOneOrder(t)
	if err := WriteSnapshot(path, good); err != nil {
		t.Fatalf("WriteSnapshot (first): %v", err)
	}

	// Make the directory unwritable so creating the temp file fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot chmod temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := WriteSnapshot(path, snapWithOneOrder(t))
	if err == nil {
		t.Skip("directory is still writable (running as root?); cannot exercise the failure path")
	}

	_ = os.Chmod(dir, 0o700)
	got, err := ReadSnapshot(path)
	if err != nil {
		t.Fatalf("previous snapshot unreadable after a failed write: %v", err)
	}
	if got == nil || len(got.Orders) != len(good.Orders) {
		t.Errorf("previous snapshot damaged by a failed write: got %+v", got)
	}
}

// TestReadSnapshotRejectsTornFile guards the reason WriteSnapshot fsyncs at all.
// A truncated snapshot must be an error, not a zero-valued snapshot that Recover
// would restore as an empty book while the WAL tail replays on top of it.
func TestReadSnapshotRejectsTornFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")
	if err := WriteSnapshot(path, snapWithOneOrder(t)); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(path, b[:len(b)/2], 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	if _, err := ReadSnapshot(path); err == nil {
		t.Error("a truncated snapshot was accepted; recovery would silently start from an empty book")
	}
}

// TestWriteSnapshotOverwritesAtomically checks the ordinary re-checkpoint path:
// a second write replaces the first and the result is the newer state.
func TestWriteSnapshotOverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")

	if err := WriteSnapshot(path, snapWithOneOrder(t)); err != nil {
		t.Fatalf("WriteSnapshot (first): %v", err)
	}

	e := matching.NewEngine(matching.DefaultConfig("X"))
	for i := int64(0); i < 3; i++ {
		o, err := types.NewOrder("u1", "X", types.SideBuy, types.OrderTypeLimit, 100-i, 5, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		e.Process(o)
	}
	if err := WriteSnapshot(path, e.TakeSnapshot()); err != nil {
		t.Fatalf("WriteSnapshot (second): %v", err)
	}

	got, err := ReadSnapshot(path)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if len(got.Orders) != 3 {
		t.Errorf("snapshot holds %d orders, want 3 (the second write)", len(got.Orders))
	}
}
