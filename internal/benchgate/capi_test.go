package benchgate

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/tape"
)

// TestCABIReplaysTheBenchTapeToTheSameDigest is the C API's acceptance test
// (docs/C-API.md §4). It goes through the C boundary for real: it builds the c-shared
// library, compiles a C driver against it, replays the bench tape through the driver,
// and folds what the driver printed into the OBDG encoder. Both chains must equal the
// native engine's digest. Calling the Go exports directly would skip the boundary this
// is meant to test.
func TestCABIReplaysTheBenchTapeToTheSameDigest(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a shared library and a C program")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler")
	}
	if out, _ := exec.Command("go", "env", "CGO_ENABLED").Output(); strings.TrimSpace(string(out)) != "1" {
		t.Skip("cgo disabled")
	}
	dir := t.TempDir()
	lib := "libobook.so"
	if runtime.GOOS == "darwin" {
		lib = "libobook.dylib"
	}
	run(t, "", "go", "build", "-buildmode=c-shared", "-o", filepath.Join(dir, lib), "../../cmd/libobook")
	run(t, "", cc, "-O2", "-I", dir, "testdata/capi_driver.c", "-L", dir, "-lobook",
		"-Wl,-rpath,"+dir, "-o", filepath.Join(dir, "driver"))

	_, tp := readBenchTape(t)
	cmd := exec.Command(filepath.Join(dir, "driver"))
	cmd.Stdin = strings.NewReader(driverInput(tp.Cmds))
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+dir, "DYLD_LIBRARY_PATH="+dir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("driver: %v\n%s", err, tail(out))
	}

	got, err := digestDriverOutput(tp, out)
	if err != nil {
		t.Fatal(err)
	}
	want, err := DigestEngine(tp)
	if err != nil {
		t.Fatal(err)
	}
	if got.Core != want.Core || got.Full != want.Full {
		t.Fatalf("the C ABI replay disagrees with the native engine; first block that differs: %s",
			firstDiff(want.CoreBlocks, got.CoreBlocks, want.FullBlocks, got.FullBlocks))
	}
	t.Logf("C ABI replay of %d commands: %d resting, %d trades, digest equal to the native engine's",
		len(tp.Cmds), got.Resting, got.Trades)
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func tail(b []byte) []byte {
	if len(b) > 2000 {
		return b[len(b)-2000:]
	}
	return b
}

func b2s(b bool) int {
	if b {
		return 1
	}
	return 0
}

// driverInput renders a tape as the driver's numeric lines.
func driverInput(cmds []tape.Cmd) string {
	var b strings.Builder
	fmt.Fprintf(&b, "N %d\n", len(cmds))
	for _, c := range cmds {
		u, _ := strconv.ParseInt(strings.TrimPrefix(c.User, "u"), 10, 64)
		order := fmt.Sprintf("%d %d %d %d %d %d", b2s(c.Sell), b2s(c.MarketOrd), c.Price, c.Qty, c.TIF, b2s(c.PostOnly))
		switch c.Kind {
		case tape.Submit:
			fmt.Fprintf(&b, "S %d %d %s\n", c.Pos, u, order)
		case tape.Cancel:
			fmt.Fprintf(&b, "C %d %d %d\n", c.Pos, u, c.Target)
		case tape.Reduce:
			fmt.Fprintf(&b, "R %d %d %d %d\n", c.Pos, u, c.Target, c.NewQty)
		case tape.Replace:
			fmt.Fprintf(&b, "P %d %d %d %s\n", c.Pos, u, c.Target, order)
		}
	}
	return b.String()
}

// digestDriverOutput names everything the driver printed by tape position, with the
// same rule the native drivers use, and runs it through the OBDG encoder. Trades for
// both chains come from the event stream on this path (docs/C-API.md §3).
func digestDriverOutput(tp *Tape, out []byte) (Digest, error) {
	pos := map[int64]uint64{}
	dg := newDigester(tp.FileSHA256, false)
	var cur *outcome
	var term terminal
	flush := func() error {
		if cur == nil {
			return nil
		}
		err := dg.command(*cur)
		cur = nil
		return err
	}
	ints := func(f []string) ([]int64, error) {
		v := make([]int64, len(f))
		for i, s := range f {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, err
			}
			v[i] = n
		}
		return v, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		v, err := ints(f[1:])
		if err != nil {
			return Digest{}, fmt.Errorf("driver line %q: %v", sc.Text(), err)
		}
		switch f[0] {
		case "C":
			if err := flush(); err != nil {
				return Digest{}, err
			}
			cur = &outcome{pos: uint64(v[0]), status: uint8(v[1]), reason: uint8(v[2])}
		case "E":
			kind, reason, agg, id := v[0], v[1], v[2], v[3]
			switch kind {
			case 1, 2:
				if _, seen := pos[id]; !seen {
					pos[id] = cur.pos
				}
				tag := byte(tagAccepted)
				if kind == 2 {
					tag = tagRejected
				}
				cur.events = append(cur.events, dEvent{tag: tag, ref: ref{pos: pos[id]}, reason: uint8(reason)})
			case 4, 5:
				p, seen := pos[id]
				if !seen {
					return Digest{}, fmt.Errorf("command %d: event names order %d, never seen", cur.pos, id)
				}
				tag := byte(tagCanceled)
				if kind == 5 {
					tag = tagReplaced
				}
				cur.events = append(cur.events, dEvent{tag: tag, ref: ref{pos: p}})
			case 3:
				maker, ok1 := pos[v[7]]
				taker, ok2 := pos[v[8]]
				if !ok1 || !ok2 {
					return Digest{}, fmt.Errorf("command %d: trade names an unseen order", cur.pos)
				}
				a := byte('B')
				if agg == 1 {
					a = 'S'
				}
				tr := dTrade{price: v[5], qty: v[6], maker: ref{pos: maker}, taker: ref{pos: taker}, aggressor: a}
				cur.events = append(cur.events, dEvent{tag: tagTrade, trade: tr})
				cur.trades = append(cur.trades, tr)
			default:
				return Digest{}, fmt.Errorf("unknown event kind %d", kind)
			}
		case "L":
			if err := flush(); err != nil {
				return Digest{}, err
			}
			p, ok := pos[v[0]]
			if !ok {
				return Digest{}, fmt.Errorf("resting order %d never seen", v[0])
			}
			term.book = append(term.book, restingOrder{ref: ref{pos: p}, sell: v[2] == 1, price: v[3], qty: v[4], filled: v[5]})
		case "T":
			if err := flush(); err != nil {
				return Digest{}, err
			}
			term.lastTrade = v[0]
		case "X":
			return Digest{}, fmt.Errorf("the driver reports a failed call: %s", sc.Text())
		}
	}
	if err := flush(); err != nil {
		return Digest{}, err
	}
	return dg.finish(term), nil
}
