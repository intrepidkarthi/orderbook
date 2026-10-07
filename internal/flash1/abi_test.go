package flash1

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTheCABIEmitsWhatTheAdapterEmits builds cmd/flash1engine as a shared library and
// drives it from a C stand-in for the harness (testdata/driver.c) with a random stream,
// through both engine_on_batch and the one-message entry points. Every report and every
// audit answer must equal what this package emits for the same stream. The real
// harness and its header are exercised on CI only (docs/FLASH1.md §4).
func TestTheCABIEmitsWhatTheAdapterEmits(t *testing.T) {
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
	lib := "libflash1engine.so"
	if runtime.GOOS == "darwin" {
		lib = "libflash1engine.dylib"
	}
	build := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	build("go", "build", "-buildmode=c-shared", "-o", filepath.Join(dir, lib), "../../cmd/flash1engine")
	build(cc, "-O2", "testdata/driver.c", "-L", dir, "-lflash1engine", "-Wl,-rpath,"+dir, "-o", filepath.Join(dir, "driver"))

	input, want := stream(20000)
	cmd := exec.Command(filepath.Join(dir, "driver"))
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+dir, "DYLD_LIBRARY_PATH="+dir)
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	g, w := strings.Split(string(got), "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			t.Fatalf("line %d: through the C ABI %q, from the adapter %q", i+1, g[i], w[i])
		}
	}
	if len(g) != len(w) {
		t.Fatalf("through the C ABI %d lines, from the adapter %d", len(g), len(w))
	}
	t.Logf("%d lines equal across the boundary", len(w))
}

// stream renders n random messages as driver input, grouped into batches and single
// calls with audit queries between them, and returns what the adapter prints for them.
// Prices sit in a narrow band so orders cross often; cancels and modifies name both
// live and dead ids.
func stream(n int) (input, want string) {
	rng := rand.New(rand.NewSource(1))
	var in, out strings.Builder
	a := New(func(r Report) {
		fmt.Fprintf(&out, "R %d %d %d %d %d %d %d %d\n", r.Type, r.Side, r.Seq, r.OrderID, r.Price, r.Qty, r.Maker, r.Taker)
	})
	var seq, next uint64
	msg := func() {
		seq++
		price, qty, side := int64(990+rng.Intn(21)), uint32(1+rng.Intn(20)), uint8(rng.Intn(2))
		switch x := rng.Intn(100); {
		case x < 60 || next == 0:
			next++
			ioc := rng.Intn(10) == 0
			fmt.Fprintf(&in, "N %d %d %d %d %d %d\n", next, seq, price, qty, side, b2i(ioc))
			a.NewOrder(seq, next, price, qty, side, ioc)
		case x < 85:
			id := 1 + uint64(rng.Int63n(int64(next)+5))
			fmt.Fprintf(&in, "C %d %d\n", id, seq)
			a.Cancel(seq, id)
		default:
			id := 1 + uint64(rng.Int63n(int64(next)+5))
			fmt.Fprintf(&in, "M %d %d %d %d %d\n", id, seq, price, qty, side)
			a.Modify(seq, id, price, qty, side)
		}
	}
	for left := n; left > 0; {
		if rng.Intn(3) == 0 {
			in.WriteString("S\n")
			msg()
			left--
		} else {
			k := min(1+rng.Intn(300), left)
			fmt.Fprintf(&in, "B %d\n", k)
			for range k {
				msg()
			}
			left -= k
		}
		if rng.Intn(4) == 0 {
			p, s := int64(990+rng.Intn(21)), uint8(rng.Intn(2))
			fmt.Fprintf(&in, "Q %d %d\n", p, s)
			fmt.Fprintf(&out, "Q %d %d %d\n", a.BestBid(), a.BestAsk(), a.DepthAt(p, s))
		}
	}
	return in.String(), out.String()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
