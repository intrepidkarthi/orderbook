package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// ocoSink records events by value. Both Event.Order and Event.Trade are pointers
// into engine-owned state that the engine keeps mutating after OnEvents returns,
// so a sink that stores the Event alone observes later mutations rather than what
// was published. Deep-copying here is what makes "was it ACCEPTED at the time"
// answerable at all.
type ocoSink struct {
	got    []Event
	status []types.OrderStatus // status of got[i].Order as published
}

func (s *ocoSink) OnEvents(evs []Event) {
	for _, e := range evs {
		c := e
		var st types.OrderStatus
		if e.Order != nil {
			oc := *e.Order
			c.Order = &oc
			st = e.Order.Status
		}
		if e.Trade != nil {
			tc := *e.Trade
			c.Trade = &tc
		}
		s.got = append(s.got, c)
		s.status = append(s.status, st)
	}
}

func (s *ocoSink) trades() []types.Trade {
	var out []types.Trade
	for _, e := range s.got {
		if e.Kind == EventTrade && e.Trade != nil {
			out = append(out, *e.Trade)
		}
	}
	return out
}

func (s *ocoSink) sawOrder(kind EventKind, orderID int64) bool {
	for _, e := range s.got {
		if e.Kind == kind && e.OrderID == orderID {
			return true
		}
	}
	return false
}

// TestOCO_StopFiringOnEntryPublishesItsExecutions is the regression for a lost
// fill. ProcessOCO called submitStopInto and discarded all three return values,
// so when the stop leg triggered on entry its executions settled through the book
// — filling and removing a real maker, and moving the last trade price — while
// reaching neither the event stream nor any MatchResult. The counterparty's fill
// simply vanished.
func TestOCO_StopFiringOnEntryPublishesItsExecutions(t *testing.T) {
	sink := &ocoSink{}
	e := NewEngine(Config{Symbol: "X", EventSink: sink})
	seedWithLastPrice(t, e) // last = 100, resting bids at 100 (5), 96, 95

	_, bidQtyBefore, ok := e.BestBid()
	if !ok {
		t.Fatal("expected a resting bid after seeding")
	}
	before := len(sink.trades())

	// Sell stop triggered at 105: last trade is 100, already at or below it, so it
	// fires on entry and sweeps the best bid.
	primary := lim(t, "trader", types.SideSell, 120, 3)
	stop := stopOrder(t, "trader", types.SideSell, 3, 105)
	oco, err := types.NewOCOOrder(primary, stop)
	if err != nil {
		t.Fatalf("NewOCOOrder: %v", err)
	}
	e.ProcessOCO(oco)

	if !stop.IsTriggered() {
		t.Fatalf("stop did not fire on entry; test setup no longer exercises the bug")
	}

	// The book must actually have moved — that is what makes the loss real rather
	// than cosmetic. Real makers were filled and removed.
	_, bidQtyAfter, _ := e.BestBid()
	if bidQtyBefore-bidQtyAfter != 3 {
		t.Fatalf("best-bid quantity went %d -> %d, want a 3-lot decrease", bidQtyBefore, bidQtyAfter)
	}

	got := sink.trades()[before:]
	if len(got) == 0 {
		t.Fatal("stop fired on entry and filled real makers, but no trade was published")
	}
	var published int64
	for _, tr := range got {
		if tr.TakerOrderID != stop.Order.ID {
			t.Errorf("trade taker = %d, want the stop leg %d", tr.TakerOrderID, stop.Order.ID)
		}
		published += tr.Quantity
	}
	if published != 3 {
		t.Errorf("published quantity = %d, want 3 (the book moved by 3)", published)
	}
}

// TestOCO_StopLegIsAccepted covers the other half of the same defect: the stop
// leg was assigned an engine id and registered in the OCO registry but never
// announced, so any later fill referenced an order id no consumer had seen.
func TestOCO_StopLegIsAccepted(t *testing.T) {
	sink := &ocoSink{}
	e := NewEngine(Config{Symbol: "X", EventSink: sink})
	seedWithLastPrice(t, e) // last = 100

	oco := makeOCO(t) // primary sell 105, stop sell triggered at 95 — rests
	e.ProcessOCO(oco)

	if oco.Stop.IsTriggered() {
		t.Fatal("stop should rest for this case, not fire")
	}
	if !sink.sawOrder(EventAccepted, oco.Primary.ID) {
		t.Errorf("no Accepted for the primary leg %d", oco.Primary.ID)
	}
	if !sink.sawOrder(EventAccepted, oco.Stop.Order.ID) {
		t.Errorf("no Accepted for the stop leg %d; its later fills would reference an unknown order",
			oco.Stop.Order.ID)
	}
}

// TestOCO_PrimaryPublishedBeforeItIsCancelled pins the emission order. The
// primary used to be published after the stop had already cancelled it, so the
// stream announced a dead order as ACCEPTED.
func TestOCO_PrimaryPublishedBeforeItIsCancelled(t *testing.T) {
	sink := &ocoSink{}
	e := NewEngine(Config{Symbol: "X", EventSink: sink})
	seedWithLastPrice(t, e) // last = 100
	e.Process(lim(t, "maker", types.SideBuy, 95, 3))

	primary := lim(t, "trader", types.SideSell, 120, 3)
	stop := stopOrder(t, "trader", types.SideSell, 3, 105) // fires on entry
	oco, err := types.NewOCOOrder(primary, stop)
	if err != nil {
		t.Fatalf("NewOCOOrder: %v", err)
	}
	e.ProcessOCO(oco)

	for i, ev := range sink.got {
		if ev.Kind == EventAccepted && ev.OrderID == primary.ID {
			if sink.status[i] == types.OrderStatusCancelled {
				t.Error("primary announced as ACCEPTED while already cancelled")
			}
			return
		}
	}
	t.Error("primary was never announced")
}
