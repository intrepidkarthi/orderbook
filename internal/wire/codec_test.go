package wire

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- round trips ---

func TestEnterRoundTrip(t *testing.T) {
	want := Enter{
		Version: Version, ClOrdID: "cl-1", Symbol: "BTC-USD",
		Side: SideBuy, Type: TypeLimit, TIF: TIFGoodTillCancel,
		PostOnly: true, Price: 30000, Quantity: 250,
	}
	b, err := EncodeEnter(nil, want)
	if err != nil {
		t.Fatalf("EncodeEnter: %v", err)
	}
	if len(b) != EnterLen {
		t.Fatalf("encoded %d bytes, want %d", len(b), EnterLen)
	}
	got, err := DecodeEnter(b)
	if err != nil {
		t.Fatalf("DecodeEnter: %v", err)
	}
	if got != want {
		t.Errorf("round trip\n got %+v\nwant %+v", got, want)
	}
}

func TestOutboundRoundTrips(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		want := Accepted{Version: Version, ClOrdID: "a-1", Price: 100, Quantity: 5, Side: SideSell}
		b, err := EncodeAccepted(nil, want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeAccepted(b)
		if err != nil || got != want {
			t.Errorf("got %+v err %v, want %+v", got, err, want)
		}
	})
	t.Run("executed", func(t *testing.T) {
		want := Executed{Version: Version, ClOrdID: "x-1", Price: 100, Quantity: 3, LeavesQty: 2, Aggressor: SideBuy}
		b, err := EncodeExecuted(nil, want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeExecuted(b)
		if err != nil || got != want {
			t.Errorf("got %+v err %v, want %+v", got, err, want)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		want := Rejected{Version: Version, ClOrdID: "r-1", Reason: ReasonPriceBand}
		b, err := EncodeRejected(nil, want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeRejected(b)
		if err != nil || got != want {
			t.Errorf("got %+v err %v, want %+v", got, err, want)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		want := Canceled{Version: Version, ClOrdID: "d-1", Reason: ReasonSelfTrade}
		b, err := EncodeCanceled(nil, want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeCanceled(b)
		if err != nil || got != want {
			t.Errorf("got %+v err %v, want %+v", got, err, want)
		}
	})
	t.Run("replaced", func(t *testing.T) {
		want := Replaced{Version: Version, ClOrdID: "p-1", LeavesQty: 7}
		b, err := EncodeReplaced(nil, want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeReplaced(b)
		if err != nil || got != want {
			t.Errorf("got %+v err %v, want %+v", got, err, want)
		}
	})
	t.Run("cmdreject", func(t *testing.T) {
		want := CmdReject{Version: Version, ClOrdID: "k-1", Reason: ReasonThrottled}
		b, err := EncodeCmdReject(nil, want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeCmdReject(b)
		if err != nil || got != want {
			t.Errorf("got %+v err %v, want %+v", got, err, want)
		}
	})
}

// TestOverlongFieldIsAnError — a truncated ClOrdID would silently collide with
// another of the client's orders, so it must fail loudly at the boundary.
func TestOverlongFieldIsAnError(t *testing.T) {
	_, err := EncodeEnter(nil, Enter{ClOrdID: strings.Repeat("x", ClOrdIDLen+1), Symbol: "X"})
	if !errors.Is(err, ErrTooLong) {
		t.Errorf("over-long ClOrdID: err = %v, want ErrTooLong", err)
	}
	_, err = EncodeEnter(nil, Enter{ClOrdID: "ok", Symbol: strings.Repeat("y", SymbolLen+1)})
	if !errors.Is(err, ErrTooLong) {
		t.Errorf("over-long Symbol: err = %v, want ErrTooLong", err)
	}
}

// TestShortBufferIsAnError — every decoder must bounds-check, since the input is
// whatever a client sent.
func TestShortBufferIsAnError(t *testing.T) {
	cases := map[string]func([]byte) error{
		"enter":     func(b []byte) error { _, err := DecodeEnter(b); return err },
		"cancel":    func(b []byte) error { _, err := DecodeCancel(b); return err },
		"accepted":  func(b []byte) error { _, err := DecodeAccepted(b); return err },
		"executed":  func(b []byte) error { _, err := DecodeExecuted(b); return err },
		"rejected":  func(b []byte) error { _, err := DecodeRejected(b); return err },
		"canceled":  func(b []byte) error { _, err := DecodeCanceled(b); return err },
		"replaced":  func(b []byte) error { _, err := DecodeReplaced(b); return err },
		"reduce":    func(b []byte) error { _, err := DecodeReduce(b); return err },
		"cmdreject": func(b []byte) error { _, err := DecodeCmdReject(b); return err },
		"login":     func(b []byte) error { _, err := DecodeLoginRequest(b); return err },
		"query":     func(b []byte) error { _, err := DecodeQuery(b); return err },
		"openorder": func(b []byte) error { _, err := DecodeOpenOrder(b); return err },
		"queryend":  func(b []byte) error { _, err := DecodeQueryEnd(b); return err },
	}
	// 0 and 1 bytes are shorter than every message, including the 2-byte Query.
	// Larger sizes are not universally "short" — a 5-byte buffer is long enough
	// for a Query and fails the type check instead, which is the header doing its
	// job rather than a bounds failure.
	for name, dec := range cases {
		for _, n := range []int{0, 1} {
			if err := dec(make([]byte, n)); !errors.Is(err, ErrShort) {
				t.Errorf("%s with %d bytes: err = %v, want ErrShort", name, n, err)
			}
		}
	}
}

// TestWrongMessageTypeIsRefused — a payload long enough but carrying another
// message's type must not be decoded as this one. Before the type byte existed,
// two messages of equal length were indistinguishable.
func TestWrongMessageTypeIsRefused(t *testing.T) {
	// Rejected, Canceled and CmdReject share a layout exactly; only the type
	// separates them, which makes them the sharpest test of it.
	rej, err := EncodeRejected(nil, Rejected{Version: Version, ClOrdID: "r-1", Reason: ReasonPriceBand})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCanceled(rej); !errors.Is(err, ErrBadType) {
		t.Errorf("a Rejected decoded as Canceled: err = %v, want ErrBadType", err)
	}
	if _, err := DecodeCmdReject(rej); !errors.Is(err, ErrBadType) {
		t.Errorf("a Rejected decoded as CmdReject: err = %v, want ErrBadType", err)
	}
	if _, err := DecodeRejected(rej); err != nil {
		t.Errorf("a Rejected failed to decode as itself: %v", err)
	}
}

// TestReduceAndReplacedShareAWidth is the type byte earning its keep a second
// time. Reduce (inbound) and Replaced (outbound) encode to the same number of
// bytes with the same field at the same offset; under v1's length-based dispatch
// one of them could not have been added at all without renaming the other's
// layout. Each must decode as itself and refuse the other.
func TestReduceAndReplacedShareAWidth(t *testing.T) {
	if ReduceLen != ReplacedLen {
		t.Fatalf("ReduceLen %d, ReplacedLen %d — this test is about the case where they match", ReduceLen, ReplacedLen)
	}
	red, err := EncodeReduce(nil, Reduce{Version: Version, ClOrdID: "cl-1", Quantity: 40})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := EncodeReplaced(nil, Replaced{Version: Version, ClOrdID: "cl-1", LeavesQty: 40})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(red, rep) {
		t.Fatal("Reduce and Replaced encoded identically — the type byte is not being written")
	}
	if _, err := DecodeReplaced(red); !errors.Is(err, ErrBadType) {
		t.Errorf("a Reduce decoded as Replaced: err = %v, want ErrBadType", err)
	}
	if _, err := DecodeReduce(rep); !errors.Is(err, ErrBadType) {
		t.Errorf("a Replaced decoded as Reduce: err = %v, want ErrBadType", err)
	}
}

// TestReduceRoundTrip — Quantity is the new total, and it must survive the wire
// exactly: a size that arrived wrong would resize a live order to a number the
// client never sent.
func TestReduceRoundTrip(t *testing.T) {
	// 30 = type(1) + version(1) + ClOrdID(20) + Quantity(8), derived from the
	// field list rather than read back from the constant.
	if ReduceLen != 30 {
		t.Errorf("ReduceLen = %d, want 30", ReduceLen)
	}
	want := Reduce{Version: Version, ClOrdID: "cl-reduce", Quantity: 40}
	b, err := EncodeReduce(nil, want)
	if err != nil {
		t.Fatalf("EncodeReduce: %v", err)
	}
	if len(b) != ReduceLen {
		t.Fatalf("encoded %d bytes, want %d", len(b), ReduceLen)
	}
	got, err := DecodeReduce(b)
	if err != nil {
		t.Fatalf("DecodeReduce: %v", err)
	}
	if got != want {
		t.Errorf("round trip\n got %+v\nwant %+v", got, want)
	}
}

// --- framing ---

func TestPacketRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload, err := EncodeCancel(nil, Cancel{Version: Version, ClOrdID: "c-9"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePacket(&buf, PacketUnsequenced, payload); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	rb := make([]byte, MaxPayload)
	pkt, err := ReadPacket(&buf, rb)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if pkt.Type != PacketUnsequenced {
		t.Errorf("type = %q, want %q", pkt.Type, PacketUnsequenced)
	}
	got, err := DecodeCancel(pkt.Payload)
	if err != nil || got.ClOrdID != "c-9" {
		t.Errorf("payload round trip: %+v err %v", got, err)
	}
}

func TestPacketWithEmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePacket(&buf, PacketClientHeartbt, nil); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	pkt, err := ReadPacket(&buf, make([]byte, 16))
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if pkt.Type != PacketClientHeartbt || len(pkt.Payload) != 0 {
		t.Errorf("got type %q payload %d bytes", pkt.Type, len(pkt.Payload))
	}
}

// TestPacketRejectsHostileLength — a length field is attacker-controlled, so an
// absurd one must be refused rather than allocated.
func TestPacketRejectsHostileLength(t *testing.T) {
	// Declared length 0 is malformed: the type byte is always counted.
	if _, err := ReadPacket(bytes.NewReader([]byte{0x00, 0x00}), make([]byte, 16)); !errors.Is(err, ErrBadPacket) {
		t.Errorf("zero length: err = %v, want ErrBadPacket", err)
	}
	// Declared length beyond MaxPayload must not be honoured.
	big := []byte{0xFF, 0xFF, 'S'}
	if _, err := ReadPacket(bytes.NewReader(big), make([]byte, 16)); err == nil {
		t.Error("oversized declared length was accepted")
	}
	// Truncated body is an error, not a short read treated as success.
	if _, err := ReadPacket(bytes.NewReader([]byte{0x00, 0x10, 'S', 1, 2}), make([]byte, 64)); err == nil {
		t.Error("truncated body was accepted")
	}
}

func TestLoginRoundTrip(t *testing.T) {
	want := LoginRequest{Username: "alice", Password: "s3cret", Session: "INC0000001", Sequence: 42}
	b, err := EncodeLoginRequest(nil, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeLoginRequest(b)
	if err != nil || got != want {
		t.Errorf("got %+v err %v, want %+v", got, err, want)
	}

	wantAcc := LoginAccepted{Session: "INC0000001", Sequence: 42}
	ab, err := EncodeLoginAccepted(nil, wantAcc)
	if err != nil {
		t.Fatal(err)
	}
	gotAcc, err := DecodeLoginAccepted(ab)
	if err != nil || gotAcc != wantAcc {
		t.Errorf("got %+v err %v, want %+v", gotAcc, err, wantAcc)
	}
}

// TestQueryRoundTrips covers the reconciliation messages.
func TestQueryRoundTrips(t *testing.T) {
	q := Query{Version: Version}
	qb, err := EncodeQuery(nil, q)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeQuery(qb); err != nil || got != q {
		t.Errorf("query: got %+v err %v", got, err)
	}

	oo := OpenOrder{Version: Version, ClOrdID: "o-1", Price: 100, LeavesQty: 7, Side: SideSell}
	ob, err := EncodeOpenOrder(nil, oo)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeOpenOrder(ob); err != nil || got != oo {
		t.Errorf("open order: got %+v err %v", got, err)
	}

	qe := QueryEnd{Version: Version, Count: 3, Seq: 99}
	eb, err := EncodeQueryEnd(nil, qe)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeQueryEnd(eb); err != nil || got != qe {
		t.Errorf("query end: got %+v err %v", got, err)
	}
}

// TestNewMessagesDidNotDisturbExistingLayouts is the reason the type byte was
// worth a version bump: adding a message must be additive. If this fails, the new
// types moved something a deployed client already parses.
func TestNewMessagesDidNotDisturbExistingLayouts(t *testing.T) {
	if Version != 2 {
		t.Errorf("Version = %d; adding message types must not require a bump", Version)
	}
	widths := map[string]int{
		"Enter": EnterLen, "Cancel": CancelLen, "Accepted": AcceptedLen,
		"Rejected": RejectedLen, "Executed": ExecutedLen, "Canceled": CanceledLen,
		"Replaced": ReplacedLen, "CmdReject": CmdRejectLen,
	}
	// Derived by hand from the field lists in wire.go, not copied from the
	// constants — a test that reads the value it is checking proves nothing.
	want := map[string]int{
		"Enter": 58, "Cancel": 22, "Accepted": 39,
		"Rejected": 24, "Executed": 47, "Canceled": 24,
		"Replaced": 30, "CmdReject": 24,
	}
	for name, got := range widths {
		if got != want[name] {
			t.Errorf("%s is %d bytes, was %d — an existing layout moved", name, got, want[name])
		}
	}
}

// --- the freeze ---

// goldenCases are byte-exact vectors. Once committed these must never change:
// a field that moves silently reinterprets every message a deployed client sends.
// Changing the layout means bumping Version and adding vectors, not editing these.
func goldenCases(t *testing.T) map[string][]byte {
	t.Helper()
	must := func(b []byte, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	}
	return map[string][]byte{
		"enter_limit_buy": must(EncodeEnter(nil, Enter{
			Version: Version, ClOrdID: "cl-1", Symbol: "BTC-USD",
			Side: SideBuy, Type: TypeLimit, TIF: TIFGoodTillCancel,
			PostOnly: false, Price: 30000, Quantity: 250,
		})),
		"enter_market_sell_ioc": must(EncodeEnter(nil, Enter{
			Version: Version, ClOrdID: "cl-2", Symbol: "BTC-USD",
			Side: SideSell, Type: TypeMarket, TIF: TIFImmediateOrCanc,
			PostOnly: false, Price: 0, Quantity: 7,
		})),
		"enter_postonly": must(EncodeEnter(nil, Enter{
			Version: Version, ClOrdID: "cl-3", Symbol: "ETH-USD",
			Side: SideBuy, Type: TypeLimit, TIF: TIFGoodTillCancel,
			PostOnly: true, Price: 2000, Quantity: 1,
		})),
		"cancel":    must(EncodeCancel(nil, Cancel{Version: Version, ClOrdID: "cl-1"})),
		"reduce":    must(EncodeReduce(nil, Reduce{Version: Version, ClOrdID: "cl-1", Quantity: 40})),
		"accepted":  must(EncodeAccepted(nil, Accepted{Version: Version, ClOrdID: "cl-1", Price: 30000, Quantity: 250, Side: SideBuy})),
		"rejected":  must(EncodeRejected(nil, Rejected{Version: Version, ClOrdID: "cl-1", Reason: ReasonPriceBand})),
		"executed":  must(EncodeExecuted(nil, Executed{Version: Version, ClOrdID: "cl-1", Price: 30000, Quantity: 100, LeavesQty: 150, Aggressor: SideSell})),
		"canceled":  must(EncodeCanceled(nil, Canceled{Version: Version, ClOrdID: "cl-1", Reason: ReasonNone})),
		"replaced":  must(EncodeReplaced(nil, Replaced{Version: Version, ClOrdID: "cl-1", LeavesQty: 40})),
		"cmdreject": must(EncodeCmdReject(nil, CmdReject{Version: Version, ClOrdID: "cl-9", Reason: ReasonThrottled})),
		"login_request": must(EncodeLoginRequest(nil, LoginRequest{
			Username: "alice", Password: "s3cret", Session: "INC0000001", Sequence: 42,
		})),
		"login_accepted": must(EncodeLoginAccepted(nil, LoginAccepted{Session: "INC0000001", Sequence: 42})),
		"query":          must(EncodeQuery(nil, Query{Version: Version})),
		"open_order":     must(EncodeOpenOrder(nil, OpenOrder{Version: Version, ClOrdID: "cl-1", Price: 30000, LeavesQty: 150, Side: SideBuy})),
		"query_end":      must(EncodeQueryEnd(nil, QueryEnd{Version: Version, Count: 2, Seq: 77})),
	}
}

// TestGoldenVectors is the freeze. Run with -update to regenerate, which should
// only ever happen alongside a Version bump.
func TestGoldenVectors(t *testing.T) {
	update := os.Getenv("WIRE_UPDATE_GOLDEN") == "1"
	dir := filepath.Join("testdata")
	if update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	for name, encoded := range goldenCases(t) {
		path := filepath.Join(dir, name+".hex")
		want := hex.EncodeToString(encoded)

		if update {
			if err := os.WriteFile(path, []byte(want+"\n"), 0o644); err != nil {
				t.Fatalf("WriteFile %s: %v", path, err)
			}
			continue
		}

		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing golden vector %s: %v (regenerate with WIRE_UPDATE_GOLDEN=1)", path, err)
		}
		if got := strings.TrimSpace(string(b)); got != want {
			t.Errorf("%s: wire layout changed\n  now:    %s\n  golden: %s\nA field moved. Bump Version and add vectors; do not edit these.", name, want, got)
		}
	}
}
