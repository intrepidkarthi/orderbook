package wal

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The measurement this whole slice exists for.
//
// Every other number here is secondary to one claim: restart cost becomes
// O(retained log) rather than O(total history). It has to be measured in the one
// shape that cannot be faked by a fixture whose book grows with its log, which is
// the mistake docs/BOUNDED-RECOVERY.md §7.1 documents — so it is built on
// submit/cancel churn, the resting book stays near empty, and what moves is the log
// and nothing else.
//
// Total history grows 100x across the benchmark's rows — roughly 11 MiB, 110 MiB
// and 1.1 GiB of journal. Retention is fixed. Recovery time must not move. The same
// benchmark with retention OFF is the control, and it must show the growth this
// package showed before rotation existed.

// buildRetainedChurnLog writes `total` records of submit/cancel churn through a
// rotating writer, checkpointing and running retention on a cadence exactly as
// cmd/obgw's checkpoint loop does, then leaves `tail` records past the last
// checkpoint for recovery to apply.
//
// retain == 0 turns deletion off, which is the control: the same total history, the
// same segments, nothing reclaimed.
func buildRetainedChurnLog(tb testing.TB, dir string, total, tail int, segBytes, retain int64) (stem, snapPath string) {
	tb.Helper()
	stem = filepath.Join(dir, "churn.wal")
	snapPath = filepath.Join(dir, "churn.snap")
	opts := Options{MaxSegmentBytes: segBytes, RetainBytes: retain, MinSegments: 2}

	w, err := OpenWith(stem, opts)
	if err != nil {
		tb.Fatalf("OpenWith: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), QueueSize: 4096, Log: w})

	churn := func(records int) {
		for i := 0; i < records; i += 2 {
			o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
				int64(1000+i%50), 1, types.TIFGoodTillCancel)
			if err != nil {
				tb.Fatalf("NewOrder: %v", err)
			}
			res := r.Process(o)
			if res == nil || res.Order == nil {
				tb.Fatalf("order %d was not accepted", i)
			}
			if _, err := r.Cancel(res.Order.ID, "u"); err != nil {
				tb.Fatalf("cancel: %v", err)
			}
		}
	}
	checkpoint := func() {
		snap, err := r.Checkpoint()
		if err != nil {
			tb.Fatalf("Checkpoint: %v", err)
		}
		if err := w.Sync(); err != nil {
			tb.Fatalf("Sync: %v", err)
		}
		if err := WriteSnapshot(snapPath, snap); err != nil {
			tb.Fatalf("WriteSnapshot: %v", err)
		}
		if _, err := Retain(stem, snapPath, opts); err != nil {
			tb.Fatalf("Retain: %v", err)
		}
	}

	// A checkpoint roughly every segment's worth of records, which is what a venue
	// with CheckpointEvery set does relative to its rotation rate.
	const perCycle = 20_000
	written := 0
	for written < total {
		n := perCycle
		if total-written < n {
			n = total - written
		}
		churn(n)
		written += n
		checkpoint()
	}
	churn(tail)

	r.Close()
	if err := w.Close(); err != nil {
		tb.Fatalf("wal Close: %v", err)
	}
	return stem, snapPath
}

// BenchmarkRestartWithRetention grows total history 100x with retention fixed, and
// again with retention off as the control.
//
// Reading the output: the `retained-MiB` metric is what the restart actually read.
// With retention on it is flat across the rows and so is `ns/op`; with it off both
// track total history. If the two columns ever move together again, retention has
// stopped bounding the read, which is the only thing this slice is for.
func BenchmarkRestartWithRetention(b *testing.B) {
	// 1 MiB segments with a 4 MiB budget, which is docs/LOG-ROTATION.md §9.1's "8
	// segments of 1 MiB" halved: the budget has to sit BELOW the smallest row's
	// history or the smallest row measures a set retention never had to touch, and
	// the comparison is then between two different things rather than between two
	// sizes of one thing.
	const (
		segBytes = 1 << 20
		retain   = 4 << 20
		tail     = 1_000
	)
	for _, mode := range []struct {
		name   string
		retain int64
	}{{"retention_on", retain}, {"retention_off", 0}} {
		for _, total := range []int{60_000, 600_000, 6_000_000} {
			b.Run(fmt.Sprintf("%s/history%d", mode.name, total), func(b *testing.B) {
				if testing.Short() && total > 60_000 {
					b.Skip("writes a multi-hundred-MiB history")
				}
				dir := b.TempDir()
				stem, snapPath := buildRetainedChurnLog(b, dir, total, tail, segBytes, mode.retain)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					eng, err := Recover(tapeCfg(), snapPath, stem)
					if err != nil {
						b.Fatalf("Recover: %v", err)
					}
					_ = eng
				}
				b.StopTimer()
				info, err := Stat(stem)
				if err != nil {
					b.Fatalf("Stat: %v", err)
				}
				b.ReportMetric(float64(info.Bytes)/(1<<20), "retained-MiB")
				b.ReportMetric(float64(info.Segments), "segments")
			})
		}
	}
}

// TestRestartCostIsBoundedByRetentionNotByHistory is BenchmarkRestartWithRetention's
// assertion, as a test, so it runs in CI rather than only when somebody remembers to
// benchmark.
//
// It is written on WALL TIME against a 1.25x threshold, which is unusual in this
// repository and deliberate: the claim being made is about restart DURATION, and
// asserting bytes-read instead would pass against an implementation that read the
// right bytes slowly. The threshold is loose because the machine is shared; the
// separation it is looking for is an order of magnitude, and the control below
// fails the test if the fixture ever stops producing one.
func TestRestartCostIsBoundedByRetentionNotByHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("writes several hundred thousand records")
	}
	const (
		segBytes = 1 << 20
		retain   = 4 << 20
		tail     = 1_000
		// Both rows must exceed the retained budget, or the small one measures a set
		// retention never had to touch and the comparison is between two different
		// things. 60,000 records of churn is roughly 11 MiB against a 4 MiB budget.
		small = 60_000
		large = 600_000 // 10x the history
	)

	measure := func(total int, retainBytes int64) (time.Duration, int64) {
		dir := t.TempDir()
		stem, snapPath := buildRetainedChurnLog(t, dir, total, tail, segBytes, retainBytes)
		info, err := Stat(stem)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		// Two runs, the faster kept: the first pays for a cold page cache.
		best := time.Duration(1<<62 - 1)
		for i := 0; i < 3; i++ {
			start := time.Now()
			if _, err := Recover(tapeCfg(), snapPath, stem); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best, info.Bytes
	}

	onSmall, bytesSmall := measure(small, retain)
	onLarge, bytesLarge := measure(large, retain)
	t.Logf("retention on:  %6d records of history -> %v, %.1f MiB retained", small, onSmall, float64(bytesSmall)/(1<<20))
	t.Logf("retention on:  %6d records of history -> %v, %.1f MiB retained", large, onLarge, float64(bytesLarge)/(1<<20))

	offSmall, offBytesSmall := measure(small, 0)
	offLarge, offBytesLarge := measure(large, 0)
	t.Logf("retention off: %6d records of history -> %v, %.1f MiB retained", small, offSmall, float64(offBytesSmall)/(1<<20))
	t.Logf("retention off: %6d records of history -> %v, %.1f MiB retained", large, offLarge, float64(offBytesLarge)/(1<<20))

	if ratio := float64(onLarge) / float64(onSmall); ratio > 1.25 {
		t.Errorf("recovery took %.2fx longer for %dx the total history with retention fixed at %d MiB (%v -> %v).\n"+
			"Retention has stopped bounding the read, which is the only thing this slice is for.\n"+
			"Check that the oldest segments are actually being deleted (floor %d) and that recovery reads only what is on disk.",
			ratio, large/small, retain>>20, onSmall, onLarge, bytesLarge)
	}
	// The control. If this does not grow, the fixture is not producing more history
	// and the assertion above is measuring nothing.
	if ratio := float64(offLarge) / float64(offSmall); ratio < 3 {
		t.Errorf("with retention OFF, %dx the history only cost %.2fx the recovery time (%v -> %v, %.1f -> %.1f MiB).\n"+
			"The fixture is not growing the log, so the retention-on assertion is vacuous.",
			large/small, ratio, offSmall, offLarge, float64(offBytesSmall)/(1<<20), float64(offBytesLarge)/(1<<20))
	}
	if offBytesLarge <= bytesLarge {
		t.Errorf("the retained set (%d bytes) is not smaller than the unretained one (%d bytes)", bytesLarge, offBytesLarge)
	}
}

// BenchmarkRotationAppendTail publishes what a rotation costs the appending
// goroutine, rather than claiming it is negligible.
//
// A rotation is two fsyncs, a link, an unlink and a directory fsync, and they land
// on the goroutine that assigns sequences — the matching goroutine when the Writer
// is a Runner's command log. At 128 MiB segments that happens every four minutes and
// is probably invisible next to a checkpoint's pause; at 1 MiB it happens every two
// seconds and will not be. A 10 ms hiccup every four minutes is a different product
// decision from a 10 ms hiccup every four seconds, so the number goes in the output
// and an operator picks.
func BenchmarkRotationAppendTail(b *testing.B) {
	for _, seg := range []struct {
		name  string
		bytes int64
	}{
		{"no_rotation", -1},
		{"segments128MiB", 128 << 20},
		{"segments1MiB", 1 << 20},
	} {
		b.Run(seg.name, func(b *testing.B) {
			dir := b.TempDir()
			w, err := OpenWith(filepath.Join(dir, "w.wal"), Options{MaxSegmentBytes: seg.bytes})
			if err != nil {
				b.Fatalf("OpenWith: %v", err)
			}
			defer w.Close()
			o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, 1000, 1, types.TIFGoodTillCancel)
			if err != nil {
				b.Fatalf("NewOrder: %v", err)
			}
			lat := make([]time.Duration, 0, b.N)
			var rotated []time.Duration
			rotations := w.Rotations()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				start := time.Now()
				if _, err := w.AppendSubmit(o); err != nil {
					b.Fatalf("AppendSubmit: %v", err)
				}
				d := time.Since(start)
				lat = append(lat, d)
				// Attribute the cost where it lands rather than inferring it from the
				// difference between two runs: the append that rotated is identifiable,
				// so its latency is measured rather than estimated.
				if n := w.Rotations(); n != rotations {
					rotations = n
					rotated = append(rotated, d)
				}
			}
			b.StopTimer()
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			pct := func(p float64) float64 {
				if len(lat) == 0 {
					return 0
				}
				i := int(float64(len(lat)-1) * p)
				return float64(lat[i].Nanoseconds())
			}
			b.ReportMetric(pct(0.50), "p50-ns")
			b.ReportMetric(pct(0.99), "p99-ns")
			b.ReportMetric(pct(0.999), "p99.9-ns")
			// The slowest append is the one a rotation landed in. At 128 MiB segments
			// there are few enough rotations in any benchmark run that they never reach
			// a percentile, which is itself the answer: the cost is real, it is rare,
			// and quoting a percentile would hide it rather than report it.
			if len(lat) > 0 {
				b.ReportMetric(float64(lat[len(lat)-1].Nanoseconds()), "max-ns")
			}
			b.ReportMetric(float64(w.Rotations()), "rotations")
			if len(rotated) > 0 {
				sort.Slice(rotated, func(i, j int) bool { return rotated[i] < rotated[j] })
				var sum time.Duration
				for _, d := range rotated {
					sum += d
				}
				b.ReportMetric(float64(sum.Nanoseconds())/float64(len(rotated)), "rot-mean-ns")
				b.ReportMetric(float64(rotated[len(rotated)-1].Nanoseconds()), "rot-max-ns")
			}
		})
	}
}
