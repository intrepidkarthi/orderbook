package types

import (
	"errors"
	"testing"
)

func icebergUnderlying(t *testing.T, totalQty int64) *Order {
	t.Helper()
	return mustOrder(t, SideBuy, OrderTypeLimit, 100, totalQty, TIFGoodTillCancel)
}

// TestIceberg_JitteredRefill: with JitterBps set, refilled slices vary in size
// (defeating a fixed-reload fingerprint) yet the total is conserved and the
// sequence is deterministic in the order id — so replay is exact.
func TestIceberg_JitteredRefill(t *testing.T) {
	build := func(id int64) *IcebergOrder {
		o := icebergUnderlying(t, 100)
		o.ID = id
		ib, err := NewIcebergOrder(o, 10)
		if err != nil {
			t.Fatalf("NewIcebergOrder: %v", err)
		}
		ib.JitterBps = 3000 // ±30%
		return ib
	}
	collect := func(ib *IcebergOrder) []int64 {
		slices := []int64{ib.Order.RemainingQty} // the initial visible slice
		for ib.Refill() {
			slices = append(slices, ib.Order.RemainingQty)
		}
		return slices
	}

	a := collect(build(7))

	var sum int64
	for _, s := range a {
		sum += s
	}
	if sum != 100 {
		t.Errorf("slices must conserve the total: sum=%d want 100 (%v)", sum, a)
	}
	// Determinism: the same id reproduces the same slice sequence.
	if b := collect(build(7)); !equalInts(a, b) {
		t.Errorf("same id must yield the same slices:\n a=%v\n b=%v", a, b)
	}
	// Variation: at least one refilled slice differs from the fixed display size.
	varied := false
	for _, s := range a[1:] {
		if s != 10 {
			varied = true
		}
		if s > 13 { // 10 + 30%
			t.Errorf("refilled slice %d exceeds the jitter ceiling 13", s)
		}
	}
	if !varied {
		t.Error("jittered refills should vary from the display size")
	}
	// A different id generally yields a different pattern.
	if equalInts(a, collect(build(999))) {
		t.Error("different order ids should generally yield different slice patterns")
	}
}

func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
