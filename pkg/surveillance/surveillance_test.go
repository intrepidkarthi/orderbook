package surveillance

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func placed(seq uint64, user string, id, size int64) Event {
	return Event{Kind: OrderPlaced, Seq: seq, UserID: user, OrderID: id, Side: types.SideBuy, Quantity: size}
}
func cancelled(seq uint64, user string, id int64) Event {
	return Event{Kind: OrderCancelled, Seq: seq, UserID: user, OrderID: id}
}
func trade(seq uint64, makerID, takerID, qty int64) Event {
	return Event{Kind: Trade, Seq: seq, MakerOrderID: makerID, TakerOrderID: takerID, Quantity: qty}
}
func tradeAt(seq uint64, makerID, takerID, qty, price int64) Event {
	return Event{Kind: Trade, Seq: seq, MakerOrderID: makerID, TakerOrderID: takerID, Quantity: qty, Price: price}
}
func placedSide(seq uint64, user string, id, size int64, side types.Side) Event {
	return Event{Kind: OrderPlaced, Seq: seq, UserID: user, OrderID: id, Side: side, Quantity: size}
}

func spoofDet() *SpoofDetector {
	return NewSpoofDetector(SpoofConfig{MinSize: 50, MaxLifetime: 5})
}

func TestSpoof_FlagsRapidLargeUnfilledCancel(t *testing.T) {
	d := spoofDet()
	d.Observe(placed(1, "spoofer", 1, 100))
	alerts := d.Observe(cancelled(3, "spoofer", 1))
	if len(alerts) != 1 || alerts[0].Kind != "spoofing" || alerts[0].UserID != "spoofer" {
		t.Fatalf("expected one spoofing alert, got %+v", alerts)
	}
}

func TestSpoof_IgnoresFilled(t *testing.T) {
	d := spoofDet()
	d.Observe(placed(1, "mm", 1, 100))
	d.Observe(trade(2, 1, 99, 10)) // partially filled ⇒ real liquidity
	if a := d.Observe(cancelled(3, "mm", 1)); len(a) != 0 {
		t.Errorf("filled order should not be spoof, got %+v", a)
	}
}

func TestSpoof_IgnoresSmallAndSlow(t *testing.T) {
	small := spoofDet()
	small.Observe(placed(1, "u", 1, 10)) // below MinSize
	if a := small.Observe(cancelled(2, "u", 1)); len(a) != 0 {
		t.Errorf("small order should not be spoof, got %+v", a)
	}

	slow := spoofDet()
	slow.Observe(placed(1, "u", 2, 100))
	if a := slow.Observe(cancelled(10, "u", 2)); len(a) != 0 { // lifetime 9 > 5
		t.Errorf("slowly-cancelled order should not be spoof, got %+v", a)
	}
}

func TestOTR_FlagsManyOrdersFewFills(t *testing.T) {
	d := NewOTRDetector(OTRConfig{Window: 100, MinOrders: 5, MaxRatio: 3.0})
	// Ten placements, one fill: OTR 10 > 3 → flagged.
	var last []Alert
	for id := int64(1); id <= 10; id++ {
		last = d.Observe(placed(uint64(id), "spoofer", id, 1))
	}
	d.Observe(trade(11, 1, 99, 1)) // one of the spoofer's orders finally fills
	last = d.Observe(placed(12, "spoofer", 12, 1))
	if len(last) != 1 || last[0].Kind != "order_to_trade_ratio" || last[0].UserID != "spoofer" {
		t.Fatalf("expected one OTR alert, got %+v", last)
	}
}

func TestOTR_QuietWhenTrading(t *testing.T) {
	d := NewOTRDetector(OTRConfig{Window: 100, MinOrders: 5, MaxRatio: 3.0})
	// Six placements, six fills: OTR 1.0 → never flagged.
	var last []Alert
	for id := int64(1); id <= 6; id++ {
		last = d.Observe(placed(uint64(id*2-1), "mm", id, 1))
		d.Observe(trade(uint64(id*2), id, 99, 1))
	}
	if len(last) != 0 {
		t.Errorf("a genuine two-sided market maker should not be flagged, got %+v", last)
	}
}

func TestOTR_NeedsMinimumSample(t *testing.T) {
	d := NewOTRDetector(OTRConfig{Window: 100, MinOrders: 5, MaxRatio: 3.0})
	// Four placements, zero fills — below MinOrders, so no alert yet.
	var last []Alert
	for id := int64(1); id <= 4; id++ {
		last = d.Observe(placed(uint64(id), "u", id, 1))
	}
	if len(last) != 0 {
		t.Errorf("below the minimum sample should not flag, got %+v", last)
	}
}

func TestCloseMarking_FlagsDominantAggressor(t *testing.T) {
	d := NewCloseMarkingDetector(CloseMarkingConfig{Window: 100, MinVolume: 10, MaxShare: 0.7})
	d.Observe(placed(1, "whale", 1, 100))
	d.Observe(placed(2, "a", 2, 100))
	d.Observe(placed(3, "b", 3, 100))
	d.Observe(trade(4, 99, 2, 10))         // a takes 10
	d.Observe(trade(5, 99, 3, 10))         // b takes 10
	last := d.Observe(trade(6, 99, 1, 80)) // whale takes 80 → 80% of 100
	if len(last) != 1 || last[0].Kind != "aggressor_dominance" || last[0].UserID != "whale" {
		t.Fatalf("expected one aggressor_dominance alert for whale, got %+v", last)
	}
}

func TestCloseMarking_QuietWhenBalanced(t *testing.T) {
	d := NewCloseMarkingDetector(CloseMarkingConfig{Window: 100, MinVolume: 10, MaxShare: 0.7})
	d.Observe(placed(1, "a", 1, 100))
	d.Observe(placed(2, "b", 2, 100))
	d.Observe(placed(3, "c", 3, 100))
	d.Observe(trade(4, 99, 1, 30))
	d.Observe(trade(5, 99, 2, 30))
	if a := d.Observe(trade(6, 99, 3, 30)); len(a) != 0 { // 33% each
		t.Errorf("balanced aggressors should not be flagged, got %+v", a)
	}
}

func TestRamping_FlagsDirectionalPush(t *testing.T) {
	d := NewRampingDetector(RampingConfig{Window: 100, MinTrades: 3, MinMoveTicks: 5})
	d.Observe(placedSide(1, "ramp", 1, 10, types.SideBuy))
	d.Observe(placedSide(2, "ramp", 2, 10, types.SideBuy))
	d.Observe(placedSide(3, "ramp", 3, 10, types.SideBuy))
	d.Observe(tradeAt(4, 99, 1, 10, 100))
	d.Observe(tradeAt(5, 99, 2, 10, 103))
	last := d.Observe(tradeAt(6, 99, 3, 10, 107)) // 100→107 = 7 ticks up over 3 buys
	if len(last) != 1 || last[0].Kind != "ramping" || last[0].UserID != "ramp" {
		t.Fatalf("expected one ramping alert, got %+v", last)
	}
}

func TestRamping_QuietOnSmallMove(t *testing.T) {
	d := NewRampingDetector(RampingConfig{Window: 100, MinTrades: 3, MinMoveTicks: 5})
	d.Observe(placedSide(1, "u", 1, 10, types.SideBuy))
	d.Observe(placedSide(2, "u", 2, 10, types.SideBuy))
	d.Observe(placedSide(3, "u", 3, 10, types.SideBuy))
	d.Observe(tradeAt(4, 99, 1, 10, 100))
	d.Observe(tradeAt(5, 99, 2, 10, 101))
	if a := d.Observe(tradeAt(6, 99, 3, 10, 102)); len(a) != 0 { // only 2 ticks
		t.Errorf("a small move should not be flagged as ramping, got %+v", a)
	}
}

func TestPinging_FlagsTinyBurst(t *testing.T) {
	d := NewPingingDetector(PingingConfig{MaxSize: 5, MaxLifetime: 3, Window: 100, MinCount: 2})
	d.Observe(placed(1, "p", 1, 1))
	d.Observe(placed(2, "p", 2, 1))
	d.Observe(placed(3, "p", 3, 1))
	d.Observe(cancelled(4, "p", 1))         // ping 1 (lifetime 3)
	d.Observe(cancelled(5, "p", 2))         // ping 2
	last := d.Observe(cancelled(6, "p", 3)) // ping 3 > MinCount 2
	if len(last) != 1 || last[0].Kind != "pinging" || last[0].UserID != "p" {
		t.Fatalf("expected one pinging alert, got %+v", last)
	}
}

func TestPinging_IgnoresLargeAndFilled(t *testing.T) {
	d := NewPingingDetector(PingingConfig{MaxSize: 5, MaxLifetime: 3, Window: 100, MinCount: 1})
	// A large order is not a ping.
	d.Observe(placed(1, "u", 1, 100))
	if a := d.Observe(cancelled(2, "u", 1)); len(a) != 0 {
		t.Errorf("large order cancel is not a ping, got %+v", a)
	}
	// A tiny order that fills is real liquidity, not a ping.
	d.Observe(placed(3, "u", 3, 1))
	d.Observe(trade(4, 3, 99, 1)) // order 3 filled as maker
	if a := d.Observe(cancelled(5, "u", 3)); len(a) != 0 {
		t.Errorf("a filled tiny order is not a ping, got %+v", a)
	}
}

func TestCrossBook_FlagsMultiSymbolManipulator(t *testing.T) {
	build := func() []Detector {
		return []Detector{NewSpoofDetector(SpoofConfig{MinSize: 50, MaxLifetime: 5})}
	}
	c := NewCrossBookMonitor(CrossBookConfig{Window: 100, MinSymbols: 2}, build)

	// Spoof in book BTC — one book, not yet cross-book.
	c.Observe("BTC", placed(1, "boss", 1, 100))
	if a := c.Observe("BTC", cancelled(3, "boss", 1)); len(a) != 0 {
		t.Fatalf("a single book should not trip the cross-book alert, got %+v", a)
	}
	// Spoof again in book ETH — now two distinct books → cross-book alert.
	c.Observe("ETH", placed(4, "boss", 2, 100))
	last := c.Observe("ETH", cancelled(6, "boss", 2))
	if len(last) != 1 || last[0].Kind != "cross_book_manipulation" || last[0].UserID != "boss" {
		t.Fatalf("expected cross_book_manipulation for boss, got %+v", last)
	}
}

func TestCrossBook_QuietOnSingleBook(t *testing.T) {
	build := func() []Detector {
		return []Detector{NewSpoofDetector(SpoofConfig{MinSize: 50, MaxLifetime: 5})}
	}
	c := NewCrossBookMonitor(CrossBookConfig{Window: 100, MinSymbols: 2}, build)
	c.Observe("BTC", placed(1, "u", 1, 100))
	c.Observe("BTC", cancelled(3, "u", 1))
	c.Observe("BTC", placed(4, "u", 2, 100))
	if a := c.Observe("BTC", cancelled(6, "u", 2)); len(a) != 0 {
		t.Errorf("repeated spoofing in ONE book is not cross-book, got %+v", a)
	}
}

func TestRate_FlagsBurst(t *testing.T) {
	d := NewRateLimiter(RateConfig{MaxOrders: 3, Window: 10})
	var last []Alert
	for seq := uint64(1); seq <= 4; seq++ {
		last = d.Observe(placed(seq, "fast", 1, 1))
	}
	if len(last) != 1 || last[0].Kind != "order_rate" {
		t.Errorf("4th order within window should trip order_rate, got %+v", last)
	}
}

func TestRate_WindowExpiry(t *testing.T) {
	d := NewRateLimiter(RateConfig{MaxOrders: 3, Window: 10})
	d.Observe(placed(1, "u", 1, 1))
	d.Observe(placed(2, "u", 1, 1))
	d.Observe(placed(3, "u", 1, 1))
	// Much later: the earlier three fall outside the window.
	if a := d.Observe(placed(100, "u", 1, 1)); len(a) != 0 {
		t.Errorf("spaced-out orders should not trip, got %+v", a)
	}
}

func TestMonitor_Aggregates(t *testing.T) {
	m := NewMonitor(spoofDet(), NewRateLimiter(RateConfig{MaxOrders: 100, Window: 10}))
	m.Observe(placed(1, "spoofer", 1, 100))
	m.Observe(cancelled(2, "spoofer", 1))
	if len(m.Alerts()) != 1 || m.Alerts()[0].Kind != "spoofing" {
		t.Errorf("monitor should have one spoofing alert, got %+v", m.Alerts())
	}
}

// TestDetectorsForgetFullyFilledOrders. Every per-order map here was populated on
// OrderPlaced and cleared only on OrderCancelled, and the event model has no "this
// order is finished" — so an order that FILLED, which is most of them on a venue
// anyone is trading, left an entry nothing would ever remove. The state each
// detector held then grew with the venue's lifetime volume instead of with the
// resting orders it is meant to be watching.
func TestDetectorsForgetFullyFilledOrders(t *testing.T) {
	// Each case places two orders, prints a trade that exhausts both, and asks the
	// detector how much per-order state it is still holding.
	t.Run("spoof", func(t *testing.T) {
		d := spoofDet()
		d.Observe(placed(1, "maker", 1, 100))
		d.Observe(placed(2, "taker", 2, 100))
		d.Observe(trade(3, 1, 2, 100))
		if n := len(d.live); n != 0 {
			t.Errorf("SpoofDetector.live holds %d entries after both orders filled, want 0", n)
		}
	})

	t.Run("otr", func(t *testing.T) {
		d := NewOTRDetector(OTRConfig{Window: 100, MinOrders: 5, MaxRatio: 4})
		d.Observe(placed(1, "maker", 1, 100))
		d.Observe(placed(2, "taker", 2, 100))
		d.Observe(trade(3, 1, 2, 100))
		if n := len(d.orderUser); n != 0 {
			t.Errorf("OTRDetector.orderUser holds %d entries after both orders filled, want 0", n)
		}
	})

	t.Run("close-marking", func(t *testing.T) {
		d := NewCloseMarkingDetector(CloseMarkingConfig{Window: 100, MinVolume: 1000, MaxShare: 0.7})
		d.Observe(placed(1, "maker", 1, 100))
		d.Observe(placed(2, "taker", 2, 100))
		d.Observe(trade(3, 1, 2, 100))
		if n := len(d.orderUser); n != 0 {
			t.Errorf("CloseMarkingDetector.orderUser holds %d entries after both orders filled, want 0", n)
		}
	})

	t.Run("ramping", func(t *testing.T) {
		d := NewRampingDetector(RampingConfig{Window: 100, MinTrades: 5, MinMoveTicks: 10})
		d.Observe(placed(1, "maker", 1, 100))
		d.Observe(placed(2, "taker", 2, 100))
		d.Observe(tradeAt(3, 1, 2, 100, 500))
		if n := len(d.orderUser); n != 0 {
			t.Errorf("RampingDetector.orderUser holds %d entries after both orders filled, want 0", n)
		}
		if n := len(d.orderSide); n != 0 {
			t.Errorf("RampingDetector.orderSide holds %d entries after both orders filled, want 0", n)
		}
	})

	t.Run("pinging", func(t *testing.T) {
		d := NewPingingDetector(PingingConfig{MaxSize: 5, MaxLifetime: 3, Window: 100, MinCount: 3})
		d.Observe(placed(1, "maker", 1, 2))
		d.Observe(placed(2, "taker", 2, 2))
		d.Observe(trade(3, 1, 2, 2))
		if n := len(d.live); n != 0 {
			t.Errorf("PingingDetector.live holds %d entries after both orders filled, want 0", n)
		}
	})
}

// TestPartialFillsAreStillWatched is the other half: an order with quantity left is
// still resting, and evicting it on the first print would blind every detector to
// the rest of its life.
func TestPartialFillsAreStillWatched(t *testing.T) {
	d := spoofDet()
	d.Observe(placed(1, "spoofer", 1, 100))
	d.Observe(trade(2, 1, 99, 1)) // one lot of a hundred
	if len(d.live) != 1 {
		t.Fatalf("a 1%%-filled order was evicted: live holds %d entries", len(d.live))
	}
	// And it is still scored on cancel — as not-a-spoof, because it traded.
	if alerts := d.Observe(cancelled(3, "spoofer", 1)); len(alerts) != 0 {
		t.Errorf("an order that traded was flagged as a spoof: %+v", alerts)
	}
	if len(d.live) != 0 {
		t.Errorf("live holds %d entries after the cancel, want 0", len(d.live))
	}
}

// TestAFilledOrderCancelledLaterRaisesNothing. Eviction on fill has to cost no
// alert: a later OrderCancelled naming an order that already filled finds nothing,
// and that is the same answer the filled-quantity guard gave before.
func TestAFilledOrderCancelledLaterRaisesNothing(t *testing.T) {
	d := spoofDet()
	d.Observe(placed(1, "u", 1, 100))
	d.Observe(trade(2, 1, 99, 100))
	if alerts := d.Observe(cancelled(3, "u", 1)); len(alerts) != 0 {
		t.Errorf("a filled order's late cancel raised %+v, want nothing", alerts)
	}

	p := NewPingingDetector(PingingConfig{MaxSize: 5, MaxLifetime: 3, Window: 100, MinCount: 1})
	p.Observe(placed(1, "u", 1, 2))
	p.Observe(trade(2, 1, 99, 2))
	if alerts := p.Observe(cancelled(3, "u", 1)); len(alerts) != 0 {
		t.Errorf("a filled tiny order's late cancel was counted as a ping: %+v", alerts)
	}
}
