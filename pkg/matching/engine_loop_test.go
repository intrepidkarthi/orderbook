package matching

import (
	"sync"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestRunner_MirrorsEngine feeds the same ordered command stream to a bare Engine
// and to a Runner (single producer) and checks the resulting books match — the
// queue changes only *how* commands arrive, not the matching outcome.
func TestRunner_MirrorsEngine(t *testing.T) {
	script := func() []*types.Order {
		return []*types.Order{
			lim(t, "s1", types.SideSell, 101, 5),
			lim(t, "s2", types.SideSell, 102, 5),
			lim(t, "b1", types.SideBuy, 100, 3),
			lim(t, "b2", types.SideBuy, 102, 7),
			ord(t, "b3", types.SideBuy, types.OrderTypeMarket, 0, 2, types.TIFImmediateOrCancel),
		}
	}

	eng := newEngine()
	var direct int
	for _, o := range script() {
		direct += len(eng.Process(o).Trades)
	}

	run := NewRunner(RunnerConfig{Engine: DefaultConfig("BTC-USD")})
	defer run.Close()
	var queued int
	for _, o := range script() {
		queued += len(run.Process(o).Trades)
	}

	if direct != queued {
		t.Errorf("trade count via Runner (%d) != direct Engine (%d)", queued, direct)
	}
	db, _, _ := eng.BestBid()
	rb, _, _ := run.BestBid()
	if db != rb {
		t.Errorf("best bid differs: engine %d vs runner %d", db, rb)
	}
	if eng.OrderCount() != run.OrderCount() {
		t.Errorf("order count differs: engine %d vs runner %d", eng.OrderCount(), run.OrderCount())
	}
}

// TestRunner_ConcurrentProducers hammers the Runner from many goroutines. The
// command order is nondeterministic, but the single writer must keep the book
// consistent (never crossed) and account every order exactly once.
func TestRunner_ConcurrentProducers(t *testing.T) {
	run := NewRunner(RunnerConfig{Engine: DefaultConfig("BTC-USD"), QueueSize: 256})

	const producers, each = 8, 200
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			user := "u" + string(rune('A'+p))
			for i := 0; i < each; i++ {
				side := types.SideBuy
				price := int64(90 + i%10)
				if i%2 == 0 {
					side = types.SideSell
					price = int64(110 - i%10)
				}
				o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, 1, types.TIFGoodTillCancel)
				if err != nil {
					t.Errorf("NewOrder: %v", err)
					return
				}
				run.Process(o)
			}
		}(p)
	}
	wg.Wait()

	// The book must never be crossed after all commands drain.
	if bid, _, okB := run.BestBid(); okB {
		if ask, _, okA := run.BestAsk(); okA && bid >= ask {
			t.Errorf("book crossed under concurrent load: bid %d >= ask %d", bid, ask)
		}
	}
	run.Close()
}

// TestRunner_SubmitAsync checks the non-blocking submit path returns each result.
func TestRunner_SubmitAsync(t *testing.T) {
	run := NewRunner(RunnerConfig{Engine: DefaultConfig("BTC-USD")})
	defer run.Close()

	run.Process(lim(t, "m", types.SideSell, 100, 10)) // resting liquidity

	chans := make([]<-chan *MatchResult, 5)
	for i := range chans {
		chans[i] = run.SubmitAsync(lim(t, "t", types.SideBuy, 100, 1))
	}
	total := 0
	for _, ch := range chans {
		res := <-ch
		total += len(res.Trades)
	}
	if total != 5 {
		t.Errorf("async submits produced %d trades, want 5", total)
	}
}

// TestRunner_HaltResume drives the circuit breaker through the queue.
func TestRunner_HaltResume(t *testing.T) {
	run := NewRunner(RunnerConfig{Engine: DefaultConfig("BTC-USD")})
	defer run.Close()

	run.Halt()
	if r := run.Process(lim(t, "a", types.SideBuy, 100, 1)); r.Status != types.OrderStatusRejected {
		t.Errorf("halted order should be rejected, got %q", r.Status)
	}
	run.Resume()
	if r := run.Process(lim(t, "a", types.SideBuy, 100, 1)); r.Status == types.OrderStatusRejected {
		t.Errorf("after resume, order should be accepted, got %v", r.RejectionReason)
	}
}
