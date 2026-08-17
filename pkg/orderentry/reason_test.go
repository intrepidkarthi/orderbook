package orderentry

import (
	"fmt"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestReasonForMapsEverySentinelItClaimsTo pins the whole mapping, one row per
// sentinel, rather than pinning the case that was most recently added.
//
// The reason it exists is a regression it would have caught. The disk-space work
// added ErrTradingHalted and ErrNewOrdersHalted to ReasonFor by REPLACING the
// matching.ErrShuttingDown case rather than adding a case beside it, so a venue
// shutting down began answering ReasonOther (1) instead of ReasonShuttingDown (15) —
// the code cmd/obsoak branches on, reached live through cmd/obgw's replace and reduce
// paths, which deliver ErrShuttingDown on the done channel. Nothing caught it: the
// full suite and -race were green, and cmd/obgw/hardening_test.go only asserts that
// the wire and orderentry constants carry equal values, which they still did.
//
// A table is the right shape here precisely because the failure was a deletion. A
// test per case tests the cases somebody remembered to write a test for; this one
// fails when a row stops being answered, whichever row it is.
func TestReasonForMapsEverySentinelItClaimsTo(t *testing.T) {
	cases := []struct {
		err  error
		want uint16
	}{
		{nil, ReasonNone},
		{fmt.Errorf("something the mapping has never heard of"), ReasonOther},
		{types.ErrOrderNotFound, ReasonUnknownOrder},
		{types.ErrOrderNotActive, ReasonUnknownOrder},
		{types.ErrDuplicateClientOrderID, ReasonDuplicateClOrd},
		{types.ErrInvalidQuantity, ReasonInvalidQuantity},
		{types.ErrCancelTooSoon, ReasonTooSoon},
		{types.ErrOrderBelowMinQty, ReasonTooSmall},
		{types.ErrOrderBelowMinNotional, ReasonTooSmall},
		{types.ErrOrderExceedsMaxQty, ReasonTooLarge},
		{types.ErrOrderExceedsMaxNotional, ReasonTooLarge},
		{types.ErrTooManyOrders, ReasonTooLarge},
		{types.ErrNotionalOverflow, ReasonTooLarge},
		{types.ErrFOKCannotFill, ReasonFOKCannotFill},
		{types.ErrTradingHalted, ReasonHalted},
		{types.ErrNewOrdersHalted, ReasonHalted},
		{matching.ErrShuttingDown, ReasonShuttingDown},
		{matching.ErrQueueFull, ReasonOverloaded},
		{types.ErrSelfTradeNotAllowed, ReasonSelfTrade},
		{types.ErrPostOnlyWouldCross, ReasonPostOnlyCross},
		{types.ErrPriceOutsideBand, ReasonPriceBand},
	}
	for _, c := range cases {
		name := "nil"
		if c.err != nil {
			name = c.err.Error()
		}
		if got := ReasonFor(c.err); got != c.want {
			t.Errorf("ReasonFor(%s) = %d, want %d", name, got, c.want)
		}
	}
}

// TestReasonForWrapsRatherThanCompares — every caller here receives an error that has
// travelled through a MatchResult or a done channel, and some of them wrap. The
// mapping uses errors.Is for that reason, and this is what says so.
func TestReasonForWrapsRatherThanCompares(t *testing.T) {
	wrapped := fmt.Errorf("submit rejected: %w", matching.ErrShuttingDown)
	if got := ReasonFor(wrapped); got != ReasonShuttingDown {
		t.Errorf("ReasonFor(wrapped ErrShuttingDown) = %d, want %d", got, ReasonShuttingDown)
	}
	wrapped = fmt.Errorf("new orders refused: %w", types.ErrNewOrdersHalted)
	if got := ReasonFor(wrapped); got != ReasonHalted {
		t.Errorf("ReasonFor(wrapped ErrNewOrdersHalted) = %d, want %d", got, ReasonHalted)
	}
}
