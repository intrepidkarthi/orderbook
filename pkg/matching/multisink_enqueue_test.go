package matching

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

type countSink struct {
	events int
	kinds  map[EventKind]int
}

func newCountSink() *countSink { return &countSink{kinds: map[EventKind]int{}} }

func (c *countSink) OnEvents(evs []Event) {
	c.events += len(evs)
	for _, e := range evs {
		c.kinds[e.Kind]++
	}
}

func meOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestMultiSinkFansOut is the point of the type: an embedder's existing sink must
// keep working when a publisher is attached alongside it.
func TestMultiSinkFansOut(t *testing.T) {
	a, b := newCountSink(), newCountSink()
	cfg := DefaultConfig("X")
	cfg.EventSink = MultiSink{a, nil, b} // nil entries are skipped, not panicked on
	e := NewEngine(cfg)

	e.Process(meOrder(t, "u", types.SideBuy, 100, 5))
	e.Process(meOrder(t, "v", types.SideSell, 100, 5))

	if a.events == 0 {
		t.Fatal("first sink received nothing")
	}
	if a.events != b.events {
		t.Errorf("sinks disagree: %d vs %d events", a.events, b.events)
	}
	if a.kinds[EventTrade] == 0 {
		t.Error("no trade reached the fanned-out sinks")
	}
}

// TestTryEnqueueAppliesWithoutReply pins that fire-and-forget actually reaches the
// engine — dispatch has always tolerated a nil reply, and this is the path a
// network ingress uses.
func TestTryEnqueueAppliesWithoutReply(t *testing.T) {
	sink := newCountSink()
	cfg := DefaultConfig("X")
	cfg.EventSink = sink
	r := NewRunner(RunnerConfig{Engine: cfg, QueueSize: 64})

	const n = 20
	for i := 0; i < n; i++ {
		if err := r.TryEnqueue(meOrder(t, "u", types.SideBuy, int64(100-i), 1)); err != nil {
			t.Fatalf("TryEnqueue: %v", err)
		}
	}
	r.Close() // drains everything already accepted

	if got := r.OrderCount(); got != n {
		t.Errorf("book holds %d orders, want %d", got, n)
	}
	if sink.kinds[EventAccepted] != n {
		t.Errorf("%d Accepted events, want %d — the outcome must arrive on the stream", sink.kinds[EventAccepted], n)
	}
}

// TestTryEnqueueCancel covers the cancel counterpart end to end.
func TestTryEnqueueCancel(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 64})

	o := meOrder(t, "u", types.SideBuy, 100, 5)
	res := r.Process(o) // synchronous here purely to learn the assigned id
	if res == nil || res.Order == nil {
		t.Fatal("submit failed")
	}
	if err := r.TryEnqueueCancel(res.Order.ID, "u"); err != nil {
		t.Fatalf("TryEnqueueCancel: %v", err)
	}
	r.Close()

	if got := r.OrderCount(); got != 0 {
		t.Errorf("book holds %d orders after cancel, want 0", got)
	}
}

// TestTryEnqueueShedsWhenFull pins the backpressure contract: a full queue is a
// refusal the caller can act on, not a block. An ingress needs to shed.
func TestTryEnqueueShedsWhenFull(t *testing.T) {
	// Queue size 1 with a busy matcher: some submissions must be refused.
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 1})
	defer r.Close()

	var full int
	for i := 0; i < 2000; i++ {
		if err := r.TryEnqueue(meOrder(t, "u", types.SideBuy, 100, 1)); errors.Is(err, ErrQueueFull) {
			full++
		}
	}
	if full == 0 {
		t.Skip("matcher kept up with a 1-deep queue; cannot exercise shedding on this machine")
	}
}

// TestTryEnqueueAfterCloseIsRefused — same post-shutdown contract as every other
// submit path: refuse, never panic.
func TestTryEnqueueAfterCloseIsRefused(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	r.Close()

	if err := r.TryEnqueue(meOrder(t, "u", types.SideBuy, 100, 1)); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("TryEnqueue after Close = %v, want ErrShuttingDown", err)
	}
	if err := r.TryEnqueueCancel(1, "u"); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("TryEnqueueCancel after Close = %v, want ErrShuttingDown", err)
	}
}
