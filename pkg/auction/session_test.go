package auction

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestAuctionSession_UniformPriceClose: orders accumulate, an indicative price is
// published during the call, and at the scheduled close everything crosses at one
// uniform price.
func TestAuctionSession_UniformPriceClose(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	s := NewAuctionSession("X", base.Add(time.Minute))
	s.Submit(ord(t, types.SideBuy, 105, 10))
	s.Submit(ord(t, types.SideBuy, 100, 10))
	s.Submit(ord(t, types.SideSell, 99, 10))
	s.Submit(ord(t, types.SideSell, 100, 10))

	if ind := s.Indicative(); !ind.HasClearing || ind.ClearingPrice != 100 || ind.Volume != 20 {
		t.Fatalf("indicative = %+v, want clearing 100 volume 20", ind)
	}
	// Before the close time nothing uncrosses.
	if _, ok := s.Close(base); ok {
		t.Fatal("auction should not close before its scheduled time")
	}
	res, ok := s.Close(base.Add(time.Minute))
	if !ok || res.ClearingPrice != 100 || res.Volume != 20 {
		t.Fatalf("uncross = %+v (ok=%v), want clearing 100 volume 20", res, ok)
	}
	for _, tr := range res.Trades {
		if tr.Price != 100 {
			t.Errorf("trade at %d, want the uniform clearing price 100", tr.Price)
		}
	}
	if !s.Closed() {
		t.Error("session should report closed after the uncross")
	}
}

// TestAuctionSession_CancelDuringCall: an order withdrawn during the call phase
// does not participate in the uncross.
func TestAuctionSession_CancelDuringCall(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	s := NewAuctionSession("X", base.Add(time.Minute))
	buy := ord(t, types.SideBuy, 105, 10)
	s.Submit(buy)
	s.Submit(ord(t, types.SideSell, 100, 10))

	if err := s.Cancel(buy.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	res, ok := s.Close(base.Add(time.Minute))
	if !ok || res.HasClearing {
		t.Errorf("with the only buy withdrawn nothing should cross, got %+v", res)
	}
}

// TestAuctionSession_ClosedGating: submits and cancels are refused once cleared.
func TestAuctionSession_ClosedGating(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	s := NewAuctionSession("X", base)
	if _, ok := s.Close(base); !ok { // now == closeAt uncrosses (empty)
		t.Fatal("Close at the scheduled time should uncross")
	}
	if err := s.Submit(ord(t, types.SideBuy, 100, 1)); !errors.Is(err, ErrAuctionClosed) {
		t.Errorf("submit after close should be ErrAuctionClosed, got %v", err)
	}
	if err := s.Cancel(1); !errors.Is(err, ErrAuctionClosed) {
		t.Errorf("cancel after close should be ErrAuctionClosed, got %v", err)
	}
	if _, ok := s.Close(base.Add(time.Hour)); ok {
		t.Error("a session should uncross only once")
	}
}

// TestRandomizedClose: the jittered close is deterministic for a seed, lands
// within the window, and varies across seeds — unpredictable yet replayable.
func TestRandomizedClose(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	window := time.Minute

	a := RandomizedClose(base, window, 42)
	if !a.Equal(RandomizedClose(base, window, 42)) {
		t.Error("same seed must produce the same close (replay determinism)")
	}
	if a.Before(base) || !a.Before(base.Add(window)) {
		t.Errorf("close %v must lie within [%v, %v)", a, base, base.Add(window))
	}
	differs := false
	for seed := range uint64(8) {
		if !RandomizedClose(base, window, seed).Equal(a) {
			differs = true
		}
	}
	if !differs {
		t.Error("different seeds should generally yield different close times")
	}
	if !RandomizedClose(base, 0, 7).Equal(base) {
		t.Error("a non-positive window returns base unchanged")
	}
}

func ExampleAuctionSession() {
	base := time.Unix(0, 0).UTC()
	s := NewAuctionSession("BTC-USD", base.Add(time.Minute))
	mk := func(side types.Side, price, qty int64) *types.Order {
		o, _ := types.NewOrder("u", "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		return o
	}
	s.Submit(mk(types.SideBuy, 105, 10))
	s.Submit(mk(types.SideSell, 100, 10))

	res, _ := s.Close(base.Add(time.Minute)) // uncross at the scheduled close
	fmt.Printf("cleared %d @ %d\n", res.Volume, res.ClearingPrice)
	// Output: cleared 10 @ 100
}
