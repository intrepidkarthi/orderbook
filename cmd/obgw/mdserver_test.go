package main

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// The market-data edge, end to end over a real socket.
//
// The contract a subscriber is owed: a snapshot names the sequence it is consistent
// with, everything after that sequence applies on top of it, and nothing at or below
// it does. If that holds, a subscriber can join at any moment and be exactly right.

// mdClient is a minimal market-data subscriber, which doubles as a check that the
// protocol is usable by someone who only has the format.
type mdClient struct {
	t    *testing.T
	conn net.Conn
	buf  []byte
}

func dialMD(t *testing.T, srv *Server) *mdClient {
	t.Helper()
	conn, err := net.Dial("tcp", srv.MDAddr().String())
	if err != nil {
		t.Fatalf("dial market data: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &mdClient{t: t, conn: conn, buf: make([]byte, wire.MaxPayload)}
}

func (c *mdClient) subscribe(incarnation string, seq uint64) {
	c.t.Helper()
	b, err := wire.EncodeMDSubscribe(nil, wire.MDSubscribe{
		Version: wire.Version, Incarnation: incarnation, Seq: seq,
	})
	if err != nil {
		c.t.Fatalf("EncodeMDSubscribe: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		c.t.Fatalf("send subscribe: %v", err)
	}
}

// read returns the next market-data payload, or nil on timeout.
func (c *mdClient) read(timeout time.Duration) []byte {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	pkt, err := wire.ReadPacket(c.conn, c.buf)
	if err != nil {
		return nil
	}
	out := make([]byte, len(pkt.Payload))
	copy(out, pkt.Payload)
	return out
}

// book is a subscriber's own view, built the way a real one would.
type book struct {
	bids, asks map[int64]int64
	seq        uint64
	lastTrade  int64
}

func newBook() *book {
	return &book{bids: map[int64]int64{}, asks: map[int64]int64{}}
}

func (b *book) side(s uint8) map[int64]int64 {
	if s == wire.SideSell {
		return b.asks
	}
	return b.bids
}

func (b *book) apply(t *testing.T, payload []byte) (kind uint8) {
	t.Helper()
	if len(payload) < 2 {
		return 0
	}
	switch payload[0] {
	case wire.MsgMDLevel:
		l, err := wire.DecodeMDLevel(payload)
		if err != nil {
			t.Fatalf("DecodeMDLevel: %v", err)
		}
		b.side(l.Side)[l.Price] = l.Qty
	case wire.MsgMDSnapshotEnd:
		e, err := wire.DecodeMDSnapshotEnd(payload)
		if err != nil {
			t.Fatalf("DecodeMDSnapshotEnd: %v", err)
		}
		b.seq = e.Seq
		b.lastTrade = e.LastTradePrice
	case wire.MsgMDDelta:
		d, err := wire.DecodeMDDelta(payload)
		if err != nil {
			t.Fatalf("DecodeMDDelta: %v", err)
		}
		if d.Qty == 0 {
			delete(b.side(d.Side), d.Price)
		} else {
			b.side(d.Side)[d.Price] = d.Qty
		}
		b.seq = d.Seq
	case wire.MsgMDTrade:
		tr, err := wire.DecodeMDTrade(payload)
		if err != nil {
			t.Fatalf("DecodeMDTrade: %v", err)
		}
		b.lastTrade = tr.Price
		b.seq = tr.Seq
	case wire.MsgMDStatus:
		st, err := wire.DecodeMDStatus(payload)
		if err != nil {
			t.Fatalf("DecodeMDStatus: %v", err)
		}
		b.seq = st.Seq
	}
	return payload[0]
}

// drain reads until nothing arrives for quiet, applying everything.
func (b *book) drain(t *testing.T, c *mdClient, quiet time.Duration) {
	t.Helper()
	for {
		p := c.read(quiet)
		if p == nil {
			return
		}
		b.apply(t, p)
	}
}

func mdServer(t *testing.T) *Server {
	t.Helper()
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", MDAddr: "127.0.0.1:0",
		Symbol: "X", Incarnation: "INC1",
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

// assertBookMatchesEngine compares a subscriber's view against the venue's.
func assertBookMatchesEngine(t *testing.T, b *book, srv *Server, when string) {
	t.Helper()
	snap := srv.runner.Snapshot(1 << 20)
	wantBids := map[int64]int64{}
	for _, l := range snap.Bids {
		if l.Quantity > 0 {
			wantBids[l.Price] = l.Quantity
		}
	}
	wantAsks := map[int64]int64{}
	for _, l := range snap.Asks {
		if l.Quantity > 0 {
			wantAsks[l.Price] = l.Quantity
		}
	}
	for _, p := range []struct {
		name      string
		got, want map[int64]int64
	}{{"bid", b.bids, wantBids}, {"ask", b.asks, wantAsks}} {
		if len(p.got) != len(p.want) {
			t.Fatalf("%s: subscriber has %d %s levels, venue has %d\n sub   %v\n venue %v",
				when, len(p.got), p.name, len(p.want), p.got, p.want)
		}
		for price, q := range p.want {
			if p.got[price] != q {
				t.Fatalf("%s: %s %d — subscriber %d, venue %d", when, p.name, price, p.got[price], q)
			}
		}
	}
}

// TestMarketDataSnapshotThenStream is the core path: join, get a snapshot of what is
// already there, then stay in step as the book moves.
func TestMarketDataSnapshotThenStream(t *testing.T) {
	srv := mdServer(t)

	// Some book before anyone subscribes, so the snapshot has content to carry.
	oe := dial(t, srv)
	oe.mustLogin("alice", "pw1")
	for i := 0; i < 5; i++ {
		oe.enter(string(rune('a'+i)), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, int64(100-i), 5)
		if _, ok := oe.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("order %d not accepted", i)
		}
	}

	sub := dialMD(t, srv)
	sub.subscribe("INC1", 0)

	b := newBook()
	// Read the snapshot through its terminator.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p := sub.read(3 * time.Second)
		if p == nil {
			t.Fatal("no snapshot arrived")
		}
		if b.apply(t, p) == wire.MsgMDSnapshotEnd {
			break
		}
	}
	if b.seq == 0 {
		t.Fatal("snapshot terminator carried no sequence, so the subscriber cannot tell what applies on top")
	}
	assertBookMatchesEngine(t, b, srv, "immediately after the snapshot")

	// Now move the book and confirm the subscriber stays in step.
	oe.enter("f", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 110, 8)
	if _, ok := oe.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("ask not accepted")
	}
	oe.cancel("a")
	if _, ok := oe.awaitType(t, wire.MsgCanceled, 3*time.Second); !ok {
		t.Fatal("cancel not confirmed")
	}
	b.drain(t, sub, 300*time.Millisecond)
	assertBookMatchesEngine(t, b, srv, "after an add and a cancel")
}

// TestMarketDataLateSubscriberCatchesUp — two subscribers joining at different times
// must converge on the same book. That is the whole point of snapshot-plus-delta.
func TestMarketDataLateSubscriberCatchesUp(t *testing.T) {
	srv := mdServer(t)
	oe := dial(t, srv)
	oe.mustLogin("alice", "pw1")

	early := dialMD(t, srv)
	early.subscribe("INC1", 0)
	earlyBook := newBook()
	earlyBook.drain(t, early, 300*time.Millisecond)

	for i := 0; i < 8; i++ {
		oe.enter(string(rune('a'+i)), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, int64(100-i), 3)
		if _, ok := oe.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("order %d not accepted", i)
		}
	}

	late := dialMD(t, srv)
	late.subscribe("INC1", 0)
	lateBook := newBook()
	lateBook.drain(t, late, 300*time.Millisecond)
	earlyBook.drain(t, early, 300*time.Millisecond)

	assertBookMatchesEngine(t, lateBook, srv, "late subscriber")
	assertBookMatchesEngine(t, earlyBook, srv, "early subscriber")
}

// TestMarketDataResumeSkipsTheSnapshot — a subscriber that already holds a cursor is
// sent what it missed and nothing else. Re-sending a snapshot would be correct but
// wasteful, and quietly re-sending one it did not ask for would leave it unable to
// tell a resume from a restart.
func TestMarketDataResumeSkipsTheSnapshot(t *testing.T) {
	srv := mdServer(t)
	oe := dial(t, srv)
	oe.mustLogin("alice", "pw1")
	oe.enter("a", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 5)
	if _, ok := oe.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}

	first := dialMD(t, srv)
	first.subscribe("INC1", 0)
	b := newBook()
	b.drain(t, first, 300*time.Millisecond)
	cursor := b.seq
	if cursor == 0 {
		t.Fatal("no sequence established")
	}

	// More activity while "disconnected".
	oe.enter("b", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 99, 7)
	if _, ok := oe.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("second order not accepted")
	}

	resumed := dialMD(t, srv)
	resumed.subscribe("INC1", cursor)
	// Reuse the book we already have; a resume must carry no snapshot.
	for {
		p := resumed.read(500 * time.Millisecond)
		if p == nil {
			break
		}
		if k := b.apply(t, p); k == wire.MsgMDLevel || k == wire.MsgMDSnapshotEnd {
			t.Fatalf("a resume sent snapshot message %q; the subscriber asked for a gap-fill", k)
		}
	}
	assertBookMatchesEngine(t, b, srv, "after resuming from a cursor")
}

// TestMarketDataRejectsAnotherIncarnation — sequence numbers mean nothing across a
// restart, and serving different content under numbers a subscriber believes it holds
// is invisible to both sides.
func TestMarketDataRejectsAnotherIncarnation(t *testing.T) {
	srv := mdServer(t)
	sub := dialMD(t, srv)
	sub.subscribe("SOME-OTHER", 5)

	p := sub.read(3 * time.Second)
	if p == nil {
		t.Fatal("no reject arrived")
	}
	rej, err := wire.DecodeMDReject(p)
	if err != nil {
		t.Fatalf("DecodeMDReject: %v", err)
	}
	if rej.Reason != wire.MDRejectWrongIncarnation {
		t.Errorf("reason = %q, want wrong-incarnation", rej.Reason)
	}
}

// TestMarketDataRejectsAnEvictedCursor — an evicted subscriber is told, not quietly
// restarted, so it knows its own picture had a hole in it.
func TestMarketDataRejectsAnEvictedCursor(t *testing.T) {
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", MDAddr: "127.0.0.1:0",
		Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
		MDRetain: 8, // deliberately tiny
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)

	oe := dial(t, srv)
	oe.mustLogin("alice", "pw1")
	for i := 0; i < 40; i++ {
		oe.enter(string(rune('a'+i%20))+string(rune('0'+i/20)), wire.SideBuy,
			wire.TypeLimit, wire.TIFGoodTillCancel, int64(100-i%15), 2)
		if _, ok := oe.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("order %d not accepted", i)
		}
	}

	sub := dialMD(t, srv)
	sub.subscribe("INC1", 1) // long evicted
	p := sub.read(3 * time.Second)
	if p == nil {
		t.Fatal("no reject arrived")
	}
	rej, err := wire.DecodeMDReject(p)
	if err != nil {
		t.Fatalf("DecodeMDReject: %v", err)
	}
	if rej.Reason != wire.MDRejectEvicted {
		t.Errorf("reason = %q, want evicted", rej.Reason)
	}
}

// TestMarketDataPublishesTradesAndHalts — depth alone is not a feed. A subscriber
// needs the prints and the venue state, in the same ordered stream.
func TestMarketDataPublishesTradesAndHalts(t *testing.T) {
	srv := mdServer(t)
	sub := dialMD(t, srv)
	sub.subscribe("INC1", 0)
	b := newBook()
	b.drain(t, sub, 300*time.Millisecond)

	mm := dial(t, srv)
	mm.mustLogin("alice", "pw1")
	mm.enter("m", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := mm.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("maker not accepted")
	}
	tk := dial(t, srv)
	tk.mustLogin("bob", "pw2")
	tk.enter("t", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 4)
	if _, ok := tk.awaitType(t, wire.MsgExecuted, 3*time.Second); !ok {
		t.Fatal("no fill")
	}
	srv.runner.Halt()

	var sawTrade, sawHalt bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p := sub.read(400 * time.Millisecond)
		if p == nil {
			break
		}
		switch b.apply(t, p) {
		case wire.MsgMDTrade:
			sawTrade = true
		case wire.MsgMDStatus:
			st, _ := wire.DecodeMDStatus(p)
			if st.State == wire.MDStateHalted {
				sawHalt = true
			}
		}
	}
	if !sawTrade {
		t.Error("no trade print reached the subscriber")
	}
	if !sawHalt {
		t.Error("the venue halted and the feed did not say so")
	}
	if b.lastTrade != 100 {
		t.Errorf("subscriber last trade %d, want 100", b.lastTrade)
	}
}

// TestMarketDataSnapshotAfterRecovery — a feed starting empty against a recovered book
// would show only what changed since the restart, which is almost nothing.
func TestMarketDataSnapshotAfterRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Addr: "127.0.0.1:0", MDAddr: "127.0.0.1:0",
		Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
		WALPath: filepath.Join(dir, "obgw.wal"),
	}
	srv := mustServer(t, cfg)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()

	oe := dial(t, srv)
	oe.mustLogin("alice", "pw1")
	for i := 0; i < 4; i++ {
		oe.enter(string(rune('a'+i)), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, int64(100-i), 6)
		if _, ok := oe.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("order %d not accepted", i)
		}
	}
	srv.Close()

	cfg.Addr, cfg.MDAddr = "127.0.0.1:0", "127.0.0.1:0"
	revived := mustServer(t, cfg)
	if err := revived.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = revived.Serve() }()
	defer revived.Close()

	sub := dialMD(t, revived)
	sub.subscribe("INC1", 0)
	b := newBook()
	b.drain(t, sub, 500*time.Millisecond)

	if len(b.bids) == 0 {
		t.Fatal("the snapshot after recovery is empty; the feed was not seeded from the recovered book")
	}
	assertBookMatchesEngine(t, b, revived, "first snapshot after a restart")
}
