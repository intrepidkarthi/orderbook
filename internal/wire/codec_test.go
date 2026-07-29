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
		"cmdreject": func(b []byte) error { _, err := DecodeCmdReject(b); return err },
		"login":     func(b []byte) error { _, err := DecodeLoginRequest(b); return err },
	}
	for name, dec := range cases {
		for _, n := range []int{0, 1, 5} {
			if err := dec(make([]byte, n)); !errors.Is(err, ErrShort) {
				t.Errorf("%s with %d bytes: err = %v, want ErrShort", name, n, err)
			}
		}
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
