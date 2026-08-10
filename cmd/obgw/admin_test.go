package main

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// adminServer starts a server with the admin edge bound on an ephemeral port.
func adminServer(t *testing.T) *Server {
	t.Helper()
	srv := mustServer(t, Config{
		Addr:          "127.0.0.1:0",
		AdminAddr:     "127.0.0.1:0",
		Symbol:        "X",
		Incarnation:   "INC0000001",
		Accounts:      map[string]string{"alice": "pw1", "bob": "pw2"},
		OutboundDepth: 64,
		StreamRing:    4096,
		RatePerSec:    1e6,
		Burst:         1e6,
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)
	return srv
}

func adminGet(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()
	if srv.AdminAddr() == nil {
		t.Fatal("admin listener not bound")
	}
	resp, err := http.Get("http://" + srv.AdminAddr().String() + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, string(body)
}

// metricValue reads a metric by name, with or without labels. The price gauges
// carry a symbol label (one series per book) and the countable ones do not, so a
// helper that only understood bare names would quietly stop finding half of them.
func metricValue(t *testing.T, body, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(line, name)
		if !ok || rest == "" {
			continue
		}
		switch rest[0] {
		case ' ':
			// bare series
		case '{':
			end := strings.IndexByte(rest, '}')
			if end < 0 {
				continue
			}
			rest = rest[end+1:]
		default:
			continue // a longer metric name that merely starts with this one
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return v
	}
	t.Fatalf("metric %q not present in:\n%s", name, body)
	return 0
}

// TestMetricsReflectRealOrderFlow — the numbers have to come from the engine's event
// stream, not from the gateway's belief about what it sent, or the two can disagree
// at exactly the moment an operator is trying to work out which to trust.
func TestMetricsReflectRealOrderFlow(t *testing.T) {
	srv := adminServer(t)

	maker := dial(t, srv)
	maker.mustLogin("alice", "pw1")
	maker.enter("m1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := maker.awaitType(t, wire.MsgAccepted, 2*time.Second); !ok {
		t.Fatal("maker not accepted")
	}

	taker := dial(t, srv)
	taker.mustLogin("bob", "pw2")
	taker.enter("t1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 4)
	if _, ok := taker.awaitType(t, wire.MsgExecuted, 2*time.Second); !ok {
		t.Fatal("no fill")
	}

	_, body := adminGet(t, srv, "/metrics")
	if got := metricValue(t, body, "orderbook_trades_total"); got != 1 {
		t.Errorf("trades = %v, want 1", got)
	}
	if got := metricValue(t, body, "orderbook_traded_lots_total"); got != 4 {
		t.Errorf("traded lots = %v, want 4", got)
	}
	if got := metricValue(t, body, "orderbook_resting_orders"); got != 1 {
		t.Errorf("resting orders = %v, want the maker's remainder", got)
	}
	if got := metricValue(t, body, "orderbook_best_ask"); got != 100 {
		t.Errorf("best ask = %v, want 100", got)
	}
	if got := metricValue(t, body, "orderbook_last_event_sequence"); got == 0 {
		t.Error("event sequence never advanced")
	}
	if got := metricValue(t, body, "obgw_connections"); got < 2 {
		t.Errorf("connections = %v, want at least the two clients", got)
	}
	if got := metricValue(t, body, "obgw_message_apply_latency_ns_count"); got < 2 {
		t.Errorf("apply latency observations = %v, want one per inbound message", got)
	}
}

// TestEmptySideIsNaNNotZero — zero is a price. A monitoring system cannot tell a
// missing bid from a bid at zero, and averaging the latter into a dashboard is how a
// venue convinces itself it has liquidity it does not have.
func TestEmptySideIsNaNNotZero(t *testing.T) {
	srv := adminServer(t)
	_, body := adminGet(t, srv, "/metrics")
	for _, name := range []string{"orderbook_best_bid", "orderbook_best_ask", "orderbook_spread"} {
		// The price gauges are labelled by instrument, so the series is
		// name{symbol="X"} rather than a bare name — matching on the bare prefix
		// would have quietly stopped checking anything.
		if !strings.Contains(body, name+`{symbol="X"} NaN`) {
			t.Errorf("%s is not NaN on an empty book:\n%s", name, body)
		}
	}
}

// TestScrapeDoesNotEnqueue — a scrape that went through the command queue would
// answer promptly while the venue was healthy and hang exactly when the matcher
// stalled, losing the reading at the only moment anyone wanted it.
func TestScrapeDoesNotEnqueue(t *testing.T) {
	srv := adminServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 99, 5)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 2*time.Second); !ok {
		t.Fatal("not accepted")
	}

	before := srv.metrics.Snapshot()
	for i := 0; i < 20; i++ {
		if code, _ := adminGet(t, srv, "/metrics"); code != http.StatusOK {
			t.Fatalf("scrape %d: status %d", i, code)
		}
	}
	if after := srv.metrics.Snapshot(); after.EventsSeen != before.EventsSeen {
		t.Errorf("scraping produced %d engine events; a scrape must not touch the queue",
			after.EventsSeen-before.EventsSeen)
	}
}

// TestHealthDoesNotProbeTheMatcher — a failed liveness check means "restart me", and
// restarting a venue that is holding a book because a probe was slow is worse than
// whatever the probe was reacting to.
func TestHealthAndReadinessOnAQuietVenue(t *testing.T) {
	srv := adminServer(t)

	if code, body := adminGet(t, srv, "/healthz"); code != http.StatusOK || !strings.Contains(body, "ok") {
		t.Errorf("healthz = %d %q, want 200 ok", code, body)
	}
	// An idle venue does not advance its sequence either. Calling that a stall would
	// take a healthy node out of rotation every quiet minute.
	srv.admin.mu.Lock()
	srv.admin.lastMoved = time.Now().Add(-time.Hour)
	srv.admin.mu.Unlock()
	if code, body := adminGet(t, srv, "/readyz"); code != http.StatusOK {
		t.Errorf("readyz = %d %q; an idle venue with an empty queue is ready", code, body)
	}
}

// TestReadinessFailsWhenTheMatcherIsStuck — the sequence standing still with work
// waiting is the only signal there is: a stalled matcher reads zero in every rate
// metric, which is exactly what a quiet market reads.
func TestReadinessFailsWhenTheMatcherIsStuck(t *testing.T) {
	srv := adminServer(t)

	// Simulate the two conditions rather than wedging the real matcher: commands
	// waiting, and a sequence that has not moved. Wedging it for real would mean
	// leaving a goroutine blocked past the end of the test.
	srv.admin.mu.Lock()
	srv.admin.lastMoved = time.Now().Add(-2 * stallWindow)
	srv.admin.mu.Unlock()

	ready, why := srv.readinessWith(1, srv.runner.QueueCap())
	if ready {
		t.Errorf("ready with a stuck matcher: %q", why)
	}
	if !strings.Contains(why, "stalled") {
		t.Errorf("reason = %q, want it to name the stall", why)
	}
}

// TestReadinessFailsWhenTheQueueIsBacking — the point of a high-water mark below
// capacity: by the time the queue is full, clients are already being refused, and a
// signal that only fires after the damage is a status light, not a control.
func TestReadinessFailsWhenTheQueueIsBacking(t *testing.T) {
	srv := adminServer(t)
	capacity := srv.runner.QueueCap()

	ready, why := srv.readinessWith(int(float64(capacity)*0.9), capacity)
	if ready {
		t.Errorf("ready at 90%% queue occupancy: %q", why)
	}
	if !strings.Contains(why, "behind") {
		t.Errorf("reason = %q, want it to name the backlog", why)
	}

	if ready, why := srv.readinessWith(int(float64(capacity)*0.1), capacity); !ready {
		t.Errorf("not ready at 10%% queue occupancy: %q", why)
	}
}

// TestAdminSurvivesShutdown — the admin goroutine sits in the same WaitGroup as the
// connection handlers, and http.Server.Serve does not return until Shutdown is
// called. Getting that order wrong deadlocks Close, which would only show up when a
// deployment tried to stop.
func TestAdminShutdownDoesNotDeadlock(t *testing.T) {
	srv := adminServer(t)
	if _, body := adminGet(t, srv, "/metrics"); body == "" {
		t.Fatal("empty scrape")
	}
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return; the admin server is holding the WaitGroup")
	}
}

// TestNoAdminAddrMeansNoListener — the venue must trade without it, because a
// misconfigured monitoring port is not a reason to refuse a market.
func TestNoAdminAddrMeansNoListener(t *testing.T) {
	srv := testServer(t)
	if srv.AdminAddr() != nil {
		t.Error("admin listener bound without an address configured")
	}
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 99, 5)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 2*time.Second); !ok {
		t.Fatal("venue did not trade without an admin listener")
	}
}
