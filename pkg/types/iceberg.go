package types

import "github.com/shopspring/decimal"

// IcebergOrder shows only a small slice of a large order to the book at a time.
// The visible slice (Order) rests like an ordinary limit; when it is fully
// consumed the order refills the next slice from its hidden reserve, until the
// total quantity is worked off. This is how large participants avoid signalling
// their full size (docs/SPEC.md §5.1).
type IcebergOrder struct {
	Order      *Order          // the currently displayed slice (Quantity == a display chunk)
	DisplayQty decimal.Decimal // size shown per slice
	Hidden     decimal.Decimal // quantity not yet displayed
}

// NewIcebergOrder wraps order (whose Quantity is the TOTAL size) so that only
// displayQty is visible at a time. displayQty must be positive and no larger
// than the total.
func NewIcebergOrder(order *Order, displayQty decimal.Decimal) (*IcebergOrder, error) {
	if order == nil {
		return nil, ErrNilOrder
	}
	if displayQty.LessThanOrEqual(decimal.Zero) {
		return nil, ErrInvalidQuantity
	}
	if displayQty.GreaterThan(order.Quantity) {
		return nil, ErrInvalidDisplayQuantity
	}

	hidden := order.Quantity.Sub(displayQty)
	// Shrink the visible slice to the display size.
	order.Quantity = displayQty
	order.RemainingQty = displayQty
	order.FilledQty = decimal.Zero
	order.Status = OrderStatusNew

	return &IcebergOrder{Order: order, DisplayQty: displayQty, Hidden: hidden}, nil
}

// Refill loads the next slice into Order (resetting its fill state to the new
// chunk size) and returns true. It returns false when nothing is hidden.
func (ib *IcebergOrder) Refill() bool {
	if ib.Hidden.LessThanOrEqual(decimal.Zero) {
		return false
	}
	next := decimal.Min(ib.DisplayQty, ib.Hidden)
	ib.Hidden = ib.Hidden.Sub(next)
	ib.Order.Quantity = next
	ib.Order.RemainingQty = next
	ib.Order.FilledQty = decimal.Zero
	ib.Order.Status = OrderStatusNew
	return true
}

// IsFullyFilled reports whether both the hidden reserve and the visible slice
// are exhausted.
func (ib *IcebergOrder) IsFullyFilled() bool {
	return ib.Hidden.LessThanOrEqual(decimal.Zero) && ib.Order.IsFilled()
}

// TotalRemaining is the hidden reserve plus the unfilled part of the visible
// slice.
func (ib *IcebergOrder) TotalRemaining() decimal.Decimal {
	return ib.Hidden.Add(ib.Order.RemainingQty)
}
