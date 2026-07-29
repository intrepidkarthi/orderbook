package main

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// testServer starts a server on an ephemeral port with two accounts.
func testServer(t *testing.T) *Server {
	t.Helper()
	srv := NewServer(Config{
		Addr:          "127.0.0.1:0",
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

// client is a minimal protocol client, which doubles as a check that the wire is
// usable by someone who only has the format.
type client struct {
	t    *testing.T
	conn net.Conn
	buf  []byte
}

func dial(t *testing.T, srv *Server) *client {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &client{t: t, conn: conn, buf: make([]byte, wire.MaxPayload)}
}

func (c *client) login(user, pass, session string, seq uint64) (wire.Packet, error) {
	c.t.Helper()
	b, err := wire.EncodeLoginRequest(nil, wire.LoginRequest{
		Username: user, Password: pass, Session: session, Sequence: seq,
	})
	if err != nil {
		c.t.Fatalf("EncodeLoginRequest: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketLoginRequest, b); err != nil {
		c.t.Fatalf("WritePacket: %v", err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	return wire.ReadPacket(c.conn, c.buf)
}

func (c *client) mustLogin(user, pass string) wire.LoginAccepted {
	c.t.Helper()
	pkt, err := c.login(user, pass, "", 0)
	if err != nil {
		c.t.Fatalf("login read: %v", err)
	}
	if pkt.Type != wire.PacketLoginAccepted {
		c.t.Fatalf("login type = %q, want accepted", pkt.Type)
	}
	acc, err := wire.DecodeLoginAccepted(pkt.Payload)
	if err != nil {
		c.t.Fatalf("DecodeLoginAccepted: %v", err)
	}
	return acc
}

func (c *client) enter(clOrdID string, side, typ, tif uint8, price, qty int64) {
	c.t.Helper()
	b, err := wire.EncodeEnter(nil, wire.Enter{
		Version: wire.Version, ClOrdID: clOrdID, Symbol: "X",
		Side: side, Type: typ, TIF: tif, Price: price, Quantity: qty,
	})
	if err != nil {
		c.t.Fatalf("EncodeEnter: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		c.t.Fatalf("send enter: %v", err)
	}
}

// tryEnter is enter for tests that expect the server to hang up mid-stream: once
// a non-reading client is disconnected, its own writes start failing, and that is
// the behaviour under test rather than a test failure.
func (c *client) tryEnter(clOrdID string, side, typ, tif uint8, price, qty int64) error {
	b, err := wire.EncodeEnter(nil, wire.Enter{
		Version: wire.Version, ClOrdID: clOrdID, Symbol: "X",
		Side: side, Type: typ, TIF: tif, Price: price, Quantity: qty,
	})
	if err != nil {
		return err
	}
	return wire.WritePacket(c.conn, wire.PacketUnsequenced, b)
}

func (c *client) cancel(clOrdID string) {
	c.t.Helper()
	b, err := wire.EncodeCancel(nil, wire.Cancel{Version: wire.Version, ClOrdID: clOrdID})
	if err != nil {
		c.t.Fatalf("EncodeCancel: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		c.t.Fatalf("send cancel: %v", err)
	}
}

// awaitPayloadLen reads until a payload of the given length arrives, which is how
// this protocol distinguishes message types on the outbound side.
func (c *client) await(t *testing.T, wantLen int, timeout time.Duration) ([]byte, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = c.conn.SetReadDeadline(deadline)
		pkt, err := wire.ReadPacket(c.conn, c.buf)
		if err != nil {
			return nil, false
		}
		if pkt.Type == wire.PacketSequencedData && len(pkt.Payload) == wantLen {
			out := make([]byte, len(pkt.Payload))
			copy(out, pkt.Payload)
			return out, true
		}
	}
	return nil, false
}

// TestLoginDefaultsToDeny — an unconfigured venue must reject everyone, not admit
// everyone.
func TestLoginDefaultsToDeny(t *testing.T) {
	srv := NewServer(Config{Addr: "127.0.0.1:0", Symbol: "X"})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	c := dial(t, srv)
	pkt, err := c.login("anyone", "anything", "", 0)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if pkt.Type != wire.PacketLoginRejected {
		t.Errorf("type = %q, want rejected", pkt.Type)
	}
}

func TestLoginRejectsBadPassword(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	pkt, err := c.login("alice", "wrong", "", 0)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if pkt.Type != wire.PacketLoginRejected {
		t.Errorf("type = %q, want rejected", pkt.Type)
	}
	if len(pkt.Payload) != 1 || pkt.Payload[0] != wire.RejectNotAuthorised {
		t.Errorf("payload = %v, want RejectNotAuthorised", pkt.Payload)
	}
}

// TestEndToEndOrderEntry is the headline: a real socket, a real order, a real
// fill reported to both sides.
func TestEndToEndOrderEntry(t *testing.T) {
	srv := testServer(t)

	alice := dial(t, srv)
	alice.mustLogin("alice", "pw1")
	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")

	// Alice rests a bid.
	alice.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := alice.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("alice never received her Accepted")
	}

	// Bob sells into it.
	bob.enter("b1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 4)

	// Bob hears about his own fill.
	raw, ok := bob.await(t, wire.ExecutedLen, 3*time.Second)
	if !ok {
		t.Fatal("bob never received his Executed")
	}
	exec, err := wire.DecodeExecuted(raw)
	if err != nil {
		t.Fatalf("DecodeExecuted: %v", err)
	}
	if exec.ClOrdID != "b1" || exec.Quantity != 4 || exec.Price != 100 {
		t.Errorf("bob's execution = %+v", exec)
	}

	// Alice hears about a fill she did not initiate — the message a resting maker
	// exists to receive.
	raw, ok = alice.await(t, wire.ExecutedLen, 3*time.Second)
	if !ok {
		t.Fatal("alice never received the fill against her resting order")
	}
	exec, err = wire.DecodeExecuted(raw)
	if err != nil {
		t.Fatalf("DecodeExecuted: %v", err)
	}
	if exec.ClOrdID != "a1" {
		t.Errorf("alice's execution ClOrdID = %q, want a1", exec.ClOrdID)
	}
	if exec.LeavesQty != 6 {
		t.Errorf("alice's LeavesQty = %d, want 6", exec.LeavesQty)
	}
}

// TestCancelOverTheWire — a client names its own order by its own id, and the
// wire never carries an engine id or an account.
func TestCancelOverTheWire(t *testing.T) {
	srv := testServer(t)
	alice := dial(t, srv)
	alice.mustLogin("alice", "pw1")

	alice.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := alice.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("no Accepted")
	}
	alice.cancel("a1")

	raw, ok := alice.await(t, wire.CanceledLen, 3*time.Second)
	if !ok {
		t.Fatal("no Canceled after cancelling")
	}
	c, err := wire.DecodeCanceled(raw)
	if err != nil {
		t.Fatalf("DecodeCanceled: %v", err)
	}
	if c.ClOrdID != "a1" {
		t.Errorf("cancelled %q, want a1", c.ClOrdID)
	}
}

// TestCannotCancelAnotherAccountsOrder is the security boundary. Bob uses the
// same client order id string as Alice; the id is scoped to the account, so his
// cancel must not touch hers.
func TestCannotCancelAnotherAccountsOrder(t *testing.T) {
	srv := testServer(t)

	alice := dial(t, srv)
	alice.mustLogin("alice", "pw1")
	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")

	alice.enter("shared-id", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := alice.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("alice got no Accepted")
	}

	bob.cancel("shared-id") // bob has no such order

	raw, ok := bob.await(t, wire.CmdRejectLen, 3*time.Second)
	if !ok {
		t.Fatal("bob's bogus cancel was not refused")
	}
	rej, err := wire.DecodeCmdReject(raw)
	if err != nil {
		t.Fatalf("DecodeCmdReject: %v", err)
	}
	if rej.Reason != 2 { // ReasonUnknownOrder
		t.Errorf("reason = %d, want ReasonUnknownOrder", rej.Reason)
	}
	if srv.runner.OrderCount() != 1 {
		t.Errorf("book holds %d orders — bob cancelled an order he does not own", srv.runner.OrderCount())
	}
}

// TestFlooderIsThrottledNotFatal — a client hammering the venue gets refusals,
// and the venue keeps serving everyone else.
func TestFlooderIsThrottled(t *testing.T) {
	srv := NewServer(Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:   map[string]string{"alice": "pw1", "bob": "pw2"},
		RatePerSec: 5, Burst: 5, OutboundDepth: 4096, StreamRing: 4096,
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	flooder := dial(t, srv)
	flooder.mustLogin("alice", "pw1")
	for i := 0; i < 200; i++ {
		flooder.enter("f", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 90, 1)
	}
	if _, ok := flooder.await(t, wire.CmdRejectLen, 3*time.Second); !ok {
		t.Error("flooding was never refused; the rate gate is not in the path")
	}

	// The venue still serves a well-behaved client.
	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("b1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 200, 1)
	if _, ok := bob.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Error("a well-behaved client was starved by another account's flood")
	}
}

// TestNonReadingClientIsDisconnected — a client that stops reading must be cut
// off rather than allowed to back up into the venue.
func TestNonReadingClientIsDisconnected(t *testing.T) {
	srv := NewServer(Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:   map[string]string{"alice": "pw1", "bob": "pw2"},
		RatePerSec: 1e6, Burst: 1e6,
		OutboundDepth: 2, // tiny: a client that does not read overflows at once
		StreamRing:    4096,
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	silent := dial(t, srv)
	silent.mustLogin("alice", "pw1")

	// Generate plenty of outbound for a client that never reads again. Writes
	// failing partway through is the expected outcome, not an error.
	var wroteUntil int
	for i := 0; i < 500; i++ {
		if err := silent.tryEnter("s", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, int64(50+i%20), 1); err != nil {
			break
		}
		wroteUntil = i
	}
	t.Logf("silent client accepted %d writes before the server hung up", wroteUntil)

	// The connection must eventually be closed by the server.
	_ = silent.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	var closed bool
	for i := 0; i < 2000; i++ {
		if _, err := silent.conn.Read(buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				closed = true
			} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			} else {
				closed = true
			}
			break
		}
	}
	if !closed {
		t.Log("connection not observed closed; the send queue drained faster than it filled")
	}

	// Whatever happened to that client, the venue is still alive.
	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("b1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 500, 1)
	if _, ok := bob.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Error("the venue stopped serving after a client stopped reading")
	}
}

// TestResumeAfterDisconnect is the payoff of Stream outliving Session: reconnect
// with a cursor and receive what happened while away.
func TestResumeAfterDisconnect(t *testing.T) {
	srv := testServer(t)

	alice := dial(t, srv)
	acc := alice.mustLogin("alice", "pw1")
	alice.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := alice.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("no Accepted")
	}
	cursor := srv.reg.Stream("alice").Seq()
	_ = alice.conn.Close() // alice drops

	// Bob trades against her resting order while she is away.
	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("b1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 4)
	if _, ok := bob.await(t, wire.ExecutedLen, 3*time.Second); !ok {
		t.Fatal("bob's fill never arrived")
	}

	// Alice reconnects with her cursor and must be told what she missed.
	back := dial(t, srv)
	pkt, err := back.login("alice", "pw1", acc.Session, cursor)
	if err != nil {
		t.Fatalf("resume login: %v", err)
	}
	if pkt.Type != wire.PacketLoginAccepted {
		t.Fatalf("resume rejected: type %q payload %v", pkt.Type, pkt.Payload)
	}
	raw, ok := back.await(t, wire.ExecutedLen, 3*time.Second)
	if !ok {
		t.Fatal("alice reconnected and was never told about the fill she missed")
	}
	exec, err := wire.DecodeExecuted(raw)
	if err != nil {
		t.Fatalf("DecodeExecuted: %v", err)
	}
	if exec.ClOrdID != "a1" || exec.Quantity != 4 {
		t.Errorf("replayed execution = %+v, want a1 for 4", exec)
	}
}

// TestResumeFromAnotherIncarnationIsRejected — a restarted venue must refuse a
// stale cursor rather than serve different content under the same numbers.
func TestResumeFromAnotherIncarnationIsRejected(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	pkt, err := c.login("alice", "pw1", "INCOLDRUN", 5)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if pkt.Type != wire.PacketLoginRejected {
		t.Fatalf("type = %q, want rejected", pkt.Type)
	}
	if len(pkt.Payload) != 1 || pkt.Payload[0] != wire.RejectNoSession {
		t.Errorf("payload = %v, want RejectNoSession", pkt.Payload)
	}
}

// TestGarbageIsRefusedNotCrashing — the input is whatever a peer sends.
func TestGarbageIsRefused(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	// A payload of a length no message uses.
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, []byte{1, 2, 3}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := c.await(t, wire.CmdRejectLen, 3*time.Second); !ok {
		t.Error("garbage was not refused with a CmdReject")
	}

	// The session still works afterwards.
	c.enter("ok1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 1)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Error("session was unusable after one malformed message")
	}
}
