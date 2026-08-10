package marketdata

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The feed is the first real consumer of trade bust, and the reason the spec
// insisted on having one: a bust nobody can be told about is a seam, not a
// feature. See docs/TRADE-BUST.md §4.4.

func bustFeedOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// printOne rests a sell and takes part of it, returning the printed trade id.
func printOne(t *testing.T, e *matching.Engine, price, qty int64) int64 {
	t.Helper()
	e.Process(bustFeedOrder(t, "maker", types.SideSell, price, 10))
	res := e.Process(bustFeedOrder(t, "taker", types.SideBuy, price, qty))
	if len(res.Trades) == 0 {
		t.Fatal("setup: no trade printed")
	}
	return res.Trades[0].ID
}

// TestFeedPublishesBust — the print and its annulment must be joinable, which
// means the trade event has to carry an id in the first place. Before trade bust
// the feed published prints anonymously and this test could not have been written.
func TestFeedPublishesBust(t *testing.T) {
	e, f := newFeedEngine2(t, 1024)
	tradeID := printOne(t, e, 100, 4)

	if err := e.Bust(tradeID, "erroneous order entry"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	updates, err := f.Since(0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}

	var print, bust *Update
	for i := range updates {
		switch updates[i].Kind {
		case UpdateTrade:
			print = &updates[i]
		case UpdateBust:
			bust = &updates[i]
		}
	}
	if print == nil {
		t.Fatal("no trade update")
	}
	if bust == nil {
		t.Fatal("no bust update; the annulment reached no subscriber")
	}
	if print.TradeID == 0 {
		t.Error("the print carries no trade id, so the bust below names nothing a subscriber has seen")
	}
	if bust.TradeID != print.TradeID {
		t.Errorf("bust names trade %d, the print was trade %d", bust.TradeID, print.TradeID)
	}
	if bust.BustReason != "erroneous order entry" {
		t.Errorf("bust reason = %q, want the operator's", bust.BustReason)
	}
	if bust.Seq <= print.Seq {
		t.Errorf("bust seq %d is not after the print's %d; the annulment must follow what it annuls",
			bust.Seq, print.Seq)
	}
}

// TestFeedBustDoesNotDisturbTheBook — the feed's whole contract is that a
// subscriber's reconstructed book equals the engine's. A bust must not move it.
func TestFeedBustDoesNotDisturbTheBook(t *testing.T) {
	e, f := newFeedEngine2(t, 1024)
	tradeID := printOne(t, e, 100, 4)

	beforeSnap := f.Snapshot()
	beforeUpdates, err := f.Since(0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	wantBids, wantAsks := applyUpdates(Snapshot{}, beforeUpdates)

	if err := e.Bust(tradeID, "erroneous"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	afterUpdates, err := f.Since(0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	gotBids, gotAsks := applyUpdates(Snapshot{}, afterUpdates)

	if len(gotBids) != len(wantBids) || len(gotAsks) != len(wantAsks) {
		t.Fatalf("depth changed across a bust: bids %v -> %v, asks %v -> %v",
			wantBids, gotBids, wantAsks, gotAsks)
	}
	for p, q := range wantBids {
		if gotBids[p] != q {
			t.Errorf("bid %d: %d -> %d across a bust", p, q, gotBids[p])
		}
	}
	for p, q := range wantAsks {
		if gotAsks[p] != q {
			t.Errorf("ask %d: %d -> %d across a bust", p, q, gotAsks[p])
		}
	}
	// And the reference price stands, matching the engine (TRADE-BUST.md §2.3).
	if got := f.Snapshot().LastTradePrice; got != beforeSnap.LastTradePrice {
		t.Errorf("LastTradePrice %d -> %d across a bust", beforeSnap.LastTradePrice, got)
	}
}

// TestFeedBustReachesAResumingSubscriber — a subscriber that was disconnected over
// the print and reconnects with a cursor from before it must still learn both the
// trade and its annulment. This is the case a bust delivered only to live
// listeners would fail.
func TestFeedBustReachesAResumingSubscriber(t *testing.T) {
	e, f := newFeedEngine2(t, 1024)

	// The subscriber's cursor, taken before anything happened.
	cursor := f.Snapshot().Seq

	tradeID := printOne(t, e, 100, 4)
	if err := e.Bust(tradeID, "erroneous"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	updates, err := f.Since(cursor)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	var sawBust bool
	for _, u := range updates {
		if u.Kind == UpdateBust && u.TradeID == tradeID {
			sawBust = true
		}
	}
	if !sawBust {
		t.Error("a subscriber resuming from before the print never learns the trade was busted")
	}
}
