package marketdata

import (
	"sort"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// L2Feed turns the engine's per-order event stream into aggregated level changes —
// the incremental depth feed a market-data publisher needs, where before there was a
// choice between full snapshots and doing this yourself.
//
// # Why this is here and not in the engine
//
// matching.EventBookDelta was reserved for exactly this and is never emitted, which
// was the right call in the end. L2 is a pure function of L3, and the event stream is
// tested to reconstruct the L3 book exactly. Producing deltas on the matching
// goroutine would mean doing work there to compute something a consumer can compute
// off it, on a path whose whole design goal is to allocate nothing and return fast.
//
// So the aggregation lives here, above the engine, driven by the same events every
// other consumer sees. The test for it asserts the derived levels equal the engine's
// own Snapshot after every command, which is a stronger claim than any assertion
// about the deltas in isolation: the two views cannot disagree without the test
// failing.
//
// # Use
//
// L2Feed implements matching.EventSink, so attach it directly, or fan events to it
// alongside your other sinks with matching.MultiSink. Call Drain to take the deltas
// accumulated since the last call.
//
// Not safe for concurrent use. OnEvents is called from the matching goroutine (or the
// pump in front of it); Drain must be serialised against it by the caller — the
// intended shape is a single publisher goroutine doing both.
type L2Feed struct {
	// live tracks each resting order's side, price and remaining quantity, which is
	// what makes a level aggregate derivable: a Trade names the order, not the level.
	live map[int64]liveOrder
	bids map[int64]int64 // price -> aggregate lots
	asks map[int64]int64
	// touched records the levels a batch changed, so a command that moves one level
	// three times produces one delta with its final quantity rather than three.
	touched map[levelKey]struct{}
	out     []L2Delta
	seq     int64
}

type liveOrder struct {
	side      types.Side
	price     int64
	remaining int64
}

type levelKey struct {
	side  types.Side
	price int64
}

// L2Delta is one aggregated level change. Qty is the level's NEW total, and zero
// means the level is now empty — a subscriber should remove it rather than show a
// zero.
//
// Deltas are absolute rather than incremental on purpose: a subscriber that missed
// one recovers on the next delta for that level instead of being permanently wrong,
// which is the same reasoning that makes Reduce carry a total and not a delta.
type L2Delta struct {
	Seq   int64      // the engine event sequence this delta was derived from
	Side  types.Side // which side of the book
	Price int64      // ticks
	Qty   int64      // the level's new aggregate in lots; 0 means the level is gone
}

// NewL2Feed builds an empty feed. It must be attached before the first order, or it
// will be aggregating from a book it never saw the start of; to attach to a running
// engine, seed it with Reset from a snapshot first.
func NewL2Feed() *L2Feed {
	return &L2Feed{
		live:    map[int64]liveOrder{},
		bids:    map[int64]int64{},
		asks:    map[int64]int64{},
		touched: map[levelKey]struct{}{},
	}
}

// OnEvents consumes one batch and accumulates the level changes it caused.
//
// It handles exactly the kinds that move a level: Accepted adds an order, Trade
// reduces both sides of a fill, Canceled removes an order, and Replaced resizes one
// in place. Triggered, Rejected, Halted and Resumed change no aggregate, and a kind
// this does not recognise is ignored rather than guessed at.
func (f *L2Feed) OnEvents(evs []matching.Event) {
	for i := range evs {
		e := &evs[i]
		f.seq = e.Seq
		switch e.Kind {
		case matching.EventAccepted:
			if e.Order == nil {
				continue
			}
			// An iceberg re-announces the same id when it reloads, so an Accepted for
			// an order already tracked replaces its contribution rather than adding a
			// second one.
			f.forget(e.Order.ID)
			f.remember(e.Order)

		case matching.EventReplaced:
			if e.Order == nil {
				continue
			}
			f.resize(e.Order.ID, e.Order.RemainingQty)

		case matching.EventCanceled:
			if e.Order == nil {
				continue
			}
			f.forget(e.Order.ID)

		case matching.EventTrade:
			if e.Trade == nil {
				continue
			}
			// A fill reduces whichever side was resting. The taker may never have
			// rested at all, in which case it is simply not tracked and fill is a
			// no-op for it.
			f.fill(e.Trade.BuyOrderID, e.Trade.Quantity)
			f.fill(e.Trade.SellOrderID, e.Trade.Quantity)
		}
	}
	f.flush()
}

func (f *L2Feed) sideMap(s types.Side) map[int64]int64 {
	if s == types.SideBuy {
		return f.bids
	}
	return f.asks
}

func (f *L2Feed) remember(o *types.Order) {
	if o.RemainingQty <= 0 {
		return // filled on arrival; it never rested, so no level changed
	}
	f.live[o.ID] = liveOrder{side: o.Side, price: o.Price, remaining: o.RemainingQty}
	f.add(o.Side, o.Price, o.RemainingQty)
}

func (f *L2Feed) forget(id int64) {
	lo, ok := f.live[id]
	if !ok {
		return
	}
	delete(f.live, id)
	f.add(lo.side, lo.price, -lo.remaining)
}

func (f *L2Feed) resize(id, remaining int64) {
	lo, ok := f.live[id]
	if !ok {
		return
	}
	f.add(lo.side, lo.price, remaining-lo.remaining)
	lo.remaining = remaining
	if remaining <= 0 {
		delete(f.live, id)
		return
	}
	f.live[id] = lo
}

func (f *L2Feed) fill(id, qty int64) {
	lo, ok := f.live[id]
	if !ok {
		return
	}
	f.resize(id, lo.remaining-qty)
}

// add applies a delta to a level and records that the level moved.
func (f *L2Feed) add(side types.Side, price, delta int64) {
	if delta == 0 {
		return
	}
	m := f.sideMap(side)
	q := m[price] + delta
	if q <= 0 {
		delete(m, price)
	} else {
		m[price] = q
	}
	f.touched[levelKey{side, price}] = struct{}{}
}

// flush turns the levels this batch touched into deltas, in a deterministic order so
// two runs of the same command stream produce the same feed.
func (f *L2Feed) flush() {
	if len(f.touched) == 0 {
		return
	}
	keys := make([]levelKey, 0, len(f.touched))
	for k := range f.touched {
		keys = append(keys, k)
		delete(f.touched, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].side != keys[j].side {
			return keys[i].side == types.SideBuy
		}
		return keys[i].price < keys[j].price
	})
	for _, k := range keys {
		f.out = append(f.out, L2Delta{Seq: f.seq, Side: k.side, Price: k.price, Qty: f.sideMap(k.side)[k.price]})
	}
}

// Adopt seeds the aggregate from a set of resting orders, without producing deltas.
//
// Used to start a feed against a book that already exists — after a recovery — where
// the levels are the starting state rather than changes to it.
func (f *L2Feed) Adopt(orders []*types.Order) {
	for _, o := range orders {
		if o == nil || o.RemainingQty <= 0 {
			continue
		}
		f.live[o.ID] = liveOrder{side: o.Side, price: o.Price, remaining: o.RemainingQty}
		m := f.sideMap(o.Side)
		m[o.Price] += o.RemainingQty
	}
	// Deliberately not marking these levels touched: they are not changes.
	clear(f.touched)
	f.out = nil
}

// Drain returns the deltas accumulated since the last call and resets the buffer.
// The returned slice is the caller's; the feed does not retain it.
func (f *L2Feed) Drain() []L2Delta {
	if len(f.out) == 0 {
		return nil
	}
	out := f.out
	f.out = nil
	return out
}

// Levels returns the current aggregate for one side, best price first — the feed's
// own view, for comparing against a venue snapshot or for seeding a late subscriber.
func (f *L2Feed) Levels(side types.Side) []L2Delta {
	m := f.sideMap(side)
	out := make([]L2Delta, 0, len(m))
	for price, qty := range m {
		out = append(out, L2Delta{Seq: f.seq, Side: side, Price: price, Qty: qty})
	}
	sort.Slice(out, func(i, j int) bool {
		if side == types.SideBuy {
			return out[i].Price > out[j].Price // best bid is the highest
		}
		return out[i].Price < out[j].Price // best ask is the lowest
	})
	return out
}
