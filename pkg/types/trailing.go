package types

import "github.com/shopspring/decimal"

// TrailingStop is a stop whose trigger price follows the market by a fixed Trail
// distance, ratcheting in the favourable direction and never retreating. A SELL
// trailing stop (protecting a long) trails Trail below the highest price seen
// and fires when price falls to it; a BUY trailing stop (protecting a short)
// trails Trail above the lowest price seen (docs/SPEC.md §5.1).
type TrailingStop struct {
	Order       *Order
	Trail       decimal.Decimal
	extreme     decimal.Decimal // best price seen (max for sell, min for buy)
	stopPrice   decimal.Decimal
	initialized bool
	triggered   bool
}

// NewTrailingStop wraps order with a positive trail distance.
func NewTrailingStop(order *Order, trail decimal.Decimal) (*TrailingStop, error) {
	if order == nil {
		return nil, ErrNilOrder
	}
	if trail.LessThanOrEqual(decimal.Zero) {
		return nil, ErrInvalidStopPrice
	}
	return &TrailingStop{Order: order, Trail: trail}, nil
}

// Observe updates the trailing extreme and stop price from a new market price.
// The extreme only moves favourably, so the stop only ratchets.
func (ts *TrailingStop) Observe(marketPrice decimal.Decimal) {
	if !ts.initialized {
		ts.extreme = marketPrice
		ts.initialized = true
	} else if ts.Order.Side == SideSell {
		if marketPrice.GreaterThan(ts.extreme) {
			ts.extreme = marketPrice
		}
	} else if marketPrice.LessThan(ts.extreme) {
		ts.extreme = marketPrice
	}

	if ts.Order.Side == SideSell {
		ts.stopPrice = ts.extreme.Sub(ts.Trail)
	} else {
		ts.stopPrice = ts.extreme.Add(ts.Trail)
	}
}

// ShouldTrigger reports whether marketPrice has reached the trailed stop.
func (ts *TrailingStop) ShouldTrigger(marketPrice decimal.Decimal) bool {
	if ts.triggered || !ts.initialized {
		return false
	}
	if ts.Order.Side == SideSell {
		return marketPrice.LessThanOrEqual(ts.stopPrice)
	}
	return marketPrice.GreaterThanOrEqual(ts.stopPrice)
}

// Trigger fires the trailing stop, releasing its underlying order.
func (ts *TrailingStop) Trigger() {
	ts.triggered = true
	ts.Order.Status = OrderStatusNew
}

// IsTriggered reports whether the trailing stop has fired.
func (ts *TrailingStop) IsTriggered() bool { return ts.triggered }

// StopPrice returns the current trailed trigger price.
func (ts *TrailingStop) StopPrice() decimal.Decimal { return ts.stopPrice }
