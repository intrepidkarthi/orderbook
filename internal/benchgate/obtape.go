// Package benchgate is the benchmark gate's shared half: the committed command tape,
// the portable output digest, and the drivers that replay one into the other
// (docs/BENCH-GATE.md).
package benchgate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/intrepidkarthi/orderbook/internal/tape"
)

// The obtape format, version 1 (docs/BENCH-GATE.md §2.2). A text file because the
// file is the contract another implementation reads, and a seed is not: replaying a
// seed means re-implementing the generator's draw order exactly, while reading this
// takes a line splitter and an integer parser in any language.
//
// The reader rejects everything it does not know — an unknown key, an unknown enum
// name, a header line out of order, a version it was not written for. It never skips
// one. A reader that skipped an unknown field would replay a tape that means
// something it cannot see, and report the result as a comparison.
const (
	magic     = "obtape 1"
	separator = "---"
)

// Header is what a tape file says about itself, apart from what it computes.
type Header struct {
	// Generator and Provenance are recorded, not interpreted: they say where the
	// body came from, and the body is the contract.
	Generator  string
	Provenance string
	// MaxOrders is the book capacity the tape was shaped under. It is explicit
	// because 0 does NOT mean unlimited: both the engine and refmatch turn 0 into
	// 100,000.
	MaxOrders int64
}

// Tape is a parsed file.
type Tape struct {
	Header
	Cmds []tape.Cmd
	// FileSHA256 is the hash of the whole file, header included. It is what the
	// output digest names its input by.
	FileSHA256 [32]byte
}

// Fixed field order per kind. A line must carry exactly these keys, in this order.
var fieldOrder = map[tape.Kind][]string{
	tape.Submit:  {"pos", "kind", "user", "side", "type", "price", "qty", "tif", "postonly"},
	tape.Cancel:  {"pos", "kind", "target", "user"},
	tape.Reduce:  {"pos", "kind", "target", "user", "newqty"},
	tape.Replace: {"pos", "kind", "target", "user", "side", "type", "price", "qty", "tif", "postonly"},
}

var kindByName = map[string]tape.Kind{"Submit": tape.Submit, "Cancel": tape.Cancel, "Reduce": tape.Reduce, "Replace": tape.Replace}

// kindOrder and featureOrder fix the order of the header's kinds and features lines.
var (
	kindOrder    = []tape.Kind{tape.Submit, tape.Cancel, tape.Reduce, tape.Replace}
	featureOrder = []string{"limit", "market", "gtc", "ioc", "fok", "postonly"}
	tifNames     = []string{"GTC", "IOC", "FOK"}
)

// Marshal writes cmds as an obtape 1 file. It refuses any command the format cannot
// say: a kind outside the four, a self-trade-prevention mode, a trade group, a
// privileged order, or positions that are not dense from zero. Refusing is the
// point — silently dropping a field would write a tape that replays differently from
// the one it was made from.
func Marshal(h Header, cmds []tape.Cmd) ([]byte, error) {
	if strings.ContainsAny(h.Generator+h.Provenance, "\n\r") {
		return nil, fmt.Errorf("obtape: header text contains a line break")
	}
	if h.MaxOrders <= 0 {
		return nil, fmt.Errorf("obtape: maxorders %d: the capacity must be explicit and positive", h.MaxOrders)
	}
	var body bytes.Buffer
	usedKinds := map[tape.Kind]bool{}
	usedFeatures := map[string]bool{}
	for i, c := range cmds {
		if c.Pos != i {
			return nil, fmt.Errorf("obtape: command %d has pos %d; positions must be dense from 0", i, c.Pos)
		}
		if _, ok := fieldOrder[c.Kind]; !ok {
			return nil, fmt.Errorf("obtape: command %d: kind %s is not in obtape 1", i, c.Kind)
		}
		if c.STP != 0 || c.TradeGroup != 0 || c.Privileged {
			return nil, fmt.Errorf("obtape: command %d carries STP/trade-group/privileged, which obtape 1 cannot say", i)
		}
		u, err := userNumber(c.User)
		if err != nil {
			return nil, fmt.Errorf("obtape: command %d: %v", i, err)
		}
		usedKinds[c.Kind] = true
		fmt.Fprintf(&body, "pos=%d kind=%s", c.Pos, c.Kind)
		if c.Kind != tape.Submit {
			if c.Target < 0 || c.Target >= c.Pos {
				return nil, fmt.Errorf("obtape: command %d targets %d, which is not an earlier position", i, c.Target)
			}
			fmt.Fprintf(&body, " target=%d", c.Target)
		}
		fmt.Fprintf(&body, " user=%d", u)
		switch c.Kind {
		case tape.Reduce:
			fmt.Fprintf(&body, " newqty=%d", c.NewQty)
		case tape.Submit, tape.Replace:
			if c.TIF > 2 {
				return nil, fmt.Errorf("obtape: command %d has TIF %d", i, c.TIF)
			}
			side := "B"
			if c.Sell {
				side = "S"
			}
			fmt.Fprintf(&body, " side=%s", side)
			if c.MarketOrd {
				// A market order's drawn price means nothing: the engine and refmatch
				// both zero it. Writing it would put a number in the contract that no
				// implementation may use.
				fmt.Fprintf(&body, " type=MARKET")
				usedFeatures["market"] = true
			} else {
				fmt.Fprintf(&body, " type=LIMIT price=%d", c.Price)
				usedFeatures["limit"] = true
			}
			fmt.Fprintf(&body, " qty=%d tif=%s postonly=%d", c.Qty, tifNames[c.TIF], b2i(c.PostOnly))
			usedFeatures[strings.ToLower(tifNames[c.TIF])] = true
			if c.PostOnly {
				usedFeatures["postonly"] = true
			}
		}
		body.WriteByte('\n')
	}

	var kinds, features []string
	for _, k := range kindOrder {
		if usedKinds[k] {
			kinds = append(kinds, k.String())
		}
	}
	for _, f := range featureOrder {
		if usedFeatures[f] {
			features = append(features, f)
		}
	}
	sum := sha256.Sum256(body.Bytes())

	var out bytes.Buffer
	fmt.Fprintf(&out, "%s\n", magic)
	fmt.Fprintf(&out, "generator %s\n", h.Generator)
	fmt.Fprintf(&out, "provenance %s\n", h.Provenance)
	fmt.Fprintf(&out, "config maxorders=%d\n", h.MaxOrders)
	fmt.Fprintf(&out, "kinds %s\n", strings.Join(kinds, " "))
	fmt.Fprintf(&out, "features %s\n", strings.Join(features, " "))
	fmt.Fprintf(&out, "body-sha256 %s\n", hex.EncodeToString(sum[:]))
	fmt.Fprintf(&out, "%s\n", separator)
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// Parse reads an obtape 1 file.
func Parse(b []byte) (*Tape, error) {
	if bytes.IndexByte(b, '\r') >= 0 {
		return nil, fmt.Errorf("obtape: CR in file; line endings are LF only")
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return nil, fmt.Errorf("obtape: file does not end with a line feed")
	}
	sepAt := bytes.Index(b, []byte("\n"+separator+"\n"))
	if sepAt < 0 {
		return nil, fmt.Errorf("obtape: no %q line between header and body", separator)
	}
	head := strings.Split(string(b[:sepAt]), "\n")
	body := b[sepAt+len(separator)+2:]

	t := &Tape{FileSHA256: sha256.Sum256(b)}
	want := []string{"obtape", "generator", "provenance", "config", "kinds", "features", "body-sha256"}
	if len(head) != len(want) {
		return nil, fmt.Errorf("obtape: header has %d lines, want %d", len(head), len(want))
	}
	if head[0] != magic {
		return nil, fmt.Errorf("obtape: first line %q, want %q (an unknown version is refused, never guessed)", head[0], magic)
	}
	values := make([]string, len(want))
	for i := 1; i < len(want); i++ {
		key, val, ok := strings.Cut(head[i], " ")
		if !ok || key != want[i] {
			return nil, fmt.Errorf("obtape: header line %d is %q, want the %q line", i+1, head[i], want[i])
		}
		if val != strings.TrimSpace(val) || val == "" && i < 4 {
			return nil, fmt.Errorf("obtape: header line %d has stray or missing whitespace", i+1)
		}
		values[i] = val
	}
	t.Generator, t.Provenance = values[1], values[2]
	cfgKey, cfgVal, _ := strings.Cut(values[3], "=")
	if cfgKey != "maxorders" {
		return nil, fmt.Errorf("obtape: config %q, want maxorders=<n>", values[3])
	}
	mo, err := parseInt(cfgVal)
	if err != nil || mo <= 0 {
		return nil, fmt.Errorf("obtape: config maxorders %q must be a positive integer", cfgVal)
	}
	t.MaxOrders = mo
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != values[6] {
		return nil, fmt.Errorf("obtape: body-sha256 says %s, body hashes to %s", values[6], got)
	}

	lines := strings.Split(string(body), "\n")
	lines = lines[:len(lines)-1] // the final LF
	for i, line := range lines {
		c, err := parseLine(line, i)
		if err != nil {
			return nil, fmt.Errorf("obtape: body line %d: %v", i+1, err)
		}
		t.Cmds = append(t.Cmds, c)
	}

	// The kinds and features lines must be exactly what the body uses, so a reader
	// learns from the header alone whether it can replay the tape. Only those two
	// lines are compared: every other rule has its own check above, and a final
	// whole-file comparison would hide a broken one from its test.
	again, err := Marshal(t.Header, t.Cmds)
	if err != nil {
		return nil, err
	}
	againHead := strings.Split(string(again[:bytes.Index(again, []byte("\n"+separator+"\n"))]), "\n")
	for _, i := range []int{4, 5} {
		if againHead[i] != head[i] {
			return nil, fmt.Errorf("obtape: header says %q, the body uses %q", head[i], againHead[i])
		}
	}
	return t, nil
}

func parseLine(line string, i int) (tape.Cmd, error) {
	fields := strings.Split(line, " ")
	if len(fields) < 2 {
		return tape.Cmd{}, fmt.Errorf("too few fields")
	}
	kv := func(j int) (string, string) {
		k, v, _ := strings.Cut(fields[j], "=")
		return k, v
	}
	if k, v := kv(1); k == "kind" {
		kind, ok := kindByName[v]
		if !ok {
			return tape.Cmd{}, fmt.Errorf("unknown kind %q", v)
		}
		order := fieldOrder[kind]
		c := tape.Cmd{Kind: kind}
		market := false
		j := 0
		for _, key := range order {
			if key == "price" && market {
				continue
			}
			if j >= len(fields) {
				return tape.Cmd{}, fmt.Errorf("missing field %q", key)
			}
			k, v := kv(j)
			if k != key {
				return tape.Cmd{}, fmt.Errorf("field %d is %q, want %q", j+1, fields[j], key)
			}
			j++
			if err := setField(&c, key, v, &market); err != nil {
				return tape.Cmd{}, err
			}
		}
		if j != len(fields) {
			return tape.Cmd{}, fmt.Errorf("unexpected field %q", fields[j])
		}
		if c.Pos != i {
			return tape.Cmd{}, fmt.Errorf("pos %d on line %d; positions are dense from 0", c.Pos, i+1)
		}
		if kind != tape.Submit && (c.Target < 0 || c.Target >= c.Pos) {
			return tape.Cmd{}, fmt.Errorf("target %d is not an earlier position", c.Target)
		}
		return c, nil
	}
	return tape.Cmd{}, fmt.Errorf("second field must be kind=, got %q", fields[1])
}

func setField(c *tape.Cmd, key, v string, market *bool) error {
	switch key {
	case "pos", "target", "user", "price", "qty", "newqty":
		n, err := parseInt(v)
		if err != nil {
			return fmt.Errorf("%s=%q: %v", key, v, err)
		}
		switch key {
		case "pos":
			c.Pos = int(n)
		case "target":
			c.Target = int(n)
		case "user":
			if n < 0 {
				return fmt.Errorf("user=%d is negative", n)
			}
			c.User = "u" + strconv.FormatInt(n, 10)
		case "price":
			c.Price = n
		case "qty":
			c.Qty = n
		case "newqty":
			c.NewQty = n
		}
	case "kind":
		// already decoded
	case "side":
		switch v {
		case "B":
		case "S":
			c.Sell = true
		default:
			return fmt.Errorf("unknown side %q", v)
		}
	case "type":
		switch v {
		case "LIMIT":
		case "MARKET":
			c.MarketOrd, *market = true, true
		default:
			return fmt.Errorf("unknown type %q", v)
		}
	case "tif":
		for t, name := range tifNames {
			if v == name {
				c.TIF = uint8(t)
				return nil
			}
		}
		return fmt.Errorf("unknown tif %q", v)
	case "postonly":
		switch v {
		case "0":
		case "1":
			c.PostOnly = true
		default:
			return fmt.Errorf("postonly=%q, want 0 or 1", v)
		}
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

// parseInt accepts only the canonical decimal form, so a file has exactly one
// spelling of every number: no leading zeros, no plus sign, no spaces.
func parseInt(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if strconv.FormatInt(n, 10) != s {
		return 0, fmt.Errorf("%q is not in canonical decimal form", s)
	}
	return n, nil
}

// userNumber turns the generator's "u17" into 17. The file carries integers, because
// another implementation should not have to know how this repository spells an
// account.
func userNumber(u string) (int64, error) {
	rest, ok := strings.CutPrefix(u, "u")
	if !ok {
		return 0, fmt.Errorf("user %q is not u<n>", u)
	}
	n, err := parseInt(rest)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("user %q is not u<n>", u)
	}
	return n, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
