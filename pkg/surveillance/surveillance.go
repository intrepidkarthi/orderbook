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
	OrderID      int64
	Side         types.Side
	Price        int64 // ticks
	Quantity     int64 // lots
	MakerOrderID int64
	TakerOrderID int64
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
	MinSize     int64  // only orders at least this large are candidates (lots)
	MaxLifetime uint64 // cancelled within this many events of placement
}

// SpoofDetector flags large resting orders that are cancelled, unfilled, very
// soon after being placed — the signature of a spoof (post size to fake
// pressure, then pull it before it trades). Repeated on one side, this is
// layering.
type SpoofDetector struct {
	cfg  SpoofConfig
	live map[int64]*orderRec
}

type orderRec struct {
	user      string
	placedSeq uint64
	size      int64
	filled    int64
}

// NewSpoofDetector builds a spoof detector.
func NewSpoofDetector(cfg SpoofConfig) *SpoofDetector {
	return &SpoofDetector{cfg: cfg, live: make(map[int64]*orderRec)}
}

// Observe implements Detector.
func (d *SpoofDetector) Observe(e Event) []Alert {
	switch e.Kind {
	case OrderPlaced:
		d.live[e.OrderID] = &orderRec{user: e.UserID, placedSeq: e.Seq, size: e.Quantity, filled: 0}

	case Trade:
		if r, ok := d.live[e.MakerOrderID]; ok {
			r.filled += e.Quantity
		}
		if r, ok := d.live[e.TakerOrderID]; ok {
			r.filled += e.Quantity
		}

	case OrderCancelled:
		r, ok := d.live[e.OrderID]
		if !ok {
			return nil
		}
		delete(d.live, e.OrderID)
		lifetime := e.Seq - r.placedSeq
		if r.filled == 0 && r.size >= d.cfg.MinSize && lifetime <= d.cfg.MaxLifetime {
			return []Alert{{
				Kind:   "spoofing",
				UserID: r.user,
				Seq:    e.Seq,
				Detail: fmt.Sprintf("order %d (size %d) cancelled unfilled %d events after placement", e.OrderID, r.size, lifetime),
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

// --- Order-to-trade ratio (OTR) ---

// OTRConfig configures the order-to-trade-ratio detector.
type OTRConfig struct {
	Window    uint64  // rolling window measured in event sequence numbers
	MinOrders int     // require at least this many in-window placements before rating
	MaxRatio  float64 // flag when placements / max(1, fills) exceeds this
}

// OTRDetector flags a user whose order-to-trade ratio — placements per executed
// fill within a rolling window — is abnormally high. A high OTR is the strongest
// standing signal of spoofing / layering and quote stuffing: many orders posted,
// few (or none) actually trading. It complements SpoofDetector (which scores a
// single order's lifetime) by scoring a user's whole flow, and RateLimiter (which
// counts messages regardless of whether they trade). Real venues police an
// order-to-trade ratio directly (e.g. Borsa Italiana, MiFID II RTS 9); this is the
// explainable analog. Alert-only — enforcement belongs to the gateway.
type OTRDetector struct {
	cfg       OTRConfig
	orderUser map[int64]string    // orderID -> owner, for attributing fills
	places    map[string][]uint64 // per-user placement seqs (rolling)
	fills     map[string][]uint64 // per-user fill seqs (rolling)
}

// NewOTRDetector builds an order-to-trade-ratio detector.
func NewOTRDetector(cfg OTRConfig) *OTRDetector {
	return &OTRDetector{
		cfg:       cfg,
		orderUser: make(map[int64]string),
		places:    make(map[string][]uint64),
		fills:     make(map[string][]uint64),
	}
}

// Observe implements Detector. It attributes fills to the owning user via the
// order it was placed under, so it needs to see OrderPlaced before the Trade.
func (d *OTRDetector) Observe(e Event) []Alert {
	switch e.Kind {
	case OrderPlaced:
		d.orderUser[e.OrderID] = e.UserID
		d.places[e.UserID] = append(d.places[e.UserID], e.Seq)
		return d.rate(e.UserID, e.Seq)
	case Trade:
		if u, ok := d.orderUser[e.MakerOrderID]; ok {
			d.fills[u] = append(d.fills[u], e.Seq)
		}
		if u, ok := d.orderUser[e.TakerOrderID]; ok {
			d.fills[u] = append(d.fills[u], e.Seq)
		}
	case OrderCancelled:
		delete(d.orderUser, e.OrderID)
	}
	return nil
}

// rate prunes both windows to [seq-Window, seq] and flags an over-ratio user.
func (d *OTRDetector) rate(user string, seq uint64) []Alert {
	var cutoff uint64
	if seq > d.cfg.Window {
		cutoff = seq - d.cfg.Window
	}
	places := pruneSeqs(d.places[user], cutoff)
	fills := pruneSeqs(d.fills[user], cutoff)
	d.places[user] = places
	d.fills[user] = fills

	if len(places) < d.cfg.MinOrders {
		return nil
	}
	denom := len(fills)
	if denom == 0 {
		denom = 1
	}
	ratio := float64(len(places)) / float64(denom)
	if ratio > d.cfg.MaxRatio {
		return []Alert{{
			Kind:   "order_to_trade_ratio",
			UserID: user,
			Seq:    seq,
			Detail: fmt.Sprintf("%d orders / %d fills = OTR %.1f within %d events (limit %.1f)",
				len(places), len(fills), ratio, d.cfg.Window, d.cfg.MaxRatio),
		}}
	}
	return nil
}

// pruneSeqs keeps only the sequence numbers at or after cutoff, reusing the
// backing array.
func pruneSeqs(seqs []uint64, cutoff uint64) []uint64 {
	kept := seqs[:0]
	for _, s := range seqs {
		if s >= cutoff {
			kept = append(kept, s)
		}
	}
	return kept
}
