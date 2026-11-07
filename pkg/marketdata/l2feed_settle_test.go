package marketdata

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func gtc(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	return mkOrder(t, user, side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
}

func askAt(f *L2Feed, price int64) int64 {
	for _, d := range f.Levels(types.SideSell) {
		if d.Price == price {
			return d.Qty
		}
	}
	return 0
}

// An order that crosses part of the way and rests the remainder must be published at
// that remainder. Three shapes, because the failure differs: understated when the fill
// is smaller than the remainder, and missing altogether when it is not.
func TestCrossCheckPartialCrossThenRest(t *testing.T) {
	for _, c := range []struct {
		name          string
		bid, ask, out int64
	}{
		{"fill smaller than the remainder", 78, 275, 197},
		{"fill larger than the remainder", 80, 100, 20},
		{"fill equal to the remainder", 50, 100, 50},
		{"filled completely on arrival", 100, 40, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, f := newFeedEngine(t)
			e.Process(gtc(t, "maker", types.SideBuy, 100, c.bid))
			res := e.Process(gtc(t, "taker", types.SideSell, 100, c.ask))
			assertAgrees(t, f, e, "after the cross")
			if got := askAt(f, 100); got != c.out {
				t.Fatalf("ask 100 is %d in the feed, want %d", got, c.out)
			}
			if c.out == 0 {
				return
			}
			_, err := e.Cancel(res.Order.ID, res.Order.UserID)
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}
			assertAgrees(t, f, e, "after cancelling the remainder")
		})
	}
}

// A later taker consuming the remainder must leave the level empty, not negative.
func TestCrossCheckRemainderThenConsumed(t *testing.T) {
	e, f := newFeedEngine(t)
	e.Process(gtc(t, "maker", types.SideBuy, 100, 78))
	e.Process(gtc(t, "taker", types.SideSell, 100, 275))
	e.Process(gtc(t, "next", types.SideBuy, 100, 100))
	assertAgrees(t, f, e, "after part of the remainder is consumed")
	e.Process(gtc(t, "next", types.SideBuy, 100, 97))
	assertAgrees(t, f, e, "after all of the remainder is consumed")
}

// A delta nobody needs is noise on every subscriber's wire: an order filled on arrival
// never changed its own side's level, so nothing should be said about it.
func TestCrossCheckNoDeltaForAnOrderFilledOnArrival(t *testing.T) {
	e, f := newFeedEngine(t)
	e.Process(gtc(t, "maker", types.SideBuy, 100, 50))
	f.Drain()
	e.Process(gtc(t, "taker", types.SideSell, 100, 20))
	for _, d := range f.Drain() {
		if d.Side == types.SideSell {
			t.Fatalf("a taker filled on arrival published %+v on its own side", d)
		}
	}
}

// The order rests, and a stop its own trade fired then trades against it, all inside one
// command. Its Accepted is read after all of that, so this separates the rules that
// count a fill twice from the ones that do not.
func TestCrossCheckRestingTakerHitByItsOwnCascade(t *testing.T) {
	e, f := newFeedEngine(t)
	stop, err := types.NewStopOrder(gtc(t, "stopper", types.SideBuy, 100, 20), 100)
	if err != nil {
		t.Fatal(err)
	}
	e.ProcessStop(stop)
	e.Process(gtc(t, "maker", types.SideBuy, 100, 50))
	e.Process(gtc(t, "taker", types.SideSell, 100, 100))
	if e.PendingStopCount() != 0 {
		t.Fatalf("the stop did not fire: %d pending", e.PendingStopCount())
	}
	assertAgrees(t, f, e, "after the cascade")
	if got := askAt(f, 100); got != 30 {
		t.Fatalf("ask 100 is %d in the feed, want 30", got)
	}
}

func TestCrossCheckStopFiresPartlyFillsAndRests(t *testing.T) {
	e, f := newFeedEngine(t)
	stop, err := types.NewStopOrder(gtc(t, "stopper", types.SideBuy, 110, 50), 110)
	if err != nil {
		t.Fatal(err)
	}
	e.ProcessStop(stop)
	e.Process(gtc(t, "s1", types.SideSell, 110, 30))
	e.Process(gtc(t, "b1", types.SideBuy, 110, 5)) // prints at 110, firing the stop
	if e.PendingStopCount() != 0 {
		t.Fatalf("the stop did not fire: %d pending", e.PendingStopCount())
	}
	assertAgrees(t, f, e, "after the stop rests its remainder")
}

func TestCrossCheckIcebergMakerSweptAcrossSlices(t *testing.T) {
	e, f := newFeedEngine(t)
	ib, err := types.NewIcebergOrder(gtc(t, "whale", types.SideSell, 100, 90), 3)
	if err != nil {
		t.Fatal(err)
	}
	e.ProcessIceberg(ib)
	e.Process(gtc(t, "plain", types.SideSell, 100, 5))
	assertAgrees(t, f, e, "resting")
	for i, qty := range []int64{1, 4, 7, 2, 11} {
		e.Process(gtc(t, "taker", types.SideBuy, 100, qty))
		assertAgrees(t, f, e, fmt.Sprintf("sweep %d", i))
	}
}

func TestCrossCheckIcebergTakerCrossesThenRests(t *testing.T) {
	e, f := newFeedEngine(t)
	e.Process(gtc(t, "maker", types.SideBuy, 100, 7))
	ib, err := types.NewIcebergOrder(gtc(t, "whale", types.SideSell, 100, 20), 5)
	if err != nil {
		t.Fatal(err)
	}
	e.ProcessIceberg(ib)
	assertAgrees(t, f, e, "after the iceberg crossed and rested")
	if got := askAt(f, 100); got > 5 {
		t.Fatalf("ask 100 is %d in the feed: a slice of 5 can't show more, that's the reserve leaking", got)
	}
}

// With jitter, slices differ in size, so a value read late doesn't describe the slice a
// trade consumed. An over-subtraction here takes lots from OTHER orders at the level.
func TestCrossCheckJitteredIcebergBesideAnotherOrder(t *testing.T) {
	f := NewL2Feed()
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = f
	cfg.IcebergPeakJitter = decimal.RequireFromString("0.5")
	e := matching.NewEngine(cfg)
	for seed := range 40 {
		price := int64(100 + seed)
		o := gtc(t, "whale", types.SideSell, price, 400)
		ib, err := types.NewIcebergOrder(o, 40)
		if err != nil {
			t.Fatal(err)
		}
		e.ProcessIceberg(ib)
		e.Process(gtc(t, "plain", types.SideSell, price, 500))
		for i, qty := range []int64{3, 100, 170, 60} {
			e.Process(gtc(t, "taker", types.SideBuy, price, qty))
			assertAgrees(t, f, e, fmt.Sprintf("price %d sweep %d", price, i))
		}
		// Clear the level for the next round.
		e.CancelAllForUser("whale")
		e.CancelAllForUser("plain")
		assertAgrees(t, f, e, fmt.Sprintf("price %d cleared", price))
	}
}

func TestCrossCheckGuardrailTripsMidMatch(t *testing.T) {
	f := NewL2Feed()
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = f
	cfg.Guardrail = matching.Guardrail{MaxTrades: 1, Window: time.Hour}
	e := matching.NewEngine(cfg)
	e.Process(gtc(t, "m1", types.SideBuy, 100, 10))
	e.Process(gtc(t, "m2", types.SideBuy, 100, 10))
	e.Process(gtc(t, "taker", types.SideSell, 100, 50))
	assertAgrees(t, f, e, "after the guardrail tripped")
}

type recorder struct {
	f    *L2Feed
	last []string
}

func (s *recorder) OnEvents(evs []matching.Event) {
	for i := range evs {
		e := &evs[i]
		line := fmt.Sprintf("#%d %v order=%d", e.Seq, e.Kind, e.OrderID)
		if e.Order != nil {
			line += fmt.Sprintf(" %v @%d qty=%d rem=%d status=%v tif=%v", e.Order.Side, e.Order.Price, e.Order.Quantity, e.Order.RemainingQty, e.Order.Status, e.Order.TimeInForce)
		}
		if e.Trade != nil {
			line += fmt.Sprintf(" maker=%d taker=%d @%d x%d", e.Trade.MakerOrderID, e.Trade.TakerOrderID, e.Trade.Price, e.Trade.Quantity)
		}
		s.last = append(s.last, line)
	}
	s.f.OnEvents(evs)
}

type tapeOpts struct {
	name  string
	cfg   func(*matching.Config)
	stops bool
}

func runTape(t *testing.T, seed int64, o tapeOpts) (compared int) {
	t.Helper()
	f := NewL2Feed()
	cfg := matching.DefaultConfig("X")
	rec := &recorder{f: f}
	cfg.EventSink = rec
	if o.cfg != nil {
		o.cfg(&cfg)
	}
	e := matching.NewEngine(cfg)
	t.Cleanup(func() {
		if t.Failed() {
			for _, line := range rec.last {
				t.Log(line)
			}
		}
	})
	rng := rand.New(rand.NewSource(seed))
	var live []*types.Order
	remember := func(ord *types.Order) { live = append(live, ord) }
	side := func() types.Side {
		if rng.Intn(2) == 0 {
			return types.SideBuy
		}
		return types.SideSell
	}
	user := func() string { return fmt.Sprintf("u%d", rng.Intn(5)) }
	for i := range 500 {
		// Prices overlap on purpose: this is what the existing random tape doesn't do,
		// and why it never saw an order cross and then rest.
		rec.last = nil
		price := int64(95 + rng.Intn(11))
		qty := 1 + rng.Int63n(30)
		switch r := rng.Intn(100); {
		case r < 15:
			if len(live) > 0 {
				j := rng.Intn(len(live))
				ord := live[j]
				live = append(live[:j], live[j+1:]...)
				_, _ = e.Cancel(ord.ID, ord.UserID)
			}
		case r < 20:
			if len(live) > 0 {
				ord := live[rng.Intn(len(live))]
				if ord.RemainingQty > 1 {
					_, _ = e.Reduce(ord.ID, 1+rng.Int63n(ord.Quantity), ord.UserID)
				}
			}
		case r < 25:
			e.Process(mkOrder(t, user(), side(), types.OrderTypeMarket, 0, qty, types.TIFImmediateOrCancel))
		case r < 30:
			e.Process(mkOrder(t, user(), side(), types.OrderTypeLimit, price, qty, types.TIFImmediateOrCancel))
		case r < 33:
			e.Process(mkOrder(t, user(), side(), types.OrderTypeLimit, price, qty, types.TIFFillOrKill))
		case r < 35:
			ord := gtc(t, user(), side(), price, qty)
			ord.PostOnly = true
			e.Process(ord)
			remember(ord)
		case r < 42:
			ord := gtc(t, user(), side(), price, 30+rng.Int63n(100))
			if ib, err := types.NewIcebergOrder(ord, 1+rng.Int63n(15)); err == nil {
				e.ProcessIceberg(ib)
				remember(ord)
			}
		case r < 47 && o.stops:
			ord := gtc(t, user(), side(), price, qty)
			if st, err := types.NewStopOrder(ord, price); err == nil {
				e.ProcessStop(st)
			}
		default:
			ord := gtc(t, user(), side(), price, qty)
			e.Process(ord)
			remember(ord)
		}
		compared++
		assertAgrees(t, f, e, fmt.Sprintf("seed %d command %d", seed, i))
	}
	return compared
}

func TestCrossCheckRandomTapes(t *testing.T) {
	jitter := func(c *matching.Config) { c.IcebergPeakJitter = decimal.RequireFromString("0.5") }
	stp := func(m matching.SelfTradePrevention) func(*matching.Config) {
		return func(c *matching.Config) { c.SelfTradePrevention = m }
	}
	for _, o := range []tapeOpts{
		{name: "default"},
		{name: "jitter", cfg: jitter},
		{name: "stp-oldest", cfg: stp(matching.STPCancelOldest)},
		{name: "stp-both", cfg: stp(matching.STPCancelBoth)},
		{name: "stp-decrement", cfg: stp(matching.STPDecrement)},
		{name: "pro-rata", cfg: func(c *matching.Config) { c.ProRata = true }},
		{name: "stops", stops: true},
		{name: "stops-jitter", stops: true, cfg: jitter},
	} {
		t.Run(o.name, func(t *testing.T) {
			total := 0
			for seed := int64(1); seed <= 1; seed++ {
				total += runTape(t, seed, o)
			}
			if total < 500 {
				t.Fatalf("only %d comparisons ran, which proves little", total)
			}
		})
	}
}
