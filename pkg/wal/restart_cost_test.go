package wal

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// What a restart costs when the log is long and the snapshot is recent.
//
// docs said for a long time that a snapshot "bounds restart time". It bounds what a
// restart APPLIES, and it now also bounds what a restart PARSES: covered records are
// read and CRC-verified but not decoded or retained. It still does not bound what a
// restart READS, and nothing truncates the file, so the time term still grows with
// the log — see docs/BOUNDED-RECOVERY.md §6.
//
// BenchmarkRecoverSnapshotPlusTail cannot show any of this: it builds a log that is
// only the tail, so the already-covered prefix is never present. These build the
// prefix on purpose.
//
// Two fixtures, because a restart is two terms added together and they must not be
// confused for one:
//
//   - buildCoveredLog places orders that all rest, so growing the prefix grows the
//     log AND the recovered book. It measures O(book) + O(log), which is what a real
//     restart costs.
//   - buildCoveredChurnLog writes submit/cancel pairs, so the book stays near empty
//     while the log grows. It isolates O(log), which is the only term this change
//     touches. Inverting the old assertion on the resting fixture would have reported
//     failure for a working skip, because the snapshot-restore term alone quadruples
//     there.
//
// One assertion per fixture, and they are asked different questions. The churn one
// asks whether allocation is flat, which it can, because there is no book term to
// grow. The resting one cannot ask that — its book term grows on purpose — so it asks
// what a covered record costs at the margin, which separates a working skip from a
// broken one by 6× rather than by the 2.0-against-3.2 a ratio would have to fit in.
//
// A venue that runs continuously never truncates its log, so this is the arithmetic
// that decides whether it can be restarted at all. See docs/PRODUCTION-READINESS.md,
// "Running continuously".

// setBytes is the size of the whole segment set, which is what a restart reads.
// os.Stat on the path is not, once the log has rotated: the stem is then an 18-byte
// marker and the records are in its siblings, so a stat would publish a log-MiB
// figure of 0.000017 into docs/BENCHMARKS.md.
func setBytes(tb testing.TB, stem string) int64 {
	tb.Helper()
	info, err := Stat(stem)
	if err != nil {
		tb.Fatalf("Stat: %v", err)
	}
	return info.Bytes
}

// buildCoveredLog writes prefix records, snapshots so that ALL of them are covered,
// then writes tail more. Recovery therefore has `tail` records to apply however
// large prefix is.
func buildCoveredLog(tb testing.TB, dir string, prefix, tail int) (walPath, snapPath string) {
	tb.Helper()
	walPath = filepath.Join(dir, "w.wal")
	snapPath = filepath.Join(dir, "s.snap")

	w, err := Open(walPath)
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), QueueSize: 4096, Log: w})

	place := func(n int) {
		for i := 0; i < n; i++ {
			o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
				int64(1000+i%50), 1, types.TIFGoodTillCancel)
			if err != nil {
				tb.Fatalf("NewOrder: %v", err)
			}
			r.Process(o)
		}
	}

	place(prefix)
	snap, err := r.Checkpoint()
	if err != nil {
		tb.Fatalf("Checkpoint: %v", err)
	}
	if err := WriteSnapshot(snapPath, snap); err != nil {
		tb.Fatalf("WriteSnapshot: %v", err)
	}
	place(tail)

	r.Close()
	if err := w.Close(); err != nil {
		tb.Fatalf("wal Close: %v", err)
	}
	return walPath, snapPath
}

// buildCoveredChurnLog writes prefix records as submit/cancel pairs, snapshots so
// that ALL of them are covered, then writes tail more the same way. The log grows
// with prefix and the resting book does not, so what it measures is the cost of
// getting past the covered records and nothing else.
func buildCoveredChurnLog(tb testing.TB, dir string, prefix, tail int) (walPath, snapPath string) {
	tb.Helper()
	walPath = filepath.Join(dir, "churn.wal")
	snapPath = filepath.Join(dir, "churn.snap")

	w, err := Open(walPath)
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), QueueSize: 4096, Log: w})

	// A submit and the cancel that undoes it: two records, no lasting book.
	churn := func(records int) {
		for i := 0; i < records; i += 2 {
			o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
				int64(1000+i%50), 1, types.TIFGoodTillCancel)
			if err != nil {
				tb.Fatalf("NewOrder: %v", err)
			}
			res := r.Process(o)
			if res == nil || res.Order == nil {
				tb.Fatalf("churn: order %d was not accepted", i)
			}
			if _, err := r.Cancel(res.Order.ID, "u"); err != nil {
				tb.Fatalf("churn: cancel: %v", err)
			}
		}
	}

	churn(prefix)
	snap, err := r.Checkpoint()
	if err != nil {
		tb.Fatalf("Checkpoint: %v", err)
	}
	// The premise, asserted rather than assumed: if the book grew with the log this
	// fixture would measure the same two terms added together that buildCoveredLog
	// does, and the flatness assertion below would be meaningless.
	if len(snap.Orders) > 8 {
		tb.Fatalf("churn fixture left %d resting orders after %d records; it no longer isolates the log term", len(snap.Orders), prefix)
	}
	if err := WriteSnapshot(snapPath, snap); err != nil {
		tb.Fatalf("WriteSnapshot: %v", err)
	}
	churn(tail)

	r.Close()
	if err := w.Close(); err != nil {
		tb.Fatalf("wal Close: %v", err)
	}
	return walPath, snapPath
}

// BenchmarkRecoverBehindACoveredPrefix holds the work constant and grows the log.
//
// Every case has the same 1,000 records to apply. If a snapshot bounded restart, the
// rows would read the same. They do not, and this fixture shows why in two terms at
// once: the prefix the snapshot already accounts for still has to be read, and the
// book those prefix orders rest in still has to be restored.
func BenchmarkRecoverBehindACoveredPrefix(b *testing.B) {
	for _, prefix := range []int{1_000, 50_000, 200_000} {
		b.Run(fmt.Sprintf("covered%d_tail1000", prefix), func(b *testing.B) {
			dir := b.TempDir()
			walPath, snapPath := buildCoveredLog(b, dir, prefix, 1_000)
			b.ReportMetric(float64(setBytes(b, walPath))/(1<<20), "log-MiB")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				eng, err := Recover(tapeCfg(), snapPath, walPath)
				if err != nil {
					b.Fatalf("Recover: %v", err)
				}
				_ = eng
			}
		})
	}
}

// BenchmarkRecoverBehindACoveredChurnPrefix is the same shape with the book term
// removed, so the rows are the cost of walking past covered records. This is the
// measurement docs/BENCHMARKS.md publishes for the skip, and the one whose numbers
// must not be added to the resting fixture's.
func BenchmarkRecoverBehindACoveredChurnPrefix(b *testing.B) {
	for _, prefix := range []int{1_000, 50_000, 200_000, 500_000} {
		b.Run(fmt.Sprintf("covered%d_tail1000", prefix), func(b *testing.B) {
			dir := b.TempDir()
			walPath, snapPath := buildCoveredChurnLog(b, dir, prefix, 1_000)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				eng, err := Recover(tapeCfg(), snapPath, walPath)
				if err != nil {
					b.Fatalf("Recover: %v", err)
				}
				_ = eng
			}
			// After the loop, not before it: ResetTimer deletes user-reported metrics,
			// so a size reported alongside the fixture never reaches the output.
			b.StopTimer()
			b.ReportMetric(float64(setBytes(b, walPath))/(1<<20), "log-MiB")
		})
	}
}

// allocToRecover measures what one Recover allocates over a fixture built by build.
func allocToRecover(t *testing.T, build func(testing.TB, string, int, int) (string, string), prefix, tail int) uint64 {
	t.Helper()
	dir := t.TempDir()
	walPath, snapPath := build(t, dir, prefix, tail)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	eng, err := Recover(tapeCfg(), snapPath, walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	runtime.ReadMemStats(&after)
	_ = eng
	return after.TotalAlloc - before.TotalAlloc
}

// TestRestartCostsNoAllocationForTheCoveredPrefix is the inverted form of what this
// file used to assert.
//
// It was TestRestartCostTracksTheWholeLogNotTheTail, and it pinned the defect: four
// times the log for the same work cost four times the allocation, because ReadAll
// parsed and retained every record before RestoreAfter dropped the covered ones. Its
// own failure message asked to be inverted when Recover learned to skip that prefix.
// This is that inversion, and it still fails if the skip regresses — it is the same
// measurement with the comparison the other way round.
//
// It asserts ALLOCATION and not time, deliberately. Allocation is what the skip
// removes outright: covered records are never decoded and never retained, so the
// only per-record cost left is reading their bytes and checksumming them, and that
// allocates nothing. Time falls but does not go flat, because every byte is still
// read and every CRC is still computed — see docs/BOUNDED-RECOVERY.md §5.2. A test
// asserting flat time here would be asserting something untrue.
//
// The fixture is the churn one on purpose; see the file comment.
func TestRestartCostsNoAllocationForTheCoveredPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 200k-record log")
	}
	const tail = 1_000

	alloc := func(prefix int) uint64 {
		return allocToRecover(t, buildCoveredChurnLog, prefix, tail)
	}

	small, large := alloc(50_000), alloc(200_000)
	ratio := float64(large) / float64(small)
	t.Logf("recovering behind a covered prefix: 50k -> %.1f MiB, 200k -> %.1f MiB (%.2fx for 4x the log, same %d records to apply)",
		float64(small)/(1<<20), float64(large)/(1<<20), ratio, tail)

	// Four times the log for the same work. Before the skip the ratio sat near 4;
	// with it, the covered records cost two reusable buffers between them and the
	// ratio sits at 1. 1.5 leaves room for measurement noise and for nothing else.
	if ratio > 1.5 {
		t.Errorf("recovery allocation still tracks total log size (ratio %.2f for a 4x larger log, same %d records to apply).\n"+
			"That means the covered prefix is being decoded and retained again — the skip stopped skipping.\n"+
			"Check that Recover still calls walkLog with the snapshot's WALSeq rather than reading the file whole,\n"+
			"and that walkLog's fallback (RecoverReport.FellBack) is not firing on ordinary logs.", ratio, tail)
	}
}

// TestARealRestartAllocatesForItsBookAndNotForItsLog covers the shape the churn
// fixture deliberately removes.
//
// The test above isolates the log term, which is the only one this change touches.
// That isolation left a hole: nothing asserted anything about a restart that grows
// its book along with its log, which is what a venue actually does and what
// PRODUCTION-READINESS calls "what a real restart costs". A regression that doubled
// the resting-book term while leaving the churn term flat would have been caught by
// nothing.
//
// A ratio will not do here, because the book term is genuinely O(book) and genuinely
// grows — a working skip measures near 2x on this fixture, a broken one near 3.2x,
// and a threshold between two numbers that close is a flake waiting to happen. So it
// measures the MARGINAL allocation per additional covered record instead, which
// separates the two populations by 6x: with the skip the 150,000 extra records cost
// ~355 bytes each, all of it the book they rest in; parsing and retaining them cost
// ~2,105 bytes each on top of that.
func TestARealRestartAllocatesForItsBookAndNotForItsLog(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 200k-record log")
	}
	const tail = 1_000

	small := allocToRecover(t, buildCoveredLog, 50_000, tail)
	large := allocToRecover(t, buildCoveredLog, 200_000, tail)
	perRecord := float64(large-small) / 150_000
	t.Logf("resting-book restart: 50k -> %.1f MiB, 200k -> %.1f MiB; %.0f bytes per additional covered record",
		float64(small)/(1<<20), float64(large)/(1<<20), perRecord)

	// 1,000 is three times the measured cost of a covered order that only has to be
	// restored into the book, and less than half the cost of one that is also parsed
	// out of the log and retained.
	if perRecord > 1_000 {
		t.Errorf("a covered record costs %.0f bytes of allocation on a restart whose book grows with its log, want under 1000.\n"+
			"That is the cost of restoring it into the book PLUS decoding and retaining it from the log; only the first is\n"+
			"supposed to be left. Check that Recover still passes the snapshot's WALSeq to walkLog rather than reading the\n"+
			"file whole, and that RecoverReport.FellBack is not firing on an ordinary log.", perRecord)
	}
}
