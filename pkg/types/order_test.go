package types

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func mustOrder(t *testing.T, side Side, typ OrderType, price, qty string, tif TimeInForce) *Order {
	t.Helper()
	o, err := NewOrder("user1", "BTC-USD", side, typ, dec(price), dec(qty), tif)
	if err != nil {
		t.Fatalf("NewOrder: unexpected error: %v", err)
	}
	return o
}

func TestNewOrder_Valid(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, "30000", "0.5", TIFGoodTillCancel)
	if o.ID == "" {
		t.Error("expected a generated id")
	}
	if o.Status != OrderStatusNew {
		t.Errorf("status = %q, want NEW", o.Status)
	}
	if !o.RemainingQty.Equal(dec("0.5")) {
		t.Errorf("remaining = %s, want 0.5", o.RemainingQty)
	}
	if !o.FilledQty.IsZero() {
		t.Errorf("filled = %s, want 0", o.FilledQty)
	}
	if !o.IsActive() || o.IsFilled() {
		t.Error("new order should be active and not filled")
	}
}

func TestNewOrder_MarketZeroesPrice(t *testing.T) {
	// A market order may be constructed with any price; it is forced to zero.
	o, err := NewOrder("u", "BTC-USD", SideSell, OrderTypeMarket, dec("123"), dec("1"), TIFImmediateOrCancel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !o.Price.IsZero() {
		t.Errorf("market price = %s, want 0", o.Price)
	}
}

func TestNewOrder_Validation(t *testing.T) {
	cases := []struct {
		name    string
		side    Side
		typ     OrderType
		price   string
		qty     string
		tif     TimeInForce
		wantErr error
	}{
		{"zero qty", SideBuy, OrderTypeLimit, "10", "0", TIFGoodTillCancel, ErrInvalidQuantity},
		{"negative qty", SideBuy, OrderTypeLimit, "10", "-1", TIFGoodTillCancel, ErrInvalidQuantity},
		{"zero price limit", SideBuy, OrderTypeLimit, "0", "1", TIFGoodTillCancel, ErrInvalidPrice},
		{"negative price limit", SideSell, OrderTypeLimit, "-5", "1", TIFGoodTillCancel, ErrInvalidPrice},
		{"bad side", "HODL", OrderTypeLimit, "10", "1", TIFGoodTillCancel, ErrInvalidSide},
		{"bad type", SideBuy, "STOP", "10", "1", TIFGoodTillCancel, ErrInvalidOrderType},
		{"bad tif", SideBuy, OrderTypeLimit, "10", "1", "GTX", ErrInvalidTimeInForce},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOrder("u", "BTC-USD", tc.side, tc.typ, dec(tc.price), dec(tc.qty), tc.tif)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestOrder_FillPartialThenFull(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, "100", "10", TIFGoodTillCancel)

	if err := o.Fill(dec("4")); err != nil {
		t.Fatalf("partial fill: %v", err)
	}
	if o.Status != OrderStatusPartiallyFilled {
		t.Errorf("status = %q, want PARTIALLY_FILLED", o.Status)
	}
	if !o.RemainingQty.Equal(dec("6")) {
		t.Errorf("remaining = %s, want 6", o.RemainingQty)
	}

	if err := o.Fill(dec("6")); err != nil {
		t.Fatalf("final fill: %v", err)
	}
	if o.Status != OrderStatusFilled || !o.IsFilled() {
		t.Errorf("status = %q, want FILLED", o.Status)
	}
	// Conservation: filled + remaining == quantity.
	if !o.FilledQty.Add(o.RemainingQty).Equal(o.Quantity) {
		t.Error("filled + remaining must equal quantity")
	}
}

func TestOrder_FillInvalid(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, "100", "10", TIFGoodTillCancel)
	if err := o.Fill(dec("0")); !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("zero fill err = %v, want ErrInvalidQuantity", err)
	}
	if err := o.Fill(dec("11")); !errors.Is(err, ErrFillExceedsRemaining) {
		t.Errorf("overfill err = %v, want ErrFillExceedsRemaining", err)
	}
}

func TestOrder_Cancel(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, "100", "10", TIFGoodTillCancel)
	if err := o.Cancel(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if o.Status != OrderStatusCancelled || o.IsActive() {
		t.Errorf("status = %q, want CANCELLED", o.Status)
	}
	// Cancelling again fails.
	if err := o.Cancel(); !errors.Is(err, ErrOrderNotActive) {
		t.Errorf("double cancel err = %v, want ErrOrderNotActive", err)
	}

	// A filled order cannot be cancelled.
	f := mustOrder(t, SideBuy, OrderTypeLimit, "100", "1", TIFGoodTillCancel)
	_ = f.Fill(dec("1"))
	if err := f.Cancel(); !errors.Is(err, ErrOrderNotActive) {
		t.Errorf("cancel filled err = %v, want ErrOrderNotActive", err)
	}
}

func TestOrder_Notional(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, "100", "10", TIFGoodTillCancel)
	if !o.NotionalValue().Equal(dec("1000")) {
		t.Errorf("notional = %s, want 1000", o.NotionalValue())
	}
	_ = o.Fill(dec("3"))
	if !o.RemainingValue().Equal(dec("700")) {
		t.Errorf("remaining value = %s, want 700", o.RemainingValue())
	}
}
