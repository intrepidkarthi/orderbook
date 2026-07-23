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
