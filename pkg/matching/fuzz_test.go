package matching

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// checkInvariants asserts the engine's core safety properties after an operation:
// the resting book is never crossed, every order the engine still owns conserves
// quantity in BOTH directions, anything resting is in a state that permits resting,
// and each level's maintained aggregate agrees with the orders actually in it.
//
// It is a WHOLE-BOOK check and not a one-order check, and that is the load-bearing
// half of it. A defect in a REVERSAL is by construction a defect in a maker; the
// suite used to check only the order passed in, which is the taker — the one order
// that cannot be the victim — and both callers that hammer the exotic surface pass
// nil, so on that whole surface it asserted exactly one property and it was not
// about quantity. A fill-or-kill that exhausted an iceberg's reserve and then failed
// left a resting order with FilledQty -6 and nine displayed lots against a stated
// quantity of three, and this function returned green.
// docs/PINNED-DEFECTS.md §5.
//
// The four assertions are not one assertion written four ways, and each catches a
// different member of the family: `filled + remaining == quantity` constrains
// neither term's SIGN (-6 + 9 == 3 satisfies it); the sign checks say nothing about
// the LEVEL that publishes the order, which the book maintains incrementally through
// a `contributed` field that is deliberately not `order.RemainingQty`; and none of
// them says a zero-remaining or cancelled order cannot be sitting in the book.
func checkInvariants(t testingTB, e *Engine, o *types.Order) {
	t.Helper()
	if bid, _, okB := e.BestBid(); okB {
		if ask, _, okA := e.BestAsk(); okA && bid >= ask {
			t.Fatalf("book crossed: best bid %d >= best ask %d", bid, ask)
		}
	}
	if o != nil {
		checkOrderConserves(t, "the submitted order", o)
	}

	book := e.Book()
	for _, r := range book.Orders() {
		who := fmt.Sprintf("a resting order (%d)", r.ID)
		checkOrderConserves(t, who, r)
		// An order with nothing left, or one already cancelled, filled or rejected,
		// satisfies conservation perfectly and has no business resting.
		if r.RemainingQty <= 0 {
			t.Fatalf("%s: rests with %d remaining", who, r.RemainingQty)
		}
		if !r.IsActive() {
			t.Fatalf("%s: rests with status %s, which is not an active status", who, r.Status)
		}
	}

	// The level's maintained TotalQty against the orders actually resting there. It
	// is the only assertion that sees the engine disagreeing with itself about an
	// order it has otherwise repaired.
	depth := book.Count() + 1
	for _, side := range []types.Side{types.SideBuy, types.SideSell} {
		var levels []*orderbook.PriceLevel
		if side == types.SideBuy {
			levels = book.GetBidLevels(depth)
		} else {
			levels = book.GetAskLevels(depth)
		}
		for _, l := range levels {
			var sum int64
			orders := book.GetOrdersAtPrice(side, l.Price)
			for _, r := range orders {
				sum += r.RemainingQty
			}
			if sum != l.TotalQty {
				t.Fatalf("level %s %d publishes %d lots and the %d orders resting there hold %d",
					side, l.Price, l.TotalQty, len(orders), sum)
			}
		}
	}
}

// checkOrderConserves is the per-order half: quantity is conserved and neither term
// has gone negative. who names the order in the failure, because "which order" is
// the whole difference between the old check and this one.
func checkOrderConserves(t testingTB, who string, o *types.Order) {
	t.Helper()
	if o.FilledQty+o.RemainingQty != o.Quantity {
		t.Fatalf("%s: quantity not conserved: filled %d + remaining %d != quantity %d",
			who, o.FilledQty, o.RemainingQty, o.Quantity)
	}
	if o.RemainingQty < 0 {
		t.Fatalf("%s: negative remaining quantity: %d", who, o.RemainingQty)
	}
	if o.FilledQty < 0 {
		t.Fatalf("%s: negative filled quantity: %d", who, o.FilledQty)
	}
}

// testingTB is the shared subset of *testing.T and *testing.F used above.
type testingTB interface {
	Helper()
	Fatalf(string, ...any)
}

// FuzzEngine feeds a decoded byte stream as a sequence of orders (and occasional
// cancels) and checks the invariants hold after every step and nothing panics.
func FuzzEngine(f *testing.F) {
	f.Add([]byte{0x11, 0x05, 0x02, 0x00, 0x84, 0x03, 0x03, 0x00})
	f.Add([]byte{0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		e := NewEngine(DefaultConfig("F"))
		var placed []int64
		// Each op is 4 bytes: [flags, priceLo, priceHi, qty]. flags bit0=side,
		// bit1=market, bits2-3 select cancel/TIF.
		for i := 0; i+3 < len(data); i += 4 {
			flags := data[i]
			price := int64(data[i+1]) | int64(data[i+2])<<8
			qty := int64(data[i+3])

			if flags&0x0c == 0x0c && len(placed) > 0 {
				// Cancel a previously placed order.
				id := placed[int(price)%len(placed)]
				_, _ = e.Cancel(id, "u")
				checkInvariants(t, e, nil)
				continue
			}

			side := types.SideBuy
			if flags&0x01 != 0 {
				side = types.SideSell
			}
			otype := types.OrderTypeLimit
			tif := types.TIFGoodTillCancel
			if flags&0x02 != 0 {
				otype = types.OrderTypeMarket
				tif = types.TIFImmediateOrCancel
				price = 0
			} else {
				price = price%4000 + 1 // 1..4000, always positive
			}
			qty = qty%100 + 1 // 1..100

			o, err := types.NewOrder("u", "F", side, otype, price, qty, tif)
			if err != nil {
				continue
			}
			res := e.Process(o)
			checkInvariants(t, e, o)
			if res.Status == types.OrderStatusNew || res.Status == types.OrderStatusPartiallyFilled {
				placed = append(placed, o.ID)
			}
		}
	})
}

// FuzzExoticOrders hammers the advanced order types (stop/iceberg/trailing) plus
// market takers that trigger them, against a warm two-sided book — the surface
// that actually breaks real engines (Binance's 2023 trailing-stop halt, ASX's
// combination-order outage). It asserts invariants hold after every step (book
// never crossed, quantity conserved) and that no exotic trips an unbounded
// trigger loop (bounded by maxStopCascade, so the call must simply return).
//
// The FILL-OR-KILL symbol is half of one deliverable with the strengthened
// checkInvariants above, and an invariant with no reachable input is decoration:
// the alphabet used to draw no fill-or-kill at all, so nothing here could reach a
// reversal, and the sign check would have sat green forever on the only target in
// the repository that hammers icebergs. With both halves, against the engine before
// docs/PINNED-DEFECTS.md §3's fix, this target fails from the seed corpus in a
// fraction of a second on a twelve-byte input and nobody has to predict the defect.
func FuzzExoticOrders(f *testing.F) {
	f.Add([]byte{0x00, 0x5f, 0x05, 0x01, 0x63, 0x03, 0x03, 0x0a, 0x02})
	// The regression seed: an iceberg buy of 10 shown 5 at 102, and a stranger's
	// fill-or-kill sell of 12 at 101 that consumes both slices and then cannot fill.
	// Against the engine before docs/PINNED-DEFECTS.md §3's fix it fails on
	// `a resting order (5): negative filled quantity: -5`. Committed so the path is
	// reached by a plain `go test` run and not only under -fuzz.
	f.Add([]byte{0x01, 0x65, 0x04, 0x04, 0x64, 0x0b})
	// And the input the fuzzer itself minimised to, kept because it is the artefact
	// rather than a reconstruction: nobody predicted it, and it fails the unfixed
	// engine on `a resting order (9): negative filled quantity: -1`.
	f.Add([]byte("8xX1010010008021010"))
	f.Fuzz(func(t *testing.T, data []byte) {
		e := NewEngine(DefaultConfig("FX"))
		// Seed a two-sided book and a last trade price (=100) so stops/trailing
		// have a live reference.
		e.Process(lim(t, "seed", types.SideBuy, 100, 50))
		e.Process(lim(t, "seed2", types.SideSell, 100, 20)) // trades → last=100
		e.Process(lim(t, "seed", types.SideBuy, 95, 50))
		e.Process(lim(t, "seed", types.SideSell, 105, 50))

		for i := 0; i+2 < len(data); i += 3 {
			price := int64(data[i+1])%200 + 1
			qty := int64(data[i+2])%50 + 1
			switch data[i] % 5 {
			case 0: // sell stop
				o, err := types.NewOrder("x", "FX", types.SideSell, types.OrderTypeMarket, 0, qty, types.TIFImmediateOrCancel)
				if err == nil {
					if so, err := types.NewStopOrder(o, price); err == nil {
						e.ProcessStop(so)
					}
				}
			case 1: // iceberg buy
				o, err := types.NewOrder("x", "FX", types.SideBuy, types.OrderTypeLimit, price, qty*2, types.TIFGoodTillCancel)
				if err == nil {
					if ib, err := types.NewIcebergOrder(o, qty); err == nil {
						e.ProcessIceberg(ib)
					}
				}
			case 2: // trailing sell stop
				o, err := types.NewOrder("x", "FX", types.SideSell, types.OrderTypeMarket, 0, qty, types.TIFImmediateOrCancel)
				if err == nil {
					if ts, err := types.NewTrailingStop(o, price%20+1); err == nil {
						e.ProcessTrailingStop(ts)
					}
				}
			case 3: // market taker to move the price and trigger resting exotics
				side := types.SideBuy
				if qty%2 == 0 {
					side = types.SideSell
				}
				if o, err := types.NewOrder("x", "FX", side, types.OrderTypeMarket, 0, qty, types.TIFImmediateOrCancel); err == nil {
					e.Process(o)
				}
			case 4: // fill-or-kill taker — the only draw that can reach a reversal
				side := types.SideBuy
				if qty%2 == 0 {
					side = types.SideSell
				}
				// A DIFFERENT account from every other draw here, and that is the whole
				// symbol rather than an incidental name. The exotics above are all "x",
				// so an "x" fill-or-kill would meet "x"'s own iceberg, be stopped by
				// self-trade prevention and never print — the reversal path would stay
				// unreachable and this case would be decoration. The defect is a
				// STRANGER's rejected order leaking a client's reserve, so the stranger
				// has to exist. Measured: with "x" here, 1.3 million executions found
				// nothing; with "y", a six-byte input does.
				if o, err := types.NewOrder("y", "FX", side, types.OrderTypeLimit, price, qty, types.TIFFillOrKill); err == nil {
					e.Process(o)
				}
			}
			checkInvariants(t, e, nil)
		}
	})
}

// TestSoak drives a long, deterministic, mixed order flow and checks invariants
// throughout — a cheap stand-in for a soak run. Skipped under -short.
func TestSoak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping soak in -short mode")
	}
	rng := rand.New(rand.NewSource(1))
	e := NewEngine(Config{Symbol: "S", MaxOrders: 200000, SelfTradePrevention: STPCancelNewest})
	var placed []int64
	buf := make([]types.Trade, 0, 16)

	const ops = 500000
	for i := 0; i < ops; i++ {
		switch {
		case len(placed) > 5 && rng.Intn(100) < 60:
			// 60%: cancel (the dominant real-world op).
			j := rng.Intn(len(placed))
			_, _ = e.Cancel(placed[j], "u")
			placed[j] = placed[len(placed)-1]
			placed = placed[:len(placed)-1]
		default:
			side := types.SideBuy
			if rng.Intn(2) == 0 {
				side = types.SideSell
			}
			price := int64(9000 + rng.Intn(2000))
			o, err := types.NewOrder("u", "S", side, types.OrderTypeLimit, price, int64(1+rng.Intn(10)), types.TIFGoodTillCancel)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			var status types.OrderStatus
			buf, status, _ = e.Match(o, buf[:0])
			if status == types.OrderStatusNew || status == types.OrderStatusPartiallyFilled {
				placed = append(placed, o.ID)
			}
		}
		if i%50000 == 0 {
			checkInvariants(t, e, nil)
		}
	}
	checkInvariants(t, e, nil)
	t.Logf("soak complete: %d ops, %d resting", ops, e.OrderCount())
}
