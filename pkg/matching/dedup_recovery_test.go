package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// dedupCfg returns a config with the client-order-id guard enabled.
func dedupCfg(ring int) Config {
	c := DefaultConfig("X")
	c.DedupClientOrderIDs = ring
	return c
}

// dedupOrder builds a fresh limit order carrying the given client id. Each call
// returns a distinct *types.Order so a resubmit is a genuine second submission,
// not the same pointer handed in twice.
func dedupOrder(t *testing.T, user, clientID string) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", types.SideBuy, types.OrderTypeLimit, 100, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	o.ClientOrderID = clientID
	return o
}

// TestDedupGuardSurvivesSnapshot is the regression for the guard silently
// emptying across restore: a client that resends after a venue restart used to
// double-book, which is exactly the FIX PossDup case the guard exists for.
func TestDedupGuardSurvivesSnapshot(t *testing.T) {
	cfg := dedupCfg(1024)
	e := NewEngine(cfg)

	e.Process(dedupOrder(t, "u1", "cid-1"))
	if got := e.Process(dedupOrder(t, "u1", "cid-1")).RejectionReason; got != types.ErrDuplicateClientOrderID {
		t.Fatalf("live duplicate: rejection = %v, want %v", got, types.ErrDuplicateClientOrderID)
	}

	e2, err := RestoreEngine(cfg, e.TakeSnapshot())
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}
	if got := e2.Book().Count(); got != 1 {
		t.Fatalf("restored book count = %d, want 1", got)
	}
	if got := e2.Process(dedupOrder(t, "u1", "cid-1")).RejectionReason; got != types.ErrDuplicateClientOrderID {
		t.Errorf("duplicate after restore: rejection = %v, want %v (book now %d)",
			got, types.ErrDuplicateClientOrderID, e2.Book().Count())
	}
	if got := e2.Book().Count(); got != 1 {
		t.Errorf("restored book count after duplicate = %d, want 1 (double-booked)", got)
	}
}

// TestDedupGuardSurvivesReplay covers the other half: recordClientOrderID used to
// short-circuit whenever the engine was in replay mode, so rebuilding a book from
// the WAL produced a book with no guard behind it.
func TestDedupGuardSurvivesReplay(t *testing.T) {
	cfg := dedupCfg(1024)
	e := NewEngine(cfg)

	e.SetReplaying(true)
	e.Process(dedupOrder(t, "u1", "cid-1"))
	e.SetReplaying(false)

	if got := e.Book().Count(); got != 1 {
		t.Fatalf("post-replay book count = %d, want 1", got)
	}
	if got := e.Process(dedupOrder(t, "u1", "cid-1")).RejectionReason; got != types.ErrDuplicateClientOrderID {
		t.Errorf("duplicate after replay: rejection = %v, want %v (book now %d)",
			got, types.ErrDuplicateClientOrderID, e.Book().Count())
	}
}

// TestDedupSnapshotKeysAreChronological pins the ordering contract that lets a
// restore into a smaller ring keep the newest keys rather than the oldest.
func TestDedupSnapshotKeysAreChronological(t *testing.T) {
	e := NewEngine(dedupCfg(4))
	for _, id := range []string{"a", "b", "c"} {
		e.Process(dedupOrder(t, "u1", id))
	}
	snap := e.TakeSnapshot()
	want := []string{"u1\x00a", "u1\x00b", "u1\x00c"}
	if len(snap.DedupKeys) != len(want) {
		t.Fatalf("DedupKeys = %q, want %q", snap.DedupKeys, want)
	}
	for i := range want {
		if snap.DedupKeys[i] != want[i] {
			t.Fatalf("DedupKeys = %q, want %q", snap.DedupKeys, want)
		}
	}

	// Restoring into a 2-slot ring must retain the two most recent ids.
	e2, err := RestoreEngine(dedupCfg(2), snap)
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}
	if got := e2.Process(dedupOrder(t, "u1", "c")).RejectionReason; got != types.ErrDuplicateClientOrderID {
		t.Errorf("newest id after restore into smaller ring: rejection = %v, want duplicate", got)
	}
	if got := e2.Process(dedupOrder(t, "u1", "a")).RejectionReason; got != nil {
		t.Errorf("evicted oldest id: rejection = %v, want nil (should be resubmittable)", got)
	}
}

// TestDedupRingEvictionUnchanged guards the refactor: recording the same key
// twice must not consume two ring slots and shrink the effective window.
func TestDedupRingEvictionUnchanged(t *testing.T) {
	e := NewEngine(dedupCfg(2))
	e.Process(dedupOrder(t, "u1", "a"))
	e.Process(dedupOrder(t, "u1", "a")) // rejected duplicate, must not re-record
	e.Process(dedupOrder(t, "u1", "b"))

	if got := e.Process(dedupOrder(t, "u1", "a")).RejectionReason; got != types.ErrDuplicateClientOrderID {
		t.Errorf("id 'a' should still be tracked in a 2-slot ring: rejection = %v", got)
	}
}
