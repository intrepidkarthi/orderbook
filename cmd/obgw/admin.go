package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"sync"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/observability"
)

// The admin edge: a third listener, serving metrics and health to the operator
// rather than to a participant.
//
// It is a separate port for the same reason market data is: order entry is
// authenticated and per-account, market data is anonymous and identical for
// everyone, and this is neither — it is for whoever runs the venue, and it should be
// reachable from a monitoring network that participants cannot reach at all. Putting
// it on the order-entry port would mean either exposing the venue's internals to
// participants or writing authorisation into a code path that has none.
//
// # What a scrape may and may not do
//
// Nothing served here goes through the command queue. Every gauge reads either an
// independently-locked structure or an atomic, so a scrape cannot block behind the
// matching goroutine.
//
// That constraint is the whole design. A metrics endpoint that enqueued a command
// would answer promptly while the venue was healthy and hang exactly when the matcher
// stalled — losing the reading at the only moment anybody wanted it, and, if an
// orchestrator probes the same port, converting a stall into a kill. So
// Runner.TrailingStopCount and Runner.IndicativeAuction, which do go through the
// queue, are deliberately not exported here.
//
// # Liveness and readiness say different things
//
// /healthz is liveness: the process exists and its listeners are bound. It does not
// probe the matcher, because a failed liveness check means "restart me", and
// restarting a venue that is holding a book because a probe was slow is worse than
// the stall it was reacting to.
//
// /readyz is readiness: the matcher is keeping up. That is the check that should take
// a node out of rotation, and it is derived from signals that cost nothing — queue
// occupancy, and whether the event sequence advances while work is waiting.

const (
	// readyQueueHighWater is the queue occupancy above which the venue reports itself
	// unready. Not 100%: by the time the queue is full, clients are already being
	// refused, and a readiness signal that only fires after the damage is a status
	// light, not a control.
	readyQueueHighWater = 0.75
	// stallWindow is how long the event sequence may stand still, with commands
	// waiting, before the matcher is called stuck. It is generous — a mass cancel
	// blocks the matching goroutine for ~872 µs per 5,000 orders, and a large
	// auction uncross is longer still, so anything tighter would alert on the engine
	// doing its job.
	stallWindow = 2 * time.Second

	adminReadTimeout  = 5 * time.Second
	adminWriteTimeout = 10 * time.Second
)

// admin holds the operator-facing state that is not part of serving a market.
type admin struct {
	ln  net.Listener
	srv *http.Server

	// stall detection: the last event sequence observed by a readiness probe, and
	// when it was observed. Guarded by its own mutex because probes can overlap.
	mu        sync.Mutex
	lastSeq   int64
	lastMoved time.Time
}

// startAdmin binds and serves the admin listener. It is separate from Listen so a
// misconfigured admin port cannot stop the venue from trading: the market matters
// more than the monitoring, and a venue that refused to start because Prometheus
// could not be served would be the wrong trade.
func (s *Server) startAdmin() error {
	if s.cfg.AdminAddr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.AdminAddr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)

	s.admin.ln = ln
	s.admin.srv = &http.Server{
		Handler:      mux,
		ReadTimeout:  adminReadTimeout,
		WriteTimeout: adminWriteTimeout,
	}
	s.admin.lastMoved = time.Now()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.admin.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("obgw: admin: %v", err)
		}
	}()
	return nil
}

// AdminAddr reports the bound admin address, valid after Listen when configured.
func (s *Server) AdminAddr() net.Addr {
	if s.admin.ln == nil {
		return nil
	}
	return s.admin.ln.Addr()
}

func (s *Server) closeAdmin() {
	if s.admin.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.admin.srv.Shutdown(ctx)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := s.metrics.WritePrometheus(w); err != nil {
		log.Printf("obgw: metrics: %v", err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ready, why := s.readiness()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	fmt.Fprintln(w, why)
}

// readiness reports whether the matcher is keeping up, and why not when it is not.
//
// Two signals, both free:
//
//   - Queue occupancy. Past the high-water mark the venue is behind, and the next
//     thing that happens is clients being refused.
//   - Progress. If commands are waiting and the engine's event sequence has not
//     moved for stallWindow, the matching goroutine is stuck. A stalled matcher is
//     invisible in every rate metric — they all read zero, which is exactly what a
//     quiet market reads — so the sequence standing still is the only signal there is.
//
// Progress is judged only while the queue is non-empty, because an idle venue does
// not advance its sequence either and calling that a stall would take a healthy node
// out of rotation every quiet minute.
func (s *Server) readiness() (bool, string) {
	// The worst book decides. A venue with one wedged matcher is not ready however
	// healthy the others are, and averaging would hide exactly the case this exists
	// to catch.
	depth, capacity := 0, 0
	worst := -1.0
	for _, b := range s.books.all() {
		d, c := b.runner.QueueLen(), b.runner.QueueCap()
		if c <= 0 {
			continue
		}
		if ratio := float64(d) / float64(c); ratio > worst {
			worst, depth, capacity = ratio, d, c
		}
	}
	if capacity == 0 {
		return s.readinessWith(s.runner.QueueLen(), s.runner.QueueCap())
	}
	return s.readinessWith(depth, capacity)
}

// readinessWith takes the queue reading as arguments so a test can present the two
// states that matter — a backing queue and a stuck matcher — without wedging the real
// matching goroutine, which would mean leaving it blocked past the end of the test.
func (s *Server) readinessWith(depth, capacity int) (bool, string) {
	seq := s.metrics.Snapshot().LastEventSeq
	now := time.Now()

	s.admin.mu.Lock()
	if seq != s.admin.lastSeq {
		s.admin.lastSeq = seq
		s.admin.lastMoved = now
	}
	since := now.Sub(s.admin.lastMoved)
	s.admin.mu.Unlock()

	if depth > 0 && since > stallWindow {
		return false, fmt.Sprintf("stalled: %d commands queued, event sequence stuck at %d for %s", depth, seq, since.Truncate(time.Millisecond))
	}
	if capacity > 0 && float64(depth)/float64(capacity) > readyQueueHighWater {
		return false, fmt.Sprintf("behind: queue %d/%d", depth, capacity)
	}
	return true, fmt.Sprintf("ready: queue %d/%d, event sequence %d", depth, capacity, seq)
}

// registerGauges wires the venue's sampled values into the collector.
//
// Every one of these reads a structure that carries its own lock or an atomic. None
// of them enqueues a command — see the note at the top of this file.
func (s *Server) registerGauges() {
	c := s.metrics

	// Countable values sum across books; a venue's queue depth is its queue depth.
	//
	// The price gauges below do NOT sum, and cannot: a last trade price averaged
	// over two instruments is not a number. At a one-book venue they read that
	// book, exactly as they always have. At a multi-book venue they read the FIRST
	// book and are wrong for the rest — the right answer is one series per symbol,
	// which needs label support this reference collector does not have. Stated here
	// rather than left for an operator to discover from a graph.
	sum := func(f func(*matching.Runner) int) func() float64 {
		return func() float64 {
			var n int
			for _, b := range s.books.all() {
				n += f(b.runner)
			}
			return float64(n)
		}
	}

	c.Gauge("orderbook_queue_depth", "Commands buffered for the matching goroutines, summed across books.",
		sum((*matching.Runner).QueueLen))
	c.Gauge("orderbook_queue_capacity", "Command queue capacity, summed across books; depth approaching this means clients are about to be refused.",
		sum((*matching.Runner).QueueCap))
	c.Gauge("orderbook_resting_orders", "Orders resting across every book.",
		sum((*matching.Runner).OrderCount))
	c.Gauge("orderbook_pending_stops", "Conditional orders waiting for their trigger, across every book.",
		sum((*matching.Runner).PendingStopCount))
	// Prices carry a symbol label, one series per book, and they do so even at a
	// one-book venue. A metric whose label set depends on how the venue happens to
	// be configured is one no dashboard can be written against — the series would
	// appear and disappear as instruments are added, which is worse than either
	// answer taken consistently.
	perBook := func(f func(*matching.Runner) float64) func() []observability.Series {
		return func() []observability.Series {
			all := s.books.all()
			out := make([]observability.Series, 0, len(all))
			for _, b := range all {
				out = append(out, observability.Series{
					Labels: []observability.Label{{Name: "symbol", Value: b.symbol}},
					Value:  f(b.runner),
				})
			}
			return out
		}
	}

	c.GaugeFamily("orderbook_last_trade_price", "Most recent execution price, in ticks, per instrument.",
		perBook(func(r *matching.Runner) float64 { return float64(r.LastTradePrice()) }))
	// NaN rather than zero for an empty side. Zero is a price, and a monitoring
	// system cannot tell a missing bid from a bid at zero; Prometheus understands NaN
	// and will not average it into anything.
	c.GaugeFamily("orderbook_best_bid", "Best bid in ticks, per instrument; NaN when there is no bid.",
		perBook(func(r *matching.Runner) float64 {
			if p, _, ok := r.BestBid(); ok {
				return float64(p)
			}
			return math.NaN()
		}))
	c.GaugeFamily("orderbook_best_ask", "Best ask in ticks, per instrument; NaN when there is no offer.",
		perBook(func(r *matching.Runner) float64 {
			if p, _, ok := r.BestAsk(); ok {
				return float64(p)
			}
			return math.NaN()
		}))
	c.GaugeFamily("orderbook_spread", "Best ask minus best bid in ticks, per instrument; NaN when either side is empty.",
		perBook(func(r *matching.Runner) float64 {
			if sp, ok := r.Spread(); ok {
				return float64(sp)
			}
			return math.NaN()
		}))
	c.GaugeFamily("orderbook_phase", "Trading phase per instrument: 0 pre-open, 1 open, 2 cancel-only, 3 halted, 4 closed, 5 closing auction.",
		perBook(func(r *matching.Runner) float64 { return float64(phaseCode(r.Phase())) }))

	c.Gauge("obgw_connections", "Established client sockets across both edges.",
		func() float64 { return float64(s.connCount()) })
	// The single most important number on this page, and the last one to be added.
	//
	// The publisher drops its OLDEST batches when its queue overflows, which is the
	// right call — blocking would stop the venue and growing without limit would end
	// it — but a dropped batch is not a delayed message, it is a lost one. Every
	// consumer downstream derives from that stream: the session index that lets a
	// client name its own order by ClOrdID, and the execution reports for fills
	// against it. So one dropped batch orphans an order in the book that no client
	// can ever cancel, and silently discards the fills against it.
	//
	// This is non-zero on any run that saturates, and until this gauge existed there
	// was nothing anywhere that said so.
	c.Gauge("obgw_publisher_dropped_total", "Event batches the publisher discarded under backpressure. Each one loses execution reports and orphans orders in the book. Alert on any increase.",
		func() float64 { return float64(s.pub.Dropped()) })
	// Goroutines and heap are here because the soak test is where this venue's real
	// unknowns are: a leak shows up over hours, not in a benchmark, and it shows up
	// here first.
	c.Gauge("obgw_goroutines", "Live goroutines; a gateway is one per connection plus a fixed set, so this should track obgw_connections.",
		func() float64 { return float64(runtime.NumGoroutine()) })
	c.Gauge("obgw_heap_bytes", "Live heap in bytes, read without stopping the world.",
		heapBytes)
	// File descriptors are the third leak that only appears over hours, and the one
	// that ends a venue's day outright: at the limit, accept fails and no new
	// participant can connect while the ones already on stay perfectly healthy — a
	// failure that looks like nothing at all from inside the process.
	c.Gauge("obgw_open_files", "Open file descriptors; -1 where the platform does not expose them.",
		openFiles)
}

// phaseCode maps a trading phase to a number, because a Prometheus gauge is a number
// and this collector has no label support for gauges. The mapping is in the HELP
// string so an operator does not have to find this function.
func phaseCode(p matching.EngineState) int {
	switch p {
	case matching.StatePreOpen:
		return 0
	case matching.StateOpen:
		return 1
	case matching.StateCancelOnly:
		return 2
	case matching.StateHalted:
		return 3
	case matching.StateClosed:
		return 4
	case matching.StateClosingAuction:
		return 5
	}
	return -1
}

func (s *Server) connCount() int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return len(s.conns)
}

// heapBytes reads live heap through runtime/metrics rather than runtime.ReadMemStats.
// ReadMemStats stops the world; at a fifteen-second scrape that is survivable and it
// is still a pause this process has no reason to take, and the whole point of these
// two gauges is to be safe to leave on during a multi-day soak.
var heapSample = []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}

var heapMu sync.Mutex

func heapBytes() float64 {
	heapMu.Lock()
	defer heapMu.Unlock()
	metrics.Read(heapSample)
	if heapSample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return float64(heapSample[0].Value.Uint64())
}

// fdDir is where this platform lists the process's descriptors: /proc/self/fd on
// Linux, /dev/fd on Darwin and the BSDs. Resolved once, because the answer cannot
// change while the process runs.
var fdDir = func() string {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	return ""
}()

// openFiles counts descriptors, returning -1 rather than 0 where it cannot. Zero is a
// plausible reading and this is never plausibly zero, so reporting it would turn "I
// could not tell you" into "everything is fine".
func openFiles() float64 {
	if fdDir == "" {
		return -1
	}
	f, err := os.Open(fdDir)
	if err != nil {
		return -1
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return -1
	}
	// Minus one: the handle opened to do the counting is in its own listing.
	return float64(len(names) - 1)
}

// applyLatency is the histogram the session read loop feeds.
//
// It measures the server's handling of one inbound client message, from decode to
// dispatch — which is admission, not matching. It includes queue wait, and for the
// messages that wait on the engine by design (Reduce holds until the stream reaches
// the order it names) it includes that wait too, because that is what the client
// experienced.
//
// What it is not is end-to-end order-to-ack latency. That has a socket at each end
// and can only be measured by a client; see the soak harness.
const applyLatencyMetric = "obgw_message_apply_latency_ns"

func (s *Server) observeApply(start time.Time) {
	s.applyHist.Observe(time.Since(start))
}
