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
