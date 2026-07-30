package orderentry

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func adoptOrder(t *testing.T, id int64, user, clOrdID string, qty, remaining int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", types.SideBuy, types.OrderTypeLimit, 100, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	o.ID = id
	o.ClientOrderID = clOrdID
	o.RemainingQty = remaining
	o.FilledQty = qty - remaining
	return o
}

// TestAdoptRestoresTheIndex — the minimum: a recovered order can be named again.
func TestAdoptRestoresTheIndex(t *testing.T) {
	reg := NewRegistry("INC1", 128)
	if _, ok := reg.OrderIDFor("alice", "a1"); ok {
		t.Fatal("a fresh registry resolved an id it was never told about")
	}

	reg.Adopt([]*types.Order{adoptOrder(t, 42, "alice", "a1", 10, 10)})

	id, ok := reg.OrderIDFor("alice", "a1")
	if !ok {
		t.Fatal("adopted order cannot be named")
	}
	if id != 42 {
		t.Errorf("resolved to %d, want 42", id)
	}
}

// TestAdoptUsesRemainingNotOriginalQuantity is the field that is easy to get wrong.
// fill() decrements from the adopted number, so seeding it with the order's original
// size reports a LeavesQty too high by whatever had already traded — a drift with no
// error on either side.
func TestAdoptUsesRemainingNotOriginalQuantity(t *testing.T) {
	reg := NewRegistry("INC1", 128)
	// Submitted 10, already filled 6, so 4 rests.
	reg.Adopt([]*types.Order{adoptOrder(t, 7, "alice", "a1", 10, 4)})

	_, _, leaves, ok := reg.fill(7, 1)
	if !ok {
		t.Fatal("adopted order not found on fill")
	}
	if leaves != 3 {
		t.Errorf("leaves = %d, want 3 — the order was adopted at its original size", leaves)
	}
}

// TestAdoptScopesByAccount — two accounts may legally use the same ClOrdID, and
// adoption must not let one reach the other's order.
func TestAdoptScopesByAccount(t *testing.T) {
	reg := NewRegistry("INC1", 128)
	reg.Adopt([]*types.Order{
		adoptOrder(t, 1, "alice", "same", 10, 10),
		adoptOrder(t, 2, "bob", "same", 20, 20),
	})

	aliceID, ok := reg.OrderIDFor("alice", "same")
	if !ok {
		t.Fatal("alice's order missing")
	}
	bobID, ok := reg.OrderIDFor("bob", "same")
	if !ok {
		t.Fatal("bob's order missing")
	}
	if aliceID == bobID {
		t.Fatalf("both accounts resolved to order %d — scoping was lost", aliceID)
	}
	if aliceID != 1 || bobID != 2 {
		t.Errorf("alice=%d bob=%d, want 1 and 2", aliceID, bobID)
	}
	if _, ok := reg.OrderIDFor("carol", "same"); ok {
		t.Error("an account that adopted nothing resolved another's order")
	}
}

// TestAdoptDeliversNothing — these orders were acknowledged in a previous
// incarnation. Re-announcing them would replay history into a fresh sequence space,
// and a client cannot resume across incarnations anyway.
func TestAdoptDeliversNothing(t *testing.T) {
	reg := NewRegistry("INC1", 128)
	reg.Adopt([]*types.Order{adoptOrder(t, 1, "alice", "a1", 10, 10)})

	if got := reg.Stream("alice").Seq(); got != 0 {
		t.Errorf("adoption advanced alice's sequence to %d; it must deliver nothing", got)
	}
	msgs, err := reg.Stream("alice").Since(0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("adoption queued %d messages: %v", len(msgs), kinds(msgs))
	}
}

// TestAdoptToleratesNilAndEmpty — recovery of an empty venue is the common case,
// and a nil in the slice must not take the process down on startup.
func TestAdoptToleratesNilAndEmpty(t *testing.T) {
	reg := NewRegistry("INC1", 128)
	reg.Adopt(nil)
	reg.Adopt([]*types.Order{})
	reg.Adopt([]*types.Order{nil, adoptOrder(t, 5, "alice", "a1", 10, 10)})

	if _, ok := reg.OrderIDFor("alice", "a1"); !ok {
		t.Error("a nil entry prevented the rest of the batch from being adopted")
	}
}

// TestAdoptedOrderThenLiveEventsAgree — adoption seeds the index, and the live
// event path must carry on from it rather than fight it. A fill and then a cancel on
// an adopted order must both report against the right account.
func TestAdoptedOrderThenLiveEventsAgree(t *testing.T) {
	reg := NewRegistry("INC1", 128)
	reg.Adopt([]*types.Order{adoptOrder(t, 9, "alice", "a1", 10, 10)})

	o := adoptOrder(t, 9, "alice", "a1", 10, 10)
	reg.Publish([]matching.Event{{
		Kind: matching.EventReplaced, OrderID: 9, UserID: "alice", Order: func() *types.Order {
			c := o
			c.RemainingQty = 4
			return c
		}(),
	}})

	msgs, err := reg.Stream("alice").Since(0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Kind != KindReplaced {
		t.Fatalf("got %v, want one Replaced", kinds(msgs))
	}
	if msgs[0].ClOrdID != "a1" {
		t.Errorf("Replaced names %q, want a1", msgs[0].ClOrdID)
	}
	if msgs[0].LeavesQty != 4 {
		t.Errorf("LeavesQty = %d, want 4", msgs[0].LeavesQty)
	}
	// And the adopted remaining must have been updated by that Replaced, not left
	// at the pre-reduce number.
	_, _, leaves, ok := reg.fill(9, 1)
	if !ok {
		t.Fatal("order lost after Replaced")
	}
	if leaves != 3 {
		t.Errorf("leaves = %d, want 3", leaves)
	}
}
