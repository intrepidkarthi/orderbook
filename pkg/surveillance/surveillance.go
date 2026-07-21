// Package surveillance detects abusive trading patterns from a stream of
// order-book events. It sits above the core engine (docs/SPEC.md §5.3): the
// matching core stays clean and neutral, while surveillance observes the event
// flow and raises alerts — the way a real venue separates matching from market
// integrity.
//
// Detectors are deliberately simple and explainable (the point is to *show* how
// manipulation looks and gets caught, per the demo plan), not production-grade
// market-abuse models.
package surveillance

import (
	"fmt"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

// EventKind is the type of an observed event.
type EventKind uint8

const (
	OrderPlaced EventKind = iota
	OrderCancelled
	Trade
)

// Event is one observed order-book action. For Trade events, MakerOrderID and
// TakerOrderID identify the two orders involved.
type Event struct {
	Kind         EventKind
	Seq          uint64
	UserID       string
	OrderID      string
	Side         types.Side
	Price        decimal.Decimal
	Quantity     decimal.Decimal
	MakerOrderID string
	TakerOrderID string
}

// Alert is a raised surveillance signal.
type Alert struct {
	Kind   string // "spoofing", "order_rate"
	UserID string
	Seq    uint64
	Detail string
}

// Detector consumes events and emits alerts.
type Detector interface {
	Observe(e Event) []Alert
}

// Monitor fans events out to a set of detectors and accumulates their alerts.
type Monitor struct {
	detectors []Detector
	alerts    []Alert
}

// NewMonitor builds a monitor over the given detectors.
func NewMonitor(detectors ...Detector) *Monitor {
	return &Monitor{detectors: detectors}
}

// Observe feeds an event to every detector, recording and returning any alerts.
func (m *Monitor) Observe(e Event) []Alert {
	var out []Alert
	for _, d := range m.detectors {
		out = append(out, d.Observe(e)...)
	}
	m.alerts = append(m.alerts, out...)
	return out
}

// Alerts returns every alert raised so far.
func (m *Monitor) Alerts() []Alert { return m.alerts }

// --- Spoofing / layering ---

// SpoofConfig configures the spoof detector.
type SpoofConfig struct {
	MinSize     decimal.Decimal // only orders at least this large are candidates
	MaxLifetime uint64          // cancelled within this many events of placement
}

// SpoofDetector flags large resting orders that are cancelled, unfilled, very
// soon after being placed — the signature of a spoof (post size to fake
// pressure, then pull it before it trades). Repeated on one side, this is
// layering.
type SpoofDetector struct {
	cfg  SpoofConfig
	live map[string]*orderRec
}

type orderRec struct {
	user      string
	placedSeq uint64
	size      decimal.Decimal
	filled    decimal.Decimal
}

// NewSpoofDetector builds a spoof detector.
func NewSpoofDetector(cfg SpoofConfig) *SpoofDetector {
	return &SpoofDetector{cfg: cfg, live: make(map[string]*orderRec)}
}

// Observe implements Detector.
func (d *SpoofDetector) Observe(e Event) []Alert {
	switch e.Kind {
	case OrderPlaced:
		d.live[e.OrderID] = &orderRec{user: e.UserID, placedSeq: e.Seq, size: e.Quantity, filled: decimal.Zero}

	case Trade:
		if r, ok := d.live[e.MakerOrderID]; ok {
			r.filled = r.filled.Add(e.Quantity)
		}
		if r, ok := d.live[e.TakerOrderID]; ok {
			r.filled = r.filled.Add(e.Quantity)
		}

	case OrderCancelled:
		r, ok := d.live[e.OrderID]
		if !ok {
			return nil
		}
		delete(d.live, e.OrderID)
		lifetime := e.Seq - r.placedSeq
		if r.filled.IsZero() && r.size.GreaterThanOrEqual(d.cfg.MinSize) && lifetime <= d.cfg.MaxLifetime {
			return []Alert{{
				Kind:   "spoofing",
				UserID: r.user,
				Seq:    e.Seq,
				Detail: fmt.Sprintf("order %s (size %s) cancelled unfilled %d events after placement", e.OrderID, r.size, lifetime),
			}}
		}
	}
	return nil
}

// --- Order-rate limit ---

// RateConfig configures the order-rate detector.
type RateConfig struct {
	MaxOrders int    // more than this many placements ...
	Window    uint64 // ... within this many events (by sequence) trips an alert
}

// RateLimiter flags a user placing orders faster than MaxOrders per Window
// events — a proxy for quote stuffing / runaway messaging.
type RateLimiter struct {
	cfg        RateConfig
	placements map[string][]uint64
}

// NewRateLimiter builds an order-rate detector.
func NewRateLimiter(cfg RateConfig) *RateLimiter {
	return &RateLimiter{cfg: cfg, placements: make(map[string][]uint64)}
}

// Observe implements Detector.
func (d *RateLimiter) Observe(e Event) []Alert {
	if e.Kind != OrderPlaced {
		return nil
	}
	var cutoff uint64
	if e.Seq > d.cfg.Window {
		cutoff = e.Seq - d.cfg.Window
	}
	recent := d.placements[e.UserID][:0]
	for _, s := range d.placements[e.UserID] {
		if s >= cutoff {
			recent = append(recent, s)
		}
	}
	recent = append(recent, e.Seq)
	d.placements[e.UserID] = recent

	if len(recent) > d.cfg.MaxOrders {
		return []Alert{{
			Kind:   "order_rate",
			UserID: e.UserID,
			Seq:    e.Seq,
			Detail: fmt.Sprintf("%d orders within %d events (limit %d)", len(recent), d.cfg.Window, d.cfg.MaxOrders),
		}}
	}
	return nil
}
