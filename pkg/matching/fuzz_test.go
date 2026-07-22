package matching

import (
	"math/rand"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// checkInvariants asserts the engine's core safety properties after an operation:
// the resting book is never crossed, and the order just processed conserves
// quantity (filled + remaining == original).
func checkInvariants(t testingTB, e *Engine, o *types.Order) {
	t.Helper()
	if bid, _, okB := e.BestBid(); okB {
		if ask, _, okA := e.BestAsk(); okA && bid >= ask {
			t.Fatalf("book crossed: best bid %d >= best ask %d", bid, ask)
		}
	}
	if o != nil && o.FilledQty+o.RemainingQty != o.Quantity {
		t.Fatalf("quantity not conserved: filled %d + remaining %d != quantity %d",
			o.FilledQty, o.RemainingQty, o.Quantity)
	}
	if o != nil && o.RemainingQty < 0 {
		t.Fatalf("negative remaining quantity: %d", o.RemainingQty)
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
func FuzzExoticOrders(f *testing.F) {
	f.Add([]byte{0x00, 0x5f, 0x05, 0x01, 0x63, 0x03, 0x03, 0x0a, 0x02})
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
			switch data[i] % 4 {
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
