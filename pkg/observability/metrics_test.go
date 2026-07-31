package observability

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func mkOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

func collectorEngine(t *testing.T) (*matching.Engine, *Collector) {
	t.Helper()
	c := NewCollector()
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = c
	return matching.NewEngine(cfg), c
}

// TestCountsRealActivity — the counters have to come from the engine, not from a mock,
// or they measure the test rather than the venue.
func TestCountsRealActivity(t *testing.T) {
	e, c := collectorEngine(t)

	maker := mkOrder(t, "mm", types.SideSell, 100, 10)
	e.Process(maker)
	e.Process(mkOrder(t, "tk", types.SideBuy, 100, 4))
	rest := mkOrder(t, "mm", types.SideBuy, 90, 5)
	e.Process(rest)
	if _, err := e.Cancel(rest.ID, "mm"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	s := c.Snapshot()
	if s.Trades != 1 {
		t.Errorf("trades = %d, want 1", s.Trades)
	}
	if s.TradedLots != 4 {
		t.Errorf("traded lots = %d, want 4", s.TradedLots)
	}
	if s.TradedNotional != 400 {
		t.Errorf("notional = %d, want 400", s.TradedNotional)
	}
	if s.OrdersAccepted < 2 {
		t.Errorf("accepted = %d, want at least the two resting orders", s.OrdersAccepted)
	}
	if s.OrdersCanceled != 1 {
		t.Errorf("cancels = %d, want 1", s.OrdersCanceled)
	}
	if s.LastEventSeq == 0 {
		t.Error("no event sequence recorded; an operator cannot tell a stalled matcher from a quiet market")
	}
}

// TestExpiryIsNotCountedAsACancel — counting them together would make a venue look
// like its participants were pulling orders when its own clock was.
func TestExpiryIsNotCountedAsACancel(t *testing.T) {
	c := NewCollector()
	o := mkOrder(t, "mm", types.SideBuy, 100, 5)
	c.OnEvents([]matching.Event{
		{Seq: 1, Kind: matching.EventCanceled, Order: o},                                // a client cancel
		{Seq: 2, Kind: matching.EventCanceled, Order: o, Reason: types.ErrOrderExpired}, // an expiry
	})
	s := c.Snapshot()
	if s.OrdersCanceled != 1 {
		t.Errorf("client cancels = %d, want 1", s.OrdersCanceled)
	}
	if s.OrdersExpired != 1 {
		t.Errorf("expiries = %d, want 1", s.OrdersExpired)
	}
}

// TestRejectionsAreBrokenDownByReason — "rejections are up" is not actionable;
// "rejections are up and they are all price-band" is.
func TestRejectionsAreBrokenDownByReason(t *testing.T) {
	c := NewCollector()
	o := mkOrder(t, "mm", types.SideBuy, 100, 5)
	c.OnEvents([]matching.Event{
		{Seq: 1, Kind: matching.EventRejected, Order: o, Reason: types.ErrPriceOutsideBand},
		{Seq: 2, Kind: matching.EventRejected, Order: o, Reason: types.ErrPriceOutsideBand},
		{Seq: 3, Kind: matching.EventRejected, Order: o, Reason: types.ErrOrderNotFound},
	})
	s := c.Snapshot()
	if s.OrdersRejected != 3 {
		t.Errorf("rejections = %d, want 3", s.OrdersRejected)
	}
	if got := s.Rejections[types.ErrPriceOutsideBand.Error()]; got != 2 {
		t.Errorf("price-band rejections = %d, want 2", got)
	}
	if got := s.Rejections[types.ErrOrderNotFound.Error()]; got != 1 {
		t.Errorf("not-found rejections = %d, want 1", got)
	}
}

// TestNotionalSaturatesRatherThanWrapping — a counter that silently went negative
// would be worse than one that stops being precise.
func TestNotionalSaturatesRatherThanWrapping(t *testing.T) {
	c := NewCollector()
	huge := &types.Trade{Price: 1 << 40, Quantity: 1 << 20}
	for i := 0; i < 64; i++ {
		c.OnEvents([]matching.Event{{Seq: int64(i + 1), Kind: matching.EventTrade, Trade: huge}})
	}
	if got := c.Snapshot().TradedNotional; got < 0 {
		t.Errorf("notional went negative (%d); it must saturate", got)
	}
}

// TestGaugesAreSampledAtScrape — a gauge that cost anything between scrapes would be
// pushing, not sampling.
func TestGaugesAreSampledAtScrape(t *testing.T) {
	c := NewCollector()
	var calls int
	value := 7.0
	c.Gauge("orderbook_book_size", "Resting orders in the book.", func() float64 { calls++; return value })

	var buf strings.Builder
	if err := c.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if calls != 1 {
		t.Errorf("gauge read %d times per scrape, want 1", calls)
	}
	if !strings.Contains(buf.String(), "orderbook_book_size 7") {
		t.Errorf("gauge missing from output:\n%s", buf.String())
	}

	value = 9
	buf.Reset()
	_ = c.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), "orderbook_book_size 9") {
		t.Error("the gauge did not re-read at the next scrape")
	}
}

func TestHistogramBucketsAndQuantiles(t *testing.T) {
	h := NewHistogram()
	for i := 0; i < 100; i++ {
		h.Observe(100 * time.Nanosecond)
	}
	for i := 0; i < 10; i++ {
		h.Observe(50 * time.Millisecond) // beyond the last bound
	}
	if h.Count() != 110 {
		t.Errorf("count = %d, want 110", h.Count())
	}
	if q := h.Quantile(0.5); q != 100 {
		t.Errorf("p50 = %d, want 100", q)
	}
	// The tail must land in the overflow bucket rather than being dropped.
	if q := h.Quantile(0.99); q < 1_000_000 {
		t.Errorf("p99 = %d, want the tail reflected", q)
	}
	var buf strings.Builder
	if err := h.writePrometheus(&buf, "test_latency"); err != nil {
		t.Fatalf("writePrometheus: %v", err)
	}
	out := buf.String()
	for _, want := range []string{`test_latency_bucket{le="100"} 100`, `test_latency_bucket{le="+Inf"} 110`, "test_latency_count 110"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestExpositionFormatIsWellFormed — every metric needs HELP and TYPE, because one
// without them is one an operator has to guess about at the worst moment.
func TestExpositionFormatIsWellFormed(t *testing.T) {
	e, c := collectorEngine(t)
	e.Process(mkOrder(t, "mm", types.SideSell, 100, 10))
	e.Process(mkOrder(t, "tk", types.SideBuy, 100, 4))
	c.Gauge("orderbook_queue_depth", "Commands waiting for the matching goroutine.", func() float64 { return 3 })
	c.Histogram("orderbook_apply_latency_ns").Observe(250 * time.Nanosecond)

	var buf strings.Builder
	if err := c.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()

	var metrics, helps, types_ int
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP "):
			helps++
		case strings.HasPrefix(line, "# TYPE "):
			types_++
		case line != "":
			metrics++
		}
	}
	if helps != types_ {
		t.Errorf("%d HELP lines and %d TYPE lines; every metric needs both", helps, types_)
	}
	if metrics == 0 {
		t.Fatal("no metrics emitted")
	}
	for _, want := range []string{
		"orderbook_trades_total 1",
		"orderbook_queue_depth 3",
		"orderbook_last_event_sequence",
		"orderbook_apply_latency_ns_count 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestGaugesCarryTheirOwnHelp — a shared placeholder would satisfy the HELP-line count
// in TestExpositionFormatIsWellFormed while telling an operator nothing.
func TestGaugesCarryTheirOwnHelp(t *testing.T) {
	c := NewCollector()
	c.Gauge("orderbook_queue_depth", "Commands waiting for the matching goroutine.", func() float64 { return 0 })
	var buf strings.Builder
	if err := c.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if !strings.Contains(buf.String(), "# HELP orderbook_queue_depth Commands waiting for the matching goroutine.") {
		t.Errorf("gauge help not emitted:\n%s", buf.String())
	}
}

// TestEventSequenceNeverGoesBackwards — the sequence is the metric an operator alerts
// on for a stalled matcher, so a stale reading is the one failure it must not have.
func TestEventSequenceNeverGoesBackwards(t *testing.T) {
	c := NewCollector()
	o := mkOrder(t, "mm", types.SideBuy, 100, 5)
	c.OnEvents([]matching.Event{{Seq: 99, Kind: matching.EventAccepted, Order: o}})
	c.OnEvents([]matching.Event{{Seq: 7, Kind: matching.EventAccepted, Order: o}})
	if got := c.Snapshot().LastEventSeq; got != 99 {
		t.Errorf("last event seq = %d, want 99", got)
	}
}

// TestLabelValuesAreEscaped — an error string containing a quote would otherwise
// produce output a scraper rejects, losing every metric in the same scrape.
func TestLabelValuesAreEscaped(t *testing.T) {
	c := NewCollector()
	o := mkOrder(t, "mm", types.SideBuy, 100, 5)
	c.OnEvents([]matching.Event{
		{Seq: 1, Kind: matching.EventRejected, Order: o, Reason: errors.New(`bad "thing"` + "\n")},
	})
	var buf strings.Builder
	if err := c.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "bad \"thing\"\n} ") {
		t.Error("label value was not escaped")
	}
	if !strings.Contains(out, `\"thing\"`) || !strings.Contains(out, `\n`) {
		t.Errorf("expected escaped quotes and newline in:\n%s", out)
	}
}

// TestConcurrentUseIsSafe — the collector sits on the matching goroutine and is
// scraped from an HTTP handler, so both happen at once by construction.
func TestConcurrentUseIsSafe(t *testing.T) {
	c := NewCollector()
	c.Gauge("g", "help", func() float64 { return 1 })
	h := c.Histogram("h")
	o := mkOrder(t, "mm", types.SideBuy, 100, 5)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c.OnEvents([]matching.Event{{Seq: int64(i + 1), Kind: matching.EventAccepted, Order: o}})
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.Observe(time.Microsecond)
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			var buf strings.Builder
			_ = c.WritePrometheus(&buf)
		}
	}()
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	if c.Snapshot().OrdersAccepted == 0 {
		t.Error("nothing was counted")
	}
}
