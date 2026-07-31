package orderentry

import (
	"strconv"
	"sync"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func nameOrder(t *testing.T, user, clOrdID string, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", types.SideBuy, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	o.ClientOrderID = clOrdID
	return o
}

// TestAnOrderIsNameableWithoutThePump is the regression test for the defect a soak
// found and 480 other tests did not.
//
// The pump is deliberately never started. That is not an artificial condition, it is
// the condition made deterministic: a pump that has not run yet and a pump that is
// four hundred milliseconds behind are the same thing to a client whose cancel is
// being refused. Before the naming index existed, this test could not pass at all —
// nothing but the pump wrote the map a cancel resolves through.
func TestAnOrderIsNameableWithoutThePump(t *testing.T) {
	reg := NewRegistry("INC1", 4096)
	pub := NewPublisher(reg, 1<<15)
	defer pub.Close()
	// No go pub.Pump().

	eng := matching.DefaultConfig("X")
	eng.EventSink = matching.MultiSink{NewNameIndex(reg), pub}
	e := matching.NewEngine(eng)

	o := nameOrder(t, "alice", "cl-1", 100, 5)
	e.Process(o)

	id, ok := reg.OrderIDFor("alice", "cl-1")
	if !ok {
		t.Fatal("a live resting order could not be named by its client id; a cancel for it would be refused and the order would rest in the book forever")
	}
	if id != o.ID {
		t.Errorf("resolved to order %d, want %d", id, o.ID)
	}
}

// TestNamingIsScopedToTheAccount — the security boundary. Without it a client could
// name another's order by guessing an identifier as ordinary as "1".
func TestNamingIsScopedToTheAccount(t *testing.T) {
	reg := NewRegistry("INC1", 4096)
	idx := NewNameIndex(reg)
	alice := nameOrder(t, "alice", "1", 100, 5)
	alice.ID = 7
	idx.OnEvents([]matching.Event{{Kind: matching.EventAccepted, Order: alice}})

	if _, ok := reg.OrderIDFor("bob", "1"); ok {
		t.Error("bob resolved alice's order")
	}
	if id, ok := reg.OrderIDFor("alice", "1"); !ok || id != 7 {
		t.Errorf("alice resolved %d, %v; want 7, true", id, ok)
	}
}

// TestALateForgetDoesNotUnnameAReusedIdentifier — the hazard the split introduces and
// the reason forget compares before it deletes.
//
// Naming is now synchronous and forgetting is not, so a client can cancel an order
// and enter a new one under the same identifier before the pump has processed the
// first one's cancellation. An unconditional delete would then unname the live order
// on the strength of the dead one, which is the same orphaning bug wearing a
// different hat.
func TestALateForgetDoesNotUnnameAReusedIdentifier(t *testing.T) {
	reg := NewRegistry("INC1", 4096)
	idx := NewNameIndex(reg)

	first := nameOrder(t, "alice", "reuse", 100, 5)
	first.ID = 1
	idx.OnEvents([]matching.Event{{Kind: matching.EventAccepted, Order: first}})
	reg.track(first)

	// The client reuses the identifier; the matcher accepts the new order.
	second := nameOrder(t, "alice", "reuse", 101, 5)
	second.ID = 2
	idx.OnEvents([]matching.Event{{Kind: matching.EventAccepted, Order: second}})
	reg.track(second)

	// Only now does the pump get round to the first order's cancellation.
	reg.forget(1)

	id, ok := reg.OrderIDFor("alice", "reuse")
	if !ok {
		t.Fatal("the live order lost its name when a dead one was forgotten")
	}
	if id != 2 {
		t.Errorf("resolved to order %d, want the live one (2)", id)
	}
}

// TestForgettingTheCurrentOrderStillRemovesTheName — the other half: the compare must
// not turn forget into a no-op, or names accumulate for the life of the venue and the
// map becomes a leak of exactly the kind a soak is run to find.
func TestForgettingTheCurrentOrderStillRemovesTheName(t *testing.T) {
	reg := NewRegistry("INC1", 4096)
	o := nameOrder(t, "alice", "gone", 100, 5)
	o.ID = 3
	NewNameIndex(reg).OnEvents([]matching.Event{{Kind: matching.EventAccepted, Order: o}})
	reg.track(o)

	reg.forget(3)
	if _, ok := reg.OrderIDFor("alice", "gone"); ok {
		t.Error("the name outlived the order")
	}
}

// TestNamingIgnoresEverythingButAcceptance — the sink runs on the matching goroutine,
// so anything it does that is not strictly necessary is a cost the whole venue pays.
func TestNamingIgnoresEverythingButAcceptance(t *testing.T) {
	reg := NewRegistry("INC1", 4096)
	o := nameOrder(t, "alice", "cl", 100, 5)
	o.ID = 9
	NewNameIndex(reg).OnEvents([]matching.Event{
		{Kind: matching.EventCanceled, Order: o},
		{Kind: matching.EventRejected, Order: o},
		{Kind: matching.EventTrade},
		{Kind: matching.EventAccepted, Order: nil},
	})
	if _, ok := reg.OrderIDFor("alice", "cl"); ok {
		t.Error("a name was recorded for an order that was never accepted")
	}
}

// TestAnExhaustedOrderLosesItsName — fill() removes the last of an order, so it must
// drop the name too or the map grows for the life of the venue.
//
// It is a separate test from the cancel path because it is a separate code path, and
// the first version of the lock split fixed one and left the other writing the naming
// map under the wrong mutex. Nothing here caught it; a soak crashed the process on
// "concurrent map writes" within thirty seconds.
func TestAnExhaustedOrderLosesItsName(t *testing.T) {
	reg := NewRegistry("INC1", 4096)
	o := nameOrder(t, "alice", "eaten", 100, 5)
	o.ID = 11
	NewNameIndex(reg).OnEvents([]matching.Event{{Kind: matching.EventAccepted, Order: o}})
	reg.track(o)

	if _, _, leaves, ok := reg.fill(11, 5); !ok || leaves != 0 {
		t.Fatalf("fill = leaves %d, ok %v; want 0, true", leaves, ok)
	}
	if _, ok := reg.OrderIDFor("alice", "eaten"); ok {
		t.Error("the name outlived the order it named")
	}
}

// TestAFillDoesNotUnnameAReusedIdentifier — the same hazard as the cancel path. A
// partial fill leaves the order live and must leave the name alone; an exhausting
// fill must only drop a name that still points at the order it exhausted.
func TestAFillDoesNotUnnameAReusedIdentifier(t *testing.T) {
	reg := NewRegistry("INC1", 4096)
	idx := NewNameIndex(reg)

	first := nameOrder(t, "alice", "reuse", 100, 5)
	first.ID = 1
	idx.OnEvents([]matching.Event{{Kind: matching.EventAccepted, Order: first}})
	reg.track(first)

	second := nameOrder(t, "alice", "reuse", 101, 5)
	second.ID = 2
	idx.OnEvents([]matching.Event{{Kind: matching.EventAccepted, Order: second}})
	reg.track(second)

	// The pump now processes a trade that exhausted the FIRST order.
	reg.fill(1, 5)

	id, ok := reg.OrderIDFor("alice", "reuse")
	if !ok || id != 2 {
		t.Errorf("resolved %d, %v; want the live order (2), true", id, ok)
	}
}

// TestNamingAndFillingConcurrentlyIsSafe — the matcher names while the pump fills, by
// construction, and the two touch the same map. This is the shape of the crash.
func TestNamingAndFillingConcurrentlyIsSafe(t *testing.T) {
	reg := NewRegistry("INC1", 4096)
	idx := NewNameIndex(reg)

	const n = 2000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // the matching goroutine
		defer wg.Done()
		for i := 1; i <= n; i++ {
			o := nameOrder(t, "alice", "c"+strconv.Itoa(i), 100, 5)
			o.ID = int64(i)
			idx.OnEvents([]matching.Event{{Kind: matching.EventAccepted, Order: o}})
		}
	}()
	go func() { // the pump
		defer wg.Done()
		for i := 1; i <= n; i++ {
			o := nameOrder(t, "alice", "c"+strconv.Itoa(i), 100, 5)
			o.ID = int64(i)
			reg.track(o)
			reg.fill(int64(i), 5)
			reg.forget(int64(i))
		}
	}()
	wg.Wait()
}
