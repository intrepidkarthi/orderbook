package marketdata

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The claim this file has to back is the one that justifies deriving L2 above the
// engine rather than emitting it from the matching goroutine: **the derived levels
// equal the engine's own snapshot, always**.
//
// Asserting that after every command is stronger than any assertion about the deltas
// in isolation, because the two views cannot drift without this failing.

func mkOrder(t *testing.T, user string, side types.Side, otype types.OrderType, price, qty int64, tif types.TimeInForce) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, otype, price, qty, tif)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// engineLevels reads the engine's aggregated book, which is the reference the feed is
// checked against.
func engineLevels(snap *orderbook.Snapshot, side types.Side) map[int64]int64 {
	out := map[int64]int64{}
	levels := snap.Bids
	if side == types.SideSell {
		levels = snap.Asks
	}
	for _, l := range levels {
		if l.Quantity > 0 {
			out[l.Price] = l.Quantity
		}
	}
	return out
}

func feedLevels(f *L2Feed, side types.Side) map[int64]int64 {
	out := map[int64]int64{}
	for _, d := range f.Levels(side) {
		out[d.Price] = d.Qty
	}
	return out
}

// assertAgrees compares the feed's derived levels against the engine's snapshot.
func assertAgrees(t *testing.T, f *L2Feed, e *matching.Engine, when string) {
	t.Helper()
	// A large depth rather than 0: depth is a hard cap on levels returned, and 0
	// means zero levels, not "all of them".
	snap := e.Snapshot(1 << 20)
	for _, side := range []types.Side{types.SideBuy, types.SideSell} {
		want := engineLevels(snap, side)
		got := feedLevels(f, side)
		if len(want) != len(got) {
			t.Fatalf("%s: %v side has %d levels in the feed, %d in the engine\n feed %v\n book %v",
				when, side, len(got), len(want), got, want)
		}
		for price, q := range want {
			if got[price] != q {
				t.Fatalf("%s: %v %d has %d lots in the feed, %d in the engine",
					when, side, price, got[price], q)
			}
		}
	}
}

func newFeedEngine(t *testing.T) (*matching.Engine, *L2Feed) {
	t.Helper()
	f := NewL2Feed()
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = f // L2Feed implements matching.EventSink
	return matching.NewEngine(cfg), f
}

func TestL2FeedTracksResting(t *testing.T) {
	e, f := newFeedEngine(t)

	e.Process(mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, 100, 10, types.TIFGoodTillCancel))
	assertAgrees(t, f, e, "after one bid")

	e.Process(mkOrder(t, "b", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel))
	assertAgrees(t, f, e, "after a second bid at the same price")

	if got := f.Levels(types.SideBuy); len(got) != 1 || got[0].Qty != 15 {
		t.Errorf("aggregate = %v, want one level of 15", got)
	}

	e.Process(mkOrder(t, "c", types.SideSell, types.OrderTypeLimit, 105, 7, types.TIFGoodTillCancel))
	assertAgrees(t, f, e, "after an ask")
}

func TestL2FeedTracksFills(t *testing.T) {
	e, f := newFeedEngine(t)
	e.Process(mkOrder(t, "mm", types.SideBuy, types.OrderTypeLimit, 100, 10, types.TIFGoodTillCancel))
	assertAgrees(t, f, e, "before the fill")

	// Partial fill: the level must drop to what is left.
	e.Process(mkOrder(t, "tk", types.SideSell, types.OrderTypeLimit, 100, 4, types.TIFGoodTillCancel))
	assertAgrees(t, f, e, "after a partial fill")
	if got := f.Levels(types.SideBuy); len(got) != 1 || got[0].Qty != 6 {
		t.Errorf("after a 4-lot fill the level is %v, want 6", got)
	}

	// Full fill: the level must disappear rather than sit at zero.
	e.Process(mkOrder(t, "tk2", types.SideSell, types.OrderTypeLimit, 100, 6, types.TIFGoodTillCancel))
	assertAgrees(t, f, e, "after the level empties")
	if got := f.Levels(types.SideBuy); len(got) != 0 {
		t.Errorf("emptied level is still present: %v", got)
	}
}

func TestL2FeedTracksCancels(t *testing.T) {
	e, f := newFeedEngine(t)
	o := mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, 100, 10, types.TIFGoodTillCancel)
	e.Process(o)
	if _, err := e.Cancel(o.ID, "a"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	assertAgrees(t, f, e, "after a cancel")
	if got := f.Levels(types.SideBuy); len(got) != 0 {
		t.Errorf("cancelled order still counted: %v", got)
	}
}

func TestL2FeedTracksReduce(t *testing.T) {
	e, f := newFeedEngine(t)
	o := mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, 100, 10, types.TIFGoodTillCancel)
	e.Process(o)
	if _, err := e.Reduce(o.ID, 3, "a"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	assertAgrees(t, f, e, "after a reduce")
	if got := f.Levels(types.SideBuy); len(got) != 1 || got[0].Qty != 3 {
		t.Errorf("after reducing to 3 the level is %v", got)
	}
}

// TestL2FeedCoalescesWithinACommand — a taker sweeping three orders at one price
// should produce ONE delta for that level carrying its final quantity, not three.
// A subscriber does not need the intermediate states and a feed that publishes them
// is three times the bandwidth for no information.
func TestL2FeedCoalescesWithinACommand(t *testing.T) {
	e, f := newFeedEngine(t)
	for i := 0; i < 3; i++ {
		e.Process(mkOrder(t, fmt.Sprintf("mm%d", i), types.SideSell, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel))
	}
	f.Drain() // discard the setup deltas

	e.Process(mkOrder(t, "tk", types.SideBuy, types.OrderTypeLimit, 100, 12, types.TIFGoodTillCancel))
	deltas := f.Drain()

	askDeltas := 0
	for _, d := range deltas {
		if d.Side == types.SideSell && d.Price == 100 {
			askDeltas++
		}
	}
	if askDeltas != 1 {
		t.Errorf("sweeping three orders at one level produced %d deltas for it, want 1 coalesced", askDeltas)
	}
	assertAgrees(t, f, e, "after a multi-order sweep")
}

// TestL2FeedDeltasAreAbsolute — Qty is the level's new total, so a subscriber that
// missed one recovers on the next rather than being permanently wrong.
func TestL2FeedDeltasAreAbsolute(t *testing.T) {
	e, f := newFeedEngine(t)
	e.Process(mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, 100, 10, types.TIFGoodTillCancel))
	e.Process(mkOrder(t, "b", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel))

	deltas := f.Drain()
	if len(deltas) < 2 {
		t.Fatalf("got %d deltas, want at least 2", len(deltas))
	}
	last := deltas[len(deltas)-1]
	if last.Qty != 15 {
		t.Errorf("final delta carries %d, want the level total of 15 — deltas must be absolute", last.Qty)
	}
	if last.Seq == 0 {
		t.Error("delta carries no sequence, so a subscriber cannot order or resume them")
	}
}

// TestL2FeedAgreesUnderARandomTape is the real test: a long pseudo-random command
// stream over every operation that moves a level, checking the derived view against
// the engine's after every single command.
func TestL2FeedAgreesUnderARandomTape(t *testing.T) {
	e, f := newFeedEngine(t)
	rng := rand.New(rand.NewSource(0xC10B))

	var resting []*types.Order
	for i := 0; i < 3000; i++ {
		switch rng.Intn(100) {
		case 0, 1, 2, 3, 4, 5, 6, 7, 8, 9: // cancel
			if len(resting) == 0 {
				continue
			}
			j := rng.Intn(len(resting))
			o := resting[j]
			resting = append(resting[:j], resting[j+1:]...)
			_, _ = e.Cancel(o.ID, o.UserID)
		case 10, 11, 12, 13, 14: // reduce
			if len(resting) == 0 {
				continue
			}
			o := resting[rng.Intn(len(resting))]
			if o.Quantity > 1 {
				_, _ = e.Reduce(o.ID, 1+rng.Int63n(o.Quantity-1), o.UserID)
			}
		case 15, 16, 17, 18, 19, 20: // aggressive market order
			side := types.SideBuy
			if rng.Intn(2) == 0 {
				side = types.SideSell
			}
			e.Process(mkOrder(t, "tk", side, types.OrderTypeMarket, 0, 1+rng.Int63n(20), types.TIFImmediateOrCancel))
		default: // passive limit
			side := types.SideBuy
			price := int64(90 + rng.Intn(10))
			if rng.Intn(2) == 0 {
				side = types.SideSell
				price = int64(100 + rng.Intn(10))
			}
			o := mkOrder(t, fmt.Sprintf("u%d", rng.Intn(5)), side, types.OrderTypeLimit, price, 1+rng.Int63n(10), types.TIFGoodTillCancel)
			res := e.Process(o)
			if res != nil && res.Order != nil && res.Order.RemainingQty > 0 {
				resting = append(resting, res.Order)
			}
		}
		assertAgrees(t, f, e, fmt.Sprintf("command %d", i))
	}
	// The tape has to have actually done something, or this proves nothing.
	if e.OrderCount() == 0 {
		t.Fatal("the tape left an empty book; it is not exercising the feed")
	}
}
