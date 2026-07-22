package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func iceberg(t *testing.T, user string, side types.Side, price, total, display int64) *types.IcebergOrder {
	t.Helper()
	ib, err := types.NewIcebergOrder(lim(t, user, side, price, total), display)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	return ib
}

func TestIceberg_ShowsOnlyDisplaySlice(t *testing.T) {
	e := newEngine()
	e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 99, 10, 3))

	// The book shows only the display slice (3), not the full 10.
	if _, qty, ok := e.BestBid(); !ok || qty != 3 {
		t.Errorf("visible best bid qty = %d, want 3 (rest hidden)", qty)
	}
}

func TestIceberg_RefillsAsConsumed(t *testing.T) {
	e := newEngine()
	e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 99, 10, 3))

	// Consume three display slices (9 total); each refill re-shows 3, 3, then 1.
	for range 3 {
		e.Process(marketOrder(t, "taker", types.SideSell, 3))
	}
	// After 9 consumed, only the final hidden unit (1) remains visible.
	if _, qty, ok := e.BestBid(); !ok || qty != 1 {
		t.Errorf("visible qty after 9 consumed = %d, want 1", qty)
	}
	// Consume the last unit; the iceberg is now fully worked off.
	e.Process(marketOrder(t, "taker", types.SideSell, 1))
	if _, _, ok := e.BestBid(); ok {
		t.Error("book should be empty after the iceberg is fully consumed")
	}
}

func TestIceberg_ImmediateCrossRefills(t *testing.T) {
	e := newEngine()
	// Resting asks: 99×3, 100×3, 101×3 (9 available).
	e.Process(lim(t, "a99", types.SideSell, 99, 3))
	e.Process(lim(t, "a100", types.SideSell, 100, 3))
	e.Process(lim(t, "a101", types.SideSell, 101, 3))

	// An aggressive iceberg buy for 8 (display 3) sweeps 99, 100, and 2 of 101.
	res := e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 101, 8, 3))
	if res.Status != types.OrderStatusFilled {
		t.Fatalf("status = %q, want FILLED", res.Status)
	}
	var total int64
	for _, tr := range res.Trades {
		total += tr.Quantity
	}
	if total != 8 {
		t.Errorf("iceberg bought %d, want 8", total)
	}
	// 1 unit of the 101 ask remains.
	if ask, qty, ok := e.BestAsk(); !ok || ask != 101 || qty != 1 {
		t.Errorf("remaining ask = %d x %d, want 101 x 1", ask, qty)
	}
}
