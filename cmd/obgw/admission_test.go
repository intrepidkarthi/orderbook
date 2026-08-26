package main

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
)

// TestMaxConnsRefusesPastTheCeiling. Everything after an accept — a goroutine, a
// wire.MaxPayload read buffer, a descriptor, a tracking entry — is spent on a peer
// that has not proved it is anybody, and the only other bound on an unauthenticated
// connection was a ten-second login timeout that a reconnecting client outruns. The
// accept loop admitted every socket unconditionally.
func TestMaxConnsRefusesPastTheCeiling(t *testing.T) {
	const cap = 3
	srv := mustServer(t, Config{
		Addr:          "127.0.0.1:0",
		AdminAddr:     "127.0.0.1:0",
		Symbol:        "X",
		Incarnation:   "INC0000001",
		Accounts:      map[string]string{"alice": "pw1"},
		MaxConns:      cap,
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

	// Fill the ceiling with peers that say nothing. Connect-and-say-nothing is the
	// cheapest exhaustion attack there is, and it is the one the cap is for.
	held := make([]net.Conn, 0, cap)
	for i := 0; i < cap; i++ {
		c, err := net.Dial("tcp", srv.Addr().String())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		held = append(held, c)
	}
	waitForConns(t, srv, cap)

	// The next one is accepted by the kernel and closed by the venue at once, so the
	// client sees EOF rather than a hang.
	over, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial over the cap: %v", err)
	}
	defer over.Close()
	_ = over.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := over.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("a connection past the cap read %v, want EOF from an immediate close", err)
	}

	_, body := adminGet(t, srv, "/metrics")
	if got := metricValue(t, body, "obgw_connections_refused_total"); got < 1 {
		t.Errorf("obgw_connections_refused_total = %v, want at least 1", got)
	}

	// Room reappears when a held socket goes, and the cap is not a permanent close.
	_ = held[0].Close()
	waitForConns(t, srv, cap-1)
	again := dial(t, srv)
	again.mustLogin("alice", "pw1")
}

// TestMaxConnsCanBeDisabled keeps the pre-existing behaviour reachable rather than
// only reachable by accident.
func TestMaxConnsCanBeDisabled(t *testing.T) {
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC0000001",
		Accounts: map[string]string{"alice": "pw1"}, MaxConns: -1,
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
	})
	if srv.cfg.MaxConns != -1 {
		t.Fatalf("MaxConns = %d after defaults, want the -1 that disables it", srv.cfg.MaxConns)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)
	for i := 0; i < 8; i++ {
		c := dial(t, srv)
		c.mustLogin("alice", "pw1")
	}
}

// TestMaxConnsDefaultsWithoutBeingAsked — a venue that sets nothing still gets a
// ceiling, because the failure it prevents (running the process out of descriptors)
// is worse than the one it causes.
func TestMaxConnsDefaultsWithoutBeingAsked(t *testing.T) {
	srv := testServer(t)
	if srv.cfg.MaxConns != defaultMaxConns {
		t.Errorf("MaxConns = %d with none configured, want %d", srv.cfg.MaxConns, defaultMaxConns)
	}
}

// waitForConns waits until the venue is holding exactly n sockets.
func waitForConns(t *testing.T, srv *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		srv.connMu.Lock()
		got := len(srv.conns)
		srv.connMu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("venue holds %d connections, want %d", got, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestWaitForStreamWakesOnTheNotificationRatherThanATimer. The wait used to re-read
// an atomic every millisecond, which is correct and costs waiters x time instead of
// costing events. The property that has to survive the change to a notification is
// the one the poll had for free: no missed wake-up, whichever order the store and
// the wait land in.
func TestWaitForStreamWakesOnTheNotificationRatherThanATimer(t *testing.T) {
	srv := testServer(t)
	sess := &session{srv: srv, out: make(chan []byte, 4), closed: make(chan struct{})}

	// Advancing first, then waiting: the counter is already past the target, so the
	// wait returns without ever selecting.
	sess.noteEmitted(7)
	done := make(chan struct{})
	go func() { sess.waitForStream(7, 3*time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a wait for a sequence already emitted did not return")
	}

	// Waiting first, then advancing: the waiter is parked on the channel and the
	// store's wake has to reach it.
	done = make(chan struct{})
	go func() { sess.waitForStream(9, 3*time.Second); close(done) }()
	time.Sleep(20 * time.Millisecond) // let it park
	sess.noteEmitted(9)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a parked waiter was not woken by noteEmitted")
	}

	// And it is still bounded: a sequence that never arrives releases the waiter on
	// the deadline rather than never.
	start := time.Now()
	sess.waitForStream(1_000_000, 60*time.Millisecond)
	if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
		t.Errorf("a wait for a sequence that never came took %v, want about the 60ms timeout", d)
	}

	// A closed session releases waiters too.
	done = make(chan struct{})
	go func() { sess.waitForStream(2_000_000, 30*time.Second); close(done) }()
	time.Sleep(20 * time.Millisecond)
	sess.close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the session did not release its waiter")
	}
}

// TestOneQueryInFlightPerSession. MsgQuery was the only client command with no
// admission control at all: it spawned a goroutine per message, each enqueuing on
// the same matching-engine command queue that carries order flow and then waiting on
// the answer. gate.Allow could not cover it — that gate rates an ORDER against a
// per-book bucket and a query is not an order — so the bound is on concurrency.
func TestOneQueryInFlightPerSession(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("q1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 5)

	// A burst down one connection. Whatever the interleaving, a session may never
	// have two Query answers in flight, so any message that is not part of a query
	// answer must be a throttle refusal — and by the end every query that started has
	// terminated with a QueryEnd.
	const burst = 40
	for i := 0; i < burst; i++ {
		b, err := wire.EncodeQuery(nil, wire.Query{Version: wire.Version})
		if err != nil {
			t.Fatalf("EncodeQuery: %v", err)
		}
		if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
			t.Fatalf("send query %d: %v", i, err)
		}
	}

	var ends, throttled, other int
	deadline := time.Now().Add(5 * time.Second)
	for ends+throttled < burst && time.Now().Before(deadline) {
		_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
		pkt, err := wire.ReadPacket(c.conn, c.buf)
		if err != nil {
			break
		}
		if pkt.Type != wire.PacketSequencedData || len(pkt.Payload) < 2 {
			continue
		}
		switch pkt.Payload[0] {
		case wire.MsgQueryEnd:
			ends++
		case wire.MsgCmdReject:
			rej, err := wire.DecodeCmdReject(pkt.Payload)
			if err != nil {
				t.Fatalf("DecodeCmdReject: %v", err)
			}
			if rej.Reason != orderentry.ReasonThrottled {
				t.Fatalf("a refused query gave reason %d, want ReasonThrottled", rej.Reason)
			}
			throttled++
		case wire.MsgOpenOrder:
			// part of an answer
		default:
			other++
		}
	}
	if ends+throttled != burst {
		t.Errorf("%d queries answered and %d refused, want %d accounted for in total", ends, throttled, burst)
	}
	if throttled == 0 {
		t.Error("a 40-deep query burst was never throttled, so the in-flight cap did not hold")
	}
	if ends == 0 {
		t.Error("no query was answered at all, so the cap refuses everything")
	}
}
