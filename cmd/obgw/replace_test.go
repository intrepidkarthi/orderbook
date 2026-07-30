package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// Without an atomic replace, a reprice is Cancel then Enter and the client is naked
// in between: if the connection dies in the gap it does not know whether it holds
// zero orders or one, and another participant can take the price. Every real venue
// offers a replace for exactly that reason.

func (c *client) replace(origID, newID string, side uint8, price, qty int64) {
	c.t.Helper()
	b, err := wire.EncodeReplaceOrder(nil, wire.ReplaceOrder{
		Version: wire.Version, OrigClOrdID: origID,
		Order: c.base(newID, side, wire.TypeLimit, price, qty),
	})
	c.sendPayload(b, err, "replace")
}

// TestReplaceCancelsAndEnters — the round trip, described by the two messages that
// already exist: a Canceled for the old id and an Accepted for the new one.
func TestReplaceCancelsAndEnters(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	c.enter("r-old", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("original not accepted")
	}

	c.replace("r-old", "r-new", wire.SideBuy, 105, 12)

	var sawCancel, sawAccept bool
	deadline := time.Now().Add(5 * time.Second)
	for (!sawCancel || !sawAccept) && time.Now().Before(deadline) {
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
			cn, err := wire.DecodeCanceled(pkt.Payload)
			if err != nil {
				t.Fatalf("DecodeCanceled: %v", err)
			}
			if cn.ClOrdID == "r-old" {
				sawCancel = true
			}
		case wire.MsgAccepted:
			acc, err := wire.DecodeAccepted(pkt.Payload)
			if err != nil {
				t.Fatalf("DecodeAccepted: %v", err)
			}
			if acc.ClOrdID == "r-new" {
				sawAccept = true
				if acc.Price != 105 || acc.Quantity != 12 {
					t.Errorf("replacement is %d@%d, want 12@105", acc.Quantity, acc.Price)
				}
			}
		case wire.MsgCmdReject:
			rej, _ := wire.DecodeCmdReject(pkt.Payload)
			t.Fatalf("replace refused: reason %d for %q", rej.Reason, rej.ClOrdID)
		}
	}
	if !sawCancel {
		t.Error("no Canceled for the original")
	}
	if !sawAccept {
		t.Error("no Accepted for the replacement")
	}
	if got := srv.runner.OrderCount(); got != 1 {
		t.Errorf("book holds %d orders, want exactly 1 — a replace must not leave both", got)
	}
	if price, qty, _ := srv.runner.BestBid(); price != 105 || qty != 12 {
		t.Errorf("book shows %d@%d, want 12@105", qty, price)
	}
}

// TestReplaceOfAnUnknownOrderEntersNothing is the failure semantics that matter. A
// client replacing an order it no longer holds did not ask to open a new position,
// and entering one anyway would double its exposure at the worst possible moment.
func TestReplaceOfAnUnknownOrderEntersNothing(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	c.replace("never-existed", "r-new", wire.SideBuy, 105, 12)

	raw, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second)
	if !ok {
		t.Fatal("replacing a nonexistent order was not refused")
	}
	rej, _ := wire.DecodeCmdReject(raw)
	if rej.ClOrdID != "never-existed" {
		t.Errorf("rejection names %q, want the original id — that is the order the client was wrong about", rej.ClOrdID)
	}
	if rej.Reason != wire.ReasonUnknownOrder {
		t.Errorf("reason = %d, want ReasonUnknownOrder", rej.Reason)
	}
	// Give any erroneous entry time to land.
	time.Sleep(200 * time.Millisecond)
	if got := srv.runner.OrderCount(); got != 0 {
		t.Errorf("book holds %d orders — the replacement was entered for an order that did not exist", got)
	}
}

// TestReplaceCannotTouchAnotherAccountsOrder — the ClOrdID index is per account, so
// naming someone else's order is not expressible.
func TestReplaceCannotTouchAnotherAccountsOrder(t *testing.T) {
	srv := testServer(t)

	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("shared", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := bob.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("bob's order not accepted")
	}

	alice := dial(t, srv)
	alice.mustLogin("alice", "pw1")
	alice.replace("shared", "hijack", wire.SideBuy, 105, 99)

	raw, ok := alice.awaitType(t, wire.MsgCmdReject, 3*time.Second)
	if !ok {
		t.Fatal("alice's replace of bob's order was not refused")
	}
	rej, _ := wire.DecodeCmdReject(raw)
	if rej.Reason != wire.ReasonUnknownOrder {
		t.Errorf("reason = %d, want ReasonUnknownOrder", rej.Reason)
	}
	if price, qty, _ := srv.runner.BestBid(); price != 100 || qty != 10 {
		t.Errorf("bob's order is now %d@%d — another account replaced it", qty, price)
	}
	if got := srv.runner.OrderCount(); got != 1 {
		t.Errorf("book holds %d orders, want bob's 1", got)
	}
}

// TestReplaceForfeitsQueuePriority — the correct behaviour, and the reason Reduce
// exists separately. An order that could reprice in place would let a participant
// reserve a place in line.
func TestReplaceForfeitsQueuePriority(t *testing.T) {
	srv := threeAccountServer(t)

	alice := dial(t, srv)
	alice.mustLogin("alice", "pw1")
	alice.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := alice.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("alice's order not accepted")
	}

	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("b1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := bob.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("bob's order not accepted")
	}

	// Alice replaces at the same price. She must end up behind bob.
	alice.replace("a1", "a2", wire.SideBuy, 100, 10)
	if _, ok := alice.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("alice's replacement was not accepted")
	}

	// Carol sells 10: it must hit bob, who is now in front.
	carol := dial(t, srv)
	carol.mustLogin("carol", "pw3")
	carol.enter("c1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)

	raw, ok := bob.awaitType(t, wire.MsgExecuted, 5*time.Second)
	if !ok {
		t.Fatal("bob was not filled — the replaced order kept its place in the queue")
	}
	ex, _ := wire.DecodeExecuted(raw)
	if ex.ClOrdID != "b1" {
		t.Errorf("fill names %q, want b1", ex.ClOrdID)
	}
}

// TestReplaceIsDurable — the replacement must be what comes back, not the original.
func TestReplaceIsDurable(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
		WALPath: filepath.Join(dir, "obgw.wal"),
	}
	srv := mustServer(t, cfg)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("d-old", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("original not accepted")
	}
	c.replace("d-old", "d-new", wire.SideBuy, 107, 4)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("replacement not accepted")
	}
	srv.Close()

	cfg.Addr = "127.0.0.1:0"
	revived := mustServer(t, cfg)
	defer revived.Close()
	if got := revived.runner.OrderCount(); got != 1 {
		t.Fatalf("recovered %d orders, want 1", got)
	}
	price, qty, ok := revived.runner.BestBid()
	if !ok {
		t.Fatal("no bid after recovery")
	}
	if price != 107 || qty != 4 {
		t.Errorf("recovered %d@%d, want the replacement 4@107 — the replace was not journalled", qty, price)
	}
}
