package matching

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func ord(t *testing.T, user string, side types.Side, typ types.OrderType, price, qty string, tif types.TimeInForce) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "BTC-USD", side, typ, dec(price), dec(qty), tif)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// lim builds a GTC limit order.
func lim(t *testing.T, user string, side types.Side, price, qty string) *types.Order {
	return ord(t, user, side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
}

func newEngine() *Engine { return NewEngine(DefaultConfig("BTC-USD")) }

func TestLimitCross_TradeAtMakerPrice(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "maker", types.SideSell, "100", "1")) // resting ask @100

	// Aggressive buy priced better than the ask; must execute at the maker price.
	res := e.Process(lim(t, "taker", types.SideBuy, "101", "1"))

	if res.Status != types.OrderStatusFilled {
		t.Fatalf("status = %q, want FILLED", res.Status)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(res.Trades))
	}
	if !res.Trades[0].Price.Equal(dec("100")) {
		t.Errorf("trade price = %s, want 100 (maker price)", res.Trades[0].Price)
	}
	if e.OrderCount() != 0 {
		t.Errorf("book should be empty, count = %d", e.OrderCount())
	}
}

func TestPartialFill_RestsRemainder(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "m", types.SideSell, "100", "5"))
	res := e.Process(lim(t, "t", types.SideBuy, "100", "8"))

	if res.Status != types.OrderStatusPartiallyFilled {
		t.Fatalf("status = %q, want PARTIALLY_FILLED", res.Status)
	}
	if !res.Trades[0].Quantity.Equal(dec("5")) {
		t.Errorf("filled qty = %s, want 5", res.Trades[0].Quantity)
	}
	// Remainder (3) rests as the best bid; asks are exhausted.
	bid, qty, ok := e.BestBid()
	if !ok || !bid.Equal(dec("100")) || !qty.Equal(dec("3")) {
		t.Errorf("resting bid = %s x %s, want 100 x 3", bid, qty)
	}
	if _, _, ok := e.BestAsk(); ok {
		t.Error("asks should be exhausted")
	}
}

func TestNoCross_Rests(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "m", types.SideSell, "100", "1"))
	res := e.Process(lim(t, "t", types.SideBuy, "99", "1")) // below the ask

	if len(res.Trades) != 0 {
		t.Fatalf("expected no trades, got %d", len(res.Trades))
	}
	if res.Status != types.OrderStatusNew {
		t.Errorf("status = %q, want NEW", res.Status)
	}
	if bid, _, ok := e.BestBid(); !ok || !bid.Equal(dec("99")) {
		t.Errorf("best bid = %s, want 99", bid)
	}
}

func TestPriceTimePriority(t *testing.T) {
	e := newEngine()
	// Two makers at the best price (FIFO), one worse.
	first := lim(t, "a", types.SideSell, "100", "1")
	second := lim(t, "b", types.SideSell, "100", "1")
	worse := lim(t, "c", types.SideSell, "101", "1")
	e.Process(first)
	e.Process(second)
	e.Process(worse)

	res := e.Process(lim(t, "t", types.SideBuy, "101", "3"))
	if len(res.Trades) != 3 {
		t.Fatalf("trades = %d, want 3", len(res.Trades))
	}
	// FIFO at 100 then the 101 level: first, second, worse.
	wantMakers := []string{first.ID, second.ID, worse.ID}
	wantPrices := []string{"100", "100", "101"}
	for i, tr := range res.Trades {
		if tr.MakerOrderID != wantMakers[i] {
			t.Errorf("trade %d maker = %s, want %s (priority violated)", i, tr.MakerOrderID, wantMakers[i])
		}
		if !tr.Price.Equal(dec(wantPrices[i])) {
			t.Errorf("trade %d price = %s, want %s", i, tr.Price, wantPrices[i])
		}
	}
}

func TestMarketSweep(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "a", types.SideSell, "100", "1"))
	e.Process(lim(t, "b", types.SideSell, "101", "1"))
	e.Process(lim(t, "c", types.SideSell, "102", "1"))

	res := e.Process(ord(t, "t", types.SideBuy, types.OrderTypeMarket, "0", "3", types.TIFImmediateOrCancel))
	if res.Status != types.OrderStatusFilled {
		t.Fatalf("status = %q, want FILLED", res.Status)
	}
	if len(res.Trades) != 3 {
		t.Fatalf("trades = %d, want 3", len(res.Trades))
	}
	got := []string{res.Trades[0].Price.String(), res.Trades[1].Price.String(), res.Trades[2].Price.String()}
	want := []string{"100", "101", "102"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("trade %d price = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestMarketNoLiquidity(t *testing.T) {
	e := newEngine()
	res := e.Process(ord(t, "t", types.SideBuy, types.OrderTypeMarket, "0", "1", types.TIFImmediateOrCancel))
	if res.Status != types.OrderStatusCancelled {
		t.Fatalf("status = %q, want CANCELLED", res.Status)
	}
	if !errors.Is(res.RejectionReason, types.ErrMarketOrderNoLiquidity) {
		t.Errorf("reason = %v, want ErrMarketOrderNoLiquidity", res.RejectionReason)
	}
	if len(res.Trades) != 0 {
		t.Errorf("trades = %d, want 0", len(res.Trades))
	}
}

func TestMarketPartialThenCancel(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "m", types.SideSell, "100", "1"))
	res := e.Process(ord(t, "t", types.SideBuy, types.OrderTypeMarket, "0", "3", types.TIFImmediateOrCancel))

	// One fill, no reject reason (partial), nothing rests.
	if res.Status != types.OrderStatusCancelled {
		t.Fatalf("status = %q, want CANCELLED", res.Status)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(res.Trades))
	}
	if res.RejectionReason != nil {
		t.Errorf("reason = %v, want nil (partial fill occurred)", res.RejectionReason)
	}
	if e.OrderCount() != 0 {
		t.Errorf("book should be empty, count = %d", e.OrderCount())
	}
}

func TestIOC_PartialCancelsRemainder(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "m", types.SideSell, "100", "1"))
	res := e.Process(ord(t, "t", types.SideBuy, types.OrderTypeLimit, "100", "3", types.TIFImmediateOrCancel))

	if res.Status != types.OrderStatusCancelled {
		t.Fatalf("status = %q, want CANCELLED", res.Status)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(res.Trades))
	}
	// Nothing from an IOC ever rests.
	if _, _, ok := e.BestBid(); ok {
		t.Error("IOC remainder must not rest on the book")
	}
}

func TestIOC_FullFill(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "m", types.SideSell, "100", "3"))
	res := e.Process(ord(t, "t", types.SideBuy, types.OrderTypeLimit, "100", "2", types.TIFImmediateOrCancel))
	if res.Status != types.OrderStatusFilled {
		t.Errorf("status = %q, want FILLED", res.Status)
	}
	// Maker remainder (1) still rests.
	if _, qty, _ := e.BestAsk(); !qty.Equal(dec("1")) {
		t.Errorf("resting ask qty = %s, want 1", qty)
	}
}

func TestFOK_InsufficientRejected_BookUnchanged(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "m", types.SideSell, "100", "2"))

	res := e.Process(ord(t, "t", types.SideBuy, types.OrderTypeLimit, "100", "5", types.TIFFillOrKill))
	if res.Status != types.OrderStatusRejected {
		t.Fatalf("status = %q, want REJECTED", res.Status)
	}
	if !errors.Is(res.RejectionReason, types.ErrFOKCannotFill) {
		t.Errorf("reason = %v, want ErrFOKCannotFill", res.RejectionReason)
	}
	if len(res.Trades) != 0 {
		t.Errorf("trades = %d, want 0 (reversed)", len(res.Trades))
	}
	// Book must be exactly as before: ask @100 qty 2, one order, status NEW.
	if ask, qty, ok := e.BestAsk(); !ok || !ask.Equal(dec("100")) || !qty.Equal(dec("2")) {
		t.Errorf("restored ask = %s x %s, want 100 x 2", ask, qty)
	}
	if e.OrderCount() != 1 {
		t.Errorf("order count = %d, want 1", e.OrderCount())
	}
}

func TestFOK_SufficientFills(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "a", types.SideSell, "100", "2"))
	e.Process(lim(t, "b", types.SideSell, "101", "2"))

	res := e.Process(ord(t, "t", types.SideBuy, types.OrderTypeLimit, "101", "4", types.TIFFillOrKill))
	if res.Status != types.OrderStatusFilled {
		t.Fatalf("status = %q, want FILLED", res.Status)
	}
	if len(res.Trades) != 2 {
		t.Errorf("trades = %d, want 2", len(res.Trades))
	}
	if e.OrderCount() != 0 {
		t.Errorf("book should be empty, count = %d", e.OrderCount())
	}
}

func TestSTP_CancelNewest(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", SelfTradePrevention: STPCancelNewest})
	e.Process(lim(t, "same", types.SideSell, "100", "1"))
	res := e.Process(lim(t, "same", types.SideBuy, "100", "1"))

	if len(res.Trades) != 0 {
		t.Fatalf("self-trade must not execute, got %d trades", len(res.Trades))
	}
	if res.Status != types.OrderStatusCancelled {
		t.Errorf("taker status = %q, want CANCELLED", res.Status)
	}
	// Maker remains intact.
	if ask, qty, ok := e.BestAsk(); !ok || !ask.Equal(dec("100")) || !qty.Equal(dec("1")) {
		t.Errorf("maker should remain: %s x %s", ask, qty)
	}
}

func TestSTP_CancelBoth(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", SelfTradePrevention: STPCancelBoth})
	e.Process(lim(t, "same", types.SideSell, "100", "1"))
	res := e.Process(lim(t, "same", types.SideBuy, "100", "1"))

	if len(res.Trades) != 0 {
		t.Fatalf("self-trade must not execute, got %d trades", len(res.Trades))
	}
	if _, _, ok := e.BestAsk(); ok {
		t.Error("maker should be cancelled too")
	}
	if e.OrderCount() != 0 {
		t.Errorf("book should be empty, count = %d", e.OrderCount())
	}
}

func TestSTP_Allow(t *testing.T) {
	e := NewEngine(Config{Symbol: "BTC-USD", SelfTradePrevention: STPAllow})
	e.Process(lim(t, "same", types.SideSell, "100", "1"))
	res := e.Process(lim(t, "same", types.SideBuy, "100", "1"))

	if len(res.Trades) != 1 {
		t.Fatalf("self-trade should execute under ALLOW, got %d trades", len(res.Trades))
	}
	if !res.Trades[0].IsSelfTrade() {
		t.Error("trade should be flagged as a self-trade")
	}
}

func TestCancel(t *testing.T) {
	e := newEngine()
	o := lim(t, "owner", types.SideBuy, "100", "1")
	e.Process(o)

	if _, err := e.Cancel(o.ID, "someone-else"); !errors.Is(err, types.ErrOrderNotFound) {
		t.Errorf("wrong-user cancel err = %v, want ErrOrderNotFound", err)
	}
	if _, err := e.Cancel(o.ID, "owner"); err != nil {
		t.Fatalf("owner cancel: %v", err)
	}
	if e.OrderCount() != 0 {
		t.Errorf("order should be gone, count = %d", e.OrderCount())
	}
	if _, err := e.Cancel(o.ID, "owner"); !errors.Is(err, types.ErrOrderNotFound) {
		t.Errorf("re-cancel err = %v, want ErrOrderNotFound", err)
	}
}

// TestQuantityConservation runs a scripted stream and checks that total traded
// quantity on the buy side equals that on the sell side (nothing created or lost).
func TestQuantityConservation(t *testing.T) {
	e := newEngine()
	var allTrades []*types.Trade

	script := []*types.Order{
		lim(t, "s1", types.SideSell, "101", "5"),
		lim(t, "s2", types.SideSell, "102", "5"),
		lim(t, "b1", types.SideBuy, "100", "3"),
		lim(t, "b2", types.SideBuy, "102", "7"), // crosses s1 (5) then s2 (2)
		ord(t, "b3", types.SideBuy, types.OrderTypeMarket, "0", "2", types.TIFImmediateOrCancel),
	}
	for _, o := range script {
		res := e.Process(o)
		allTrades = append(allTrades, res.Trades...)
	}

	traded := decimal.Zero
	for _, tr := range allTrades {
		traded = traded.Add(tr.Quantity)
	}
	// b2 fills 7, b3 fills the remaining 1 of s2's leftover (5-2=3 left, wait 5+5 asks,
	// b2 takes 5 + 2 = 7, leaving s2 with 3; b3 market buy 2 takes 2 from s2) => 7 + 2 = 9.
	if !traded.Equal(dec("9")) {
		t.Errorf("total traded = %s, want 9", traded)
	}
}
