package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func proRataEngine() *Engine {
	return NewEngine(Config{Symbol: "BTC-USD", ProRata: true})
}

func sumQty(trades []*types.Trade) int64 {
	var s int64
	for _, t := range trades {
		s += t.Quantity
	}
	return s
}

func TestProRata_AllocatesProportionally(t *testing.T) {
	e := proRataEngine()
	e.Process(lim(t, "a", types.SideSell, 100, 10)) // 25% of the level
	e.Process(lim(t, "b", types.SideSell, 100, 30)) // 75% of the level

	// Buy 20 at 100 → a gets 5 (20·10/40), b gets 15 (20·30/40).
	res := e.Process(lim(t, "taker", types.SideBuy, 100, 20))
	if len(res.Trades) != 2 {
		t.Fatalf("trades = %d, want 2", len(res.Trades))
	}
	if res.Trades[0].Quantity != 5 || res.Trades[1].Quantity != 15 {
		t.Errorf("allocations = %d, %d; want 5, 15", res.Trades[0].Quantity, res.Trades[1].Quantity)
	}
	// Both makers remain, level now holds 20 (40 − 20).
	if _, qty, _ := e.BestAsk(); qty != 20 {
		t.Errorf("level qty after = %d, want 20", qty)
	}
}

func TestProRata_FullConsume(t *testing.T) {
	e := proRataEngine()
	e.Process(lim(t, "a", types.SideSell, 100, 10))
	e.Process(lim(t, "b", types.SideSell, 100, 30))

	res := e.Process(lim(t, "taker", types.SideBuy, 100, 40))
	if sumQty(res.Trades) != 40 {
		t.Errorf("total filled = %d, want 40", sumQty(res.Trades))
	}
	if _, _, ok := e.BestAsk(); ok {
		t.Error("level should be fully consumed")
	}
}

func TestProRata_RemainderSumsExactly(t *testing.T) {
	e := proRataEngine()
	// Three equal orders (10 each). Buy 10 → each ≈3.3333; the remainder must be
	// distributed so the total is exactly 10 (quantity conservation).
	e.Process(lim(t, "a", types.SideSell, 100, 10))
	e.Process(lim(t, "b", types.SideSell, 100, 10))
	e.Process(lim(t, "c", types.SideSell, 100, 10))

	res := e.Process(lim(t, "taker", types.SideBuy, 100, 10))
	if sumQty(res.Trades) != 10 {
		t.Errorf("total filled = %d, want exactly 10", sumQty(res.Trades))
	}
	// Level had 30, 10 consumed ⇒ 20 remains.
	if _, qty, _ := e.BestAsk(); qty != 20 {
		t.Errorf("level qty after = %d, want 20", qty)
	}
}
