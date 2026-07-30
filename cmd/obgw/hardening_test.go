package main

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
)

// TestWrongSymbolIsRefused — the Symbol field used to be decoded and thrown away,
// so an order naming any instrument was booked into this gateway's.
func TestWrongSymbolIsRefused(t *testing.T) {
	srv := testServer(t) // serves "X"
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	b, err := wire.EncodeEnter(nil, wire.Enter{
		Version: wire.Version, ClOrdID: "a1", Symbol: "SOMETHING-ELSE",
		Side: wire.SideBuy, Type: wire.TypeLimit, TIF: wire.TIFGoodTillCancel,
		Price: 100, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("EncodeEnter: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := c.await(t, wire.CmdRejectLen, 3*time.Second); !ok {
		t.Fatal("an order for another instrument was not refused")
	}
	if n := srv.runner.OrderCount(); n != 0 {
		t.Errorf("book holds %d orders — a foreign symbol was booked here", n)
	}
}

// TestIdleConnectionIsReaped — a peer that connects and says nothing must not
// hold a goroutine, a buffer and a stream indefinitely.
func TestIdleConnectionIsReaped(t *testing.T) {
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts: map[string]string{"alice": "pw1"},
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// Say nothing at all. The login deadline must close this.
	_ = conn.SetReadDeadline(time.Now().Add(loginTimeout + 5*time.Second))
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err == nil {
		t.Error("a silent peer was neither served nor disconnected")
	}
}

// TestServerSendsHeartbeats — the packet type was declared and never sent, which
// left a client unable to tell a quiet venue from a dead one.
func TestServerSendsHeartbeats(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	deadline := time.Now().Add(heartbeatInterval * 3)
	for time.Now().Before(deadline) {
		_ = c.conn.SetReadDeadline(deadline)
		pkt, err := wire.ReadPacket(c.conn, c.buf)
		if err != nil {
			break
		}
		if pkt.Type == wire.PacketServerHeartbt {
			return
		}
	}
	t.Error("no server heartbeat within three intervals")
}

// TestMessageTypeNotLength is the regression for dispatching on payload length. A
// payload the right length but the wrong type must be refused, not misread.
func TestMessageTypeNotLength(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	// A well-formed Enter, with only its type byte changed to Cancel's.
	b, err := wire.EncodeEnter(nil, wire.Enter{
		Version: wire.Version, ClOrdID: "a1", Symbol: "X",
		Side: wire.SideBuy, Type: wire.TypeLimit, TIF: wire.TIFGoodTillCancel,
		Price: 100, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("EncodeEnter: %v", err)
	}
	b[0] = wire.MsgCancel

	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := c.await(t, wire.CmdRejectLen, 3*time.Second); !ok {
		t.Fatal("a mistyped payload was not refused")
	}
	if n := srv.runner.OrderCount(); n != 0 {
		t.Errorf("book holds %d orders — the mistyped payload was executed anyway", n)
	}
}

// TestWrongProtocolVersionIsRefused — a client built against a different layout
// must be told, not silently misread.
func TestWrongProtocolVersionIsRefused(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	b, err := wire.EncodeEnter(nil, wire.Enter{
		Version: wire.Version, ClOrdID: "a1", Symbol: "X",
		Side: wire.SideBuy, Type: wire.TypeLimit, TIF: wire.TIFGoodTillCancel,
		Price: 100, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("EncodeEnter: %v", err)
	}
	b[1] = 99 // a version this server does not speak

	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := c.await(t, wire.CmdRejectLen, 3*time.Second); !ok {
		t.Fatal("an unknown protocol version was not refused")
	}
}

// TestDurabilityRoundTrip — the reference server shipped with no WAL at all, so
// the one artifact showing people how to use the library demonstrated running
// without the durability the library provides.
func TestDurabilityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "obgw.wal")
	snapPath := filepath.Join(dir, "obgw.snap")

	cfg := Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts: map[string]string{"alice": "pw1"},
		WALPath:  walPath, SnapshotPath: snapPath,
	}

	srv := mustServer(t, cfg)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	for i := 0; i < 5; i++ {
		c.enter(string(rune('a'+i)), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, int64(100-i), 1)
		if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("order %d not accepted", i)
		}
	}
	want := srv.runner.OrderCount()
	if want != 5 {
		t.Fatalf("book holds %d, want 5", want)
	}
	srv.Close() // syncs and closes the log

	// A fresh server over the same files must come back with the same book.
	cfg.Addr = "127.0.0.1:0"
	revived := mustServer(t, cfg)
	defer revived.Close()
	if got := revived.runner.OrderCount(); got != want {
		t.Errorf("after restart the book holds %d orders, want %d", got, want)
	}
}

// TestReasonCodesAgreeAcrossPackages — the vocabulary is defined twice, once in
// the wire package and once in the supported one. Drift means clients are handed
// a reason that means something else.
func TestReasonCodesAgreeAcrossPackages(t *testing.T) {
	pairs := []struct {
		name     string
		wireVal  uint16
		entryVal uint16
	}{
		{"None", wire.ReasonNone, orderentry.ReasonNone},
		{"Other", wire.ReasonOther, orderentry.ReasonOther},
		{"UnknownOrder", wire.ReasonUnknownOrder, orderentry.ReasonUnknownOrder},
		{"DuplicateClOrd", wire.ReasonDuplicateClOrd, orderentry.ReasonDuplicateClOrd},
		{"TooSmall", wire.ReasonTooSmall, orderentry.ReasonTooSmall},
		{"TooLarge", wire.ReasonTooLarge, orderentry.ReasonTooLarge},
		{"PriceBand", wire.ReasonPriceBand, orderentry.ReasonPriceBand},
		{"SelfTrade", wire.ReasonSelfTrade, orderentry.ReasonSelfTrade},
		{"PostOnlyCross", wire.ReasonPostOnlyCross, orderentry.ReasonPostOnlyCross},
		{"FOKCannotFill", wire.ReasonFOKCannotFill, orderentry.ReasonFOKCannotFill},
		{"Halted", wire.ReasonHalted, orderentry.ReasonHalted},
		{"Throttled", wire.ReasonThrottled, orderentry.ReasonThrottled},
		{"Overloaded", wire.ReasonOverloaded, orderentry.ReasonOverloaded},
		{"NotAuthorised", wire.ReasonNotAuthorised, orderentry.ReasonNotAuthorised},
		{"Malformed", wire.ReasonMalformed, orderentry.ReasonMalformed},
		{"ShuttingDown", wire.ReasonShuttingDown, orderentry.ReasonShuttingDown},
	}
	for _, p := range pairs {
		if p.wireVal != p.entryVal {
			t.Errorf("Reason%s: wire=%d orderentry=%d — the two vocabularies have drifted",
				p.name, p.wireVal, p.entryVal)
		}
	}
}

// TestOpenOrderReport is the in-band recovery path. Resume can legitimately
// fail — an evicted cursor, or a restarted venue — and without this a client
// refused at login has no way back to a correct picture except building a second
// integration.
func TestOpenOrderReport(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	// Three orders; one is then cancelled, so the report must show two.
	for _, id := range []string{"a1", "a2", "a3"} {
		c.enter(id, wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 5)
		if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("%s not accepted", id)
		}
	}
	c.cancel("a2")
	if _, ok := c.await(t, wire.CanceledLen, 3*time.Second); !ok {
		t.Fatal("cancel not confirmed")
	}

	q, err := wire.EncodeQuery(nil, wire.Query{Version: wire.Version})
	if err != nil {
		t.Fatalf("EncodeQuery: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, q); err != nil {
		t.Fatalf("send query: %v", err)
	}

	seen := map[string]int64{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("query never terminated")
		}
		_ = c.conn.SetReadDeadline(deadline)
		pkt, err := wire.ReadPacket(c.conn, c.buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if pkt.Type != wire.PacketSequencedData || len(pkt.Payload) < 2 {
			continue
		}
		switch pkt.Payload[0] {
		case wire.MsgOpenOrder:
			oo, err := wire.DecodeOpenOrder(pkt.Payload)
			if err != nil {
				t.Fatalf("DecodeOpenOrder: %v", err)
			}
			seen[oo.ClOrdID] = oo.LeavesQty
		case wire.MsgQueryEnd:
			end, err := wire.DecodeQueryEnd(pkt.Payload)
			if err != nil {
				t.Fatalf("DecodeQueryEnd: %v", err)
			}
			if int(end.Count) != len(seen) {
				t.Errorf("terminator says %d orders, %d arrived — a truncated report would look complete", end.Count, len(seen))
			}
			if end.Seq == 0 {
				t.Error("terminator carries no stream position, so the client cannot tell what to apply on top")
			}
			if len(seen) != 2 {
				t.Errorf("report lists %v, want a1 and a3 (a2 was cancelled)", seen)
			}
			if _, gone := seen["a2"]; gone {
				t.Error("a cancelled order appeared in the open-order report")
			}
			if seen["a1"] != 5 {
				t.Errorf("a1 leaves %d, want 5", seen["a1"])
			}
			return
		}
	}
}

// TestOpenOrderReportIsEmptyNotSilent — "you have nothing open" must be an
// answer, not an absence of one.
func TestOpenOrderReportWhenNothingOpen(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	q, err := wire.EncodeQuery(nil, wire.Query{Version: wire.Version})
	if err != nil {
		t.Fatalf("EncodeQuery: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, q); err != nil {
		t.Fatalf("send query: %v", err)
	}
	raw, ok := c.await(t, wire.QueryEndLen, 3*time.Second)
	if !ok {
		t.Fatal("no terminator for an empty report")
	}
	end, err := wire.DecodeQueryEnd(raw)
	if err != nil {
		t.Fatalf("DecodeQueryEnd: %v", err)
	}
	if end.Count != 0 {
		t.Errorf("count = %d, want 0", end.Count)
	}
}

// TestOpenOrderReportIsPerAccount — the report is the session's account, never
// the venue's book.
func TestOpenOrderReportIsPerAccount(t *testing.T) {
	srv := testServer(t)

	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("b1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 90, 5)
	if _, ok := bob.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("bob's order not accepted")
	}

	alice := dial(t, srv)
	alice.mustLogin("alice", "pw1")
	q, err := wire.EncodeQuery(nil, wire.Query{Version: wire.Version})
	if err != nil {
		t.Fatalf("EncodeQuery: %v", err)
	}
	if err := wire.WritePacket(alice.conn, wire.PacketUnsequenced, q); err != nil {
		t.Fatalf("send query: %v", err)
	}
	raw, ok := alice.await(t, wire.QueryEndLen, 3*time.Second)
	if !ok {
		t.Fatal("no terminator")
	}
	end, err := wire.DecodeQueryEnd(raw)
	if err != nil {
		t.Fatalf("DecodeQueryEnd: %v", err)
	}
	if end.Count != 0 {
		t.Errorf("alice sees %d orders; bob's book leaked into her report", end.Count)
	}
}
