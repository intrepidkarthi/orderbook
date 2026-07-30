package orderentry

import (
	"errors"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Reason codes a client can branch on. These mirror internal/wire's numbering and
// are duplicated rather than imported because internal/ is not importable from a
// supported package — and because the wire numbering is frozen while the engine's
// error set is not.
const (
	ReasonNone           uint16 = 0
	ReasonOther          uint16 = 1
	ReasonUnknownOrder   uint16 = 2
	ReasonDuplicateClOrd uint16 = 3
	ReasonTooSmall       uint16 = 4
	ReasonTooLarge       uint16 = 5
	ReasonPriceBand      uint16 = 6
	ReasonSelfTrade      uint16 = 7
	ReasonPostOnlyCross  uint16 = 8
	ReasonFOKCannotFill  uint16 = 9
	ReasonHalted         uint16 = 10
	ReasonThrottled      uint16 = 11
	ReasonOverloaded     uint16 = 12
	ReasonNotAuthorised  uint16 = 13
	ReasonMalformed      uint16 = 14
	ReasonShuttingDown   uint16 = 15
	// ReasonInvalidQuantity refuses a size for an order that does exist — a
	// reduce that is not a reduction, or one below what is already filled. The
	// venue looked; the client's model of its own order is wrong.
	ReasonInvalidQuantity uint16 = 16
	// ReasonTooSoon refuses a withdrawal of displayed size before the venue's
	// minimum resting time has elapsed — the one refusal a client should retry.
	ReasonTooSoon uint16 = 17
)

// ReasonFor maps an engine error onto the wire vocabulary.
//
// The mapping is deliberately lossy and deliberately narrow. Exposing every
// sentinel in pkg/types would couple a frozen wire format to an internal error
// list, so that adding a sentinel — an ordinary, non-breaking change — would
// silently become a protocol change. Anything unrecognised maps to ReasonOther,
// which a client must already handle.
func ReasonFor(err error) uint16 {
	if err == nil {
		return ReasonNone
	}
	switch {
	case errors.Is(err, types.ErrOrderNotFound), errors.Is(err, types.ErrOrderNotActive):
		return ReasonUnknownOrder
	case errors.Is(err, types.ErrDuplicateClientOrderID):
		return ReasonDuplicateClOrd
	case errors.Is(err, types.ErrInvalidQuantity):
		return ReasonInvalidQuantity
	case errors.Is(err, types.ErrCancelTooSoon):
		return ReasonTooSoon
	case errors.Is(err, types.ErrOrderBelowMinQty), errors.Is(err, types.ErrOrderBelowMinNotional):
		return ReasonTooSmall
	case errors.Is(err, types.ErrOrderExceedsMaxQty),
		errors.Is(err, types.ErrOrderExceedsMaxNotional),
		errors.Is(err, types.ErrTooManyOrders),
		errors.Is(err, types.ErrNotionalOverflow):
		return ReasonTooLarge
	case errors.Is(err, types.ErrFOKCannotFill):
		return ReasonFOKCannotFill
	case errors.Is(err, matching.ErrShuttingDown):
		return ReasonShuttingDown
	case errors.Is(err, matching.ErrQueueFull):
		return ReasonOverloaded
	case errors.Is(err, types.ErrSelfTradeNotAllowed):
		return ReasonSelfTrade
	case errors.Is(err, types.ErrPostOnlyWouldCross):
		return ReasonPostOnlyCross
	case errors.Is(err, types.ErrPriceOutsideBand):
		return ReasonPriceBand
	}
	return ReasonOther
}
