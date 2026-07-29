package gateway

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func gwOrder(t *testing.T, user string, i int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", types.SideBuy, types.OrderTypeLimit, 100+i%7, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestRateGateConcurrent is the regression for the data race. RateGate's bucket
// map was unsynchronised while the package doc recommended one gateway in front
// of a goroutine per connection — the exact topology that races it. Run under
// -race this failed reliably before the mutex.
func TestRateGateConcurrent(t *testing.T) {
	g := NewRateGate(1e6, 1e6) // effectively unlimited: exercise the map, not the policy

	const goroutines, iterations = 32, 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	now := time.Now()

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				// Mix of shared and distinct accounts: shared keys contend on one
				// bucket, distinct keys grow the map concurrently.
				g.Allow("shared", now)
				g.Allow(string(rune('a'+i%26)), now)
			}
		}(i)
	}
	close(start)
	wg.Wait()
}

// TestGatewaySubmitConcurrent drives the whole edge the way a server would: many
// connection goroutines through one Gateway into one Runner.
func TestGatewaySubmitConcurrent(t *testing.T) {
	r := matching.NewRunner(matching.RunnerConfig{Engine: matching.DefaultConfig("X"), QueueSize: 512})
	defer r.Close()

	gw := New(r, Config{Rate: 1e6, Burst: 1e6})

	const goroutines, iterations = 24, 100
	var wg sync.WaitGroup
	var accepted, throttled int64
	start := make(chan struct{})
	now := time.Now()

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				_, err := gw.Submit(gwOrder(t, string(rune('a'+i%8)), int64(j)), now)
				switch err {
				case nil:
					atomic.AddInt64(&accepted, 1)
				case ErrThrottled:
					atomic.AddInt64(&throttled, 1)
				default:
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if accepted+throttled != goroutines*iterations {
		t.Errorf("accounted for %d submissions, want %d", accepted+throttled, goroutines*iterations)
	}
	if accepted == 0 {
		t.Error("every submission was throttled; the gate is misconfigured for this test")
	}
}

// TestRateGateStillThrottles guards against the mutex quietly changing policy:
// the limit must still bite, and per account rather than globally.
func TestRateGateStillThrottles(t *testing.T) {
	g := NewRateGate(1, 1) // one token, refilling at 1/sec
	now := time.Now()

	if !g.Allow("u1", now) {
		t.Fatal("first order for u1 should pass")
	}
	if g.Allow("u1", now) {
		t.Error("second immediate order for u1 should be throttled")
	}
	if !g.Allow("u2", now) {
		t.Error("u2 has its own bucket and should pass")
	}
	if !g.Allow("u1", now.Add(2*time.Second)) {
		t.Error("u1 should have refilled after two seconds")
	}
}
