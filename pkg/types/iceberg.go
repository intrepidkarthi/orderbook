package types

// IcebergOrder shows only a small slice of a large order to the book at a time.
// The visible slice (Order) rests like an ordinary limit; when it is fully
// consumed the order refills the next slice from its hidden reserve, until the
// total quantity is worked off. This is how large participants avoid signalling
// their full size (docs/SPEC.md §5.1). DisplayQty/Hidden are in integer lots.
type IcebergOrder struct {
	Order      *Order // the currently displayed slice (Quantity == a display chunk)
	DisplayQty int64  // size shown per slice (lots)
	Hidden     int64  // quantity not yet displayed (lots)
	// JitterBps, when > 0, varies each *refilled* slice by up to ±JitterBps basis
	// points around DisplayQty, so a watcher cannot fingerprint the reserve by its
	// fixed reload size (iceberg detection / pinging). The variation is derived
	// deterministically from (Order.ID, refill count), so replay is exact.
	JitterBps int64
	refills   int64 // slices refilled so far — part of the jitter seed
}

// NewIcebergOrder wraps order (whose Quantity is the TOTAL size) so that only
// displayQty is visible at a time. displayQty must be positive and no larger
// than the total.
func NewIcebergOrder(order *Order, displayQty int64) (*IcebergOrder, error) {
	if order == nil {
		return nil, ErrNilOrder
	}
	if displayQty <= 0 {
		return nil, ErrInvalidQuantity
	}
	if displayQty > order.Quantity {
		return nil, ErrInvalidDisplayQuantity
	}

	hidden := order.Quantity - displayQty
	// Shrink the visible slice to the display size.
	order.Quantity = displayQty
	order.RemainingQty = displayQty
	order.FilledQty = 0
	order.Status = OrderStatusNew

	return &IcebergOrder{Order: order, DisplayQty: displayQty, Hidden: hidden}, nil
}

// Refill loads the next slice into Order (resetting its fill state to the new
// chunk size) and returns true. It returns false when nothing is hidden. When
// JitterBps is set the visible slice size is deterministically jittered so the
// reload is not a fixed, sniffable size.
func (ib *IcebergOrder) Refill() bool {
	if ib.Hidden <= 0 {
		return false
	}
	next := min(ib.DisplayQty, ib.Hidden)
	if ib.JitterBps > 0 {
		next = min(jitterSlice(ib.DisplayQty, ib.JitterBps, uint64(ib.Order.ID), uint64(ib.refills)), ib.Hidden)
	}
	ib.refills++
	ib.Hidden -= next
	ib.Order.Quantity = next
	ib.Order.RemainingQty = next
	ib.Order.FilledQty = 0
	ib.Order.Status = OrderStatusNew
	return true
}

// jitterSlice returns base scaled by a deterministic factor within ±bps basis
// points, seeded by (a, b) via a splitmix64 scramble — no global RNG, so it is
// replay-exact. The result is clamped to at least 1 lot.
func jitterSlice(base, bps int64, a, b uint64) int64 {
	z := a*0x9e3779b97f4a7c15 + b + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	delta := int64(z%uint64(2*bps+1)) - bps // basis points in [-bps, +bps]
	return max(base+base*delta/10000, 1)
}

// IsFullyFilled reports whether both the hidden reserve and the visible slice
// are exhausted.
func (ib *IcebergOrder) IsFullyFilled() bool {
	return ib.Hidden <= 0 && ib.Order.IsFilled()
}

// TotalRemaining is the hidden reserve plus the unfilled part of the visible
// slice.
func (ib *IcebergOrder) TotalRemaining() int64 {
	return ib.Hidden + ib.Order.RemainingQty
}
