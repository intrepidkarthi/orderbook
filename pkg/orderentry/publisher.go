package orderentry

import (
	"sync"
	"sync/atomic"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

// Publisher is the EventSink that carries engine events out to per-account
// streams without ever blocking the matching goroutine.
//
// The EventSink contract is strict for a reason: OnEvents runs ON the matching
// goroutine, so anything it waits for stops the venue. It therefore does exactly
// two things — copy the batch by value into a bounded queue, and return.
//
// The copy is not an optimisation, it is required. matching.Event holds pointers
// into engine-owned state that the matcher keeps mutating after OnEvents returns,
// so a publisher that retained the slice or its pointers would read whatever the
// engine happened to do next.
type Publisher struct {
	mu       sync.Mutex
	cond     *sync.Cond
	pending  []matching.Event
	closed   bool
	dropped  atomic.Uint64
	maxDepth int
	reg      *Registry
	done     chan struct{}
	busy     bool       // the pump is mid-batch
	drained  *sync.Cond // signalled when the queue empties
}

// NewPublisher builds a publisher feeding reg. maxDepth bounds the queue between
// the matcher and the pump; when it is exceeded the OLDEST batches are dropped
// and counted.
//
// Dropping is the deliberate choice. The alternatives are worse: blocking stops
// the matching goroutine, which converts one slow consumer into a venue-wide
// outage, and growing without limit converts it into an out-of-memory kill. A
// dropped batch is visible through Dropped, and a client that missed messages
// discovers it through the sequence gap rather than being told it is up to date.
func NewPublisher(reg *Registry, maxDepth int) *Publisher {
	if maxDepth <= 0 {
		maxDepth = 1 << 14
	}
	p := &Publisher{maxDepth: maxDepth, reg: reg, done: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	p.drained = sync.NewCond(&p.mu)
	return p
}

// OnEvents implements matching.EventSink. It runs on the matching goroutine and
// must stay allocation-light and wait-free.
func (p *Publisher) OnEvents(evs []matching.Event) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	for i := range evs {
		p.pending = append(p.pending, copyEvent(&evs[i]))
	}
	if len(p.pending) > p.maxDepth {
		drop := len(p.pending) - p.maxDepth
		p.pending = append(p.pending[:0], p.pending[drop:]...)
		p.dropped.Add(uint64(drop))
	}
	p.mu.Unlock()
	p.cond.Signal()
}

// copyEvent deep-copies the parts of an event that outlive the callback. Order
// and Trade are engine-owned and mutate immediately afterwards.
func copyEvent(e *matching.Event) matching.Event {
	c := *e
	if e.Order != nil {
		o := *e.Order
		c.Order = &o
	}
	if e.Trade != nil {
		t := *e.Trade
		c.Trade = &t
	}
	return c
}

// Dropped reports how many events were discarded because the pump fell behind.
// Export it: a non-zero value means clients have gaps.
func (p *Publisher) Dropped() uint64 { return p.dropped.Load() }

// Pump runs the fan-out loop. Run it in its own goroutine; it returns when Close
// is called and the queue has drained.
//
// All encoding and fan-out happens here rather than in OnEvents, which is the
// whole point of the split: the matcher's cost is one value-copy per event, and
// everything expensive is somebody else's goroutine.
func (p *Publisher) Pump() {
	defer close(p.done)
	for {
		p.mu.Lock()
		for len(p.pending) == 0 && !p.closed {
			p.cond.Wait()
		}
		if len(p.pending) == 0 && p.closed {
			p.mu.Unlock()
			return
		}
		batch := p.pending
		p.pending = nil
		p.busy = true
		p.mu.Unlock()

		p.reg.Publish(batch)

		p.mu.Lock()
		p.busy = false
		if len(p.pending) == 0 {
			p.drained.Broadcast()
		}
		p.mu.Unlock()
	}
}

// Close stops the pump after it has drained, and waits for it.
func (p *Publisher) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.done
		return
	}
	p.closed = true
	p.mu.Unlock()
	p.cond.Broadcast()
	<-p.done
}

// Wait blocks until every event queued so far has been fanned out. It exists for
// tests and orderly shutdown; a server never calls it on the hot path.
func (p *Publisher) Wait() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.pending) > 0 || p.busy {
		p.cond.Signal()
		p.drained.Wait()
	}
}
