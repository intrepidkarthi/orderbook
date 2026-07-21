package types

import (
	"errors"
	"testing"
)

func icebergUnderlying(t *testing.T, totalQty string) *Order {
	t.Helper()
	return mustOrder(t, SideBuy, OrderTypeLimit, "100", totalQty, TIFGoodTillCancel)
}

func TestNewIcebergOrder_Validation(t *testing.T) {
	if _, err := NewIcebergOrder(nil, dec("1")); !errors.Is(err, ErrNilOrder) {
		t.Errorf("nil err = %v, want ErrNilOrder", err)
	}
	if _, err := NewIcebergOrder(icebergUnderlying(t, "10"), dec("0")); !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("zero display err = %v, want ErrInvalidQuantity", err)
	}
	if _, err := NewIcebergOrder(icebergUnderlying(t, "10"), dec("11")); !errors.Is(err, ErrInvalidDisplayQuantity) {
		t.Errorf("oversized display err = %v, want ErrInvalidDisplayQuantity", err)
	}
}

func TestNewIcebergOrder_ShrinksVisibleSlice(t *testing.T) {
	ib, err := NewIcebergOrder(icebergUnderlying(t, "10"), dec("3"))
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	if !ib.Order.Quantity.Equal(dec("3")) || !ib.Order.RemainingQty.Equal(dec("3")) {
		t.Errorf("visible slice = %s (rem %s), want 3", ib.Order.Quantity, ib.Order.RemainingQty)
	}
	if !ib.Hidden.Equal(dec("7")) {
		t.Errorf("hidden = %s, want 7", ib.Hidden)
	}
	if !ib.TotalRemaining().Equal(dec("10")) {
		t.Errorf("total remaining = %s, want 10", ib.TotalRemaining())
	}
}

func TestIceberg_RefillCycle(t *testing.T) {
	ib, _ := NewIcebergOrder(icebergUnderlying(t, "10"), dec("3"))

	consumed := dec("0")
	// Work off the whole 10 in slices of 3, 3, 3, 1.
	wantSlices := []string{"3", "3", "3", "1"}
	for i, want := range wantSlices {
		if !ib.Order.RemainingQty.Equal(dec(want)) {
			t.Fatalf("slice %d visible = %s, want %s", i, ib.Order.RemainingQty, want)
		}
		if err := ib.Order.Fill(ib.Order.RemainingQty); err != nil {
			t.Fatalf("fill slice %d: %v", i, err)
		}
		consumed = consumed.Add(dec(want))
		refilled := ib.Refill()
		if i < len(wantSlices)-1 && !refilled {
			t.Fatalf("expected refill after slice %d", i)
		}
		if i == len(wantSlices)-1 && refilled {
			t.Fatalf("should not refill after the last slice")
		}
	}
	if !consumed.Equal(dec("10")) {
		t.Errorf("consumed = %s, want 10", consumed)
	}
	if !ib.IsFullyFilled() {
		t.Error("iceberg should be fully filled after working off the total")
	}
}
