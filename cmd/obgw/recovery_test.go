package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// Recovery restored the book and nothing else. The session layer's ClOrdID index
// started empty against a non-empty venue, so a recovered order was visible in a
// Query reply and unreachable by every other message — and a fill against one
// produced no execution report, because the publisher had no record of the order it
// belonged to. That last one is the failure the whole stream-outlives-the-connection
// design exists to prevent.

// durableServer starts a server over the given files.
func durableServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	srv := mustServer(t, cfg)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	return srv
}

// durableConfig is a two-account venue with durability on.
func durableConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1", "bob": "pw2"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
		WALPath:      filepath.Join(dir, "obgw.wal"),
		SnapshotPath: filepath.Join(dir, "obgw.snap"),
	}
}

// TestRecoveredOrderCanBeCancelled — a client that can see an order must be able to
// act on it. Seeing it and being told it does not exist are contradictory answers
// from the same venue.
func TestRecoveredOrderCanBeCancelled(t *testing.T) {
	cfg := durableConfig(t)
	srv := durableServer(t, cfg)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}
	srv.Close()

	cfg.Addr = "127.0.0.1:0"
	revived := durableServer(t, cfg)
	defer revived.Close()
	if got := revived.runner.OrderCount(); got != 1 {
		t.Fatalf("recovered %d orders, want 1", got)
	}

	c2 := dial(t, revived)
	c2.mustLogin("alice", "pw1")
	c2.cancel("a1")

	if _, ok := c2.awaitType(t, wire.MsgCanceled, 3*time.Second); !ok {
		t.Fatal("the cancel of a recovered order was not confirmed")
	}
	if got := revived.runner.OrderCount(); got != 0 {
		t.Errorf("book still holds %d orders after the cancel", got)
	}
}

// TestRecoveredOrderCanBeReduced — the same for the message this release added.
func TestRecoveredOrderCanBeReduced(t *testing.T) {
	cfg := durableConfig(t)
	srv := durableServer(t, cfg)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 100)
	if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}
	srv.Close()

	cfg.Addr = "127.0.0.1:0"
	revived := durableServer(t, cfg)
	defer revived.Close()

	c2 := dial(t, revived)
	c2.mustLogin("alice", "pw1")
	c2.reduce("a1", 40)

	raw, ok := c2.awaitType(t, wire.MsgReplaced, 3*time.Second)
	if !ok {
		t.Fatal("the reduce of a recovered order was not confirmed")
	}
	rep, err := wire.DecodeReplaced(raw)
	if err != nil {
		t.Fatalf("DecodeReplaced: %v", err)
	}
	if rep.LeavesQty != 40 {
		t.Errorf("LeavesQty = %d, want 40", rep.LeavesQty)
	}
	if _, qty, _ := revived.runner.BestBid(); qty != 40 {
		t.Errorf("depth = %d, want 40", qty)
	}
}

// TestRecoveredOrderStillReportsItsFills is the severe case. A maker whose resting
// order fills after a restart must be told. Before adoption the publisher had no
// record of the order, so publishTrade found nothing and dropped the report — the
// client's position was wrong and it had no way to know.
func TestRecoveredOrderStillReportsItsFills(t *testing.T) {
	cfg := durableConfig(t)
	srv := durableServer(t, cfg)

	maker := dial(t, srv)
	maker.mustLogin("alice", "pw1")
	maker.enter("m1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := maker.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("maker's order not accepted")
	}
	srv.Close()

	cfg.Addr = "127.0.0.1:0"
	revived := durableServer(t, cfg)
	defer revived.Close()

	// The maker reconnects. A new incarnation means a fresh sequence space, which
	// is why it logs in without a resume cursor.
	maker2 := dial(t, revived)
	maker2.mustLogin("alice", "pw1")

	// Someone else takes the recovered liquidity.
	taker := dial(t, revived)
	taker.mustLogin("bob", "pw2")
	taker.enter("t1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 4)

	raw, ok := maker2.awaitType(t, wire.MsgExecuted, 3*time.Second)
	if !ok {
		t.Fatal("the maker was never told its recovered order filled")
	}
	ex, err := wire.DecodeExecuted(raw)
	if err != nil {
		t.Fatalf("DecodeExecuted: %v", err)
	}
	if ex.ClOrdID != "m1" {
		t.Errorf("fill names %q, want m1", ex.ClOrdID)
	}
	if ex.Quantity != 4 {
		t.Errorf("filled %d, want 4", ex.Quantity)
	}
	if ex.LeavesQty != 6 {
		t.Errorf("LeavesQty = %d, want 6", ex.LeavesQty)
	}
}

// TestAdoptedRemainingSurvivesAPartialFill pins the field that is easy to get
// wrong. A recovered order that was already partly filled must be adopted at its
// REMAINING size, not its original one, or the next fill reports a LeavesQty too
// high by exactly what had already traded — and the client's position drifts
// without either side erroring.
func TestAdoptedRemainingSurvivesAPartialFill(t *testing.T) {
	cfg := durableConfig(t)
	srv := durableServer(t, cfg)

	maker := dial(t, srv)
	maker.mustLogin("alice", "pw1")
	maker.enter("m1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := maker.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("maker's order not accepted")
	}

	// Fill 6 of 10 before the restart, leaving 4 resting.
	taker := dial(t, srv)
	taker.mustLogin("bob", "pw2")
	taker.enter("t1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 6)
	if _, ok := maker.awaitType(t, wire.MsgExecuted, 3*time.Second); !ok {
		t.Fatal("the pre-restart fill was not reported")
	}
	srv.Close()

	cfg.Addr = "127.0.0.1:0"
	revived := durableServer(t, cfg)
	defer revived.Close()
	if _, qty, _ := revived.runner.BestBid(); qty != 4 {
		t.Fatalf("recovered depth = %d, want 4 — the premise of this test", qty)
	}

	maker2 := dial(t, revived)
	maker2.mustLogin("alice", "pw1")

	taker2 := dial(t, revived)
	taker2.mustLogin("bob", "pw2")
	taker2.enter("t2", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 1)

	raw, ok := maker2.awaitType(t, wire.MsgExecuted, 3*time.Second)
	if !ok {
		t.Fatal("no execution report after the restart")
	}
	ex, err := wire.DecodeExecuted(raw)
	if err != nil {
		t.Fatalf("DecodeExecuted: %v", err)
	}
	if ex.LeavesQty != 3 {
		t.Errorf("LeavesQty = %d, want 3 — the order was adopted at its original size, not its remaining one", ex.LeavesQty)
	}
}

// TestAdoptionKeepsAccountsApart — adoption must not collapse the scoping that
// stops one client naming another's order. Both accounts here use the same ClOrdID
// string, which is legal and must stay unambiguous across a restart.
func TestAdoptionKeepsAccountsApart(t *testing.T) {
	cfg := durableConfig(t)
	srv := durableServer(t, cfg)

	alice := dial(t, srv)
	alice.mustLogin("alice", "pw1")
	alice.enter("same-id", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := alice.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("alice's order not accepted")
	}

	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("same-id", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 90, 20)
	if _, ok := bob.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("bob's order not accepted")
	}
	srv.Close()

	cfg.Addr = "127.0.0.1:0"
	revived := durableServer(t, cfg)
	defer revived.Close()

	// alice cancels "same-id" and must remove her own order, not bob's.
	a2 := dial(t, revived)
	a2.mustLogin("alice", "pw1")
	a2.cancel("same-id")
	if _, ok := a2.awaitType(t, wire.MsgCanceled, 3*time.Second); !ok {
		t.Fatal("alice's cancel was not confirmed")
	}

	// Bob's order is the one still resting, at his price.
	if got := revived.runner.OrderCount(); got != 1 {
		t.Fatalf("book holds %d orders, want 1", got)
	}
	price, qty, ok := revived.runner.BestBid()
	if !ok {
		t.Fatal("no bid left")
	}
	if price != 90 || qty != 20 {
		t.Errorf("surviving order is %d@%d, want 20@90 — the wrong account's order was cancelled", qty, price)
	}
}

// TestQueryAfterRecoveryAgreesWithWhatCanBeActedOn — the report and the reachable
// set were allowed to disagree, which is the contradiction this fixes. Every order
// a Query names must be one the client can then cancel.
func TestQueryAfterRecoveryAgreesWithWhatCanBeActedOn(t *testing.T) {
	cfg := durableConfig(t)
	srv := durableServer(t, cfg)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	for _, id := range []string{"q1", "q2", "q3"} {
		c.enter(id, wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 5)
		if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("%s not accepted", id)
		}
	}
	srv.Close()

	cfg.Addr = "127.0.0.1:0"
	revived := durableServer(t, cfg)
	defer revived.Close()

	c2 := dial(t, revived)
	c2.mustLogin("alice", "pw1")

	q, err := wire.EncodeQuery(nil, wire.Query{Version: wire.Version})
	if err != nil {
		t.Fatalf("EncodeQuery: %v", err)
	}
	if err := wire.WritePacket(c2.conn, wire.PacketUnsequenced, q); err != nil {
		t.Fatalf("send query: %v", err)
	}

	var reported []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("query never terminated")
		}
		_ = c2.conn.SetReadDeadline(deadline)
		pkt, err := wire.ReadPacket(c2.conn, c2.buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if pkt.Type != wire.PacketSequencedData || len(pkt.Payload) < 2 {
			continue
		}
		if pkt.Payload[0] == wire.MsgOpenOrder {
			oo, err := wire.DecodeOpenOrder(pkt.Payload)
			if err != nil {
				t.Fatalf("DecodeOpenOrder: %v", err)
			}
			reported = append(reported, oo.ClOrdID)
			continue
		}
		if pkt.Payload[0] == wire.MsgQueryEnd {
			break
		}
	}
	if len(reported) != 3 {
		t.Fatalf("query reported %v, want three orders", reported)
	}

	// Every reported order must be cancellable. Before adoption all three were
	// reported and none was.
	for _, id := range reported {
		c2.cancel(id)
		if _, ok := c2.awaitType(t, wire.MsgCanceled, 3*time.Second); !ok {
			t.Errorf("%s was reported by Query but could not be cancelled", id)
		}
	}
	if got := revived.runner.OrderCount(); got != 0 {
		t.Errorf("book still holds %d orders", got)
	}
}

// TestATornLogStillYieldsANameableBook composes two things that are tested apart and
// were never tested together: a log torn by a crash, and a client naming an order that
// survived it.
//
// The WAL distinguishes a torn tail from media corruption, and recovery seeds the
// session index from the recovered book. Both have their own tests. What neither
// covers is the whole chain a real restart runs — torn log, recovered book, adopted
// index, and a cancel resolved on the matching goroutine — which is the chain a venue
// actually depends on at the worst moment it will ever have.
//
// The tear is written the way a crash writes one: a length header with fewer bytes
// after it than it claims, appended to a log whose earlier records are intact.
func TestATornLogStillYieldsANameableBook(t *testing.T) {
	cfg := durableConfig(t)
	srv := durableServer(t, cfg)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("survivor", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}
	// No snapshot: force recovery to go through the log rather than restoring a
	// checkpoint and reading nothing after it.
	cfg.SnapshotPath = ""
	srv.Close()

	f, err := os.OpenFile(cfg.WALPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	// A four-byte length prefix claiming 4 KiB, followed by nothing.
	if _, err := f.Write([]byte{0x00, 0x00, 0x10, 0x00}); err != nil {
		t.Fatalf("tear the log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close wal: %v", err)
	}

	cfg.Addr = "127.0.0.1:0"
	revived, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("a torn tail refused to start the venue: %v", err)
	}
	if err := revived.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = revived.Serve() }()
	defer revived.Close()

	if got := revived.runner.OrderCount(); got != 1 {
		t.Fatalf("recovered %d orders, want the one written before the tear", got)
	}

	c2 := dial(t, revived)
	c2.mustLogin("alice", "pw1")
	c2.cancel("survivor")
	if _, ok := c2.awaitType(t, wire.MsgCanceled, 3*time.Second); !ok {
		t.Fatal("an order that survived a torn log could not be named by its client id")
	}
	if got := revived.runner.OrderCount(); got != 0 {
		t.Errorf("book still holds %d orders after the cancel", got)
	}
}
