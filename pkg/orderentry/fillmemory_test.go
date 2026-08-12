package orderentry

import "testing"

// The fill memory is documented as bounded, and until this test existed that was a
// comment rather than a fact. A soak found it still allocating half an hour in and
// the only way to tell "still filling toward its cap" from "not bounded at all" was
// to wait — which is the wrong tool for a question that is deterministic.
//
// See docs/TESTING.md: a claim with no test is a claim nobody can check.

func TestFillMemoryStaysBounded(t *testing.T) {
	r := NewRegistry("INC1", 128)
	r.SetFillMemory(1000)

	// Ten times the cap, each a distinct trade.
	for i := 1; i <= 10_000; i++ {
		r.mu.Lock()
		r.rememberFillLocked(int64(i), "alice", "cl-1")
		r.mu.Unlock()
	}

	r.mu.RLock()
	fills, order := len(r.fills), len(r.fillOrder)
	r.mu.RUnlock()

	if fills > 1000 {
		t.Errorf("fills map holds %d entries, cap is 1000 — the bound does not bind", fills)
	}
	if order > 1000 {
		t.Errorf("fillOrder holds %d entries, cap is 1000", order)
	}
	// And the two must agree, or eviction is deleting keys the map never had (or
	// leaving keys the queue has forgotten, which is the leak this guards).
	if fills != order {
		t.Errorf("fills=%d fillOrder=%d — the map and its eviction queue disagree", fills, order)
	}
}

// TestFillMemoryEvictsOldestFirst — the routing contract is that a bust of a RECENT
// print is deliverable. If eviction dropped arbitrary entries, recency would buy
// nothing and the bound would be worthless.
func TestFillMemoryEvictsOldestFirst(t *testing.T) {
	r := NewRegistry("INC1", 128)
	r.SetFillMemory(3)

	for i := int64(1); i <= 5; i++ {
		r.mu.Lock()
		r.rememberFillLocked(i, "alice", "cl-1")
		r.mu.Unlock()
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, gone := range []int64{1, 2} {
		if _, ok := r.fills[gone]; ok {
			t.Errorf("trade %d survived eviction; the oldest should go first", gone)
		}
	}
	for _, kept := range []int64{3, 4, 5} {
		if _, ok := r.fills[kept]; !ok {
			t.Errorf("trade %d was evicted while older entries remained", kept)
		}
	}
}

// TestFillMemoryDoesNotDoubleCountASecondSide — rememberFillLocked is called once
// per side of a trade with the same id. If the second call queued the id again, the
// eviction queue would run at twice the map's rate and evict live entries early.
func TestFillMemoryDoesNotDoubleCountASecondSide(t *testing.T) {
	r := NewRegistry("INC1", 128)
	r.SetFillMemory(100)

	r.mu.Lock()
	r.rememberFillLocked(42, "alice", "a-1")
	r.rememberFillLocked(42, "bob", "b-1")
	r.mu.Unlock()

	r.mu.RLock()
	defer r.mu.RUnlock()
	if got := len(r.fillOrder); got != 1 {
		t.Errorf("fillOrder holds %d entries for one trade, want 1", got)
	}
	if got := r.fills[42].n; got != 2 {
		t.Errorf("trade 42 recorded %d counterparties, want 2", got)
	}
}
