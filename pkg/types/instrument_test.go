package types

import (
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestInstrument_RoundTrip(t *testing.T) {
	inst := NewInstrument("BTC-USD", d("0.01"), d("0.001"))

	cases := []struct {
		price     string
		wantTicks int64
		qty       string
		wantLots  int64
	}{
		{"30000", 3000000, "0.5", 500},
		{"30000.01", 3000001, "0.001", 1},
		{"0.05", 5, "1", 1000},
		{"100.55", 10055, "12.345", 12345},
	}
	for _, c := range cases {
		gotTicks := inst.PriceToTicks(d(c.price))
		if gotTicks != c.wantTicks {
			t.Errorf("PriceToTicks(%s) = %d, want %d", c.price, gotTicks, c.wantTicks)
		}
		// ticks → price → ticks must be stable.
		if back := inst.PriceToTicks(inst.TicksToPrice(gotTicks)); back != gotTicks {
			t.Errorf("price round-trip lost value: %d -> %s -> %d", gotTicks, inst.TicksToPrice(gotTicks), back)
		}
		gotLots := inst.QtyToLots(d(c.qty))
		if gotLots != c.wantLots {
			t.Errorf("QtyToLots(%s) = %d, want %d", c.qty, gotLots, c.wantLots)
		}
		if back := inst.QtyToLots(inst.LotsToQty(gotLots)); back != gotLots {
			t.Errorf("qty round-trip lost value: %d -> %s -> %d", gotLots, inst.LotsToQty(gotLots), back)
		}
	}
}

func TestInstrument_TicksToPriceValue(t *testing.T) {
	inst := NewInstrument("X", d("0.01"), d("1"))
	if got := inst.TicksToPrice(10055); !got.Equal(d("100.55")) {
		t.Errorf("TicksToPrice(10055) = %s, want 100.55", got)
	}
	if got := inst.LotsToQty(3); !got.Equal(d("3")) {
		t.Errorf("LotsToQty(3) = %s, want 3", got)
	}
}

func TestInstrument_DefaultsToUnitGrid(t *testing.T) {
	// Zero tick/lot default to 1, so ticks == whole price units.
	inst := NewInstrument("X", decimal.Zero, decimal.Zero)
	if got := inst.PriceToTicks(d("42")); got != 42 {
		t.Errorf("unit-grid PriceToTicks(42) = %d, want 42", got)
	}
	if got := inst.QtyToLots(d("7")); got != 7 {
		t.Errorf("unit-grid QtyToLots(7) = %d, want 7", got)
	}
}

func TestInstrument_NewOrder(t *testing.T) {
	inst := NewInstrument("BTC-USD", d("0.01"), d("0.001"))
	o, err := inst.NewOrder("alice", SideBuy, OrderTypeLimit, d("30000.50"), d("0.25"), TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if o.Price != 3000050 {
		t.Errorf("price ticks = %d, want 3000050", o.Price)
	}
	if o.Quantity != 250 {
		t.Errorf("qty lots = %d, want 250", o.Quantity)
	}
	if o.Symbol != "BTC-USD" {
		t.Errorf("symbol = %q, want BTC-USD", o.Symbol)
	}
	// Market orders ignore the price entirely.
	m, _ := inst.NewOrder("bob", SideSell, OrderTypeMarket, d("999"), d("1"), TIFImmediateOrCancel)
	if m.Price != 0 {
		t.Errorf("market price ticks = %d, want 0", m.Price)
	}
}
