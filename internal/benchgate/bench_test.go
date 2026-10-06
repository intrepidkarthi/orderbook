package benchgate

import (
	"os"
	"runtime"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/tape"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// benchTapePath is the tape the benchmark replays. BENCHGATE_TAPE overrides it, so
// a comparison can make base and head replay the SAME file even when head has cut a
// new one (docs/BENCH-GATE.md §5.2, step 3).
func benchTapePath() string {
	if p := os.Getenv("BENCHGATE_TAPE"); p != "" {
		return p
	}
	return benchTapeFile
}

// replayPlan is a tape prepared for timing: every order built once, so the timed
// loop copies values instead of constructing them.
type replayPlan struct {
	cmds      []tape.Cmd
	templates []types.Order // one per Submit or Replace, in tape order
	slot      []int         // tape position -> index into templates, -1 if none
	capacity  int
	// what a correctness replay of the same tape ends with: the guard
	resting int
	trades  int
}

func planFor(b *testing.B) *replayPlan {
	b.Helper()
	src, err := os.ReadFile(benchTapePath())
	if err != nil {
		b.Fatal(err)
	}
	tp, err := Parse(src)
	if err != nil {
		b.Fatal(err)
	}
	p := &replayPlan{cmds: tp.Cmds, slot: make([]int, len(tp.Cmds)), capacity: int(tp.MaxOrders)}
	for i, c := range tp.Cmds {
		p.slot[i] = -1
		if c.Kind == tape.Submit || c.Kind == tape.Replace {
			o, err := engineOrder(c)
			if err != nil {
				b.Fatal(err)
			}
			p.slot[i] = len(p.templates)
			p.templates = append(p.templates, *o)
		}
	}
	// The guard's expectation comes from the correctness driver, not from the timed
	// loop, so a timed loop that stopped doing the work cannot agree with itself.
	d, err := DigestEngine(tp)
	if err != nil {
		b.Fatal(err)
	}
	p.resting, p.trades = d.Resting, int(d.CoreTrades)
	return p
}

// BenchmarkTapeReplay times one full replay of the bench tape per op. Because the op
// is the whole replay, allocs/op is an exact total for 50,000 commands: one extra
// allocation anywhere on the path shows, with none of the integer-division rounding
// a per-command benchmark has.
//
// Inside the timed region: the engine calls and nothing else. Outside it, under
// StopTimer: a fresh engine, a fresh copy of every order, and a GC, so the previous
// replay's garbage is not collected during the next one. Never in the loop: the
// generator, fmt, hashing, maps, or per-command clock reads.
func BenchmarkTapeReplay(b *testing.B) {
	p := planFor(b)
	b.Run("sink=nil", func(b *testing.B) {
		ids := make([]int64, len(p.cmds))
		buf := make([]types.Trade, 0, 64)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			cfg := matching.DefaultConfig(benchSymbol)
			cfg.Clock = counterClock()
			cfg.MaxOrders = p.capacity
			e := matching.NewEngine(cfg)
			orders := make([]types.Order, len(p.templates))
			copy(orders, p.templates)
			clear(ids)
			trades := 0
			runtime.GC()
			b.StartTimer()

			for pos, c := range p.cmds {
				switch c.Kind {
				case tape.Submit:
					o := &orders[p.slot[pos]]
					buf, _, _ = e.Match(o, buf[:0])
					trades += len(buf)
					ids[pos] = o.ID
				case tape.Cancel:
					_, _ = e.Cancel(ids[c.Target], c.User)
				case tape.Reduce:
					_, _ = e.Reduce(ids[c.Target], c.NewQty, c.User)
				case tape.Replace:
					if res, err := e.Replace(ids[c.Target], c.User, &orders[p.slot[pos]]); err == nil {
						ids[pos] = res.Order.ID
						trades += len(res.Trades)
					}
				}
			}

			b.StopTimer()
			if got := e.OrderCount(); got != p.resting || trades != p.trades {
				b.Fatalf("the timed replay ended with %d resting and %d trades; the correctness replay "+
					"of the same tape ends with %d and %d, so the loop is not doing the work it times",
					got, trades, p.resting, p.trades)
			}
			b.StartTimer()
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(p.cmds)), "ns/cmd")
	})
}
