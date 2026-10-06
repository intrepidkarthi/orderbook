package benchgate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/tape"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

// --- the vector ------------------------------------------------------------------

// vectorCmds is a hand-written six-command tape: a resting order, a crossing order
// that trades, a reduce, a cancel of an order that has already filled, a replace,
// and an IOC remainder.
var vectorCmds = []tape.Cmd{
	{Pos: 0, Kind: tape.Submit, User: "u3", Sell: true, Price: 101, Qty: 5},
	{Pos: 1, Kind: tape.Submit, User: "u2", Price: 101, Qty: 2},
	{Pos: 2, Kind: tape.Reduce, User: "u3", Target: 0, NewQty: 4},
	{Pos: 3, Kind: tape.Cancel, User: "u2", Target: 1},
	{Pos: 4, Kind: tape.Replace, User: "u3", Sell: true, Target: 0, Price: 103, Qty: 3},
	{Pos: 5, Kind: tape.Submit, User: "u2", Price: 102, Qty: 2, TIF: 1},
}

const vectorFile = "testdata/vector-v1.obt"

var vectorHeader = Header{Generator: "hand-written", Provenance: "docs/BENCH-GATE.md §3.3 vector", MaxOrders: 1000}

// w builds expected bytes straight from the spec's layout table, with its own
// helpers, so the vector checks the encoder against the document rather than
// against itself.
type w struct{ b []byte }

func (x *w) tag(c byte) *w   { x.b = append(x.b, c); return x }
func (x *w) u8(v uint8) *w   { x.b = append(x.b, v); return x }
func (x *w) u64(v uint64) *w { x.b = binary.BigEndian.AppendUint64(x.b, v); return x }
func (x *w) i64(v int64) *w  { return x.u64(uint64(v)) }
func (x *w) ref(pos uint64) *w {
	return x.u64(pos).u8(0) // leg 0
}
func (x *w) trade(ord uint64, price, qty int64, maker, taker uint64, agg byte) *w {
	return x.tag('X').u64(ord).i64(price).i64(qty).ref(maker).ref(taker).u8(agg)
}

// expectedVector returns the three parts of each chain: header, the block's records,
// and the terminal record.
func expectedVector(tapeSHA [32]byte) (core, full [3][]byte) {
	header := append(append([]byte("OBDG"), 1), tapeSHA[:]...)

	var c, f w
	// 0: sell 5 @ 101 rests.
	c.tag('c').u64(0).u8(0)
	f.tag('C').u64(0).u8(1).u8(0).tag('A').ref(0)
	// 1: buy 2 @ 101 fills against 0.
	c.tag('c').u64(1).u8(0).trade(1, 101, 2, 0, 1, 'B')
	f.tag('C').u64(1).u8(3).u8(0).tag('A').ref(1).trade(1, 101, 2, 0, 1, 'B')
	// 2: reduce 0 to a total of 4 (2 filled, 2 left), keeping its place.
	c.tag('c').u64(2).u8(0)
	f.tag('C').u64(2).u8(6).u8(0).tag('P').ref(0)
	// 3: cancel 1, which has filled and left the book: not found, no event.
	c.tag('c').u64(3).u8(1)
	f.tag('C').u64(3).u8(5).u8(7)
	// 4: replace 0 with sell 3 @ 103: the cancel, then the replacement rests.
	c.tag('c').u64(4).u8(0)
	f.tag('C').u64(4).u8(1).u8(0).tag('D').ref(0).tag('A').ref(4)
	// 5: buy 2 @ 102 IOC meets nothing and its remainder is cancelled.
	c.tag('c').u64(5).u8(0)
	f.tag('C').u64(5).u8(4).u8(0).tag('A').ref(5).tag('D').ref(5)

	var t w
	t.tag('E').u64(1).tag('L').ref(4).u8('S').i64(103).i64(3).i64(0).u8('O').i64(101).u64(1)

	return [3][]byte{header, c.b, t.b}, [3][]byte{header, f.b, t.b}
}

// chainOf hashes the three parts the way §3.3 says: H0 = SHA256(header),
// H1 = SHA256(H0 || records), F = SHA256(H1 || terminal).
func chainOf(p [3][]byte) [32]byte {
	h0 := sha256.Sum256(p[0])
	h1 := sha256.Sum256(append(h0[:], p[1]...))
	return sha256.Sum256(append(h1[:], p[2]...))
}

func hexFile(p [3][]byte) []byte {
	return []byte(fmt.Sprintf("header %x\nrecords %x\nterminal %x\n", p[0], p[1], p[2]))
}

// TestDigestVector checks both drivers' encoder output against bytes written from
// the spec, and the committed files other implementations read against both.
func TestDigestVector(t *testing.T) {
	src, err := Marshal(vectorHeader, vectorCmds)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("BENCHGATE_UPDATE") == "1" {
		writeVector(t, src)
	}
	committed, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatalf("%v (write it with BENCHGATE_UPDATE=1)", err)
	}
	if !bytes.Equal(committed, src) {
		t.Fatalf("%s is not what the vector tape marshals to", vectorFile)
	}
	tp, err := Parse(committed)
	if err != nil {
		t.Fatal(err)
	}
	core, full := expectedVector(tp.FileSHA256)
	for name, want := range map[string][3][]byte{"core": core, "full": full} {
		got, err := os.ReadFile("testdata/vector-v1-" + name + ".hex")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, hexFile(want)) {
			t.Errorf("testdata/vector-v1-%s.hex does not hold the spec's bytes", name)
		}
	}

	for name, run := range map[string]func(*Tape, bool) (Digest, error){"engine": digestEngine, "refmatch": digestRefmatch} {
		d, err := run(tp, true)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := bytes.Join(core[:], nil); !bytes.Equal(d.coreRaw, want) {
			t.Errorf("%s core bytes:\n got %x\nwant %x", name, d.coreRaw, want)
		}
		if want := bytes.Join(full[:], nil); !bytes.Equal(d.fullRaw, want) {
			t.Errorf("%s full bytes:\n got %x\nwant %x", name, d.fullRaw, want)
		}
		if d.Core != chainOf(core) || d.Full != chainOf(full) {
			t.Errorf("%s: F does not chain the way §3.3 says", name)
		}
	}
	wantF := fmt.Sprintf("core %x\nfull %x\n", chainOf(core), chainOf(full))
	gotF, err := os.ReadFile("testdata/vector-v1-f.txt")
	if err != nil || string(gotF) != wantF {
		t.Errorf("testdata/vector-v1-f.txt does not hold the vector's F values:\n got %q\nwant %q", gotF, wantF)
	}
}

func writeVector(t *testing.T, src []byte) {
	t.Helper()
	if err := os.WriteFile(vectorFile, src, 0o644); err != nil {
		t.Fatal(err)
	}
	tp, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	core, full := expectedVector(tp.FileSHA256)
	for name, p := range map[string][3][]byte{"core": core, "full": full} {
		if err := os.WriteFile("testdata/vector-v1-"+name+".hex", hexFile(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := fmt.Sprintf("core %x\nfull %x\n", chainOf(core), chainOf(full))
	if err := os.WriteFile("testdata/vector-v1-f.txt", []byte(f), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- the bench tape --------------------------------------------------------------

const benchDigestFile = "testdata/bench-v1.digest"

// TestBenchTapeDigest replays the committed tape through the engine and holds it to
// the committed digest, under the rules in digestrules.go.
func TestBenchTapeDigest(t *testing.T) {
	_, tp := readBenchTape(t)
	d, err := DigestEngine(tp)
	if err != nil {
		t.Fatal(err)
	}
	var recorded *DigestFile
	if b, err := os.ReadFile(benchDigestFile); err == nil {
		if recorded, err = ParseDigestFile(b); err != nil {
			t.Fatal(err)
		}
	}
	verdict, err := checkDigest(recorded, matching.SemanticsVersion, tp.FileSHA256, d, os.Getenv("BENCHGATE_UPDATE") == "1")
	switch verdict {
	case digestWrite:
		out := FormatDigestFile(matching.SemanticsVersion, "bench-v1.obt", tp.FileSHA256, d)
		if err := os.WriteFile(benchDigestFile, out, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s: core %x full %x, %d resting, %d trades", benchDigestFile, d.Core, d.Full, d.Resting, d.Trades)
	case digestRefused:
		if recorded != nil {
			t.Errorf("first block that differs: %s", firstDiff(recorded.CoreBlocks, d.CoreBlocks, recorded.FullBlocks, d.FullBlocks))
		}
		t.Fatal(err)
	}
	if d.Trades != d.CoreTrades {
		t.Fatalf("the event stream printed %d trades and the returned trade lists %d", d.Trades, d.CoreTrades)
	}
}

// TestBenchTapeRefmatchAgrees replays the tape through the reference matcher and
// requires the same digest, chain by chain and block by block. refmatch runs as
// shard 1, so its ids differ from the engine's; only naming by position can make
// the two agree.
func TestBenchTapeRefmatchAgrees(t *testing.T) {
	_, tp := readBenchTape(t)
	e, err := DigestEngine(tp)
	if err != nil {
		t.Fatal(err)
	}
	r, err := DigestRefmatch(tp)
	if err != nil {
		t.Fatal(err)
	}
	if e.Core != r.Core || e.Full != r.Full {
		t.Fatalf("engine and refmatch disagree on the bench tape; first block that differs: %s",
			firstDiff(e.CoreBlocks, r.CoreBlocks, e.FullBlocks, r.FullBlocks))
	}
	if e.Resting != r.Resting || e.Trades != r.Trades {
		t.Fatalf("engine %d resting / %d trades, refmatch %d / %d", e.Resting, e.Trades, r.Resting, r.Trades)
	}
}

// firstDiff names the first block, and the chain, where two runs part.
func firstDiff(ac, bc, af, bf [][32]byte) string {
	for i := 0; i < len(af) || i < len(bf); i++ {
		if i >= len(af) || i >= len(bf) {
			return fmt.Sprintf("block %d exists on one side only", i+1)
		}
		if ac[i] != bc[i] || af[i] != bf[i] {
			chains := []string{}
			if ac[i] != bc[i] {
				chains = append(chains, "core")
			}
			if af[i] != bf[i] {
				chains = append(chains, "full")
			}
			return fmt.Sprintf("block %d (commands %d-%d), chain %s", i+1, i*blockSize, (i+1)*blockSize-1, strings.Join(chains, "+"))
		}
	}
	return "none: the blocks agree, so the terminal record differs"
}

// TestDigestFileRules is §3.5's table.
func TestDigestFileRules(t *testing.T) {
	var tapeSHA [32]byte
	tapeSHA[0] = 1
	d := Digest{Core: [32]byte{2}, Full: [32]byte{3}}
	file := func(sem int, core byte) *DigestFile {
		return &DigestFile{Semantics: sem, TapeSHA256: tapeSHA, Core: [32]byte{core}, Full: [32]byte{3}}
	}
	cases := []struct {
		name     string
		recorded *DigestFile
		update   bool
		want     digestVerdict
	}{
		{"no file, no update", nil, false, digestRefused},
		{"no file, update", nil, true, digestWrite},
		{"same version, same digest", file(4, 2), false, digestOK},
		{"same version, different digest", file(4, 9), false, digestRefused},
		{"same version, update refused", file(4, 9), true, digestRefused},
		// The case that isolates the refusal: nothing differs, so only the rule
		// against rewriting at an unchanged version can refuse it. Without this row
		// a sabotaged refusal stayed green, because the different-digest row above is
		// refused by the mismatch check either way.
		{"same version, same digest, update refused", file(4, 2), true, digestRefused},
		{"older version, no update", file(3, 2), false, digestRefused},
		{"older version, update", file(3, 9), true, digestWrite},
		{"newer version: a downgrade", file(5, 2), true, digestRefused},
		{"different tape", &DigestFile{Semantics: 4, TapeSHA256: [32]byte{9}, Core: [32]byte{2}, Full: [32]byte{3}}, false, digestRefused},
	}
	for _, tc := range cases {
		if got, err := checkDigest(tc.recorded, 4, tapeSHA, d, tc.update); got != tc.want {
			t.Errorf("%s: got verdict %d (%v), want %d", tc.name, got, err, tc.want)
		}
	}
}

// TestDigestFileRoundTrip: the file reader accepts exactly what the writer writes.
func TestDigestFileRoundTrip(t *testing.T) {
	d := Digest{Core: [32]byte{1}, Full: [32]byte{2}, CoreBlocks: [][32]byte{{3}, {4}}, FullBlocks: [][32]byte{{5}, {6}}, Resting: 7, Trades: 8}
	b := FormatDigestFile(4, "x.obt", [32]byte{9}, d)
	df, err := ParseDigestFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if df.Core != d.Core || df.Full != d.Full || len(df.FullBlocks) != 2 || df.Resting != 7 || df.Trades != 8 || df.Semantics != 4 {
		t.Fatalf("round trip lost something: %+v", df)
	}
	if _, err := ParseDigestFile(bytes.Replace(b, []byte("obdigest 1"), []byte("obdigest 2"), 1)); err == nil {
		t.Fatal("an unknown digest version was accepted")
	}
}

// TestEncoderRefusesWhatItCannotName: the three hard errors §3.1 promises instead of
// guesses.
func TestEncoderRefusesWhatItCannotName(t *testing.T) {
	if _, err := engineReason(errors.New("a reason nobody mapped")); err == nil {
		t.Error("an unmapped engine error got a format code")
	}
	d := newEngineDriver(10, 1)
	d.log.evs = []matching.Event{{Kind: matching.EventCanceled, OrderID: 42}}
	if err := d.events(tape.Cmd{Pos: 0}, &outcome{}); err == nil {
		t.Error("an event naming an id never seen was named anyway")
	}
	d.pos[42] = 0
	d.log.evs = []matching.Event{{Kind: matching.EventCanceled, OrderID: 42, Reason: errors.New("expired")}}
	if err := d.events(tape.Cmd{Pos: 0}, &outcome{}); err == nil {
		t.Error("a cancel carrying a reason was encoded, though v1 has no byte for it")
	}
	g := newDigester([32]byte{}, false)
	if err := g.command(outcome{events: []dEvent{{tag: 'H'}}}); err == nil {
		t.Error("a reserved event tag was encoded")
	}
}
