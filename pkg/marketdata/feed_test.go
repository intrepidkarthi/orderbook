package marketdata

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The claim the feed exists to support:
//
//	Snapshot(at Seq S) + every Update after S == the engine's current book
//
// A subscriber can start anywhere and be exactly right. Everything else here is a
// failure mode of that one sentence.

func newFeedEngine2(t *testing.T, retain int) (*matching.Engine, *Feed) {
	t.Helper()
	f := NewFeed("INC1", retain)
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = f
	return matching.NewEngine(cfg), f
}

// applyUpdates folds a snapshot and the updates after it into level maps, the way a
// subscriber would.
func applyUpdates(snap Snapshot, updates []Update) (bids, asks map[int64]int64) {
	bids, asks = map[int64]int64{}, map[int64]int64{}
	for _, d := range snap.Bids {
		bids[d.Price] = d.Qty
	}
	for _, d := range snap.Asks {
		asks[d.Price] = d.Qty
	}
	for _, u := range updates {
		if u.Kind != UpdateBookDelta {
			continue
		}
		m := bids
		if u.Side == types.SideSell {
			m = asks
		}
		if u.Qty == 0 {
			delete(m, u.Price)
		} else {
			m[u.Price] = u.Qty
		}
	}
	return bids, asks
}

func assertSubscriberMatchesEngine(t *testing.T, e *matching.Engine, f *Feed, snap Snapshot, when string) {
	t.Helper()
	updates, err := f.Since(snap.Seq)
	if err != nil {
		t.Fatalf("%s: Since(%d): %v", when, snap.Seq, err)
	}
	gotBids, gotAsks := applyUpdates(snap, updates)
	engSnap := e.Snapshot(1 << 20)
	wantBids := engineLevels(engSnap, types.SideBuy)
	wantAsks := engineLevels(engSnap, types.SideSell)

	for _, pair := range []struct {
		name      string
		got, want map[int64]int64
	}{{"bid", gotBids, wantBids}, {"ask", gotAsks, wantAsks}} {
		if len(pair.got) != len(pair.want) {
			t.Fatalf("%s: subscriber has %d %s levels, engine has %d\n sub  %v\n book %v",
				when, len(pair.got), pair.name, len(pair.want), pair.got, pair.want)
		}
		for price, q := range pair.want {
			if pair.got[price] != q {
				t.Fatalf("%s: %s %d — subscriber %d, engine %d",
					when, pair.name, price, pair.got[price], q)
			}
		}
	}
}

// TestSnapshotPlusDeltasEqualsTheBook is the guarantee, checked from many different
// starting points across one command stream. A subscriber joining at command 40 must
// end up in the same place as one that joined at command 4.
func TestSnapshotPlusDeltasEqualsTheBook(t *testing.T) {
	e, f := newFeedEngine2(t, 1<<16)
	rng := rand.New(rand.NewSource(0xFEED))

	var resting []*types.Order
	var snaps []Snapshot
	for i := 0; i < 2000; i++ {
		switch rng.Intn(10) {
		case 0, 1: // cancel
			if len(resting) == 0 {
				continue
			}
			j := rng.Intn(len(resting))
			o := resting[j]
			resting = append(resting[:j], resting[j+1:]...)
			_, _ = e.Cancel(o.ID, o.UserID)
		case 2: // aggressive
			side := types.SideBuy
			if rng.Intn(2) == 0 {
				side = types.SideSell
			}
			e.Process(mkOrder(t, "tk", side, types.OrderTypeMarket, 0, 1+rng.Int63n(15), types.TIFImmediateOrCancel))
		default: // passive
			side := types.SideBuy
			price := int64(90 + rng.Intn(8))
			if rng.Intn(2) == 0 {
				side = types.SideSell
				price = int64(100 + rng.Intn(8))
			}
			o := mkOrder(t, fmt.Sprintf("u%d", rng.Intn(4)), side, types.OrderTypeLimit, price, 1+rng.Int63n(9), types.TIFGoodTillCancel)
			res := e.Process(o)
			if res != nil && res.Order != nil && res.Order.RemainingQty > 0 {
				resting = append(resting, res.Order)
			}
		}
		// Take a snapshot periodically; every one of them must remain a valid
		// starting point for the rest of the stream.
		if i%97 == 0 {
			snaps = append(snaps, f.Snapshot())
		}
	}
	if len(snaps) < 5 {
		t.Fatalf("only %d snapshots taken; the tape is not exercising this", len(snaps))
	}
	for i, snap := range snaps {
		assertSubscriberMatchesEngine(t, e, f, snap, fmt.Sprintf("snapshot %d (seq %d)", i, snap.Seq))
	}
	// And a subscriber that started from nothing, before the first update.
	assertSubscriberMatchesEngine(t, e, f, Snapshot{Incarnation: "INC1", Seq: 0}, "from sequence 0")
}

// TestSequenceIsDenseAndGapFree — a subscriber orders and de-duplicates by sequence,
// so a hole or a repeat is unrecoverable for it.
func TestSequenceIsDenseAndGapFree(t *testing.T) {
	e, f := newFeedEngine2(t, 1<<16)
	for i := 0; i < 200; i++ {
		e.Process(mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, int64(90+i%5), 3, types.TIFGoodTillCancel))
	}
	e.Process(mkOrder(t, "b", types.SideSell, types.OrderTypeMarket, 0, 50, types.TIFImmediateOrCancel))

	updates, err := f.Since(0)
	if err != nil {
		t.Fatalf("Since(0): %v", err)
	}
	if len(updates) == 0 {
		t.Fatal("no updates published")
	}
	for i, u := range updates {
		if u.Seq != uint64(i+1) {
			t.Fatalf("update %d carries seq %d, want %d — the sequence is not dense", i, u.Seq, i+1)
		}
	}
	if got := f.Seq(); got != uint64(len(updates)) {
		t.Errorf("feed reports seq %d, published %d updates", got, len(updates))
	}
}

// TestTradesArePublished — depth alone is not a market-data feed; a subscriber needs
// the prints, in the same ordered stream.
func TestTradesArePublished(t *testing.T) {
	e, f := newFeedEngine2(t, 1<<16)
	e.Process(mkOrder(t, "mm", types.SideSell, types.OrderTypeLimit, 100, 10, types.TIFGoodTillCancel))
	e.Process(mkOrder(t, "tk", types.SideBuy, types.OrderTypeLimit, 100, 4, types.TIFGoodTillCancel))

	updates, err := f.Since(0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	var trades int
	for _, u := range updates {
		if u.Kind != UpdateTrade {
			continue
		}
		trades++
		if u.TradePrice != 100 || u.TradeQty != 4 {
			t.Errorf("print is %d@%d, want 4@100", u.TradeQty, u.TradePrice)
		}
		if u.Aggressor != types.SideBuy {
			t.Errorf("aggressor is %v, want buy", u.Aggressor)
		}
	}
	if trades != 1 {
		t.Errorf("published %d trades, want 1", trades)
	}
	if got := f.Snapshot().LastTradePrice; got != 100 {
		t.Errorf("snapshot last trade %d, want 100", got)
	}
}

// TestDeltasPrecedeTheirTrade — applied in order, a subscriber should see the book
// reach its post-trade state and then be told what printed. The reverse briefly shows
// a trade against depth still displayed.
func TestDeltasPrecedeTheirTrade(t *testing.T) {
	e, f := newFeedEngine2(t, 1<<16)
	e.Process(mkOrder(t, "mm", types.SideSell, types.OrderTypeLimit, 100, 10, types.TIFGoodTillCancel))
	base := f.Seq()
	e.Process(mkOrder(t, "tk", types.SideBuy, types.OrderTypeLimit, 100, 4, types.TIFGoodTillCancel))

	updates, err := f.Since(base)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	var sawDelta bool
	for _, u := range updates {
		switch u.Kind {
		case UpdateBookDelta:
			sawDelta = true
		case UpdateTrade:
			if !sawDelta {
				t.Error("a trade was published before the level change it caused")
			}
		}
	}
}

// TestEvictedSubscriberIsRefusedNotTruncated — handing back a partial answer that
// looks complete is how a subscriber silently loses a price level forever.
func TestEvictedSubscriberIsRefused(t *testing.T) {
	e, f := newFeedEngine2(t, 16) // a deliberately tiny retention ring
	for i := 0; i < 200; i++ {
		e.Process(mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, int64(90+i%5), 3, types.TIFGoodTillCancel))
	}
	if _, err := f.Since(1); !errors.Is(err, ErrSequenceEvicted) {
		t.Errorf("Since(1) on a long-evicted cursor: err = %v, want ErrSequenceEvicted", err)
	}
	// And the documented recovery works: a fresh snapshot is a valid starting point.
	snap := f.Snapshot()
	assertSubscriberMatchesEngine(t, e, f, snap, "after eviction, from a fresh snapshot")
}

// TestSubscriberAheadOfTheFeedIsRefused — claiming updates that were never published
// means the subscriber is out of step, and serving it nothing would leave it stuck
// there permanently.
func TestSubscriberAheadOfTheFeedIsRefused(t *testing.T) {
	e, f := newFeedEngine2(t, 1<<16)
	e.Process(mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel))
	if _, err := f.Since(f.Seq() + 5); !errors.Is(err, ErrSequenceEvicted) {
		t.Errorf("a cursor ahead of the feed: err = %v, want a refusal", err)
	}
}

// TestUpToDateSubscriberGetsNothingAndNoError — the ordinary steady state must not
// look like an error.
func TestUpToDateSubscriberGetsNothing(t *testing.T) {
	e, f := newFeedEngine2(t, 1<<16)
	e.Process(mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel))
	updates, err := f.Since(f.Seq())
	if err != nil {
		t.Fatalf("an up-to-date subscriber got an error: %v", err)
	}
	if len(updates) != 0 {
		t.Errorf("got %d updates, want none", len(updates))
	}
}

// TestResumeAcrossIncarnationsIsRefused — sequence numbers mean nothing across a
// restart, and serving different content under numbers a subscriber believes it has
// is the failure that is invisible to both sides.
func TestResumeAcrossIncarnationsIsRefused(t *testing.T) {
	e, f := newFeedEngine2(t, 1<<16)
	e.Process(mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel))

	if _, err := f.Resume("SOME-OTHER-RUN", 1); !errors.Is(err, ErrWrongIncarnation) {
		t.Errorf("err = %v, want ErrWrongIncarnation", err)
	}
	if _, err := f.Resume("INC1", 0); err != nil {
		t.Errorf("resuming the right incarnation failed: %v", err)
	}
	if f.Snapshot().Incarnation != "INC1" {
		t.Error("a snapshot does not name its incarnation, so a subscriber cannot tell when it is stale")
	}
}

// TestRetentionIsBounded — an unbounded ring turns one stalled subscriber into a
// venue-wide memory leak.
func TestRetentionIsBounded(t *testing.T) {
	const retain = 32
	e, f := newFeedEngine2(t, retain)
	for i := 0; i < 500; i++ {
		e.Process(mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, int64(90+i%5), 3, types.TIFGoodTillCancel))
	}
	oldest, count := f.Retained()
	if count > retain {
		t.Errorf("ring holds %d updates, cap is %d", count, retain)
	}
	if oldest == 0 {
		t.Fatal("nothing retained")
	}
	// Everything from the oldest retained point must still be servable.
	if _, err := f.Since(oldest - 1); err != nil {
		t.Errorf("the oldest retained sequence is not servable: %v", err)
	}
}

// TestHaltIsInTheSameStream — a status change qualifies the data around it, so it has
// to be ordered against that data rather than delivered on the side.
func TestHaltIsInTheSameStream(t *testing.T) {
	e, f := newFeedEngine2(t, 1<<16)
	e.Process(mkOrder(t, "a", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel))
	e.Halt()
	e.SetCancelOnly()
	e.Resume()

	updates, err := f.Since(0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	var seen []VenueState
	for _, u := range updates {
		if u.Kind == UpdateStatus {
			seen = append(seen, u.State)
		}
	}
	want := []VenueState{StateHalted, StateCancelOnly, StateOpen}
	if len(seen) != len(want) {
		t.Fatalf("status updates %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("status %d is %v, want %v", i, seen[i], want[i])
		}
	}
}
