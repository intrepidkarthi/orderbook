package wal

import (
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// countingSink is the cheapest possible real sink: it does what the contract
// demands (return immediately) so the measurement is the engine's emission cost,
// not a consumer's.
type countingSink struct{ n int }

func (s *countingSink) OnEvents(evs []matching.Event) { s.n += len(evs) }

func benchOrder(i int) *types.Order {
	o, _ := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, int64(1000+i%2000), 1, types.TIFGoodTillCancel)
	return o
}

// BenchmarkRunnerBare is the baseline: the concurrency front with no durability
// and no event stream. Published microbenchmarks measured the engine directly, so
// nothing previously put a number on the API the docs tell people to embed.
func BenchmarkRunnerBare(b *testing.B) {
	r := matching.NewRunner(matching.RunnerConfig{Engine: matching.DefaultConfig("X"), QueueSize: 8192})
	defer r.Close()
	orders := make([]*types.Order, b.N)
	for i := range orders {
		orders[i] = benchOrder(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Process(orders[i])
	}
}

// BenchmarkRunnerWithSink adds the event stream, isolating emission cost.
func BenchmarkRunnerWithSink(b *testing.B) {
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = &countingSink{}
	r := matching.NewRunner(matching.RunnerConfig{Engine: cfg, QueueSize: 8192})
	defer r.Close()
	orders := make([]*types.Order, b.N)
	for i := range orders {
		orders[i] = benchOrder(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Process(orders[i])
	}
}

// BenchmarkRunnerDurableGroupCommit is the realistic embedding: write-ahead log
// plus event stream, with the fsync amortised across a batch the way a venue
// actually runs it. Syncing per command would measure the disk, not the engine.
func BenchmarkRunnerDurableGroupCommit(b *testing.B) {
	dir := b.TempDir()
	w, err := Open(filepath.Join(dir, "wal.log"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer w.Close()

	cfg := matching.DefaultConfig("X")
	cfg.EventSink = &countingSink{}
	r := matching.NewRunner(matching.RunnerConfig{Engine: cfg, QueueSize: 8192, Log: w})
	defer r.Close()

	orders := make([]*types.Order, b.N)
	for i := range orders {
		orders[i] = benchOrder(i)
	}
	const groupSize = 256

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Process(orders[i])
		if i%groupSize == groupSize-1 {
			_ = w.Sync()
		}
	}
}

// BenchmarkRunnerDurableSyncEvery is the pessimistic bound: fsync per command.
// Worth publishing alongside the group-commit number so the cost of the
// durability policy is visible rather than assumed.
func BenchmarkRunnerDurableSyncEvery(b *testing.B) {
	dir := b.TempDir()
	w, err := Open(filepath.Join(dir, "wal.log"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer w.Close()

	cfg := matching.DefaultConfig("X")
	cfg.EventSink = &countingSink{}
	r := matching.NewRunner(matching.RunnerConfig{Engine: cfg, QueueSize: 8192, Log: w})
	defer r.Close()

	orders := make([]*types.Order, b.N)
	for i := range orders {
		orders[i] = benchOrder(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Process(orders[i])
		_ = w.Sync()
	}
}

// BenchmarkCheckpoint measures taking a consistent snapshot on the matching
// goroutine against a warm book — the operation that bounds recovery time.
func BenchmarkCheckpoint(b *testing.B) {
	r := matching.NewRunner(matching.RunnerConfig{Engine: matching.DefaultConfig("X"), QueueSize: 8192})
	defer r.Close()
	for i := 0; i < 5000; i++ {
		r.Process(benchOrder(i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Checkpoint(); err != nil {
			b.Fatal(err)
		}
	}
}
