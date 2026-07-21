package matching

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func pegged(t *testing.T, user string, side types.Side, qty string, ref types.PegReference, offset string) *types.PeggedOrder {
	t.Helper()
	// The underlying limit's price is a placeholder; the engine resolves it.
	p, err := types.NewPeggedOrder(lim(t, user, side, "1", qty), ref, dec(offset))
	if err != nil {
		t.Fatalf("NewPeggedOrder: %v", err)
	}
	return p
}

func TestPegged_ToBidJoinsBid(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "b", types.SideBuy, "100", "5"))
	e.Process(lim(t, "a", types.SideSell, "101", "5"))

	// Buy pegged to the bid at offset 0 → rests at 100 alongside the bid.
	res := e.ProcessPegged(pegged(t, "peg", types.SideBuy, "3", types.PegToBid, "0"))
	if res.Status == types.OrderStatusRejected {
		t.Fatalf("unexpected reject: %v", res.RejectionReason)
	}
	if _, qty, _ := e.BestBid(); !qty.Equal(dec("8")) {
		t.Errorf("bid qty = %s, want 8 (5 + pegged 3)", qty)
	}
}

func TestPegged_ToMidWithOffset(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "b", types.SideBuy, "100", "5"))
	e.Process(lim(t, "a", types.SideSell, "101", "5")) // mid = 100.5

	// Buy pegged to mid − 0.5 → 100.0 → joins the bid.
	e.ProcessPegged(pegged(t, "peg", types.SideBuy, "2", types.PegToMid, "-0.5"))
	if bid, qty, _ := e.BestBid(); !bid.Equal(dec("100")) || !qty.Equal(dec("7")) {
		t.Errorf("bid = %s x %s, want 100 x 7", bid, qty)
	}
}

func TestPegged_ReferenceUnavailable(t *testing.T) {
	e := newEngine() // empty book
	res := e.ProcessPegged(pegged(t, "peg", types.SideBuy, "3", types.PegToBid, "0"))
	if res.Status != types.OrderStatusRejected || !errors.Is(res.RejectionReason, types.ErrPegReferenceUnavailable) {
		t.Errorf("expected reject with ErrPegReferenceUnavailable, got %q / %v", res.Status, res.RejectionReason)
	}
}

func TestNewPeggedOrder_Validation(t *testing.T) {
	if _, err := types.NewPeggedOrder(nil, types.PegToMid, decimal.Zero); !errors.Is(err, types.ErrNilOrder) {
		t.Errorf("nil order err = %v, want ErrNilOrder", err)
	}
	o := lim(t, "u", types.SideBuy, "100", "1")
	if _, err := types.NewPeggedOrder(o, "WEIRD", decimal.Zero); !errors.Is(err, types.ErrInvalidPegReference) {
		t.Errorf("bad ref err = %v, want ErrInvalidPegReference", err)
	}
}
