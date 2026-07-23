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

func windowCutoff(now, width uint64) uint64 {
	if now > width {
		return now - width
	}
	return 0
}

// --- Cross-book correlation (cross-product / cross-venue manipulation) ---

// CrossBookConfig configures the cross-book correlator.
type CrossBookConfig struct {
	Window     uint64 // rolling window of events by the correlator's global sequence
	MinSymbols int    // flag a user tripping manipulation alerts in at least this many distinct books
}

// CrossBookMonitor fans events from several books into their own per-symbol
// Monitors and correlates the *manipulation* alerts across books per user. A user
// tripping manipulation-type alerts (spoofing, ramping, aggressor dominance,
// pinging, high OTR) in several correlated books at once is the cross-product /
// cross-venue signature — spoof one product to profit in another (Oystacher /
// 3Red, CFTC $2.5M). Single-book detectors miss it by construction; this is the
// SMARTS-style layer a venue adds on top of them.
type CrossBookMonitor struct {
	cfg      CrossBookConfig
	build    func() []Detector   // detector factory, one set per new book
	monitors map[string]*Monitor // per symbol
	hits     map[string][]crossHit
	seq      uint64 // global event counter across all books
}

type crossHit struct {
	seq    uint64
	symbol string
}

// NewCrossBookMonitor builds a cross-book correlator. build is called once per
// distinct symbol to create that book's detector set (e.g. a spoof + ramping
// detector), so each book is scored independently before correlation.
func NewCrossBookMonitor(cfg CrossBookConfig, build func() []Detector) *CrossBookMonitor {
	return &CrossBookMonitor{
		cfg:      cfg,
		build:    build,
		monitors: make(map[string]*Monitor),
		hits:     make(map[string][]crossHit),
	}
}

// Observe feeds an event tagged with its book symbol to that book's monitor and
// correlates any manipulation alert across books. It returns cross-book alerts;
// the underlying per-symbol alerts are recorded on each book's Monitor (reachable
// via MonitorFor).
func (c *CrossBookMonitor) Observe(symbol string, e Event) []Alert {
	c.seq++
	m := c.monitors[symbol]
	if m == nil {
		m = NewMonitor(c.build()...)
		c.monitors[symbol] = m
	}
	var out []Alert
	for _, a := range m.Observe(e) {
		if isManipulation(a.Kind) {
			out = append(out, c.record(symbol, a.UserID)...)
		}
	}
	return out
}

// MonitorFor returns the per-book Monitor for a symbol (nil if none seen yet).
func (c *CrossBookMonitor) MonitorFor(symbol string) *Monitor { return c.monitors[symbol] }

func (c *CrossBookMonitor) record(symbol, user string) []Alert {
	cutoff := windowCutoff(c.seq, c.cfg.Window)
	kept := c.hits[user][:0]
	for _, h := range c.hits[user] {
		if h.seq >= cutoff {
			kept = append(kept, h)
		}
	}
	kept = append(kept, crossHit{seq: c.seq, symbol: symbol})
	c.hits[user] = kept

	syms := make(map[string]struct{}, len(kept))
	for _, h := range kept {
		syms[h.symbol] = struct{}{}
	}
	if len(syms) >= c.cfg.MinSymbols {
		return []Alert{{
			Kind:   "cross_book_manipulation",
			UserID: user,
			Seq:    c.seq,
			Detail: fmt.Sprintf("manipulation alerts across %d books within %d events", len(syms), c.cfg.Window),
		}}
	}
	return nil
}

// isManipulation reports whether an alert kind is a manipulation signal worth
// correlating across books (vs a pure operational one).
func isManipulation(kind string) bool {
	switch kind {
	case "spoofing", "ramping", "aggressor_dominance", "pinging", "order_to_trade_ratio":
		return true
	}
	return false
}

// --- Aggressor dominance (marking / banging the close) ---

// CloseMarkingConfig configures the aggressor-dominance detector.
type CloseMarkingConfig struct {
	Window    uint64  // rolling window of events by sequence
	MinVolume int64   // require at least this much taker volume in-window before rating
	MaxShare  float64 // flag a user whose share of taker volume exceeds this (e.g. 0.7)
}

// CloseMarkingDetector flags a user who accounts for an outsized share of the
// aggressive (taker) volume in a rolling window — the observable signature of
// marking / banging the close (concentrating aggression into a print to set a
// reference price). It is time-of-session-neutral: point the window at the close
// (or run it continuously) and it catches the dominance that Athena's "Gravy"
// algo showed — >70% of the last-seconds volume (SEC 2014). Alert-only.
type CloseMarkingDetector struct {
	cfg       CloseMarkingConfig
	orderUser map[int64]string // taker order -> owner
	window    []takerFill      // (seq, user, qty), oldest first
	perUser   map[string]int64 // in-window taker volume per user
	total     int64            // in-window taker volume
}

type takerFill struct {
	seq  uint64
	user string
	qty  int64
}

// NewCloseMarkingDetector builds an aggressor-dominance detector.
func NewCloseMarkingDetector(cfg CloseMarkingConfig) *CloseMarkingDetector {
	return &CloseMarkingDetector{
		cfg:       cfg,
		orderUser: make(map[int64]string),
		perUser:   make(map[string]int64),
	}
}

// Observe implements Detector.
func (d *CloseMarkingDetector) Observe(e Event) []Alert {
	switch e.Kind {
	case OrderPlaced:
		d.orderUser[e.OrderID] = e.UserID
	case OrderCancelled:
		delete(d.orderUser, e.OrderID)
	case Trade:
		user, ok := d.orderUser[e.TakerOrderID]
		if !ok {
			return nil
		}
		d.window = append(d.window, takerFill{e.Seq, user, e.Quantity})
		d.perUser[user] += e.Quantity
		d.total += e.Quantity
		d.prune(e.Seq)
		if d.total < d.cfg.MinVolume {
			return nil
		}
		if share := float64(d.perUser[user]) / float64(d.total); share > d.cfg.MaxShare {
			return []Alert{{
				Kind:   "aggressor_dominance",
				UserID: user,
				Seq:    e.Seq,
				Detail: fmt.Sprintf("%.0f%% of %d taker volume within %d events (limit %.0f%%)",
					share*100, d.total, d.cfg.Window, d.cfg.MaxShare*100),
			}}
		}
	}
	return nil
}

func (d *CloseMarkingDetector) prune(now uint64) {
	cutoff := windowCutoff(now, d.cfg.Window)
	i := 0
	for i < len(d.window) && d.window[i].seq < cutoff {
		f := d.window[i]
		d.total -= f.qty
		if d.perUser[f.user] -= f.qty; d.perUser[f.user] <= 0 {
			delete(d.perUser, f.user)
		}
		i++
	}
	if i > 0 {
		d.window = d.window[i:]
	}
}

// --- Ramping / momentum ignition ---

// RampingConfig configures the ramping detector.
type RampingConfig struct {
	Window       uint64 // rolling window of events by sequence
	MinTrades    int    // at least this many of a user's aggressive trades in-window
	MinMoveTicks int64  // net one-directional price move over those trades to flag
}

// RampingDetector flags a user whose aggressive (taker) trades push the price
// consistently in one direction by at least MinMoveTicks within a window — the
// observable core of ramping / momentum ignition (a burst of one-sided aggression
// to move the market). It scores the sustained directional pressure, not the
// subsequent reversal (which needs post-window prices and belongs to a heavier
// model). Alert-only.
type RampingDetector struct {
	cfg       RampingConfig
	orderUser map[int64]string     // taker order -> owner
	orderSide map[int64]types.Side // taker order -> aggressor side
	hist      map[string][]rampPt  // per-user windowed aggressive trades
}

type rampPt struct {
	seq   uint64
	price int64
	side  types.Side
}

// NewRampingDetector builds a ramping detector.
func NewRampingDetector(cfg RampingConfig) *RampingDetector {
	return &RampingDetector{
		cfg:       cfg,
		orderUser: make(map[int64]string),
		orderSide: make(map[int64]types.Side),
		hist:      make(map[string][]rampPt),
	}
}

// Observe implements Detector.
func (d *RampingDetector) Observe(e Event) []Alert {
	switch e.Kind {
	case OrderPlaced:
		d.orderUser[e.OrderID] = e.UserID
		d.orderSide[e.OrderID] = e.Side
	case OrderCancelled:
		delete(d.orderUser, e.OrderID)
		delete(d.orderSide, e.OrderID)
	case Trade:
		user, ok := d.orderUser[e.TakerOrderID]
		if !ok {
			return nil
		}
		side := d.orderSide[e.TakerOrderID]
		pts := append(d.hist[user], rampPt{e.Seq, e.Price, side})
		cutoff := windowCutoff(e.Seq, d.cfg.Window)
		kept := pts[:0]
		for _, p := range pts {
			if p.seq >= cutoff {
				kept = append(kept, p)
			}
		}
		d.hist[user] = kept
		if len(kept) < d.cfg.MinTrades {
			return nil
		}
		first, last := kept[0], kept[len(kept)-1]
		move := last.price - first.price
		up := last.side == types.SideBuy && move >= d.cfg.MinMoveTicks
		down := last.side == types.SideSell && -move >= d.cfg.MinMoveTicks
		if up || down {
			return []Alert{{
				Kind:   "ramping",
				UserID: user,
				Seq:    e.Seq,
				Detail: fmt.Sprintf("%d aggressive %s trades moved price %d→%d (%d ticks) within %d events",
					len(kept), last.side, first.price, last.price, move, d.cfg.Window),
			}}
		}
	}
	return nil
}

// --- Pinging (hidden-liquidity detection) ---

// PingingConfig configures the pinging detector.
type PingingConfig struct {
	MaxSize     int64  // only orders this small or smaller are "pings" (lots)
	MaxLifetime uint64 // cancelled unfilled within this many events of placement
	Window      uint64 // rolling window of events by sequence
	MinCount    int    // more than this many pings in-window trips an alert
}

// PingingDetector flags a user firing bursts of tiny orders that are cancelled
// unfilled almost immediately — the signature of pinging: probing for hidden /
// iceberg liquidity before trading against the whale. It is the small-order dual
// of SpoofDetector (which watches large orders): surgical pings slip under a
// message-rate limit but cluster in this detector. Alert-only.
type PingingDetector struct {
	cfg   PingingConfig
	live  map[int64]pingRec   // tiny live orders
	pings map[string][]uint64 // per-user ping seqs (rolling)
}

type pingRec struct {
	user      string
	placedSeq uint64
	filled    bool
}

// NewPingingDetector builds a pinging detector.
func NewPingingDetector(cfg PingingConfig) *PingingDetector {
	return &PingingDetector{
		cfg:   cfg,
		live:  make(map[int64]pingRec),
		pings: make(map[string][]uint64),
	}
}

// Observe implements Detector.
func (d *PingingDetector) Observe(e Event) []Alert {
	switch e.Kind {
	case OrderPlaced:
		if e.Quantity <= d.cfg.MaxSize {
			d.live[e.OrderID] = pingRec{user: e.UserID, placedSeq: e.Seq}
		}
	case Trade:
		if r, ok := d.live[e.MakerOrderID]; ok {
			r.filled = true
			d.live[e.MakerOrderID] = r
		}
		if r, ok := d.live[e.TakerOrderID]; ok {
			r.filled = true
			d.live[e.TakerOrderID] = r
		}
	case OrderCancelled:
		r, ok := d.live[e.OrderID]
		if !ok {
			return nil
		}
		delete(d.live, e.OrderID)
		if r.filled || e.Seq-r.placedSeq > d.cfg.MaxLifetime {
			return nil // a filled or slowly-cancelled tiny order isn't a ping
		}
		recent := pruneSeqs(d.pings[r.user], windowCutoff(e.Seq, d.cfg.Window))
		recent = append(recent, e.Seq)
		d.pings[r.user] = recent
		if len(recent) > d.cfg.MinCount {
			return []Alert{{
				Kind:   "pinging",
				UserID: r.user,
				Seq:    e.Seq,
				Detail: fmt.Sprintf("%d tiny orders (≤%d) placed and pulled within %d events (limit %d)",
					len(recent), d.cfg.MaxSize, d.cfg.Window, d.cfg.MinCount),
			}}
		}
	}
	return nil
}
