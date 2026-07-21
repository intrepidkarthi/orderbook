package surveillance

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func placed(seq uint64, user, id, size string) Event {
	return Event{Kind: OrderPlaced, Seq: seq, UserID: user, OrderID: id, Side: types.SideBuy, Quantity: dec(size)}
}
func cancelled(seq uint64, user, id string) Event {
	return Event{Kind: OrderCancelled, Seq: seq, UserID: user, OrderID: id}
}
func trade(seq uint64, makerID, takerID, qty string) Event {
	return Event{Kind: Trade, Seq: seq, MakerOrderID: makerID, TakerOrderID: takerID, Quantity: dec(qty)}
}

func spoofDet() *SpoofDetector {
	return NewSpoofDetector(SpoofConfig{MinSize: dec("50"), MaxLifetime: 5})
}

func TestSpoof_FlagsRapidLargeUnfilledCancel(t *testing.T) {
	d := spoofDet()
	d.Observe(placed(1, "spoofer", "o1", "100"))
	alerts := d.Observe(cancelled(3, "spoofer", "o1"))
	if len(alerts) != 1 || alerts[0].Kind != "spoofing" || alerts[0].UserID != "spoofer" {
		t.Fatalf("expected one spoofing alert, got %+v", alerts)
	}
}

func TestSpoof_IgnoresFilled(t *testing.T) {
	d := spoofDet()
	d.Observe(placed(1, "mm", "o1", "100"))
	d.Observe(trade(2, "o1", "x", "10")) // partially filled ⇒ real liquidity
	if a := d.Observe(cancelled(3, "mm", "o1")); len(a) != 0 {
		t.Errorf("filled order should not be spoof, got %+v", a)
	}
}

func TestSpoof_IgnoresSmallAndSlow(t *testing.T) {
	small := spoofDet()
	small.Observe(placed(1, "u", "o1", "10")) // below MinSize
	if a := small.Observe(cancelled(2, "u", "o1")); len(a) != 0 {
		t.Errorf("small order should not be spoof, got %+v", a)
	}

	slow := spoofDet()
	slow.Observe(placed(1, "u", "o2", "100"))
	if a := slow.Observe(cancelled(10, "u", "o2")); len(a) != 0 { // lifetime 9 > 5
		t.Errorf("slowly-cancelled order should not be spoof, got %+v", a)
	}
}

func TestRate_FlagsBurst(t *testing.T) {
	d := NewRateLimiter(RateConfig{MaxOrders: 3, Window: 10})
	var last []Alert
	for seq := uint64(1); seq <= 4; seq++ {
		last = d.Observe(placed(seq, "fast", "o", "1"))
	}
	if len(last) != 1 || last[0].Kind != "order_rate" {
		t.Errorf("4th order within window should trip order_rate, got %+v", last)
	}
}

func TestRate_WindowExpiry(t *testing.T) {
	d := NewRateLimiter(RateConfig{MaxOrders: 3, Window: 10})
	d.Observe(placed(1, "u", "o", "1"))
	d.Observe(placed(2, "u", "o", "1"))
	d.Observe(placed(3, "u", "o", "1"))
	// Much later: the earlier three fall outside the window.
	if a := d.Observe(placed(100, "u", "o", "1")); len(a) != 0 {
		t.Errorf("spaced-out orders should not trip, got %+v", a)
	}
}

func TestMonitor_Aggregates(t *testing.T) {
	m := NewMonitor(spoofDet(), NewRateLimiter(RateConfig{MaxOrders: 100, Window: 10}))
	m.Observe(placed(1, "spoofer", "o1", "100"))
	m.Observe(cancelled(2, "spoofer", "o1"))
	if len(m.Alerts()) != 1 || m.Alerts()[0].Kind != "spoofing" {
		t.Errorf("monitor should have one spoofing alert, got %+v", m.Alerts())
	}
}
