package fix

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

var acme = types.NewInstrument("ACME", decimal.RequireFromString("0.01"), decimal.NewFromInt(1))

// client builds one client message; the session fields a session layer would own
// are fixed.
func client(msgType, sender string, fields ...Field) []byte {
	b := NewBuilder(msgType).Add(49, sender).Add(56, "VENUE").AddInt(34, 1).Add(52, "20261007-09:30:00.000")
	for _, f := range fields {
		b.Add(f.Tag, f.Value)
	}
	return b.Bytes()
}

func nos(sender, clOrdID, side, qty, price, tif string, extra ...Field) []byte {
	f := []Field{{11, clOrdID}, {55, "ACME"}, {54, side}, {38, qty}, {40, "2"}, {44, price}, {59, tif}}
	return client("D", sender, append(f, extra...)...)
}

func cxl(sender, clOrdID, orig, side string) []byte {
	return client("F", sender, Field{11, clOrdID}, Field{41, orig}, Field{55, "ACME"}, Field{54, side})
}

// sessionIn is the captured client sequence of docs/FIX.md §5.
func sessionIn() [][]byte {
	return [][]byte{
		nos("maker", "A1", "2", "10", "100.00", "1"),                // rests
		nos("taker", "B1", "1", "4", "100.00", "1"),                 // fills 4 of A1
		nos("taker", "B2", "1", "6", "101.00", "3"),                 // IOC: fills the other 6
		nos("maker", "A2", "2", "5", "102.00", "1"),                 // rests
		cxl("maker", "C1", "A2", "2"),                               // cancelled
		nos("maker", "A3", "2", "3", "103.00", "1"),                 // rests
		nos("taker", "P1", "1", "1", "103.00", "1", Field{18, "6"}), // post-only would cross: rejected
		cxl("maker", "C2", "ZZ", "2"),                               // unknown order
		nos("maker", "A1", "2", "1", "104.00", "1"),                 // duplicate ClOrdID
		cxl("maker", "C3", "A1", "2"),                               // already filled: too late
	}
}

func visible(b []byte) string { return string(bytes.ReplaceAll(b, []byte{SOH}, []byte("|"))) }

func runSession(t *testing.T, in [][]byte) string {
	t.Helper()
	var out strings.Builder
	rep := NewReporter(ReporterConfig{
		SenderCompID: "VENUE", Instrument: acme,
		Clock: func() time.Time { return time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC) },
	}, func(user string, msg []byte) {
		out.WriteString(user + " " + visible(msg) + "\n")
	})
	cfg := matching.DefaultConfig("ACME")
	cfg.EventSink = rep
	e := matching.NewEngine(cfg)
	en := NewEntry(e, rep)
	for i, m := range in {
		if err := en.Handle(m); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	return out.String()
}

// TestCapturedSession replays the client sequence and requires every report the
// engine sends back to equal the committed capture, byte for byte. FIX_WRITE=1
// rewrites both files; read the diff before committing it.
func TestCapturedSession(t *testing.T) {
	var in strings.Builder
	for _, m := range sessionIn() {
		in.WriteString(visible(m) + "\n")
	}
	got := runSession(t, sessionIn())
	if os.Getenv("FIX_WRITE") == "1" {
		os.WriteFile("testdata/session.in", []byte(in.String()), 0o644)
		os.WriteFile("testdata/session.out", []byte(got), 0o644)
	}
	wantIn, err := os.ReadFile("testdata/session.in")
	if err != nil {
		t.Fatal(err)
	}
	if string(wantIn) != in.String() {
		t.Fatal("testdata/session.in differs from the sequence this test sends")
	}
	want, err := os.ReadFile("testdata/session.out")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		g, w := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(g) && i < len(w); i++ {
			if g[i] != w[i] {
				t.Fatalf("report %d:\n got %s\nwant %s", i+1, g[i], w[i])
			}
		}
		t.Fatalf("%d reports, captured %d", len(g), len(w))
	}
	// The capture's own messages must be valid FIX.
	for i, line := range strings.Split(strings.TrimSpace(got), "\n") {
		_, msg, _ := strings.Cut(line, " ")
		if _, _, err := Parse([]byte(strings.ReplaceAll(msg, "|", "\x01"))); err != nil {
			t.Fatalf("report %d does not parse: %v", i+1, err)
		}
	}
}
