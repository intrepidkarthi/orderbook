package main

import (
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

func enc(t *testing.T, f func() ([]byte, error)) []byte {
	t.Helper()
	b, err := f()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

// TestFeedBuildsTheBookFromTheWire drives the subscriber's apply loop with
// wire-synthesized payloads — the same bytes obgw's md server emits — and
// asserts the view: snapshot levels land, an absolute delta replaces, a
// zero-qty delta removes, a trade prints and moves last, a status changes the
// venue state, and the sequence tracks the stream.
func TestFeedBuildsTheBookFromTheWire(t *testing.T) {
	f := newFeed("", "BTC-USD")
	v := wire.Version

	f.apply(enc(t, func() ([]byte, error) {
		return wire.EncodeMDLevel(nil, wire.MDLevel{Version: v, Side: wire.SideBuy, Price: 9998, Qty: 500})
	}))
	f.apply(enc(t, func() ([]byte, error) {
		return wire.EncodeMDLevel(nil, wire.MDLevel{Version: v, Side: wire.SideSell, Price: 10002, Qty: 300})
	}))
	f.apply(enc(t, func() ([]byte, error) {
		return wire.EncodeMDSnapshotEnd(nil, wire.MDSnapshotEnd{Version: v, Count: 2, Seq: 40, LastTradePrice: 10000})
	}))
	f.apply(enc(t, func() ([]byte, error) {
		return wire.EncodeMDDelta(nil, wire.MDDelta{Version: v, Seq: 41, Side: wire.SideBuy, Price: 9998, Qty: 900})
	}))
	f.apply(enc(t, func() ([]byte, error) {
		return wire.EncodeMDDelta(nil, wire.MDDelta{Version: v, Seq: 42, Side: wire.SideSell, Price: 10002, Qty: 0})
	}))
	f.apply(enc(t, func() ([]byte, error) {
		return wire.EncodeMDTrade(nil, wire.MDTrade{Version: v, Seq: 43, Price: 9998, Qty: 250, Aggressor: wire.SideSell})
	}))
	f.apply(enc(t, func() ([]byte, error) {
		return wire.EncodeMDStatus(nil, wire.MDStatus{Version: v, Seq: 44, State: wire.MDStateHalted})
	}))

	s := f.snapshot(10)
	if s.Seq != 44 {
		t.Errorf("seq = %d, want 44", s.Seq)
	}
	if s.State != "halted" {
		t.Errorf("state = %q, want halted", s.State)
	}
	if len(s.Bids) != 1 || s.Bids[0].Price != 9998 || s.Bids[0].Qty != 900 {
		t.Errorf("bids = %+v, want the absolute-replaced level 9998x900", s.Bids)
	}
	if len(s.Asks) != 0 {
		t.Errorf("asks = %+v, want the zero-qty delta to have removed the level", s.Asks)
	}
	if s.LastTrade != 9998 {
		t.Errorf("last trade = %d, want 9998", s.LastTrade)
	}
	if len(s.Trades) != 1 || s.Trades[0].Aggressor != "sell" || s.Trades[0].Qty != 250 {
		t.Errorf("trades = %+v, want one sell-aggressor print of 250", s.Trades)
	}
}

// TestFeedSubscribesAndAppliesOverASocket — a fake venue accepts the dial,
// requires a well-formed fresh subscription, and streams a snapshot plus a
// delta; the feed must build the same book it would from obgw.
func TestFeedSubscribesAndAppliesOverASocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	v := wire.Version

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, wire.MaxPayload)
		pkt, err := wire.ReadPacket(conn, buf)
		if err != nil {
			return
		}
		sub, err := wire.DecodeMDSubscribe(pkt.Payload)
		if err != nil || sub.Seq != 0 {
			return // a dashboard always subscribes fresh
		}
		// As strict as the real venue. obgw refuses a subscription that does not
		// name an instrument it serves, and this double did not — which is why the
		// dashboard could stop sending a symbol at wire v4 and still pass every
		// test here while being unable to connect to an actual gateway. A test
		// double more permissive than the thing it stands in for is a test that
		// certifies the wrong system.
		if sub.Symbol != "BTC-USD" {
			t.Errorf("subscribe named symbol %q, want BTC-USD — the real venue would refuse this", sub.Symbol)
			return
		}
		send := func(b []byte) { _ = wire.WritePacket(conn, wire.PacketSequencedData, b) }
		b, _ := wire.EncodeMDLevel(nil, wire.MDLevel{Version: v, Side: wire.SideBuy, Price: 9999, Qty: 700})
		send(b)
		b, _ = wire.EncodeMDSnapshotEnd(nil, wire.MDSnapshotEnd{Version: v, Count: 1, Seq: 7, LastTradePrice: 10000})
		send(b)
		b, _ = wire.EncodeMDDelta(nil, wire.MDDelta{Version: v, Seq: 8, Side: wire.SideSell, Price: 10001, Qty: 400})
		send(b)
		time.Sleep(500 * time.Millisecond)
	}()

	f := newFeed(ln.Addr().String(), "BTC-USD")
	go func() { _ = f.session() }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		s := f.snapshot(10)
		if s.Seq == 8 {
			if len(s.Bids) != 1 || s.Bids[0].Price != 9999 || len(s.Asks) != 1 || s.Asks[0].Price != 10001 {
				t.Fatalf("book = bids %+v asks %+v, want 9999x700 / 10001x400", s.Bids, s.Asks)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("feed never reached seq 8 (at %d)", s.Seq)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMetricsParserReadsTheExpositionFormat — comments skipped, NaN preserved
// as a value (an empty book reports NaN, not zero, because zero is a price),
// junk lines ignored rather than fatal.
func TestMetricsParserReadsTheExpositionFormat(t *testing.T) {
	text := `# HELP orderbook_queue_depth Commands buffered.
# TYPE orderbook_queue_depth gauge
orderbook_queue_depth 12
orderbook_queue_capacity 1024
orderbook_best_bid NaN
not a metric line
orderbook_spread 3
`
	vals, err := parseMetrics(strings.NewReader(text), "BTC-USD")
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if vals["orderbook_queue_depth"] != 12 || vals["orderbook_queue_capacity"] != 1024 || vals["orderbook_spread"] != 3 {
		t.Errorf("values = %+v", vals)
	}
	if v, ok := vals["orderbook_best_bid"]; !ok || v == v { // NaN != NaN
		t.Errorf("best_bid = %v, want NaN preserved by the parser", v)
	}
	// And the JSON boundary drops it, because encoding/json refuses NaN and
	// the page shows "—" — which is what NaN meant.
	if _, ok := jsonSafe(vals)["orderbook_best_bid"]; ok {
		t.Error("jsonSafe let NaN through to the encoder")
	}
}

// TestSSEDeliversAndShedsSlowClients — a connected client gets frames; a
// client that stops draining is cut rather than buffered without bound, the
// same shed-not-block rule the venue applies to its own subscribers.
func TestSSEDeliversAndShedsSlowClients(t *testing.T) {
	h := newHub()
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	// Wait for the subscription to register, then broadcast one frame.
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.clients)
		h.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.broadcast([]byte(`{"x":1}`))

	// The retry hint and the data frame arrive in as many reads as the stack
	// feels like; accumulate until the frame shows up.
	buf := make([]byte, 4096)
	got := ""
	for !strings.Contains(got, `data: {"x":1}`) {
		if time.Now().After(deadline) {
			t.Fatalf("stream did not carry the frame:\n%s", got)
		}
		n, err := resp.Body.Read(buf)
		if err != nil {
			t.Fatalf("read: %v (got so far: %q)", err, got)
		}
		got += string(buf[:n])
	}
	if !strings.Contains(got, "retry: 2000") {
		t.Errorf("no retry hint before the first frame:\n%s", got)
	}

	// The shed: a subscriber that never drains. Fill its buffer past capacity
	// and it must be removed and closed, while broadcast never blocks.
	ch := h.subscribe()
	for range clientBuffer + 1 {
		h.broadcast([]byte("frame"))
	}
	select {
	case _, open := <-ch:
		_ = open // drained one buffered frame or saw the close — either is fine
	default:
	}
	h.mu.Lock()
	_, still := h.clients[ch]
	h.mu.Unlock()
	if still {
		t.Error("a client that never drains was kept — broadcast would eventually block on it")
	}
}

// TestMetricsParserSelectsItsOwnInstrument — the venue exposes one price series
// per book, so a dashboard has to pick. Keying on the whole `name{labels}` string
// (which the parser did before v4) loses every labelled gauge, and on a page that
// looks exactly like a venue with no bids.
func TestMetricsParserSelectsItsOwnInstrument(t *testing.T) {
	text := `# HELP orderbook_best_bid Best bid per instrument.
# TYPE orderbook_best_bid gauge
orderbook_best_bid{symbol="BTC-USD"} 30000
orderbook_best_bid{symbol="ETH-USD"} 2000
# HELP orderbook_queue_depth Commands buffered.
# TYPE orderbook_queue_depth gauge
orderbook_queue_depth 7
`
	vals, err := parseMetrics(strings.NewReader(text), "ETH-USD")
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if got := vals["orderbook_best_bid"]; got != 2000 {
		t.Errorf("best bid = %v, want the ETH series (2000) under the bare name", got)
	}
	// A venue-wide series has no labels and belongs to every dashboard.
	if got := vals["orderbook_queue_depth"]; got != 7 {
		t.Errorf("queue depth = %v, want 7 — an unlabelled series was dropped", got)
	}
	// And an instrument this dashboard does not watch contributes nothing.
	if vals2, err := parseMetrics(strings.NewReader(text), "DOGE-USD"); err != nil {
		t.Fatal(err)
	} else if _, present := vals2["orderbook_best_bid"]; present {
		t.Error("a series for another instrument was shown as this one's")
	}
}
