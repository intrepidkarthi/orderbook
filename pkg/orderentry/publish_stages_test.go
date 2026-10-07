package orderentry

import (
	"sync"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestPublishWaitIsMeasuredFromTheOldestEvent: two batches reach the publisher d
// apart before its pump runs. The pump takes both at once, and the wait it reports
// must cover the older one; the fan-out is the publish call alone.
func TestPublishWaitIsMeasuredFromTheOldestEvent(t *testing.T) {
	const d = 60 * time.Millisecond
	p := NewPublisher(NewRegistry("INC1", 128), 0)
	var mu sync.Mutex
	var waits, fanouts []time.Duration
	p.ObservePublish(func(w, f time.Duration) {
		mu.Lock()
		waits, fanouts = append(waits, w), append(fanouts, f)
		mu.Unlock()
	})
	ev := func() []matching.Event {
		o, _ := types.NewOrder("u1", "BTC-USD", types.SideBuy, types.OrderTypeLimit, 100, 1, types.TIFGoodTillCancel)
		o.ID = 1
		return []matching.Event{{Kind: matching.EventAccepted, OrderID: 1, UserID: "u1", Order: o}}
	}
	p.OnEvents(ev())
	time.Sleep(d)
	p.OnEvents(ev())
	go p.Pump()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(waits)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.Close()
	p.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 1 {
		t.Fatalf("%d observations for one batch", len(waits))
	}
	if waits[0] < d*3/4 {
		t.Fatalf("the batch's oldest event waited %v, reported %v", d, waits[0])
	}
	if fanouts[0] < 0 || fanouts[0] >= d/2 {
		t.Fatalf("fan-out %v for a two-event batch", fanouts[0])
	}
}

// TestPublishObserverIsOptional: without one, publishing reads no clock and the
// queue's start is never stamped.
func TestPublishObserverIsOptional(t *testing.T) {
	p := NewPublisher(NewRegistry("INC1", 128), 0)
	p.OnEvents([]matching.Event{{Kind: matching.EventCanceled, OrderID: 1, UserID: "u1"}})
	if !p.since.IsZero() {
		t.Fatal("the queue's start was stamped with no observer set")
	}
	p.Close()
}
