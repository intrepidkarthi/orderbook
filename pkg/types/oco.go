package types

// OCOOrder links two orders so that completing one cancels the other. The
// canonical use is a bracket exit for an open position: a take-profit limit
// (Primary) paired with a stop-loss (Stop). Whichever fires first, the engine
// cancels its counterpart (docs/SPEC.md §5.1).
type OCOOrder struct {
	Primary *Order     // e.g. take-profit limit
	Stop    *StopOrder // e.g. stop-loss
}

// NewOCOOrder pairs a primary order with a stop order. Both must be non-nil.
func NewOCOOrder(primary *Order, stop *StopOrder) (*OCOOrder, error) {
	if primary == nil || stop == nil {
		return nil, ErrNilOrder
	}
	return &OCOOrder{Primary: primary, Stop: stop}, nil
}
