package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestSnapshotRestore_ThenTailReplay is the recovery contract: snapshot at
// sequence N, then a fresh engine restored from that snapshot and fed the same
// command tail (the log after N) must end byte-identical to the engine that
// processed everything continuously — including that a stop resting at snapshot
// time still fires correctly on the tail.
func TestSnapshotRestore_ThenTailReplay(t *testing.T) {
	prefix := func(e *Engine) {
		e.Process(lim(t, "b100", types.SideBuy, 100, 10))
		e.Process(lim(t, "s100", types.SideSell, 100, 5)) // trades 5 → last=100, bid 100 has 5 left
		e.Process(lim(t, "b96", types.SideBuy, 96, 5))
		e.Process(lim(t, "b95", types.SideBuy, 95, 5))
		e.ProcessStop(stopOrder(t, "trader", types.SideSell, 3, 95)) // rests pending (95 < 100)
	}
	tail := func(e *Engine) *MatchResult {
		e.Process(lim(t, "s101", types.SideSell, 101, 4))              // rests an ask
		return e.Process(marketOrder(t, "seller", types.SideSell, 11)) // sweeps bids to 95 → fires the stop
	}

	// Continuous engine.
	e1 := newEngine()
	prefix(e1)
	snap := e1.TakeSnapshot()
	r1 := tail(e1)

	// Restored engine + the same tail.
	e2, err := RestoreEngine(DefaultConfig("BTC-USD"), snap)
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}
	if e2.PendingStopCount() != 1 {
		t.Fatalf("restored engine lost the pending stop: %d", e2.PendingStopCount())
	}
	r2 := tail(e2)

	// Identical tail outcome...
	if len(r1.Trades) != len(r2.Trades) {
		t.Fatalf("tail trade count differs: continuous %d vs restored %d", len(r1.Trades), len(r2.Trades))
	}
	for i := range r1.Trades {
		if r1.Trades[i].Price != r2.Trades[i].Price || r1.Trades[i].Quantity != r2.Trades[i].Quantity {
			t.Errorf("tail trade %d differs: %+v vs %+v", i, *r1.Trades[i], *r2.Trades[i])
		}
	}
	// ...and identical resting book (id, price, remaining qty) + engine state.
	o1, o2 := e1.Book().Orders(), e2.Book().Orders()
	if len(o1) != len(o2) {
		t.Fatalf("resting count differs: %d vs %d", len(o1), len(o2))
	}
	for i := range o1 {
		if o1[i].ID != o2[i].ID || o1[i].Price != o2[i].Price || o1[i].RemainingQty != o2[i].RemainingQty {
			t.Fatalf("resting order %d differs:\n cont=%+v\n rest=%+v", i, o1[i], o2[i])
		}
	}
	if e1.PendingStopCount() != e2.PendingStopCount() {
		t.Errorf("pending stops differ: %d vs %d", e1.PendingStopCount(), e2.PendingStopCount())
	}
	if e1.LastTradePrice() != e2.LastTradePrice() {
		t.Errorf("last trade price differs: %d vs %d", e1.LastTradePrice(), e2.LastTradePrice())
	}
}

// TestLoadSnapshot_RequiresFreshEngine guards the restore precondition.
func TestLoadSnapshot_RequiresFreshEngine(t *testing.T) {
	e := newEngine()
	e.Process(lim(t, "a", types.SideBuy, 100, 1))
	if err := e.LoadSnapshot(&EngineSnapshot{Symbol: "BTC-USD"}); err == nil {
		t.Error("LoadSnapshot into a non-fresh engine should fail")
	}
}
