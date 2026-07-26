package signals

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func TestCVD_AccumulatesSignedFlow(t *testing.T) {
	c := NewCVD()

	if got := c.Observe([]*types.Trade{trade(types.SideBuy, 10)}); got != 10 {
		t.Errorf("batch delta = %d, want 10", got)
	}
	if got := c.Observe([]*types.Trade{trade(types.SideSell, 4)}); got != -4 {
		t.Errorf("batch delta = %d, want -4", got)
	}
	if got := c.Value(); got != 6 {
		t.Errorf("CVD = %d, want 6", got)
	}

	c.Add(-6)
	if got := c.Value(); got != 0 {
		t.Errorf("CVD after Add(-6) = %d, want 0", got)
	}

	c.Reset()
	if got := c.Value(); got != 0 {
		t.Errorf("CVD after Reset = %d, want 0", got)
	}
}

func TestTickRuleSide(t *testing.T) {
	cases := []struct {
		name             string
		price, prevPrice int64
		prevSide, want   types.Side
	}{
		{"uptick is a buy", 101, 100, types.SideSell, types.SideBuy},
		{"downtick is a sell", 99, 100, types.SideBuy, types.SideSell},
		{"zero tick repeats a buy", 100, 100, types.SideBuy, types.SideBuy},
		{"zero tick repeats a sell", 100, 100, types.SideSell, types.SideSell},
		{"zero tick with no prior defaults to buy", 100, 100, "", types.SideBuy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TickRuleSide(tc.price, tc.prevPrice, tc.prevSide); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLeeReadySide(t *testing.T) {
	cases := []struct {
		name      string
		price     int64
		mid       float64
		prevPrice int64
		prevSide  types.Side
		want      types.Side
	}{
		{"above the mid is a buy", 102, 100.5, 100, types.SideSell, types.SideBuy},
		{"below the mid is a sell", 99, 100.5, 100, types.SideBuy, types.SideSell},
		{"at the mid falls back to the tick rule", 100, 100, 99, types.SideSell, types.SideBuy},
		{"no quote falls back to the tick rule", 101, 0, 100, types.SideSell, types.SideBuy},
		{"negative mid falls back to the tick rule", 99, -1, 100, types.SideBuy, types.SideSell},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LeeReadySide(tc.price, tc.mid, tc.prevPrice, tc.prevSide)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAbsorption(t *testing.T) {
	cfg := AbsorptionConfig{MinDelta: 100, MaxMove: 2, MinTrades: 5}

	if !cfg.Absorbed(150, 1, 10) {
		t.Error("heavy one-sided flow that barely moved price should be absorption")
	}
	if !cfg.Absorbed(-150, -1, 10) {
		t.Error("absorption is side-agnostic; heavy selling absorbed should qualify too")
	}
	if cfg.Absorbed(150, 9, 10) {
		t.Error("flow that moved the price a long way is not absorbed")
	}
	if cfg.Absorbed(20, 0, 10) {
		t.Error("light flow going nowhere is a quiet market, not absorption")
	}
	if cfg.Absorbed(150, 1, 2) {
		t.Error("too few trades to call anything")
	}
}

func TestDivergence(t *testing.T) {
	prev := Extremes{PriceHigh: 100, PriceLow: 90, CVDHigh: 50, CVDLow: -50}

	bearish := Extremes{PriceHigh: 105, PriceLow: 95, CVDHigh: 40, CVDLow: -20}
	if got := Divergence(prev, bearish); got != BearishDivergence {
		t.Errorf("higher price high on weaker CVD = %v, want BEARISH", got)
	}

	bullish := Extremes{PriceHigh: 98, PriceLow: 85, CVDHigh: 30, CVDLow: -40}
	if got := Divergence(prev, bullish); got != BullishDivergence {
		t.Errorf("lower price low on stronger CVD = %v, want BULLISH", got)
	}

	confirmed := Extremes{PriceHigh: 105, PriceLow: 95, CVDHigh: 70, CVDLow: -20}
	if got := Divergence(prev, confirmed); got != NoDivergence {
		t.Errorf("price and CVD both making higher highs = %v, want NONE", got)
	}
}

func TestDivergenceKind_String(t *testing.T) {
	cases := map[DivergenceKind]string{
		NoDivergence:      "NONE",
		BearishDivergence: "BEARISH",
		BullishDivergence: "BULLISH",
	}
	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", kind, got, want)
		}
	}
}

// The tick rule is a pure function of price changes, so a series that only ever
// upticks must be classified entirely as buys. This pins the direction
// convention that the whole misclassification experiment depends on.
func TestTickRule_MonotonicSeriesIsOneSided(t *testing.T) {
	prices := []int64{100, 101, 102, 103, 104}
	side := types.SideBuy
	for i := 1; i < len(prices); i++ {
		side = TickRuleSide(prices[i], prices[i-1], side)
		if side != types.SideBuy {
			t.Fatalf("rising series classified %v at index %d, want all buys", side, i)
		}
	}
}
