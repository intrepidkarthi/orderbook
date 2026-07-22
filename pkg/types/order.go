// Package types defines the core domain values shared across the order book and
// matching engine: orders, trades, and the small set of errors they raise.
//
// Prices and quantities are integer ticks and lots (int64) — exact and fast, the
// production choice. Human decimals are converted at the API boundary by an
// Instrument (see instrument.go); the hot path never touches decimal or floats.
// This is the single most important representation decision in the project; see
// docs/SPEC.md §6.1.
package types

import "time"

// Side is the direction of an order.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderType is how an order interacts with the book.
type OrderType string

const (
	// OrderTypeLimit rests at a price (or better) until filled or cancelled.
	OrderTypeLimit OrderType = "LIMIT"
	// OrderTypeMarket takes liquidity immediately at the best available prices.
	OrderTypeMarket OrderType = "MARKET"
)

// TimeInForce controls how long an order remains active.
type TimeInForce string

const (
	// TIFGoodTillCancel rests until explicitly cancelled.
	TIFGoodTillCancel TimeInForce = "GTC"
	// TIFImmediateOrCancel fills what it can immediately, cancels the rest.
	TIFImmediateOrCancel TimeInForce = "IOC"
	// TIFFillOrKill fills the entire order immediately or cancels all of it.
	TIFFillOrKill TimeInForce = "FOK"
)

// OrderStatus is the lifecycle state of an order.
type OrderStatus string

const (
	OrderStatusNew             OrderStatus = "NEW"
	OrderStatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled          OrderStatus = "FILLED"
	OrderStatusCancelled       OrderStatus = "CANCELLED"
	OrderStatusRejected        OrderStatus = "REJECTED"
	OrderStatusPendingTrigger  OrderStatus = "PENDING_TRIGGER" // resting stop, not yet fired
)

// Order is a single instruction to buy or sell a quantity of a symbol.
//
// Price is in integer ticks (0 for market orders); Quantity, FilledQty and
// RemainingQty are in integer lots. RemainingQty is the authoritative "how much
// is still live" figure the matching engine reads and mutates; FilledQty +
// RemainingQty always equals Quantity.
//
// ID is a monotonic int64 assigned on entry to the engine (or the book) — unique,
// ordered, and deterministic. It doubles as the price-time sequence number.
// ClientOrderID is an optional caller-supplied correlation string.
type Order struct {
	ID            int64       `json:"id"`
	ClientOrderID string      `json:"client_order_id,omitempty"`
	UserID        string      `json:"user_id"`
	Symbol        string      `json:"symbol"`
	Side          Side        `json:"side"`
	Type          OrderType   `json:"type"`
	Price         int64       `json:"price"` // ticks
	Quantity      int64       `json:"quantity"`
	FilledQty     int64       `json:"filled_qty"`
	RemainingQty  int64       `json:"remaining_qty"`
	TimeInForce   TimeInForce `json:"time_in_force"`
	PostOnly      bool        `json:"post_only,omitempty"` // maker-only: reject if it would cross
	Status        OrderStatus `json:"status"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// AsPostOnly marks the order maker-only and returns it (for fluent construction).
// A post-only order is rejected if it would cross the spread instead of resting.
func (o *Order) AsPostOnly() *Order {
	o.PostOnly = true
	return o
}

// NewOrder constructs a validated order from integer ticks/lots. The ID is left
// zero and assigned on entry to the engine/book.
//
// Quantity must be positive. Limit orders require a positive price; market orders
// ignore price (it is forced to zero, since a market order takes whatever the
// book offers).
func NewOrder(userID, symbol string, side Side, orderType OrderType, price, quantity int64, tif TimeInForce) (*Order, error) {
	switch side {
	case SideBuy, SideSell:
	default:
		return nil, ErrInvalidSide
	}

	switch orderType {
	case OrderTypeLimit, OrderTypeMarket:
	default:
		return nil, ErrInvalidOrderType
	}

	switch tif {
	case TIFGoodTillCancel, TIFImmediateOrCancel, TIFFillOrKill:
	default:
		return nil, ErrInvalidTimeInForce
	}

	if quantity <= 0 {
		return nil, ErrInvalidQuantity
	}

	if orderType == OrderTypeMarket {
		price = 0
	} else if price <= 0 {
		return nil, ErrInvalidPrice
	}

	now := time.Now().UTC()
	return &Order{
		UserID:       userID,
		Symbol:       symbol,
		Side:         side,
		Type:         orderType,
		Price:        price,
		Quantity:     quantity,
		FilledQty:    0,
		RemainingQty: quantity,
		TimeInForce:  tif,
		Status:       OrderStatusNew,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// Fill applies a fill of qty to the order, advancing FilledQty/RemainingQty and
// Status. It returns an error if qty is non-positive or exceeds RemainingQty.
func (o *Order) Fill(qty int64) error {
	if qty <= 0 {
		return ErrInvalidQuantity
	}
	if qty > o.RemainingQty {
		return ErrFillExceedsRemaining
	}

	o.FilledQty += qty
	o.RemainingQty -= qty
	o.UpdatedAt = time.Now().UTC()

	if o.RemainingQty == 0 {
		o.Status = OrderStatusFilled
	} else {
		o.Status = OrderStatusPartiallyFilled
	}
	return nil
}

// Cancel marks the order cancelled. Filled or already-cancelled orders cannot
// be cancelled.
func (o *Order) Cancel() error {
	if o.Status == OrderStatusFilled {
		return ErrOrderNotActive
	}
	if o.Status == OrderStatusCancelled {
		return ErrOrderNotActive
	}
	o.Status = OrderStatusCancelled
	o.UpdatedAt = time.Now().UTC()
	return nil
}

// IsFilled reports whether the order has no remaining quantity.
func (o *Order) IsFilled() bool { return o.RemainingQty == 0 }

// IsActive reports whether the order can still be matched or cancelled.
func (o *Order) IsActive() bool {
	return o.Status == OrderStatusNew || o.Status == OrderStatusPartiallyFilled
}

// NotionalValue is Price × Quantity in tick·lot units (zero for market orders).
func (o *Order) NotionalValue() int64 { return o.Price * o.Quantity }

// RemainingValue is Price × RemainingQty in tick·lot units.
func (o *Order) RemainingValue() int64 { return o.Price * o.RemainingQty }

// Fresh returns a copy of the order reset to its initial, unfilled state
// (FilledQty=0, RemainingQty=Quantity, Status=NEW, ID=0) while keeping its
// parameters (user, symbol, side, type, price, quantity, TIF). It exists for
// deterministic replay and simulation, where the same order must be re-submitted
// to a fresh engine without carrying prior fill state or its assigned ID. The
// original order is not modified.
func (o *Order) Fresh() *Order {
	c := *o
	c.FilledQty = 0
	c.RemainingQty = o.Quantity
	c.Status = OrderStatusNew
	c.ID = 0
	c.UpdatedAt = o.CreatedAt
	return &c
}
