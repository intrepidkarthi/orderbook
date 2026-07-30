package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// Reduce was the asymmetry the protocol shipped with: the engine had had the
// operation since v0.10.0 and the wire had carried the outbound Replaced that
// reports it, but no inbound message could ask for one. A client's only route to a
// smaller order was cancel-then-new, which sends it to the back of its price level
// — the exact cost Engine.Reduce exists to avoid.

// threeAccountServer is needed wherever a taker must not be one of the makers:
// self-trade prevention would otherwise decide the outcome instead of the queue.
func threeAccountServer(t *testing.T) *Server {
	t.Helper()
	srv := mustServer(t, Config{
		Addr:          "127.0.0.1:0",
		Symbol:        "X",
		Incarnation:   "INC0000001",
		Accounts:      map[string]string{"alice": "pw1", "bob": "pw2", "carol": "pw3"},
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

// TestReduceOverTheWire — the round trip: a client asks for a smaller order and is
// told the new size on its own stream.
func TestReduceOverTheWire(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	c.enter("r1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 100)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}

	c.reduce("r1", 40)
	raw, ok := c.awaitType(t, wire.MsgReplaced, 3*time.Second)
	if !ok {
		t.Fatal("no Replaced; the client cannot tell whether its reduce landed")
	}
	rep, err := wire.DecodeReplaced(raw)
	if err != nil {
		t.Fatalf("DecodeReplaced: %v", err)
	}
	if rep.ClOrdID != "r1" {
		t.Errorf("Replaced names %q, want r1", rep.ClOrdID)
	}
	if rep.LeavesQty != 40 {
		t.Errorf("LeavesQty = %d, want 40", rep.LeavesQty)
	}

	// The venue's own book must agree with what the client was told.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, qty, ok := srv.runner.BestBid(); ok && qty == 40 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	_, qty, _ := srv.runner.BestBid()
	t.Errorf("book shows depth %d, want 40 — the client was told a size the venue does not hold", qty)
}

// TestReduceKeepsQueuePositionOverTheWire is the point of the whole message. If
// the reduced order lost priority, a client could have got the same result with
// cancel-then-new and nothing needed adding to the protocol.
func TestReduceKeepsQueuePositionOverTheWire(t *testing.T) {
	srv := threeAccountServer(t)

	// alice rests first and is therefore at the front of the level.
	alice := dial(t, srv)
	alice.mustLogin("alice", "pw1")
	alice.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 100)
	if _, ok := alice.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("alice's order not accepted")
	}

	// bob queues behind her at the same price.
	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("b1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 100)
	if _, ok := bob.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("bob's order not accepted")
	}

	alice.reduce("a1", 40)
	if _, ok := alice.awaitType(t, wire.MsgReplaced, 3*time.Second); !ok {
		t.Fatal("alice's reduce was not confirmed")
	}

	// carol sells 40. It must all go to alice, who is still in front.
	carol := dial(t, srv)
	carol.mustLogin("carol", "pw3")
	carol.enter("c1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 40)

	raw, ok := alice.awaitType(t, wire.MsgExecuted, 3*time.Second)
	if !ok {
		t.Fatal("alice was not filled — the reduced order went to the back of the queue")
	}
	ex, err := wire.DecodeExecuted(raw)
	if err != nil {
		t.Fatalf("DecodeExecuted: %v", err)
	}
	if ex.ClOrdID != "a1" {
		t.Errorf("fill names %q, want a1", ex.ClOrdID)
	}
	if ex.Quantity != 40 {
		t.Errorf("filled %d, want 40", ex.Quantity)
	}
	if ex.LeavesQty != 0 {
		t.Errorf("leaves %d, want 0 — a 40-lot order filled for 40 is done", ex.LeavesQty)
	}
}

// TestReduceIsNotAnIncrease — a size increase would let a participant reserve a
// place in line and grow into it, so it is refused rather than reinterpreted.
func TestReduceIsNotAnIncrease(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	c.enter("r1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}

	c.reduce("r1", 500)
	raw, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second)
	if !ok {
		t.Fatal("an increase was accepted, or refused in silence")
	}
	rej, err := wire.DecodeCmdReject(raw)
	if err != nil {
		t.Fatalf("DecodeCmdReject: %v", err)
	}
	if rej.ClOrdID != "r1" {
		t.Errorf("rejection names %q, want r1", rej.ClOrdID)
	}
	if rej.Reason != wire.ReasonInvalidQuantity {
		t.Errorf("reason = %d, want ReasonInvalidQuantity (%d)", rej.Reason, wire.ReasonInvalidQuantity)
	}

	// And the order must be exactly as it was.
	if _, qty, _ := srv.runner.BestBid(); qty != 10 {
		t.Errorf("depth = %d, want 10 — a refused reduce changed the book", qty)
	}
}

// TestReduceToNonPositiveIsRejected — zero is a cancel, and a client that means to
// cancel must say so. Reinterpreting it here would silently give the message two
// meanings.
func TestReduceToNonPositiveIsRejected(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	c.enter("r1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}

	for _, qty := range []int64{0, -5} {
		c.reduce("r1", qty)
		raw, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second)
		if !ok {
			t.Fatalf("reduce to %d was not refused", qty)
		}
		rej, err := wire.DecodeCmdReject(raw)
		if err != nil {
			t.Fatalf("DecodeCmdReject: %v", err)
		}
		if rej.Reason != wire.ReasonInvalidQuantity {
			t.Errorf("reduce to %d: reason = %d, want ReasonInvalidQuantity", qty, rej.Reason)
		}
	}
	if _, qty, _ := srv.runner.BestBid(); qty != 10 {
		t.Errorf("depth = %d, want 10", qty)
	}
	if srv.runner.OrderCount() != 1 {
		t.Error("a reduce to zero cancelled the order; it must be refused, not reinterpreted")
	}
}

// TestCannotReduceAnotherAccountsOrder — the wire carries no account and no engine
// id, so naming someone else's order is not expressible. This checks the guard
// that makes guessing a common ClOrdID useless.
func TestCannotReduceAnotherAccountsOrder(t *testing.T) {
	srv := testServer(t)

	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("shared", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 100)
	if _, ok := bob.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("bob's order not accepted")
	}

	alice := dial(t, srv)
	alice.mustLogin("alice", "pw1")
	alice.reduce("shared", 1)

	raw, ok := alice.awaitType(t, wire.MsgCmdReject, 3*time.Second)
	if !ok {
		t.Fatal("alice's reduce of bob's order was not refused")
	}
	rej, err := wire.DecodeCmdReject(raw)
	if err != nil {
		t.Fatalf("DecodeCmdReject: %v", err)
	}
	if rej.Reason != wire.ReasonUnknownOrder {
		t.Errorf("reason = %d, want ReasonUnknownOrder — a probe must not be able to tell \"not yours\" from \"does not exist\"", rej.Reason)
	}
	if _, qty, _ := srv.runner.BestBid(); qty != 100 {
		t.Errorf("bob's depth = %d, want 100 — another account resized his order", qty)
	}
}

// TestReduceIsDurable — the size the client was told must be the size the venue
// holds after a restart. A reduce applied to the book but absent from the log was
// silently undone by recovery, which is worse than refusing the reduce.
func TestReduceIsDurable(t *testing.T) {
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
	c.enter("d1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 100)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}
	c.reduce("d1", 40)
	if _, ok := c.awaitType(t, wire.MsgReplaced, 3*time.Second); !ok {
		t.Fatal("reduce not confirmed")
	}
	srv.Close() // syncs and closes the log

	cfg.Addr = "127.0.0.1:0"
	revived := mustServer(t, cfg)
	defer revived.Close()
	if got := revived.runner.OrderCount(); got != 1 {
		t.Fatalf("recovered %d orders, want 1", got)
	}
	if _, qty, ok := revived.runner.BestBid(); !ok || qty != 40 {
		t.Errorf("after restart the book shows %d (ok=%v), want 40 — the reduce did not survive", qty, ok)
	}
}
