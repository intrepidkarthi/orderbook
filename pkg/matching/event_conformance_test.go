package matching

import (
	"fmt"
	"sort"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// mirrorOrder is one order as reconstructed from the event stream alone.
type mirrorOrder struct {
	id        int64
	price     int64
	side      types.Side
	remaining int64
}

// mirrorBook rebuilds an L3 book from nothing but the EventSink stream, which is
// exactly what a drop copy, a market-data feed or a resuming client must be able
// to do. If this diverges from the engine's own book, every consumer built on the
// stream is wrong in the same way.
type mirrorBook struct {
	orders map[int64]*mirrorOrder
	seq    int64
	gaps   []string
}

func newMirror() *mirrorBook {
	return &mirrorBook{orders: map[int64]*mirrorOrder{}}
}

func (m *mirrorBook) OnEvents(evs []Event) {
	for _, e := range evs {
		if m.seq != 0 && e.Seq != m.seq+1 {
			m.gaps = append(m.gaps, fmt.Sprintf("seq jumped %d -> %d", m.seq, e.Seq))
		}
		m.seq = e.Seq

		switch e.Kind {
		case EventAccepted:
			if e.Order == nil {
				continue
			}
			// A pending stop or trailing rests off-book in the stop book, so it is
			// announced but must not appear in an L3 reconstruction of the book.
			if e.Order.Status == types.OrderStatusPendingTrigger {
				continue
			}
			// Quantity is the as-submitted size; trades that follow reduce it.
			// RemainingQty cannot be used here because emitResult runs after
			// settleInto, so a marketable order is already terminal at this point.
			m.orders[e.Order.ID] = &mirrorOrder{
				id: e.Order.ID, price: e.Order.Price, side: e.Order.Side, remaining: e.Order.Quantity,
			}
		case EventTrade:
			if e.Trade == nil {
				continue
			}
			m.fill(e.Trade.MakerOrderID, e.Trade.Quantity)
			m.fill(e.Trade.TakerOrderID, e.Trade.Quantity)
		case EventReplaced:
			if e.Order == nil {
				continue
			}
			if o, ok := m.orders[e.Order.ID]; ok {
				o.remaining = e.Order.RemainingQty
			} else {
				m.gaps = append(m.gaps, fmt.Sprintf("replace for order %d never announced", e.Order.ID))
			}
		case EventCanceled:
			if e.Order != nil {
				delete(m.orders, e.Order.ID)
			}
		case EventRejected:
			// nothing entered the book
		}
	}
}

func (m *mirrorBook) fill(id, qty int64) {
	o, ok := m.orders[id]
	if !ok {
		m.gaps = append(m.gaps, fmt.Sprintf("trade against order %d never announced", id))
		return
	}
	o.remaining -= qty
	if o.remaining <= 0 {
		delete(m.orders, id)
	}
}

// resting renders the mirror in a comparable form.
func (m *mirrorBook) resting() []string {
	out := make([]string, 0, len(m.orders))
	for _, o := range m.orders {
		out = append(out, fmt.Sprintf("%d@%d:%d", o.id, o.price, o.remaining))
	}
	sort.Strings(out)
	return out
}

// engineResting renders the engine's real book the same way.
func engineResting(e *Engine) []string {
	orders := e.Book().Orders()
	out := make([]string, 0, len(orders))
	for _, o := range orders {
		out = append(out, fmt.Sprintf("%d@%d:%d", o.ID, o.Price, o.RemainingQty))
	}
	sort.Strings(out)
	return out
}

func assertMirrorMatches(t *testing.T, name string, e *Engine, m *mirrorBook) {
	t.Helper()
	got, want := m.resting(), engineResting(e)
	if len(m.gaps) > 0 {
		t.Errorf("%s: stream defects: %v", name, m.gaps)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: mirror diverged from the engine book\n  mirror: %v\n  engine: %v", name, got, want)
	}
}

// conformanceCase drives one scenario against a fresh engine whose events feed a
// mirror, then asserts the two books agree.
type conformanceCase struct {
	name string
	cfg  func() Config // nil => DefaultConfig
	run  func(t *testing.T, e *Engine)
}

func runConformance(t *testing.T, c conformanceCase) {
	t.Helper()
	m := newMirror()
	cfg := DefaultConfig("X")
	if c.cfg != nil {
		cfg = c.cfg()
	}
	cfg.EventSink = m
	e := NewEngine(cfg)
	c.run(t, e)
	assertMirrorMatches(t, c.name, e, m)
}

func cOrd(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

func cMkt(t *testing.T, user string, side types.Side, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeMarket, 0, qty, types.TIFImmediateOrCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestEventStreamReconstructsBook is the conformance gate for the claim in
// event.go that Accepted/Trade/Canceled reconstruct the L3 book. Each subtest is
// one order class; a failure names the class whose events are incomplete.
func TestEventStreamReconstructsBook(t *testing.T) {
	cases := []conformanceCase{
		{name: "plain limits rest", run: func(t *testing.T, e *Engine) {
			e.Process(cOrd(t, "a", types.SideBuy, 100, 5))
			e.Process(cOrd(t, "b", types.SideBuy, 99, 3))
			e.Process(cOrd(t, "c", types.SideSell, 105, 4))
		}},
		{name: "partial fill leaves a remainder", run: func(t *testing.T, e *Engine) {
			e.Process(cOrd(t, "a", types.SideBuy, 100, 10))
			e.Process(cOrd(t, "b", types.SideSell, 100, 4))
		}},
		{name: "full sweep empties the level", run: func(t *testing.T, e *Engine) {
			e.Process(cOrd(t, "a", types.SideBuy, 100, 5))
			e.Process(cOrd(t, "b", types.SideSell, 100, 5))
		}},
		{name: "explicit cancel", run: func(t *testing.T, e *Engine) {
			o := cOrd(t, "a", types.SideBuy, 100, 5)
			e.Process(o)
			e.Process(cOrd(t, "b", types.SideBuy, 99, 5))
			if _, err := e.Cancel(o.ID, "a"); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		}},
		{name: "market order remainder", run: func(t *testing.T, e *Engine) {
			e.Process(cOrd(t, "a", types.SideBuy, 100, 2))
			e.Process(cMkt(t, "b", types.SideSell, 10)) // only 2 available
		}},
		{name: "IOC remainder", run: func(t *testing.T, e *Engine) {
			e.Process(cOrd(t, "a", types.SideBuy, 100, 2))
			ioc, err := types.NewOrder("b", "X", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFImmediateOrCancel)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			e.Process(ioc)
		}},
		{name: "stop fires and rests its remainder", run: func(t *testing.T, e *Engine) {
			seedWithLastPrice(t, e)
			e.ProcessStop(stopOrder(t, "s", types.SideSell, 3, 105))
		}},
		{name: "stop rests pending", run: func(t *testing.T, e *Engine) {
			seedWithLastPrice(t, e)
			e.ProcessStop(stopOrder(t, "s", types.SideSell, 3, 90))
		}},
		{name: "iceberg refills", run: func(t *testing.T, e *Engine) {
			ib, err := types.NewIcebergOrder(cOrd(t, "mm", types.SideBuy, 100, 1000), 100)
			if err != nil {
				t.Fatalf("NewIcebergOrder: %v", err)
			}
			e.ProcessIceberg(ib)
			e.Process(cOrd(t, "t", types.SideSell, 100, 150)) // consumes a slice and part of the next
		}},
		{name: "OCO stop fires on entry", run: func(t *testing.T, e *Engine) {
			seedWithLastPrice(t, e)
			primary := cOrd(t, "u", types.SideSell, 120, 3)
			st := stopOrder(t, "u", types.SideSell, 3, 105)
			oco, err := types.NewOCOOrder(primary, st)
			if err != nil {
				t.Fatalf("NewOCOOrder: %v", err)
			}
			e.ProcessOCO(oco)
		}},
		{name: "OCO primary fills, stop cancelled", run: func(t *testing.T, e *Engine) {
			seedWithLastPrice(t, e)
			oco := makeOCO(t)
			e.ProcessOCO(oco)
			e.Process(cOrd(t, "buyer", types.SideBuy, 105, 3))
		}},
	}
	cases = append(cases, stpCases(t)...)
	cases = append(cases, extraCases(t)...)
	cases = append(cases, bustConformanceCase(t)) // see bust_test.go

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runConformance(t, c) })
	}
}

// stpCases covers every self-trade-prevention mode. Each mutates or removes
// orders mid-match, which is precisely where emissions were missing.
func stpCases(t *testing.T) []conformanceCase {
	modes := []struct {
		name string
		mode SelfTradePrevention
	}{
		{"STP cancel-newest", STPCancelNewest},
		{"STP cancel-oldest", STPCancelOldest},
		{"STP cancel-both", STPCancelBoth},
		{"STP decrement", STPDecrement},
		{"STP allow", STPAllow},
	}
	out := make([]conformanceCase, 0, len(modes))
	for _, md := range modes {
		md := md
		out = append(out, conformanceCase{
			name: md.name,
			cfg:  func() Config { c := DefaultConfig("X"); c.SelfTradePrevention = md.mode; return c },
			run: func(t *testing.T, e *Engine) {
				e.Process(cOrd(t, "same", types.SideBuy, 100, 5))
				e.Process(cOrd(t, "other", types.SideBuy, 99, 5))
				e.Process(cOrd(t, "same", types.SideSell, 100, 3)) // would self-trade
			},
		})
	}
	return out
}

func extraCases(t *testing.T) []conformanceCase {
	return []conformanceCase{
		{name: "FOK that cannot fill unwinds cleanly", run: func(t *testing.T, e *Engine) {
			e.Process(cOrd(t, "a", types.SideBuy, 100, 2))
			fok, err := types.NewOrder("b", "X", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFFillOrKill)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			e.Process(fok)
		}},
		{name: "FOK that can fill", run: func(t *testing.T, e *Engine) {
			e.Process(cOrd(t, "a", types.SideBuy, 100, 9))
			fok, err := types.NewOrder("b", "X", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFFillOrKill)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			e.Process(fok)
		}},
		{name: "stop cascade fired by another order's match", run: func(t *testing.T, e *Engine) {
			seedWithLastPrice(t, e)
			e.ProcessStop(stopOrder(t, "s", types.SideSell, 3, 97)) // rests; fires when price falls
			e.Process(cOrd(t, "t", types.SideSell, 96, 6))          // drives last price down to 96
		}},
		{name: "post-only that would cross", run: func(t *testing.T, e *Engine) {
			e.Process(cOrd(t, "a", types.SideBuy, 100, 5))
			po, err := types.NewOrder("b", "X", types.SideSell, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			po.PostOnly = true
			e.Process(po)
		}},
		{name: "cancel a pending stop", run: func(t *testing.T, e *Engine) {
			seedWithLastPrice(t, e)
			st := stopOrder(t, "s", types.SideSell, 3, 90)
			e.ProcessStop(st)
			if _, err := e.Cancel(st.Order.ID, "s"); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		}},
		{name: "iceberg fully exhausted", run: func(t *testing.T, e *Engine) {
			ib, err := types.NewIcebergOrder(cOrd(t, "mm", types.SideBuy, 100, 300), 100)
			if err != nil {
				t.Fatalf("NewIcebergOrder: %v", err)
			}
			e.ProcessIceberg(ib)
			e.Process(cOrd(t, "t", types.SideSell, 100, 300))
		}},
	}
}
