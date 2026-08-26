package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// stpSink records the fields of a Canceled that a consumer would reconcile against
// its own copy of the order. It copies at callback time because the engine reuses
// the order pointer afterwards.
type stpSink struct {
	cancels []struct {
		orderID   int64
		remaining int64
		quantity  int64
		status    types.OrderStatus
	}
}

func (s *stpSink) OnEvents(evs []Event) {
	for _, e := range evs {
		if e.Kind != EventCanceled || e.Order == nil {
			continue
		}
		s.cancels = append(s.cancels, struct {
			orderID   int64
			remaining int64
			quantity  int64
			status    types.OrderStatus
		}{e.Order.ID, e.Order.RemainingQty, e.Order.Quantity, e.Order.Status})
	}
}

// TestSTPDecrementToZeroCancelsTheMakerItRemoves: DECREMENT that takes a maker to
// zero publishes a Canceled for it, so the order must READ cancelled. It used to
// remove the order and emit the event without touching Status, leaving any consumer
// that reconciles the two — an invariant checker, a client SDK — holding a
// PartiallyFilled order the venue had already dropped.
func TestSTPDecrementToZeroCancelsTheMakerItRemoves(t *testing.T) {
	sink := &stpSink{}
	e := NewEngine(Config{Symbol: "X", SelfTradePrevention: STPDecrement, EventSink: sink})
	mk := e.Process(lim(t, "same", types.SideSell, 100, 3))

	e.Process(lim(t, "same", types.SideBuy, 100, 5)) // overlap 3 → maker to zero

	if mk.Order.Status != types.OrderStatusCancelled {
		t.Errorf("maker status = %q, want CANCELLED", mk.Order.Status)
	}
	if len(sink.cancels) != 1 {
		t.Fatalf("got %d Canceled events, want 1", len(sink.cancels))
	}
	if got := sink.cancels[0]; got.status != types.OrderStatusCancelled {
		t.Errorf("Canceled event carried status %q, want CANCELLED", got.status)
	}
}

// TestSTPDecrementToZeroCancelsTheMakerUnderProRata is the same assertion on the
// pro-rata allocator, which carries its own copy of the branch.
func TestSTPDecrementToZeroCancelsTheMakerUnderProRata(t *testing.T) {
	e := NewEngine(Config{Symbol: "X", ProRata: true, SelfTradePrevention: STPDecrement})
	mk := e.Process(lim(t, "same", types.SideSell, 100, 3))

	e.Process(lim(t, "same", types.SideBuy, 100, 5))

	if mk.Order.Status != types.OrderStatusCancelled {
		t.Errorf("maker status = %q, want CANCELLED", mk.Order.Status)
	}
}

// TestSTPCancelOldestAnnouncesTheIcebergReserveItDestroys: cancelling an iceberg's
// visible slice cancels the ORDER, reserve included. The reserve cannot be restored
// — the taker's own mode asked for the removal (PINNED-DEFECTS.md §9) — so the
// Canceled has to report the size the client actually loses, and the tracking entry
// has to go with it. It used to report the visible slice only and strand the rest.
func TestSTPCancelOldestAnnouncesTheIcebergReserveItDestroys(t *testing.T) {
	sink := &stpSink{}
	e := NewEngine(Config{Symbol: "X", SelfTradePrevention: STPCancelOldest, EventSink: sink})
	ib := iceberg(t, "whale", types.SideSell, 100, 100, 10) // 10 shown, 90 hidden
	e.ProcessIceberg(ib)

	e.Process(lim(t, "whale", types.SideBuy, 100, 5)) // same account → CANCEL_OLDEST

	if len(sink.cancels) != 1 {
		t.Fatalf("got %d Canceled events, want 1", len(sink.cancels))
	}
	got := sink.cancels[0]
	if got.orderID != ib.Order.ID {
		t.Fatalf("Canceled names order %d, want the iceberg %d", got.orderID, ib.Order.ID)
	}
	if got.remaining != 100 {
		t.Errorf("Canceled reports %d lots remaining, want the whole order at 100", got.remaining)
	}
	if got.quantity != 100 {
		t.Errorf("Canceled reports quantity %d, want 100", got.quantity)
	}
	if _, _, ok := e.BestAsk(); ok {
		t.Error("the cancelled iceberg is still resting")
	}
	if _, tracked := e.icebergOrders[ib.Order.ID]; tracked {
		t.Error("cancelled iceberg left an entry in the tracking map")
	}
	if ib.Hidden != 0 {
		t.Errorf("hidden reserve = %d, want 0 after the order was cancelled", ib.Hidden)
	}
}

// TestSTPCancelBothAnnouncesTheIcebergReserveItDestroys: the CANCEL_BOTH branch is a
// separate copy of the same removal and gets the same guarantee.
func TestSTPCancelBothAnnouncesTheIcebergReserveItDestroys(t *testing.T) {
	sink := &stpSink{}
	e := NewEngine(Config{Symbol: "X", SelfTradePrevention: STPCancelBoth, EventSink: sink})
	ib := iceberg(t, "whale", types.SideSell, 100, 40, 10)
	e.ProcessIceberg(ib)

	e.Process(lim(t, "whale", types.SideBuy, 100, 5))

	// CANCEL_BOTH cancels the taker too, so the iceberg's own event is the one to find.
	var found bool
	for _, c := range sink.cancels {
		if c.orderID != ib.Order.ID {
			continue
		}
		found = true
		if c.remaining != 40 {
			t.Errorf("Canceled reports %d lots remaining, want the whole order at 40", c.remaining)
		}
	}
	if !found {
		t.Fatalf("no Canceled published for the iceberg, got %+v", sink.cancels)
	}
	if _, tracked := e.icebergOrders[ib.Order.ID]; tracked {
		t.Error("cancelled iceberg left an entry in the tracking map")
	}
}

// TestSTPDecrementWorksThroughTheIcebergReserve: DECREMENT is defined over the two
// orders' overlap, and an iceberg is one order. Stopping at the visible slice took a
// display chunk off the taker and left the reserve resting behind an order the book
// no longer held; the decrement now continues into the reserve until one side is out.
func TestSTPDecrementWorksThroughTheIcebergReserve(t *testing.T) {
	e := NewEngine(Config{Symbol: "X", SelfTradePrevention: STPDecrement})
	ib := iceberg(t, "whale", types.SideSell, 100, 50, 10) // 10 shown, 40 hidden
	e.ProcessIceberg(ib)

	taker := lim(t, "whale", types.SideBuy, 100, 25)
	res := e.Process(taker)

	if len(res.Trades) != 0 {
		t.Errorf("DECREMENT prints no trade, got %d", len(res.Trades))
	}
	if taker.RemainingQty != 0 || taker.Status != types.OrderStatusCancelled {
		t.Errorf("taker (smaller side) should fully cancel: rem=%d status=%q",
			taker.RemainingQty, taker.Status)
	}
	// 50 total, 25 decremented away, 25 still working — visible slice plus reserve.
	if got := ib.TotalRemaining(); got != 25 {
		t.Errorf("iceberg total remaining = %d, want 25", got)
	}
	if _, qty, ok := e.BestAsk(); !ok || qty != 5 {
		t.Errorf("iceberg should still be resting with a 5-lot slice, got qty=%d ok=%v", qty, ok)
	}
	if _, tracked := e.icebergOrders[ib.Order.ID]; !tracked {
		t.Error("a still-working iceberg lost its tracking entry")
	}
}

// TestSTPDecrementExhaustsTheIcebergAndCancelsIt: when the taker is the larger side,
// the decrement consumes the whole iceberg — reserve included — and the order ends
// cancelled with nothing left in the tracking map.
func TestSTPDecrementExhaustsTheIcebergAndCancelsIt(t *testing.T) {
	e := NewEngine(Config{Symbol: "X", SelfTradePrevention: STPDecrement})
	ib := iceberg(t, "whale", types.SideSell, 100, 30, 10)
	e.ProcessIceberg(ib)

	taker := lim(t, "whale", types.SideBuy, 100, 45)
	e.Process(taker)

	if got := ib.TotalRemaining(); got != 0 {
		t.Errorf("iceberg total remaining = %d, want 0", got)
	}
	if ib.Order.Status != types.OrderStatusCancelled {
		t.Errorf("exhausted iceberg status = %q, want CANCELLED", ib.Order.Status)
	}
	if _, tracked := e.icebergOrders[ib.Order.ID]; tracked {
		t.Error("exhausted iceberg left an entry in the tracking map")
	}
	// 45 wanted, 30 decremented away, 15 rests.
	if taker.RemainingQty != 15 {
		t.Errorf("taker remaining = %d, want 15", taker.RemainingQty)
	}
	if _, qty, ok := e.BestBid(); !ok || qty != 15 {
		t.Errorf("taker should rest with 15, got qty=%d ok=%v", qty, ok)
	}
}
