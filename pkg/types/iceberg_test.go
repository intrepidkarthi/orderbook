package types

import (
	"errors"
	"testing"
)

func icebergUnderlying(t *testing.T, totalQty int64) *Order {
	t.Helper()
	return mustOrder(t, SideBuy, OrderTypeLimit, 100, totalQty, TIFGoodTillCancel)
}

func TestNewIcebergOrder_Validation(t *testing.T) {
	if _, err := NewIcebergOrder(nil, 1); !errors.Is(err, ErrNilOrder) {
		t.Errorf("nil err = %v, want ErrNilOrder", err)
	}
	if _, err := NewIcebergOrder(icebergUnderlying(t, 10), 0); !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("zero display err = %v, want ErrInvalidQuantity", err)
	}
	if _, err := NewIcebergOrder(icebergUnderlying(t, 10), 11); !errors.Is(err, ErrInvalidDisplayQuantity) {
		t.Errorf("oversized display err = %v, want ErrInvalidDisplayQuantity", err)
	}
}

func TestNewIcebergOrder_ShrinksVisibleSlice(t *testing.T) {
	ib, err := NewIcebergOrder(icebergUnderlying(t, 10), 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	if ib.Order.Quantity != 3 || ib.Order.RemainingQty != 3 {
		t.Errorf("visible slice = %d (rem %d), want 3", ib.Order.Quantity, ib.Order.RemainingQty)
	}
	if ib.Hidden != 7 {
		t.Errorf("hidden = %d, want 7", ib.Hidden)
	}
	if ib.TotalRemaining() != 10 {
		t.Errorf("total remaining = %d, want 10", ib.TotalRemaining())
	}
}

func TestIceberg_RefillCycle(t *testing.T) {
	ib, _ := NewIcebergOrder(icebergUnderlying(t, 10), 3)

	var consumed int64
	// Work off the whole 10 in slices of 3, 3, 3, 1.
	wantSlices := []int64{3, 3, 3, 1}
	for i, want := range wantSlices {
		if ib.Order.RemainingQty != want {
			t.Fatalf("slice %d visible = %d, want %d", i, ib.Order.RemainingQty, want)
		}
		if err := ib.Order.Fill(ib.Order.RemainingQty); err != nil {
			t.Fatalf("fill slice %d: %v", i, err)
		}
		consumed += want
		refilled := ib.Refill()
		if i < len(wantSlices)-1 && !refilled {
			t.Fatalf("expected refill after slice %d", i)
		}
		if i == len(wantSlices)-1 && refilled {
			t.Fatalf("should not refill after the last slice")
		}
	}
	if consumed != 10 {
		t.Errorf("consumed = %d, want 10", consumed)
	}
	if !ib.IsFullyFilled() {
		t.Error("iceberg should be fully filled after working off the total")
	}
}
