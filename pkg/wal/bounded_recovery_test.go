package wal

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Recovery reads the whole log and parses only the part the snapshot does not
// already cover. These tests exist because the parts it stops doing are the parts
// nothing else would notice were gone.
//
// The one that matters most is TestCorruptionInTheCoveredPrefixStillRefuses. Before
// this change there was no test anywhere that corrupted a record BEHIND a snapshot:
// integrity_test.go recovers with no snapshot at all, and cmd/obgw's drill clears
// SnapshotPath on purpose. So the faster version of this optimisation — seek past
// the covered prefix instead of reading it — would have passed the entire suite
// while quietly dropping the venue's only check on the bytes it skipped.

// buildSnapshottedLog writes n submit records and checkpoints after the at-th one
// (at == 0 writes no snapshot). Every order rests, so the recovered book is a
// function of how many records were applied — which is what makes a digest
// comparison across the boundary meaningful.
func buildSnapshottedLog(tb testing.TB, dir string, n, at int) (walPath, snapPath string) {
	tb.Helper()
	walPath = filepath.Join(dir, "w.wal")
	snapPath = filepath.Join(dir, "s.snap")

	w, err := Open(walPath)
	if err != nil {
		tb.Fatalf("Open: %v", err)
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
				tb.Fatalf("checkpoint after %d records is stamped WALSeq %d; the fixture's premise is one record per command", at, snap.WALSeq)
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

// walFrame locates one record in a headered log.
type walFrame struct{ hdr, crc, payload, end int }

// walFrames walks a headered log's framing the way a reader does, so a test can
// damage record k specifically rather than "some bytes near the front".
func walFrames(tb testing.TB, raw []byte) []walFrame {
	tb.Helper()
	if len(raw) < len(Magic) || string(raw[:len(Magic)]) != Magic {
		tb.Fatalf("not a headered log; first bytes %q", raw[:min(len(raw), 8)])
	}
	var out []walFrame
	for off := len(Magic); off+8 <= len(raw); {
		n := int(binary.BigEndian.Uint32(raw[off : off+4]))
		if n <= 0 || off+8+n > len(raw) {
			break
		}
		out = append(out, walFrame{hdr: off, crc: off + 4, payload: off + 8, end: off + 8 + n})
		off += 8 + n
	}
	return out
}

func readFile(tb testing.TB, path string) []byte {
	tb.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("ReadFile: %v", err)
	}
	return b
}

func writeFile(tb testing.TB, path string, b []byte) {
	tb.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		tb.Fatalf("WriteFile: %v", err)
	}
}

// fullParse is the path this change replaces: read every record, then let the
// sequence filter decide what to apply. It is the oracle every equivalence test
// here compares against.
func fullParse(tb testing.TB, snapPath, walPath string) string {
	tb.Helper()
	snap, err := ReadSnapshot(snapPath)
	if err != nil {
		tb.Fatalf("ReadSnapshot: %v", err)
	}
	entries, err := ReadAll(walPath)
	if err != nil {
		tb.Fatalf("ReadAll: %v", err)
	}
	var eng *matching.Engine
	var after int64
	if snap != nil {
		if eng, err = matching.RestoreEngine(tapeCfg(), snap); err != nil {
			tb.Fatalf("RestoreEngine: %v", err)
		}
		after = snap.WALSeq
	} else {
		eng = matching.NewEngine(tapeCfg())
	}
	RestoreAfter(eng, entries, after)
	return eng.TakeSnapshot().Digest()
}

// TestCorruptionInTheCoveredPrefixStillRefuses is the test that makes "the skip
// still verifies" enforceable rather than aspirational.
//
// A record whose bytes changed on disk must stop the venue starting even when the
// snapshot already covers it. Skipping by seeking would pass every other test in
// this package and fail this one, which is the point of writing it.
func TestCorruptionInTheCoveredPrefixStillRefuses(t *testing.T) {
	dir := t.TempDir()
	walPath, snapPath := buildSnapshottedLog(t, dir, 1_000, 500)

	raw := readFile(t, walPath)
	frames := walFrames(t, raw)
	if len(frames) != 1_000 {
		t.Fatalf("walked %d frames, want 1000", len(frames))
	}
	// Ordinal 10: a long way behind the snapshot's boundary at 500, so recovery has
	// no reason to parse it and every reason to verify it.
	raw[frames[9].payload+2]++
	writeFile(t, walPath, raw)

	_, err := Recover(tapeCfg(), snapPath, walPath)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Recover err = %v, want ErrCorrupt — a record altered behind the snapshot started a venue", err)
	}
	// Rule 5: the record is named by its ordinal in the FILE. RUNBOOKS sends an
	// operator to "the records before the corrupt one", so a number that counted only
	// the records recovery retained would send them 500 records off.
	if !strings.Contains(err.Error(), "record 10 ") {
		t.Errorf("error names the wrong record: %v\nwant the file ordinal (10), not a count of retained records", err)
	}
}

// TestCorruptLengthPrefixInTheCoveredPrefixIsRefused — the other half of the
// framing check. A damaged length is caught before it is used to size a read, and
// that has to keep happening behind a snapshot too.
func TestCorruptLengthPrefixInTheCoveredPrefixIsRefused(t *testing.T) {
	dir := t.TempDir()
	walPath, snapPath := buildSnapshottedLog(t, dir, 1_000, 500)

	raw := readFile(t, walPath)
	frames := walFrames(t, raw)
	binary.BigEndian.PutUint32(raw[frames[9].hdr:frames[9].hdr+4], 0xFFFFFFF0)
	writeFile(t, walPath, raw)

	_, err := Recover(tapeCfg(), snapPath, walPath)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Recover err = %v, want ErrCorrupt", err)
	}
	if !strings.Contains(err.Error(), "record 10 ") {
		t.Errorf("error names the wrong record: %v", err)
	}
}

// poisonRecord replaces record `ordinal`'s payload with bytes that are not JSON and
// recomputes its CRC, so the frame is complete, correctly sized and checksum-valid,
// and the only thing wrong with it is that it is not a record. That is a writer bug
// or a format mismatch — the one damage class a checksum cannot see.
func poisonRecord(tb testing.TB, path string, ordinal int) {
	tb.Helper()
	raw := readFile(tb, path)
	frames := walFrames(tb, raw)
	if ordinal < 1 || ordinal > len(frames) {
		tb.Fatalf("ordinal %d is not in a log of %d records", ordinal, len(frames))
	}
	f := frames[ordinal-1]
	for i := f.payload; i < f.end; i++ {
		raw[i] = 'Z'
	}
	binary.BigEndian.PutUint32(raw[f.crc:f.crc+4], crc32.Checksum(raw[f.payload:f.end], crcTable))
	writeFile(tb, path, raw)
}

// rewriteSeq gives record `ordinal` the sequence number newSeq and fixes its CRC, so
// the file stays perfectly well-formed and the only thing wrong with it is that one
// record's sequence is not its position. newSeq must have the same digit count as the
// sequence it replaces, or the frame would have to move.
func rewriteSeq(tb testing.TB, path string, ordinal int, newSeq int64) {
	tb.Helper()
	raw := readFile(tb, path)
	frames := walFrames(tb, raw)
	if ordinal < 1 || ordinal > len(frames) {
		tb.Fatalf("ordinal %d is not in a log of %d records", ordinal, len(frames))
	}
	f := frames[ordinal-1]
	var e Entry
	if err := json.Unmarshal(raw[f.payload:f.end], &e); err != nil {
		tb.Fatalf("Unmarshal: %v", err)
	}
	if e.Seq != int64(ordinal) {
		tb.Fatalf("record %d carries seq %d; the fixture's premise is broken", ordinal, e.Seq)
	}
	e.Seq = newSeq
	payload, err := json.Marshal(e)
	if err != nil {
		tb.Fatalf("Marshal: %v", err)
	}
	if len(payload) != f.end-f.payload {
		tb.Fatalf("rewritten payload is %d bytes, was %d — the frame would have to move", len(payload), f.end-f.payload)
	}
	copy(raw[f.payload:f.end], payload)
	binary.BigEndian.PutUint32(raw[f.crc:f.crc+4], crc32.Checksum(payload, crcTable))
	writeFile(tb, path, raw)
}

// TestTheLastCoveredRecordIsStillChecked pins the `>=` in walkLog's parse predicate,
// which the spec states as Rule 2 ("parse every record from ordinal `after` onward")
// and which nothing else exercises.
//
// Record `after` is the last one the snapshot covers; record `after+1` is the first
// one to apply. Parsing from `after` inspects both ends of the boundary rather than
// one, and the difference is a real record: change the predicate to `>` and a
// sequence that disagrees with its ordinal at exactly `after` is never read, the
// fallback never fires, and recovery proceeds on a file whose invariant it has no
// evidence for. The test above damages ordinal 600 against a boundary at 500, which
// is caught either way — this one damages 500 itself.
func TestTheLastCoveredRecordIsStillChecked(t *testing.T) {
	dir := t.TempDir()
	walPath, snapPath := buildSnapshottedLog(t, dir, 1_000, 500)
	rewriteSeq(t, walPath, 500, 501)

	eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v", err)
	}
	if !rep.FellBack {
		t.Error("the last record the snapshot covers carries the wrong sequence and recovery did not notice.\n" +
			"walkLog must parse from ordinal afterSeq, not from afterSeq+1: both ends of the boundary get inspected")
	}
	if got, want := eng.TakeSnapshot().Digest(), fullParse(t, snapPath, walPath); got != want {
		t.Errorf("fallback did not reproduce the full parse\n got %s\nwant %s", got, want)
	}
}

// TestUndecodableRecordBehindTheSnapshotStartsTheVenue pins a behaviour this change
// altered, which its spec did not originally name and a review caught.
//
// Two kinds of damage look alike from a distance and are not alike. Bytes that
// changed on disk fail their CRC, and recovery refuses them wherever they sit —
// TestCorruptionInTheCoveredPrefixStillRefuses is that property and it is unchanged.
// Bytes that are complete, pass their CRC and are not JSON are the other kind, and
// the only way to see them is to decode them, which is precisely the work this change
// stopped doing for records a snapshot already covers.
//
// So the check now moves with the boundary. Before this change, such a record refused
// to start the venue from anywhere in the file. It still does at and past the
// boundary; strictly behind it, the venue starts.
//
// It is pinned rather than restored. The recovered book is identical either way,
// because RestoreAfter drops a covered record for its sequence whether or not it
// decoded — the test asserts that equality against the same log undamaged, so a
// regression that started the venue on a DIFFERENT book fails here. Restoring the
// check means decoding the covered prefix, which is the entire cost this slice
// removed. docs/BOUNDED-RECOVERY.md §5.2 records the decision.
func TestUndecodableRecordBehindTheSnapshotStartsTheVenue(t *testing.T) {
	// The book this log recovers to when nothing is wrong with it.
	clean := t.TempDir()
	cleanWAL, cleanSnap := buildSnapshottedLog(t, clean, 1_000, 500)
	want := fullParse(t, cleanSnap, cleanWAL)

	// Behind the boundary: read, checksummed, never decoded, so the venue starts.
	behind := t.TempDir()
	behindWAL, behindSnap := buildSnapshottedLog(t, behind, 1_000, 500)
	poisonRecord(t, behindWAL, 10)

	eng, rep, err := RecoverWithReport(tapeCfg(), behindSnap, behindWAL)
	if err != nil {
		t.Fatalf("Recover err = %v, want nil — a record the snapshot already covers has no bearing on the book", err)
	}
	if got := eng.TakeSnapshot().Digest(); got != want {
		t.Fatalf("recovered a different book than the undamaged log does\n got %s\nwant %s\n"+
			"starting is only defensible because the book is identical; if it is not, the skip is wrong", got, want)
	}
	if rep.Skipped != 500 || rep.Applied != 500 {
		t.Errorf("report says %d skipped / %d applied, want 500 / 500", rep.Skipped, rep.Applied)
	}

	// ReadAll is the strict reader and still finds it, by file ordinal. This is where
	// an operator goes when they want to know what is actually in the file.
	if _, err := ReadAll(behindWAL); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAll err = %v, want ErrCorrupt — ReadAll returns every record, so it must decode every record", err)
	} else if !strings.Contains(err.Error(), "record 10 ") {
		t.Errorf("ReadAll names the wrong record: %v", err)
	}

	// Open must NOT be stricter than Recover. Recovery just succeeded on this file;
	// obgw opens the log immediately afterwards (cmd/obgw/server.go), so an Open that
	// refused here would turn a successful recovery into a failed start — an outage
	// manufactured by two readers of the same bytes disagreeing.
	w, err := Open(behindWAL)
	if err != nil {
		t.Fatalf("Open err = %v, want nil — Recover accepted this file, so Open must too", err)
	}
	if w.Seq() != 1_000 {
		t.Errorf("Open resumed at seq %d, want 1000", w.Seq())
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Past the boundary the same damage is still refused: recovery has to decode that
	// record to apply it, so it finds out.
	past := t.TempDir()
	pastWAL, pastSnap := buildSnapshottedLog(t, past, 1_000, 500)
	poisonRecord(t, pastWAL, 600)

	_, err = Recover(tapeCfg(), pastSnap, pastWAL)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Recover err = %v, want ErrCorrupt — record 600 is one the snapshot does not cover", err)
	}
	if !strings.Contains(err.Error(), "record 600 ") {
		t.Errorf("error names the wrong record: %v", err)
	}
}

// TestSkippedRecoveryMatchesFullParseAtEveryBoundary is the equivalence property:
// wherever the snapshot boundary falls, recovering with the skip lands on exactly
// the book the full parse lands on, and on the book the uninterrupted run had.
//
// Every boundary, not a sample of them. An off-by-one in the retention boundary —
// keeping from `after` instead of `after+1` — double-applies one record, and one
// duplicated order in a 200-order book is invisible to anything coarser.
func TestSkippedRecoveryMatchesFullParseAtEveryBoundary(t *testing.T) {
	const n = 200
	dir := t.TempDir()
	walPath := filepath.Join(dir, "w.wal")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
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
	live := snaps[n].Digest() // the uninterrupted run's final state
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	for k := 1; k <= n; k++ {
		snapPath := filepath.Join(dir, fmt.Sprintf("s%d.snap", k))
		if err := WriteSnapshot(snapPath, snaps[k]); err != nil {
			t.Fatalf("WriteSnapshot: %v", err)
		}
		eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, walPath)
		if err != nil {
			t.Fatalf("boundary %d: RecoverWithReport: %v", k, err)
		}
		got := eng.TakeSnapshot().Digest()
		if want := fullParse(t, snapPath, walPath); got != want {
			t.Fatalf("boundary %d: the skipping read and the full parse disagree\n got %s\nwant %s", k, got, want)
		}
		if got != live {
			t.Fatalf("boundary %d: recovered book differs from the uninterrupted run\n got %s\nwant %s", k, got, live)
		}
		if rep.FellBack {
			t.Fatalf("boundary %d: fell back to a full parse on a log this package wrote", k)
		}
		if rep.Skipped != k || rep.Applied != n-k {
			t.Fatalf("boundary %d: report says %d skipped / %d applied, want %d / %d", k, rep.Skipped, rep.Applied, k, n-k)
		}
		if rep.SnapshotAhead {
			t.Fatalf("boundary %d: reported a snapshot ahead of a log that contains it", k)
		}
	}
}

// TestRecoveryWithNoSnapshotReadsEverything — the after == 0 case must be the old
// behaviour exactly, since it is the one an operator falls back to and the one
// cmd/obgw's recovery drill forces.
func TestRecoveryWithNoSnapshotReadsEverything(t *testing.T) {
	dir := t.TempDir()
	walPath, _ := buildSnapshottedLog(t, dir, 300, 0)

	eng, rep, err := RecoverWithReport(tapeCfg(), filepath.Join(dir, "absent.snap"), walPath)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v", err)
	}
	if got, want := eng.TakeSnapshot().Digest(), fullParse(t, filepath.Join(dir, "absent.snap"), walPath); got != want {
		t.Errorf("cold recovery\n got %s\nwant %s", got, want)
	}
	if eng.OrderCount() != 300 {
		t.Errorf("recovered %d orders, want 300", eng.OrderCount())
	}
	if rep.Skipped != 0 || rep.Applied != 300 {
		t.Errorf("report says %d skipped / %d applied, want 0 / 300", rep.Skipped, rep.Applied)
	}
	if rep.SnapshotAhead || rep.FellBack || rep.SnapshotSeq != 0 {
		t.Errorf("report = %+v, want a plain cold start", rep)
	}
}

// handBuiltLog frames entries into a headered log with correct lengths and correct
// CRCs, so the only thing a test varies is the sequence numbers the records carry.
func handBuiltLog(tb testing.TB, path string, seqs []int64) {
	tb.Helper()
	raw := []byte(Magic)
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

// emptySnapshotAt writes a snapshot of an empty book stamped with the given WALSeq.
func emptySnapshotAt(tb testing.TB, path string, walSeq int64) {
	tb.Helper()
	snap := matching.NewEngine(tapeCfg()).TakeSnapshot()
	snap.WALSeq = walSeq
	if err := WriteSnapshot(path, snap); err != nil {
		tb.Fatalf("WriteSnapshot: %v", err)
	}
}

// TestAFileThatDeclaresBaseOneAndCarriesSequence1001FallsBack is the guard that
// rotation re-aimed rather than deleted. It was
// TestSegmentStartingAtANonOneSequenceFallsBack, and its assertions have not
// changed; only the name has, to say what it actually guards now.
//
// It hand-builds an OBWAL\x01 file, which declares base 1 by having no base field
// and being the only file there is. Its records carry 1001 onward, so they disagree
// with the sequences their declared base implies, and recovery must still fall back
// and still re-read the whole thing. That is slice 1's Rule 1 rebased: the anchor is
// no longer "record 1 carries sequence 1" but "record 1 carries the base this file
// declares", which means exactly the same thing here and keeps meaning something on
// a rotated set.
//
// A properly rotated segment declares base 1001 in its header and its name, and its
// records agree with it, so it does NOT fall back — that is
// TestADeclaredSegmentDoesNotFallBack in rotation_test.go, and the two together are
// what stop rotation quietly turning the fallback into the common path.
func TestAFileThatDeclaresBaseOneAndCarriesSequence1001FallsBack(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "segment.wal")
	snapPath := filepath.Join(dir, "s.snap")

	const records = 100
	seqs := make([]int64, records)
	for i := range seqs {
		seqs[i] = int64(1001 + i)
	}
	handBuiltLog(t, walPath, seqs)
	// Every record in the segment is beyond the snapshot's boundary, so every one of
	// them must be applied.
	emptySnapshotAt(t, snapPath, 500)

	eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v", err)
	}
	if !rep.FellBack {
		t.Error("a log whose first record is sequence 1001 was skipped by ordinal without falling back")
	}
	if got, want := eng.TakeSnapshot().Digest(), fullParse(t, snapPath, walPath); got != want {
		t.Errorf("fallback did not reproduce the full parse\n got %s\nwant %s", got, want)
	}
	if eng.OrderCount() != records {
		t.Errorf("recovered %d orders, want %d — the segment was skipped and applied nothing", eng.OrderCount(), records)
	}
}

// TestOnlyTheFirstRecordDisagreeingStillFallsBack is the case that makes parsing
// record 1 load-bearing rather than decorative, and it took a sabotage run to find.
//
// Deleting the record-1 check does NOT break the test above: that log stops before
// the boundary, so the walk parses its last record for a sequence anyway and catches
// the disagreement there. The check earns its place on a file whose head disagrees
// and whose tail does not — the shape two writers interleaving on one path produce.
// Here record 1 carries sequence 1001 and records 2..100 carry their own ordinals,
// with the boundary at 50. Everything recovery parses agrees. Without the anchor,
// record 1 is discarded as covered when its sequence says it is not, and an order
// that should be resting is silently absent.
func TestOnlyTheFirstRecordDisagreeingStillFallsBack(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "spliced.wal")
	snapPath := filepath.Join(dir, "s.snap")

	seqs := make([]int64, 100)
	for i := range seqs {
		seqs[i] = int64(i + 1)
	}
	seqs[0] = 1001
	handBuiltLog(t, walPath, seqs)
	emptySnapshotAt(t, snapPath, 50)

	eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v", err)
	}
	if !rep.FellBack {
		t.Error("record 1's sequence disagreed with its ordinal and the skip went ahead anyway")
	}
	if got, want := eng.TakeSnapshot().Digest(), fullParse(t, snapPath, walPath); got != want {
		t.Errorf("fallback did not reproduce the full parse\n got %s\nwant %s", got, want)
	}
	// 51 records: ordinals 51..100 carry sequences past the boundary, and record 1
	// carries 1001, which is past it too.
	if eng.OrderCount() != 51 {
		t.Errorf("recovered %d orders, want 51 — record 1 was dropped as covered when its sequence says otherwise", eng.OrderCount())
	}
}

// TestASequenceThatIsNotItsOrdinalAfterTheBoundaryFallsBack covers the same rule at
// the other end. Rule 1 catches a segment; this catches a file where the invariant
// breaks somewhere recovery actually parses.
func TestASequenceThatIsNotItsOrdinalAfterTheBoundaryFallsBack(t *testing.T) {
	dir := t.TempDir()
	walPath, snapPath := buildSnapshottedLog(t, dir, 1_000, 500)

	// Ordinal 600, past the boundary, so recovery parses it.
	rewriteSeq(t, walPath, 600, 601)

	eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v", err)
	}
	if !rep.FellBack {
		t.Error("a record whose sequence is not its ordinal did not trigger the fallback")
	}
	if got, want := eng.TakeSnapshot().Digest(), fullParse(t, snapPath, walPath); got != want {
		t.Errorf("fallback did not reproduce the full parse\n got %s\nwant %s", got, want)
	}
}

// TestSnapshotAheadOfLogIsReported — the snapshot claims commands the log does not
// hold. Recovery is still correct, because those commands' effects are already in
// the snapshot, so it starts. It says so, because Open then resumes the sequence
// from the log and the venue reuses numbers the snapshot already claims.
func TestSnapshotAheadOfLogIsReported(t *testing.T) {
	dir := t.TempDir()
	walPath, _ := buildSnapshottedLog(t, dir, 100, 0)
	snapPath := filepath.Join(dir, "ahead.snap")

	snap := matching.NewEngine(tapeCfg()).TakeSnapshot()
	snap.WALSeq = 500
	if err := WriteSnapshot(snapPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v — a snapshot ahead of its log is recoverable and must not refuse", err)
	}
	if !rep.SnapshotAhead {
		t.Errorf("report = %+v, want SnapshotAhead — the condition outlives the restart and nothing else names it", rep)
	}
	if rep.Applied != 0 {
		t.Errorf("applied %d records from a log entirely behind the snapshot, want 0", rep.Applied)
	}
	if rep.LogLastSeq != 100 || rep.SnapshotSeq != 500 {
		t.Errorf("report = %+v, want LogLastSeq 100 and SnapshotSeq 500", rep)
	}
	if eng.OrderCount() != 0 {
		t.Errorf("recovered %d orders from an empty snapshot, want 0", eng.OrderCount())
	}
}

// TestLogShorterThanTheBoundaryIsReported is the same condition arrived at the other
// way: a log truncated below the snapshot's boundary. The read runs off the end of
// the file before it reaches the ordinal it was told to start at, and that must be a
// named condition rather than a quietly empty tail.
func TestLogShorterThanTheBoundaryIsReported(t *testing.T) {
	dir := t.TempDir()
	walPath, snapPath := buildSnapshottedLog(t, dir, 1_000, 500)

	raw := readFile(t, walPath)
	frames := walFrames(t, raw)
	writeFile(t, walPath, raw[:frames[299].end]) // keep 300 complete records

	eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v", err)
	}
	if !rep.SnapshotAhead || rep.LogLastSeq != 300 {
		t.Errorf("report = %+v, want SnapshotAhead with LogLastSeq 300", rep)
	}
	if rep.Applied != 0 {
		t.Errorf("applied %d records, want 0", rep.Applied)
	}
	if eng.OrderCount() != 500 {
		t.Errorf("recovered %d orders, want the snapshot's 500", eng.OrderCount())
	}
}

// stripToV1 rewrites a headered log as the format this package wrote before
// checksums existed: no magic, and [len][payload] per record with the CRC removed.
func stripToV1(tb testing.TB, dir, walPath string) string {
	tb.Helper()
	raw := readFile(tb, walPath)
	body := raw[len(Magic):]
	var legacy []byte
	for len(body) > 0 {
		n := int(binary.BigEndian.Uint32(body[0:4]))
		legacy = append(legacy, body[0:4]...)
		legacy = append(legacy, body[8:8+n]...)
		body = body[8+n:]
	}
	v1Path := filepath.Join(dir, "v1.wal")
	writeFile(tb, v1Path, legacy)
	return v1Path
}

// TestV1LogRecoversIdenticallyBehindASnapshot — a headerless log has no checksum, so
// "the payload decodes" is the only integrity signal it has and the only guard
// against walking a misframed file. Recovery therefore keeps decoding every v1
// record and saves only the retention: v1 gets the allocation win, not the parse
// win. The behaviour an operator sees must be unchanged.
func TestV1LogRecoversIdenticallyBehindASnapshot(t *testing.T) {
	dir := t.TempDir()
	walPath, snapPath := buildSnapshottedLog(t, dir, 200, 100)

	v1Path := stripToV1(t, dir, walPath)

	eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, v1Path)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v", err)
	}
	if got, want := eng.TakeSnapshot().Digest(), fullParse(t, snapPath, v1Path); got != want {
		t.Errorf("v1 recovery\n got %s\nwant %s", got, want)
	}
	if eng.OrderCount() != 200 {
		t.Errorf("recovered %d orders, want 200", eng.OrderCount())
	}
	if rep.FellBack {
		t.Error("a v1 log written by this package fell back to a second pass")
	}
	if rep.Skipped != 100 || rep.Applied != 100 {
		t.Errorf("report says %d skipped / %d applied, want 100 / 100", rep.Skipped, rep.Applied)
	}
}

// TestV1UndecodableRecordBehindTheSnapshotStillStops pins the `!checksummed` term in
// walkLog's parse predicate, which nothing else does.
//
// The test above proves v1 recovers correctly from a CLEAN log, and it passes with
// that term deleted, because a clean log decodes identically whether or not you look.
// This is the case that separates them. A v1 record has no checksum, so "it decodes"
// is the only integrity signal the format has, and it is also the only guard against
// a misframed walk: a damaged v1 length prefix makes every following frame nonsense,
// and today that nonsense fails to decode and the reader stops. Skip the decode
// behind a snapshot and the walk keeps going and hands back records from beyond the
// damage as though they were real.
//
// Here record 10 is behind the boundary at 100 and does not decode. Recovery must
// stop there, exactly as the full parse does, and apply nothing — the 200-record
// clean case applies 100.
func TestV1UndecodableRecordBehindTheSnapshotStillStops(t *testing.T) {
	dir := t.TempDir()
	walPath, snapPath := buildSnapshottedLog(t, dir, 200, 100)
	v1Path := stripToV1(t, dir, walPath)

	// v1 framing is [len][payload]; overwrite record 10's payload in place, leaving
	// its length prefix correct so the file stays framed and only the decode fails.
	raw := readFile(t, v1Path)
	off := 0
	for ord := 1; ord < 10; ord++ {
		off += 4 + int(binary.BigEndian.Uint32(raw[off:off+4]))
	}
	n := int(binary.BigEndian.Uint32(raw[off : off+4]))
	for i := off + 4; i < off+4+n; i++ {
		raw[i] = 'Z'
	}
	writeFile(t, v1Path, raw)

	// The oracle: ReadAll stops at record 9, so nothing survives the sequence filter.
	if entries, err := ReadAll(v1Path); err != nil {
		t.Fatalf("ReadAll on a v1 log err = %v, want a clean stop at the undecodable record", err)
	} else if len(entries) != 9 {
		t.Fatalf("ReadAll returned %d entries, want 9 — the fixture is not damaged where it thinks it is", len(entries))
	}

	eng, rep, err := RecoverWithReport(tapeCfg(), snapPath, v1Path)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v", err)
	}
	if got, want := eng.TakeSnapshot().Digest(), fullParse(t, snapPath, v1Path); got != want {
		t.Fatalf("v1 recovery walked past an undecodable record\n got %s\nwant %s\n"+
			"walkLog must decode every record of a headerless log, snapshot or no snapshot: it has no other check", got, want)
	}
	if eng.OrderCount() != 100 {
		t.Errorf("recovered %d orders, want the snapshot's 100 and nothing after the damage", eng.OrderCount())
	}
	if rep.Applied != 0 {
		t.Errorf("applied %d records past an undecodable one, want 0", rep.Applied)
	}
}

// TestOpenResumesFromTheLastRecordWithoutParsingTheRest — lastSeq needs one number
// and used to parse the whole file to get it, which is the SECOND full parse an
// obgw restart paid. It now walks frames and decodes one record; corruption must
// still surface, because Open refusing is what stops a venue appending to a log it
// cannot read.
func TestOpenResumesFromTheLastRecordWithoutParsingTheRest(t *testing.T) {
	dir := t.TempDir()
	walPath, _ := buildSnapshottedLog(t, dir, 50, 0)

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if w.Seq() != 50 {
		t.Errorf("resumed at seq %d, want 50", w.Seq())
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Damage a record in the middle. Open must still refuse: it reads and verifies
	// every frame even though it retains none of them.
	raw := readFile(t, walPath)
	frames := walFrames(t, raw)
	raw[frames[24].payload+2]++
	writeFile(t, walPath, raw)
	if _, err := Open(walPath); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Open err = %v, want ErrCorrupt", err)
	}
}
