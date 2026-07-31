package main

import (
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// DAY and GTD over the wire. DAY rides the existing Enter as a new TIF value, because
// the venue's session close is its deadline and no field is needed. GTD needs one, and
// Enter has nowhere to put it, so it gets its own message.

func expiryServer(t *testing.T, close time.Time, expireEvery time.Duration) *Server {
	t.Helper()
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
		SessionClose: func() time.Time { return close },
		ExpireEvery:  expireEvery,
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)
	return srv
}

func (c *client) enterDated(clOrdID string, side uint8, price, qty int64, expires time.Time) {
	c.t.Helper()
	b, err := wire.EncodeEnterDated(nil, wire.EnterDated{
		Version:   wire.Version,
		Order:     c.base(clOrdID, side, wire.TypeLimit, price, qty),
		ExpiresAt: expires.UnixNano(),
	})
	c.sendPayload(b, err, "enter dated")
}

// TestDayOrderOverTheWire — accepted while the session is open.
func TestDayOrderOverTheWire(t *testing.T) {
	srv := expiryServer(t, time.Now().Add(time.Hour), time.Hour)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	b, err := wire.EncodeEnter(nil, wire.Enter{
		Version: wire.Version, ClOrdID: "d1", Symbol: "X",
		Side: wire.SideBuy, Type: wire.TypeLimit, TIF: wire.TIFDay, Price: 100, Quantity: 5,
	})
	c.sendPayload(b, err, "enter day")
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("DAY order not accepted")
	}
	if srv.runner.OrderCount() != 1 {
		t.Fatalf("book holds %d, want 1", srv.runner.OrderCount())
	}
	// It must actually be a DAY order. Accepting it as GTC would look identical here
	// and leave it resting forever, which is how the first version of this test passed
	// while the TIF byte was being ignored.
	open := srv.runner.Snapshot(1 << 20)
	if len(open.Bids) != 1 {
		t.Fatalf("expected one bid level, got %d", len(open.Bids))
	}
	orders, err := srv.runner.OpenOrdersFor("alice")
	if err != nil {
		t.Fatalf("OpenOrdersFor: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d open orders", len(orders))
	}
	if orders[0].TimeInForce != "DAY" {
		t.Errorf("time-in-force is %q, want DAY — the TIF byte was ignored", orders[0].TimeInForce)
	}
	if orders[0].ExpiresAt.IsZero() {
		t.Error("the DAY order carries no deadline")
	}
}

// TestDayOrderAfterTheCloseIsRefused — an order with no session left to rest in.
func TestDayOrderAfterTheCloseIsRefused(t *testing.T) {
	srv := expiryServer(t, time.Now().Add(-time.Hour), time.Hour) // already closed
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	b, err := wire.EncodeEnter(nil, wire.Enter{
		Version: wire.Version, ClOrdID: "d1", Symbol: "X",
		Side: wire.SideBuy, Type: wire.TypeLimit, TIF: wire.TIFDay, Price: 100, Quantity: 5,
	})
	c.sendPayload(b, err, "enter day")
	if _, ok := c.awaitType(t, wire.MsgRejected, 3*time.Second); !ok {
		t.Fatal("a DAY order after the close was not rejected")
	}
	if srv.runner.OrderCount() != 0 {
		t.Errorf("book holds %d after a refused order", srv.runner.OrderCount())
	}
}

// TestGTDOnAPlainEnterIsRefused — Enter cannot carry a deadline, so accepting the TIF
// there would leave an order the client believes is dated resting forever.
func TestGTDOnAPlainEnterIsRefused(t *testing.T) {
	srv := expiryServer(t, time.Now().Add(time.Hour), time.Hour)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	b, err := wire.EncodeEnter(nil, wire.Enter{
		Version: wire.Version, ClOrdID: "g1", Symbol: "X",
		Side: wire.SideBuy, Type: wire.TypeLimit, TIF: wire.TIFGoodTillDate, Price: 100, Quantity: 5,
	})
	c.sendPayload(b, err, "enter gtd")
	if _, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second); !ok {
		t.Fatal("GTD on a plain Enter was not refused")
	}
	if srv.runner.OrderCount() != 0 {
		t.Error("a GTD order with no deadline rested")
	}
}

// TestDatedOrderExpiresOnTheTicker is the end-to-end proof that the deadline is the
// venue's job, not the client's: nobody cancels this order and it leaves anyway.
func TestDatedOrderExpiresOnTheTicker(t *testing.T) {
	srv := expiryServer(t, time.Now().Add(time.Hour), 20*time.Millisecond)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	c.enterDated("g1", wire.SideBuy, 100, 5, time.Now().Add(150*time.Millisecond))
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("dated order not accepted")
	}
	if srv.runner.OrderCount() != 1 {
		t.Fatalf("book holds %d, want 1", srv.runner.OrderCount())
	}

	// The client sends nothing further. The venue must expire it on its own.
	if _, ok := c.awaitType(t, wire.MsgCanceled, 5*time.Second); !ok {
		t.Fatal("no Canceled arrived; the client was never told its order expired")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.runner.OrderCount() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("book still holds %d orders past the deadline", srv.runner.OrderCount())
}

// TestDatedOrderInThePastIsRefused.
func TestDatedOrderInThePastIsRefused(t *testing.T) {
	srv := expiryServer(t, time.Now().Add(time.Hour), time.Hour)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	c.enterDated("g1", wire.SideBuy, 100, 5, time.Now().Add(-time.Minute))
	if _, ok := c.awaitType(t, wire.MsgRejected, 3*time.Second); !ok {
		t.Fatal("a deadline in the past was not rejected")
	}
	if srv.runner.OrderCount() != 0 {
		t.Error("a dead-on-arrival order rested")
	}
}
