package matching

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// BenchmarkShards_Scaling measures the only axis this engine scales on.
//
// Every other benchmark here drives one book from one goroutine, which is the
// engine's contract and says nothing about a machine with more than one core. A
// book cannot be parallelised — it is a single writer by design (MULTI-SYMBOL §2)
// — so the question "does this use the machine" is entirely the question "what
// does aggregate throughput do as books are added". Nobody had measured it, and a
// scaling claim nobody measured is a scaling claim.
//
// b.N operations are spread across `books` shards, one producer goroutine per
// shard, so ns/op is aggregate wall time per operation and the ratio between rows
// is the speedup. Order values are allocated inside the timed loop — unlike the
// single-book benchmarks, which hoist that out — because a producer that must
// build its own orders is what a shard actually faces. The cost is identical for
// every row, so it dampens the ratio rather than flattering it.
func BenchmarkShards_Scaling(b *testing.B) {
	for _, books := range []int{1, 2, 4, 6, 8} {
		b.Run(fmt.Sprintf("books=%d", books), func(b *testing.B) {
			dir := b.TempDir()
			s := NewShards(ShardsConfig{
				NewConfig: func(sym string) Config {
					return Config{Symbol: sym, MaxOrders: 100_000}
				},
				QueueSize: 8192,
				Manifest:  NewManifest(filepath.Join(dir, "venue.json")),
			})
			defer s.Close()

			runners := make([]*Runner, books)
			syms := make([]string, books)
			for i := range runners {
				syms[i] = fmt.Sprintf("SYM%02d", i)
				if runners[i] = s.Runner(syms[i]); runners[i] == nil {
					b.Fatalf("shard %s: %v", syms[i], s.Err())
				}
			}

			// Exact distribution: the rows must not differ by a rounding remainder.
			share := make([]int, books)
			for i := range share {
				share[i] = b.N / books
				if i < b.N%books {
					share[i]++
				}
			}

			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range runners {
				wg.Add(1)
				go func(r *Runner, sym string, n int) {
					defer wg.Done()
					<-start
					driveShard(r, sym, n)
				}(runners[i], syms[i], share[i])
			}

			b.ResetTimer()
			close(start)
			wg.Wait()
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/s")
		})
	}
}

// driveShard runs a 70/20/10 rest / cancel / marketable mix against one runner,
// holding at most shardResting orders on the book so throughput measures the
// engine rather than a book that grows for the length of the run.
const shardResting = 2_000

func driveShard(r *Runner, sym string, n int) {
	live := make([]int64, 0, shardResting)
	for i := 0; i < n; i++ {
		switch i % 10 {
		case 0: // marketable sell, crosses the resting bids
			r.Process(mkShardOrder(sym, types.SideSell, 900, 1))
		case 1, 2: // cancel the oldest resting order
			if len(live) > 0 {
				r.Cancel(live[0], "u")
				live = live[1:]
				continue
			}
			fallthrough
		default: // rest a bid below the ask floor
			res := r.Process(mkShardOrder(sym, types.SideBuy, int64(900+i%100), 1))
			if res == nil || res.Order == nil || res.Order.ID == 0 {
				continue
			}
			if len(live) >= shardResting {
				r.Cancel(live[0], "u")
				live = live[1:]
			}
			live = append(live, res.Order.ID)
		}
	}
}

func mkShardOrder(sym string, side types.Side, price, qty int64) *types.Order {
	o, err := types.NewOrder("u", sym, side, types.OrderTypeLimit, price, qty,
		types.TIFGoodTillCancel)
	if err != nil {
		panic(err)
	}
	return o
}
