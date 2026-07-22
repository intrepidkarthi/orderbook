package orderbook

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// stop builds a resting sell/buy stop whose underlying id is seq — the id doubles
// as the deterministic trigger-ordering key.
func stop(t *testing.T, seq int64, side types.Side, stopPrice int64) *types.StopOrder {
	t.Helper()
	o, err := types.NewOrder("u", "BTC-USD", side, types.OrderTypeMarket, 0, 1, types.TIFImmediateOrCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	o.ID = seq
	s, err := types.NewStopOrder(o, stopPrice)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	return s
}

func TestStopBook_AddGetRemoveCount(t *testing.T) {
	sb := NewStopBook("BTC-USD")
	s := stop(t, 1, types.SideSell, 95)
	sb.Add(s)
	if sb.Count() != 1 {
		t.Fatalf("count = %d, want 1", sb.Count())
	}
	if got, ok := sb.Get(s.Order.ID); !ok || got != s {
		t.Error("Get did not return the added stop")
	}
	if !sb.Remove(s.Order.ID) || sb.Count() != 0 {
		t.Error("Remove failed")
	}
	if sb.Remove(999999) {
		t.Error("removing missing id should be false")
	}
}

func TestStopBook_TriggersInSequenceOrder(t *testing.T) {
	sb := NewStopBook("BTC-USD")
	// Three sell stops; only those with stop >= marketPrice fire (price fell to 94).
	sb.Add(stop(t, 3, types.SideSell, 95))
	sb.Add(stop(t, 1, types.SideSell, 96))
	sb.Add(stop(t, 2, types.SideSell, 90)) // will NOT fire at 94
	sb.Add(stop(t, 4, types.SideSell, 94))

	fired := sb.CheckTriggers(94)
	if len(fired) != 3 {
		t.Fatalf("fired = %d, want 3 (stops 96, 95, 94)", len(fired))
	}
	// Deterministic: ordered by id (1, 3, 4).
	wantSeq := []int64{1, 3, 4}
	for i, s := range fired {
		if s.Order.ID != wantSeq[i] {
			t.Errorf("fired[%d] id = %d, want %d", i, s.Order.ID, wantSeq[i])
		}
		if !s.IsTriggered() {
			t.Error("fired stop should be marked triggered")
		}
	}
	// The 90-stop remains; fired stops were removed.
	if sb.Count() != 1 {
		t.Errorf("remaining = %d, want 1 (the 90 stop)", sb.Count())
	}
}

func TestStopBook_BuyStops(t *testing.T) {
	sb := NewStopBook("BTC-USD")
	sb.Add(stop(t, 1, types.SideBuy, 105))
	sb.Add(stop(t, 2, types.SideBuy, 110))
	// Price rose to 106: only the 105 buy stop fires.
	fired := sb.CheckTriggers(106)
	if len(fired) != 1 || fired[0].Order.ID != 1 {
		t.Fatalf("expected only the 105 buy stop to fire, got %d", len(fired))
	}
}
