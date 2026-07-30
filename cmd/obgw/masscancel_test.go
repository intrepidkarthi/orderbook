package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// Mass cancel and cancel-on-disconnect are the two controls a market maker reaches
// for when its own state is wrong or its connectivity dies. Engine.CancelAllForUser
// has existed since v0.9.0 with no way for a client to invoke it, which is the
// difference between a venue you can test against and one you would quote on.

func (c *client) massCancel() {
	c.t.Helper()
	b, err := wire.EncodeMassCancel(nil, wire.MassCancel{Version: wire.Version})
	if err != nil {
		c.t.Fatalf("EncodeMassCancel: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		c.t.Fatalf("send mass cancel: %v", err)
	}
}

func (c *client) setCOD(enabled bool) {
	c.t.Helper()
	b, err := wire.EncodeCancelOnDisconnect(nil, wire.CancelOnDisconnect{Version: wire.Version, Enabled: enabled})
	if err != nil {
		c.t.Fatalf("EncodeCancelOnDisconnect: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		c.t.Fatalf("send COD: %v", err)
	}
}

// TestMassCancelRemovesEverything — the basic contract, plus the count.
func TestMassCancelRemovesEverything(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	for _, id := range []string{"m1", "m2", "m3", "m4"} {
		c.enter(id, wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 5)
		if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("%s not accepted", id)
		}
	}

	c.massCancel()
	raw, ok := c.awaitType(t, wire.MsgMassCancelAck, 5*time.Second)
	if !ok {
		t.Fatal("no acknowledgement; a client cannot tell a completed sweep from a dead connection")
	}
	ack, err := wire.DecodeMassCancelAck(raw)
	if err != nil {
		t.Fatalf("DecodeMassCancelAck: %v", err)
	}
	if ack.Count != 4 {
		t.Errorf("ack says %d orders cancelled, want 4", ack.Count)
	}
	if ack.Seq == 0 {
		t.Error("ack carries no stream position, so the client cannot tell what to apply on top")
	}
	if got := srv.runner.OrderCount(); got != 0 {
		t.Errorf("book still holds %d orders", got)
	}
}

// TestMassCancelAckFollowsTheCancels is the ordering guarantee. An ack saying
// "4 cancelled" that arrived before the four Canceled messages would have a client
// briefly believing it holds a book the venue has already emptied.
func TestMassCancelAckFollowsTheCancels(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	const n = 6
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		c.enter(id, wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, int64(100-i), 5)
		if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("order %d not accepted", i)
		}
	}
	c.massCancel()

	// Read until the ack, counting Canceled messages seen before it.
	cancels := 0
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("no ack after %d cancels", cancels)
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
		case wire.MsgCanceled:
			cancels++
		case wire.MsgMassCancelAck:
			ack, err := wire.DecodeMassCancelAck(pkt.Payload)
			if err != nil {
				t.Fatalf("DecodeMassCancelAck: %v", err)
			}
			if cancels != n {
				t.Errorf("ack arrived after %d Canceled messages, want all %d first", cancels, n)
			}
			if int(ack.Count) != n {
				t.Errorf("ack count %d, want %d", ack.Count, n)
			}
			return
		}
	}
}

// TestMassCancelIsPerAccount — the sweep is the session's account and nobody else's.
func TestMassCancelIsPerAccount(t *testing.T) {
	srv := testServer(t)

	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("b1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 90, 7)
	if _, ok := bob.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("bob's order not accepted")
	}

	alice := dial(t, srv)
	alice.mustLogin("alice", "pw1")
	alice.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 5)
	if _, ok := alice.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("alice's order not accepted")
	}
	alice.massCancel()
	raw, ok := alice.awaitType(t, wire.MsgMassCancelAck, 5*time.Second)
	if !ok {
		t.Fatal("no ack")
	}
	ack, _ := wire.DecodeMassCancelAck(raw)
	if ack.Count != 1 {
		t.Errorf("alice's sweep removed %d orders, want 1 — it reached beyond her account", ack.Count)
	}
	if got := srv.runner.OrderCount(); got != 1 {
		t.Errorf("book holds %d orders, want bob's 1 survivor", got)
	}
	price, _, _ := srv.runner.BestBid()
	if price != 90 {
		t.Errorf("surviving order is at %d, want bob's 90", price)
	}
}

// TestMassCancelOnEmptyBookIsAnswered — "you had nothing" must be an answer, not an
// absence of one, for the same reason QueryEnd exists.
func TestMassCancelOnEmptyBookIsAnswered(t *testing.T) {
	srv := testServer(t)
	_ = srv
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.massCancel()
	raw, ok := c.awaitType(t, wire.MsgMassCancelAck, 5*time.Second)
	if !ok {
		t.Fatal("an empty sweep went unacknowledged")
	}
	ack, _ := wire.DecodeMassCancelAck(raw)
	if ack.Count != 0 {
		t.Errorf("count = %d, want 0", ack.Count)
	}
}

// TestCancelOnDisconnectIsAcknowledged — a control that decides whether your book
// survives must never leave the client guessing.
func TestCancelOnDisconnectIsAcknowledged(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	for _, enabled := range []bool{true, false, true} {
		c.setCOD(enabled)
		raw, ok := c.awaitType(t, wire.MsgCODAck, 3*time.Second)
		if !ok {
			t.Fatalf("setting cancel-on-disconnect to %v was not acknowledged", enabled)
		}
		ack, err := wire.DecodeCODAck(raw)
		if err != nil {
			t.Fatalf("DecodeCODAck: %v", err)
		}
		if ack.Enabled != enabled {
			t.Errorf("ack says %v, want %v", ack.Enabled, enabled)
		}
	}
}

// TestCancelOnDisconnectPullsTheBook is the point of the feature: a client that
// cannot manage its orders any more should not still be quoting.
func TestCancelOnDisconnectPullsTheBook(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.setCOD(true)
	if _, ok := c.awaitType(t, wire.MsgCODAck, 3*time.Second); !ok {
		t.Fatal("COD not acknowledged")
	}
	for _, id := range []string{"d1", "d2"} {
		c.enter(id, wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 5)
		if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("%s not accepted", id)
		}
	}
	if got := srv.runner.OrderCount(); got != 2 {
		t.Fatalf("book holds %d, want 2 before the disconnect", got)
	}

	_ = c.conn.Close() // the client vanishes

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.runner.OrderCount() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("book still holds %d orders after the session dropped", srv.runner.OrderCount())
}

// TestWithoutCancelOnDisconnectTheBookSurvives — the default must be that orders
// outlive a connection, which is the whole premise of the resume design.
func TestWithoutCancelOnDisconnectTheBookSurvives(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("s1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 5)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}
	_ = c.conn.Close()

	// Give any erroneous sweep time to land.
	time.Sleep(250 * time.Millisecond)
	if got := srv.runner.OrderCount(); got != 1 {
		t.Errorf("book holds %d orders, want 1 — a connection drop pulled a book that had not asked for it", got)
	}
}

// TestVenueShutdownDoesNotFireCancelOnDisconnect is the trap this feature sets for
// itself. A graceful shutdown drops every connection at once; firing the sweep there
// would journal a cancel for every order of every COD session and permanently
// destroy books that are meant to come back after the restart.
func TestVenueShutdownDoesNotFireCancelOnDisconnect(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
		WALPath:      filepath.Join(dir, "obgw.wal"),
		SnapshotPath: filepath.Join(dir, "obgw.snap"),
	}
	srv := mustServer(t, cfg)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.setCOD(true)
	if _, ok := c.awaitType(t, wire.MsgCODAck, 3*time.Second); !ok {
		t.Fatal("COD not acknowledged")
	}
	c.enter("v1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 5)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}

	srv.Close() // the venue goes away, not the client

	cfg.Addr = "127.0.0.1:0"
	revived := mustServer(t, cfg)
	defer revived.Close()
	if got := revived.runner.OrderCount(); got != 1 {
		t.Errorf("recovered %d orders, want 1 — the venue's own shutdown fired cancel-on-disconnect and destroyed the book", got)
	}
}
