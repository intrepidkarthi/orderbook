package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/observability"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// The durability path, timed — docs/LAG-AND-SHED.md §5.
//
// Before this the only Observe call sites in non-test code were the message-apply
// histogram and the client side of cmd/obsoak. The append and the fsync — the two
// operations that decide whether an acknowledged order survives — were measured in a
// benchmark on a laptop and nowhere else, which meant the venue published a 20 ms
// recovery point objective it could not verify.
//
// The rule everything here tests: the append histogram never contains an fsync, and
// the sync histogram contains nothing but one.

// commandLogCalls drives every method of matching.CommandLog exactly once.
//
// The map is keyed by method name so the reflection check below can prove it is
// complete rather than merely long: a seventeenth method on the interface fails this
// at the moment it is added, which is the same guard pkg/wal's entry-kind alphabet
// test puts on the replay side.
func commandLogCalls(t *testing.T, l matching.CommandLog) map[string]func() (int64, error) {
	t.Helper()
	order := func() *types.Order {
		o, err := types.NewOrder("alice", "X", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		return o
	}
	stop, err := types.NewStopOrder(order(), 99)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	leg, err := types.NewStopOrder(order(), 99)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	oco, err := types.NewOCOOrder(order(), leg)
	if err != nil {
		t.Fatalf("NewOCOOrder: %v", err)
	}
	ib, err := types.NewIcebergOrder(order(), 1)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	peg, err := types.NewPeggedOrder(order(), types.PegToBid, 0)
	if err != nil {
		t.Fatalf("NewPeggedOrder: %v", err)
	}
	trail, err := types.NewTrailingStop(order(), 5)
	if err != nil {
		t.Fatalf("NewTrailingStop: %v", err)
	}
	return map[string]func() (int64, error){
		"AppendSubmit":     func() (int64, error) { return l.AppendSubmit(order()) },
		"AppendCancel":     func() (int64, error) { return l.AppendCancel(1, "alice") },
		"AppendReduce":     func() (int64, error) { return l.AppendReduce(1, 2, "alice") },
		"AppendReplace":    func() (int64, error) { return l.AppendReplace(1, "alice", order()) },
		"AppendCancelAll":  func() (int64, error) { return l.AppendCancelAll("alice") },
		"AppendStop":       func() (int64, error) { return l.AppendStop(stop) },
		"AppendOCO":        func() (int64, error) { return l.AppendOCO(oco) },
		"AppendIceberg":    func() (int64, error) { return l.AppendIceberg(ib) },
		"AppendPegged":     func() (int64, error) { return l.AppendPegged(peg) },
		"AppendTrailing":   func() (int64, error) { return l.AppendTrailing(trail) },
		"AppendHalt":       l.AppendHalt,
		"AppendResume":     l.AppendResume,
		"AppendCancelOnly": l.AppendCancelOnly,
		"AppendSetMark":    func() (int64, error) { return l.AppendSetMark(100) },
		"AppendBust":       func() (int64, error) { return l.AppendBust(1, "erroneous order entry") },
		"AppendSetPhase":   func() (int64, error) { return l.AppendSetPhase(matching.StatePreOpen) },
	}
}

// TestAppendLatencyCountsEveryCommandKind is deliverable 9, and the assertion is not
// "the count is greater than zero" — it is that the histogram's count equals the
// log's own sequence.
//
// The Writer's sequence counts exactly the records that were appended. If the
// histogram and the sequence disagree, a method is unwrapped, and a decorator that
// forgets one silently stops timing an entire command kind while every other number
// on the page still looks right. AppendSetPhase is the obvious candidate: it is the
// method that was added last, and the one docs/JOURNAL-COMPLETENESS.md §4.2 exists
// because of.
func TestAppendLatencyCountsEveryCommandKind(t *testing.T) {
	// The interface's own method count, so a seventeenth Append* fails here rather
	// than being silently untimed.
	n := reflect.TypeOf((*matching.CommandLog)(nil)).Elem().NumMethod()
	calls := commandLogCalls(t, (*wal.Writer)(nil))
	if n != len(calls) {
		t.Fatalf("matching.CommandLog has %d methods and this tape drives %d — a command kind was added, "+
			"and until it is driven here nothing proves timedLog wraps it", n, len(calls))
	}

	path := filepath.Join(t.TempDir(), "obgw.wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	col := observability.NewCollector()
	hist := col.Histogram(walAppendLatencyMetric)
	timed := &timedLog{inner: w, hist: hist}

	for name, fn := range commandLogCalls(t, timed) {
		if _, err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	if seq := w.Seq(); hist.Count() != seq {
		t.Errorf("%s_count = %d and the log's sequence is %d — %d command kind(s) are appended and not timed",
			walAppendLatencyMetric, hist.Count(), seq, seq-hist.Count())
	}
	if hist.Sum() <= 0 {
		t.Error("every append took zero nanoseconds; the histogram is being fed a constant")
	}
}

// TestAppendLatencyCountsEveryCommandKindOnALiveVenue is the same equality through
// the venue's own wiring, because a decorator that is correct and never installed is
// the other way this fails.
func TestAppendLatencyCountsEveryCommandKindOnALiveVenue(t *testing.T) {
	cfg := durableConfig(t)
	srv := durableServer(t, cfg)
	defer srv.Close()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	for i := 0; i < 5; i++ {
		c.enter("a"+itoa(i), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100+int64(i), 10)
		if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
			t.Fatalf("a%d not accepted", i)
		}
	}
	c.cancel("a0")
	if _, ok := c.awaitType(t, wire.MsgCanceled, 3*time.Second); !ok {
		t.Fatal("cancel not applied")
	}

	if srv.appendHist == nil {
		t.Fatal("a durable venue registered no append histogram")
	}
	// Long enough for the 20 ms group-commit loop to tick several times with nothing
	// left to append. The equality has to hold across those ticks: a sync observed
	// into the append histogram would push the count past the log's sequence, which
	// is the shape of "both measurements share one histogram".
	time.Sleep(150 * time.Millisecond)
	if got, want := srv.appendHist.Count(), srv.books.first().wal.Seq(); got != want {
		t.Errorf("%s_count = %d, log sequence = %d — the append histogram is being fed something that is "+
			"not an append", walAppendLatencyMetric, got, want)
	}
}

// TestAppendLatencyExcludesTheSync is deliverable 10, and it is the ONLY test that
// can see which way round the decorators are nested.
//
// In the default configuration the two orderings behave identically, and the mode
// where it matters is the one nobody runs locally. Wrapped the wrong way, timedLog's
// measurement of an append would CONTAIN syncingLog's fsync, so the append histogram
// in the mode where durability matters most would be a copy of the sync histogram
// under a different name — which is why the decisive assertion here is that the
// append histogram's total is a small fraction of the sync histogram's, not a
// millisecond threshold that a fast disk would satisfy either way.
func TestAppendLatencyExcludesTheSync(t *testing.T) {
	cfg := durableConfig(t)
	cfg.SyncEveryCommand = true
	srv := durableServer(t, cfg)
	defer srv.Close()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	const orders = 60
	for i := 0; i < orders; i++ {
		c.enter("s"+itoa(i), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100+int64(i%9), 10)
		if _, ok := c.awaitType(t, wire.MsgAccepted, 5*time.Second); !ok {
			t.Fatalf("s%d not accepted", i)
		}
	}

	appends, syncs := srv.appendHist, srv.syncHist
	if appends.Count() != syncs.Count() {
		t.Fatalf("%d appends and %d syncs under -sync-every-command; every append syncs exactly once",
			appends.Count(), syncs.Count())
	}
	if appends.Count() < orders {
		t.Fatalf("only %d appends recorded for %d orders", appends.Count(), orders)
	}
	// The decisive one. Wrapped outside, the append interval strictly CONTAINS the
	// fsync, so this sum could never be smaller.
	if appends.Sum()*2 >= syncs.Sum() {
		t.Errorf("appends total %d ns against %d ns of fsync — the append histogram contains the sync, "+
			"which means timedLog is wrapped OUTSIDE syncingLog instead of inside",
			appends.Sum(), syncs.Sum())
	}
	// And the two are separately readable, which is the whole point: the append is
	// the venue's own work, the sync is the storage device, and a per-command venue's
	// real cost is the sum — one term each rather than one averaged number.
	if p99, syncP50 := appends.Quantile(0.99), syncs.Quantile(0.5); p99 >= syncP50 {
		t.Errorf("append p99 %d ns >= sync p50 %d ns; the two histograms are not disjoint", p99, syncP50)
	}
	if p99 := appends.Quantile(0.99); p99 > int64(time.Millisecond) {
		t.Errorf("append p99 = %d ns, above the 1 ms alert threshold docs/RUNBOOKS.md publishes, on a buffered write", p99)
	}
	t.Logf("append p50 %d ns p99 %d ns; sync p50 %d ns p99 %d ns",
		appends.Quantile(0.5), appends.Quantile(0.99), syncs.Quantile(0.5), syncs.Quantile(0.99))
}

// TestSyncLatencyIsObserved is deliverable 11, and the second half is the more
// interesting one.
//
// syncLoop is a bare goroutine on a 20 ms ticker. If it stops — panics, is never
// started by a future refactor of Serve, or is skipped by a configuration nobody
// re-read — the venue keeps accepting and acknowledging orders and stops making them
// durable, and there is no signal for that today: walFailed latches on a sync that
// FAILS, and a sync that never HAPPENS moves nothing.
//
// The count advancing at ~50/s on a COMPLETELY IDLE venue is that heartbeat. Read
// wrong it looks like a venue doing work while quiet; a heartbeat that stopped when
// the market went quiet would stop exactly when nobody is watching.
func TestSyncLatencyIsObserved(t *testing.T) {
	t.Run("the count advances on an idle venue", func(t *testing.T) {
		cfg := durableConfig(t)
		srv := durableServer(t, cfg)
		defer srv.Close()

		// No client, no orders, nothing to flush. The ticker fires anyway.
		//
		// POLLED rather than slept, and the difference is which machine the assertion
		// is about. Sleeping 250 ms and demanding 8 beats of a 20 ms ticker leaves a
		// 36% margin against the scheduler, which is fine on an idle laptop and is not
		// fine on a shared CI runner instrumenting every package for coverage — the
		// goroutine simply does not get scheduled 8 times inside that window, and the
		// venue is not what failed. This waits for the beats instead of budgeting for
		// them, so a slow machine is slow rather than red.
		//
		// It still catches the defect it names: a heartbeat that does not beat leaves
		// the count at 0 and this fails at the deadline.
		const wantBeats = 8
		var got int64
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			if got = srv.syncHist.Count(); got >= wantBeats {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if got < wantBeats {
			t.Errorf("%s_count = %d after waiting up to 10 s for %d beats of a 20 ms ticker — "+
				"the group-commit loop's only heartbeat does not beat", walSyncLatencyMetric, got, wantBeats)
		}
	})

	t.Run("per-command mode syncs once per command", func(t *testing.T) {
		cfg := durableConfig(t)
		cfg.SyncEveryCommand = true
		srv := durableServer(t, cfg)
		defer srv.Close()

		c := dial(t, srv)
		c.mustLogin("alice", "pw1")
		const orders = 20
		for i := 0; i < orders; i++ {
			c.enter("p"+itoa(i), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100+int64(i), 10)
			if _, ok := c.awaitType(t, wire.MsgAccepted, 5*time.Second); !ok {
				t.Fatalf("p%d not accepted", i)
			}
		}
		if got, want := srv.syncHist.Count(), srv.books.first().wal.Seq(); got != want {
			t.Errorf("%s_count = %d and the log wrote %d records; in per-command mode they are the same number",
				walSyncLatencyMetric, got, want)
		}
		// And the group-commit ticker is NOT also running, which would double the
		// fsyncs for no durability gain.
		before := srv.syncHist.Count()
		time.Sleep(150 * time.Millisecond)
		if after := srv.syncHist.Count(); after != before {
			t.Errorf("sync count moved from %d to %d with no commands: the ticker is running alongside "+
				"per-command mode and re-syncing an already synced file", before, after)
		}
	})
}

// TestNonDurableVenueRegistersNoDurabilityHistograms — two histograms that can only
// ever read zero are two more families on a page docs/LAG-AND-SHED.md §14 already
// worries is getting long.
func TestNonDurableVenueRegistersNoDurabilityHistograms(t *testing.T) {
	srv := testServer(t)
	if srv.appendHist != nil || srv.syncHist != nil {
		t.Error("a venue with no log registered the durability histograms")
	}
}

// TestAppendTimingAllocatesNothing is deliverable 12's assertion half: the number of
// allocations per append is UNCHANGED by the timing.
//
// Two assertions, and the first is the exact one. The measurement in isolation — two
// clock reads and one Observe — must allocate exactly zero: a time.Time and a
// time.Duration are values, nothing is boxed into an interface, and the bucket search
// is a sort.Search over seventeen constants followed by three atomic adds. That is
// deterministic, and it is what would break if Observe ever grew an interface
// parameter.
//
// The second compares the decorated append against the bare one, and it is deliberately
// a band rather than an equality. pkg/wal's own append allocates a count that drifts by
// one across a run — a bufio boundary, a marshal buffer that grew — and it drifts
// DOWNWARD as often as up, so an exact comparison fails on noise rather than on
// regressions. A measurement that allocated would add a whole object per append, which
// this catches; a baseline that wobbled by a fraction is not that.
func TestAppendTimingAllocatesNothing(t *testing.T) {
	col := observability.NewCollector()
	hist := col.Histogram(walAppendLatencyMetric)

	if n := testing.AllocsPerRun(1000, func() {
		start := time.Now()
		hist.Observe(time.Since(start))
	}); n != 0 {
		t.Errorf("the measurement itself allocates %.2f objects per call, want 0", n)
	}

	order := func() *types.Order {
		o, err := types.NewOrder("alice", "X", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		return o
	}
	open := func() *wal.Writer {
		w, err := wal.Open(filepath.Join(t.TempDir(), "obgw.wal"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = w.Close() })
		return w
	}

	bare := open()
	o := order()
	plain := allocsPerCall(2000, func() {
		if _, err := bare.AppendSubmit(o); err != nil {
			t.Fatalf("AppendSubmit: %v", err)
		}
	})

	timed := &timedLog{inner: open(), hist: hist}
	metered := allocsPerCall(2000, func() {
		if _, err := timed.AppendSubmit(o); err != nil {
			t.Fatalf("AppendSubmit: %v", err)
		}
	})

	if metered > plain+0.5 {
		t.Errorf("append allocates %.2f objects timed against %.2f untimed; the measurement is not free",
			metered, plain)
	}
	t.Logf("append allocations: %.2f untimed, %.2f timed", plain, metered)
}

// allocsPerCall counts allocations per call as a real average.
//
// testing.AllocsPerRun cannot be used here: it truncates with an integer division, so
// a true cost of 8.99 reports as 8 and 9.00 reports as 9, and the append's amortised
// count sits close enough to the boundary that the reading flips between runs. That
// jitter is a whole allocation wide, which is exactly the size of the regression this
// is meant to catch.
func allocsPerCall(n int, f func()) float64 {
	f() // warm anything one-off out of the measurement
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < n; i++ {
		f()
	}
	runtime.ReadMemStats(&after)
	return float64(after.Mallocs-before.Mallocs) / float64(n)
}

// BenchmarkAppendWithoutTiming and BenchmarkAppendWithTiming are deliverable 12: the
// cost of the measurement, published in docs/BENCHMARKS.md as a row rather than
// asserted as a claim.
//
// Read them as a pair. The claim under test is ~80 ns of clock reads and one Observe
// against a 1,625 ns append — around 5% of the append and under half a percent of the
// group-committed write path. §14 names the way that could be wrong: on a virtualised
// host where time.Now() traps into the hypervisor, two clock reads per append is not
// 5% of an append, and this benchmark run on THAT host is what would reveal it.
func BenchmarkAppendWithoutTiming(b *testing.B) {
	w, err := wal.Open(filepath.Join(b.TempDir(), "obgw.wal"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer w.Close()
	o, err := types.NewOrder("alice", "X", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	if err != nil {
		b.Fatalf("NewOrder: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.AppendSubmit(o); err != nil {
			b.Fatalf("AppendSubmit: %v", err)
		}
	}
}

func BenchmarkAppendWithTiming(b *testing.B) {
	w, err := wal.Open(filepath.Join(b.TempDir(), "obgw.wal"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer w.Close()
	col := observability.NewCollector()
	timed := &timedLog{inner: w, hist: col.Histogram(walAppendLatencyMetric)}
	o, err := types.NewOrder("alice", "X", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	if err != nil {
		b.Fatalf("NewOrder: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := timed.AppendSubmit(o); err != nil {
			b.Fatalf("AppendSubmit: %v", err)
		}
	}
}

// TestDrillTheGroupCommitLoopHasStopped is RUNBOOKS' new "The group-commit loop has
// stopped" section, and it kills the goroutine for real rather than describing what
// would happen if it died.
//
// This is the highest-severity row this slice adds and the one nobody would have
// asked for. syncLoop is a bare goroutine on a 20 ms ticker; if it stops, the venue
// keeps accepting and acknowledging orders and stops making them durable. walFailed
// latches on a sync that FAILS — a sync that never HAPPENS moves nothing, so before
// this there was no signal at all.
//
// The alert is a compound: the sync count flat WHILE the event sequence advances.
// Either half alone is ambiguous — a quiet venue advances neither — so the drill has
// to move one and hold the other.
func TestDrillTheGroupCommitLoopHasStopped(t *testing.T) {
	col := observability.NewCollector()
	w, err := wal.Open(filepath.Join(t.TempDir(), "obgw.wal"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	set := newBookSet()
	set.add(&symbolBook{symbol: "X", wal: w})
	srv := &Server{
		metrics:  col,
		syncHist: col.Histogram(walSyncLatencyMetric),
		quit:     make(chan struct{}),
		books:    set,
	}

	var seq int64
	advance := func() {
		seq++
		col.OnEvents([]matching.Event{{Seq: seq, Kind: matching.EventAccepted}})
	}

	go srv.syncLoop()

	// Healthy: the heartbeat beats on an idle venue, because the ticker fires whether
	// or not anything is buffered. Read wrong that looks like work while quiet; a
	// heartbeat that stopped when the market went quiet would stop exactly when
	// nobody is watching.
	deadline := time.Now().Add(3 * time.Second)
	for srv.syncHist.Count() < 5 && time.Now().Before(deadline) {
		advance()
		time.Sleep(5 * time.Millisecond)
	}
	if srv.syncHist.Count() < 5 {
		t.Fatalf("%s_count = %d with the loop running; the heartbeat does not beat",
			walSyncLatencyMetric, srv.syncHist.Count())
	}

	// The failure: the goroutine is gone. Nothing else about the venue changes.
	close(srv.quit)
	time.Sleep(80 * time.Millisecond) // let the loop observe quit and return
	frozen := srv.syncHist.Count()
	before := col.Snapshot().LastEventSeq

	for i := 0; i < 40; i++ {
		advance()
		time.Sleep(5 * time.Millisecond)
	}

	if after := col.Snapshot().LastEventSeq; after <= before {
		t.Fatalf("test premise broken: the event sequence did not advance (%d -> %d)", before, after)
	}
	if got := srv.syncHist.Count(); got != frozen {
		t.Errorf("%s_count moved %d -> %d after the loop was killed; if this can advance without the "+
			"goroutine, the alert cannot distinguish a dead loop from a live one",
			walSyncLatencyMetric, frozen, got)
	}
	if srv.walFailed.Load() {
		t.Error("walFailed latched: it catches a sync that FAILED, and this is a sync that never happened — " +
			"which is exactly why the count is the only signal there is")
	}
}

// BenchmarkTimingOverhead is the measurement on its own — two clock reads and one
// Observe, with no append underneath.
//
// It exists because the append-to-append difference is noisy on a laptop: the log file
// grows across a benchmark run, and page-cache behaviour moves the baseline by more
// than the thing being measured. This number does not move, and it is the one to
// re-run on a virtualised host where time.Now() may trap into the hypervisor
// (docs/LAG-AND-SHED.md §14).
func BenchmarkTimingOverhead(b *testing.B) {
	hist := observability.NewCollector().Histogram(walAppendLatencyMetric)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		hist.Observe(time.Since(start))
	}
}

// walSyncWarnThreshold and walSyncPageThreshold are docs/RUNBOOKS.md's two tiers for
// obgw_wal_sync_latency_ns p99, written here as numbers so a test can hold the
// runbook to them.
const (
	walSyncWarnThreshold = 100 * time.Millisecond
	walSyncPageThreshold = time.Second
)

// TestSyncLatencyThresholdsAreReachable is the guard on the worst kind of defect this
// slice could have shipped: a metric that reads healthy while the condition it
// describes is happening.
//
// RUNBOOKS pages when this histogram's p99 goes above one second, because that p99 is
// the variable half of the venue's recovery point objective. It shipped against a
// histogram whose top finite bucket was 250 ms, and a bucketed quantile cannot report
// a value above its top bound — so a venue whose every fsync took two seconds reported
// p99 = 250 ms, the warn tier fired, and the PAGE TIER COULD NOT. An operator reading
// the page during that incident would have concluded the durability window was a
// quarter of a second when it was two. Prometheus's histogram_quantile saturates
// identically, so the exported page was wrong in the same way the local reader was.
//
// The fix was to widen the shared bounds rather than to trim the alert, and this is
// what stops them being narrowed again: it asserts the reading an operator would take,
// at each tier, under the condition that tier exists for.
func TestSyncLatencyThresholdsAreReachable(t *testing.T) {
	observe := func(d time.Duration) *observability.Histogram {
		h := observability.NewCollector().Histogram(walSyncLatencyMetric)
		for i := 0; i < 1000; i++ {
			h.Observe(d)
		}
		return h
	}

	// Healthy: a few milliseconds an fsync, which is what APFS does in the benchmark.
	if q := observe(4 * time.Millisecond); time.Duration(q.Quantile(0.99)) > walSyncWarnThreshold {
		t.Errorf("a 4ms fsync reports p99 %s, above the %s warn tier: the venue would page on a healthy disk",
			time.Duration(q.Quantile(0.99)), walSyncWarnThreshold)
	}

	// Slow, but not yet a page. This tier must fire and the page tier must not, or the
	// two tiers are one tier and the severity split is decoration.
	slow := observe(200 * time.Millisecond)
	if q := time.Duration(slow.Quantile(0.99)); q <= walSyncWarnThreshold {
		t.Errorf("a 200ms fsync reports p99 %s, which does not cross the %s warn tier", q, walSyncWarnThreshold)
	}
	if q := time.Duration(slow.Quantile(0.99)); q > walSyncPageThreshold {
		t.Errorf("a 200ms fsync reports p99 %s and would PAGE at the %s tier; the two tiers must stay distinguishable", q, walSyncPageThreshold)
	}

	// The one that was broken. Every fsync takes two seconds, so the venue's real
	// recovery point objective is 20 ms + 2 s and somebody has to be woken up.
	bad := observe(2 * time.Second)
	if q := time.Duration(bad.Quantile(0.99)); q <= walSyncPageThreshold {
		t.Errorf("every fsync takes 2s and p99 reports %s, which does not cross the %s page tier — "+
			"the highest-severity durability alert in RUNBOOKS cannot fire, and an operator reading %s "+
			"during the incident would put the durability window two orders of magnitude below the truth",
			q, walSyncPageThreshold, q)
	}
	// The mean is exact at any magnitude and is what RUNBOOKS sends the operator to
	// when the quantile is pinned at the top bound. It must agree with reality even
	// where the quantile has stopped being able to.
	if mean := time.Duration(bad.Sum() / bad.Count()); mean < 2*time.Second {
		t.Errorf("_sum/_count reports %s for a population of 2s fsyncs; the exact reading is not exact", mean)
	}

	// And the exported page has to be able to say it too: Prometheus computes the
	// quantile from the le= bounds, so a bound above the page threshold must exist on
	// the wire and not only in this process.
	var buf strings.Builder
	col := observability.NewCollector()
	col.Histogram(walSyncLatencyMetric).Observe(2 * time.Second)
	if err := col.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	want := fmt.Sprintf("%s_bucket{le=\"%d\"}", walSyncLatencyMetric, walSyncPageThreshold.Nanoseconds())
	if !strings.Contains(buf.String(), want) {
		t.Errorf("the exposition has no %s bound, so histogram_quantile cannot report a p99 above the page "+
			"threshold either:\n%s", want, buf.String())
	}
}
