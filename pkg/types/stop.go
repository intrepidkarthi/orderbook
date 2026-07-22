package types

// StopOrder is a trigger wrapper around an ordinary order. While resting it sits
// off-book; when the market price reaches StopPrice the underlying Order (a
// market order for a stop, or a limit order for a stop-limit) is released to the
// matching engine.
//
// Direction follows the underlying side: a BUY stop triggers when the market
// rises to the stop (price ≥ StopPrice); a SELL stop triggers when the market
// falls to it (price ≤ StopPrice). A sell stop below the market is the classic
// stop-loss for a long position. StopPrice is in integer ticks.
type StopOrder struct {
	Order     *Order
	StopPrice int64 // ticks
	triggered bool
}

// NewStopOrder wraps order with a positive stop price (in ticks).
func NewStopOrder(order *Order, stopPrice int64) (*StopOrder, error) {
	if order == nil {
		return nil, ErrNilOrder
	}
	if stopPrice <= 0 {
		return nil, ErrInvalidStopPrice
	}
	return &StopOrder{Order: order, StopPrice: stopPrice}, nil
}

// ShouldTrigger reports whether marketPrice has reached the stop (and it has not
// already fired).
func (s *StopOrder) ShouldTrigger(marketPrice int64) bool {
	if s.triggered {
		return false
	}
	if s.Order.Side == SideBuy {
		return marketPrice >= s.StopPrice
	}
	return marketPrice <= s.StopPrice
}

// Trigger marks the stop as fired and resets the underlying order to NEW so the
// engine can process it.
func (s *StopOrder) Trigger() {
	s.triggered = true
	s.Order.Status = OrderStatusNew
}

// IsTriggered reports whether the stop has fired.
func (s *StopOrder) IsTriggered() bool { return s.triggered }
