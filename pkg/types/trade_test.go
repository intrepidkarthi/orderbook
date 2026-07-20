package types

import "testing"

func TestNewTrade_BuyTaker(t *testing.T) {
	buy := mustOrder(t, SideBuy, OrderTypeLimit, "100", "5", TIFGoodTillCancel)   // taker
	sell := mustOrder(t, SideSell, OrderTypeLimit, "100", "5", TIFGoodTillCancel) // maker

	tr := NewTrade("BTC-USD", dec("100"), dec("5"), buy, sell, SideBuy)

	if tr.TakerOrderID != buy.ID {
		t.Errorf("taker = %s, want buy order", tr.TakerOrderID)
	}
	if tr.MakerOrderID != sell.ID {
		t.Errorf("maker = %s, want sell order", tr.MakerOrderID)
	}
	if !tr.NotionalValue().Equal(dec("500")) {
		t.Errorf("notional = %s, want 500", tr.NotionalValue())
	}
}

func TestNewTrade_SellTaker(t *testing.T) {
	buy := mustOrder(t, SideBuy, OrderTypeLimit, "100", "5", TIFGoodTillCancel)   // maker
	sell := mustOrder(t, SideSell, OrderTypeLimit, "100", "5", TIFGoodTillCancel) // taker

	tr := NewTrade("BTC-USD", dec("100"), dec("5"), buy, sell, SideSell)

	if tr.TakerOrderID != sell.ID {
		t.Errorf("taker = %s, want sell order", tr.TakerOrderID)
	}
	if tr.MakerOrderID != buy.ID {
		t.Errorf("maker = %s, want buy order", tr.MakerOrderID)
	}
}

func TestTrade_IsSelfTrade(t *testing.T) {
	a, _ := NewOrder("same", "BTC-USD", SideBuy, OrderTypeLimit, dec("100"), dec("1"), TIFGoodTillCancel)
	b, _ := NewOrder("same", "BTC-USD", SideSell, OrderTypeLimit, dec("100"), dec("1"), TIFGoodTillCancel)
	tr := NewTrade("BTC-USD", dec("100"), dec("1"), a, b, SideBuy)
	if !tr.IsSelfTrade() {
		t.Error("expected self-trade when both users match")
	}

	c, _ := NewOrder("other", "BTC-USD", SideSell, OrderTypeLimit, dec("100"), dec("1"), TIFGoodTillCancel)
	tr2 := NewTrade("BTC-USD", dec("100"), dec("1"), a, c, SideBuy)
	if tr2.IsSelfTrade() {
		t.Error("did not expect self-trade for different users")
	}
}
