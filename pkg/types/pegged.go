package types

import "github.com/shopspring/decimal"

// PegReference is the market price a pegged order tracks.
type PegReference string

const (
	PegToBid PegReference = "BID" // best bid
	PegToAsk PegReference = "ASK" // best ask
	PegToMid PegReference = "MID" // mid price
)

// PeggedOrder is a limit order whose price is derived from a market reference
// plus a signed offset at the moment it is submitted (price = reference +
// Offset). This is a static peg — it resolves once on entry; continuously
// re-pegging as the book moves is a future extension (docs/SPEC.md §5.1).
type PeggedOrder struct {
	Order  *Order
	Ref    PegReference
	Offset decimal.Decimal
}

// NewPeggedOrder wraps a limit order with a peg reference and offset.
func NewPeggedOrder(order *Order, ref PegReference, offset decimal.Decimal) (*PeggedOrder, error) {
	if order == nil {
		return nil, ErrNilOrder
	}
	switch ref {
	case PegToBid, PegToAsk, PegToMid:
	default:
		return nil, ErrInvalidPegReference
	}
	return &PeggedOrder{Order: order, Ref: ref, Offset: offset}, nil
}
