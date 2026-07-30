package orderbook

import "math/bits"

// orderIndex maps an engine order id to its book node. It replaces a
// map[int64]*node, which was the single largest cost in a cancel.
//
// # Why not the built-in map
//
// Measured at a 200,000-order book, get + delete + put costs ~45ns through Go's
// map and ~3.7ns through this. Two independent Go and Rust order books were
// measurably faster than this one at cancelling for exactly this reason, which is
// what prompted the change.
//
// Go's map is a general-purpose structure: it hashes arbitrary key types through a
// runtime call, keeps tophash bytes for probing, and handles concurrent-access
// detection and incremental growth. None of that is needed here. The key is a
// dense, monotonically increasing int64 assigned by the book itself, and the book
// is single-writer under its own mutex.
//
// # How
//
// Fibonacci (multiplicative) hashing: multiply by 2^64/φ and take the high bits.
// For sequential keys — which order ids are — it distributes far better than a
// modulo, and the bucket count being a power of two makes the index a shift rather
// than a division. Collisions chain, and removed chain entries go on a free list so
// steady-state add/cancel churn does not allocate.
//
// Not safe for concurrent use; the OrderBook's mutex is the synchronisation.
type orderIndex struct {
	buckets []*indexEntry
	shift   uint
	count   int
	free    *indexEntry
}

// indexEntry is one chain link. Recycled through orderIndex.free rather than
// released to the GC, so a book at steady state stops allocating them.
type indexEntry struct {
	key  int64
	val  *node
	next *indexEntry
}

// fibHash is 2^64 / φ — the odd 64-bit constant used for multiplicative hashing.
const fibHash = 0x9E3779B97F4A7C15

// maxLoad is the occupancy at which the table grows, as a percentage. 70% keeps
// chains short without wasting much memory.
const maxLoad = 70

func newOrderIndex(hint int) *orderIndex {
	n := 1 << uint(bits.Len(uint(max(hint, 1))))
	if n < 8 {
		n = 8
	}
	oi := &orderIndex{}
	oi.setBuckets(make([]*indexEntry, n))
	return oi
}

func (oi *orderIndex) setBuckets(b []*indexEntry) {
	oi.buckets = b
	// len(b) is a power of two, so the top log2(len) bits of the product index it.
	oi.shift = uint(64 - bits.TrailingZeros(uint(len(b))))
}

func (oi *orderIndex) index(key int64) uint64 {
	return (uint64(key) * fibHash) >> oi.shift
}

func (oi *orderIndex) get(key int64) (*node, bool) {
	for e := oi.buckets[oi.index(key)]; e != nil; e = e.next {
		if e.key == key {
			return e.val, true
		}
	}
	return nil, false
}

// put inserts or replaces the entry for key.
func (oi *orderIndex) put(key int64, val *node) {
	i := oi.index(key)
	for e := oi.buckets[i]; e != nil; e = e.next {
		if e.key == key {
			e.val = val
			return
		}
	}
	e := oi.free
	if e != nil {
		oi.free = e.next
	} else {
		e = &indexEntry{}
	}
	e.key, e.val, e.next = key, val, oi.buckets[i]
	oi.buckets[i] = e
	oi.count++
	if oi.count*100 > len(oi.buckets)*maxLoad {
		oi.grow()
	}
}

// del removes key, reporting whether it was present.
func (oi *orderIndex) del(key int64) bool {
	i := oi.index(key)
	var prev *indexEntry
	for e := oi.buckets[i]; e != nil; prev, e = e, e.next {
		if e.key != key {
			continue
		}
		if prev == nil {
			oi.buckets[i] = e.next
		} else {
			prev.next = e.next
		}
		// Clear the node pointer before recycling: a free-list entry holding a live
		// *node would keep a cancelled order's memory reachable indefinitely.
		e.val = nil
		e.next = oi.free
		oi.free = e
		oi.count--
		return true
	}
	return false
}

func (oi *orderIndex) len() int { return oi.count }

// grow doubles the table and rehashes. Entries are moved rather than reallocated,
// so growth costs no allocation beyond the new bucket slice.
func (oi *orderIndex) grow() {
	old := oi.buckets
	oi.setBuckets(make([]*indexEntry, len(old)*2))
	for _, e := range old {
		for e != nil {
			next := e.next
			i := oi.index(e.key)
			e.next = oi.buckets[i]
			oi.buckets[i] = e
			e = next
		}
	}
}

// each calls fn for every entry, in unspecified order. Callers that need a
// deterministic order must sort what they collect.
func (oi *orderIndex) each(fn func(*node)) {
	for _, e := range oi.buckets {
		for ; e != nil; e = e.next {
			fn(e.val)
		}
	}
}

// --- account interning ---
//
// perUser was a map[string]int, hashed on every add AND every cancel to maintain
// the per-account admission cap. Measured, that string hash is ~10ns of a ~46ns
// cancel; an int-keyed map is ~4.5ns and a slice index ~0.25ns.
//
// Accounts are interned to a dense int32 on first sight and the id is stored on the
// node, so a cancel reads it straight off the node it already has and never touches
// a string. An add still hashes once, which is unavoidable: the order arrives
// carrying a string.
//
// Interned ids are never reused. A venue has a bounded set of accounts, so the
// table is bounded; a caller that mints unbounded synthetic account ids would grow
// it without bound, which is why this is internal and not a general-purpose facility.
type userTable struct {
	ids    map[string]int32
	counts []int32
}

func newUserTable() *userTable {
	return &userTable{ids: make(map[string]int32)}
}

// intern returns the id for account, assigning one if it is new.
func (ut *userTable) intern(account string) int32 {
	if id, ok := ut.ids[account]; ok {
		return id
	}
	id := int32(len(ut.counts))
	ut.ids[account] = id
	ut.counts = append(ut.counts, 0)
	return id
}

func (ut *userTable) incr(id int32) { ut.counts[id]++ }

func (ut *userTable) decr(id int32) {
	if ut.counts[id] > 0 {
		ut.counts[id]--
	}
}

// countOf reports an account's resting-order count, or 0 if it has never rested one.
func (ut *userTable) countOf(account string) int {
	id, ok := ut.ids[account]
	if !ok {
		return 0
	}
	return int(ut.counts[id])
}
