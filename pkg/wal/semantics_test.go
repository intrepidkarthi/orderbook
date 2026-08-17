package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

// The semantics stamp, on disk and at the gate.
//
// docs/SEMANTICS-VERSION.md is the spec. The two things these tests exist to hold are:
// the stamp is WIRED (it comes from matching.SemanticsVersion, not from a literal that
// would diverge the first time the constant moved), and the gate is on the RECORDS
// RECOVERY WOULD REPLAY rather than on the files it can see. The second is what keeps
// the check credible — a check that fires on the happy path is a check that gets
// switched off, and then it is not there on the day it matters.

// --- fixtures ---------------------------------------------------------------

// stampedSet builds a set of segments with the declared semantics named per segment.
// Each entry is (base, semantics, records); a semantics of 0 produces a pre-stamp
// OBWAL\x02 segment, which is the shape every log on every disk has today.
func stampedSet(tb testing.TB, dir string, spans [][3]int) (stem string) {
	tb.Helper()
	stem = filepath.Join(dir, "s.wal")
	writeFile(tb, stem, segHeader(markerBase))
	for _, sp := range spans {
		base, sem, n := int64(sp[0]), sp[1], sp[2]
		seqs := make([]int64, n)
		for i := range seqs {
			seqs[i] = base + int64(i)
		}
		handBuiltSegmentAt(tb, segPath(stem, base), base, sem, seqs)
	}
	return stem
}

// snapshotAt writes a snapshot of an empty book stamped with walSeq and the given
// declared semantics. A semantics of 0 is a snapshot written before the stamp existed.
func snapshotAt(tb testing.TB, path string, walSeq int64, sem int) {
	tb.Helper()
	snap := matching.NewEngine(tapeCfg()).TakeSnapshot()
	snap.WALSeq = walSeq
	snap.Semantics = sem
	if err := WriteSnapshot(path, snap); err != nil {
		tb.Fatalf("WriteSnapshot: %v", err)
	}
}

// --- the stamp on disk ------------------------------------------------------

// TestASegmentDeclaresItsSemantics — deliverable 2. The header is the gate's only
// input, so its shape, its contents and its self-check are all asserted here.
func TestASegmentDeclaresItsSemantics(t *testing.T) {
	dir := t.TempDir()
	walPath, _ := buildRotatedLog(t, dir, 2_000, 0, 16<<10)
	names := segmentSetOf(t, walPath)
	if len(names) < 2 {
		t.Fatalf("the fixture produced %d segments; it is not exercising rotation", len(names))
	}
	newest := filepath.Join(dir, names[len(names)-1])

	raw := readFile(t, newest)
	if string(raw[:len(SegMagicV3)]) != SegMagicV3 {
		t.Fatalf("a freshly written segment opens with %q, want %q", raw[:6], SegMagicV3)
	}
	if got := binary.BigEndian.Uint32(raw[len(SegMagicV3)+8 : len(SegMagicV3)+12]); int(got) != matching.SemanticsVersion {
		t.Errorf("the segment declares semantics %d, want %d", got, matching.SemanticsVersion)
	}

	// The CRC covers the twelve bytes of base AND semantics, as one field. A flipped
	// bit anywhere in them must be refused: the direction that matters is the quiet
	// one, where a flip turns another build's number into this build's and lets a
	// mismatched log through the gate.
	for off := len(SegMagicV3); off < SegHeaderBytesV3-4; off++ {
		damaged := append([]byte(nil), raw...)
		damaged[off] ^= 0x01
		writeFile(t, newest, damaged)
		if _, err := ReadAll(walPath); !errors.Is(err, ErrCorrupt) {
			t.Errorf("a flipped bit at header offset %d was accepted; err = %v", off, err)
		}
	}
	writeFile(t, newest, raw)
	if _, err := ReadAll(walPath); err != nil {
		t.Fatalf("the restored fixture no longer reads: %v", err)
	}
}

// TestTheStampIsWired — deliverable 18, and it is the sabotage in §10 item 6 written
// as an assertion.
//
// A literal 1 in segHeaderV3 and in TakeSnapshot would pass every other test in this
// file: the bytes would be right, the gate would agree with itself, and the two would
// diverge silently the first time matching.SemanticsVersion moved. So this reads both
// artifacts back through the REAL write paths and compares them against the constant.
func TestTheStampIsWired(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "w.wal")
	snapPath := filepath.Join(dir, "w.snap")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := w.AppendSubmit(mustOrder(t, "u", 1000, 1)); err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	eng := matching.NewEngine(tapeCfg())
	if err := Checkpoint(snapPath, eng, 1); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Both write paths, because they are different code and a literal can hide in
	// either. The first segment of a log comes from Open; every one after it comes
	// from materialiseSegment, which is the rotation protocol.
	rotDir := t.TempDir()
	rotPath, _ := buildRotatedLog(t, rotDir, 2_000, 0, 16<<10)

	for _, path := range []string{walPath, rotPath} {
		set, err := enumerateSet(path)
		if err != nil {
			t.Fatalf("enumerateSet %s: %v", path, err)
		}
		if len(set.segs) == 0 {
			t.Fatalf("%s: no segment was written", path)
		}
		for _, seg := range set.segs {
			if seg.semantics != matching.SemanticsVersion {
				t.Errorf("%s declares semantics %d and matching.SemanticsVersion is %d — the stamp is a "+
					"literal somewhere rather than the constant, and the two diverge the moment the "+
					"constant moves while every other test still passes",
					seg.name, seg.semantics, matching.SemanticsVersion)
			}
		}
	}
	snap, err := ReadSnapshot(snapPath)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if snap.Semantics != matching.SemanticsVersion {
		t.Errorf("the snapshot this build wrote declares semantics %d, want %d", snap.Semantics, matching.SemanticsVersion)
	}
}

// TestASnapshotDeclaresItsSemantics and TestTheDigestIsBlindToTheStamp — deliverable 3.
//
// The second half is the one with an argument behind it, and there are two. A digest
// that reported "different" while two books were byte-identical would stop being
// usable for the thing it exists for (a follower comparison). And internal/semcheck's
// fingerprint contains snapshot digests, so a Semantics inside the digest would make
// every bump satisfy its own evidence — bump, fingerprint moves, regenerate, done —
// and Rule 22 would be unenforceable.
func TestASnapshotDeclaresItsSemantics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.snap")
	eng := matching.NewEngine(tapeCfg())
	if err := Checkpoint(path, eng, 7); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	snap, err := ReadSnapshot(path)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if snap.Semantics != matching.SemanticsVersion {
		t.Errorf("snapshot semantics %d, want %d", snap.Semantics, matching.SemanticsVersion)
	}

	// A snapshot written before the stamp existed carries no field at all, and reads
	// back as 0 rather than as an error.
	legacy := filepath.Join(dir, "old.snap")
	old := matching.NewEngine(tapeCfg()).TakeSnapshot()
	old.Semantics = 0
	old.WALSeq = 7
	if err := WriteSnapshot(legacy, old); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if raw := readFile(t, legacy); strings.Contains(string(raw), "semantics") {
		t.Error("an unstamped snapshot wrote a semantics field; omitempty keeps pre-stamp files byte-identical")
	}
	back, err := ReadSnapshot(legacy)
	if err != nil {
		t.Fatalf("ReadSnapshot on an unstamped snapshot: %v", err)
	}
	if back.Semantics != 0 {
		t.Errorf("an unstamped snapshot reports semantics %d, want 0 (unknown)", back.Semantics)
	}
}

func TestTheDigestIsBlindToTheStamp(t *testing.T) {
	eng := matching.NewEngine(tapeCfg())
	if _, err := eng.Cancel(1, "u"); err == nil {
		t.Fatal("the fixture's premise is a cancel of nothing")
	}
	a := eng.TakeSnapshot()
	b := eng.TakeSnapshot()
	a.Semantics, b.Semantics = 1, 99
	if a.Digest() != b.Digest() {
		t.Errorf("two snapshots differing only in Semantics have different digests.\n"+
			"Two engines whose books are identical must compare equal even when one is a build ahead, or the "+
			"digest stops being usable for the follower comparison it exists for. And internal/semcheck's "+
			"fingerprint contains digests, so a bump would move the fingerprint on its own and every bump "+
			"would satisfy its own evidence.\n a=%s\n b=%s", a.Digest(), b.Digest())
	}
}

// --- the gate refuses what it must -----------------------------------------

// TestARecordFromAnEarlierSemanticsIsRefused — deliverable 4.
func TestARecordFromAnEarlierSemanticsIsRefused(t *testing.T) {
	dir := t.TempDir()
	// Segment 1 declares nothing (pre-stamp), segment 101 declares this build's.
	stem := stampedSet(t, dir, [][3]int{{1, 0, 100}, {101, matching.SemanticsVersion, 100}})
	snapPath := filepath.Join(dir, "s.snap")
	// The snapshot sits INSIDE the mismatched segment, so 50 of its records would be
	// replayed.
	snapshotAt(t, snapPath, 50, matching.SemanticsVersion)

	eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, stem)
	if !errors.Is(err, ErrSemanticsMismatch) {
		t.Fatalf("Recover err = %v, want ErrSemanticsMismatch", err)
	}
	if eng != nil {
		t.Error("a refused recovery returned an engine; the caller would serve a book from it")
	}
	msg := err.Error()
	// Built from the constant, never from a literal: a test that hard-codes this
	// build's number stops testing the message the day the number moves, which is
	// precisely the day the message matters most.
	for _, want := range []string{
		fmt.Sprintf("semantics %d", matching.SemanticsVersion), // this build's number
		"s.wal.0000000000000001",                               // the segment
		"1..100",                                               // its sequence range
		"50 would be replayed",                                 // the count
		"-wal-accept-semantics 0",
		"RUNBOOKS.md",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q. An operator meeting this at 3am has to be able to tell a "+
				"whole segment from one record and a checkpoint from a downgrade.\n%s", want, msg)
		}
	}
	if rep.Semantics != matching.SemanticsVersion {
		t.Errorf("report names semantics %d, want %d", rep.Semantics, matching.SemanticsVersion)
	}
}

// TestAMismatchedSegmentInTheMiddleOfTheReplaySetIsRefused — deliverable 7. The gate
// is not "the oldest" or "the newest": it is every segment that contributes a record.
func TestAMismatchedSegmentInTheMiddleOfTheReplaySetIsRefused(t *testing.T) {
	dir := t.TempDir()
	v := matching.SemanticsVersion
	stem := stampedSet(t, dir, [][3]int{{1, v, 50}, {51, 0, 50}, {101, v, 50}})

	_, _, err := RecoverWithReport(tapeCfg(), "", stem)
	if !errors.Is(err, ErrSemanticsMismatch) {
		t.Fatalf("Recover err = %v, want ErrSemanticsMismatch", err)
	}
	if !strings.Contains(err.Error(), "s.wal.0000000000000051") {
		t.Errorf("the refusal does not name the middle segment: %v", err)
	}
	if strings.Contains(err.Error(), "0000000000000001") || strings.Contains(err.Error(), "0000000000000101") {
		t.Errorf("the refusal names a segment that matches this build: %v", err)
	}
}

// TestAnUnstampedSegmentWithRecordsToApplyIsRefused — deliverable 8, on all three
// pre-stamp shapes. Unknown is not this build's, and the three shapes that declare
// nothing are the entire installed base.
func TestAnUnstampedSegmentWithRecordsToApplyIsRefused(t *testing.T) {
	seqs := make([]int64, 12)
	for i := range seqs {
		seqs[i] = int64(i + 1)
	}
	cases := []struct {
		name  string
		build func(tb testing.TB, dir string) string
	}{
		{"OBWAL\\x02 segment set", func(tb testing.TB, dir string) string {
			return stampedSet(tb, dir, [][3]int{{1, 0, 12}})
		}},
		{"OBWAL\\x01 single file", func(tb testing.TB, dir string) string {
			path := filepath.Join(dir, "h.wal")
			handBuiltLog(tb, path, seqs)
			return path
		}},
		{"headerless v1 file", func(tb testing.TB, dir string) string {
			modern := filepath.Join(dir, "m.wal")
			handBuiltLog(tb, modern, seqs)
			return stripToV1(tb, dir, modern)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := c.build(t, dir)

			if _, err := Recover(tapeCfg(), "", path); !errors.Is(err, ErrSemanticsMismatch) {
				t.Fatalf("Recover err = %v, want ErrSemanticsMismatch — treating unknown as compatible is "+
					"affirmatively false today, because a pre-stamp log is exactly the one that does not have "+
					"the three matching changes semantics 1 records", err)
			}
			// And the diagnostic reader is unaffected: an operator during an incident
			// must still be able to look at the file.
			if entries, err := ReadAll(path); err != nil || len(entries) != 12 {
				t.Errorf("ReadAll = %d entries, %v — the diagnostic reader must never refuse", len(entries), err)
			}
			// Naming the version starts it.
			eng, rep, err := RecoverWithOptions(tapeCfg(), "", path, RecoverOptions{AcceptSemantics: []int{0}})
			if err != nil {
				t.Fatalf("RecoverWithOptions: %v", err)
			}
			if eng.OrderCount() != 12 {
				t.Errorf("recovered %d orders, want 12", eng.OrderCount())
			}
			if !rep.SemanticsAccepted {
				t.Error("report does not say an override let pre-stamp records through")
			}
		})
	}
}

// --- the gate accepts what it must ------------------------------------------

// TestAMismatchedSegmentBehindTheSnapshotStarts — deliverable 5, and the direction
// nobody tests for.
//
// Over-refusal is what produces the permanent override, and the override is what makes
// the whole mechanism worthless. A segment entirely behind the snapshot boundary
// contributes nothing to the recovered book — RestoreAfter drops it by sequence
// whatever it contains — so refusing on it is refusing on a file that could be deleted
// with no effect.
func TestAMismatchedSegmentBehindTheSnapshotStarts(t *testing.T) {
	v := matching.SemanticsVersion
	mixedDir := t.TempDir()
	mixed := stampedSet(t, mixedDir, [][3]int{{1, 0, 100}, {101, v, 100}})
	mixedSnap := filepath.Join(mixedDir, "s.snap")
	snapshotAt(t, mixedSnap, 100, v)

	cleanDir := t.TempDir()
	clean := stampedSet(t, cleanDir, [][3]int{{1, v, 100}, {101, v, 100}})
	cleanSnap := filepath.Join(cleanDir, "s.snap")
	snapshotAt(t, cleanSnap, 100, v)

	eng, rep, err := RecoverWithReport(tapeCfg(), mixedSnap, mixed)
	if err != nil {
		t.Fatalf("a set whose mismatched segment is entirely covered refused to start: %v", err)
	}
	want, _, err := RecoverWithReport(tapeCfg(), cleanSnap, clean)
	if err != nil {
		t.Fatalf("the all-matching control refused: %v", err)
	}
	if got, wantDigest := eng.TakeSnapshot().Digest(), want.TakeSnapshot().Digest(); got != wantDigest {
		t.Errorf("the recovered book differs from the all-matching control\n got %s\nwant %s", got, wantDigest)
	}
	if len(rep.LogSemantics) != 2 || rep.LogSemantics[0] != 0 || rep.LogSemantics[1] != v {
		t.Errorf("report LogSemantics = %v, want [0 %d] — a set that spans an upgrade is a fact to report, "+
			"not a corruption", rep.LogSemantics, v)
	}
	if rep.SemanticsAccepted {
		t.Error("report claims an override was used; nothing from the mismatched segment was replayed")
	}
	if rep.Applied != 100 {
		t.Errorf("applied %d records, want 100", rep.Applied)
	}
}

// TestASnapshotAheadOfAMismatchedNewestSegmentStarts — deliverable 6, and the reason
// the newest segment waits for the walk.
//
// Gating the newest segment optimistically ("it always contributes") would refuse here,
// and this state follows an ORDINARY POWER LOSS: a checkpoint does not sync the log
// first, so a snapshot can be durable while records it covers are still buffered. A
// false refusal on a benign crash is exactly the noise that gets the check switched off.
func TestASnapshotAheadOfAMismatchedNewestSegmentStarts(t *testing.T) {
	dir := t.TempDir()
	stem := stampedSet(t, dir, [][3]int{{1, 0, 40}})
	snapPath := filepath.Join(dir, "s.snap")
	snapshotAt(t, snapPath, 60, matching.SemanticsVersion)

	eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, stem)
	if err != nil {
		t.Fatalf("a snapshot ahead of a mismatched log refused to start: %v", err)
	}
	if !rep.SnapshotAhead {
		t.Error("report does not name the snapshot-ahead condition")
	}
	if rep.Applied != 0 {
		t.Errorf("applied %d records, want 0 — everything the log holds is already in the snapshot", rep.Applied)
	}
	if eng.OrderCount() != 0 {
		t.Errorf("recovered %d orders from an empty snapshot, want 0", eng.OrderCount())
	}
}

// TestASnapshotFromAnotherBuildIsNeverAGate — Rule 4. Restoring a book an older build
// actually had is the documented upgrade procedure; gating on it would refuse the
// procedure, and would give the design two gates with two rules for an operator to
// learn one override for.
func TestASnapshotFromAnotherBuildIsNeverAGate(t *testing.T) {
	dir := t.TempDir()
	v := matching.SemanticsVersion
	stem := stampedSet(t, dir, [][3]int{{1, v, 40}})
	snapPath := filepath.Join(dir, "s.snap")

	for _, sem := range []int{0, v, v + 7} {
		snapshotAt(t, snapPath, 10, sem)
		_, rep, err := RecoverWithReport(tapeCfg(), snapPath, stem)
		if err != nil {
			t.Fatalf("a snapshot declaring semantics %d refused to start: %v", sem, err)
		}
		if rep.SnapshotSemantics != sem {
			t.Errorf("report says the snapshot declares %d, want %d", rep.SnapshotSemantics, sem)
		}
		if rep.Applied != 30 {
			t.Errorf("applied %d records, want 30", rep.Applied)
		}
	}
}

// TestLogAndSnapshotDisagreeing is the ordinary state after the documented upgrade:
// the snapshot was written by the old build and the log by the new one. It must start
// with no ceremony at all — that property is what keeps the check credible.
func TestLogAndSnapshotDisagreeing(t *testing.T) {
	dir := t.TempDir()
	v := matching.SemanticsVersion
	stem := stampedSet(t, dir, [][3]int{{1, v, 60}})
	snapPath := filepath.Join(dir, "s.snap")
	snapshotAt(t, snapPath, 20, 0)

	_, rep, err := RecoverWithReport(tapeCfg(), snapPath, stem)
	if err != nil {
		t.Fatalf("a pre-stamp snapshot under a stamped log refused to start: %v", err)
	}
	if rep.SnapshotSemantics != 0 || len(rep.LogSemantics) != 1 || rep.LogSemantics[0] != v {
		t.Errorf("report = snapshot %d / log %v, want 0 / [%d]", rep.SnapshotSemantics, rep.LogSemantics, v)
	}
	if rep.SemanticsAccepted {
		t.Error("nothing was overridden; the log is this build's")
	}
}

// TestAnUpToDateSetStartsWithNoCeremony is the happy path stated as an assertion. A
// check that fires when nothing is wrong is a check operators learn to switch off, so
// "an ordinary venue never meets it" is a property worth a test of its own.
func TestAnUpToDateSetStartsWithNoCeremony(t *testing.T) {
	dir := t.TempDir()
	walPath, snapPath := buildRotatedLog(t, dir, 2_000, 900, 16<<10)
	_, rep, err := RecoverWithReport(tapeCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("a set written entirely by this build refused to start: %v", err)
	}
	if rep.SemanticsAccepted {
		t.Error("report claims an override was needed on a set this build wrote")
	}
	if len(rep.LogSemantics) != 1 || rep.LogSemantics[0] != matching.SemanticsVersion {
		t.Errorf("report LogSemantics = %v, want [%d]", rep.LogSemantics, matching.SemanticsVersion)
	}
}

// --- the override -----------------------------------------------------------

// TestAnOverrideNamesTheVersionItAccepts — deliverable 9, and Rule 12 is the point.
//
// A boolean survives the next bump and stops being a decision. A version goes stale and
// forces the decision to be re-made, which is the same device internal/apicheck uses
// when it makes you regenerate a golden and read the diff.
func TestAnOverrideNamesTheVersionItAccepts(t *testing.T) {
	dir := t.TempDir()
	stem := stampedSet(t, dir, [][3]int{{1, 0, 40}})

	full, _, err := RecoverWithOptions(tapeCfg(), "", stem, RecoverOptions{AcceptSemantics: []int{0}})
	if err != nil {
		t.Fatalf("naming 0 did not accept an unstamped log: %v", err)
	}
	if full.OrderCount() != 40 {
		t.Errorf("recovered %d orders, want 40", full.OrderCount())
	}

	// An override naming 0 must not accept a segment stamped with something else.
	other := t.TempDir()
	stamped := stampedSet(t, other, [][3]int{{1, matching.SemanticsVersion + 5, 40}})
	if _, _, err := RecoverWithOptions(tapeCfg(), "", stamped, RecoverOptions{AcceptSemantics: []int{0}}); !errors.Is(err, ErrSemanticsMismatch) {
		t.Errorf("an override naming 0 accepted a segment declaring %d; it is a list of versions, not a "+
			"permission to ignore the check. err = %v", matching.SemanticsVersion+5, err)
	}

	// And it relaxes the semantics gate and nothing else. An operator reaching for it
	// during an incident must not acquire a second, unrelated permission by accident.
	t.Run("does not relax ErrCorrupt", func(t *testing.T) {
		d := t.TempDir()
		s := stampedSet(t, d, [][3]int{{1, 0, 40}})
		raw := readFile(t, segPath(s, 1))
		frames := walFrames(t, raw)
		raw[frames[5].payload+2]++
		writeFile(t, segPath(s, 1), raw)
		_, _, err := RecoverWithOptions(tapeCfg(), "", s, RecoverOptions{AcceptSemantics: []int{0}})
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("err = %v, want ErrCorrupt", err)
		}
	})
	t.Run("does not relax ErrLogGap", func(t *testing.T) {
		d := t.TempDir()
		s := stampedSet(t, d, [][3]int{{1, 0, 40}, {101, 0, 40}})
		_, _, err := RecoverWithOptions(tapeCfg(), "", s, RecoverOptions{AcceptSemantics: []int{0}})
		if !errors.Is(err, ErrLogGap) {
			t.Fatalf("err = %v, want ErrLogGap", err)
		}
	})
	t.Run("does not relax the retention floor", func(t *testing.T) {
		d := t.TempDir()
		s := stampedSet(t, d, [][3]int{{500, 0, 40}})
		snapPath := filepath.Join(d, "s.snap")
		snapshotAt(t, snapPath, 100, matching.SemanticsVersion)
		_, _, err := RecoverWithOptions(tapeCfg(), snapPath, s, RecoverOptions{AcceptSemantics: []int{0}})
		if !errors.Is(err, ErrLogGap) {
			t.Fatalf("err = %v, want ErrLogGap", err)
		}
	})
}

// --- Open, which never refuses ----------------------------------------------

// TestOpenAcceptsWhatRecoverRefuses — deliverable 10, and BOUNDED-RECOVERY.md §9.1's
// rule that Open is the most permissive reader in the package.
//
// cmd/obgw calls Recover and then Open on the same path, so a stricter Open means a
// venue that recovers its book successfully and then cannot open the log it just
// recovered from: an outage manufactured by two readers of the same bytes disagreeing.
func TestOpenAcceptsWhatRecoverRefuses(t *testing.T) {
	dir := t.TempDir()
	stem := stampedSet(t, dir, [][3]int{{1, 0, 100}, {101, matching.SemanticsVersion, 100}})
	snapPath := filepath.Join(dir, "s.snap")
	snapshotAt(t, snapPath, 50, matching.SemanticsVersion)

	if _, err := Recover(tapeCfg(), snapPath, stem); !errors.Is(err, ErrSemanticsMismatch) {
		t.Fatalf("the fixture does not refuse: %v", err)
	}
	w, err := Open(stem)
	if err != nil {
		t.Fatalf("Open refused a log Recover refused: %v", err)
	}
	if _, err := w.AppendSubmit(mustOrder(t, "u", 1000, 1)); err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// ReadAll is the diagnostic reader RUNBOOKS sends an operator to during an
	// incident, and a diagnostic that refuses to show you the file is not one.
	if _, err := ReadAll(stem); err != nil {
		t.Errorf("ReadAll refused: %v", err)
	}
}

// TestUpgradingSealsTheOldSegment — deliverable 11, Rule 15.
//
// Without it the stamp is a lie in the one direction that matters: the active segment
// would declare one semantics and hold records from two, so the header stops describing
// its own contents, which is the only thing it was for.
func TestUpgradingSealsTheOldSegment(t *testing.T) {
	dir := t.TempDir()
	stem := stampedSet(t, dir, [][3]int{{1, matching.SemanticsVersion, 40}, {41, 0, 40}})
	before := readFile(t, segPath(stem, 41))

	w, err := Open(stem)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	path, base := w.ActiveSegment()
	if base != 81 {
		t.Errorf("the active segment is based at %d, want 81 (last+1)", base)
	}
	if filepath.Base(path) == filepath.Base(segPath(stem, 41)) {
		t.Fatal("Open kept appending to a segment that declares a different semantics")
	}
	if _, err := w.AppendSubmit(mustOrder(t, "u", 1000, 1)); err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if after := readFile(t, segPath(stem, 41)); string(before) != string(after) {
		t.Errorf("the sealed segment changed: %d bytes before, %d after", len(before), len(after))
	}
	set, err := enumerateSet(stem)
	if err != nil {
		t.Fatalf("enumerateSet: %v", err)
	}
	newest, _ := set.newest()
	if newest.semantics != matching.SemanticsVersion {
		t.Errorf("the new segment declares semantics %d, want %d", newest.semantics, matching.SemanticsVersion)
	}
	entries, err := ReadAll(stem)
	if err != nil {
		t.Fatalf("ReadAll over the sealed set: %v", err)
	}
	if len(entries) != 81 || entries[80].Seq != 81 {
		t.Errorf("the set holds %d records ending at %d, want 81 ending at 81 — contiguity broke",
			len(entries), entries[len(entries)-1].Seq)
	}
}

// TestAnEmptyMismatchedSegmentIsReplacedNotRotated — deliverable 12, Rule 16.
//
// Rotation starts the next segment at last+1, and an empty segment's last is base−1, so
// the new segment would claim the base the old one already has and collide with its own
// filename: EEXIST at exactly the moment a venue is trying to start. The general path is
// arithmetically impossible on this input, which is why the special case is here.
func TestAnEmptyMismatchedSegmentIsReplacedNotRotated(t *testing.T) {
	dir := t.TempDir()
	stem := stampedSet(t, dir, [][3]int{{1, matching.SemanticsVersion, 40}, {41, 0, 0}})

	w, err := Open(stem)
	if err != nil {
		t.Fatalf("Open on an empty mismatched segment: %v", err)
	}
	path, base := w.ActiveSegment()
	if base != 41 {
		t.Errorf("the active segment is based at %d, want 41 — the base of a record-free segment is preserved", base)
	}
	if _, err := w.AppendSubmit(mustOrder(t, "u", 1000, 1)); err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if raw := readFile(t, path); string(raw[:len(SegMagicV3)]) != SegMagicV3 {
		t.Errorf("the replaced header is %q, want %q", raw[:6], SegMagicV3)
	}
	if names := segmentSetOf(t, stem); len(names) != 2 {
		t.Errorf("the set is %v; replacing a header must not add a segment", names)
	}
	entries, err := ReadAll(stem)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 41 || entries[40].Seq != 41 {
		t.Errorf("the set holds %d records ending at %d, want 41 ending at 41", len(entries), entries[len(entries)-1].Seq)
	}
	// No temp is left behind, and no stale one would be read.
	if _, err := os.Lstat(path + ".hdr.tmp"); !os.IsNotExist(err) {
		t.Error("the header replacement left its temp file behind")
	}
}

// TestAfterAnUpgradeACrashRecoveryNoLongerRefuses — deliverable 13.
//
// This is what makes the condition SELF-HEALING rather than permanent. Without the
// Open-time seal a venue that upgraded correctly meets the refusal on every crash
// recovery until the segment fills, which at the 128 MiB default is a long time to be
// one power loss away from an outage. The SECOND crash is the one that shows it.
func TestAfterAnUpgradeACrashRecoveryNoLongerRefuses(t *testing.T) {
	dir := t.TempDir()
	stem := stampedSet(t, dir, [][3]int{{1, 0, 20}})
	snapPath := filepath.Join(dir, "s.snap")
	// The venue checkpointed under the old build, so nothing from the old segment is
	// left to replay. This is the documented upgrade path.
	snapshotAt(t, snapPath, 20, 0)

	for crash := 1; crash <= 3; crash++ {
		w, err := Open(stem)
		if err != nil {
			t.Fatalf("crash %d: Open: %v", crash, err)
		}
		for i := 0; i < 3; i++ {
			if _, err := w.AppendSubmit(mustOrder(t, "u", 1000, 1)); err != nil {
				t.Fatalf("crash %d: AppendSubmit: %v", crash, err)
			}
		}
		if err := w.Close(); err != nil { // the kill: everything is on disk, nothing checkpointed
			t.Fatalf("crash %d: Close: %v", crash, err)
		}
		if _, _, err := RecoverWithReport(tapeCfg(), snapPath, stem); err != nil {
			t.Fatalf("crash %d: recovery refused after an upgrade that sealed the old segment: %v\n"+
				"Without the Open-time seal this recurs on every crash until the segment fills.", crash, err)
		}
	}
}

// TestTheGateRunsBeforeAnyRecordIsApplied is Rule 11's two stages asserted by effect
// rather than by structure: a sealed segment that mismatches is refused from the
// directory alone, so a set whose LATER segments are unreadable still refuses with the
// semantics error rather than with a corruption error from four hundred thousand
// records later.
func TestTheGateRunsBeforeAnyRecordIsApplied(t *testing.T) {
	dir := t.TempDir()
	v := matching.SemanticsVersion
	stem := stampedSet(t, dir, [][3]int{{1, 0, 40}, {41, v, 40}})
	// Damage a record in the LATER segment. The sealed-segment gate runs first, from
	// the directory, so the semantics error is what an operator sees.
	raw := readFile(t, segPath(stem, 41))
	frames := walFrames(t, raw)
	raw[frames[3].payload+2]++
	writeFile(t, segPath(stem, 41), raw)

	_, _, err := RecoverWithReport(tapeCfg(), "", stem)
	if !errors.Is(err, ErrSemanticsMismatch) {
		t.Fatalf("err = %v, want ErrSemanticsMismatch — the sealed-segment gate must run before the walk", err)
	}
}
