package types

// TrailingStop is a stop whose trigger price follows the market by a fixed Trail
// distance, ratcheting in the favourable direction and never retreating. A SELL
// trailing stop (protecting a long) trails Trail below the highest price seen
// and fires when price falls to it; a BUY trailing stop (protecting a short)
// trails Trail above the lowest price seen (docs/SPEC.md §5.1). Trail and all
// tracked prices are in integer ticks.
type TrailingStop struct {
	Order       *Order
	Trail       int64 // ticks
	extreme     int64 // best price seen (max for sell, min for buy)
	stopPrice   int64
	initialized bool
	triggered   bool
}

// NewTrailingStop wraps order with a positive trail distance (in ticks).
func NewTrailingStop(order *Order, trail int64) (*TrailingStop, error) {
	if order == nil {
		return nil, ErrNilOrder
	}
	if trail <= 0 {
		return nil, ErrInvalidStopPrice
	}
	return &TrailingStop{Order: order, Trail: trail}, nil
}

// Observe updates the trailing extreme and stop price from a new market price.
// The extreme only moves favourably, so the stop only ratchets.
func (ts *TrailingStop) Observe(marketPrice int64) {
	if !ts.initialized {
		ts.extreme = marketPrice
		ts.initialized = true
	} else if ts.Order.Side == SideSell {
		if marketPrice > ts.extreme {
			ts.extreme = marketPrice
		}
	} else if marketPrice < ts.extreme {
		ts.extreme = marketPrice
	}

	if ts.Order.Side == SideSell {
		ts.stopPrice = ts.extreme - ts.Trail
	} else {
		ts.stopPrice = ts.extreme + ts.Trail
	}
}

// ShouldTrigger reports whether marketPrice has reached the trailed stop.
func (ts *TrailingStop) ShouldTrigger(marketPrice int64) bool {
	if ts.triggered || !ts.initialized {
		return false
	}
	if ts.Order.Side == SideSell {
		return marketPrice <= ts.stopPrice
	}
	return marketPrice >= ts.stopPrice
}

// Trigger fires the trailing stop, releasing its underlying order.
func (ts *TrailingStop) Trigger() {
	ts.triggered = true
	ts.Order.Status = OrderStatusNew
}

// IsTriggered reports whether the trailing stop has fired.
func (ts *TrailingStop) IsTriggered() bool { return ts.triggered }

// StopPrice returns the current trailed trigger price (in ticks).
func (ts *TrailingStop) StopPrice() int64 { return ts.stopPrice }

// TrailingState is the encodable form of a trailing stop's ratchet, so a snapshot
// can restore one exactly rather than restarting its extreme from the next
// observed price — which would hand back distance the stop had already locked in.
type TrailingState struct {
	Extreme     int64
	StopPrice   int64
	Initialized bool
	Triggered   bool
}

// State captures the ratchet for snapshotting.
func (ts *TrailingStop) State() TrailingState {
	return TrailingState{
		Extreme:     ts.extreme,
		StopPrice:   ts.stopPrice,
		Initialized: ts.initialized,
		Triggered:   ts.triggered,
	}
}

// RestoreState reinstates a ratchet captured by State. Intended for snapshot
// restore; it deliberately bypasses Observe, since replaying the extreme through
// Observe would require the price history that produced it.
func (ts *TrailingStop) RestoreState(s TrailingState) {
	ts.extreme = s.Extreme
	ts.stopPrice = s.StopPrice
	ts.initialized = s.Initialized
	ts.triggered = s.Triggered
}
