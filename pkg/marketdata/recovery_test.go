package marketdata

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// bookDigest fingerprints an order book's full resting state (every order, in
// price-time order, on both sides) independent of wall-clock — the thing a
// recovered engine must reproduce exactly.
func bookDigest(b *orderbook.OrderBook) string {
	s := b.SnapshotL3(1_000_000)
	var h []byte
	add := func(side string, os []orderbook.L3Order) {
		for _, o := range os {
			h = append(h, side...)
			h = strconv.AppendInt(h, o.Price, 10)
			h = append(h, '|')
			h = strconv.AppendInt(h, o.Quantity, 10)
			h = append(h, '|')
			h = strconv.AppendInt(h, o.OrderID, 10)
			h = append(h, '|')
			h = append(h, o.UserID...)
			h = append(h, '\n')
		}
	}
	add("B", s.Bids)
	add("A", s.Asks)
	sum := sha256.Sum256(h)
	return hex.EncodeToString(sum[:])
}

// TestRecovery_ReplayRebuildsBook is the snapshot/command-log recovery property:
// a fresh engine that replays the recorded command log rebuilds byte-identical
// resting book state — the basis for crash recovery from a WAL.
func TestRecovery_ReplayRebuildsBook(t *testing.T) {
	// A flow that leaves resting liquidity on both sides plus partial fills.
	orders := []*types.Order{
		lim(t, "mm1", types.SideSell, 105, 10),
		lim(t, "mm2", types.SideSell, 106, 8),
		lim(t, "mm3", types.SideBuy, 100, 12),
		lim(t, "mm4", types.SideBuy, 99, 6),
		lim(t, "t1", types.SideBuy, 105, 4),  // partially fills mm1 (6 left)
		lim(t, "t2", types.SideSell, 100, 5), // partially fills mm3 (7 left)
	}
	log := NewStream(orders...)

	// The "live" book, built by applying the log.
	live := engine()
	Replay(live, log)

	// Recovery: a brand-new engine replays the same log from scratch.
	recovered := engine()
	Replay(recovered, log)

	if got, want := bookDigest(recovered.Book()), bookDigest(live.Book()); got != want {
		t.Errorf("recovered book != live book\n live=%s\n recovered=%s", want, got)
	}
	if recovered.OrderCount() != live.OrderCount() {
		t.Errorf("resting count differs: live %d vs recovered %d", live.OrderCount(), recovered.OrderCount())
	}
}

// TestRecovery_SnapshotThenTailReplay models WAL recovery from a mid-stream
// snapshot: rebuild the prefix, capture where we are, then apply only the tail —
// the result must match applying the whole log.
func TestRecovery_SnapshotThenTailReplay(t *testing.T) {
	full := []*types.Order{
		lim(t, "a", types.SideSell, 105, 10),
		lim(t, "b", types.SideBuy, 100, 10),
		lim(t, "c", types.SideSell, 104, 5),
		lim(t, "d", types.SideBuy, 101, 5),
	}
	whole := engine()
	Replay(whole, NewStream(full...))

	// Recover by replaying the prefix, then the tail separately (as a WAL reader
	// would resume from the last checkpoint).
	prefix := NewStream(full[:2]...)
	tail := NewStream(full[2:]...)
	resumed := engine()
	Replay(resumed, prefix)
	Replay(resumed, tail)

	if bookDigest(resumed.Book()) != bookDigest(whole.Book()) {
		t.Error("prefix+tail replay did not reproduce the whole-log book")
	}
}
