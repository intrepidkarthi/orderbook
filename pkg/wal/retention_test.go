package wal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Retention is the half of this slice that can destroy data, so every test here is
// written to fail in the direction of "it deleted something it should not have"
// rather than "it kept too much".

// churnVenue drives a Runner over a rotating log, checkpointing and running
// retention on a cadence, the way cmd/obgw's checkpoint loop does.
type churnVenue struct {
	tb       testing.TB
	stem     string
	snapPath string
	opts     Options
	w        *Writer
	r        *matching.Runner
	n        int
}

func newChurnVenue(tb testing.TB, dir string, opts Options) *churnVenue {
	tb.Helper()
	v := &churnVenue{
		tb: tb, opts: opts,
		stem:     filepath.Join(dir, "w.wal"),
		snapPath: filepath.Join(dir, "s.snap"),
	}
	w, err := OpenWith(v.stem, opts)
	if err != nil {
		tb.Fatalf("OpenWith: %v", err)
	}
	v.w = w
	v.r = matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), QueueSize: 4096, Log: w})
	return v
}

// place writes n submit/cancel pairs, so the log grows and the resting book does
// not — the same isolation buildCoveredChurnLog uses.
func (v *churnVenue) place(n int) {
	v.tb.Helper()
	for i := 0; i < n; i += 2 {
		o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
			int64(1000+v.n%50), 1, types.TIFGoodTillCancel)
		if err != nil {
			v.tb.Fatalf("NewOrder: %v", err)
		}
		res := v.r.Process(o)
		if res == nil || res.Order == nil {
			v.tb.Fatalf("order %d was not accepted", v.n)
		}
		if _, err := v.r.Cancel(res.Order.ID, "u"); err != nil {
			v.tb.Fatalf("cancel: %v", err)
		}
		v.n += 2
	}
}

// checkpoint is the cmd/obgw sequence: snapshot, then retention against the
// snapshot that was just made durable.
func (v *churnVenue) checkpoint() RetentionResult {
	v.tb.Helper()
	snap, err := v.r.Checkpoint()
	if err != nil {
		v.tb.Fatalf("Checkpoint: %v", err)
	}
	if err := v.w.Sync(); err != nil {
		v.tb.Fatalf("Sync: %v", err)
	}
	if err := WriteSnapshot(v.snapPath, snap); err != nil {
		v.tb.Fatalf("WriteSnapshot: %v", err)
	}
	res, err := Retain(v.stem, v.snapPath, v.opts)
	if err != nil {
		v.tb.Fatalf("Retain: %v", err)
	}
	return res
}

func (v *churnVenue) close() {
	v.r.Close()
	if err := v.w.Close(); err != nil {
		v.tb.Fatalf("wal Close: %v", err)
	}
}

// TestRetentionNeverDeletesAheadOfTheSnapshot is the safety property, checked after
// every cycle while appends are running.
//
// The invariant is snap.WALSeq + 1 >= base(S₁): the snapshot plus the retained set
// must cover a contiguous sequence space, with no hole between what the base
// accounts for and what the tail still holds. Everything else in this file is a
// detail; this is the one that decides whether retention can lose a venue's book.
func TestRetentionNeverDeletesAheadOfTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	v := newChurnVenue(t, dir, Options{
		MaxSegmentBytes: 16 << 10,
		RetainBytes:     64 << 10,
		MinSegments:     2,
	})
	defer v.close()

	for cycle := 0; cycle < 25; cycle++ {
		v.place(400)
		res := v.checkpoint()

		snap, err := ReadSnapshot(v.snapPath)
		if err != nil || snap == nil {
			t.Fatalf("cycle %d: ReadSnapshot: %v", cycle, err)
		}
		set, err := enumerateSet(v.stem)
		if err != nil {
			t.Fatalf("cycle %d: enumerateSet: %v", cycle, err)
		}
		if floor := set.floor(); snap.WALSeq+1 < floor {
			t.Fatalf("cycle %d: retention outran the snapshot — it covers through %d and the oldest retained segment starts at %d.\n"+
				"Sequences %d..%d are in no file this venue can read, and it would recover a plausible book that skipped them.\n"+
				"deleted this cycle: %v", cycle, snap.WALSeq, floor, snap.WALSeq+1, floor-1, res.Deleted)
		}
		// And the venue can actually restart from what is left, at every cycle.
		eng, rep, err := RecoverWithReport(tapeCfg(), v.snapPath, v.stem)
		if err != nil {
			t.Fatalf("cycle %d: restart after retention: %v", cycle, err)
		}
		if rep.FellBack {
			t.Fatalf("cycle %d: a retained set fell back to a full re-read", cycle)
		}
		_ = eng
	}

	if got := segmentSetOf(t, v.stem); len(got) < 2 {
		t.Fatalf("retention left %d segments; at least the active one plus the floor must remain", len(got))
	}
	info, err := Stat(v.stem)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Floor <= 1 {
		t.Errorf("nothing was ever deleted (floor %d), so this test asserted nothing", info.Floor)
	}
	t.Logf("after 25 cycles: %d segments, %.1f KiB retained, floor at sequence %d, %d records written",
		info.Segments, float64(info.Bytes)/1024, info.Floor, v.n)
}

// TestRetentionKeepsWholeSegmentsTheSnapshotOnlyPartlyCovers pins term (b) as
// `WALSeq >= last(S)` rather than `>= base(S)`.
//
// It is the subtle sabotage: relaxing it to base(S) is wrong only for the records
// ABOVE the snapshot's sequence inside the deleted segment, so a test that counted
// segments would pass and a venue would lose a few hundred commands per cycle.
func TestRetentionKeepsWholeSegmentsTheSnapshotOnlyPartlyCovers(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	snapPath := filepath.Join(dir, "s.snap")

	// Ten segments of 100 sequences each, hand-built so the arithmetic is exact.
	const per = 100
	for s := 0; s < 10; s++ {
		base := int64(s*per + 1)
		seqs := make([]int64, per)
		for i := range seqs {
			seqs[i] = base + int64(i)
		}
		handBuiltSegment(t, segPath(stem, base), base, seqs)
	}
	// The snapshot covers through 450: segment 5 (401..500) is only half covered.
	emptySnapshotAt(t, snapPath, 450)

	res, err := Retain(stem, snapPath, Options{RetainBytes: 1, MinSegments: -1})
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if res.Floor != 401 {
		t.Fatalf("retention left the floor at %d, want 401 — a segment ending at %d is not covered by a snapshot at 450.\n"+
			"deleted: %v", res.Floor, 500, res.Deleted)
	}
	// And the venue still starts, which is the point of the floor being where it is.
	if _, err := Recover(tapeCfg(), snapPath, stem); err != nil {
		t.Fatalf("Recover after retention: %v", err)
	}
}

// TestRetentionIgnoresACorruptSnapshot. A snapshot that exists and fails its CRC is
// one ReadSnapshot refuses and recovery will not use, so the log is the only base
// the venue has left. Gating retention on existence rather than on verifiability
// would delete exactly that fallback.
func TestRetentionIgnoresACorruptSnapshot(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	snapPath := filepath.Join(dir, "s.snap")

	const per = 100
	for s := 0; s < 10; s++ {
		base := int64(s*per + 1)
		seqs := make([]int64, per)
		for i := range seqs {
			seqs[i] = base + int64(i)
		}
		handBuiltSegment(t, segPath(stem, base), base, seqs)
	}
	emptySnapshotAt(t, snapPath, 900)

	before := segmentSetOf(t, stem)

	raw := readFile(t, snapPath)
	raw[len(SnapMagic)+8]++ // inside the body, so the snapshot's own CRC fails
	writeFile(t, snapPath, raw)
	if _, err := ReadSnapshot(snapPath); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("the fixture's snapshot still verifies (%v); this test asserts nothing", err)
	}

	res, err := Retain(stem, snapPath, Options{RetainBytes: 1, MinSegments: -1})
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("retention deleted %v against a snapshot that does not verify — that is the recovery fallback it just destroyed", res.Deleted)
	}
	if got := segmentSetOf(t, stem); len(got) != len(before) {
		t.Errorf("the set went from %d segments to %d", len(before), len(got))
	}
	if res.Skipped == "" {
		t.Error("retention skipped a cycle and said nothing about why")
	}
}

// TestRetentionIsOffByDefault. Deleting a venue's journal is not a behaviour anybody
// should acquire by upgrading, and it is the one thing in this package that cannot
// be undone.
func TestRetentionIsOffByDefault(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	snapPath := filepath.Join(dir, "s.snap")
	const per = 100
	for s := 0; s < 10; s++ {
		base := int64(s*per + 1)
		seqs := make([]int64, per)
		for i := range seqs {
			seqs[i] = base + int64(i)
		}
		handBuiltSegment(t, segPath(stem, base), base, seqs)
	}
	emptySnapshotAt(t, snapPath, 1_000)

	res, err := Retain(stem, snapPath, Options{})
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("the default Options deleted %v", res.Deleted)
	}
	if len(segmentSetOf(t, stem)) != 10 {
		t.Errorf("the set is no longer 10 segments")
	}
}

// TestRetentionNeverDeletesTheActiveSegment. Not only because it is being written:
// Open derives the next sequence from the newest segment, so deleting it restarts
// the venue's sequence space underneath a snapshot that already claims those
// numbers — permanently, and self-inflicted.
func TestRetentionNeverDeletesTheActiveSegment(t *testing.T) {
	dir := t.TempDir()
	v := newChurnVenue(t, dir, Options{MaxSegmentBytes: 8 << 10, RetainBytes: 1, MinSegments: -1})
	v.place(2_000)
	res := v.checkpoint()
	active, _ := v.w.ActiveSegment()
	v.close()

	for _, name := range res.Deleted {
		if filepath.Join(dir, name) == active {
			t.Fatalf("retention deleted the active segment %s", name)
		}
	}
	names := segmentSetOf(t, v.stem)
	if len(names) < 1 {
		t.Fatal("retention emptied the set")
	}
	if filepath.Join(dir, names[len(names)-1]) != active {
		t.Errorf("the newest segment is %s, want the active %s", names[len(names)-1], filepath.Base(active))
	}
	// The sequence space survives the restart, which is the failure this guards.
	w, err := OpenWith(v.stem, Options{MaxSegmentBytes: 8 << 10})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer w.Close()
	if w.Seq() < int64(v.n) {
		t.Errorf("after retention the venue resumes at sequence %d, having written %d records", w.Seq(), v.n)
	}
}

// TestRetentionRespectsTheSegmentFloor — MinSegments is not a correctness term, the
// snapshot predicate is. It keeps recent history on disk for the forensics the
// runbooks assume and keeps a reconnecting follower out of the bootstrap path.
func TestRetentionRespectsTheSegmentFloor(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	snapPath := filepath.Join(dir, "s.snap")
	const per = 100
	for s := 0; s < 10; s++ {
		base := int64(s*per + 1)
		seqs := make([]int64, per)
		for i := range seqs {
			seqs[i] = base + int64(i)
		}
		handBuiltSegment(t, segPath(stem, base), base, seqs)
	}
	emptySnapshotAt(t, snapPath, 1_000)

	if _, err := Retain(stem, snapPath, Options{RetainBytes: 1, MinSegments: 4}); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	// Four sealed plus the active one.
	if got := segmentSetOf(t, stem); len(got) != 5 {
		t.Fatalf("retention left %d segments, want 5 (a floor of 4 sealed plus the active one): %v", len(got), got)
	}
}

// TestCrashDuringRetention. Deletion is oldest-first precisely so that a crash
// part-way through leaves a prefix of length k for some k less than intended, which
// is a VALID set. That is also what makes one directory fsync after the last unlink
// sufficient.
func TestCrashDuringRetention(t *testing.T) {
	const per = 100
	build := func(t *testing.T) (stem, snapPath string) {
		dir := t.TempDir()
		stem = filepath.Join(dir, "w.wal")
		snapPath = filepath.Join(dir, "s.snap")
		for s := 0; s < 10; s++ {
			base := int64(s*per + 1)
			seqs := make([]int64, per)
			for i := range seqs {
				seqs[i] = base + int64(i)
			}
			handBuiltSegment(t, segPath(stem, base), base, seqs)
		}
		emptySnapshotAt(t, snapPath, 900)
		return stem, snapPath
	}

	// The book the untouched set recovers to.
	base, baseSnap := build(t)
	wantEng, err := Recover(tapeCfg(), baseSnap, base)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	want := wantEng.TakeSnapshot().Digest()

	for k := 0; k <= 9; k++ {
		t.Run(fmt.Sprintf("after%dunlinks", k), func(t *testing.T) {
			stem, snapPath := build(t)
			// Simulate the crash by deleting the k oldest segments by hand — which is
			// exactly the state a killed process leaves, because deletion is oldest
			// first and each unlink is atomic.
			names := segmentSetOf(t, stem)
			for i := 0; i < k && i < len(names)-1; i++ {
				if err := os.Remove(filepath.Join(filepath.Dir(stem), names[i])); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			}
			eng, err := Recover(tapeCfg(), snapPath, stem)
			if err != nil {
				t.Fatalf("a set left by a crash after %d unlinks does not recover: %v", k, err)
			}
			if got := eng.TakeSnapshot().Digest(); got != want {
				t.Errorf("after %d unlinks the recovered book differs\n got %s\nwant %s", k, got, want)
			}
			// And the next retention cycle picks up where the crash left off.
			if _, err := Retain(stem, snapPath, Options{RetainBytes: 1, MinSegments: -1}); err != nil {
				t.Errorf("retention after a crash: %v", err)
			}
		})
	}

	// The half the hand-simulated crash above cannot test: that retention's OWN
	// deletion order is oldest-first. Above, the k deleted segments are chosen by the
	// test; here they are chosen by the predicate, and a budget that permits exactly
	// k deletions must produce exactly the k-oldest gone and a set with no hole in it.
	// Deleting newest-first would satisfy the byte budget just as well and leave a
	// set that cannot be recovered at all.
	for k := 1; k <= 8; k++ {
		t.Run(fmt.Sprintf("budget_for_%d_deletions", k), func(t *testing.T) {
			stem, snapPath := build(t)
			set, err := enumerateSetByName(stem)
			if err != nil {
				t.Fatalf("enumerateSetByName: %v", err)
			}
			// The budget is the exact size of the set with its k oldest segments gone,
			// so "delete until you are inside the budget" means exactly k and the
			// assertion below is arithmetic rather than an approximation.
			budget := set.bytes()
			for i := 0; i < k; i++ {
				budget -= set.segs[i].size
			}
			res, err := Retain(stem, snapPath, Options{RetainBytes: budget, MinSegments: -1})
			if err != nil {
				t.Fatalf("Retain: %v", err)
			}
			if len(res.Deleted) != k {
				t.Fatalf("a budget permitting %d deletions deleted %d: %v", k, len(res.Deleted), res.Deleted)
			}
			if want := int64(k*per_) + 1; res.Floor != want {
				t.Fatalf("floor is %d after %d deletions, want %d — the segments deleted were not the oldest %d",
					res.Floor, k, want, k)
			}
			eng, err := Recover(tapeCfg(), snapPath, stem)
			if err != nil {
				t.Fatalf("after retention deleted %d segments the set does not recover: %v\n"+
					"Deleting anything but a prefix leaves a hole, which is the shape startup validation exists to catch\n"+
					"and the one thing retention must never inflict on itself.", k, err)
			}
			// The book is the snapshot plus what is left, which is the same book: the
			// deleted segments are all covered by the snapshot at 900.
			if got := eng.TakeSnapshot().Digest(); got != want_(t, base, baseSnap) {
				t.Errorf("after %d deletions the recovered book differs\n got %s", k, got)
			}
		})
	}
}

// per_ is the number of sequences each hand-built segment in TestCrashDuringRetention
// holds, named so the floor arithmetic above reads as arithmetic.
const per_ = 100

func want_(t *testing.T, stem, snapPath string) string {
	t.Helper()
	eng, err := Recover(tapeCfg(), snapPath, stem)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return eng.TakeSnapshot().Digest()
}

// TestArchivalHappensBeforeDeletion. A venue running retention without archival has
// a recovery point equal to its newest snapshot and is one corrupt snapshot away
// from nothing, which is the argument for -wal-archive being the first flag an
// operator sets after -wal-retain.
func TestArchivalHappensBeforeDeletion(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	snapPath := filepath.Join(dir, "s.snap")
	archive := filepath.Join(dir, "archive")
	const per = 100
	for s := 0; s < 10; s++ {
		base := int64(s*per + 1)
		seqs := make([]int64, per)
		for i := range seqs {
			seqs[i] = base + int64(i)
		}
		handBuiltSegment(t, segPath(stem, base), base, seqs)
	}
	emptySnapshotAt(t, snapPath, 1_000)

	res, err := Retain(stem, snapPath, Options{RetainBytes: 1, MinSegments: -1, ArchiveDir: archive})
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if len(res.Deleted) == 0 {
		t.Fatal("nothing was deleted, so nothing was archived")
	}
	archived, err := ArchivedSegments(archive, stem)
	if err != nil {
		t.Fatalf("ArchivedSegments: %v", err)
	}
	if len(archived) != len(res.Deleted) {
		t.Fatalf("archived %d segments and deleted %d", len(archived), len(res.Deleted))
	}
	// Byte-identical: an archived segment is the segment. Nothing is compressed,
	// encrypted or re-checksummed, so it can be copied back into place.
	for _, name := range archived {
		raw := readFile(t, filepath.Join(archive, name))
		if len(raw) < SegHeaderBytesV3 || string(raw[:len(SegMagicV3)]) != SegMagicV3 {
			t.Errorf("archived %s is not a segment", name)
		}
	}
	// Restore the archive and the whole history reads again.
	for _, name := range archived {
		writeFile(t, filepath.Join(dir, name), readFile(t, filepath.Join(archive, name)))
	}
	entries, err := ReadAll(stem)
	if err != nil {
		t.Fatalf("ReadAll after restoring the archive: %v", err)
	}
	if len(entries) != 1_000 {
		t.Errorf("restored set holds %d records, want 1000", len(entries))
	}
}

// TestArchivalFailureStopsRetention — a segment that could not be archived is not
// deleted. The whole point of (f) is that the copy is a precondition, not a
// best-effort side effect.
func TestArchivalFailureStopsRetention(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	snapPath := filepath.Join(dir, "s.snap")
	const per = 100
	for s := 0; s < 10; s++ {
		base := int64(s*per + 1)
		seqs := make([]int64, per)
		for i := range seqs {
			seqs[i] = base + int64(i)
		}
		handBuiltSegment(t, segPath(stem, base), base, seqs)
	}
	emptySnapshotAt(t, snapPath, 1_000)

	// An archive directory that cannot be written to.
	archive := filepath.Join(dir, "archive")
	if err := os.Mkdir(archive, 0o500); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(archive, 0o755) })

	res, err := Retain(stem, snapPath, Options{RetainBytes: 1, MinSegments: -1, ArchiveDir: archive})
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("retention deleted %v that it could not archive", res.Deleted)
	}
	if res.Skipped == "" {
		t.Error("retention stopped and said nothing about why")
	}
}

// TestReadAfterBootstrapsBelowTheFloor is the log-shipping half of retention.
//
// A primary that shipped whatever it still held would send a catch-up starting at
// some segment's base; the follower's gap check would see a sequence past the one it
// expects and terminate with "gap in the feed" — a protocol error raised against the
// one source that is supposed to be authoritative. The right answer is a snapshot,
// which is the same answer market data already gives an evicted subscriber.
func TestReadAfterBootstrapsBelowTheFloor(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")
	const per = 100
	for s := 0; s < 6; s++ {
		base := int64(s*per + 1)
		seqs := make([]int64, per)
		for i := range seqs {
			seqs[i] = base + int64(i)
		}
		handBuiltSegment(t, segPath(stem, base), base, seqs)
	}
	// Retention has deleted the first two segments.
	for s := 0; s < 2; s++ {
		if err := os.Remove(segPath(stem, int64(s*per+1))); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	}

	// A follower at 250 is inside the retained set: it gets records.
	entries, err := ReadAfter(stem, 250)
	if err != nil {
		t.Fatalf("ReadAfter(250): %v", err)
	}
	if len(entries) == 0 || entries[0].Seq != 251 {
		t.Fatalf("catch-up from 250 starts at %v, want 251", entries[0].Seq)
	}

	// A follower at 200 needs record 201, which is exactly the floor: still fine.
	if _, err := ReadAfter(stem, 200); err != nil {
		t.Errorf("ReadAfter(200) at exactly the floor: %v", err)
	}

	// A follower at 50 needs record 51, which is gone.
	if _, err := ReadAfter(stem, 50); !errors.Is(err, ErrBelowFloor) {
		t.Fatalf("ReadAfter(50) err = %v, want ErrBelowFloor — the primary would have shipped a gap", err)
	}
}

// TestRetentionRacingAReaderIsSurvivable. Retention runs in the same process on the
// checkpoint cadence, so a catch-up read can race an unlink. The fix is not a lease
// or a refcount: an already-open fd survives an unlink on POSIX, so the reader opens
// what it selected and reads afterwards, and ENOENT on open means "re-enumerate".
func TestRetentionRacingAReaderIsSurvivable(t *testing.T) {
	dir := t.TempDir()
	v := newChurnVenue(t, dir, Options{MaxSegmentBytes: 16 << 10, RetainBytes: 48 << 10, MinSegments: 1})
	defer v.close()
	v.place(1_000)
	v.checkpoint()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Whatever the floor is at this instant, reading from it must either work
			// or say ErrBelowFloor. It must never report corruption or a gap.
			info, err := Stat(v.stem)
			if err != nil {
				continue
			}
			if _, err := ReadAfter(v.stem, info.Floor); err != nil && !errors.Is(err, ErrBelowFloor) {
				t.Errorf("catch-up read racing retention: %v", err)
				return
			}
		}
	}()

	for cycle := 0; cycle < 12; cycle++ {
		v.place(400)
		v.checkpoint()
	}
	close(stop)
	wg.Wait()
}

// TestRetentionLeavesAMarkerWhereTheStemWas covers the one file whose ABSENCE is
// silent.
//
// A pre-rotation log can still be the oldest member of a set: `Open` seals a torn
// stem where it stands and starts the next segment at last+1, so the stem stays in
// the set holding records 1..k. When retention comes to reclaim it, unlinking it
// would leave numbered segments and nothing at the path the operator passed to
// -wal — which is the one shape a downgrade cannot survive, because an older build
// finds no file, concludes there is no log, and starts an empty venue beside
// everything. It is replaced by the marker instead, atomically.
func TestRetentionLeavesAMarkerWhereTheStemWas(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "w.wal")

	// A pre-rotation log with a torn tail, which Open seals rather than appends to.
	writeLog(t, stem, 40)
	raw := readFile(t, stem)
	writeFile(t, stem, raw[:len(raw)-9])

	opts := Options{MaxSegmentBytes: 4 << 10, RetainBytes: 1, MinSegments: -1}
	w, err := OpenWith(stem, opts)
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	placeUntilError(t, w, 40, 600)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st, err := os.Stat(stem); err != nil || st.Size() == int64(SegHeaderBytes) {
		t.Fatalf("the fixture's stem is not a pre-rotation log holding records (%v)", err)
	}
	if len(segmentSetOf(t, stem)) < 3 {
		t.Fatalf("the fixture produced %d numbered segments, want at least 3", len(segmentSetOf(t, stem)))
	}

	snapPath := filepath.Join(dir, "s.snap")
	emptySnapshotAt(t, snapPath, 10_000) // covers everything
	res, err := Retain(stem, snapPath, opts)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if len(res.Deleted) == 0 {
		t.Fatal("retention reclaimed nothing, so this test asserts nothing")
	}

	st, err := os.Stat(stem)
	if err != nil {
		t.Fatalf("the stem is gone after retention: %v\n"+
			"A set whose stem does not exist is the one shape an older build cannot detect: it finds no file at -wal,\n"+
			"concludes there is no log, and starts an EMPTY venue beside segments holding everything.", err)
	}
	if st.Size() != int64(SegHeaderBytes) {
		t.Errorf("the stem is %d bytes after retention, want the %d-byte marker", st.Size(), SegHeaderBytes)
	}
	if _, err := Recover(tapeCfg(), snapPath, stem); err != nil {
		t.Errorf("the set does not recover after its stem was reclaimed: %v", err)
	}
}

// TestArchivingIntoTheLogsOwnDirectoryIsRefused.
//
// The failure this prevents is silent and total. With ArchiveDir set to the
// directory holding the set, a segment's archive target IS the segment: the
// idempotency check in archiveSegment — a file already at the target with a matching
// size, which is what makes a cycle that crashed between the copy and the unlink safe
// to repeat — finds the live segment, agrees it is already archived, and returns
// success. reclaim unlinks it immediately afterwards.
//
// Measured before the fix, on a 24-segment set: 22 reported in res.Archived, 22 gone
// from disk, ArchivedSegments returning only the two still live in the set, and
// cmd/obgw logging "retention deleted 22 segment(s) ..., archived 22" — a success
// line for the total loss of the archive.
//
// It is an easy mistake to make on purpose. Retention without archival has a recovery
// point of "the newest snapshot", so main.go and every operator document here push
// -wal-archive at anyone who sets -wal-retain, and the log directory is the first path
// to hand.
func TestArchivingIntoTheLogsOwnDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	v := newChurnVenue(t, dir, Options{
		MaxSegmentBytes: 4 << 10,
		RetainBytes:     8 << 10,
		MinSegments:     1,
		ArchiveDir:      dir,
	})
	defer v.close()
	v.place(600)

	snap, err := v.r.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := v.w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := WriteSnapshot(v.snapPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	before, err := Stat(v.stem)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if before.Segments < 3 {
		t.Fatalf("the fixture has %d segments; it is not exercising retention", before.Segments)
	}

	res, err := Retain(v.stem, v.snapPath, v.opts)
	if !errors.Is(err, ErrArchiveIsTheLog) {
		t.Fatalf("Retain returned (%v), want ErrArchiveIsTheLog.\n"+
			"Archiving into the set's own directory reports every segment archived and destroys every one of "+
			"them: deleted=%d archived=%d", err, len(res.Deleted), len(res.Archived))
	}
	if len(res.Deleted) != 0 {
		t.Errorf("retention deleted %d segment(s) despite refusing the archive: %v", len(res.Deleted), res.Deleted)
	}
	after, err := Stat(v.stem)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if after.Segments != before.Segments {
		t.Errorf("the set went from %d segments to %d; nothing may be deleted when the archive is refused",
			before.Segments, after.Segments)
	}
}

// TestTheArchiveDirectoryIsComparedByInodeNotByString — the same directory reached
// by a different path is the same mistake, and a string comparison alone would let a
// symlink or a relative path through into the destructive case.
func TestTheArchiveDirectoryIsComparedByInodeNotByString(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "as-a-symlink")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("this filesystem will not make a symlink: %v", err)
	}
	if err := CheckArchiveDir(link, dir); !errors.Is(err, ErrArchiveIsTheLog) {
		t.Errorf("CheckArchiveDir through a symlink to the log directory = %v, want ErrArchiveIsTheLog", err)
	}
	if err := CheckArchiveDir(dir+string(filepath.Separator), dir); !errors.Is(err, ErrArchiveIsTheLog) {
		t.Errorf("CheckArchiveDir with a trailing separator = %v, want ErrArchiveIsTheLog", err)
	}
	// A SUBDIRECTORY of the log directory is fine, and must stay fine: the enumerator
	// skips anything that is not a regular file, so archived copies under it are never
	// mistaken for segments.
	sub := filepath.Join(dir, "archive")
	if err := CheckArchiveDir(sub, dir); err != nil {
		t.Errorf("CheckArchiveDir on a subdirectory of the log directory = %v, want nil", err)
	}
	if err := CheckArchiveDir("", dir); err != nil {
		t.Errorf("CheckArchiveDir with archival off = %v, want nil", err)
	}
}

// TestTheByteBudgetIsFlooredByMinSegments pins the interaction the operator-facing
// sizing numbers were written as though it did not exist.
//
// RetainBytes is not a bound the retained set is held to; it is the point at which
// retention STOPS wanting to delete. MinSegments is a separate term that stops it
// being ABLE to, and it is checked after the budget, so the floor always wins. The
// smallest set retention will leave is therefore MinSegments sealed segments plus the
// active one — (MinSegments + 1) x MaxSegmentBytes, which at the shipped defaults of
// 4 and 128 MiB is 640 MiB, whatever -wal-retain says.
//
// Asserted with a one-byte budget, which is the clearest possible statement of it:
// nothing can satisfy RetainBytes: 1, so whatever is left is the floor and nothing
// else. An operator who sets -wal-retain 100MiB against the defaults gets 640 MiB,
// and before this the only thing that said so was res.Skipped, which cmd/obgw
// discarded.
func TestTheByteBudgetIsFlooredByMinSegments(t *testing.T) {
	const segBytes = 4 << 10
	for _, minSegs := range []int{1, 4} {
		dir := t.TempDir()
		v := newChurnVenue(t, dir, Options{
			MaxSegmentBytes: segBytes,
			RetainBytes:     1, // a budget nothing can satisfy
			MinSegments:     minSegs,
		})
		v.place(1200)
		res := v.checkpoint()
		v.close()

		info, err := Stat(v.stem)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Segments != minSegs+1 {
			t.Errorf("MinSegments %d left %d segments, want %d — the floor is MinSegments sealed plus the "+
				"active one", minSegs, info.Segments, minSegs+1)
		}
		if res.Skipped == "" {
			t.Errorf("MinSegments %d: retention stopped at the floor and said nothing in Skipped, which is "+
				"the only place an operator can learn why the disk is not being reclaimed", minSegs)
		}
		// The floor is a byte number, and it is the one the sizing advice has to use.
		if info.Bytes <= int64(minSegs)*segBytes {
			t.Errorf("MinSegments %d retained %d bytes, want more than %d — the arithmetic in the docs is "+
				"(MinSegments + 1) x MaxSegmentBytes", minSegs, info.Bytes, int64(minSegs)*segBytes)
		}
	}
}
