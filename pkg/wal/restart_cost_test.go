package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// What a restart costs when the log is long and the snapshot is recent.
//
// docs said for a long time that a snapshot "bounds restart time". It bounds what a
// restart APPLIES. wal.Recover calls ReadAll, which parses the whole file, and only
// then does RestoreAfter skip the records the snapshot already covers — so the cost
// scales with the total log and not with the tail.
//
// BenchmarkRecoverSnapshotPlusTail cannot show this: it builds a log that is only
// the tail, so the already-covered prefix it exists to skip is never present. These
// build the prefix on purpose.
//
// A venue that runs continuously never truncates its log, so this is the arithmetic
// that decides whether it can be restarted at all. See docs/PRODUCTION-READINESS.md,
// "Running continuously".

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

// BenchmarkRecoverBehindACoveredPrefix holds the work constant and grows the log.
//
// Every case has the same 1,000 records to apply. If a snapshot bounded restart, the
// rows would read the same. They do not: the cost tracks the prefix, which is the
// part the snapshot already accounts for.
func BenchmarkRecoverBehindACoveredPrefix(b *testing.B) {
	for _, prefix := range []int{1_000, 50_000, 200_000} {
		b.Run(fmt.Sprintf("covered%d_tail1000", prefix), func(b *testing.B) {
			dir := b.TempDir()
			walPath, snapPath := buildCoveredLog(b, dir, prefix, 1_000)
			if fi, err := os.Stat(walPath); err == nil {
				b.ReportMetric(float64(fi.Size())/(1<<20), "log-MiB")
			}
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

// TestRestartCostTracksTheWholeLogNotTheTail pins the shape so a change to it is
// deliberate, in either direction.
//
// It asserts the CURRENT behaviour, which is the undesirable one. That is on
// purpose: the property is load-bearing for anyone running this continuously, and an
// unpinned defect is one that can silently get worse. When Recover learns to skip a
// covered prefix without parsing it — the record ordinal is the sequence, so it can —
// this test should fail, and the fix is to invert it: assert that quadrupling the
// prefix leaves the cost flat.
func TestRestartCostTracksTheWholeLogNotTheTail(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 200k-record log")
	}
	const tail = 1_000

	alloc := func(prefix int) uint64 {
		dir := t.TempDir()
		walPath, snapPath := buildCoveredLog(t, dir, prefix, tail)

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

	small, large := alloc(50_000), alloc(200_000)
	ratio := float64(large) / float64(small)
	t.Logf("recovering behind a covered prefix: 50k -> %.1f MiB, 200k -> %.1f MiB (%.1fx for 4x the log, same %d records to apply)",
		float64(small)/(1<<20), float64(large)/(1<<20), ratio, tail)

	// Four times the log for the same work. Today the cost follows the log, so the
	// ratio sits near 4; a bound would put it near 1.
	if ratio < 2 {
		t.Errorf("recovery cost no longer tracks total log size (ratio %.2f for a 4x larger log).\n"+
			"If Recover now skips the covered prefix, that is the fix this test was waiting for:\n"+
			"invert it to assert the ratio stays near 1, and update docs/PRODUCTION-READINESS.md.", ratio)
	}
	// And a guard against it getting worse than linear.
	if ratio > 6 {
		t.Errorf("recovery cost is growing faster than the log (ratio %.2f for a 4x larger log)", ratio)
	}
}
