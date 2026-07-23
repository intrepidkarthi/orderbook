package auction

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func ord(t *testing.T, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder("u", "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestBatchAuction_ClearsAtUniformPrice: everything crossing the clearing price
// trades at that single price, maximising volume.
func TestBatchAuction_ClearsAtUniformPrice(t *testing.T) {
	b := NewBatchAuction("X")
	b.Add(ord(t, types.SideBuy, 105, 10))
	b.Add(ord(t, types.SideBuy, 100, 10))
	b.Add(ord(t, types.SideSell, 99, 10))
	b.Add(ord(t, types.SideSell, 100, 10))

	r := b.Cross()
	if !r.HasClearing || r.ClearingPrice != 100 || r.Volume != 20 {
		t.Fatalf("uncross = %+v, want clearing 100 volume 20", r)
	}
	var total int64
	for _, tr := range r.Trades {
		if tr.Price != 100 {
			t.Errorf("trade at %d, want the uniform clearing price 100", tr.Price)
		}
		total += tr.Quantity
	}
	if total != 20 {
		t.Errorf("executed %d, want 20", total)
	}
}

// TestBatchAuction_Priority: the more aggressive buy fills first when the buy side
// is over-subscribed at the clearing price.
func TestBatchAuction_Priority(t *testing.T) {
	b := NewBatchAuction("X")
	aggressive := ord(t, types.SideBuy, 110, 5)
	marginal := ord(t, types.SideBuy, 100, 5)
	b.Add(marginal)   // added first...
	b.Add(aggressive) // ...but higher price gets priority
	b.Add(ord(t, types.SideSell, 100, 5))

	r := b.Cross()
	if r.Volume != 5 {
		t.Fatalf("volume = %d, want 5", r.Volume)
	}
	if aggressive.RemainingQty != 0 {
		t.Errorf("aggressive buy (110) should fill first, rem=%d", aggressive.RemainingQty)
	}
	if marginal.RemainingQty != 5 {
		t.Errorf("marginal buy (100) should be unfilled, rem=%d", marginal.RemainingQty)
	}
}

// TestBatchAuction_NoCross: a disjoint book does not clear.
func TestBatchAuction_NoCross(t *testing.T) {
	b := NewBatchAuction("X")
	b.Add(ord(t, types.SideBuy, 99, 10))
	b.Add(ord(t, types.SideSell, 101, 10))
	if r := b.Cross(); r.HasClearing || len(r.Trades) != 0 {
		t.Errorf("disjoint batch should not clear, got %+v", r)
	}
}
