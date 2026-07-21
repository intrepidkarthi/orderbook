package types

import "github.com/shopspring/decimal"

// StopOrder is a trigger wrapper around an ordinary order. While resting it sits
// off-book; when the market price reaches StopPrice the underlying Order (a
// market order for a stop, or a limit order for a stop-limit) is released to the
// matching engine.
//
// Direction follows the underlying side: a BUY stop triggers when the market
// rises to the stop (price ≥ StopPrice); a SELL stop triggers when the market
// falls to it (price ≤ StopPrice). A sell stop below the market is the classic
// stop-loss for a long position.
type StopOrder struct {
	Order     *Order
	StopPrice decimal.Decimal
	triggered bool
}

// NewStopOrder wraps order with a positive stop price.
func NewStopOrder(order *Order, stopPrice decimal.Decimal) (*StopOrder, error) {
	if order == nil {
		return nil, ErrNilOrder
	}
	if stopPrice.LessThanOrEqual(decimal.Zero) {
		return nil, ErrInvalidStopPrice
	}
	return &StopOrder{Order: order, StopPrice: stopPrice}, nil
}

// ShouldTrigger reports whether marketPrice has reached the stop (and it has not
// already fired).
func (s *StopOrder) ShouldTrigger(marketPrice decimal.Decimal) bool {
	if s.triggered {
		return false
	}
	if s.Order.Side == SideBuy {
		return marketPrice.GreaterThanOrEqual(s.StopPrice)
	}
	return marketPrice.LessThanOrEqual(s.StopPrice)
}

// Trigger marks the stop as fired and resets the underlying order to NEW so the
// engine can process it.
func (s *StopOrder) Trigger() {
	s.triggered = true
	s.Order.Status = OrderStatusNew
}

// IsTriggered reports whether the stop has fired.
func (s *StopOrder) IsTriggered() bool { return s.triggered }
