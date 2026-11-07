package marketdata

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestPublicDepthWalkthrough plays a small book one command at a time and checks, after each, that public
// depth matches the engine's book. Run it with -v to read the log: every event as the feed reads it, what
// the feed publishes, its record of each resting order, and both books.
//
// Steps 1-3 fill on arrival, which the feed always got right. Steps 4 and 5 cross part of the way and rest
// the remainder, one smaller and one larger than its fill: that is what this fix is for. Step 6 shows that
// without it the error would carry on into later commands.
//
// The detail that matters: an event carries a pointer to its order, and the feed reads the batch once the
// whole command has run. So a taker's Accepted already shows its fills, and the trades that follow it in
// the same batch must not be taken off it again.
func TestPublicDepthWalkthrough(t *testing.T) {
	tap := &depthTap{t: t, f: NewL2Feed(), name: map[int64]string{}}
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = tap
	e := matching.NewEngine(cfg)

	for _, s := range []struct {
		title      string
		user       string
		side       types.Side
		price, qty int64
		asks, bids string // the depth this step must leave, best level first
	}{
		{"setup: S255 sells 100 @ 255", "S255", types.SideSell, 255, 100, "255:100", "-"},
		{"setup: S256 sells 50 @ 256", "S256", types.SideSell, 256, 50, "255:100 256:50", "-"},
		{"setup: S257 sells 100 @ 257", "S257", types.SideSell, 257, 100, "255:100 256:50 257:100", "-"},
		{"1. B1 buys 10 @ 255", "B1", types.SideBuy, 255, 10, "255:90 256:50 257:100", "-"},
		{"2. B2 buys 30 @ 255", "B2", types.SideBuy, 255, 30, "255:60 256:50 257:100", "-"},
		{"3. B3 buys 100 @ 257: takes 60 @ 255 and 40 @ 256", "B3", types.SideBuy, 257, 100, "256:10 257:100", "-"},
		{"4. B4 buys 120 @ 256: takes 10, rests 110", "B4", types.SideBuy, 256, 120, "257:100", "256:110"},
		{"5. B5 buys 105 @ 257: takes 100, rests 5", "B5", types.SideBuy, 257, 105, "-", "257:5 256:110"},
		{"6. S6 sells 110 @ 256: takes 5 @ 257, then 105 @ 256", "S6", types.SideSell, 256, 110, "-", "256:5"},
	} {
		t.Logf("== %s", s.title)
		e.Process(mkOrder(t, s.user, s.side, types.OrderTypeLimit, s.price, s.qty, types.TIFGoodTillCancel))
		tap.logBooks(e)
		assertAgrees(t, tap.f, e, s.title)
		if asks, bids := depthLine(tap.f.Levels(types.SideSell)), depthLine(tap.f.Levels(types.SideBuy)); asks != s.asks || bids != s.bids {
			t.Fatalf("%s: public depth is asks %s | bids %s, want asks %s | bids %s", s.title, asks, bids, s.asks, s.bids)
		}
	}
}

// depthTap sits between the engine and the feed. It logs each batch exactly as the feed is about to read
// it, hands it on, and logs what the feed publishes.
type depthTap struct {
	t    *testing.T
	f    *L2Feed
	name map[int64]string // order id -> the walkthrough's name for it
}

func (d *depthTap) who(id int64) string {
	if n, ok := d.name[id]; ok {
		return n
	}
	return fmt.Sprintf("id %d", id)
}

func (d *depthTap) OnEvents(evs []matching.Event) {
	d.t.Logf("  events, as the feed reads them (after the whole command has run):")
	for _, e := range evs {
		switch {
		case e.Kind == matching.EventTrade && e.Trade != nil:
			tr := e.Trade
			d.t.Logf("    seq %-3d TRADE     %3d @ %d  buy=%s sell=%s (resting: %s)",
				e.Seq, tr.Quantity, tr.Price, d.who(tr.BuyOrderID), d.who(tr.SellOrderID), d.who(tr.MakerOrderID))
		case e.Order != nil:
			o := e.Order
			d.name[o.ID] = o.UserID
			side := "sell"
			if o.Side == types.SideBuy {
				side = "buy"
			}
			d.t.Logf("    seq %-3d %-9v %s (id %d) %s %d @ %d  ->  filled %d, remaining %d, %s",
				e.Seq, e.Kind, o.UserID, o.ID, side, o.Quantity, o.Price, o.FilledQty, o.RemainingQty, o.Status)
		default:
			d.t.Logf("    seq %-3d %v", e.Seq, e.Kind)
		}
	}
	d.f.OnEvents(evs)
	out := []string{}
	for _, delta := range d.f.Drain() {
		side := "ask"
		if delta.Side == types.SideBuy {
			side = "bid"
		}
		out = append(out, fmt.Sprintf("%s %d = %d", side, delta.Price, delta.Qty))
	}
	if len(out) == 0 {
		out = append(out, "(nothing)")
	}
	d.t.Logf("  feed publishes:  %s", strings.Join(out, ", "))
}

func (d *depthTap) logBooks(e *matching.Engine) {
	record := []string{}
	for id, lo := range d.f.live {
		record = append(record, fmt.Sprintf("%s=%d@%d", d.who(id), lo.remaining, lo.price))
	}
	sort.Strings(record)
	d.t.Logf("  feed's record:   %s", strings.Join(record, "  "))
	snap := e.Snapshot(1 << 20)
	asks, bids := []L2Delta{}, []L2Delta{}
	for _, l := range snap.Asks {
		asks = append(asks, L2Delta{Price: l.Price, Qty: l.Quantity})
	}
	for _, l := range snap.Bids {
		bids = append(bids, L2Delta{Price: l.Price, Qty: l.Quantity})
	}
	d.t.Logf("  engine's book:   asks %s | bids %s", depthLine(asks), depthLine(bids))
	d.t.Logf("  public depth:    asks %s | bids %s",
		depthLine(d.f.Levels(types.SideSell)), depthLine(d.f.Levels(types.SideBuy)))
}

// depthLine writes levels as price:quantity, in the order given (best first), or "-" for none.
func depthLine(levels []L2Delta) string {
	parts := []string{}
	for _, l := range levels {
		if l.Qty > 0 {
			parts = append(parts, fmt.Sprintf("%d:%d", l.Price, l.Qty))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}
