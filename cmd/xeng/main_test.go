package main

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/benchgate"
	"github.com/intrepidkarthi/orderbook/internal/tape"
)

const (
	basicTape   = "../../internal/benchgate/testdata/bench-basic-v1.obt"
	basicDigest = "../../internal/benchgate/testdata/bench-basic-v1.digest"
)

// selfRun exports the basic tape, replays it through this engine's adapter, and
// returns the tape, the adapter's output and the committed digest.
func selfRun(t *testing.T) (*benchgate.Tape, string, [32]byte) {
	t.Helper()
	tp, err := readTape(basicTape)
	if err != nil {
		t.Fatal(err)
	}
	var in, out bytes.Buffer
	if err := benchgate.ExportText(tp, &in); err != nil {
		t.Fatal(err)
	}
	if err := run(&in, &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(basicDigest)
	if err != nil {
		t.Fatal(err)
	}
	want, err := benchgate.ParseDigestFile(b)
	if err != nil {
		t.Fatal(err)
	}
	return tp, out.String(), want.Core
}

// TestTheProtocolReproducesTheCommittedDigest is the protocol's acceptance test: this
// engine, driven the way any adapter drives its engine and folded by DigestText,
// gives the core digest the native driver committed. If it did not, a differing
// digest from another engine would say nothing.
func TestTheProtocolReproducesTheCommittedDigest(t *testing.T) {
	tp, out, want := selfRun(t)
	res, err := benchgate.DigestText(tp, strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if res.Core != want {
		t.Fatalf("core %x through the protocol, %x committed", res.Core, want)
	}
	t.Logf("%d trades, %d resting, replay %.2f ms", res.Trades, res.Resting, float64(res.ReplayNS)/1e6)
}

// TestAnAdapterBugCannotAgree applies the mistakes an adapter could make to a correct
// output. Each must either be refused by DigestText or change the digest; none may
// come back agreeing.
func TestAnAdapterBugCannotAgree(t *testing.T) {
	tp, out, want := selfRun(t)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	find := func(prefix string, nth int) int {
		for i, l := range lines {
			if strings.HasPrefix(l, prefix) {
				if nth == 0 {
					return i
				}
				nth--
			}
		}
		t.Fatalf("no %q line #%d", prefix, nth)
		return -1
	}
	// A command with two trades, for the reorder case.
	twoTrades := -1
	for i := 0; i+2 < len(lines); i++ {
		if strings.HasPrefix(lines[i], "c ") && strings.HasPrefix(lines[i+1], "X ") && strings.HasPrefix(lines[i+2], "X ") {
			twoTrades = i + 1
			break
		}
	}
	if twoTrades < 0 {
		t.Fatal("no command with two trades")
	}
	field := func(l string, i int, v string) string {
		f := strings.Fields(l)
		f[i] = v
		return strings.Join(f, " ")
	}
	flip := func(s string) string {
		return map[string]string{"0": "1", "1": "0", "B": "S", "S": "B"}[s]
	}
	cases := []struct {
		name string
		edit func(l []string) []string
	}{
		{"a refused cancel reported as done", func(l []string) []string {
			i := find("c ", 0)
			for ; !strings.HasSuffix(l[i], " 1"); i++ {
			}
			l[i] = field(l[i], 2, "0")
			return l
		}},
		{"trades of one command reordered", func(l []string) []string {
			l[twoTrades], l[twoTrades+1] = l[twoTrades+1], l[twoTrades]
			return l
		}},
		{"aggressor flipped", func(l []string) []string {
			i := find("X ", 3)
			l[i] = field(l[i], 5, flip(strings.Fields(l[i])[5]))
			return l
		}},
		{"maker and taker swapped", func(l []string) []string {
			i := find("X ", 5)
			f := strings.Fields(l[i])
			f[3], f[4] = f[4], f[3]
			l[i] = strings.Join(f, " ")
			return l
		}},
		{"trade at the taker's price", func(l []string) []string {
			i := find("X ", 7)
			l[i] = field(l[i], 1, "1040")
			return l
		}},
		{"a trade dropped", func(l []string) []string {
			i := find("X ", 9)
			return append(l[:i], l[i+1:]...)
		}},
		{"a command dropped", func(l []string) []string {
			i := find("c ", 100)
			return append(l[:i], l[i+1:]...)
		}},
		{"a trade naming a cancel's position", func(l []string) []string {
			i := find("X ", 11)
			for j, c := range tp.Cmds {
				if c.Kind == tape.Cancel {
					l[i] = field(l[i], 3, strconv.Itoa(j))
					break
				}
			}
			return l
		}},
		{"two resting orders of one level in the wrong order", func(l []string) []string {
			i := find("L ", 0)
			for ; strings.Fields(l[i])[3] != strings.Fields(l[i+1])[3]; i++ {
			}
			l[i], l[i+1] = l[i+1], l[i]
			return l
		}},
		{"filled quantity reported as zero", func(l []string) []string {
			i := find("L ", 0)
			for ; strings.Fields(l[i])[5] == "0"; i++ {
			}
			l[i] = field(l[i], 5, "0")
			return l
		}},
		{"a resting order missing", func(l []string) []string {
			i := find("L ", 3)
			return append(l[:i], l[i+1:]...)
		}},
		{"last trade price wrong", func(l []string) []string {
			i := find("E ", 0)
			l[i] = field(l[i], 1, "1")
			return l
		}},
		{"trade count wrong", func(l []string) []string {
			i := find("E ", 0)
			l[i] = field(l[i], 2, "1")
			return l
		}},
		{"no timing line", func(l []string) []string { return l[:len(l)-1] }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			edited := c.edit(append([]string(nil), lines...))
			res, err := benchgate.DigestText(tp, strings.NewReader(strings.Join(edited, "\n")+"\n"))
			if err == nil && res.Core == want {
				t.Fatal("the edited output still agrees with the committed digest")
			}
		})
	}
}

// TestMalformedOutputIsAnError: some adapter bugs would change the digest anyway, but
// a "differs" there hides which line is wrong. These must be refused with an error.
func TestMalformedOutputIsAnError(t *testing.T) {
	tp, out, _ := selfRun(t)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	firstX, firstC := -1, -1
	for i, l := range lines {
		if firstX < 0 && strings.HasPrefix(l, "X ") {
			firstX = i
		}
		if firstC < 0 && strings.HasPrefix(l, "c ") {
			firstC = i
		}
	}
	cancelPos := -1
	for _, c := range tp.Cmds {
		if c.Kind == tape.Cancel {
			cancelPos = c.Pos
			break
		}
	}
	cases := map[string]func(l []string) []string{
		"a command dropped": func(l []string) []string { return append(l[:firstC+5], l[firstC+6:]...) },
		"a command repeated": func(l []string) []string {
			return append(l[:firstC+1], append([]string{l[firstC]}, l[firstC+1:]...)...)
		},
		"a trade naming a cancel's position": func(l []string) []string {
			f := strings.Fields(l[firstX])
			f[3] = strconv.Itoa(cancelPos)
			l[firstX] = strings.Join(f, " ")
			return l
		},
		"two commands swapped": func(l []string) []string {
			for i := firstC; i+1 < len(l); i++ {
				if strings.HasPrefix(l[i], "c ") && strings.HasPrefix(l[i+1], "c ") {
					l[i], l[i+1] = l[i+1], l[i]
					break
				}
			}
			return l
		},
		"a trade before any command": func(l []string) []string { return append([]string{l[firstX]}, l...) },
		"an unknown line":            func(l []string) []string { return append(l[:2], append([]string{"Z 1 2"}, l[2:]...)...) },
		"no timing line":             func(l []string) []string { return l[:len(l)-1] },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			edited := edit(append([]string(nil), lines...))
			if _, err := benchgate.DigestText(tp, strings.NewReader(strings.Join(edited, "\n")+"\n")); err == nil {
				t.Fatal("accepted without an error")
			}
		})
	}
}

// BenchmarkSelfReplay times this engine's adapter over bench-basic-v1, the loop the
// cross-engine table times (docs/CROSS-ENGINE.md §4), for profiling the gap.
func BenchmarkSelfReplay(b *testing.B) {
	tp, err := readTape(basicTape)
	if err != nil {
		b.Fatal(err)
	}
	var in bytes.Buffer
	if err := benchgate.ExportText(tp, &in); err != nil {
		b.Fatal(err)
	}
	text := in.Bytes()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := run(bytes.NewReader(text), io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
