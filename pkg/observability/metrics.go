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
	"strings"
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

	gaugeMu  sync.RWMutex
	gauges   map[string]gauge
	families map[string]gaugeFamily

	histMu sync.Mutex
	hists  map[string]*Histogram

	// counters are the series that are NOT engine events: work the venue refused
	// before the event stream existed. Registration takes the lock; the increment
	// does not, because the caller keeps the handle. See Collector.Counter.
	counterMu sync.Mutex
	counters  map[string]*counterFamily
}

// NewCollector builds an empty collector.
func NewCollector() *Collector {
	return &Collector{
		reasons:  map[string]*atomic.Int64{},
		gauges:   map[string]gauge{},
		families: map[string]gaugeFamily{},
		hists:    map[string]*Histogram{},
		counters: map[string]*counterFamily{},
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

// Label is one dimension of a metric series.
type Label struct{ Name, Value string }

// Series is one labelled reading inside a gauge family.
type Series struct {
	Labels []Label
	Value  float64
}

type gaugeFamily struct {
	help string
	read func() []Series
}

// GaugeFamily registers a gauge that has one reading PER LABEL SET rather than
// one reading full stop — a venue's best bid once per instrument, say.
//
// It exists because the alternative is worse in a specific way. A gauge that
// aggregates across dimensions is fine for anything countable (queue depth is
// queue depth) and meaningless for anything that is a price: an average of two
// instruments' last trade is not a number anybody can act on, and reporting just
// one of them under a bare name is a graph that is quietly about the wrong book.
//
// HELP and TYPE are written once for the family and one line per series, which is
// what the exposition format requires and what registering the same name twice
// would get wrong.
//
// fn is called during a scrape: it must not block, and it should return series in
// a stable order — though Render sorts them anyway, so a scrape is byte-stable
// even if a caller's map iteration is not.
func (c *Collector) GaugeFamily(name, help string, fn func() []Series) {
	c.gaugeMu.Lock()
	defer c.gaugeMu.Unlock()
	c.families[name] = gaugeFamily{help: help, read: fn}
}

// renderLabels turns a label set into the {a="1",b="2"} the format wants.
func renderLabels(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	sorted := make([]Label, len(labels))
	copy(sorted, labels)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range sorted {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteByte('=')
		b.WriteString(quote(l.Value))
	}
	b.WriteByte('}')
	return b.String()
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

// Counter is a monotone count of something that is not an engine event.
//
// The engine's own counters live on the Collector and are fed from OnEvents. This
// one exists for everything that never reaches the event stream: an order the
// gateway refused before the matcher saw it, a mass cancel the queue would not
// take. Those are counts of work the venue REFUSED, and until this type there was
// nowhere to put them — see docs/LAG-AND-SHED.md §3.
//
// One atomic.Int64. The handle is resolved once, at registration, so an increment
// is one atomic add on a pointer the caller already holds: no map, no hash, no
// lock, no allocation. That matters because the path that increments hardest is
// the shed path, which runs while the venue is shedding and has least to spare.
type Counter struct{ v atomic.Int64 }

// Add increases the counter. n is signed for the same reason atomic.Int64 is, and
// callers pass 1; a negative n would produce a series that rate() reads as a reset,
// which is a lie this type will not stop you telling.
func (ctr *Counter) Add(n int64) { ctr.v.Add(n) }

// Value reads the counter.
func (ctr *Counter) Value() int64 { return ctr.v.Load() }

// counterFamily is one metric name and every label set registered under it.
type counterFamily struct {
	help   string
	series map[string]*counterSeries // keyed by rendered label set
}

type counterSeries struct {
	rendered string
	ctr      *Counter
}

// Counter registers (or returns) a monotone counter series and hands back the
// handle to increment.
//
// Same name plus the same labels returns the SAME handle, exactly as
// Histogram(name) does — registering twice is idempotent rather than a duplicate
// series. Same name with different labels is another series in the same family,
// rendered under one HELP and one TYPE line like a gauge family.
//
// This is deliberately NOT part of Snapshot. Snapshot is the fixed struct of engine
// counters, and a map of maps bolted onto it would grow a frozen surface for a
// convenience nothing needs: a caller either holds the handle or reads the
// exposition, which is what an operator's monitoring does anyway.
func (c *Collector) Counter(name, help string, labels ...Label) *Counter {
	key := renderLabels(labels)
	c.counterMu.Lock()
	defer c.counterMu.Unlock()
	fam, ok := c.counters[name]
	if !ok {
		fam = &counterFamily{help: help, series: map[string]*counterSeries{}}
		c.counters[name] = fam
	}
	if s, ok := fam.series[key]; ok {
		return s.ctr
	}
	s := &counterSeries{rendered: key, ctr: &Counter{}}
	fam.series[key] = s
	return s.ctr
}

// writeCounters emits every registered counter family: HELP and TYPE once per name,
// one line per label set, both sorted so a scrape is byte-stable.
func (c *Collector) writeCounters(w io.Writer) error {
	c.counterMu.Lock()
	names := make([]string, 0, len(c.counters))
	for name := range c.counters {
		names = append(names, name)
	}
	sort.Strings(names)
	type rendered struct {
		name, help string
		lines      []string
		values     []int64
	}
	out := make([]rendered, 0, len(names))
	for _, name := range names {
		fam := c.counters[name]
		keys := make([]string, 0, len(fam.series))
		for k := range fam.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		r := rendered{name: name, help: fam.help}
		for _, k := range keys {
			r.lines = append(r.lines, k)
			r.values = append(r.values, fam.series[k].ctr.Value())
		}
		out = append(out, r)
	}
	c.counterMu.Unlock()

	for _, r := range out {
		if len(r.lines) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", r.name, r.help, r.name); err != nil {
			return err
		}
		for i, labels := range r.lines {
			if _, err := fmt.Fprintf(w, "%s%s %d\n", r.name, labels, r.values[i]); err != nil {
				return err
			}
		}
	}
	return nil
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

	if err := c.writeCounters(w); err != nil {
		return err
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

	c.gaugeMu.RLock()
	fnames := make([]string, 0, len(c.families))
	for name := range c.families {
		fnames = append(fnames, name)
	}
	sort.Strings(fnames)
	fams := make([]gaugeFamily, len(fnames))
	for i, name := range fnames {
		fams[i] = c.families[name]
	}
	c.gaugeMu.RUnlock()
	for i, name := range fnames {
		series := fams[i].read()
		if len(series) == 0 {
			continue
		}
		rendered := make([]string, len(series))
		for j, sr := range series {
			rendered[j] = renderLabels(sr.Labels)
		}
		sort.SliceStable(series, func(a, b int) bool { return rendered[a] < rendered[b] })
		sort.Strings(rendered)
		// HELP and TYPE once, then one line per series — registering the same name
		// through Gauge twice would emit them per line and produce a document most
		// parsers accept and none should have to.
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, fams[i].help, name); err != nil {
			return err
		}
		for j, sr := range series {
			if _, err := fmt.Fprintf(w, "%s%s %s\n", name, rendered[j], formatFloat(sr.Value)); err != nil {
				return err
			}
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
// want to. The bucket boundaries span the range this engine actually operates in —
// tens of nanoseconds for a cancel, milliseconds for a mass cancel or a recovery —
// and, at the top, the range a degraded storage device operates in, because a
// paging threshold above the highest bound is a threshold that can never fire.
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
	// And past HERE is a storage device that has stopped behaving like one.
	//
	// These two exist for a specific alert: obgw_wal_sync_latency_ns pages at a p99
	// above one second, because that p99 is the variable half of the venue's recovery
	// point objective (docs/LAG-AND-SHED.md §5.4). With 250 ms as the top finite
	// bound, a venue whose every fsync took two seconds reported a p99 of exactly
	// 250000000 — both from Quantile here and from Prometheus's histogram_quantile,
	// which returns the highest finite bound when the quantile lands in +Inf. The
	// warn tier fired, the page tier could not, and the number an operator read
	// during the incident was a quarter of a second when the truth was two.
	//
	// A threshold above the top bucket is not a strict threshold, it is a dead one,
	// and a metric reading healthy while the thing it measures is the thing that is
	// slow is worse than no metric at all. So the range is extended to cover the
	// alert rather than the alert trimmed to fit the range. Five seconds is where an
	// fsync stops being slow and starts being a failed device; beyond it the +Inf
	// bucket and the exact _sum/_count mean carry the reading.
	1_000_000_000, 5_000_000_000,
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
//
// A reading equal to the top bound means "at least that", not "that": everything in
// the +Inf bucket reports as the top bound. Sum()/Count() is exact at any magnitude
// and is what to read when a quantile is pinned there.
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
