package benchgate

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/tape"
)

var testHeader = Header{Generator: "hand-written", Provenance: "obtape_test.go", MaxOrders: 1000}

// small is a tape that uses every kind and every feature obtape 1 can say.
func small(t *testing.T) []byte {
	t.Helper()
	cmds := []tape.Cmd{
		{Pos: 0, Kind: tape.Submit, User: "u2", Price: 100, Qty: 5},
		{Pos: 1, Kind: tape.Submit, User: "u3", Sell: true, Price: 101, Qty: 3, PostOnly: true},
		{Pos: 2, Kind: tape.Submit, User: "u5", Sell: true, MarketOrd: true, Qty: 2, TIF: 1},
		{Pos: 3, Kind: tape.Reduce, User: "u2", Target: 0, NewQty: 4},
		{Pos: 4, Kind: tape.Replace, User: "u3", Sell: true, Target: 1, Price: 100, Qty: 1, TIF: 2},
		{Pos: 5, Kind: tape.Cancel, User: "u2", Target: 0},
	}
	b, err := Marshal(testHeader, cmds)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestObtapeRoundTrip: what the generator draws survives a write and a read, except
// the one value the format drops on purpose (a market order's drawn price), and
// writing what was read reproduces the file byte for byte.
func TestObtapeRoundTrip(t *testing.T) {
	cmds := tape.Gen(tape.Bench, 7, 5000)
	b, err := Marshal(Header{Generator: "internal/tape", Provenance: "profile=bench seed=0x7 n=5000", MaxOrders: 1000000}, cmds)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]tape.Cmd, len(cmds))
	for i, c := range cmds {
		if c.MarketOrd {
			c.Price = 0
		}
		want[i] = c
	}
	if !reflect.DeepEqual(got.Cmds, want) {
		for i := range want {
			if got.Cmds[i] != want[i] {
				t.Fatalf("command %d changed in the round trip:\n got %+v\nwant %+v", i, got.Cmds[i], want[i])
			}
		}
	}
	again, err := Marshal(got.Header, got.Cmds)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(b) {
		t.Fatal("writing what was read does not reproduce the file")
	}
	if got.FileSHA256 != sha256.Sum256(b) {
		t.Fatal("FileSHA256 is not the hash of the whole file")
	}
}

// TestObtapeMarketLineHasNoPrice: the drawn price of a market order is a number no
// implementation may use, so it is not in the contract at all.
func TestObtapeMarketLineHasNoPrice(t *testing.T) {
	for _, line := range strings.Split(string(small(t)), "\n") {
		if strings.Contains(line, "type=MARKET") && strings.Contains(line, "price=") {
			t.Fatalf("a MARKET line carries a price: %q", line)
		}
	}
}

// rehash replaces old with new once and then fixes the body-sha256 line, so a
// rejection test reaches the check it names rather than the checksum.
func rehash(t *testing.T, b []byte, old, new string) []byte {
	t.Helper()
	s := string(b)
	if !strings.Contains(s, old) {
		t.Fatalf("mutation target %q not in file", old)
	}
	s = strings.Replace(s, old, new, 1)
	i := strings.Index(s, "\n---\n") + len("\n---\n")
	sum := sha256.Sum256([]byte(s[i:]))
	j := strings.Index(s, "body-sha256 ") + len("body-sha256 ")
	return []byte(s[:j] + hex.EncodeToString(sum[:]) + s[j+64:])
}

// TestObtapeRejects: every rule the reader enforces, each broken on its own, each
// asserting the message of the check it is about, so a broken check cannot hide
// behind a neighbouring one.
func TestObtapeRejects(t *testing.T) {
	base := small(t)
	raw := func(old, new string) []byte { return []byte(strings.Replace(string(base), old, new, 1)) }
	cases := []struct {
		name string
		file []byte
		want string
	}{
		{"unknown version", raw("obtape 1\n", "obtape 2\n"), "unknown version"},
		{"header out of order", raw("generator hand-written\nprovenance obtape_test.go\n", "provenance obtape_test.go\ngenerator hand-written\n"), "want the \"generator\" line"},
		{"body edited, hash not", raw("qty=5", "qty=6"), "body-sha256 says"},
		{"unknown key", rehash(t, base, "postonly=0", "posonly=0"), "want \"postonly\""},
		{"extra field", rehash(t, base, "pos=5 kind=Cancel target=0 user=2", "pos=5 kind=Cancel target=0 user=2 note=x"), "unexpected field"},
		{"unknown kind", rehash(t, base, "kind=Cancel", "kind=CancelAll"), "unknown kind"},
		{"unknown side", rehash(t, base, "side=B", "side=X"), "unknown side"},
		{"unknown type", rehash(t, base, "type=LIMIT", "type=STOP"), "unknown type"},
		{"unknown tif", rehash(t, base, "tif=GTC", "tif=GTD"), "unknown tif"},
		{"non-canonical integer", rehash(t, base, "qty=5", "qty=05"), "canonical"},
		{"positions not dense", rehash(t, base, "pos=5 ", "pos=6 "), "dense"},
		{"target not earlier", rehash(t, base, "pos=5 kind=Cancel target=0", "pos=5 kind=Cancel target=5"), "not an earlier position"},
		{"kinds line wrong", raw("kinds Submit Cancel Reduce Replace", "kinds Submit Cancel Reduce"), "the body uses"},
		{"features line wrong", raw("features limit market gtc ioc fok postonly", "features limit market gtc"), "the body uses"},
		{"CR line ending", raw("---\n", "---\r\n"), "CR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.file)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rejected by the wrong check: %v (want one mentioning %q)", err, tc.want)
			}
		})
	}
}

// TestObtapeWriterRefuses: the writer never drops what it cannot say.
func TestObtapeWriterRefuses(t *testing.T) {
	ok := tape.Cmd{Pos: 0, Kind: tape.Submit, User: "u1", Price: 100, Qty: 1}
	cases := map[string]struct {
		h    Header
		cmds []tape.Cmd
	}{
		"STP mode":       {testHeader, []tape.Cmd{func() tape.Cmd { c := ok; c.STP = 2; return c }()}},
		"trade group":    {testHeader, []tape.Cmd{func() tape.Cmd { c := ok; c.TradeGroup = 1; return c }()}},
		"privileged":     {testHeader, []tape.Cmd{func() tape.Cmd { c := ok; c.Privileged = true; return c }()}},
		"halt":           {testHeader, []tape.Cmd{{Pos: 0, Kind: tape.Halt}}},
		"gap":            {testHeader, []tape.Cmd{func() tape.Cmd { c := ok; c.Pos = 1; return c }()}},
		"zero capacity":  {Header{Generator: "x", Provenance: "y"}, []tape.Cmd{ok}},
		"odd user name":  {testHeader, []tape.Cmd{func() tape.Cmd { c := ok; c.User = "alice"; return c }()}},
		"header newline": {Header{Generator: "a\nb", Provenance: "y", MaxOrders: 1}, []tape.Cmd{ok}},
	}
	for name, tc := range cases {
		if _, err := Marshal(tc.h, tc.cmds); err == nil {
			t.Errorf("%s: written", name)
		}
	}
}
