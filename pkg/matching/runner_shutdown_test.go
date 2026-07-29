package matching

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func shutOrder(t *testing.T, i int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder("u1", "X", types.SideBuy, types.OrderTypeLimit, 100+i%5, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestRunnerCloseUnderConcurrentProducers is the regression. Close used to close
// the shared command queue from the consumer side, so any producer mid-send
// panicked with "send on closed channel". Correct shutdown therefore required
// proving every producer had already stopped — which a server with a goroutine
// per connection cannot do.
func TestRunnerCloseUnderConcurrentProducers(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 8})

	const producers = 64
	var wg, ready sync.WaitGroup
	var refused, applied int64
	start := make(chan struct{})

	ready.Add(producers)
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			<-start
			for i := 0; i < 50; i++ {
				res := r.Process(shutOrder(t, int64(p*50+i)))
				if res == nil {
					t.Errorf("nil result from Process")
					return
				}
				if errors.Is(res.RejectionReason, ErrShuttingDown) {
					atomic.AddInt64(&refused, 1)
				} else {
					atomic.AddInt64(&applied, 1)
				}
				if i == 0 {
					ready.Done() // every producer has landed one submit
				}
			}
		}(p)
	}

	close(start)
	// Close must land while producers are mid-flight, not before they start, or
	// the test proves nothing about the race it exists to cover.
	ready.Wait()
	r.Close()
	wg.Wait()

	if applied+refused != producers*50 {
		t.Errorf("accounted for %d submissions, want %d", applied+refused, producers*50)
	}
	if applied == 0 {
		t.Error("no submission was applied; the test closed before doing any work")
	}
}

// TestRunnerCloseIsIdempotent — a server may close from a signal handler and from
// a deferred cleanup.
func TestRunnerCloseIsIdempotent(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	r.Close()
	r.Close()
	r.Close()
}

// TestRunnerSubmitAfterCloseIsRefused pins the post-shutdown contract across
// every submit path: a refusal, never a panic and never a hang.
func TestRunnerSubmitAfterCloseIsRefused(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	r.Close()

	if res := r.Process(shutOrder(t, 1)); res == nil || !errors.Is(res.RejectionReason, ErrShuttingDown) {
		t.Errorf("Process after Close = %+v, want a rejection carrying ErrShuttingDown", res)
	}
	if _, err := r.Cancel(1, "u1"); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("Cancel after Close = %v, want ErrShuttingDown", err)
	}
	if _, err := r.TrySubmit(shutOrder(t, 2)); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("TrySubmit after Close = %v, want ErrShuttingDown", err)
	}
	if _, err := r.TrySubmitAsync(shutOrder(t, 3)); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("TrySubmitAsync after Close = %v, want ErrShuttingDown", err)
	}
	res := <-r.SubmitAsync(shutOrder(t, 4))
	if res == nil || !errors.Is(res.RejectionReason, ErrShuttingDown) {
		t.Errorf("SubmitAsync after Close = %+v, want a rejection carrying ErrShuttingDown", res)
	}
}

// TestRunnerCloseDrainsAcceptedCommands: a command that made it into the queue
// before the fence closed must still be applied, so its producer gets a real
// result rather than a refusal for work the engine actually did.
func TestRunnerCloseDrainsAcceptedCommands(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 256})

	const n = 100
	results := make([]<-chan *MatchResult, n)
	for i := 0; i < n; i++ {
		results[i] = r.SubmitAsync(shutOrder(t, int64(i)))
	}
	r.Close()

	var applied int
	for _, ch := range results {
		res := <-ch
		if res == nil {
			t.Fatal("nil result")
		}
		if !errors.Is(res.RejectionReason, ErrShuttingDown) {
			applied++
		}
	}
	if applied != n {
		t.Errorf("%d of %d accepted commands were applied; the rest were dropped on shutdown", applied, n)
	}
	if got := r.OrderCount(); got != applied {
		t.Errorf("book holds %d orders but %d commands were applied", got, applied)
	}
}
