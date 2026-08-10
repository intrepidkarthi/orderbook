package matching

import "fmt"

// Venue-wide identifiers: partitioning the id space instead of centralising it.
//
// A venue with more than one book needs an order id that names an order without
// also being told the symbol — an audit trail, a drop copy and a trade bust each
// want one field, not two. The obvious way to get one is a counter shared by every
// shard, and it is the wrong way: a shared counter makes a shard's ids depend on
// how its traffic interleaved with every other shard's, so replaying one shard's
// log alone would no longer reproduce its own ids. That trades the property this
// project is most confident about for one it merely wants.
//
// Partitioning keeps both. The high bits name the shard and come from config; the
// low bits are the per-shard counter that recovery already reproduces exactly.
//
// See docs/MULTI-SYMBOL.md §4.1.

const (
	// ShardIndexBits is the width of the shard field. An id must stay positive —
	// a negative id would break every ordering comparison and every omitempty in
	// the log — so the budget is 63 bits, not 64, and this is the 15 of them.
	ShardIndexBits = 15
	// SeqBits is the width of the per-shard counter: 2.8e14 orders or trades for
	// each symbol.
	SeqBits = 48

	// MaxShardIndex is the largest assignable shard index (32,767 symbols).
	MaxShardIndex = 1<<ShardIndexBits - 1
	// MaxShardSeq is the largest per-shard sequence an id can carry.
	MaxShardSeq = 1<<SeqBits - 1
)

// ComposeID builds a venue-wide id from a shard index and a per-shard sequence.
//
// Shard index 0 composes to the sequence itself, which is deliberate: a
// single-symbol venue — every deployment of this library so far — keeps the small
// dense ids it already has, and its snapshots, logs and golden vectors are
// unchanged by this feature existing.
func ComposeID(shardIndex int, seq int64) int64 {
	return int64(shardIndex)<<SeqBits | seq
}

// SplitID recovers the shard index and per-shard sequence from a venue-wide id.
// A cancel can therefore be routed to its shard by arithmetic rather than by a
// lookup that would need a lock the matching path does not want.
func SplitID(id int64) (shardIndex int, seq int64) {
	return int(id >> SeqBits), id & MaxShardSeq
}

// checkShardIndex rejects an index that would overflow its field or produce a
// negative id. It is called at construction, because a venue that discovers this
// at id-assignment time discovers it on the matching goroutine.
func checkShardIndex(i int) error {
	if i < 0 || i > MaxShardIndex {
		return fmt.Errorf("matching: shard index %d out of range [0,%d]", i, MaxShardIndex)
	}
	return nil
}
