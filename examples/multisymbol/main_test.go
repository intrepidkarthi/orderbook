// Deliverable #5 of docs/MULTI-SYMBOL.md, proven over real sockets: two
// subscribers on two symbols each reconstruct their own book, and neither can be
// served the other's by accident.
package main

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/marketdata"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func newVenue(t *testing.T, symbols ...string) *Venue {
	t.Helper()
	v, err := NewVenue(VenueConfig{
		Symbols: symbols, Dir: t.TempDir(),
		Incarnation: "INC1", MDAddr: "127.0.0.1:0", QueueSize: 1024,
	})
	if err != nil {
		t.Fatalf("NewVenue: %v", err)
	}
	t.Cleanup(v.Close)
	return v
}

func place(t *testing.T, v *Venue, sym, user string, side types.Side, price, qty int64) *matching.MatchResult {
	t.Helper()
	o, err := types.NewOrder(user, sym, side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	r := v.Runner(sym)
	if r == nil {
		t.Fatalf("no shard for %s", sym)
	}
	return r.Process(o)
}

// sub is a market-data subscriber that keeps its own book, the way a real one
// would — and, like a real one, is told the symbol exactly once.
type sub struct {
	t    *testing.T
	conn net.Conn
	buf  []byte
	bids map[int64]int64
	asks map[int64]int64
	seq  uint64
}

func dialSub(t *testing.T, v *Venue, symbol string) *sub {
	t.Helper()
	conn, err := net.Dial("tcp", v.MDAddr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s := &sub{t: t, conn: conn, buf: make([]byte, wire.MaxPayload),
		bids: map[int64]int64{}, asks: map[int64]int64{}}

	b, err := wire.EncodeMDSubscribe(nil, wire.MDSubscribe{
		Version: wire.Version, Incarnation: "INC1", Seq: 0, Symbol: symbol,
	})
	if err != nil {
		t.Fatalf("EncodeMDSubscribe: %v", err)
	}
	if err := wire.WritePacket(conn, wire.PacketUnsequenced, b); err != nil {
		t.Fatalf("send subscribe: %v", err)
	}
	return s
}

// read returns the next payload, or nil on timeout.
func (s *sub) read(timeout time.Duration) []byte {
	_ = s.conn.SetReadDeadline(time.Now().Add(timeout))
	pkt, err := wire.ReadPacket(s.conn, s.buf)
	if err != nil {
		return nil
	}
	out := make([]byte, len(pkt.Payload))
	copy(out, pkt.Payload)
	return out
}

func (s *sub) side(b uint8) map[int64]int64 {
	if b == wire.SideSell {
		return s.asks
	}
	return s.bids
}

// apply folds one message in, returning its type.
func (s *sub) apply(p []byte) uint8 {
	s.t.Helper()
	if len(p) < 2 {
		return 0
	}
	switch p[0] {
	case wire.MsgMDLevel:
		l, err := wire.DecodeMDLevel(p)
		if err != nil {
			s.t.Fatalf("DecodeMDLevel: %v", err)
		}
		s.side(l.Side)[l.Price] = l.Qty
	case wire.MsgMDSnapshotEnd:
		e, err := wire.DecodeMDSnapshotEnd(p)
		if err != nil {
			s.t.Fatalf("DecodeMDSnapshotEnd: %v", err)
		}
		s.seq = e.Seq
	case wire.MsgMDDelta:
		d, err := wire.DecodeMDDelta(p)
		if err != nil {
			s.t.Fatalf("DecodeMDDelta: %v", err)
		}
		if d.Qty == 0 {
			delete(s.side(d.Side), d.Price)
		} else {
			s.side(d.Side)[d.Price] = d.Qty
		}
		s.seq = d.Seq
	case wire.MsgMDTrade:
		tr, err := wire.DecodeMDTrade(p)
		if err != nil {
			s.t.Fatalf("DecodeMDTrade: %v", err)
		}
		s.seq = tr.Seq
	}
	return p[0]
}

// drainSnapshot reads through the snapshot terminator.
func (s *sub) drainSnapshot() {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p := s.read(3 * time.Second)
		if p == nil {
			s.t.Fatal("no snapshot arrived")
		}
		if s.apply(p) == wire.MsgMDSnapshotEnd {
			return
		}
	}
	s.t.Fatal("snapshot never terminated")
}

// drain reads until nothing arrives for quiet.
func (s *sub) drain(quiet time.Duration) {
	for {
		p := s.read(quiet)
		if p == nil {
			return
		}
		s.apply(p)
	}
}

func assertMatchesBook(t *testing.T, s *sub, v *Venue, symbol, when string) {
	t.Helper()
	snap := v.Runner(symbol).Snapshot(1 << 20)
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
	}{{"bid", s.bids, wantBids}, {"ask", s.asks, wantAsks}} {
		if len(p.got) != len(p.want) {
			t.Fatalf("%s %s: subscriber has %d %s levels, venue has %d\n sub   %v\n venue %v",
				symbol, when, len(p.got), p.name, len(p.want), p.got, p.want)
		}
		for price, q := range p.want {
			if p.got[price] != q {
				t.Errorf("%s %s: %s %d — subscriber %d, venue %d", symbol, when, p.name, price, p.got[price], q)
			}
		}
	}
}

// TestTwoSubscribersTwoSymbols is the deliverable. Each subscriber names its
// instrument once, and everything after that belongs to that instrument — which is
// what lets every other market-data payload stay symbol-free.
func TestTwoSubscribersTwoSymbols(t *testing.T) {
	v := newVenue(t, "BTC-USD", "ETH-USD")

	// Distinct books, so a subscriber served the wrong one would be obvious.
	place(t, v, "BTC-USD", "mm", types.SideBuy, 30000, 5)
	place(t, v, "BTC-USD", "mm", types.SideSell, 30010, 4)
	place(t, v, "ETH-USD", "mm", types.SideBuy, 2000, 11)
	place(t, v, "ETH-USD", "mm", types.SideSell, 2005, 9)

	btc := dialSub(t, v, "BTC-USD")
	eth := dialSub(t, v, "ETH-USD")
	btc.drainSnapshot()
	eth.drainSnapshot()

	assertMatchesBook(t, btc, v, "BTC-USD", "after the snapshot")
	assertMatchesBook(t, eth, v, "ETH-USD", "after the snapshot")

	// Neither subscriber has a single level of the other's book.
	if _, leaked := btc.bids[2000]; leaked {
		t.Error("the BTC subscriber received an ETH level")
	}
	if _, leaked := eth.bids[30000]; leaked {
		t.Error("the ETH subscriber received a BTC level")
	}

	// Move both books; each subscriber stays in step with its own.
	place(t, v, "BTC-USD", "mm2", types.SideBuy, 29990, 3)
	place(t, v, "ETH-USD", "t", types.SideBuy, 2005, 9) // takes the ETH ask
	place(t, v, "ETH-USD", "mm2", types.SideSell, 2010, 2)

	btc.drain(400 * time.Millisecond)
	eth.drain(400 * time.Millisecond)
	assertMatchesBook(t, btc, v, "BTC-USD", "after both books moved")
	assertMatchesBook(t, eth, v, "ETH-USD", "after both books moved")
}

// TestUnknownSymbolIsRefused — a subscriber cannot detect being served the wrong
// book for itself, because every message after the subscribe is symbol-free. So
// the venue refuses rather than guesses.
func TestUnknownSymbolIsRefused(t *testing.T) {
	v := newVenue(t, "BTC-USD")

	s := dialSub(t, v, "DOGE-USD")
	p := s.read(3 * time.Second)
	if p == nil {
		t.Fatal("no response to a subscription for an unserved symbol")
	}
	if p[0] != wire.MsgMDReject {
		t.Fatalf("response type %q, want MDReject", rune(p[0]))
	}
	r, err := wire.DecodeMDReject(p)
	if err != nil {
		t.Fatalf("DecodeMDReject: %v", err)
	}
	if r.Reason != wire.MDRejectUnknownSymbol {
		t.Errorf("reject reason %q, want MDRejectUnknownSymbol", rune(r.Reason))
	}
}

// TestVenueIDsArePartitionedAndRoutable ties the wire back to §4.1: the ids a
// subscriber and a client see are unique venue-wide, and an id routes to its own
// book without a lookup.
func TestVenueIDsArePartitionedAndRoutable(t *testing.T) {
	v := newVenue(t, "BTC-USD", "ETH-USD")

	seen := map[int64]string{}
	for _, sym := range []string{"BTC-USD", "ETH-USD"} {
		for i := 0; i < 5; i++ {
			res := place(t, v, sym, "mm", types.SideBuy, 100+int64(i), 1)
			id := res.Order.ID
			if prev, dup := seen[id]; dup {
				t.Errorf("id %d issued by both %s and %s", id, prev, sym)
			}
			seen[id] = sym
			if got := v.RunnerFor(id); got != v.Runner(sym) {
				t.Errorf("%s order %d did not route to its own shard", sym, id)
			}
		}
	}
	if len(seen) != 10 {
		t.Fatalf("expected 10 distinct ids, got %d", len(seen))
	}
}

// TestVenueSurvivesRestart — the whole point of ShardsConfig.NewLog. Every book
// comes back, from its own log, with its own shard index.
func TestVenueSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	symbols := []string{"BTC-USD", "ETH-USD"}

	v, err := NewVenue(VenueConfig{Symbols: symbols, Dir: dir, Incarnation: "INC1"})
	if err != nil {
		t.Fatalf("NewVenue: %v", err)
	}
	place(t, v, "BTC-USD", "mm", types.SideBuy, 30000, 5)
	place(t, v, "BTC-USD", "mm", types.SideSell, 30010, 4)
	place(t, v, "ETH-USD", "mm", types.SideBuy, 2000, 11)

	want := map[string]string{}
	for _, sym := range symbols {
		snap, err := v.Runner(sym).Checkpoint()
		if err != nil {
			t.Fatalf("Checkpoint(%s): %v", sym, err)
		}
		want[sym] = snap.Digest()
	}
	v.Close()

	// A fresh venue over the same directory. Nothing is passed between the two but
	// the files.
	again, err := NewVenue(VenueConfig{Symbols: symbols, Dir: dir, Incarnation: "INC2"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	for _, sym := range symbols {
		idx, err := again.ShardIndex(sym)
		if err != nil {
			t.Fatalf("ShardIndex(%s): %v", sym, err)
		}
		cfg := matching.DefaultConfig(sym)
		cfg.DedupClientOrderIDs = 4096
		cfg.ShardIndex = idx
		// The recovery config carries a sink because the LIVE engine had one, and
		// the digest covers the event counter. An engine replayed with no sink
		// emits nothing, so its EventSeq stays at zero and it compares unequal to
		// the venue it is supposed to have reproduced — a difference in what was
		// published, not in the book. Real recovery attaches the sink after replay
		// for exactly that reason (see cmd/obgw); here the comparison is against a
		// live engine, so the arms have to match.
		cfg.EventSink = marketdata.NewFeed("REPLAY", 1<<16)
		eng, err := recoverSymbol(cfg, filepath.Join(dir, sym+".wal"))
		if err != nil {
			t.Fatalf("recover(%s): %v", sym, err)
		}
		if got := eng.TakeSnapshot().Digest(); got != want[sym] {
			t.Errorf("%s did not come back\n got %s\nwant %s", sym, got, want[sym])
		}
	}
}
