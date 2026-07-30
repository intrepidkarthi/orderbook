package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// RestingOrders exists so the layer above can rebuild per-order state after a
// recovery. OpenOrdersFor could not serve that: it is scoped to one account, and a
// recovering venue does not know the accounts until it has read the book.

func restingIDs(orders []*types.Order) map[int64]bool {
	out := map[int64]bool{}
	for _, o := range orders {
		out[o.ID] = true
	}
	return out
}

func TestRestingOrdersSpansAccounts(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))

	a := redOrder(t, "alice", types.SideBuy, 100, 5)
	b := redOrder(t, "bob", types.SideSell, 110, 7)
	e.Process(a)
	e.Process(b)

	got := e.RestingOrders()
	if len(got) != 2 {
		t.Fatalf("got %d orders, want 2", len(got))
	}
	ids := restingIDs(got)
	if !ids[a.ID] || !ids[b.ID] {
		t.Errorf("got ids %v, want both %d and %d", ids, a.ID, b.ID)
	}
}

// TestRestingOrdersReturnsCopies — the originals are engine-owned and the matching
// goroutine keeps mutating them. Handing out the pointers would make every caller a
// data race.
func TestRestingOrdersReturnsCopies(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "alice", types.SideBuy, 100, 5)
	e.Process(o)

	got := e.RestingOrders()
	if len(got) != 1 {
		t.Fatalf("got %d orders, want 1", len(got))
	}
	if got[0] == o {
		t.Fatal("returned the engine-owned order itself, not a copy")
	}
	got[0].Quantity = 999
	if o.Quantity == 999 {
		t.Error("mutating the returned order changed the book's copy")
	}
}

// TestRestingOrdersCarriesTheFieldsAdoptionNeeds — the point of the method is that
// the caller can rebuild an index from it, which needs the account, the client's own
// id, and what actually remains.
func TestRestingOrdersCarriesTheFieldsAdoptionNeeds(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))

	maker := redOrder(t, "alice", types.SideBuy, 100, 10)
	maker.ClientOrderID = "a1"
	e.Process(maker)
	// Fill 6, leaving 4 resting.
	e.Process(redOrder(t, "bob", types.SideSell, 100, 6))

	got := e.RestingOrders()
	if len(got) != 1 {
		t.Fatalf("got %d orders, want 1 (the partly-filled maker)", len(got))
	}
	o := got[0]
	if o.UserID != "alice" {
		t.Errorf("UserID = %q, want alice", o.UserID)
	}
	if o.ClientOrderID != "a1" {
		t.Errorf("ClientOrderID = %q, want a1", o.ClientOrderID)
	}
	if o.Quantity != 10 {
		t.Errorf("Quantity = %d, want 10", o.Quantity)
	}
	if o.RemainingQty != 4 {
		t.Errorf("RemainingQty = %d, want 4 — an index seeded from this would over-report what is live", o.RemainingQty)
	}
}

// TestRestingOrdersExcludesWhatIsNotResting — a fully filled order is gone, and
// pending stops are not resting orders. Adopting either would create index entries
// for things the book does not hold.
func TestRestingOrdersExcludesWhatIsNotResting(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))

	maker := redOrder(t, "alice", types.SideBuy, 100, 5)
	e.Process(maker)
	e.Process(redOrder(t, "bob", types.SideSell, 100, 5)) // consumes it entirely

	stop, err := types.NewStopOrder(redOrder(t, "carol", types.SideBuy, 130, 3), 120)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	e.ProcessStop(stop)
	if e.PendingStopCount() == 0 {
		t.Fatal("the stop did not rest; this test needs one pending")
	}

	if got := e.RestingOrders(); len(got) != 0 {
		t.Errorf("got %d orders, want 0 — a filled order or a pending stop was included", len(got))
	}
}

func TestRestingOrdersOnAnEmptyBook(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	if got := e.RestingOrders(); len(got) != 0 {
		t.Errorf("got %d orders from an empty book", len(got))
	}
}
