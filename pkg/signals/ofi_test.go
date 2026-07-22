package signals

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func TestOFIStep_NoChange(t *testing.T) {
	s := snap([][2]int64{{100, 5}}, [][2]int64{{101, 5}})
	if e := OFIStep(s, s); e != 0 {
		t.Errorf("no-change OFI = %d, want 0", e)
	}
}

func TestOFIStep_BidQtyUp(t *testing.T) {
	prev := snap([][2]int64{{100, 5}}, [][2]int64{{101, 5}})
	cur := snap([][2]int64{{100, 8}}, [][2]int64{{101, 5}})
	// Same bid price, +3 size; ask unchanged. e = (8-5) - 0 = +3.
	if e := OFIStep(prev, cur); e != 3 {
		t.Errorf("bid-qty-up OFI = %d, want 3", e)
	}
}

func TestOFIStep_AskQtyUp(t *testing.T) {
	prev := snap([][2]int64{{100, 5}}, [][2]int64{{101, 5}})
	cur := snap([][2]int64{{100, 5}}, [][2]int64{{101, 9}})
	// Same ask price, +4 size (more sell pressure). e = 0 - (9-5) = -4.
	if e := OFIStep(prev, cur); e != -4 {
		t.Errorf("ask-qty-up OFI = %d, want -4", e)
	}
}

func TestOFIStep_BidPriceUp(t *testing.T) {
	prev := snap([][2]int64{{100, 5}}, [][2]int64{{102, 5}})
	cur := snap([][2]int64{{101, 3}}, [][2]int64{{102, 5}})
	// Bid stepped up: fresh buy size = current bid qty (3). Ask unchanged. e = +3.
	if e := OFIStep(prev, cur); e != 3 {
		t.Errorf("bid-price-up OFI = %d, want 3", e)
	}
}

func TestOFIStep_AskPriceDown(t *testing.T) {
	prev := snap([][2]int64{{100, 5}}, [][2]int64{{102, 4}})
	cur := snap([][2]int64{{100, 5}}, [][2]int64{{101, 2}})
	// Ask stepped down: fresh sell size = current ask qty (2). e = 0 - 2 = -2.
	if e := OFIStep(prev, cur); e != -2 {
		t.Errorf("ask-price-down OFI = %d, want -2", e)
	}
}

func TestOFI_Accumulator(t *testing.T) {
	o := NewOFI()
	s0 := snap([][2]int64{{100, 5}}, [][2]int64{{101, 5}})
	s1 := snap([][2]int64{{100, 8}}, [][2]int64{{101, 5}}) // +3
	s2 := snap([][2]int64{{100, 8}}, [][2]int64{{101, 9}}) // -4

	if e := o.Observe(s0); e != 0 {
		t.Errorf("first observe = %v, want 0 (primes prev)", e)
	}
	if e := o.Observe(s1); e != 3 {
		t.Errorf("step 1 = %v, want 3", e)
	}
	if e := o.Observe(s2); e != -4 {
		t.Errorf("step 2 = %v, want -4", e)
	}
	if got := o.Cumulative(); got != -1 { // 3 + (-4)
		t.Errorf("cumulative = %v, want -1", got)
	}

	o.Reset()
	if got := o.Cumulative(); got != 0 {
		t.Errorf("after reset = %v, want 0", got)
	}
}

// TestOFI_FromRealBook drives snapshots through the actual order book: adding
// resting bid size (buy pressure) at the touch must yield positive OFI.
func TestOFI_FromRealBook(t *testing.T) {
	ob := orderbook.New(orderbook.Config{Symbol: "BTC-USD"})
	add := func(user string, side types.Side, price, qty int64) {
		o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		if err := ob.Add(o); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	add("a", types.SideBuy, 100, 5)
	add("b", types.SideSell, 101, 5)
	before := ob.Snapshot(5)

	add("c", types.SideBuy, 100, 5) // more resting bid size at the best bid
	after := ob.Snapshot(5)

	if e := OFIStep(before, after); e <= 0 {
		t.Errorf("adding bid size should give positive OFI, got %d", e)
	}
}
