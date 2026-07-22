package types

import (
	"errors"
	"testing"
)

func mustOrder(t *testing.T, side Side, typ OrderType, price, qty int64, tif TimeInForce) *Order {
	t.Helper()
	o, err := NewOrder("user1", "BTC-USD", side, typ, price, qty, tif)
	if err != nil {
		t.Fatalf("NewOrder: unexpected error: %v", err)
	}
	return o
}

func TestNewOrder_Valid(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, 30000, 5, TIFGoodTillCancel)
	if o.ID != 0 {
		t.Error("id should be unset (0) until the order enters the engine")
	}
	if o.Status != OrderStatusNew {
		t.Errorf("status = %q, want NEW", o.Status)
	}
	if o.RemainingQty != 5 {
		t.Errorf("remaining = %d, want 5", o.RemainingQty)
	}
	if o.FilledQty != 0 {
		t.Errorf("filled = %d, want 0", o.FilledQty)
	}
	if !o.IsActive() || o.IsFilled() {
		t.Error("new order should be active and not filled")
	}
}

func TestNewOrder_MarketZeroesPrice(t *testing.T) {
	// A market order may be constructed with any price; it is forced to zero.
	o, err := NewOrder("u", "BTC-USD", SideSell, OrderTypeMarket, 123, 1, TIFImmediateOrCancel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.Price != 0 {
		t.Errorf("market price = %d, want 0", o.Price)
	}
}

func TestNewOrder_Validation(t *testing.T) {
	cases := []struct {
		name    string
		side    Side
		typ     OrderType
		price   int64
		qty     int64
		tif     TimeInForce
		wantErr error
	}{
		{"zero qty", SideBuy, OrderTypeLimit, 10, 0, TIFGoodTillCancel, ErrInvalidQuantity},
		{"negative qty", SideBuy, OrderTypeLimit, 10, -1, TIFGoodTillCancel, ErrInvalidQuantity},
		{"zero price limit", SideBuy, OrderTypeLimit, 0, 1, TIFGoodTillCancel, ErrInvalidPrice},
		{"negative price limit", SideSell, OrderTypeLimit, -5, 1, TIFGoodTillCancel, ErrInvalidPrice},
		{"bad side", "HODL", OrderTypeLimit, 10, 1, TIFGoodTillCancel, ErrInvalidSide},
		{"bad type", SideBuy, "STOP", 10, 1, TIFGoodTillCancel, ErrInvalidOrderType},
		{"bad tif", SideBuy, OrderTypeLimit, 10, 1, "GTX", ErrInvalidTimeInForce},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOrder("u", "BTC-USD", tc.side, tc.typ, tc.price, tc.qty, tc.tif)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestOrder_FillPartialThenFull(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, 100, 10, TIFGoodTillCancel)

	if err := o.Fill(4); err != nil {
		t.Fatalf("partial fill: %v", err)
	}
	if o.Status != OrderStatusPartiallyFilled {
		t.Errorf("status = %q, want PARTIALLY_FILLED", o.Status)
	}
	if o.RemainingQty != 6 {
		t.Errorf("remaining = %d, want 6", o.RemainingQty)
	}

	if err := o.Fill(6); err != nil {
		t.Fatalf("final fill: %v", err)
	}
	if o.Status != OrderStatusFilled || !o.IsFilled() {
		t.Errorf("status = %q, want FILLED", o.Status)
	}
	// Conservation: filled + remaining == quantity.
	if o.FilledQty+o.RemainingQty != o.Quantity {
		t.Error("filled + remaining must equal quantity")
	}
}

func TestOrder_FillInvalid(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, 100, 10, TIFGoodTillCancel)
	if err := o.Fill(0); !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("zero fill err = %v, want ErrInvalidQuantity", err)
	}
	if err := o.Fill(11); !errors.Is(err, ErrFillExceedsRemaining) {
		t.Errorf("overfill err = %v, want ErrFillExceedsRemaining", err)
	}
}

func TestOrder_Cancel(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, 100, 10, TIFGoodTillCancel)
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
	f := mustOrder(t, SideBuy, OrderTypeLimit, 100, 1, TIFGoodTillCancel)
	_ = f.Fill(1)
	if err := f.Cancel(); !errors.Is(err, ErrOrderNotActive) {
		t.Errorf("cancel filled err = %v, want ErrOrderNotActive", err)
	}
}

func TestOrder_Fresh(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, 100, 10, TIFGoodTillCancel)
	o.ID = 42     // pretend it entered the engine
	_ = o.Fill(4) // mutate the original

	f := o.Fresh()
	// Params preserved; id reset (a fresh copy re-enters the engine unassigned).
	if f.Side != o.Side || f.Price != 100 || f.Quantity != 10 {
		t.Error("Fresh must preserve side, price, quantity")
	}
	// State reset.
	if f.FilledQty != 0 || f.RemainingQty != 10 || f.Status != OrderStatusNew || f.ID != 0 {
		t.Errorf("Fresh not reset: filled=%d remaining=%d status=%s id=%d",
			f.FilledQty, f.RemainingQty, f.Status, f.ID)
	}
	// Original untouched.
	if o.FilledQty != 4 || o.Status != OrderStatusPartiallyFilled {
		t.Error("Fresh must not mutate the original order")
	}
}

func TestOrder_Notional(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeLimit, 100, 10, TIFGoodTillCancel)
	if o.NotionalValue() != 1000 {
		t.Errorf("notional = %d, want 1000", o.NotionalValue())
	}
	_ = o.Fill(3)
	if o.RemainingValue() != 700 {
		t.Errorf("remaining value = %d, want 700", o.RemainingValue())
	}
}
