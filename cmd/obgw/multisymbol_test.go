package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

// The reference gateway over more than one instrument — the last thing standing
// between docs/MULTI-SYMBOL.md and a venue anyone can run.
//
// Every assertion here is about something that has exactly one right answer and
// several plausible wrong ones: which book an order reaches, which book a cancel
// reaches when the client never named a symbol, and whether an account's
// venue-wide operations really are venue-wide.

func multiServer(t *testing.T, symbols ...string) *Server {
	t.Helper()
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", MDAddr: "127.0.0.1:0",
		Symbols: symbols, DataDir: t.TempDir(), Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1", "bob": "pw2"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)
	return srv
}

// enterOn is enter() with the symbol chosen, since the shared helper assumes one.
func enterOn(t *testing.T, c *client, symbol, clOrdID string, side, otype, tif uint8, price, qty int64) {
	t.Helper()
	b, err := wire.EncodeEnter(nil, wire.Enter{
		Version: wire.Version, ClOrdID: clOrdID, Symbol: symbol,
		Side: side, Type: otype, TIF: tif, Price: price, Quantity: qty,
	})
	if err != nil {
		t.Fatalf("EncodeEnter: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		t.Fatalf("send enter: %v", err)
	}
}

// TestOrdersReachTheirOwnBook — the minimum, and the thing a single shared runner
// would get wrong silently.
func TestOrdersReachTheirOwnBook(t *testing.T) {
	srv := multiServer(t, "BTC-USD", "ETH-USD")
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	enterOn(t, c, "BTC-USD", "b-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 30000, 5)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("BTC order not accepted")
	}
	enterOn(t, c, "ETH-USD", "e-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 2000, 7)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("ETH order not accepted")
	}

	btc := srv.books.bySymbol("BTC-USD").runner
	eth := srv.books.bySymbol("ETH-USD").runner
	if p, q, ok := btc.BestBid(); !ok || p != 30000 || q != 5 {
		t.Errorf("BTC best bid = %d x %d (ok=%v), want 30000 x 5", p, q, ok)
	}
	if p, q, ok := eth.BestBid(); !ok || p != 2000 || q != 7 {
		t.Errorf("ETH best bid = %d x %d (ok=%v), want 2000 x 7", p, q, ok)
	}
	if btc.OrderCount() != 1 || eth.OrderCount() != 1 {
		t.Errorf("order counts BTC=%d ETH=%d, want 1 and 1 — an order reached the wrong book",
			btc.OrderCount(), eth.OrderCount())
	}
}

// TestAnOrderForAnUnservedSymbolIsRefused — the venue serves a set, not anything.
func TestAnOrderForAnUnservedSymbolIsRefused(t *testing.T) {
	srv := multiServer(t, "BTC-USD", "ETH-USD")
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	enterOn(t, c, "DOGE-USD", "d-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 1, 1)
	if _, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second); !ok {
		t.Error("an order for an unserved instrument was not refused")
	}
}

// TestCancelReachesTheRightBookWithoutNamingASymbol is the load-bearing one.
//
// wire.Cancel names an order by ClOrdID alone — that is the trade
// docs/MULTI-SYMBOL.md §4.5 made to avoid a much larger protocol change — so the
// gateway has to know which book the id belongs to without being told. Getting
// this wrong does not error: the cancel reaches a book that has never heard of
// the order, is refused as unknown, and the real order rests forever.
func TestCancelReachesTheRightBookWithoutNamingASymbol(t *testing.T) {
	srv := multiServer(t, "BTC-USD", "ETH-USD")
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	enterOn(t, c, "BTC-USD", "b-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 30000, 5)
	c.await(t, wire.AcceptedLen, 3*time.Second)
	enterOn(t, c, "ETH-USD", "e-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 2000, 7)
	c.await(t, wire.AcceptedLen, 3*time.Second)

	c.cancel("e-1")
	if _, ok := c.awaitType(t, wire.MsgCanceled, 3*time.Second); !ok {
		t.Fatal("the ETH cancel was never confirmed")
	}
	if got := srv.books.bySymbol("ETH-USD").runner.OrderCount(); got != 0 {
		t.Errorf("ETH still holds %d orders after its cancel", got)
	}
	if got := srv.books.bySymbol("BTC-USD").runner.OrderCount(); got != 1 {
		t.Errorf("BTC holds %d orders; the ETH cancel reached the wrong book", got)
	}
}

// TestCancelRoutesWhileItsOwnEnterIsStillQueued is why the session remembers the
// symbol rather than resolving the engine id up front.
//
// The naming index is written by the matching goroutine. A cancel sent
// immediately behind its Enter arrives while that Enter may still be queued, so
// there is no id to read a shard field out of — and refusing here is the
// orphaned-order defect docs/SOAK.md measured at 12,843 orders in thirty seconds.
func TestCancelRoutesWhileItsOwnEnterIsStillQueued(t *testing.T) {
	srv := multiServer(t, "BTC-USD", "ETH-USD")
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	// Back to back, with no wait between them: the cancel is on the wire before
	// the Enter can possibly have been applied.
	for i := 0; i < 20; i++ {
		id := "race-" + string(rune('a'+i))
		enterOn(t, c, "ETH-USD", id, wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 2000, 1)
		c.cancel(id)
	}

	// Every one of them must end up cancelled, not orphaned.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.books.bySymbol("ETH-USD").runner.OrderCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("%d ETH orders were left resting; a cancel was refused for an order that was about to exist",
		srv.books.bySymbol("ETH-USD").runner.OrderCount())
}

// TestMassCancelIsVenueWide — an account's mass cancel means everything it has,
// and an account can rest on every instrument.
func TestMassCancelIsVenueWide(t *testing.T) {
	srv := multiServer(t, "BTC-USD", "ETH-USD")
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	enterOn(t, c, "BTC-USD", "b-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 30000, 5)
	c.await(t, wire.AcceptedLen, 3*time.Second)
	enterOn(t, c, "BTC-USD", "b-2", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 29999, 5)
	c.await(t, wire.AcceptedLen, 3*time.Second)
	enterOn(t, c, "ETH-USD", "e-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 2000, 7)
	c.await(t, wire.AcceptedLen, 3*time.Second)

	b, err := wire.EncodeMassCancel(nil, wire.MassCancel{Version: wire.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		t.Fatal(err)
	}
	p, ok := c.awaitType(t, wire.MsgMassCancelAck, 5*time.Second)
	if !ok {
		t.Fatal("no mass cancel ack")
	}
	ack, err := wire.DecodeMassCancelAck(p)
	if err != nil {
		t.Fatalf("DecodeMassCancelAck: %v", err)
	}
	if ack.Count != 3 {
		t.Errorf("ack count = %d, want 3 — the count is per book, not per venue", ack.Count)
	}
	for _, sym := range []string{"BTC-USD", "ETH-USD"} {
		if got := srv.books.bySymbol(sym).runner.OrderCount(); got != 0 {
			t.Errorf("%s still holds %d orders after a mass cancel", sym, got)
		}
	}
}

// TestQueryReportsEveryBook — an account's open orders are its open orders.
func TestQueryReportsEveryBook(t *testing.T) {
	srv := multiServer(t, "BTC-USD", "ETH-USD")
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	enterOn(t, c, "BTC-USD", "b-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 30000, 5)
	c.await(t, wire.AcceptedLen, 3*time.Second)
	enterOn(t, c, "ETH-USD", "e-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 2000, 7)
	c.await(t, wire.AcceptedLen, 3*time.Second)

	q, err := wire.EncodeQuery(nil, wire.Query{Version: wire.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, q); err != nil {
		t.Fatal(err)
	}
	p, ok := c.awaitType(t, wire.MsgQueryEnd, 5*time.Second)
	if !ok {
		t.Fatal("no query end")
	}
	end, err := wire.DecodeQueryEnd(p)
	if err != nil {
		t.Fatalf("DecodeQueryEnd: %v", err)
	}
	if end.Count != 2 {
		t.Errorf("query reported %d open orders, want 2 — one book was not asked", end.Count)
	}
}

// TestIDsAreUniqueAcrossBooks ties the gateway back to the id design: two books,
// disjoint id ranges, and a manifest that survives the process.
func TestIDsAreUniqueAcrossBooks(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Addr: "127.0.0.1:0", Symbols: []string{"BTC-USD", "ETH-USD"},
		DataDir: dir, Incarnation: "INC1",
		Accounts: map[string]string{"alice": "pw1"},
	}
	srv := mustServer(t, cfg)
	defer srv.Close()

	btc := srv.books.bySymbol("BTC-USD")
	eth := srv.books.bySymbol("ETH-USD")
	if btc.shardIndex == eth.shardIndex {
		t.Fatalf("both books hold shard index %d", btc.shardIndex)
	}

	// The manifest is on disk and binds the same symbols to the same indices.
	man, err := matching.LoadManifest(filepath.Join(dir, "venue.json"))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	for _, b := range []*symbolBook{btc, eth} {
		idx, err := man.IndexFor(b.symbol)
		if err != nil {
			t.Fatalf("IndexFor(%s): %v", b.symbol, err)
		}
		if idx != b.shardIndex {
			t.Errorf("%s: manifest says %d, book says %d", b.symbol, idx, b.shardIndex)
		}
	}
}

// TestMarketDataIsPerBook — two subscribers, two books, over the gateway's own
// market-data port.
func TestMarketDataIsPerBook(t *testing.T) {
	srv := multiServer(t, "BTC-USD", "ETH-USD")
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	enterOn(t, c, "BTC-USD", "b-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 30000, 5)
	c.await(t, wire.AcceptedLen, 3*time.Second)
	enterOn(t, c, "ETH-USD", "e-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 2000, 7)
	c.await(t, wire.AcceptedLen, 3*time.Second)

	for _, tc := range []struct {
		symbol    string
		wantPrice int64
		wantQty   int64
	}{{"BTC-USD", 30000, 5}, {"ETH-USD", 2000, 7}} {
		sub := dialMD(t, srv)
		b, err := wire.EncodeMDSubscribe(nil, wire.MDSubscribe{
			Version: wire.Version, Incarnation: "INC1", Seq: 0, Symbol: tc.symbol,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := wire.WritePacket(sub.conn, wire.PacketUnsequenced, b); err != nil {
			t.Fatal(err)
		}

		var levels int
		var gotPrice, gotQty int64
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			p := sub.read(3 * time.Second)
			if p == nil {
				t.Fatalf("%s: no snapshot", tc.symbol)
			}
			if p[0] == wire.MsgMDLevel {
				l, err := wire.DecodeMDLevel(p)
				if err != nil {
					t.Fatalf("DecodeMDLevel: %v", err)
				}
				levels++
				gotPrice, gotQty = l.Price, l.Qty
			}
			if p[0] == wire.MsgMDSnapshotEnd {
				break
			}
		}
		if levels != 1 {
			t.Errorf("%s snapshot carried %d levels, want 1 — the feeds are shared", tc.symbol, levels)
		}
		if gotPrice != tc.wantPrice || gotQty != tc.wantQty {
			t.Errorf("%s snapshot level = %d x %d, want %d x %d",
				tc.symbol, gotPrice, gotQty, tc.wantPrice, tc.wantQty)
		}
	}
}

// TestMultiSymbolVenueSurvivesRestart — one log and one snapshot per book, and a
// manifest that keeps every id meaning what it meant.
func TestMultiSymbolVenueSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Addr: "127.0.0.1:0", Symbols: []string{"BTC-USD", "ETH-USD"},
		DataDir: dir, Incarnation: "INC1",
		Accounts: map[string]string{"alice": "pw1"},
		WALPath:  "ignored-for-multi", // DataDir is what counts
	}
	srv := mustServer(t, cfg)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	enterOn(t, c, "BTC-USD", "b-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 30000, 5)
	c.await(t, wire.AcceptedLen, 3*time.Second)
	enterOn(t, c, "ETH-USD", "e-1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 2000, 7)
	c.await(t, wire.AcceptedLen, 3*time.Second)

	want := map[string]int64{}
	for _, sym := range []string{"BTC-USD", "ETH-USD"} {
		snap, err := srv.books.bySymbol(sym).runner.Checkpoint()
		if err != nil {
			t.Fatalf("Checkpoint(%s): %v", sym, err)
		}
		if len(snap.Orders) != 1 {
			t.Fatalf("%s: %d resting orders before restart, want 1", sym, len(snap.Orders))
		}
		want[sym] = snap.Orders[0].ID
	}
	srv.Close()

	again := mustServer(t, cfg)
	defer again.Close()
	for sym, id := range want {
		snap, err := again.books.bySymbol(sym).runner.Checkpoint()
		if err != nil {
			t.Fatalf("Checkpoint(%s) after restart: %v", sym, err)
		}
		if len(snap.Orders) != 1 {
			t.Fatalf("%s: %d resting orders after restart, want 1", sym, len(snap.Orders))
		}
		if got := snap.Orders[0].ID; got != id {
			t.Errorf("%s order id %d before restart, %d after — the manifest did not hold", sym, id, got)
		}
	}
}
