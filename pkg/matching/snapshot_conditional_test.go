package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func limitOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestSnapshotRestoresIcebergReserve pins the reserve surviving a restore. It used
// to be dropped, so an iceberg came back as a bare displayed slice and silently
// lost the bulk of its size.
func TestSnapshotRestoresIcebergReserve(t *testing.T) {
	cfg := DefaultConfig("X")
	e := NewEngine(cfg)

	ib, err := types.NewIcebergOrder(limitOrder(t, "mm", types.SideBuy, 100, 1000), 100)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	e.ProcessIceberg(ib)

	e2, err := RestoreEngine(cfg, e.TakeSnapshot())
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}

	got, ok := e2.icebergOrders[ib.Order.ID]
	if !ok {
		t.Fatalf("iceberg %d absent after restore", ib.Order.ID)
	}
	if got.Hidden != ib.Hidden {
		t.Errorf("hidden reserve = %d, want %d", got.Hidden, ib.Hidden)
	}
	if got.TotalRemaining() != ib.TotalRemaining() {
		t.Errorf("total remaining = %d, want %d", got.TotalRemaining(), ib.TotalRemaining())
	}
	// The registry and the book must share one order, or a fill never refills.
	if got.Order != e2.Book().Orders()[0] {
		t.Error("restored iceberg does not share its *Order with the book")
	}
}

// TestSnapshotRestoredIcebergStillRefills is the behavioural half: the reserve is
// only genuinely restored if trading against the visible slice reloads it.
func TestSnapshotRestoredIcebergStillRefills(t *testing.T) {
	cfg := DefaultConfig("X")
	e := NewEngine(cfg)

	ib, err := types.NewIcebergOrder(limitOrder(t, "mm", types.SideBuy, 100, 1000), 100)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	e.ProcessIceberg(ib)

	e2, err := RestoreEngine(cfg, e.TakeSnapshot())
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}
	before := e2.icebergOrders[ib.Order.ID].TotalRemaining()

	// Sweep the visible slice.
	e2.Process(limitOrder(t, "taker", types.SideSell, 100, 100))

	got, ok := e2.icebergOrders[ib.Order.ID]
	if !ok {
		t.Fatal("iceberg gone after its visible slice was consumed")
	}
	if got.TotalRemaining() != before-100 {
		t.Errorf("total remaining = %d, want %d", got.TotalRemaining(), before-100)
	}
	if got.Order.RemainingQty == 0 {
		t.Error("visible slice was not refilled from the reserve after restore")
	}
}

// TestSnapshotRestoresTrailingStop covers the worst of the four: a trailing stop
// lives only in the engine's map, never in the book, so an incomplete snapshot
// dropped the order entirely rather than merely degrading it.
func TestSnapshotRestoresTrailingStop(t *testing.T) {
	cfg := DefaultConfig("X")
	e := NewEngine(cfg)

	ts, err := types.NewTrailingStop(marketOrder(t, "u1", types.SideSell, 10), 5)
	if err != nil {
		t.Fatalf("NewTrailingStop: %v", err)
	}
	ts.Observe(120) // ratchet up so the restored stop must remember 120, not restart
	e.ProcessTrailingStop(ts)

	wantStop := ts.StopPrice()

	e2, err := RestoreEngine(cfg, e.TakeSnapshot())
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}
	got, ok := e2.trailingStops[ts.Order.ID]
	if !ok {
		t.Fatalf("trailing stop %d vanished across restore", ts.Order.ID)
	}
	if got.StopPrice() != wantStop {
		t.Errorf("restored stop price = %d, want %d (ratchet lost)", got.StopPrice(), wantStop)
	}

	// The ratchet must not retreat: a worse price leaves the trigger where it was.
	got.Observe(100)
	if got.StopPrice() != wantStop {
		t.Errorf("stop retreated to %d after an unfavourable observation, want %d", got.StopPrice(), wantStop)
	}
}

// TestSnapshotRestoresOCOPairing pins the association between legs. Losing it
// left two independent orders, either of which could fill — a double execution
// of what the client submitted as an either-or.
func TestSnapshotRestoresOCOPairing(t *testing.T) {
	cfg := DefaultConfig("X")
	e := NewEngine(cfg)

	primary := limitOrder(t, "u1", types.SideSell, 120, 10)
	stopOrder := marketOrder(t, "u1", types.SideSell, 10)
	stop, err := types.NewStopOrder(stopOrder, 80)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	oco, err := types.NewOCOOrder(primary, stop)
	if err != nil {
		t.Fatalf("NewOCOOrder: %v", err)
	}
	e.ProcessOCO(oco)

	e2, err := RestoreEngine(cfg, e.TakeSnapshot())
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}

	gotPrimary, ok := e2.ocoByOrderID[primary.ID]
	if !ok {
		t.Fatalf("OCO pairing for primary %d lost across restore", primary.ID)
	}
	gotStop, ok := e2.ocoByOrderID[stopOrder.ID]
	if !ok {
		t.Fatalf("OCO pairing for stop leg %d lost across restore", stopOrder.ID)
	}
	if gotPrimary != gotStop {
		t.Error("legs restored into two different pairs; they must share one *OCOOrder")
	}
	if gotPrimary.Primary.ID != primary.ID || gotPrimary.Stop.Order.ID != stopOrder.ID {
		t.Errorf("restored pair = (primary %d, stop %d), want (%d, %d)",
			gotPrimary.Primary.ID, gotPrimary.Stop.Order.ID, primary.ID, stopOrder.ID)
	}
}

// TestSnapshotRestoresMarkPrice guards the manipulation clamps. A zero mark price
// after restore made the first post-recovery update unconstrained, since both the
// max-step and min-depth checks are skipped when the current mark is 0.
func TestSnapshotRestoresMarkPrice(t *testing.T) {
	cfg := DefaultConfig("X")
	e := NewEngine(cfg)
	if err := e.SetMarkPrice(100); err != nil {
		t.Fatalf("SetMarkPrice: %v", err)
	}

	e2, err := RestoreEngine(cfg, e.TakeSnapshot())
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}
	if got := e2.MarkPrice(); got != 100 {
		t.Errorf("restored mark price = %d, want 100", got)
	}
}

// TestSnapshotConditionalOrdersAreDeterministic pins the ordering of the new
// slices. They are built from Go maps, whose iteration order is randomised, so
// without an explicit sort two snapshots of one engine would differ byte-for-byte
// and break the determinism the whole recovery story rests on.
func TestSnapshotConditionalOrdersAreDeterministic(t *testing.T) {
	cfg := DefaultConfig("X")
	e := NewEngine(cfg)

	for i := int64(0); i < 8; i++ {
		ib, err := types.NewIcebergOrder(limitOrder(t, "mm", types.SideBuy, 100-i, 1000), 100)
		if err != nil {
			t.Fatalf("NewIcebergOrder: %v", err)
		}
		e.ProcessIceberg(ib)
	}

	first := e.TakeSnapshot()
	for i := 0; i < 20; i++ {
		next := e.TakeSnapshot()
		if len(next.Icebergs) != len(first.Icebergs) {
			t.Fatalf("iceberg count changed between snapshots")
		}
		for j := range first.Icebergs {
			if next.Icebergs[j] != first.Icebergs[j] {
				t.Fatalf("snapshot %d differs at iceberg %d: %+v vs %+v", i, j, next.Icebergs[j], first.Icebergs[j])
			}
		}
	}
}
