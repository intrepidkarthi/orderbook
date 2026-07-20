package types

import "errors"

// Validation errors raised when constructing orders.
var (
	ErrInvalidPrice       = errors.New("invalid price: must be positive")
	ErrInvalidQuantity    = errors.New("invalid quantity: must be positive")
	ErrInvalidSide        = errors.New("invalid order side")
	ErrInvalidOrderType   = errors.New("invalid order type")
	ErrInvalidTimeInForce = errors.New("invalid time in force")
)

// Lifecycle / matching errors.
var (
	ErrFillExceedsRemaining   = errors.New("fill quantity exceeds remaining quantity")
	ErrOrderNotFound          = errors.New("order not found")
	ErrOrderNotActive         = errors.New("order is not active")
	ErrOrderBookFull          = errors.New("order book has reached maximum capacity")
	ErrMarketOrderNoLiquidity = errors.New("market order cannot be filled: no liquidity")
	ErrFOKCannotFill          = errors.New("FOK order cannot be fully filled")
	ErrSelfTradeNotAllowed    = errors.New("self-trade not allowed")
)
