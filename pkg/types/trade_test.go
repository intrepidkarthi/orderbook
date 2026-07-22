package types

import "testing"

func TestNewTrade_BuyTaker(t *testing.T) {
	buy := mustOrder(t, SideBuy, OrderTypeLimit, 100, 5, TIFGoodTillCancel)   // taker
	sell := mustOrder(t, SideSell, OrderTypeLimit, 100, 5, TIFGoodTillCancel) // maker
	buy.ID, sell.ID = 1, 2

	tr := NewTrade("BTC-USD", 100, 5, buy, sell, SideBuy)

	if tr.TakerOrderID != buy.ID {
		t.Errorf("taker = %d, want buy order", tr.TakerOrderID)
	}
	if tr.MakerOrderID != sell.ID {
		t.Errorf("maker = %d, want sell order", tr.MakerOrderID)
	}
	if tr.NotionalValue() != 500 {
		t.Errorf("notional = %d, want 500", tr.NotionalValue())
	}
}

func TestNewTrade_SellTaker(t *testing.T) {
	buy := mustOrder(t, SideBuy, OrderTypeLimit, 100, 5, TIFGoodTillCancel)   // maker
	sell := mustOrder(t, SideSell, OrderTypeLimit, 100, 5, TIFGoodTillCancel) // taker
	buy.ID, sell.ID = 1, 2

	tr := NewTrade("BTC-USD", 100, 5, buy, sell, SideSell)

	if tr.TakerOrderID != sell.ID {
		t.Errorf("taker = %d, want sell order", tr.TakerOrderID)
	}
	if tr.MakerOrderID != buy.ID {
		t.Errorf("maker = %d, want buy order", tr.MakerOrderID)
	}
}

func TestTrade_IsSelfTrade(t *testing.T) {
	a, _ := NewOrder("same", "BTC-USD", SideBuy, OrderTypeLimit, 100, 1, TIFGoodTillCancel)
	b, _ := NewOrder("same", "BTC-USD", SideSell, OrderTypeLimit, 100, 1, TIFGoodTillCancel)
	tr := NewTrade("BTC-USD", 100, 1, a, b, SideBuy)
	if !tr.IsSelfTrade() {
		t.Error("expected self-trade when both users match")
	}

	c, _ := NewOrder("other", "BTC-USD", SideSell, OrderTypeLimit, 100, 1, TIFGoodTillCancel)
	tr2 := NewTrade("BTC-USD", 100, 1, a, c, SideBuy)
	if tr2.IsSelfTrade() {
		t.Error("did not expect self-trade for different users")
	}
}
