package gateway

import (
	"errors"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func lim(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

func TestRateGate_ThrottlesAndRefills(t *testing.T) {
	g := NewRateGate(2, 3) // 2/sec, burst 3
	now := time.Unix(0, 0).UTC()

	// Burst of 3 is admitted; the 4th (same instant) is denied.
	for i := range 3 {
		if !g.Allow("u", now) {
			t.Fatalf("order %d within burst should be admitted", i)
		}
	}
	if g.Allow("u", now) {
		t.Error("4th order in the same instant should be throttled")
	}
	// A different account has its own bucket.
	if !g.Allow("v", now) {
		t.Error("a different account should not be throttled")
	}
	// After half a second, ~1 token has refilled.
	if !g.Allow("u", now.Add(500*time.Millisecond)) {
		t.Error("a token should have refilled after 0.5s")
	}
}

func TestGateway_CancelBypassesGate(t *testing.T) {
	runner := matching.NewRunner(matching.RunnerConfig{Engine: matching.DefaultConfig("BTC-USD")})
	defer runner.Close()
	g := New(runner, Config{Rate: 2, Burst: 1})
	now := time.Unix(0, 0).UTC()

	// Rest an order (consumes the only token).
	o := lim(t, "u", types.SideBuy, 90, 1)
	if _, err := g.Submit(o, now); err != nil {
		t.Fatalf("first submit should pass: %v", err)
	}
	// The bucket is now empty: a second submit at the same instant is throttled.
	if _, err := g.Submit(lim(t, "u", types.SideBuy, 89, 1), now); !errors.Is(err, ErrThrottled) {
		t.Fatalf("second submit should be throttled, got %v", err)
	}
	// ...but a cancel is never gated.
	if _, err := g.Cancel(o.ID, "u"); err != nil {
		t.Errorf("cancel should bypass the rate gate, got %v", err)
	}
}

func TestGateway_TakerBump(t *testing.T) {
	runner := matching.NewRunner(matching.RunnerConfig{Engine: matching.DefaultConfig("BTC-USD")})
	defer runner.Close()
	g := New(runner, Config{SpeedBump: 350 * time.Microsecond})

	// The gateway sees pre-engine orders (ids are engine-assigned), so identify the
	// bumped order by its account.
	var bumped []string
	g.OnBump = func(o *types.Order, _ time.Time) { bumped = append(bumped, o.UserID) }
	now := time.Unix(0, 0).UTC()

	// A resting maker (does not cross an empty book) is not bumped.
	maker := lim(t, "mm", types.SideSell, 100, 5)
	if _, err := g.Submit(maker, now); err != nil {
		t.Fatalf("maker submit: %v", err)
	}
	// A marketable buy (crosses the resting ask) is a taker → bumped.
	taker := lim(t, "t", types.SideBuy, 100, 2)
	if _, err := g.Submit(taker, now); err != nil {
		t.Fatalf("taker submit: %v", err)
	}
	// A non-crossing buy rests as a maker → not bumped.
	rester := lim(t, "t2", types.SideBuy, 90, 1)
	if _, err := g.Submit(rester, now); err != nil {
		t.Fatalf("rester submit: %v", err)
	}

	if len(bumped) != 1 || bumped[0] != "t" {
		t.Errorf("only the marketable taker should be speed-bumped, got accounts %v", bumped)
	}
}
