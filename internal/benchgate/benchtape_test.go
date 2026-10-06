package benchgate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/refmatch"
	"github.com/intrepidkarthi/orderbook/internal/tape"
)

// The committed bench tape (docs/BENCH-GATE.md §2). The file is the contract and
// the seed is provenance: another implementation reads the file, and these tests
// keep the file honest about where it came from.
const (
	benchTapeFile = "testdata/bench-v1.obt"
	benchSeed     = 0x5EED1234
	benchN        = 50_000
	benchCapacity = 1_000_000
	// benchTapeSHA256 pins the whole file. A frozen tape is never edited: a new
	// workload is a new file (bench-v2.obt), and this one stays for as long as
	// anything names it.
	benchTapeSHA256 = "d6e01e40255ea37911b2974ddbe62ef8fee0ab818986f9a65e5688c6be0d2ef7"
)

const generatorLine = "internal/tape lcg mul=6364136223846793005 inc=1442695040888963407 shr=11"

func benchHeader() Header {
	return Header{
		Generator:  generatorLine,
		Provenance: fmt.Sprintf("profile=%s seed=%#x n=%d", tape.Bench.Name, uint64(benchSeed), benchN),
		MaxOrders:  benchCapacity,
	}
}

func readBenchTape(t testing.TB) ([]byte, *Tape) {
	t.Helper()
	b, err := os.ReadFile(benchTapeFile)
	if err != nil {
		t.Fatalf("%v (write it once with BENCHGATE_WRITE_TAPE=1)", err)
	}
	tp, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return b, tp
}

// TestWriteBenchTape writes the tape when asked to and does nothing otherwise. It
// refuses to overwrite: a frozen file changes by being replaced with a new version.
func TestWriteBenchTape(t *testing.T) {
	if os.Getenv("BENCHGATE_WRITE_TAPE") != "1" {
		t.Skip("set BENCHGATE_WRITE_TAPE=1 to write " + benchTapeFile)
	}
	if _, err := os.Stat(benchTapeFile); err == nil {
		t.Fatalf("%s exists and is frozen; cut a new version instead of rewriting it", benchTapeFile)
	}
	b, err := Marshal(benchHeader(), tape.Gen(tape.Bench, benchSeed, benchN))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(benchTapeFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(benchTapeFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	t.Logf("wrote %s: %d bytes, sha256 %x", benchTapeFile, len(b), sum)
}

// TestBenchTapeFileIsFrozen pins the file's bytes and size.
func TestBenchTapeFileIsFrozen(t *testing.T) {
	b, _ := readBenchTape(t)
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != benchTapeSHA256 {
		t.Fatalf("%s hashes to %s, pinned %s. A frozen tape is never edited; cut bench-v2.obt instead.",
			benchTapeFile, got, benchTapeSHA256)
	}
	if len(b) > 4<<20 {
		t.Fatalf("%s is %d bytes, over the 4 MiB the spec allows", benchTapeFile, len(b))
	}
}

// TestBenchTapeMatchesGenerator regenerates the tape from its provenance line and
// requires the same bytes. The file stays the contract either way; this says
// whether the generator that made it still does.
func TestBenchTapeMatchesGenerator(t *testing.T) {
	b, tp := readBenchTape(t)
	prov := map[string]string{}
	for _, f := range strings.Fields(tp.Provenance) {
		k, v, _ := strings.Cut(f, "=")
		prov[k] = v
	}
	seed, err := strconv.ParseUint(strings.TrimPrefix(prov["seed"], "0x"), 16, 64)
	if err != nil {
		t.Fatalf("provenance seed %q: %v", prov["seed"], err)
	}
	n, err := strconv.Atoi(prov["n"])
	if err != nil {
		t.Fatalf("provenance n %q: %v", prov["n"], err)
	}
	if prov["profile"] != tape.Bench.Name || tp.Generator != generatorLine {
		t.Fatalf("provenance %q / generator %q do not name tape.Bench", tp.Provenance, tp.Generator)
	}
	again, err := Marshal(tp.Header, tape.Gen(tape.Bench, seed, n))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(b) {
		t.Fatalf("generator moved: tape.Gen(bench, %#x, %d) no longer reproduces %s; "+
			"leave v1 frozen and cut bench-v2.obt", seed, n, benchTapeFile)
	}
}

// shape is what replaying the tape through refmatch produced.
type shape struct {
	submits, rejected, bookFull    int
	targeted, reachedLive          int
	trades, commands, restingAtEnd int
	peakResting                    int
	stp                            [6]int
}

func replayShape(t testing.TB, tp *Tape, capacity int64) shape {
	t.Helper()
	d := newRefDriver(capacity, len(tp.Cmds))
	var s shape
	for _, c := range tp.Cmds {
		r, err := d.apply(c)
		if err != nil {
			t.Fatal(err)
		}
		s.commands++
		s.trades += len(r.Trades)
		if r.Verdict.Reason == refmatch.RejectOrderBookFull {
			s.bookFull++
		}
		switch c.Kind {
		case tape.Submit:
			s.submits++
			if r.Verdict.Status == refmatch.StatusRejected {
				s.rejected++
			}
		default:
			s.targeted++
			if r.Verdict.Reason != refmatch.RejectOrderNotFound && r.Verdict.Reason != refmatch.RejectOrderNotActive {
				s.reachedLive++
			}
		}
		if n := len(d.m.Book()); n > s.peakResting {
			s.peakResting = n
		}
	}
	s.restingAtEnd = len(d.m.Book())
	s.stp = d.m.STPDecisions()
	return s
}

// TestBenchTapeShape holds the tape to the floors the spec promises (§2.1), so a
// tape that stops being a workload says so instead of benchmarking an empty book.
func TestBenchTapeShape(t *testing.T) {
	_, tp := readBenchTape(t)
	s := replayShape(t, tp, tp.MaxOrders)
	t.Logf("shape: %d commands, %d submits (%d rejected), %d targeted (%d reached a live order), "+
		"%d trades, %d resting at the end, peak %d", s.commands, s.submits, s.rejected,
		s.targeted, s.reachedLive, s.trades, s.restingAtEnd, s.peakResting)
	if s.bookFull != 0 {
		t.Errorf("%d commands refused for a full book at capacity %d; the capacity must never bind", s.bookFull, tp.MaxOrders)
	}
	if s.rejected*100 > s.submits*5 {
		t.Errorf("%d of %d submits rejected, over the 5%% floor", s.rejected, s.submits)
	}
	if s.stp != [6]int{} {
		t.Errorf("self-trade prevention decided %v on a Portable tape; no account may meet itself", s.stp)
	}
	if s.reachedLive*10 < s.targeted {
		t.Errorf("only %d of %d targeted commands reached a live order, under 10%%", s.reachedLive, s.targeted)
	}
	if s.restingAtEnd < 1000 {
		t.Errorf("%d orders resting at the end, under 1,000", s.restingAtEnd)
	}
	if s.trades*4 < s.commands {
		t.Errorf("%d trades over %d commands, under one per four", s.trades, s.commands)
	}
}
