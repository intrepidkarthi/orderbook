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
	events  int
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
	p.resting, p.trades, p.events = d.Resting, int(d.CoreTrades), d.EngineEvents
	return p
}

// countSink is the cheapest consumer an embedder can attach: it counts what the
// engine publishes and keeps nothing.
type countSink struct{ n int }

func (c *countSink) OnEvents(evs []matching.Event) { c.n += len(evs) }

// BenchmarkTapeReplay times one full replay of the bench tape per op. Because the op
// is the whole replay, allocs/op is a total over 50,000 commands rather than a
// per-command figure rounded by integer division.
//
// sink=nil is the bare engine with emission off. sink=count attaches a counting sink,
// so the engine builds and publishes its event stream: the configuration the
// documentation recommends embedding, and the one sink=nil cannot see a regression in.
//
// Inside the timed region: the engine calls and nothing else. Outside it, under
// StopTimer: a fresh engine, a fresh copy of every order, and a GC, so the previous
// replay's garbage is not collected during the next one. Never in the loop: the
// generator, fmt, hashing, maps, or per-command clock reads.
func BenchmarkTapeReplay(b *testing.B) {
	p := planFor(b)
	b.Run("sink=nil", func(b *testing.B) { replayLoop(b, p, false) })
	b.Run("sink=count", func(b *testing.B) { replayLoop(b, p, true) })
}

func replayLoop(b *testing.B, p *replayPlan, withSink bool) {
	ids := make([]int64, len(p.cmds))
	buf := make([]types.Trade, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		cfg := matching.DefaultConfig(benchSymbol)
		cfg.Clock = counterClock()
		cfg.MaxOrders = p.capacity
		var sink *countSink
		if withSink {
			sink = &countSink{}
			cfg.EventSink = sink
		}
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
		if withSink && sink.n != p.events {
			b.Fatalf("the sink counted %d events; the correctness replay published %d, so the timed "+
				"replay is not publishing what it should", sink.n, p.events)
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(p.cmds)), "ns/cmd")
}

// TestPrintDigest prints the full-chain digest this tree's engine produces on the
// tape BENCHGATE_TAPE names. cmd/benchgate asks both arms of a comparison for it on
// base's tape: if they differ, matching behaviour changed and timing the replay
// would compare two different computations.
func TestPrintDigest(t *testing.T) {
	if os.Getenv("BENCHGATE_PRINT_DIGEST") != "1" {
		t.Skip("set BENCHGATE_PRINT_DIGEST=1")
	}
	src, err := os.ReadFile(benchTapePath())
	if err != nil {
		t.Fatal(err)
	}
	tp, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	d, err := DigestEngine(tp)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("OBDG full %x", d.Full)
}
