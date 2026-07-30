package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// The engine has supported stop, stop-limit, OCO, iceberg, pegged and trailing
// orders since v0.5.0, and the wire could express two of the six. Four order types
// were reachable only by an embedder calling the engine in-process — the same shape
// of gap as Reduce before v0.12.0.

func (c *client) base(clOrdID string, side, typ uint8, price, qty int64) wire.BaseOrder {
	return wire.BaseOrder{
		ClOrdID: clOrdID, Symbol: "X", Side: side, Type: typ,
		TIF: wire.TIFGoodTillCancel, Price: price, Quantity: qty,
	}
}

func (c *client) sendPayload(b []byte, err error, what string) {
	c.t.Helper()
	if err != nil {
		c.t.Fatalf("encode %s: %v", what, err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, b); err != nil {
		c.t.Fatalf("send %s: %v", what, err)
	}
}

func (c *client) enterStop(clOrdID string, side uint8, stopPrice, limitPrice, qty int64) {
	c.t.Helper()
	typ := wire.TypeMarket
	if limitPrice != 0 {
		typ = wire.TypeLimit
	}
	b, err := wire.EncodeEnterStop(nil, wire.EnterStop{
		Version: wire.Version, Order: c.base(clOrdID, side, typ, limitPrice, qty), StopPrice: stopPrice,
	})
	c.sendPayload(b, err, "enter stop")
}

func (c *client) enterIceberg(clOrdID string, side uint8, price, total, display int64) {
	c.t.Helper()
	b, err := wire.EncodeEnterIceberg(nil, wire.EnterIceberg{
		Version: wire.Version, Order: c.base(clOrdID, side, wire.TypeLimit, price, total), DisplayQty: display,
	})
	c.sendPayload(b, err, "enter iceberg")
}

func (c *client) enterPegged(clOrdID string, side, ref uint8, offset, qty int64) {
	c.t.Helper()
	b, err := wire.EncodeEnterPegged(nil, wire.EnterPegged{
		Version: wire.Version, Order: c.base(clOrdID, side, wire.TypeLimit, 0, qty), Ref: ref, Offset: offset,
	})
	c.sendPayload(b, err, "enter pegged")
}

func (c *client) enterTrailing(clOrdID string, side uint8, trail, qty int64) {
	c.t.Helper()
	b, err := wire.EncodeEnterTrailing(nil, wire.EnterTrailing{
		Version: wire.Version, Order: c.base(clOrdID, side, wire.TypeMarket, 0, qty), Trail: trail,
	})
	c.sendPayload(b, err, "enter trailing")
}

func (c *client) enterOCO(primaryID, stopID string, side uint8, primaryPrice, stopPrice, stopLimit, qty int64) {
	c.t.Helper()
	b, err := wire.EncodeEnterOCO(nil, wire.EnterOCO{
		Version:     wire.Version,
		Primary:     c.base(primaryID, side, wire.TypeLimit, primaryPrice, qty),
		StopClOrdID: stopID, StopPrice: stopPrice, StopLimitPrice: stopLimit,
	})
	c.sendPayload(b, err, "enter oco")
}

// TestStopOverTheWire — a stop rests off-book and fires when the market reaches it.
func TestStopOverTheWire(t *testing.T) {
	srv := threeAccountServer(t)

	mm := dial(t, srv)
	mm.mustLogin("alice", "pw1")
	// Liquidity for the stop to hit once it fires.
	mm.enter("bid", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 95, 10)
	if _, ok := mm.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("maker bid not accepted")
	}

	c := dial(t, srv)
	c.mustLogin("bob", "pw2")
	c.enterStop("st1", wire.SideSell, 100, 0, 5) // stop-market sell, triggers at 100

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.runner.PendingStopCount() == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := srv.runner.PendingStopCount(); got != 1 {
		t.Fatalf("pending stops = %d, want 1 — the stop never reached the engine", got)
	}

	// Print a trade to arm it: carol sells into alice's bid at 95, which is at or
	// below the 100 trigger for a sell stop.
	carol := dial(t, srv)
	carol.mustLogin("carol", "pw3")
	carol.enter("mk", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 95, 1)

	// The stop fires and takes the rest of alice's bid.
	raw, ok := c.awaitType(t, wire.MsgExecuted, 5*time.Second)
	if !ok {
		t.Fatal("the stop never fired after a trade reached its trigger")
	}
	ex, err := wire.DecodeExecuted(raw)
	if err != nil {
		t.Fatalf("DecodeExecuted: %v", err)
	}
	if ex.ClOrdID != "st1" {
		t.Errorf("fill names %q, want st1", ex.ClOrdID)
	}
	if got := srv.runner.PendingStopCount(); got != 0 {
		t.Errorf("%d stops still pending after firing, want 0", got)
	}
}

// TestStopRequiresAPositiveTrigger — zero would mean "fire on arrival", which is a
// market order, and the client should have to say so.
func TestStopRequiresAPositiveTrigger(t *testing.T) {
	srv := testServer(t)
	_ = srv
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enterStop("bad", wire.SideSell, 0, 0, 5)

	raw, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second)
	if !ok {
		t.Fatal("a stop with no trigger price was accepted")
	}
	rej, _ := wire.DecodeCmdReject(raw)
	if rej.ClOrdID != "bad" {
		t.Errorf("rejection names %q, want bad", rej.ClOrdID)
	}
}

// TestIcebergShowsOnlyASlice is the point of an iceberg: the book must not display
// the reserve.
func TestIcebergShowsOnlyASlice(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enterIceberg("ice", wire.SideBuy, 100, 100, 10)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, qty, ok := srv.runner.BestBid(); ok && qty > 0 {
			if qty != 10 {
				t.Errorf("book displays %d lots, want the 10-lot slice — the reserve is visible", qty)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the iceberg never reached the book")
}

// TestIcebergRejectsAnImpossibleSlice — a display larger than the total, or zero.
func TestIcebergRejectsAnImpossibleSlice(t *testing.T) {
	srv := testServer(t)
	_ = srv
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	for _, display := range []int64{0, 500} {
		c.enterIceberg("ice", wire.SideBuy, 100, 100, display)
		raw, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second)
		if !ok {
			t.Fatalf("display %d was accepted", display)
		}
		rej, _ := wire.DecodeCmdReject(raw)
		if rej.Reason != wire.ReasonInvalidQuantity {
			t.Errorf("display %d: reason = %d, want ReasonInvalidQuantity", display, rej.Reason)
		}
	}
}

// TestPeggedTracksTheReference — the peg computes the price, so the resting order
// must not sit at the zero the client sent.
func TestPeggedTracksTheReference(t *testing.T) {
	srv := testServer(t)

	mm := dial(t, srv)
	mm.mustLogin("bob", "pw2")
	mm.enter("ref", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := mm.await(t, wire.AcceptedLen, 3*time.Second); !ok {
		t.Fatal("reference bid not accepted")
	}

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enterPegged("peg", wire.SideBuy, wire.PegBid, -1, 5) // one tick inside the bid

	raw, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second)
	if !ok {
		t.Fatal("the pegged order was never accepted")
	}
	acc, err := wire.DecodeAccepted(raw)
	if err != nil {
		t.Fatalf("DecodeAccepted: %v", err)
	}
	if acc.Price == 0 {
		t.Error("the pegged order rested at price 0 — the peg did not compute a price")
	}
}

// TestPeggedRefusesAClientPrice — the peg owns the price. Silently overwriting a
// client-supplied one would leave the client believing it set something it did not.
func TestPeggedRefusesAClientPrice(t *testing.T) {
	srv := testServer(t)
	_ = srv
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	b, err := wire.EncodeEnterPegged(nil, wire.EnterPegged{
		Version: wire.Version,
		Order:   c.base("peg", wire.SideBuy, wire.TypeLimit, 100, 5), // price set — not allowed
		Ref:     wire.PegBid, Offset: 0,
	})
	c.sendPayload(b, err, "enter pegged")

	if _, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second); !ok {
		t.Error("a pegged order carrying its own price was accepted")
	}
}

// TestPeggedRejectsAnUnknownReference — an unrecognised peg byte must not fall
// through to a default reference the client did not ask for.
func TestPeggedRejectsAnUnknownReference(t *testing.T) {
	srv := testServer(t)
	_ = srv
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	b, err := wire.EncodeEnterPegged(nil, wire.EnterPegged{
		Version: wire.Version, Order: c.base("peg", wire.SideBuy, wire.TypeLimit, 0, 5),
		Ref: 'Z', Offset: 0,
	})
	c.sendPayload(b, err, "enter pegged")
	if _, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second); !ok {
		t.Error("an unknown peg reference was accepted")
	}
}

// TestTrailingStopReachesTheEngine.
func TestTrailingStopReachesTheEngine(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enterTrailing("trail", wire.SideSell, 5, 10)

	// Trailing stops are held apart from the stop book: there is no fixed trigger
	// price to key on until the market has moved.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.runner.TrailingStopCount() >= 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Error("the trailing stop never reached the engine")
}

// TestTrailingStopRequiresAPositiveTrail.
func TestTrailingStopRequiresAPositiveTrail(t *testing.T) {
	srv := testServer(t)
	_ = srv
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enterTrailing("bad", wire.SideSell, 0, 10)
	if _, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second); !ok {
		t.Error("a trailing stop with no trail distance was accepted")
	}
}

// TestOCOPlacesBothLegs — the primary rests on the book and the stop leg rests off
// it, which is what makes the pair an either-or.
func TestOCOPlacesBothLegs(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enterOCO("tp", "sl", wire.SideSell, 110, 90, 0, 5)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.runner.OrderCount() == 1 && srv.runner.PendingStopCount() == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("book has %d resting and %d pending stops, want 1 and 1",
		srv.runner.OrderCount(), srv.runner.PendingStopCount())
}

// TestConditionalOrdersAreDurable is the one that matters most. These commands were
// never written to the log — reachable only in-process, so the gap was invisible.
// Putting them on the wire without this would ship a client-facing feature whose
// state silently does not survive a restart.
func TestConditionalOrdersAreDurable(t *testing.T) {
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
	c.enterStop("st", wire.SideSell, 90, 0, 5)
	c.enterIceberg("ice", wire.SideBuy, 100, 100, 10)
	c.enterTrailing("tr", wire.SideSell, 5, 7)
	c.enterOCO("tp", "sl", wire.SideSell, 130, 80, 0, 3)

	// Two in the stop book (the plain stop and the OCO's stop leg), one trailing in
	// its own map, and two resting (the iceberg slice and the OCO primary).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.runner.PendingStopCount() == 2 && srv.runner.TrailingStopCount() == 1 && srv.runner.OrderCount() == 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stops, trailing, resting := srv.runner.PendingStopCount(), srv.runner.TrailingStopCount(), srv.runner.OrderCount()
	if stops != 2 || trailing != 1 || resting != 2 {
		t.Fatalf("before restart: %d pending stops, %d trailing, %d resting; want 2, 1, 2", stops, trailing, resting)
	}
	srv.Close()

	cfg.Addr = "127.0.0.1:0"
	revived := mustServer(t, cfg)
	defer revived.Close()
	if got := revived.runner.PendingStopCount(); got != stops {
		t.Errorf("after restart: %d pending stops, want %d — conditional orders were not journalled", got, stops)
	}
	if got := revived.runner.TrailingStopCount(); got != trailing {
		t.Errorf("after restart: %d trailing stops, want %d", got, trailing)
	}
	if got := revived.runner.OrderCount(); got != resting {
		t.Errorf("after restart: %d resting orders, want %d", got, resting)
	}
	// The iceberg must come back showing a slice, not its reserve.
	if _, qty, ok := revived.runner.BestBid(); !ok || qty != 10 {
		t.Errorf("recovered iceberg displays %d lots (ok=%v), want the 10-lot slice", qty, ok)
	}
}

// TestConditionalEntryIsThrottled — a path that skipped the admission gate would be
// a way around the venue's own rate limit.
func TestConditionalEntryIsThrottled(t *testing.T) {
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 256, StreamRing: 4096,
		RatePerSec: 1, Burst: 2, // effectively closed after two
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	for i := 0; i < 12; i++ {
		c.enterStop("s"+string(rune('a'+i)), wire.SideSell, int64(90+i), 0, 1)
	}
	raw, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second)
	if !ok {
		t.Fatal("conditional entry bypassed the rate limit entirely")
	}
	rej, _ := wire.DecodeCmdReject(raw)
	if rej.Reason != wire.ReasonThrottled {
		t.Errorf("reason = %d, want ReasonThrottled", rej.Reason)
	}
}

// TestConditionalOrdersRefuseAnotherSymbol — the same guard Enter has. Booking an
// order that named a different instrument was a real bug in v0.10.0.
func TestConditionalOrdersRefuseAnotherSymbol(t *testing.T) {
	srv := testServer(t)
	_ = srv
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	bad := c.base("wrong", wire.SideSell, wire.TypeMarket, 0, 5)
	bad.Symbol = "SOMETHING-ELSE"
	b, err := wire.EncodeEnterStop(nil, wire.EnterStop{Version: wire.Version, Order: bad, StopPrice: 100})
	c.sendPayload(b, err, "enter stop")

	raw, ok := c.awaitType(t, wire.MsgCmdReject, 3*time.Second)
	if !ok {
		t.Fatal("a stop naming another instrument was accepted")
	}
	rej, _ := wire.DecodeCmdReject(raw)
	if rej.Reason != wire.ReasonMalformed {
		t.Errorf("reason = %d, want ReasonMalformed", rej.Reason)
	}
}
