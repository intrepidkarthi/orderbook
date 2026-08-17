package wal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

// Startup validation. Every case here refuses to start rather than reporting, and
// the reason is the same in all of them: the failure mode is a book that is missing
// commands and looks fine. RestoreAfter applies whatever it is handed whose Seq is
// past the snapshot, so a set with a hole produces a plausible, wrong book with no
// error anywhere — every remaining record verifying its checksum perfectly.

// TestAMissingMiddleSegmentRefuses is the shape a retention bug, a partial restore
// or an operator's rm produces, and the one a venue must never trade on.
func TestAMissingMiddleSegmentRefuses(t *testing.T) {
	dir := t.TempDir()
	walPath, _ := buildRotatedLog(t, dir, 2_000, 0, 16<<10)
	names := segmentSetOf(t, walPath)
	if len(names) < 4 {
		t.Fatalf("fixture produced %d segments, want at least 4", len(names))
	}
	// Count what the intact set holds, so "refused" can be told from "read less".
	whole, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	victim := names[1]
	if err := os.Remove(filepath.Join(dir, victim)); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, err = ReadAll(walPath)
	if !errors.Is(err, ErrLogGap) {
		t.Fatalf("ReadAll err = %v, want ErrLogGap — a set with a hole in it read as a log", err)
	}
	if !strings.Contains(err.Error(), names[0]) || !strings.Contains(err.Error(), names[2]) {
		t.Errorf("the error does not name the two segments either side of the hole: %v", err)
	}
	if _, err := Recover(tapeCfg(), "", walPath); !errors.Is(err, ErrLogGap) {
		t.Errorf("Recover err = %v, want ErrLogGap — %d records were on disk and the venue would have started on a subset", err, len(whole))
	}
}

// TestATruncatedSegmentInTheMiddleRefuses. A torn tail is legal in the newest
// segment and legal in any segment immediately followed by one whose base is exactly
// last+1 — which is how the sealed segments Open creates on purpose stay legal.
// Anywhere else it is a gap. That is one contiguity test rather than a special case,
// and this is the other half of what it catches: a filesystem, a partial copy or an
// operator's head -c cutting a sealed segment short.
func TestATruncatedSegmentInTheMiddleRefuses(t *testing.T) {
	dir := t.TempDir()
	walPath, _ := buildRotatedLog(t, dir, 2_000, 0, 16<<10)
	names := segmentSetOf(t, walPath)

	victim := filepath.Join(dir, names[1])
	raw := readFile(t, victim)
	writeFile(t, victim, raw[:len(raw)/2])

	_, err := ReadAll(walPath)
	if !errors.Is(err, ErrLogGap) {
		t.Fatalf("ReadAll err = %v, want ErrLogGap — a sealed segment cut short is a hole, not a torn tail", err)
	}
}

// TestOverlappingSegmentsRefuse — two files claiming the same sequences is the shape
// two concurrent writers produce, and this package still does not prevent them. What
// it does is refuse to recover from the result rather than picking one arbitrarily.
func TestOverlappingSegmentsRefuse(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")

	first := make([]int64, 100)
	for i := range first {
		first[i] = int64(i + 1)
	}
	overlap := make([]int64, 100)
	for i := range overlap {
		overlap[i] = int64(50 + i)
	}
	handBuiltSegment(t, segPath(stem, 1), 1, first)
	handBuiltSegment(t, segPath(stem, 50), 50, overlap)

	_, err := ReadAll(stem)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAll err = %v, want ErrCorrupt for two segments claiming the same commands", err)
	}
	if !strings.Contains(err.Error(), "same commands") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// TestDuplicateBasesRefuse. Impossible through names alone — a directory cannot hold
// two files with one name — and reachable through a header that disagrees with its
// name, which is why it gets its own message rather than surfacing as an overlap.
func TestDuplicateBasesRefuse(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")

	seqs := make([]int64, 10)
	for i := range seqs {
		seqs[i] = int64(i + 1)
	}
	handBuiltSegment(t, segPath(stem, 1), 1, seqs)
	// A legacy stem alongside segment 1: both declare base 1.
	handBuiltLog(t, stem, seqs)

	_, err := ReadAll(stem)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAll err = %v, want ErrCorrupt", err)
	}
	if !strings.Contains(err.Error(), "base sequence 1") {
		t.Errorf("the error does not name the duplicated base: %v", err)
	}
}

// TestSnapshotBelowTheRetentionFloorRefuses is the tripwire every retention bug
// trips, and the reason the predicate in retention.go is written as a predicate.
//
// The snapshot is the base; the retained log is the tail. If the oldest retained
// sequence is more than one past the snapshot's WALSeq, the commands in between are
// in no file this venue can read. Recovering anyway produces a book that skipped
// them, with every remaining record verifying perfectly and nothing anywhere saying
// so. Refusing converts that into an outage with two numbers in it.
func TestSnapshotBelowTheRetentionFloorRefuses(t *testing.T) {
	dir := t.TempDir()
	walPath, _ := buildRotatedLog(t, dir, 2_000, 0, 16<<10)
	names := segmentSetOf(t, walPath)
	if len(names) < 4 {
		t.Fatalf("fixture produced %d segments, want at least 4", len(names))
	}
	// Delete a PREFIX by hand, which is what a retention pass that outran its
	// snapshot would leave. The set is internally valid; only the snapshot is behind.
	for _, n := range names[:2] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	}
	floor := segmentSetOf(t, walPath)[0]

	snapPath := filepath.Join(dir, "old.snap")
	emptySnapshotAt(t, snapPath, 5) // a snapshot from long before the floor

	_, err := Recover(tapeCfg(), snapPath, walPath)
	if !errors.Is(err, ErrLogGap) {
		t.Fatalf("Recover err = %v, want ErrLogGap — the venue started on a book missing every command below the floor", err)
	}
	if !strings.Contains(err.Error(), "5") || !strings.Contains(err.Error(), floor) {
		t.Errorf("the error must name both the snapshot's sequence and the floor: %v", err)
	}

	// A snapshot exactly at floor-1 is the boundary and must be accepted: the tail
	// starts at the next sequence, so nothing is missing.
	set, err := enumerateSet(walPath)
	if err != nil {
		t.Fatalf("enumerateSet: %v", err)
	}
	okSnap := filepath.Join(dir, "ok.snap")
	emptySnapshotAt(t, okSnap, set.floor()-1)
	if _, err := Recover(tapeCfg(), okSnap, walPath); err != nil {
		t.Errorf("a snapshot at floor-1 must recover: %v", err)
	}
}

// TestNoSnapshotWithAFloorAboveOneRefuses is the same condition with WALSeq = 0. It
// is the case docs/RUNBOOKS.md's old "delete the snapshot and replay from the
// beginning" procedure walks into once retention has fired: the beginning is not
// there any more.
func TestNoSnapshotWithAFloorAboveOneRefuses(t *testing.T) {
	dir := t.TempDir()
	walPath, _ := buildRotatedLog(t, dir, 2_000, 0, 16<<10)
	names := segmentSetOf(t, walPath)
	if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, err := Recover(tapeCfg(), filepath.Join(dir, "absent.snap"), walPath)
	if !errors.Is(err, ErrLogGap) {
		t.Fatalf("Recover err = %v, want ErrLogGap — replaying a retained set from nothing skips everything below the floor", err)
	}
}

// TestAnEmptySetIsNotAnError covers the three shapes of "no records", none of which
// is a failure.
func TestAnEmptySetIsNotAnError(t *testing.T) {
	build := map[string]func(t *testing.T, stem string){
		"nothing at all": func(t *testing.T, stem string) {},
		"a marker and no segments": func(t *testing.T, stem string) {
			if err := writeMarker(stem); err != nil {
				t.Fatalf("writeMarker: %v", err)
			}
		},
		"a marker and one empty segment": func(t *testing.T, stem string) {
			if err := writeMarker(stem); err != nil {
				t.Fatalf("writeMarker: %v", err)
			}
			writeFile(t, segPath(stem, 1), segHeader(1))
		},
	}
	for name, setup := range build {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			stem := filepath.Join(dir, "w.wal")
			setup(t, stem)

			snapPath := filepath.Join(dir, "s.snap")
			snap := matching.NewEngine(tapeCfg()).TakeSnapshot()
			snap.WALSeq = 0
			if err := WriteSnapshot(snapPath, snap); err != nil {
				t.Fatalf("WriteSnapshot: %v", err)
			}

			entries, err := ReadAll(stem)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("read %d entries from a set with no records", len(entries))
			}
			if _, err := Recover(tapeCfg(), snapPath, stem); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			// And a writer picks up from it without losing the sequence space.
			w, err := Open(stem)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if w.Seq() != 0 {
				t.Errorf("resumed at seq %d, want 0", w.Seq())
			}
			if _, err := w.AppendSubmit(mustOrder(t, "u", 1000, 1)); err != nil {
				t.Fatalf("AppendSubmit: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if entries, err := ReadAll(stem); err != nil || len(entries) != 1 || entries[0].Seq != 1 {
				t.Errorf("after one append: %d entries, err %v", len(entries), err)
			}
		})
	}
}

// TestADirectoryIsNotALog fixes a silent failure that predates segments and that the
// enumerator has to stat the stem to find anyway.
//
// A WAL path that is a directory used to recover as a clean EMPTY log: walkLog
// opened it, the first read failed, the loop broke, and Recover returned a fresh
// engine and a nil error. An embedder that called Recover without a following Open
// started an empty venue and said nothing.
func TestADirectoryIsNotALog(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	if err := os.Mkdir(stem, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if _, err := ReadAll(stem); !errors.Is(err, ErrNotALog) {
		t.Errorf("ReadAll err = %v, want ErrNotALog", err)
	}
	eng, err := Recover(tapeCfg(), "", stem)
	if !errors.Is(err, ErrNotALog) {
		t.Fatalf("Recover err = %v (engine %v), want ErrNotALog — a directory recovered as an empty venue", err, eng != nil)
	}
	if _, err := Open(stem); !errors.Is(err, ErrNotALog) {
		t.Errorf("Open err = %v, want ErrNotALog", err)
	}
}

// TestAStrayFileIsNotInTheSet. Enumeration is an allow-list because the directory a
// WAL lives in also holds its snapshot, the snapshot's temp file while WriteSnapshot
// works, and whatever else an embedder puts there — examples/multisymbol derives its
// snapshot path as walPath+".snap". A <stem>.* glob would hand a snapshot to a frame
// parser and report the venue's log as corrupt.
func TestAStrayFileIsNotInTheSet(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	writeLog(t, stem, 10)

	for _, name := range []string{
		"w.wal.snap", "w.wal.snap.tmp", "w.wal.0000000000000001.tmp", "w.wal.gz",
		"w.wal.000000000000001", "w.wal.00000000000000001", "w.wal.00000000000000x1",
		"w.wal.0000000000000002.torn", "venue.json",
	} {
		writeFile(t, filepath.Join(dir, name), []byte("not a segment, not remotely"))
	}

	entries, err := ReadAll(stem)
	if err != nil {
		t.Fatalf("ReadAll: %v — a stray file in the directory was parsed as a segment", err)
	}
	if len(entries) != 10 {
		t.Errorf("read %d entries, want 10", len(entries))
	}
	if got := segmentSetOf(t, stem); len(got) != 0 {
		t.Errorf("stray files joined the set: %v", got)
	}
}
