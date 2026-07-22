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
	ErrFillExceedsRemaining    = errors.New("fill quantity exceeds remaining quantity")
	ErrOrderNotFound           = errors.New("order not found")
	ErrOrderNotActive          = errors.New("order is not active")
	ErrOrderBookFull           = errors.New("order book has reached maximum capacity")
	ErrMarketOrderNoLiquidity  = errors.New("market order cannot be filled: no liquidity")
	ErrFOKCannotFill           = errors.New("FOK order cannot be fully filled")
	ErrSelfTradeNotAllowed     = errors.New("self-trade not allowed")
	ErrNilOrder                = errors.New("order must not be nil")
	ErrInvalidStopPrice        = errors.New("invalid stop price: must be positive")
	ErrPostOnlyWouldCross      = errors.New("post-only order would cross the spread")
	ErrInvalidDisplayQuantity  = errors.New("display quantity must be positive and <= total quantity")
	ErrInvalidPegReference     = errors.New("invalid peg reference")
	ErrPegReferenceUnavailable = errors.New("peg reference price is unavailable")
	ErrTradingHalted           = errors.New("trading is halted")
	ErrPriceOutsideBand        = errors.New("price is outside the allowed band")
	ErrOrderTypeDisabled       = errors.New("order type is disabled")
	ErrNewOrdersHalted         = errors.New("engine is cancel-only: new liquidity is not accepted")
)
