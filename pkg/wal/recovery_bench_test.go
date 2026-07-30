package wal

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Recovery-time benchmarks.
//
// The package doc has always said recovery is "bounded to O(recent) by snapshotting
// and replaying only the WAL tail", and docs/BENCHMARKS.md published no recovery time
// at all — only the cost of *writing* the log. Restart duration is the number an
// operator actually asks for, and "O(recent)" is a shape, not an answer: it says the
// cost is proportional to the tail without saying what the constant is.
//
// So these measure the three things a restart is made of, each at several sizes so
// the scaling is visible rather than asserted:
//
//   - reading and replaying a log tail
//   - loading a snapshot
//   - the two together, which is what a real recovery does
//
// Prompted by joaquinbejar/OrderBook-rs, which publishes replay and snapshot-restore
// times at 100 / 1,000 / 10,000 and is right to.

// buildLog writes n submit records and returns the path.
func buildLog(b *testing.B, dir string, n int) string {
	b.Helper()
	path := filepath.Join(dir, fmt.Sprintf("wal-%d.log", n))
	w, err := Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	for i := 0; i < n; i++ {
		o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
			int64(1000+i%4000), 1, types.TIFGoodTillCancel)
		if err != nil {
			b.Fatalf("NewOrder: %v", err)
		}
		if _, err := w.AppendSubmit(o); err != nil {
			b.Fatalf("AppendSubmit: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		b.Fatalf("Close: %v", err)
	}
	return path
}

func benchCfg() matching.Config {
	c := matching.DefaultConfig("X")
	c.MaxOrders = 2_000_000
	c.DedupClientOrderIDs = 4096
	return c
}

// BenchmarkReadAll isolates the cost of reading and checksum-verifying the log,
// separately from applying it, so a slow recovery can be attributed to the right half.
func BenchmarkReadAll(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("%d_records", n), func(b *testing.B) {
			path := buildLog(b, b.TempDir(), n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				entries, err := ReadAll(path)
				if err != nil {
					b.Fatalf("ReadAll: %v", err)
				}
				if len(entries) != n {
					b.Fatalf("read %d entries, want %d", len(entries), n)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(n), "records")
		})
	}
}

// BenchmarkReplayTail is the operator-facing number: how long a restart takes when it
// has to replay this many commands, end to end, from an empty engine.
func BenchmarkReplayTail(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("%d_records", n), func(b *testing.B) {
			path := buildLog(b, b.TempDir(), n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				eng, err := Recover(benchCfg(), "", path)
				if err != nil {
					b.Fatalf("Recover: %v", err)
				}
				if eng.OrderCount() != n {
					b.Fatalf("recovered %d orders, want %d", eng.OrderCount(), n)
				}
			}
			b.StopTimer()
			// Per-record cost is the useful comparison across sizes; the whole-restart
			// figure is ns/op.
			b.ReportMetric(float64(n), "records")
		})
	}
}

// BenchmarkWriteSnapshot — the pause a checkpoint imposes on the matching goroutine,
// at several book sizes, since checkpoint cadence trades restart time against this.
func BenchmarkWriteSnapshot(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("%d_orders", n), func(b *testing.B) {
			eng := matching.NewEngine(benchCfg())
			buf := make([]types.Trade, 0, 8)
			for i := 0; i < n; i++ {
				o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
					int64(1000+i%4000), 1, types.TIFGoodTillCancel)
				if err != nil {
					b.Fatalf("NewOrder: %v", err)
				}
				buf, _, _ = eng.Match(o, buf[:0])
			}
			snap := eng.TakeSnapshot()
			path := filepath.Join(b.TempDir(), "snap.json")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := WriteSnapshot(path, snap); err != nil {
					b.Fatalf("WriteSnapshot: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(n), "orders")
		})
	}
}

// BenchmarkRestoreSnapshot — loading a snapshot with no log tail, which is the floor
// under any recovery: a restart cannot be faster than reading its own checkpoint.
func BenchmarkRestoreSnapshot(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("%d_orders", n), func(b *testing.B) {
			dir := b.TempDir()
			eng := matching.NewEngine(benchCfg())
			buf := make([]types.Trade, 0, 8)
			for i := 0; i < n; i++ {
				o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
					int64(1000+i%4000), 1, types.TIFGoodTillCancel)
				if err != nil {
					b.Fatalf("NewOrder: %v", err)
				}
				buf, _, _ = eng.Match(o, buf[:0])
			}
			snapPath := filepath.Join(dir, "snap.json")
			if err := WriteSnapshot(snapPath, eng.TakeSnapshot()); err != nil {
				b.Fatalf("WriteSnapshot: %v", err)
			}
			// An empty log, so this measures the snapshot half alone.
			logPath := buildLog(b, dir, 0)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := Recover(benchCfg(), snapPath, logPath)
				if err != nil {
					b.Fatalf("Recover: %v", err)
				}
				if got.OrderCount() != n {
					b.Fatalf("restored %d orders, want %d", got.OrderCount(), n)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(n), "orders")
		})
	}
}

// BenchmarkRecoverSnapshotPlusTail is what a real restart does, and the measurement
// that gives "bounded to O(recent)" a number: a large book from a snapshot plus a
// short tail of commands after it.
//
// The point of the comparison is the pair of tail sizes at a fixed book: if the claim
// holds, the difference between them is the tail and not the book.
func BenchmarkRecoverSnapshotPlusTail(b *testing.B) {
	const book = 100_000
	for _, tail := range []int{0, 1_000, 10_000} {
		b.Run(fmt.Sprintf("book100k_tail%d", tail), func(b *testing.B) {
			dir := b.TempDir()
			eng := matching.NewEngine(benchCfg())
			buf := make([]types.Trade, 0, 8)
			for i := 0; i < book; i++ {
				o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit,
					int64(1000+i%4000), 1, types.TIFGoodTillCancel)
				if err != nil {
					b.Fatalf("NewOrder: %v", err)
				}
				buf, _, _ = eng.Match(o, buf[:0])
			}
			snap := eng.TakeSnapshot()
			snap.WALSeq = 0 // no log folded in yet, so the whole tail replays
			snapPath := filepath.Join(dir, "snap.json")
			if err := WriteSnapshot(snapPath, snap); err != nil {
				b.Fatalf("WriteSnapshot: %v", err)
			}
			logPath := buildLog(b, dir, tail)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := Recover(benchCfg(), snapPath, logPath)
				if err != nil {
					b.Fatalf("Recover: %v", err)
				}
				if got.OrderCount() != book+tail {
					b.Fatalf("recovered %d orders, want %d", got.OrderCount(), book+tail)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(tail), "tail")
		})
	}
}
