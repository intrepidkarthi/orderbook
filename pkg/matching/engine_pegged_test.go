package matching

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func pegged(t *testing.T, user string, side types.Side, qty int64, ref types.PegReference, offset int64) *types.PeggedOrder {
	t.Helper()
	// The underlying limit's price is a placeholder; the engine resolves it.
	p, err := types.NewPeggedOrder(lim(t, user, side, 1, qty), ref, offset)
	if err != nil {
		t.Fatalf("NewPeggedOrder: %v", err)
	}
	return p
}

func TestPegged_ToBidJoinsBid(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "b", types.SideBuy, 100, 5))
	e.Process(lim(t, "a", types.SideSell, 101, 5))

	// Buy pegged to the bid at offset 0 → rests at 100 alongside the bid.
	res := e.ProcessPegged(pegged(t, "peg", types.SideBuy, 3, types.PegToBid, 0))
	if res.Status == types.OrderStatusRejected {
		t.Fatalf("unexpected reject: %v", res.RejectionReason)
	}
	if _, qty, _ := e.BestBid(); qty != 8 {
		t.Errorf("bid qty = %d, want 8 (5 + pegged 3)", qty)
	}
}

func TestPegged_ToMidWithOffset(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "b", types.SideBuy, 100, 5))
	e.Process(lim(t, "a", types.SideSell, 102, 5)) // mid = 101

	// Buy pegged to mid − 1 tick → 100 → joins the bid.
	e.ProcessPegged(pegged(t, "peg", types.SideBuy, 2, types.PegToMid, -1))
	if bid, qty, _ := e.BestBid(); bid != 100 || qty != 7 {
		t.Errorf("bid = %d x %d, want 100 x 7", bid, qty)
	}
}

func TestPegged_ReferenceUnavailable(t *testing.T) {
	e := newEngine() // empty book
	res := e.ProcessPegged(pegged(t, "peg", types.SideBuy, 3, types.PegToBid, 0))
	if res.Status != types.OrderStatusRejected || !errors.Is(res.RejectionReason, types.ErrPegReferenceUnavailable) {
		t.Errorf("expected reject with ErrPegReferenceUnavailable, got %q / %v", res.Status, res.RejectionReason)
	}
}

func TestNewPeggedOrder_Validation(t *testing.T) {
	if _, err := types.NewPeggedOrder(nil, types.PegToMid, 0); !errors.Is(err, types.ErrNilOrder) {
		t.Errorf("nil order err = %v, want ErrNilOrder", err)
	}
	o := lim(t, "u", types.SideBuy, 100, 1)
	if _, err := types.NewPeggedOrder(o, "WEIRD", 0); !errors.Is(err, types.ErrInvalidPegReference) {
		t.Errorf("bad ref err = %v, want ErrInvalidPegReference", err)
	}
}
