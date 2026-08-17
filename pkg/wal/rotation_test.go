package wal

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Rotation, and the one thing it must not break.
//
// Slice 1 skips a covered prefix by treating a record's position in the file as its
// sequence, and it VERIFIES that assumption: a disagreement discards the skip and
// re-reads everything. A rotated segment begins at position 1 carrying sequence
// k >> 1, so the naive segmentation makes that fallback fire on every rotated log.
// The book would still be right. The venue would be slower to restart than it was
// before slice 1 existed, every restart, and the only trace would be one log line.
//
// TestADeclaredSegmentDoesNotFallBack is the only test in this repository that fails
// if that happens. Everything else here would pass.

// segmentSetOf lists the numbered members of the set at stem, in base order.
func segmentSetOf(tb testing.TB, stem string) []string {
	tb.Helper()
	info, err := Stat(stem)
	if err != nil {
		tb.Fatalf("Stat: %v", err)
	}
	var names []string
	for _, n := range info.Names {
		if _, ok := segBaseFromName(filepath.Base(stem), n); ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// buildRotatedLog writes n resting submit records through a Writer whose segments
// are segBytes, checkpointing after the at-th (at == 0 writes no snapshot). It is
// buildSnapshottedLog with rotation on, so the two can be compared record for
// record and book for book.
func buildRotatedLog(tb testing.TB, dir string, n, at int, segBytes int64) (walPath, snapPath string) {
	tb.Helper()
	walPath = filepath.Join(dir, "w.wal")
	snapPath = filepath.Join(dir, "s.snap")

	w, err := OpenWith(walPath, Options{MaxSegmentBytes: segBytes})
	if err != nil {
		tb.Fatalf("OpenWith: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), QueueSize: 4096, Log: w})
	for i := 0; i < n; i++ {
		o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
			int64(1000+i%50), 1, types.TIFGoodTillCancel)
		if err != nil {
			tb.Fatalf("NewOrder: %v", err)
		}
		r.Process(o)
		if at > 0 && i+1 == at {
			snap, err := r.Checkpoint()
			if err != nil {
				tb.Fatalf("Checkpoint: %v", err)
			}
			if snap.WALSeq != int64(at) {
				tb.Fatalf("checkpoint after %d records is stamped WALSeq %d", at, snap.WALSeq)
			}
			if err := WriteSnapshot(snapPath, snap); err != nil {
				tb.Fatalf("WriteSnapshot: %v", err)
			}
		}
	}
	r.Close()
	if err := w.Close(); err != nil {
		tb.Fatalf("wal Close: %v", err)
	}
	return walPath, snapPath
}

// TestADeclaredSegmentDoesNotFallBack is the assertion this whole slice turns on.
//
// A properly rotated set, recovered against a snapshot that falls INSIDE a segment
// whose base is far above 1, must produce the book the equivalent single file
// produces and must report FellBack == false. Revert the per-segment arithmetic to
// slice 1's bare ordinal and this is the only thing in the repository that fails:
// every other test still passes, the recovered book is still correct, and the venue
// is slower to restart than it was before slice 1 was written.
func TestADeclaredSegmentDoesNotFallBack(t *testing.T) {
	const (
		records = 3_000
		at      = 2_000
	)
	// Small segments so the snapshot lands well inside a segment based far above 1,
	// which is the shape a single file can never produce.
	rotated := t.TempDir()
	rotWAL, rotSnap := buildRotatedLog(t, rotated, records, at, 16<<10)
	names := segmentSetOf(t, rotWAL)
	if len(names) < 4 {
		t.Fatalf("the fixture produced %d segments (%v); it is not exercising rotation", len(names), names)
	}

	// The same command stream in one file, which is the oracle.
	single := t.TempDir()
	oneWAL, oneSnap := buildSnapshottedLog(t, single, records, at)

	eng, rep, err := RecoverWithReport(tapeCfg(), rotSnap, rotWAL)
	if err != nil {
		t.Fatalf("RecoverWithReport over a rotated set: %v", err)
	}
	if rep.FellBack {
		t.Fatalf("recovery fell back to a full re-read on an ordinary rotated set.\n"+
			"Every segment after the first begins at position 1 carrying a sequence far above 1, so a reader that\n"+
			"treats position as sequence declares the file suspect and reads it twice. The book is still correct and\n"+
			"the venue is SLOWER to restart than it was before the covered-prefix skip existed, with no error anywhere.\n"+
			"The per-segment arithmetic is seq = base + ordinal - 1, with base from the segment header.\n"+
			"report = %+v, segments = %v", rep, names)
	}
	if got, want := eng.TakeSnapshot().Digest(), fullParse(t, oneSnap, oneWAL); got != want {
		t.Fatalf("a rotated set recovered a different book than the same stream in one file\n got %s\nwant %s", got, want)
	}
	if rep.Skipped != at || rep.Applied != records-at {
		t.Errorf("report says %d skipped / %d applied, want %d / %d", rep.Skipped, rep.Applied, at, records-at)
	}
	if rep.Segments != len(names) {
		t.Errorf("report counted %d segments, want %d", rep.Segments, len(names))
	}
	if eng.OrderCount() != records {
		t.Errorf("recovered %d orders, want %d", eng.OrderCount(), records)
	}
}

// TestSegmentNameAndHeaderMustAgree — the two declarations of a base are written by
// one process in one operation, so they can only disagree if the file was renamed,
// copied or restored from a backup. The reader cannot tell which of them is lying,
// and both choices are bad: trusting the name puts records into the wrong sequence
// space, trusting the header puts the set in the wrong order.
//
// This is also the test that catches deriving the base from record 1's Seq — the
// tempting shortcut, since record 1 is parsed anyway. That sabotage leaves
// TestADeclaredSegmentDoesNotFallBack passing, which is the point of writing both.
func TestSegmentNameAndHeaderMustAgree(t *testing.T) {
	dir := t.TempDir()
	walPath, _ := buildRotatedLog(t, dir, 2_000, 0, 16<<10)
	names := segmentSetOf(t, walPath)
	if len(names) < 3 {
		t.Fatalf("fixture produced %d segments, want at least 3", len(names))
	}

	// Rename the second segment to claim a base it does not declare.
	victim := filepath.Join(dir, names[1])
	renamed := segPath(walPath, 999_999)
	if err := os.Rename(victim, renamed); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	_, err := ReadAll(walPath)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAll err = %v, want ErrCorrupt — a renamed segment lies about which commands it holds", err)
	}
	// Both numbers, so an operator can see which one moved.
	if !strings.Contains(err.Error(), "999999") {
		t.Errorf("error does not name the base the FILENAME declares: %v", err)
	}
	if !strings.Contains(err.Error(), strings.TrimLeft(names[1][len(names[1])-16:], "0")) {
		t.Errorf("error does not name the base the HEADER declares: %v", err)
	}
	if _, err := Recover(tapeCfg(), "", walPath); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Recover err = %v, want ErrCorrupt", err)
	}
	if _, err := Open(walPath); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Open err = %v, want ErrCorrupt", err)
	}
}

// TestSegmentHeaderChecksumIsChecked — the base sequence is the one number the whole
// design rests on. An undetected bit flip in it shifts a segment's entire sequence
// space, which either double-applies or silently drops a run of commands, so the
// header carries four bytes to make it self-checking.
func TestSegmentHeaderChecksumIsChecked(t *testing.T) {
	dir := t.TempDir()
	walPath, _ := buildRotatedLog(t, dir, 2_000, 0, 16<<10)
	names := segmentSetOf(t, walPath)

	victim := filepath.Join(dir, names[1])
	raw := readFile(t, victim)
	raw[len(SegMagic)+7]++ // the low byte of the base, leaving the CRC stale
	writeFile(t, victim, raw)

	if _, err := ReadAll(walPath); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAll err = %v, want ErrCorrupt from a damaged segment header", err)
	}
}

// TestARecordNeverStraddlesASegment — append writes the frame header and the payload
// as two separate buffered writes. A rotation between them leaves a header in
// segment n whose payload is in segment n+1: unreadable by every reader in this
// package, and indistinguishable from a torn tail followed by a segment of garbage.
//
// Deciding rotation before the header write is what makes it impossible. This test
// is what notices if the decision moves.
func TestARecordNeverStraddlesASegment(t *testing.T) {
	const n = 20_000
	dir := t.TempDir()
	rotWAL, _ := buildRotatedLog(t, dir, n, 0, 64<<10)

	names := segmentSetOf(t, rotWAL)
	if len(names) < 10 {
		t.Fatalf("fixture produced %d segments, want at least 10", len(names))
	}
	// Every segment's final record is complete: walking each one in isolation
	// consumes it to the last byte.
	set, err := enumerateSet(rotWAL)
	if err != nil {
		t.Fatalf("enumerateSet: %v", err)
	}
	for _, seg := range set.segs {
		var out []Entry
		sw, err := walkSegment(seg, 0, &out)
		if err != nil {
			t.Fatalf("%s: %v", seg.name, err)
		}
		if sw.torn {
			t.Errorf("%s ends %d bytes short of its size — a record straddles the boundary", seg.name, seg.size-sw.end)
		}
		if sw.records == 0 {
			t.Errorf("%s holds no complete records", seg.name)
		}
	}

	// And the set reads back as exactly the same stream one file would hold.
	single := t.TempDir()
	oneWAL, _ := buildSnapshottedLog(t, single, n, 0)
	got, err := ReadAll(rotWAL)
	if err != nil {
		t.Fatalf("ReadAll over the set: %v", err)
	}
	want, err := ReadAll(oneWAL)
	if err != nil {
		t.Fatalf("ReadAll over the single file: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("the set holds %d records and the single file holds %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Seq != want[i].Seq || got[i].Kind != want[i].Kind {
			t.Fatalf("record %d differs: %+v vs %+v", i+1, got[i], want[i])
		}
	}
}

// TestRotatedSetRecoversTheSameBookAsOneFile is the equivalence property across a
// set, at every snapshot boundary, for sets of several sizes.
//
// It is TestSkippedRecoveryMatchesFullParseAtEveryBoundary's shape with the log cut
// into pieces. Every boundary matters for the same reason it did there: an off-by-one
// where a segment begins is invisible to anything coarser than one duplicated or one
// missing order.
func TestRotatedSetRecoversTheSameBookAsOneFile(t *testing.T) {
	const n = 200
	for _, segBytes := range []int64{1 << 30, 40 << 10, 8 << 10, 1500} {
		t.Run(fmt.Sprintf("segbytes%d", segBytes), func(t *testing.T) {
			dir := t.TempDir()
			walPath := filepath.Join(dir, "w.wal")

			w, err := OpenWith(walPath, Options{MaxSegmentBytes: segBytes})
			if err != nil {
				t.Fatalf("OpenWith: %v", err)
			}
			r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), QueueSize: 4096, Log: w})
			snaps := make([]*matching.EngineSnapshot, n+1)
			for i := 0; i < n; i++ {
				o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
					int64(1000+i%50), 1, types.TIFGoodTillCancel)
				if err != nil {
					t.Fatalf("NewOrder: %v", err)
				}
				r.Process(o)
				snap, err := r.Checkpoint()
				if err != nil {
					t.Fatalf("Checkpoint: %v", err)
				}
				snaps[i+1] = snap
			}
			live := snaps[n].Digest()
			r.Close()
			if err := w.Close(); err != nil {
				t.Fatalf("wal Close: %v", err)
			}
			t.Logf("%d segments: %v", len(segmentSetOf(t, walPath)), segmentSetOf(t, walPath))

			for k := 1; k <= n; k++ {
				snapPath := filepath.Join(dir, fmt.Sprintf("s%d.snap", k))
				if err := WriteSnapshot(snapPath, snaps[k]); err != nil {
					t.Fatalf("WriteSnapshot: %v", err)
				}
				eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, walPath)
				if err != nil {
					t.Fatalf("boundary %d: RecoverWithReport: %v", k, err)
				}
				if got := eng.TakeSnapshot().Digest(); got != live {
					t.Fatalf("boundary %d: recovered book differs from the uninterrupted run\n got %s\nwant %s", k, got, live)
				}
				if rep.FellBack {
					t.Fatalf("boundary %d: fell back to a full re-read on a set this package wrote", k)
				}
				if rep.Skipped != k || rep.Applied != n-k {
					t.Fatalf("boundary %d: report says %d skipped / %d applied, want %d / %d", k, rep.Skipped, rep.Applied, k, n-k)
				}
			}
		})
	}
}

// TestCrashDuringRotation walks the crash matrix of docs/LOG-ROTATION.md §3.3.
//
// Rotation is eight steps and a crash between any two of them leaves a different
// state on disk. Every one of them must recover to the same book, and the one state
// the protocol is built to make unreachable — a segment that exists without a header
// — must actually be unreachable.
func TestCrashDuringRotation(t *testing.T) {
	const (
		before = 400 // records written before the crash point
		after  = 200 // records written by the restarted venue
	)
	// The book the same command stream reaches with no crash in it.
	want := func() string {
		dir := t.TempDir()
		walPath := filepath.Join(dir, "w.wal")
		w, err := OpenWith(walPath, Options{MaxSegmentBytes: 8 << 10})
		if err != nil {
			t.Fatalf("OpenWith: %v", err)
		}
		eng := placeThrough(t, w, 1, before+after)
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return eng
	}()

	for step := 1; step <= 7; step++ {
		t.Run(fmt.Sprintf("crash_before_step%d", step), func(t *testing.T) {
			dir := t.TempDir()
			walPath := filepath.Join(dir, "w.wal")

			w, err := OpenWith(walPath, Options{MaxSegmentBytes: 8 << 10})
			if err != nil {
				t.Fatalf("OpenWith: %v", err)
			}
			crash := fmt.Errorf("injected crash before rotation step %d", step)
			fired := false
			w.beforeRotateStep = func(n int) error {
				if n == step && !fired {
					fired = true
					return crash
				}
				return nil
			}
			// Write until the rotation fires and fails. The failure is the crash: the
			// process would be gone, so the writer is simply abandoned.
			placeUntilError(t, w, 1, before)
			if !fired {
				t.Fatalf("rotation never reached step %d in %d records", step, before)
			}
			// Abandon without syncing, exactly as a killed process does. The fd leaks
			// for the rest of the test, which is what a crash looks like from here.

			// Nothing may exist on disk that declares a base above 1 in its name and
			// does not declare one in its header. Segment 1 is exempt and only segment
			// 1: it is the migrated stem, which keeps the framing it already had,
			// because a framing change is legal at a segment boundary and only there.
			for _, name := range segmentSetOf(t, walPath) {
				base, _ := segBaseFromName(filepath.Base(walPath), name)
				if base == 1 {
					continue
				}
				raw := readFile(t, filepath.Join(dir, name))
				if len(raw) < SegHeaderBytesV3 || string(raw[:len(SegMagicV3)]) != SegMagicV3 {
					t.Fatalf("crash before step %d left %s with no segment header (%d bytes).\n"+
						"Steps 3-5 write the header into a temp file, fsync it and LINK it into place precisely so this\n"+
						"state cannot exist; a reader that met it would have to guess a base, which is guessing which\n"+
						"commands the file holds.", step, name, len(raw))
				}
			}

			// The restart. It must succeed, and continuing from it must reach the same
			// book the uninterrupted run reached.
			w2, err := OpenWith(walPath, Options{MaxSegmentBytes: 8 << 10})
			if err != nil {
				t.Fatalf("crash before step %d: restart failed: %v", step, err)
			}
			from := int(w2.Seq()) + 1
			eng := placeThrough(t, w2, from, before+after)
			if err := w2.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if eng != want {
				t.Errorf("crash before step %d recovered a different book\n got %s\nwant %s", step, eng, want)
			}
		})
	}
}

// placeThrough appends submits carrying prices derived from `from`..`to` and returns
// the digest of the book the whole 1..to stream produces when recovered.
func placeThrough(t *testing.T, w *Writer, from, to int) string {
	t.Helper()
	for i := from; i <= to; i++ {
		o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
			int64(1000+i%50), 1, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		if _, err := w.AppendSubmit(o); err != nil {
			t.Fatalf("AppendSubmit %d: %v", i, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	path, _ := w.ActiveSegment()
	_ = path
	entries, err := ReadAll(w.stem)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	eng := matching.NewEngine(tapeCfg())
	Restore(eng, entries)
	return eng.TakeSnapshot().Digest()
}

// placeUntilError appends until an append fails, which is what a failed rotation
// does to the caller.
func placeUntilError(t *testing.T, w *Writer, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
			int64(1000+i%50), 1, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		if _, err := w.AppendSubmit(o); err != nil {
			return
		}
	}
}

// TestATornTailSealsItsSegment is the docs/BOUNDED-RECOVERY.md §6.1 reproduction,
// which used to end with the venue refusing to start on the SECOND restart after a
// crash.
//
// Open took O_APPEND and wrote the next record BEHIND the fragment's bytes. The
// fragment's length prefix then found enough following bytes to look complete, its
// CRC failed, and recovery refused. §6.1 said the fix was to truncate, and that
// truncating in a recovery path needed its own spec because the fragment is
// sometimes the only evidence a command was attempted. Segments give a fix that
// truncates nothing: leave the fragment, stop writing to that file forever, start a
// new one whose base is exactly last+1.
func TestATornTailSealsItsSegment(t *testing.T) {
	for _, cut := range []int{1, 5, 17} {
		t.Run(fmt.Sprintf("cut%d", cut), func(t *testing.T) {
			dir := t.TempDir()
			walPath := filepath.Join(dir, "w.wal")
			writeLog(t, walPath, 5)

			raw := readFile(t, walPath)
			writeFile(t, walPath, raw[:len(raw)-cut])

			// First restart: four complete records, a fragment, no error.
			entries, err := ReadAll(walPath)
			if err != nil {
				t.Fatalf("ReadAll after the crash: %v", err)
			}
			if len(entries) != 4 {
				t.Fatalf("read %d entries, want 4", len(entries))
			}

			w, err := Open(walPath)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if w.Seq() != 4 {
				t.Fatalf("resumed at seq %d, want 4", w.Seq())
			}
			o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, 500, 1, types.TIFGoodTillCancel)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			if _, err := w.AppendSubmit(o); err != nil {
				t.Fatalf("AppendSubmit: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// Second restart: this is the one that used to fail.
			entries, err = ReadAll(walPath)
			if err != nil {
				t.Fatalf("ReadAll on the second restart: %v\n"+
					"This is docs/BOUNDED-RECOVERY.md §6.1: a record was appended behind a torn fragment, which made the\n"+
					"fragment look complete and fail its checksum. Open must seal a torn segment instead of appending to it.", err)
			}
			if len(entries) != 5 {
				t.Fatalf("read %d entries, want 5 (four before the crash, one after)", len(entries))
			}
			if entries[4].Seq != 5 {
				t.Errorf("the appended record has seq %d, want 5", entries[4].Seq)
			}
			// The fragment is still on disk. Nothing was truncated; that is the whole
			// argument for solving this with a segment boundary rather than with ftruncate.
			stemSize := int64(0)
			if st, err := os.Stat(walPath); err == nil {
				stemSize = st.Size()
			}
			if stemSize != int64(len(raw)-cut) {
				t.Errorf("the torn segment is %d bytes, want the %d it had after the crash — something truncated it",
					stemSize, len(raw)-cut)
			}
			// And a third restart is still fine, which is the property that was broken.
			if _, err := Open(walPath); err != nil {
				t.Errorf("third Open: %v", err)
			}
		})
	}
}

// TestTornTailInAnEmptySegmentIsSealed is the same rule at the one boundary where
// last+1 collides with the torn segment's own base: a crash during the first record
// of a brand-new segment. No complete record is in that file, so the fragment moves
// aside — it is not deleted — and the segment is written cleanly at the base it
// already claimed.
func TestTornTailInAnEmptySegmentIsSealed(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "w.wal")

	w, err := OpenWith(walPath, Options{MaxSegmentBytes: 4 << 10})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	placeUntilError(t, w, 1, 400)
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	before := w.Seq()
	names := segmentSetOf(t, walPath)
	if len(names) < 2 {
		t.Fatalf("fixture produced %d segments, want at least 2", len(names))
	}
	newest := filepath.Join(dir, names[len(names)-1])
	// Cut the newest segment back to its header plus half a record.
	raw := readFile(t, newest)
	if len(raw) < SegHeaderBytes+6 {
		t.Fatalf("newest segment is only %d bytes", len(raw))
	}
	writeFile(t, newest, raw[:SegHeaderBytes+6])

	w2, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open over a segment holding only a fragment: %v", err)
	}
	o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, 500, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	seq, err := w2.AppendSubmit(o)
	if err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if entries[len(entries)-1].Seq != seq {
		t.Errorf("last record has seq %d, want %d", entries[len(entries)-1].Seq, seq)
	}
	if seq > before+1 {
		t.Errorf("the restart skipped %d sequences", seq-before-1)
	}
	// The fragment is still on disk, under a name no enumerator matches.
	if _, err := os.Stat(newest + ".torn"); err != nil {
		t.Errorf("the fragment was not preserved: %v", err)
	}
}

// TestTheStemBecomesSegmentOneOnTheFirstRotation pins the migration.
//
// A venue's first rotation is the first moment the stem stops being able to describe
// the whole log, and therefore the first moment a downgrade could read the stem, find
// no segments and start an empty venue beside a set that holds everything. Before
// that instant the stem IS the log and an old build reads it correctly — which is
// why nothing migrates at Open, and why a venue that never rotates is byte-identical
// to what this package wrote before segments existed.
func TestTheStemBecomesSegmentOneOnTheFirstRotation(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "w.wal")

	w, err := OpenWith(walPath, Options{MaxSegmentBytes: 8 << 10})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	// Before any rotation the stem is one ordinary file holding the records, not a
	// set. It carries a segment header declaring base 1 and this build's matching
	// semantics — a log that says nothing about which matcher wrote it is what
	// docs/SEMANTICS-VERSION.md exists to end — and it is still the whole log.
	if _, err := w.AppendSubmit(mustOrder(t, "u", 1000, 1)); err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if raw := readFile(t, walPath); string(raw[:len(SegMagicV3)]) != SegMagicV3 {
		t.Fatalf("an unrotated log does not carry a segment header: %q", raw[:8])
	}
	if names := segmentSetOf(t, walPath); len(names) != 0 {
		t.Fatalf("an unrotated log produced numbered segments: %v", names)
	}

	placeUntilError(t, w, 2, 400)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	names := segmentSetOf(t, walPath)
	if len(names) < 2 || !strings.HasSuffix(names[0], "0000000000000001") {
		t.Fatalf("after rotation the set is %v; the stem should have become segment 1", names)
	}
	// And the stem now holds the marker: 18 bytes, no records.
	raw := readFile(t, walPath)
	if len(raw) != SegHeaderBytes || string(raw[:len(SegMagic)]) != SegMagic {
		t.Fatalf("the stem is %d bytes beginning %q, want an %d-byte set marker", len(raw), raw[:min(len(raw), 6)], SegHeaderBytes)
	}
	if entries, err := ReadAll(walPath); err != nil {
		t.Fatalf("ReadAll after migration: %v", err)
	} else if len(entries) != 400 {
		t.Errorf("read %d entries after migration, want 400", len(entries))
	}
}

// TestAnOldBuildRefusesAMarkerLoudly is the downgrade story, asserted rather than
// asserted-in-prose.
//
// Without a marker, an older build pointed at a rotated set finds nothing at -wal,
// concludes there is no log, starts an EMPTY VENUE and writes sequence 1 beside
// segments holding everything. With one, it peeks six bytes, does not find
// OBWAL\x01, treats the file as a headerless v1 log, reads "OBWA" as a length prefix
// of 1,329,747,777 bytes and refuses. The constant is asserted here because
// docs/RUNBOOKS.md quotes it and an operator grepping for the wrong number finds
// nothing.
func TestAnOldBuildRefusesAMarkerLoudly(t *testing.T) {
	if got := binary.BigEndian.Uint32([]byte("OBWA")); got != 1_329_747_777 {
		t.Fatalf("the first four bytes of a marker read as a length prefix are %d, not the 1329747777 the runbook quotes", got)
	}
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	if err := writeMarker(stem); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}
	raw := readFile(t, stem)
	if len(raw) != SegHeaderBytes {
		t.Fatalf("the marker is %d bytes, want %d", len(raw), SegHeaderBytes)
	}
	// This build reads it as what it is: a set with no segments.
	entries, err := ReadAll(stem)
	if err != nil {
		t.Fatalf("this build must read a marker as an empty set: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a marker yielded %d entries", len(entries))
	}
	// An old build has no OBWAL\x02 case, so it falls into the v1 path and reads the
	// first four bytes as a length. MaxRecordBytes is what makes that a refusal.
	if n := int(binary.BigEndian.Uint32(raw[:4])); n <= MaxRecordBytes {
		t.Errorf("a marker's first four bytes read as %d, which is inside the %d-byte bound — "+
			"an old build would try to read it as a record rather than refusing", n, MaxRecordBytes)
	}
}

// handBuiltSegment frames entries into a DECLARED segment: a header naming base and
// this build's matching semantics, then correctly framed and checksummed records
// carrying the sequences given.
func handBuiltSegment(tb testing.TB, path string, base int64, seqs []int64) {
	tb.Helper()
	handBuiltSegmentAt(tb, path, base, matching.SemanticsVersion, seqs)
}

// handBuiltSegmentAt is handBuiltSegment with the declared matching semantics named,
// so a test can build the segment a DIFFERENT build would have written. Passing 0
// produces a pre-stamp segment, which is the shape every log on every disk has today.
func handBuiltSegmentAt(tb testing.TB, path string, base int64, sem int, seqs []int64) {
	tb.Helper()
	raw := segHeaderV3(base, sem)
	if sem == 0 {
		// A segment that declares NOTHING, rather than one that declares zero: the
		// 18-byte OBWAL\x02 header, which is what every pre-stamp build wrote.
		raw = segHeader(base)
	}
	for i, seq := range seqs {
		o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
			int64(1000+i%50), 1, types.TIFGoodTillCancel)
		if err != nil {
			tb.Fatalf("NewOrder: %v", err)
		}
		payload, err := json.Marshal(Entry{Seq: seq, Kind: KindSubmit, Order: o})
		if err != nil {
			tb.Fatalf("Marshal: %v", err)
		}
		raw = binary.BigEndian.AppendUint32(raw, uint32(len(payload)))
		raw = binary.BigEndian.AppendUint32(raw, crc32.Checksum(payload, crcTable))
		raw = append(raw, payload...)
	}
	writeFile(tb, path, raw)
}

// TestASegmentWhoseRecordsDisagreeWithItsDeclaredBaseFallsBack is slice 1's Rule 1
// rebased rather than deleted.
//
// The anchor used to be "record 1 carries sequence 1". It is now "record 1 carries
// the base the header declares", which means exactly the same thing on an unrotated
// file and keeps meaning something on a rotated one. Derive the base from record 1
// instead — the tempting shortcut, since record 1 is parsed anyway — and the data can
// never disagree with itself: the anchor is deleted in effect while appearing to be
// present, and this test is what notices.
func TestASegmentWhoseRecordsDisagreeWithItsDeclaredBaseFallsBack(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	snapPath := filepath.Join(dir, "s.snap")

	// A valid two-segment set, hand-built: 1..100 then 101..200.
	first := make([]int64, 100)
	for i := range first {
		first[i] = int64(i + 1)
	}
	second := make([]int64, 100)
	for i := range second {
		second[i] = int64(101 + i)
	}
	handBuiltSegment(t, segPath(stem, 1), 1, first)
	handBuiltSegment(t, segPath(stem, 101), 101, second)
	emptySnapshotAt(t, snapPath, 150)

	_, rep, err := RecoverWithReport(tapeCfg(), snapPath, stem)
	if err != nil {
		t.Fatalf("RecoverWithReport on a good set: %v", err)
	}
	if rep.FellBack {
		t.Fatalf("a well-formed hand-built set fell back: %+v", rep)
	}

	// Now give the second segment's records sequences that its header does not
	// declare — the shape a segment restored from another venue, or two writers
	// interleaving, produces.
	for i := range second {
		second[i] = int64(9_001 + i)
	}
	handBuiltSegment(t, segPath(stem, 101), 101, second)

	_, rep, err = RecoverWithReport(tapeCfg(), snapPath, stem)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v", err)
	}
	if !rep.FellBack {
		t.Error("a segment whose records carry sequences its header does not declare was skipped by position without falling back")
	}
}

// TestRecoverNeverWritesToTheDirectory. cmd/obgw recovers before it opens, so a
// reader that migrated a legacy stem would mutate the directory during the one
// operation an operator runs when they are least sure what is on disk. It would also
// stop Recover working on a read-only copy of a venue's data directory, which is how
// anyone sane investigates a log they do not trust.
func TestRecoverNeverWritesToTheDirectory(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "w.wal")
	writeLog(t, walPath, 20)

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	eng, err := Recover(tapeCfg(), "", walPath)
	if err != nil {
		t.Fatalf("Recover against a read-only directory: %v — a reader is writing", err)
	}
	if eng.OrderCount() != 20 {
		t.Errorf("recovered %d orders, want 20", eng.OrderCount())
	}
	if _, err := ReadAll(walPath); err != nil {
		t.Errorf("ReadAll against a read-only directory: %v", err)
	}
}

// writeV1Log writes a headerless v1 log by hand: [4-byte length][payload], no magic
// and no CRC — the format this package wrote before checksums existed.
//
// Nothing else in pkg/wal rotates one of these, and that gap is what let a
// mis-framed rotation through the whole -race suite. See
// TestRotatingAV1LogKeepsTheWholeSetReadable.
func writeV1Log(tb testing.TB, path string, n int) {
	tb.Helper()
	var raw []byte
	for i := 0; i < n; i++ {
		o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
			int64(1000+i%50), 1, types.TIFGoodTillCancel)
		if err != nil {
			tb.Fatalf("NewOrder: %v", err)
		}
		payload, err := json.Marshal(Entry{Seq: int64(i + 1), Kind: KindSubmit, Order: o})
		if err != nil {
			tb.Fatalf("Marshal: %v", err)
		}
		raw = binary.BigEndian.AppendUint32(raw, uint32(len(payload)))
		raw = append(raw, payload...)
	}
	writeFile(tb, path, raw)
}

// TestRotatingAV1LogKeepsTheWholeSetReadable is the framing transition, asserted for
// the first time.
//
// The Writer doc comment claims "a framing change is legal at a segment boundary and
// only there, so rotating really does get checksums onto an old file", and §7 of
// docs/LOG-ROTATION.md calls that sentence "becoming true for the first time". It was
// not: append computes the frame header from w.checksummed BEFORE it decides to
// rotate, so the record that TRIGGERS the rotation was framed v1 — four bytes of
// length, no CRC — and then written into a segment whose header declares every record
// checksummed. The reader takes the record's first four payload bytes as the CRC,
// fails it, and the set is permanently unreadable from that segment on. Open still
// succeeded because it reads only the newest segment, so a venue that upgraded from a
// pre-checksum build kept running and died at the NEXT restart.
//
// A deployment whose WAL predates checksums is supported on purpose (kindLegacy), and
// rotation is on by default at 128 MiB, so its first rotation is the first thing that
// happens to it after the upgrade.
func TestRotatingAV1LogKeepsTheWholeSetReadable(t *testing.T) {
	const (
		pre   = 20
		total = 200
	)
	dir := t.TempDir()
	stem := filepath.Join(dir, "u.wal")
	writeV1Log(t, stem, pre)

	v1Before := readFile(t, stem)

	w, err := OpenWith(stem, Options{MaxSegmentBytes: 4 << 10})
	if err != nil {
		t.Fatalf("OpenWith on a v1 log: %v", err)
	}
	// The property this test is about — framing never changes mid-file — is now
	// enforced one step earlier and one step harder. A v1 file declares no matching
	// semantics, so Open migrates it into the set and seals it before writing
	// anything (docs/SEMANTICS-VERSION.md Rule 17), and this build's records go into
	// a new checksummed segment beside it. The old assertion here was
	// !w.Checksummed(), a PROXY for "the v1 file's framing is preserved"; the proxy
	// stopped matching the property, so the property is asserted directly below —
	// byte-identically — which the proxy never could.
	if !w.Checksummed() {
		t.Fatal("the sealed-past segment is not checksummed; sealing is what gets checksums onto an old file")
	}
	for i := pre + 1; i <= total; i++ {
		if _, err := w.AppendSubmit(mustOrder(t, "u", int64(1000+i%50), 1)); err != nil {
			t.Fatalf("AppendSubmit %d: %v", i, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if w.Rotations() == 0 {
		t.Fatalf("the fixture never rotated; it is not exercising the transition")
	}
	if !w.Checksummed() {
		t.Error("a rotated segment declares its header, so its records must carry CRCs")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The v1 file itself never changed a byte. It is segment 1 now, which is where
	// the first rotation would have put it anyway.
	if after := readFile(t, filepath.Join(dir, "u.wal.0000000000000001")); !bytes.Equal(v1Before, after) {
		t.Errorf("the v1 segment changed under an append: %d bytes before, %d after", len(v1Before), len(after))
	}

	names := segmentSetOf(t, stem)
	if len(names) < 2 {
		t.Fatalf("the set is %v; the fixture is not exercising rotation", names)
	}
	entries, err := ReadAll(stem)
	if err != nil {
		t.Fatalf("ReadAll over a rotated v1 log: %v\n"+
			"The record that triggered the rotation was framed before the rotation was decided, so it went into\n"+
			"the new OBWAL\\x02 segment with v1 framing and no CRC. Every reader takes its first four payload bytes\n"+
			"as a checksum. The set is unreadable from that segment on, permanently, and Open does not notice\n"+
			"because it reads only the newest segment. segments = %v", err, names)
	}
	if len(entries) != total {
		t.Fatalf("read %d entries across the transition, want %d", len(entries), total)
	}
	for i, e := range entries {
		if e.Seq != int64(i+1) {
			t.Fatalf("entry %d carries sequence %d, want %d", i, e.Seq, i+1)
		}
	}
	// The set holds records from two matchers: the v1 file's twenty, written before
	// semantics were stamped, and this build's behind them. Replaying all of it needs
	// the pre-stamp half named, which is the whole of what
	// docs/SEMANTICS-VERSION.md §4 asks of a venue upgrading with a v1 log in hand.
	if _, _, err := RecoverWithOptions(tapeCfg(), "", stem, RecoverOptions{AcceptSemantics: []int{0}}); err != nil {
		t.Errorf("Recover over a rotated v1 log: %v", err)
	}
	// And without it, recovery refuses rather than serving a book the venue that
	// wrote those twenty records never had.
	if _, err := Recover(tapeCfg(), "", stem); !errors.Is(err, ErrSemanticsMismatch) {
		t.Errorf("Recover err = %v, want ErrSemanticsMismatch", err)
	}
}

// TestOpenWritesTheMagicIntoAnExistingEmptyFile.
//
// readSegHeader classifies a zero-byte file as kindHeadered "exactly as hasHeader did:
// there is nothing there to be a v1 log" — but that classification is only true if
// something then WRITES the header. Open's fresh-path branch does. Its default branch,
// which is where an EXISTING empty file lands, did not: it took the fd, appended
// CRC-framed records at offset 0, and left a file every reader classifies as a v1
// headerless log. ReadAll then returns zero entries and a nil error, Recover starts an
// empty venue, and the next Open resumes at sequence 0 — so every restart rewrites
// sequence 1 over the last one. Total, silent loss of the journal.
//
// A zero-byte file at the WAL path is not exotic: `touch`, a provisioning script, a
// restored volume, or a venue that opened the path and was killed before the first
// Sync all produce one.
func TestOpenWritesTheMagicIntoAnExistingEmptyFile(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "e.wal")
	writeFile(t, stem, nil)

	w, err := Open(stem)
	if err != nil {
		t.Fatalf("Open on an existing empty file: %v", err)
	}
	if !w.Checksummed() {
		t.Error("an empty file is not a v1 log — records written into it must carry CRCs")
	}
	if _, err := w.AppendSubmit(mustOrder(t, "u", 1000, 1)); err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if raw := readFile(t, stem); len(raw) < SegHeaderBytesV3 || string(raw[:len(SegMagicV3)]) != SegMagicV3 {
		t.Fatalf("the file does not begin with a segment header: %q\n"+
			"Records were appended at offset 0 with CRC frames into a file every reader classifies as a v1\n"+
			"headerless log. ReadAll returns nothing with a nil error and the venue cannot tell it happened.", raw[:min(len(raw), 8)])
	}
	entries, err := ReadAll(stem)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 1 || entries[0].Seq != 1 {
		t.Fatalf("read %d entries %v, want one carrying sequence 1", len(entries), entries)
	}

	// And the second restart must resume from 1 rather than rewriting it.
	w2, err := Open(stem)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	seq, err := w2.AppendSubmit(mustOrder(t, "u", 1001, 1))
	if err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if seq != 2 {
		t.Fatalf("the second restart wrote sequence %d, want 2 — it is rewriting the log from the start", seq)
	}
}

// TestOpenHealsASetWhoseStemIsMissing.
//
// Numbered segments with NOTHING at the stem is the one downgrade shape
// docs/LOG-ROTATION.md §2.5 says must never exist, and the only one an older build
// cannot detect: it finds no file at -wal, concludes there is no log, starts an empty
// venue and begins writing sequence 1 beside a set holding everything. Every other
// downgrade shape is loud — an old build appending into a legacy stem next to a
// numbered segment produces an overlap this build refuses on the next start.
//
// It is reachable without a bug. The migration at the first rotation is a rename
// followed by a marker write and nothing makes the pair atomic: a process killed
// between them, or a writeMarker that fails with ENOSPC or EROFS — the disk condition
// this mechanism exists for — leaves exactly this. §12.1 claimed the window was safe;
// §12.8 records that it is not.
//
// Open could not repair it because it inferred "this is a legacy stem" from
// newest.named, which is false the moment the rename has happened, so neither Open
// nor any later rotation ever wrote the marker. Measured before the fix: still absent
// after 46 further rotations.
func TestOpenHealsASetWhoseStemIsMissing(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "h.wal")

	w, err := OpenWith(stem, Options{MaxSegmentBytes: 4 << 10})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	placeUntilError(t, w, 1, 300)
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if w.Rotations() == 0 {
		t.Fatal("the fixture never rotated; there is no migration to interrupt")
	}
	entries, err := ReadAll(stem)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := len(entries)
	if want == 0 {
		t.Fatal("the fixture wrote no records")
	}

	// The state a kill between the rename and the marker leaves, and the state a
	// failed writeMarker leaves: segments, and no file at the stem.
	if err := os.Remove(stem); err != nil {
		t.Fatalf("removing the stem: %v", err)
	}

	w2, err := OpenWith(stem, Options{MaxSegmentBytes: 4 << 10})
	if err != nil {
		t.Fatalf("OpenWith over a set with no stem: %v", err)
	}
	defer func() { _ = w2.Close() }()

	raw := readFile(t, stem)
	if len(raw) != SegHeaderBytes || string(raw[:len(SegMagic)]) != SegMagic {
		t.Fatalf("after Open the stem is %d bytes beginning %q, want the %d-byte set marker.\n"+
			"Numbered segments with no file at the stem is the shape an older build reads as \"there is no log\":\n"+
			"it starts an empty venue and writes sequence 1 beside %d records.",
			len(raw), raw[:min(len(raw), 6)], SegHeaderBytes, want)
	}
	// The repair must not have cost anything: the same records, and the writer still
	// appends behind them.
	after, err := ReadAll(stem)
	if err != nil {
		t.Fatalf("ReadAll after the repair: %v", err)
	}
	if len(after) != want {
		t.Errorf("the set holds %d records after the repair, want %d", len(after), want)
	}
	seq, err := w2.AppendSubmit(mustOrder(t, "u", 1000, 1))
	if err != nil {
		t.Fatalf("AppendSubmit after the repair: %v", err)
	}
	if seq != int64(want)+1 {
		t.Errorf("the next append took sequence %d, want %d", seq, want+1)
	}
	if _, err := Recover(tapeCfg(), "", stem); err != nil {
		t.Errorf("Recover after the repair: %v", err)
	}
}

// TestHealingTheStemLeavesAPreRotationLogAlone — the repair must be narrow. A stem
// that EXISTS is never rewritten, whatever it holds: a set that has never rotated
// keeps its records at the stem, and overwriting it with a marker would delete them.
func TestHealingTheStemLeavesAPreRotationLogAlone(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "p.wal")

	w, err := OpenWith(stem, Options{MaxSegmentBytes: -1}) // rotation off
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	placeUntilError(t, w, 1, 40)
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before := readFile(t, stem)

	w2, err := OpenWith(stem, Options{MaxSegmentBytes: -1})
	if err != nil {
		t.Fatalf("second OpenWith: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := readFile(t, stem); len(got) != len(before) {
		t.Fatalf("Open rewrote an unrotated log: %d bytes became %d", len(before), len(got))
	}
	if entries, err := ReadAll(stem); err != nil {
		t.Errorf("ReadAll: %v", err)
	} else if len(entries) != 40 {
		t.Errorf("the unrotated log holds %d records, want 40", len(entries))
	}
}

// TestTheActiveSegmentsFdNamesTheFile.
//
// An *os.File keeps the name it was opened with, and every write, flush, fsync and
// close error it produces is a *PathError carrying that name. materialiseSegment
// builds a segment through <base>.tmp, links it into place and unlinks the temp — and
// used to hand that fd back, so from the first rotation onward every durability
// failure on the ACTIVE segment named a file that does not exist on disk.
//
// Observed on a real full disk: "WAL SYNC FAILED — halting the book ...: write
// /Volumes/WALTINY/venue/v.wal.0000000000078054.tmp: no space left on device", with ls
// showing v.wal.0000000000078054 present at 1,069,056 bytes and no .tmp file anywhere
// in the directory. An operator following docs/RUNBOOKS.md greps for the name in the
// error and finds nothing — and docs/LOG-ROTATION.md §3.3 teaches that a stray
// <base>.tmp means a rotation that crashed between materialising a segment and linking
// it into place, so the message argues for the wrong diagnosis as well as the wrong
// file.
func TestTheActiveSegmentsFdNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "n.wal")

	w, err := OpenWith(stem, Options{MaxSegmentBytes: 4 << 10})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer func() { _ = w.Close() }()
	placeUntilError(t, w, 1, 300)
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if w.Rotations() == 0 {
		t.Fatal("the fixture never rotated; the temp fd is only handed out by a rotation")
	}

	path, _ := w.ActiveSegment()
	if got := w.f.Name(); got != path {
		t.Errorf("the active segment's fd is named %q, want %q.\n"+
			"Every write, flush and fsync error on this segment reports the first name, and no file exists "+
			"under it.", got, path)
	}
	if strings.HasSuffix(w.f.Name(), ".tmp") {
		t.Errorf("the active segment's fd still carries a .tmp name: %q", w.f.Name())
	}
	// The name must be the file that is actually growing, not merely a plausible one.
	before := fileSize(t, path)
	placeUntilError(t, w, 301, 320)
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if after := fileSize(t, path); after <= before {
		t.Errorf("%s is %d bytes and was %d — the fd is not writing to the file it names", path, after, before)
	}
	// And the reopen did not cost the header or the records.
	if entries, err := ReadAll(stem); err != nil {
		t.Errorf("ReadAll: %v", err)
	} else if len(entries) != 320 {
		t.Errorf("read %d entries, want 320", len(entries))
	}
}

func fileSize(tb testing.TB, path string) int64 {
	tb.Helper()
	st, err := os.Stat(path)
	if err != nil {
		tb.Fatalf("Stat %s: %v", path, err)
	}
	return st.Size()
}
