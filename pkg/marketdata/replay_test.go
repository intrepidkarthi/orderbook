package marketdata

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func lim(t *testing.T, user string, side types.Side, price, qty string) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, dec(price), dec(qty), types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

func engine() *matching.Engine { return matching.NewEngine(matching.DefaultConfig("BTC-USD")) }

// scriptOrders is a fixed set of orders that produce several trades.
func scriptOrders(t *testing.T) []*types.Order {
	return []*types.Order{
		lim(t, "s1", types.SideSell, "101", "5"),
		lim(t, "s2", types.SideSell, "102", "5"),
		lim(t, "b1", types.SideBuy, "100", "3"),
		lim(t, "b2", types.SideBuy, "102", "7"), // crosses s1 then s2
		lim(t, "b3", types.SideBuy, "101", "1"),
	}
}

func TestRecordAndReplay_Deterministic(t *testing.T) {
	orders := scriptOrders(t)

	// Run 1: record through a recorder.
	rec := NewRecorder(engine())
	for _, o := range orders {
		rec.Process(o)
	}
	stream := rec.Stream()
	if stream.Len() != len(orders) {
		t.Fatalf("stream len = %d, want %d", stream.Len(), len(orders))
	}
	digest1 := Digest(rec.Trades())

	// Run 2: replay the recorded stream through a fresh engine.
	replayTrades := Replay(engine(), stream)
	digest2 := Digest(replayTrades)

	if digest1 != digest2 {
		t.Errorf("replay digest mismatch:\n run1=%s\n run2=%s", digest1, digest2)
	}
	if len(rec.Trades()) == 0 {
		t.Fatal("expected trades to be recorded")
	}
	if len(replayTrades) != len(rec.Trades()) {
		t.Errorf("replay produced %d trades, recorded %d", len(replayTrades), len(rec.Trades()))
	}
}

func TestReplay_Repeatable(t *testing.T) {
	stream := NewStream(scriptOrders(t)...)
	// Replaying the same stream twice yields the same digest.
	d1 := Digest(Replay(engine(), stream))
	d2 := Digest(Replay(engine(), stream))
	if d1 != d2 {
		t.Errorf("repeat replay mismatch: %s vs %s", d1, d2)
	}
	// The stream's templates are not consumed/mutated by replay.
	if stream.Len() != 5 {
		t.Errorf("stream len after replay = %d, want 5", stream.Len())
	}
}

func TestDigest_Distinguishes(t *testing.T) {
	base := Digest(Replay(engine(), NewStream(scriptOrders(t)...)))

	// A different flow (extra crossing order) must change the digest.
	extra := append(scriptOrders(t), lim(t, "b4", types.SideBuy, "102", "2"))
	other := Digest(Replay(engine(), NewStream(extra...)))

	if base == other {
		t.Error("digest should differ for different order flow")
	}
	// Empty tape has a well-defined, stable digest.
	if Digest(nil) != Digest([]*types.Trade{}) {
		t.Error("empty digests should match")
	}
}

func TestValueDigest(t *testing.T) {
	stream := NewStream(scriptOrders(t)...)
	// Stable across replays (matching outcome is deterministic).
	if v1, v2 := ValueDigest(Replay(engine(), stream)), ValueDigest(Replay(engine(), stream)); v1 != v2 {
		t.Errorf("ValueDigest not stable across replay: %s vs %s", v1, v2)
	}
	// Distinguishes a different flow.
	extra := append(scriptOrders(t), lim(t, "b4", types.SideBuy, "102", "2"))
	if ValueDigest(Replay(engine(), stream)) == ValueDigest(Replay(engine(), NewStream(extra...))) {
		t.Error("ValueDigest should differ for different flow")
	}
	// Value fingerprint (no ids) differs from the id-inclusive Digest.
	trades := Replay(engine(), stream)
	if ValueDigest(trades) == Digest(trades) {
		t.Error("ValueDigest and Digest hash different content; should not collide here")
	}
}
