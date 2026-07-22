package types

import "github.com/shopspring/decimal"

// Instrument defines the price/size grid for a symbol and converts between human
// decimals (at the API boundary) and the integer ticks/lots the engine works in
// internally. Integers are exact and fast — the production choice — while
// decimals stay only at the edge for parsing and display.
type Instrument struct {
	Symbol   string
	TickSize decimal.Decimal // smallest price increment, e.g. 0.01
	LotSize  decimal.Decimal // smallest quantity increment, e.g. 0.001
}

// NewInstrument builds an instrument; zero tick/lot default to 1.
func NewInstrument(symbol string, tickSize, lotSize decimal.Decimal) Instrument {
	if tickSize.IsZero() {
		tickSize = decimal.NewFromInt(1)
	}
	if lotSize.IsZero() {
		lotSize = decimal.NewFromInt(1)
	}
	return Instrument{Symbol: symbol, TickSize: tickSize, LotSize: lotSize}
}

// PriceToTicks converts a decimal price to integer ticks (rounded to the grid).
func (in Instrument) PriceToTicks(p decimal.Decimal) int64 {
	return p.Div(in.TickSize).Round(0).IntPart()
}

// TicksToPrice converts integer ticks back to a decimal price.
func (in Instrument) TicksToPrice(t int64) decimal.Decimal {
	return in.TickSize.Mul(decimal.NewFromInt(t))
}

// QtyToLots converts a decimal quantity to integer lots (rounded to the grid).
func (in Instrument) QtyToLots(q decimal.Decimal) int64 {
	return q.Div(in.LotSize).Round(0).IntPart()
}

// LotsToQty converts integer lots back to a decimal quantity.
func (in Instrument) LotsToQty(l int64) decimal.Decimal {
	return in.LotSize.Mul(decimal.NewFromInt(l))
}

// NewOrder builds a validated order from decimal price/quantity, converting to
// ticks/lots at this instrument's grid — the ergonomic boundary constructor.
func (in Instrument) NewOrder(userID string, side Side, orderType OrderType, price, quantity decimal.Decimal, tif TimeInForce) (*Order, error) {
	var priceTicks int64
	if orderType != OrderTypeMarket {
		priceTicks = in.PriceToTicks(price)
	}
	return NewOrder(userID, in.Symbol, side, orderType, priceTicks, in.QtyToLots(quantity), tif)
}
