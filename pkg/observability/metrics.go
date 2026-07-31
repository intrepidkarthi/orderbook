// Package observability turns the engine's event stream and gauges into metrics a
// venue can actually be operated from.
//
// It exists because an unobservable venue is one you cannot operate even when it is
// behaving. Correctness tells you the engine does the right thing; metrics tell you
// whether it is doing it right now, and that is a different question that no test
// answers.
//
// # No dependencies
//
// The exposition format is Prometheus text, emitted directly. A Prometheus client
// library is a large dependency for something that is, written out, a few dozen lines
// of formatting — and this repository's one dependency is a decimal type. Anything
// that scrapes Prometheus scrapes this.
//
// # What it costs the matching goroutine
//
// Counters are atomic adds, because Collector.OnEvents is called inline on the
// matching goroutine like every other sink. A mutex there would put lock contention on
// the hot path to service a scrape that happens every fifteen seconds. Reading the
// metrics takes no lock either: a scrape is a set of atomic loads, so it can never
// stall matching.
//
// The histogram is the one thing that is not free — a bucket search per observation —
// so it is fed from wherever the caller measures, not from the event stream.
package observability

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

// Collector accumulates engine activity. It implements matching.EventSink, so attach
// it alongside your other sinks with matching.MultiSink.
//
// Safe for concurrent use: writes are atomic and reads are atomic loads.
type Collector struct {
	ordersAccepted atomic.Int64
	ordersRejected atomic.Int64
	ordersCanceled atomic.Int64
	ordersExpired  atomic.Int64
	ordersReplaced atomic.Int64
	stopsTriggered atomic.Int64
	trades         atomic.Int64
	tradedLots     atomic.Int64
	tradedNotional atomic.Int64
	halts          atomic.Int64
	resumes        atomic.Int64
	cancelOnly     atomic.Int64
	eventsSeen     atomic.Int64
	// lastEventSeq is the engine sequence of the most recent event. A stalled matcher
	// is invisible in rate metrics — everything simply reads zero, which looks
	// identical to a quiet market — so the sequence itself is exported and an
	// operator alerts on it not moving.
	lastEventSeq atomic.Int64

	// rejections by reason, because "rejections are up" is not actionable and
	// "rejections are up and they are all price-band" is.
	reasonMu sync.RWMutex
	reasons  map[string]*atomic.Int64

	gaugeMu sync.RWMutex
	gauges  map[string]gauge

	histMu sync.Mutex
	hists  map[string]*Histogram
}

// NewCollector builds an empty collector.
func NewCollector() *Collector {
	return &Collector{
		reasons: map[string]*atomic.Int64{},
		gauges:  map[string]gauge{},
		hists:   map[string]*Histogram{},
	}
}

// OnEvents implements matching.EventSink.
func (c *Collector) OnEvents(evs []matching.Event) {
	for i := range evs {
		e := &evs[i]
		c.eventsSeen.Add(1)
		c.advanceSeq(e.Seq)
		switch e.Kind {
		case matching.EventAccepted:
			c.ordersAccepted.Add(1)
		case matching.EventRejected:
			c.ordersRejected.Add(1)
			c.countReason(e.Reason)
		case matching.EventCanceled:
			// An expiry is a cancel the venue issued, and counting it as a client
			// cancel would make a venue look like its participants were pulling orders
			// when in fact its own clock was.
			if e.Reason != nil {
				c.ordersExpired.Add(1)
			} else {
				c.ordersCanceled.Add(1)
			}
		case matching.EventReplaced:
			c.ordersReplaced.Add(1)
		case matching.EventTriggered:
			c.stopsTriggered.Add(1)
		case matching.EventTrade:
			if e.Trade == nil {
				continue
			}
			c.trades.Add(1)
			c.tradedLots.Add(e.Trade.Quantity)
			// Saturating rather than wrapping: a notional counter that silently went
			// negative would be worse than one that stops being precise.
			if prod, ok := mulNoOverflow(e.Trade.Price, e.Trade.Quantity); ok {
				c.addSaturating(&c.tradedNotional, prod)
			}
		case matching.EventHalted:
			c.halts.Add(1)
		case matching.EventResumed:
			c.resumes.Add(1)
		case matching.EventCancelOnly:
			c.cancelOnly.Add(1)
		}
	}
}

// advanceSeq moves lastEventSeq forward and never back. A plain load-then-store would
// be race-free and still wrong: two goroutines could interleave and leave the lower
// sequence published, which is precisely the reading an operator would then alert on.
func (c *Collector) advanceSeq(seq int64) {
	for {
		old := c.lastEventSeq.Load()
		if seq <= old || c.lastEventSeq.CompareAndSwap(old, seq) {
			return
		}
	}
}

func (c *Collector) addSaturating(dst *atomic.Int64, delta int64) {
	for {
		old := dst.Load()
		sum := old + delta
		if delta > 0 && sum < old {
			sum = 1<<63 - 1
		}
		if dst.CompareAndSwap(old, sum) {
			return
		}
	}
}

func mulNoOverflow(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if p/b != a {
		return 0, false
	}
	return p, true
}

func (c *Collector) countReason(err error) {
	name := "none"
	if err != nil {
		name = err.Error()
	}
	c.reasonMu.RLock()
	ctr, ok := c.reasons[name]
	c.reasonMu.RUnlock()
	if ok {
		ctr.Add(1)
		return
	}
	c.reasonMu.Lock()
	defer c.reasonMu.Unlock()
	if ctr, ok := c.reasons[name]; ok {
		ctr.Add(1)
		return
	}
	ctr = &atomic.Int64{}
	ctr.Add(1)
	c.reasons[name] = ctr
}

type gauge struct {
	help string
	read func() float64
}

// Gauge registers a named value read at scrape time — queue depth, book size, the
// venue's phase. Sampled rather than pushed, so a gauge costs nothing between scrapes.
//
// help is required by the same rule as everything else here: a metric an operator has
// to guess the meaning of is one they will guess about at the worst possible moment.
//
// fn is called during a scrape, so it must not block and must be safe from the
// scraping goroutine. Wrap anything engine-owned in the Runner's accessors rather than
// reading the engine directly.
func (c *Collector) Gauge(name, help string, fn func() float64) {
	c.gaugeMu.Lock()
	defer c.gaugeMu.Unlock()
	c.gauges[name] = gauge{help: help, read: fn}
}

// Histogram returns (creating if needed) a latency histogram under name.
func (c *Collector) Histogram(name string) *Histogram {
	c.histMu.Lock()
	defer c.histMu.Unlock()
	if h, ok := c.hists[name]; ok {
		return h
	}
	h := NewHistogram()
	c.hists[name] = h
	return h
}

// Snapshot is the counter set at one instant, for tests and for anything that wants
// the numbers without parsing an exposition format.
type Snapshot struct {
	EventsSeen     int64
	LastEventSeq   int64
	OrdersAccepted int64
	OrdersRejected int64
	OrdersCanceled int64
	OrdersExpired  int64
	OrdersReplaced int64
	StopsTriggered int64
	Trades         int64
	TradedLots     int64
	TradedNotional int64
	Halts          int64
	Resumes        int64
	CancelOnly     int64
	Rejections     map[string]int64
}

// Snapshot reads every counter.
func (c *Collector) Snapshot() Snapshot {
	s := Snapshot{
		EventsSeen:     c.eventsSeen.Load(),
		LastEventSeq:   c.lastEventSeq.Load(),
		OrdersAccepted: c.ordersAccepted.Load(),
		OrdersRejected: c.ordersRejected.Load(),
		OrdersCanceled: c.ordersCanceled.Load(),
		OrdersExpired:  c.ordersExpired.Load(),
		OrdersReplaced: c.ordersReplaced.Load(),
		StopsTriggered: c.stopsTriggered.Load(),
		Trades:         c.trades.Load(),
		TradedLots:     c.tradedLots.Load(),
		TradedNotional: c.tradedNotional.Load(),
		Halts:          c.halts.Load(),
		Resumes:        c.resumes.Load(),
		CancelOnly:     c.cancelOnly.Load(),
		Rejections:     map[string]int64{},
	}
	c.reasonMu.RLock()
	for name, ctr := range c.reasons {
		s.Rejections[name] = ctr.Load()
	}
	c.reasonMu.RUnlock()
	return s
}

// WritePrometheus emits the Prometheus text exposition format.
//
// Counter names end in _total and carry HELP and TYPE lines, because a metric without
// them is one an operator has to guess the meaning of at the moment they can least
// afford to.
func (c *Collector) WritePrometheus(w io.Writer) error {
	s := c.Snapshot()
	counters := []struct {
		name, help string
		value      int64
	}{
		{"orderbook_events_total", "Engine events observed.", s.EventsSeen},
		{"orderbook_orders_accepted_total", "Orders that entered the book or began filling.", s.OrdersAccepted},
		{"orderbook_orders_rejected_total", "Orders the engine refused.", s.OrdersRejected},
		{"orderbook_orders_canceled_total", "Orders cancelled by a client or an operator.", s.OrdersCanceled},
		{"orderbook_orders_expired_total", "Orders removed by the venue when their time-in-force elapsed.", s.OrdersExpired},
		{"orderbook_orders_replaced_total", "Orders resized in place, keeping queue position.", s.OrdersReplaced},
		{"orderbook_stops_triggered_total", "Conditional orders whose trigger was reached.", s.StopsTriggered},
		{"orderbook_trades_total", "Executions printed.", s.Trades},
		{"orderbook_traded_lots_total", "Quantity executed, in lots.", s.TradedLots},
		{"orderbook_traded_notional_total", "Executed notional in tick-lots (saturating).", s.TradedNotional},
		{"orderbook_halts_total", "Transitions into a halted state.", s.Halts},
		{"orderbook_resumes_total", "Transitions back to open.", s.Resumes},
		{"orderbook_cancel_only_total", "Transitions into a phase that refuses new liquidity.", s.CancelOnly},
	}
	for _, m := range counters {
		if err := writeMetric(w, m.name, m.help, "counter", "", float64(m.value)); err != nil {
			return err
		}
	}
	// Not a _total: it is the engine's sequence, and an operator alerts on it standing
	// still. A stalled matcher looks exactly like a quiet market in every rate metric.
	if err := writeMetric(w, "orderbook_last_event_sequence", "Sequence of the most recent engine event; alert if it stops advancing.", "gauge", "", float64(s.LastEventSeq)); err != nil {
		return err
	}

	names := make([]string, 0, len(s.Rejections))
	for name := range s.Rejections {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 0 {
		fmt.Fprintln(w, "# HELP orderbook_rejections_total Order rejections by reason.")
		fmt.Fprintln(w, "# TYPE orderbook_rejections_total counter")
		for _, name := range names {
			fmt.Fprintf(w, "orderbook_rejections_total{reason=%s} %d\n", quote(name), s.Rejections[name])
		}
	}

	c.gaugeMu.RLock()
	gnames := make([]string, 0, len(c.gauges))
	for name := range c.gauges {
		gnames = append(gnames, name)
	}
	sort.Strings(gnames)
	gauges := make([]gauge, len(gnames))
	for i, name := range gnames {
		gauges[i] = c.gauges[name]
	}
	c.gaugeMu.RUnlock()
	for i, name := range gnames {
		if err := writeMetric(w, name, gauges[i].help, "gauge", "", gauges[i].read()); err != nil {
			return err
		}
	}

	c.histMu.Lock()
	hnames := make([]string, 0, len(c.hists))
	for name := range c.hists {
		hnames = append(hnames, name)
	}
	sort.Strings(hnames)
	hists := make([]*Histogram, len(hnames))
	for i, name := range hnames {
		hists[i] = c.hists[name]
	}
	c.histMu.Unlock()
	for i, name := range hnames {
		if err := hists[i].writePrometheus(w, name); err != nil {
			return err
		}
	}
	return nil
}

func writeMetric(w io.Writer, name, help, typ, labels string, v float64) error {
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s%s %s\n",
		name, help, name, typ, name, labels, formatFloat(v)); err != nil {
		return err
	}
	return nil
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// quote renders a Prometheus label value, escaping what the format requires.
func quote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			out = append(out, '\\', s[i])
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, s[i])
		}
	}
	return string(append(out, '"'))
}

// Histogram is a fixed-bucket latency histogram in nanoseconds.
//
// Fixed buckets rather than a summary with quantiles, because quantiles cannot be
// aggregated across instances and a venue that ever runs more than one process will
// want to. The bucket boundaries span the range this engine actually operates in:
// tens of nanoseconds for a cancel, milliseconds for a mass cancel or a recovery.
type Histogram struct {
	buckets []atomic.Int64
	count   atomic.Int64
	sum     atomic.Int64
}

// histogramBounds are upper bounds in nanoseconds.
var histogramBounds = []int64{
	100, 250, 500, 1_000, 2_500, 5_000, 10_000, 25_000, 50_000,
	100_000, 250_000, 500_000, 1_000_000, 5_000_000, 25_000_000,
	// Past here is not the engine, it is something being waited on: a scheduler that
	// did not run us, a socket that did not drain, a GC pause. The soak harness
	// measures client-observed latency with the same buckets and a tail that stopped
	// at 25 ms would report every one of those as the same number.
	100_000_000, 250_000_000,
}

// NewHistogram builds an empty histogram.
func NewHistogram() *Histogram {
	return &Histogram{buckets: make([]atomic.Int64, len(histogramBounds)+1)}
}

// Observe records one duration.
func (h *Histogram) Observe(d time.Duration) {
	ns := d.Nanoseconds()
	h.count.Add(1)
	h.sum.Add(ns)
	i := sort.Search(len(histogramBounds), func(i int) bool { return ns <= histogramBounds[i] })
	h.buckets[i].Add(1)
}

// Count and Sum report the observation count and total nanoseconds.
func (h *Histogram) Count() int64 { return h.count.Load() }
func (h *Histogram) Sum() int64   { return h.sum.Load() }

// Quantile estimates a quantile from the bucket counts. Bucketed, so it is an upper
// bound within a bucket's width rather than an exact value — stated because a
// histogram quantile presented as exact is a small lie that compounds.
func (h *Histogram) Quantile(q float64) int64 {
	total := h.count.Load()
	if total == 0 {
		return 0
	}
	target := int64(q * float64(total))
	var seen int64
	for i := range h.buckets {
		seen += h.buckets[i].Load()
		if seen >= target {
			if i >= len(histogramBounds) {
				return histogramBounds[len(histogramBounds)-1]
			}
			return histogramBounds[i]
		}
	}
	return histogramBounds[len(histogramBounds)-1]
}

func (h *Histogram) writePrometheus(w io.Writer, name string) error {
	fmt.Fprintf(w, "# HELP %s Latency in nanoseconds.\n# TYPE %s histogram\n", name, name)
	var cumulative int64
	for i, bound := range histogramBounds {
		cumulative += h.buckets[i].Load()
		if _, err := fmt.Fprintf(w, "%s_bucket{le=\"%d\"} %d\n", name, bound, cumulative); err != nil {
			return err
		}
	}
	cumulative += h.buckets[len(histogramBounds)].Load()
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, cumulative)
	fmt.Fprintf(w, "%s_sum %d\n", name, h.sum.Load())
	_, err := fmt.Fprintf(w, "%s_count %d\n", name, h.count.Load())
	return err
}
