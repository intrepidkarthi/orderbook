package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// gateSink blocks the matching goroutine inside OnEvents until released, letting a
// test deterministically fill the command queue behind a stuck consumer.
type gateSink struct {
	started chan struct{}
	release chan struct{}
	done    bool
}

func (g *gateSink) OnEvents(_ []Event) {
	if !g.done {
		g.done = true
		close(g.started)
		<-g.release
	}
}

// TestBackpressure_ShedsNewOrders checks TrySubmit sheds with ErrQueueFull once
// the bounded queue is full, and succeeds when there is capacity.
func TestBackpressure_ShedsNewOrders(t *testing.T) {
	g := &gateSink{started: make(chan struct{}), release: make(chan struct{})}
	r := NewRunner(RunnerConfig{Engine: Config{Symbol: "X", EventSink: g}, QueueSize: 1})

	// First order is pulled by the loop, which then blocks in OnEvents; the queue
	// buffer is now empty and the consumer is stuck.
	if _, err := r.TrySubmitAsync(mkOrder("a", types.SideBuy, 100, 1)); err != nil {
		t.Fatalf("first submit should enqueue: %v", err)
	}
	<-g.started

	// One more fills the size-1 buffer; the next must shed.
	if _, err := r.TrySubmitAsync(mkOrder("b", types.SideBuy, 99, 1)); err != nil {
		t.Fatalf("second submit should fill the buffer, got %v", err)
	}
	if _, err := r.TrySubmit(mkOrder("c", types.SideBuy, 98, 1)); err != ErrQueueFull {
		t.Errorf("third submit should shed, got err=%v", err)
	}
	if r.QueueLen() != r.QueueCap() {
		t.Errorf("queue should be full: len=%d cap=%d", r.QueueLen(), r.QueueCap())
	}

	// Release the consumer; the blocking path waits for space and succeeds — the
	// queue recovers once drained.
	close(g.release)
	if res := r.Process(mkOrder("d", types.SideBuy, 97, 1)); res == nil || res.Status == "" {
		t.Errorf("submit after drain should succeed, got %+v", res)
	}
	r.Close()
}
