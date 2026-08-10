package main

import (
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// Trade bust, end to end over real sockets, on both edges at once.
//
// This is the test docs/TRADE-BUST.md §4.5 said could not be written: until wire
// v3 neither Executed nor MDTrade carried a trade id, so a venue could annul a
// fill it had never named, and no client could be told which one. The assertions
// below are the reason the version was bumped.

// TestBustReachesBothEdges — the two counterparties get a private Busted naming
// the fill, and every market-data subscriber gets a public MDBust naming the same
// print. The ids must agree: a drop copy and a feed disagreeing about the name of
// one trade is a reconciliation bug shipped on purpose.
func TestBustReachesBothEdges(t *testing.T) {
	srv := mdServer(t)

	maker := dial(t, srv)
	maker.mustLogin("alice", "pw1")
	taker := dial(t, srv)
	taker.mustLogin("bob", "pw2")

	sub := dialMD(t, srv)
	sub.subscribe("INC1", 0)
	// Read past the snapshot so what follows is live stream.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p := sub.read(3 * time.Second)
		if p == nil {
			t.Fatal("no snapshot arrived")
		}
		if p[0] == wire.MsgMDSnapshotEnd {
			break
		}
	}

	maker.enter("m-1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := maker.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("maker order not accepted")
	}
	taker.enter("t-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 4)

	// Both sides see the fill, and both must see the same trade id.
	makerFill := awaitExecuted(t, maker)
	takerFill := awaitExecuted(t, taker)
	if makerFill.TradeID == 0 {
		t.Fatal("Executed carries TradeID 0 — the fill has no name, so no bust can reference it")
	}
	if makerFill.TradeID != takerFill.TradeID {
		t.Fatalf("counterparties disagree about the trade id: maker %d, taker %d",
			makerFill.TradeID, takerFill.TradeID)
	}

	// The public print carries the same id.
	mdTrade := awaitMDTrade(t, sub)
	if mdTrade.TradeID != makerFill.TradeID {
		t.Errorf("market data calls the print %d, order entry calls it %d",
			mdTrade.TradeID, makerFill.TradeID)
	}

	// The operator busts it.
	if err := srv.runner.Bust(makerFill.TradeID, "erroneous order entry"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	for name, c := range map[string]*client{"maker": maker, "taker": taker} {
		p, ok := c.awaitType(t, wire.MsgBusted, 3*time.Second)
		if !ok {
			t.Fatalf("%s never received a Busted message", name)
		}
		b, err := wire.DecodeBusted(p)
		if err != nil {
			t.Fatalf("%s: DecodeBusted: %v", name, err)
		}
		if b.TradeID != makerFill.TradeID {
			t.Errorf("%s: Busted names trade %d, want %d", name, b.TradeID, makerFill.TradeID)
		}
		if b.ClOrdID == "" {
			t.Errorf("%s: Busted carries no ClOrdID", name)
		}
	}

	bust := awaitMDBust(t, sub)
	if bust.TradeID != makerFill.TradeID {
		t.Errorf("MDBust names trade %d, want %d", bust.TradeID, makerFill.TradeID)
	}
	if bust.Seq <= mdTrade.Seq {
		t.Errorf("MDBust seq %d is not after the print's %d", bust.Seq, mdTrade.Seq)
	}
}

func awaitExecuted(t *testing.T, c *client) wire.Executed {
	t.Helper()
	p, ok := c.awaitType(t, wire.MsgExecuted, 3*time.Second)
	if !ok {
		t.Fatal("no Executed arrived")
	}
	e, err := wire.DecodeExecuted(p)
	if err != nil {
		t.Fatalf("DecodeExecuted: %v", err)
	}
	return e
}

func awaitMDTrade(t *testing.T, c *mdClient) wire.MDTrade {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p := c.read(3 * time.Second)
		if p == nil || len(p) == 0 {
			continue
		}
		if p[0] == wire.MsgMDTrade {
			tr, err := wire.DecodeMDTrade(p)
			if err != nil {
				t.Fatalf("DecodeMDTrade: %v", err)
			}
			return tr
		}
	}
	t.Fatal("no MDTrade arrived")
	return wire.MDTrade{}
}

func awaitMDBust(t *testing.T, c *mdClient) wire.MDBust {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p := c.read(3 * time.Second)
		if p == nil || len(p) == 0 {
			continue
		}
		if p[0] == wire.MsgMDBust {
			b, err := wire.DecodeMDBust(p)
			if err != nil {
				t.Fatalf("DecodeMDBust: %v", err)
			}
			return b
		}
	}
	t.Fatal("no MDBust arrived")
	return wire.MDBust{}
}

// TestDuplicateClOrdIDIsRefusedWhileLive — docs/MULTI-SYMBOL.md deliverable #4.
//
// The naming index is keyed by account and client id with no symbol, so a repeat
// while the first order is still live would overwrite it and retarget this
// account's next cancel to whichever order won. That is invisible at a
// one-instrument venue and is precisely the assumption §4.5 spends to keep
// Cancel/Reduce/ReplaceOrder naming an order by client id alone — so it is
// enforced here rather than hoped for.
func TestDuplicateClOrdIDIsRefusedWhileLive(t *testing.T) {
	srv := mdServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	c.enter("dup-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 90, 5)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("the first order was not accepted")
	}

	// Same client id, still live. The refusal is a CmdReject rather than a
	// Rejected, and PROTOCOL.md's distinction is the reason: Rejected means the
	// engine looked and declined, CmdReject means the command never reached it.
	// A duplicate client id is caught at admission, so the engine never sees it.
	c.enter("dup-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 91, 5)
	p, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second)
	if !ok {
		t.Fatal("a duplicate live ClOrdID was not rejected — the account's next cancel now retargets")
	}
	r, err := wire.DecodeCmdReject(p)
	if err != nil {
		t.Fatalf("DecodeCmdReject: %v", err)
	}
	if r.Reason != wire.ReasonDuplicateClOrd {
		t.Errorf("reject reason = %d, want ReasonDuplicateClOrd (%d)", r.Reason, wire.ReasonDuplicateClOrd)
	}

	// And it stays refused after the order leaves the book — by a different layer,
	// which is the part worth knowing. The engine's own DedupClientOrderIDs ring
	// makes a client id single-use, deterministically, on the matching goroutine.
	// That is FIX's per-day uniqueness and it was already here; what it could not
	// do is span symbols, because the ring belongs to one engine.
	//
	// So the two checks are complements rather than duplicates: the engine's is
	// authoritative and per-symbol, the admission check above is venue-wide and
	// catches the cross-symbol case the ring cannot see.
	c.cancel("dup-1")
	if _, ok := c.awaitType(t, wire.MsgCanceled, 3*time.Second); !ok {
		t.Fatal("cancel not confirmed")
	}
	c.enter("dup-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 92, 5)
	p2, ok := c.awaitType(t, wire.MsgRejected, 3*time.Second)
	if !ok {
		t.Fatal("a reused client id was accepted after cancel — the engine's dedup ring is not doing its job")
	}
	r2, err := wire.DecodeRejected(p2)
	if err != nil {
		t.Fatalf("DecodeRejected: %v", err)
	}
	if r2.Reason != wire.ReasonDuplicateClOrd {
		t.Errorf("engine reject reason = %d, want ReasonDuplicateClOrd (%d)", r2.Reason, wire.ReasonDuplicateClOrd)
	}
}
