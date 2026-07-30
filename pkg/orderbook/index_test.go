package orderbook

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// orderIndex replaces a map[int64]*node, so it has to behave like one in every way
// the book relies on — including the cases a map gets right for free: growth,
// collisions, and reuse after deletion.

func TestOrderIndexRoundTrip(t *testing.T) {
	oi := newOrderIndex(8)
	nodes := make([]node, 100)
	for i := range nodes {
		oi.put(int64(i+1), &nodes[i])
	}
	if oi.len() != 100 {
		t.Fatalf("len = %d, want 100", oi.len())
	}
	for i := range nodes {
		got, ok := oi.get(int64(i + 1))
		if !ok {
			t.Fatalf("key %d missing", i+1)
		}
		if got != &nodes[i] {
			t.Errorf("key %d returned the wrong node", i+1)
		}
	}
	if _, ok := oi.get(0); ok {
		t.Error("returned a value for a key never inserted")
	}
	if _, ok := oi.get(101); ok {
		t.Error("returned a value for a key past the end")
	}
}

// TestOrderIndexGrowsWithoutLosingEntries — growth rehashes every chain, and a bug
// there loses orders silently, which in a book means orders that can never be
// cancelled.
func TestOrderIndexGrowsWithoutLosingEntries(t *testing.T) {
	oi := newOrderIndex(8) // deliberately tiny, so this grows many times
	const n = 10_000
	nodes := make([]node, n)
	for i := range nodes {
		oi.put(int64(i+1), &nodes[i])
	}
	if oi.len() != n {
		t.Fatalf("len = %d, want %d", oi.len(), n)
	}
	if len(oi.buckets) < n {
		t.Errorf("only %d buckets for %d entries — the table did not grow", len(oi.buckets), n)
	}
	for i := range nodes {
		if got, ok := oi.get(int64(i + 1)); !ok || got != &nodes[i] {
			t.Fatalf("key %d lost or wrong after growth", i+1)
		}
	}
}

func TestOrderIndexDelete(t *testing.T) {
	oi := newOrderIndex(64)
	nodes := make([]node, 50)
	for i := range nodes {
		oi.put(int64(i+1), &nodes[i])
	}
	// Delete every other key.
	for i := 1; i <= 50; i += 2 {
		if !oi.del(int64(i)) {
			t.Fatalf("del(%d) reported the key absent", i)
		}
	}
	if oi.len() != 25 {
		t.Fatalf("len = %d, want 25", oi.len())
	}
	for i := 1; i <= 50; i++ {
		_, ok := oi.get(int64(i))
		if i%2 == 1 && ok {
			t.Errorf("key %d survived deletion", i)
		}
		if i%2 == 0 && !ok {
			t.Errorf("key %d was deleted along with its neighbour", i)
		}
	}
	if oi.del(999) {
		t.Error("del reported success for a key that was never present")
	}
}

// TestOrderIndexRecyclesEntries — the free list is why steady-state churn does not
// allocate. If it leaks, a long-running venue allocates one entry per cancel forever.
func TestOrderIndexRecyclesEntries(t *testing.T) {
	oi := newOrderIndex(1024)
	nodes := make([]node, 500)
	for i := range nodes {
		oi.put(int64(i+1), &nodes[i])
	}
	for i := range nodes {
		oi.del(int64(i + 1))
	}
	if oi.len() != 0 {
		t.Fatalf("len = %d after deleting everything", oi.len())
	}
	// Count what is on the free list; every deleted entry should be there.
	free := 0
	for e := oi.free; e != nil; e = e.next {
		free++
		if e.val != nil {
			t.Fatal("a recycled entry still holds a node pointer, which pins a cancelled order in memory")
		}
	}
	if free != 500 {
		t.Errorf("free list holds %d entries, want 500 — deleted entries are being dropped instead of recycled", free)
	}
	// Reinserting must reuse them rather than allocate.
	for i := range nodes {
		oi.put(int64(i+1), &nodes[i])
	}
	if oi.free != nil {
		t.Error("free list was not drained on reinsertion")
	}
}

// TestOrderIndexHandlesCollisions forces two keys into one bucket, which is the
// case a chained table exists to handle and the one a bad delete breaks.
func TestOrderIndexHandlesCollisions(t *testing.T) {
	oi := newOrderIndex(8)
	// With 8 buckets the index is the top 3 bits of key*fibHash. Find three keys
	// that share a bucket rather than assuming which ones do.
	var same []int64
	target := oi.index(1)
	for k := int64(1); k < 100_000 && len(same) < 3; k++ {
		if oi.index(k) == target {
			same = append(same, k)
		}
	}
	if len(same) < 3 {
		t.Skip("could not find three colliding keys")
	}
	nodes := make([]node, 3)
	for i, k := range same {
		oi.put(k, &nodes[i])
	}
	// Delete the middle of the chain — the case that breaks a wrong prev pointer.
	if !oi.del(same[1]) {
		t.Fatal("del of a chained key failed")
	}
	if _, ok := oi.get(same[1]); ok {
		t.Error("deleted key still present")
	}
	for _, i := range []int{0, 2} {
		if got, ok := oi.get(same[i]); !ok || got != &nodes[i] {
			t.Errorf("key %d lost when its chain neighbour was deleted", same[i])
		}
	}
}

func TestOrderIndexPutReplaces(t *testing.T) {
	oi := newOrderIndex(16)
	a, b := &node{}, &node{}
	oi.put(7, a)
	oi.put(7, b)
	if oi.len() != 1 {
		t.Errorf("len = %d after replacing a key, want 1", oi.len())
	}
	if got, _ := oi.get(7); got != b {
		t.Error("put did not replace the existing value")
	}
}

func TestOrderIndexEach(t *testing.T) {
	oi := newOrderIndex(8)
	nodes := make([]node, 200)
	for i := range nodes {
		oi.put(int64(i+1), &nodes[i])
	}
	seen := 0
	oi.each(func(n *node) {
		if n == nil {
			t.Fatal("each yielded a nil node")
		}
		seen++
	})
	if seen != 200 {
		t.Errorf("each visited %d entries, want 200", seen)
	}
}

// --- account interning ---

func TestUserTableInterning(t *testing.T) {
	ut := newUserTable()
	a1 := ut.intern("alice")
	a2 := ut.intern("alice")
	b := ut.intern("bob")
	if a1 != a2 {
		t.Errorf("alice interned to %d then %d — ids must be stable", a1, a2)
	}
	if a1 == b {
		t.Error("alice and bob share an id")
	}

	ut.incr(a1)
	ut.incr(a1)
	ut.incr(b)
	if got := ut.countOf("alice"); got != 2 {
		t.Errorf("alice count = %d, want 2", got)
	}
	if got := ut.countOf("bob"); got != 1 {
		t.Errorf("bob count = %d, want 1", got)
	}
	if got := ut.countOf("carol"); got != 0 {
		t.Errorf("an account that never rested an order counts %d, want 0", got)
	}

	ut.decr(a1)
	if got := ut.countOf("alice"); got != 1 {
		t.Errorf("alice count = %d after one decrement, want 1", got)
	}
	// Decrementing past zero must not underflow into a huge count, which would
	// then wrongly refuse orders under a per-account cap.
	ut.decr(b)
	ut.decr(b)
	if got := ut.countOf("bob"); got != 0 {
		t.Errorf("bob count = %d after over-decrementing, want 0", got)
	}
}

// TestPerUserCountSurvivesInterning is the book-level check: the admission cap reads
// through the interned table now, and a cancel decrements using the id cached on the
// node rather than re-hashing the account string.
func TestPerUserCountSurvivesInterning(t *testing.T) {
	ob := New(Config{Symbol: "X", MaxOrders: 100})
	mk := func(user string, price int64) *types.Order {
		o, err := types.NewOrder(user, "X", types.SideBuy, types.OrderTypeLimit, price, 5, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		return o
	}
	a1, a2, b1 := mk("alice", 100), mk("alice", 99), mk("bob", 98)
	for _, o := range []*types.Order{a1, a2, b1} {
		if err := ob.Add(o); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if got := ob.OrdersByUser("alice"); got != 2 {
		t.Errorf("alice has %d resting, want 2", got)
	}
	if got := ob.OrdersByUser("bob"); got != 1 {
		t.Errorf("bob has %d resting, want 1", got)
	}
	if _, err := ob.Remove(a1.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := ob.OrdersByUser("alice"); got != 1 {
		t.Errorf("alice has %d resting after one cancel, want 1", got)
	}
	if got := ob.OrdersByUser("bob"); got != 1 {
		t.Errorf("bob's count changed when alice cancelled: %d", got)
	}
}
