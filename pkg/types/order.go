// Package types defines the core domain values shared across the order book and
// matching engine: orders, trades, and the small set of errors they raise.
//
// Prices and quantities are exact decimals (shopspring/decimal) — never floats —
// so that money arithmetic is precise. This is the single most important
// correctness decision in the project; see docs/SPEC.md §6.1.
package types

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

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
)

// Order is a single instruction to buy or sell a quantity of a symbol.
//
// RemainingQty is the authoritative "how much is still live" figure the matching
// engine reads and mutates; FilledQty + RemainingQty always equals Quantity.
type Order struct {
	ID           string          `json:"id"`
	UserID       string          `json:"user_id"`
	Symbol       string          `json:"symbol"`
	Side         Side            `json:"side"`
	Type         OrderType       `json:"type"`
	Price        decimal.Decimal `json:"price"`
	Quantity     decimal.Decimal `json:"quantity"`
	FilledQty    decimal.Decimal `json:"filled_qty"`
	RemainingQty decimal.Decimal `json:"remaining_qty"`
	TimeInForce  TimeInForce     `json:"time_in_force"`
	Status       OrderStatus     `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	SequenceNum  uint64          `json:"sequence_num"`
}

// NewOrder constructs a validated order with a time-ordered UUIDv7 id.
//
// Quantity must be positive. Limit orders require a positive price; market
// orders ignore price (it is forced to zero, since a market order takes
// whatever the book offers).
func NewOrder(userID, symbol string, side Side, orderType OrderType, price, quantity decimal.Decimal, tif TimeInForce) (*Order, error) {
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

	if quantity.LessThanOrEqual(decimal.Zero) {
		return nil, ErrInvalidQuantity
	}

	if orderType == OrderTypeMarket {
		price = decimal.Zero
	} else if price.LessThanOrEqual(decimal.Zero) {
		return nil, ErrInvalidPrice
	}

	now := time.Now().UTC()
	return &Order{
		ID:           uuid.Must(uuid.NewV7()).String(),
		UserID:       userID,
		Symbol:       symbol,
		Side:         side,
		Type:         orderType,
		Price:        price,
		Quantity:     quantity,
		FilledQty:    decimal.Zero,
		RemainingQty: quantity,
		TimeInForce:  tif,
		Status:       OrderStatusNew,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// Fill applies a fill of qty to the order, advancing FilledQty/RemainingQty and
// Status. It returns an error if qty is non-positive or exceeds RemainingQty.
func (o *Order) Fill(qty decimal.Decimal) error {
	if qty.LessThanOrEqual(decimal.Zero) {
		return ErrInvalidQuantity
	}
	if qty.GreaterThan(o.RemainingQty) {
		return ErrFillExceedsRemaining
	}

	o.FilledQty = o.FilledQty.Add(qty)
	o.RemainingQty = o.RemainingQty.Sub(qty)
	o.UpdatedAt = time.Now().UTC()

	if o.RemainingQty.IsZero() {
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
func (o *Order) IsFilled() bool { return o.RemainingQty.IsZero() }

// IsActive reports whether the order can still be matched or cancelled.
func (o *Order) IsActive() bool {
	return o.Status == OrderStatusNew || o.Status == OrderStatusPartiallyFilled
}

// NotionalValue is Price × Quantity (zero for market orders, which have no price).
func (o *Order) NotionalValue() decimal.Decimal { return o.Price.Mul(o.Quantity) }

// RemainingValue is Price × RemainingQty.
func (o *Order) RemainingValue() decimal.Decimal { return o.Price.Mul(o.RemainingQty) }

// Fresh returns a copy of the order reset to its initial, unfilled state
// (FilledQty=0, RemainingQty=Quantity, Status=NEW, SequenceNum=0) while keeping
// its identity and parameters (ID, user, symbol, side, type, price, quantity,
// TIF). It exists for deterministic replay and simulation, where the same order
// must be re-submitted to a fresh engine without carrying prior fill state. The
// original order is not modified.
func (o *Order) Fresh() *Order {
	c := *o
	c.FilledQty = decimal.Zero
	c.RemainingQty = o.Quantity
	c.Status = OrderStatusNew
	c.SequenceNum = 0
	c.UpdatedAt = o.CreatedAt
	return &c
}
