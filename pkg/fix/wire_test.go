package fix

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func soh(s string) []byte { return bytes.ReplaceAll([]byte(s), []byte("|"), []byte{SOH}) }

// independentFrame recomputes BodyLength and CheckSum with a loop of its own, so a
// shared mistake in Builder and Parse cannot pass unnoticed.
func independentFrame(t *testing.T, msg []byte) {
	t.Helper()
	s := string(msg)
	start := len("8=FIX.4.4|9=")
	end := start
	for s[end] != SOH {
		end++
	}
	n, _ := strconv.Atoi(s[start:end])
	at := bytes.LastIndex(msg, []byte("\x0110="))
	if got := at + 1 - (end + 1); got != n {
		t.Fatalf("BodyLength says %d, body is %d bytes", n, got)
	}
	sum := 0
	for i := 0; i <= at; i++ {
		sum += int(msg[i])
	}
	if want := fmt.Sprintf("10=%03d\x01", sum%256); s[at+1:] != want {
		t.Fatalf("trailer %q, want %q", s[at+1:], want)
	}
}

func TestBuiltMessagesParseAndFrameCorrectly(t *testing.T) {
	for _, msg := range sessionIn() {
		independentFrame(t, msg)
		m, n, err := Parse(msg)
		if err != nil || n != len(msg) {
			t.Fatalf("%s: n=%d err=%v", visible(msg), n, err)
		}
		if m.Fields[0] != (Field{8, "FIX.4.4"}) || m.Fields[2].Tag != 35 || m.Fields[len(m.Fields)-1].Tag != 10 {
			t.Fatalf("header or trailer out of place: %v", m.Fields)
		}
	}
}

func TestStreamOfMessages(t *testing.T) {
	var stream []byte
	in := sessionIn()
	for _, m := range in {
		stream = append(stream, m...)
	}
	for i := 0; len(stream) > 0; i++ {
		m, n, err := Parse(stream)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !bytes.Equal(stream[:n], in[i]) || m.Type() != string([]byte{in[i][bytes.Index(in[i], []byte("35="))+3]}) {
			t.Fatalf("message %d parsed as %v", i, m)
		}
		stream = stream[n:]
	}
}

// TestFramingIsVerified is issue #6's second item: each corruption is refused with
// the matching error, and a prefix of a valid message asks for more input.
func TestFramingIsVerified(t *testing.T) {
	good := nos("maker", "A1", "2", "10", "100.00", "1")
	at := bytes.Index(good, []byte("9="))
	semi := bytes.IndexByte(good[at:], SOH) + at
	n, _ := strconv.Atoi(string(good[at+2 : semi]))
	withLength := func(n int) []byte {
		b := append([]byte{}, good[:at+2]...)
		b = strconv.AppendInt(b, int64(n), 10)
		return append(b, good[semi:]...)
	}
	corruptSum := append([]byte{}, good...)
	if corruptSum[len(corruptSum)-2] == '9' {
		corruptSum[len(corruptSum)-2] = '0'
	} else {
		corruptSum[len(corruptSum)-2]++
	}
	corruptBody := append([]byte{}, good...)
	corruptBody[bytes.Index(corruptBody, []byte("38=10"))+4] = '2' // OrderQty 10 -> 12, sum not updated

	cases := []struct {
		name string
		b    []byte
		want error
	}{
		{"CheckSum digit changed", corruptSum, ErrCheckSum},
		{"a body byte changed", corruptBody, ErrCheckSum},
		{"BodyLength one short", withLength(n - 1), ErrBodyLength},
		{"BodyLength one long", append(withLength(n+1), soh("8=FIX.4.4|")...), ErrBodyLength},
		{"BeginString FIX.4.2", append(soh("8=FIX.4.2|"), good[len("8=FIX.4.4|"):]...), ErrBeginString},
		{"a prefix of a message", good[:len(good)-5], ErrIncomplete},
		{"a prefix inside BeginString", good[:4], ErrIncomplete},
		{"MsgType not third", NewBuilderRaw(t, "49=maker|35=D|"), ErrGarbled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := Parse(c.b)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// NewBuilderRaw frames an arbitrary body correctly, to test rules beyond framing.
func NewBuilderRaw(t *testing.T, body string) []byte {
	t.Helper()
	b := &Builder{body: soh(body)}
	return b.Bytes()
}

func TestDecodingRefusals(t *testing.T) {
	cases := map[string]struct {
		msg []byte
		tag int
	}{
		"price off the tick grid":  {nos("u", "X1", "1", "1", "100.005", "1"), 44},
		"fractional quantity":      {nos("u", "X1", "1", "1.5", "100.00", "1"), 38},
		"zero quantity":            {nos("u", "X1", "1", "0", "100.00", "1"), 38},
		"side 3":                   {nos("u", "X1", "3", "1", "100.00", "1"), 54},
		"TimeInForce 6 (GTD)":      {nos("u", "X1", "1", "1", "100.00", "6"), 59},
		"another symbol":           {client("D", "u", Field{11, "X1"}, Field{55, "OTHER"}, Field{54, "1"}, Field{38, "1"}, Field{40, "2"}, Field{44, "1"}), 55},
		"limit without a price":    {client("D", "u", Field{11, "X1"}, Field{55, "ACME"}, Field{54, "1"}, Field{38, "1"}, Field{40, "2"}), 44},
		"OrdType 3 (stop)":         {client("D", "u", Field{11, "X1"}, Field{55, "ACME"}, Field{54, "1"}, Field{38, "1"}, Field{40, "3"}), 40},
		"no ClOrdID":               {client("D", "u", Field{55, "ACME"}, Field{54, "1"}, Field{38, "1"}, Field{40, "1"}), 11},
		"cancel with no OrigClOrd": {client("F", "u", Field{11, "C"}, Field{55, "ACME"}, Field{54, "1"}), 41},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m, _, err := Parse(c.msg)
			if err != nil {
				t.Fatal(err)
			}
			if m.Type() == "D" {
				_, err = DecodeNewOrderSingle(m, acme)
			} else {
				_, err = DecodeOrderCancelRequest(m)
			}
			var fe *FieldError
			if !errors.As(err, &fe) || fe.Tag != c.tag {
				t.Fatalf("got %v, want a refusal of tag %d", err, c.tag)
			}
		})
	}
}

func TestDecodersRefuseOtherTypes(t *testing.T) {
	d, _, _ := Parse(nos("u", "X1", "1", "1", "100.00", "1"))
	f, _, _ := Parse(cxl("u", "C1", "X1", "1"))
	if _, err := DecodeNewOrderSingle(f, acme); !errors.Is(err, ErrWrongType) {
		t.Fatalf("D decoder given F: %v", err)
	}
	if _, err := DecodeOrderCancelRequest(d); !errors.Is(err, ErrWrongType) {
		t.Fatalf("F decoder given D: %v", err)
	}
}

func TestDecodedOrder(t *testing.T) {
	m, _, _ := Parse(nos("gw", "X1", "2", "7", "100.25", "4", Field{1, "acct9"}, Field{18, "6"}))
	o, err := DecodeNewOrderSingle(m, acme)
	if err != nil {
		t.Fatal(err)
	}
	if o.UserID != "acct9" || o.ClientOrderID != "X1" || o.Side != types.SideSell || o.Price != 10025 ||
		o.Quantity != 7 || o.TimeInForce != types.TIFFillOrKill || !o.PostOnly {
		t.Fatalf("decoded %+v", o)
	}
	m, _, _ = Parse(client("D", "gw", Field{11, "X2"}, Field{55, "ACME"}, Field{54, "1"}, Field{38, "1"}, Field{40, "1"}))
	if o, err = DecodeNewOrderSingle(m, acme); err != nil || o.Type != types.OrderTypeMarket || o.UserID != "gw" || o.TimeInForce != types.TIFDay {
		t.Fatalf("market order without Account or TimeInForce: %+v, %v", o, err)
	}
}
