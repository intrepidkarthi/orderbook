package matching

import (
	"sync"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestShards_RoutesAndIsolates checks orders route to their symbol's shard and
// distinct symbols keep independent books.
func TestShards_RoutesAndIsolates(t *testing.T) {
	sh := NewShards(ShardsConfig{})
	defer sh.Close()

	mk := func(sym, user string, side types.Side, price, qty int64) *types.Order {
		o, err := types.NewOrder(user, sym, side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		return o
	}

	sh.Process(mk("AAA", "mm", types.SideSell, 100, 5))
	sh.Process(mk("BBB", "mm", types.SideSell, 200, 7))

	// Each symbol's book is independent.
	if bid, qty, _ := sh.Runner("AAA").BestAsk(); bid != 100 || qty != 5 {
		t.Errorf("AAA best ask = %d x %d, want 100 x 5", bid, qty)
	}
	if bid, qty, _ := sh.Runner("BBB").BestAsk(); bid != 200 || qty != 7 {
		t.Errorf("BBB best ask = %d x %d, want 200 x 7", bid, qty)
	}
	// A buy on AAA never touches BBB.
	sh.Process(mk("AAA", "t", types.SideBuy, 100, 5))
	if sh.Runner("AAA").OrderCount() != 0 {
		t.Errorf("AAA should be empty after the fill")
	}
	if sh.Runner("BBB").OrderCount() != 1 {
		t.Errorf("BBB should still hold its resting ask")
	}
	if got := sh.Symbols(); len(got) != 2 || got[0] != "AAA" || got[1] != "BBB" {
		t.Errorf("symbols = %v, want [AAA BBB]", got)
	}
}

// TestShards_ConcurrentProducers hammers many symbols from many goroutines; the
// per-symbol single writers must keep each book consistent under -race.
func TestShards_ConcurrentProducers(t *testing.T) {
	sh := NewShards(ShardsConfig{QueueSize: 64})
	symbols := []string{"S0", "S1", "S2", "S3"}

	var wg sync.WaitGroup
	for p := 0; p < 8; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				sym := symbols[i%len(symbols)]
				side := types.SideBuy
				price := int64(90 + i%10)
				if i%2 == 0 {
					side, price = types.SideSell, int64(110-i%10)
				}
				o, err := types.NewOrder("u", sym, side, types.OrderTypeLimit, price, 1, types.TIFGoodTillCancel)
				if err != nil {
					t.Errorf("NewOrder: %v", err)
					return
				}
				sh.Process(o)
			}
		}(p)
	}
	wg.Wait()

	for _, sym := range symbols {
		r := sh.Runner(sym)
		if bid, _, okB := r.BestBid(); okB {
			if ask, _, okA := r.BestAsk(); okA && bid >= ask {
				t.Errorf("%s book crossed: bid %d >= ask %d", sym, bid, ask)
			}
		}
	}
	sh.Close()
}
