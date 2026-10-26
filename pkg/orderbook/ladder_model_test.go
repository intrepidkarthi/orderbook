package orderbook

import (
	"math/rand"
	"slices"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// bookModel is the oracle for docs/BOOK-LADDER.md §6.1: a price -> FIFO map, sorted
// when read. Nothing about it is fast, which is the point. Whatever the book keeps
// its levels in, its answers must be the ones this gives.
type bookModel struct {
	fifo  map[types.Side]map[int64][]*types.Order
	where map[int64]*types.Order
}

func newBookModel() *bookModel {
	return &bookModel{
		fifo:  map[types.Side]map[int64][]*types.Order{types.SideBuy: {}, types.SideSell: {}},
		where: map[int64]*types.Order{},
	}
}

func (m *bookModel) add(o *types.Order) {
	m.fifo[o.Side][o.Price] = append(m.fifo[o.Side][o.Price], o)
	m.where[o.ID] = o
}

func (m *bookModel) remove(id int64) {
	o := m.where[id]
	q := m.fifo[o.Side][o.Price]
	i := slices.Index(q, o)
	q = slices.Delete(q, i, i+1)
	if len(q) == 0 {
		delete(m.fifo[o.Side], o.Price)
	} else {
		m.fifo[o.Side][o.Price] = q
	}
	delete(m.where, id)
}

// prices is one side's prices, best first.
func (m *bookModel) prices(side types.Side) []int64 {
	var ps []int64
	for p := range m.fifo[side] {
		ps = append(ps, p)
	}
	slices.Sort(ps)
	if side == types.SideBuy {
		slices.Reverse(ps)
	}
	return ps
}

func (m *bookModel) total(side types.Side, price int64) int64 {
	var t int64
	for _, o := range m.fifo[side][price] {
		t += o.RemainingQty
	}
	return t
}

// TestBookAgreesWithModel applies random adds, cancels, partial fills and sweeps
// that empty the best level, and after every step requires the book to give the
// model's answer to every read that depends on how levels are kept.
func TestBookAgreesWithModel(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		ob := New(Config{Symbol: "X", MaxOrders: 1 << 20})
		m := newBookModel()
		var live []int64
		nextID := int64(1)
		// Narrow and wide price ranges both: a narrow one keeps levels deep and
		// often emptied at the best; a wide one keeps many shallow levels.
		span := int64(8 + rng.Intn(400))
		for step := range 2_000 {
			switch r := rng.Intn(10); {
			case r < 5 || len(live) == 0:
				side := types.SideBuy
				price := 1_000 - 1 - rng.Int63n(span)
				if rng.Intn(2) == 0 {
					side, price = types.SideSell, 1_000+rng.Int63n(span)
				}
				o := limit(t, "u"+string(rune('a'+rng.Intn(5))), side, price, 1+rng.Int63n(9))
				o.ID = nextID
				nextID++
				mustAdd(t, ob, o)
				m.add(o)
				live = append(live, o.ID)
			case r < 7:
				i := rng.Intn(len(live))
				id := live[i]
				if _, err := ob.Remove(id); err != nil {
					t.Fatalf("seed %d step %d: Remove(%d): %v", seed, step, id, err)
				}
				m.remove(id)
				live = slices.Delete(live, i, i+1)
			case r < 9:
				// A partial fill in place, as the engine does it: the order's own
				// quantity first, then the book's aggregate.
				o := m.where[live[rng.Intn(len(live))]]
				if o.RemainingQty > 1 {
					q := 1 + rng.Int63n(o.RemainingQty-1)
					o.FilledQty += q
					o.RemainingQty -= q
					ob.UpdateOrderQuantity(o.ID, q)
				}
			default:
				// Sweep the best level of one side: the case a ladder makes O(1).
				side := types.SideBuy
				if rng.Intn(2) == 0 {
					side = types.SideSell
				}
				ps := m.prices(side)
				if len(ps) == 0 {
					break
				}
				for _, o := range slices.Clone(m.fifo[side][ps[0]]) {
					if _, err := ob.Remove(o.ID); err != nil {
						t.Fatalf("seed %d step %d: sweep Remove(%d): %v", seed, step, o.ID, err)
					}
					m.remove(o.ID)
					live = slices.DeleteFunc(live, func(id int64) bool { return id == o.ID })
				}
			}
			checkAgainstModel(t, seed, step, ob, m)
		}
	}
}

func checkAgainstModel(t *testing.T, seed int64, step int, ob *OrderBook, m *bookModel) {
	t.Helper()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed %d step %d: "+format, append([]any{seed, step}, args...)...)
	}
	for _, side := range []types.Side{types.SideBuy, types.SideSell} {
		ps := m.prices(side)

		var bp, bq int64
		var ok bool
		var peek *types.Order
		var levels []*PriceLevel
		if side == types.SideBuy {
			bp, bq, ok = ob.BestBid()
			peek = ob.PeekBestBidOrder()
			levels = ob.GetBidLevels(len(ps) + 1)
		} else {
			bp, bq, ok = ob.BestAsk()
			peek = ob.PeekBestAskOrder()
			levels = ob.GetAskLevels(len(ps) + 1)
		}
		front, frontOK := ob.FrontOrder(side)

		if len(ps) == 0 {
			if ok || peek != nil || frontOK || len(levels) != 0 {
				fail("%s empty in the model, book has best=%v peek=%v front=%v levels=%d", side, ok, peek != nil, frontOK, len(levels))
			}
			continue
		}
		if !ok || bp != ps[0] || bq != m.total(side, ps[0]) {
			fail("%s best: book (%d, %d, %v), model (%d, %d)", side, bp, bq, ok, ps[0], m.total(side, ps[0]))
		}
		if head := m.fifo[side][ps[0]][0]; peek != head || front != head {
			fail("%s front order: peek %v front %v, model %d", side, peek, front, head.ID)
		}
		if len(levels) != len(ps) {
			fail("%s has %d levels, model %d", side, len(levels), len(ps))
		}
		for i, l := range levels {
			if l.Price != ps[i] || l.TotalQty != m.total(side, ps[i]) || l.Count() != len(m.fifo[side][ps[i]]) {
				fail("%s level %d: book (%d, %d, %d), model (%d, %d, %d)", side, i, l.Price, l.TotalQty, l.Count(), ps[i], m.total(side, ps[i]), len(m.fifo[side][ps[i]]))
			}
		}
		mid := ps[len(ps)/2]
		if got, want := ob.GetOrdersAtPrice(side, mid), m.fifo[side][mid]; !slices.Equal(got, want) {
			fail("%s queue at %d differs", side, mid)
		}
	}

	var want []*types.Order
	for _, side := range []types.Side{types.SideBuy, types.SideSell} {
		for _, p := range m.prices(side) {
			want = append(want, m.fifo[side][p]...)
		}
	}
	if got := ob.Orders(); !slices.Equal(got, want) {
		fail("Orders() is not bids then asks, best first, FIFO within a level")
	}
	if ob.Count() != len(m.where) {
		fail("Count %d, model %d", ob.Count(), len(m.where))
	}

	var all []int64
	for _, side := range []types.Side{types.SideBuy, types.SideSell} {
		all = append(all, m.prices(side)...)
	}
	if len(all) > 0 {
		lo, hi := all[step%len(all)], all[(step*7)%len(all)]
		if lo > hi {
			lo, hi = hi, lo
		}
		var depth int64
		for _, side := range []types.Side{types.SideBuy, types.SideSell} {
			for _, p := range m.prices(side) {
				if p >= lo && p <= hi {
					depth += m.total(side, p)
				}
			}
		}
		if got := ob.DepthWithin(lo, hi); got != depth {
			fail("DepthWithin(%d, %d) = %d, model %d", lo, hi, got, depth)
		}
	}
}
