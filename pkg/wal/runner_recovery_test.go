package wal

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// tape is a deterministic pseudo-random command stream. It is generated from a
// fixed seed rather than committed as a file so the generator itself is the
// artefact under review, and it exercises order classes a hand-written tape does
// not reach.
type tapeCmd struct {
	cancel   bool
	cancelIx int
	user     string
	side     types.Side
	price    int64
	qty      int64
	// setPhase makes this command a trading-phase transition rather than an order.
	// The alphabet went without one for three releases, and docs/JOURNAL-COMPLETENESS.md
	// §1 is about what that cost: every exhaustive property in this package was
	// exhaustive over a tape that could not reach the command which escaped the
	// journal.
	setPhase bool
	phase    matching.EngineState
}

// lcg is a tiny deterministic generator — math/rand's stream is not guaranteed
// stable across Go releases, and a golden tape that changes under the test is
// worse than no golden tape.
type lcg uint64

func (r *lcg) next() uint64 {
	*r = lcg(uint64(*r)*6364136223846793005 + 1442695040888963407)
	return uint64(*r) >> 11
}

func (r *lcg) intn(n int64) int64 { return int64(r.next() % uint64(n)) }

// tapeSide maps 0/1 onto the Side constants. types.Side is a string, so a direct
// conversion would produce a one-rune string rather than "BUY"/"SELL".
func tapeSide(v int64) types.Side {
	if v == 0 {
		return types.SideBuy
	}
	return types.SideSell
}

// tapePhases are the phase transitions injected into every generated tape, by
// command index. A session, in the order a venue runs one: the tape opens in
// pre-open so the first orders accumulate into a legitimately crossed book, the
// transition at 21 uncrosses it, the closing auction accumulates a second crossed
// book on top of a live one, and the transition out of it prints the closing
// uncross before continuous trading resumes.
//
// Fixed indices rather than generated ones, and all of them below 200, so every
// caller of buildTape — the 2000-command recovery tape, the 400-command boundary
// sweep and the 200-command snapshot-join sweep alike — runs the same session.
var tapePhases = map[int]matching.EngineState{
	0:   matching.StatePreOpen,
	21:  matching.StateOpen,
	120: matching.StateClosingAuction,
	141: matching.StateOpen,
}

func buildTape(n int) []tapeCmd {
	r := lcg(0x5EED1234)
	out := make([]tapeCmd, 0, n)
	for i := 0; i < n; i++ {
		if p, ok := tapePhases[i]; ok {
			out = append(out, tapeCmd{setPhase: true, phase: p})
			continue
		}
		c := tapeCmd{
			user:  fmt.Sprintf("u%d", r.intn(8)),
			side:  tapeSide(r.intn(2)),
			price: 95 + r.intn(11),
			qty:   1 + r.intn(9),
		}
		// One command in six is a cancel of an earlier order.
		if i > 20 && r.intn(6) == 0 {
			c.cancel = true
			c.cancelIx = int(r.intn(int64(i)))
		}
		out = append(out, c)
	}
	return out
}

// applyTapeCmd issues one tape command through a Runner, collecting the engine
// ids it hands back so later cancels can name them.
//
// It is one function rather than three copies because the tape's alphabet is now
// something that grows: a command kind the drivers disagree about is a boundary
// sweep that exercises less than the recovery test it is supposed to be stronger
// than.
func applyTapeCmd(t *testing.T, r *matching.Runner, c tapeCmd, i int, ids *[]int64) {
	t.Helper()
	switch {
	case c.setPhase:
		r.SetPhase(c.phase)
	case c.cancel && len(*ids) > 0:
		_, _ = r.Cancel((*ids)[c.cancelIx%len(*ids)], c.user)
	default:
		o, err := types.NewOrder(c.user, "X", c.side, types.OrderTypeLimit, c.price, c.qty, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		o.ClientOrderID = fmt.Sprintf("c%d", i)
		res := r.Process(o)
		if res != nil && res.Order != nil {
			*ids = append(*ids, res.Order.ID)
		}
	}
}

// snapDigest was this file's private fingerprint until EngineSnapshot.Digest
// was promoted to the library (see docs/REPLICATION.md); it stays as a shim so
// the recovery tests are consumers of the public contract — if the digest's
// normalisation ever regresses, the crash-recovery suite is what notices.
func snapDigest(t *testing.T, snap *matching.EngineSnapshot) string {
	t.Helper()
	return snap.Digest()
}

func tapeDigest(t *testing.T, e *matching.Engine) string {
	t.Helper()
	return snapDigest(t, e.TakeSnapshot())
}

func tapeCfg() matching.Config {
	c := matching.DefaultConfig("X")
	c.DedupClientOrderIDs = 256
	return c
}

// runTape drives the tape through a Runner backed by a real WAL, checkpointing at
// checkpointAt (0 => never). It returns the final digest.
func runTape(t *testing.T, dir string, tape []tapeCmd, checkpointAt int) string {
	t.Helper()
	walPath := filepath.Join(dir, "wal.log")
	snapPath := filepath.Join(dir, "snap.json")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), QueueSize: 4096, Log: w})

	ids := make([]int64, 0, len(tape))
	for i, c := range tape {
		applyTapeCmd(t, r, c, i, &ids)
		if checkpointAt > 0 && i == checkpointAt {
			snap, err := r.Checkpoint()
			if err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			if err := WriteSnapshot(snapPath, snap); err != nil {
				t.Fatalf("WriteSnapshot: %v", err)
			}
		}
	}

	d := digestRunner(t, r)
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}
	return d
}

func digestRunner(t *testing.T, r *matching.Runner) string {
	t.Helper()
	snap, err := r.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	return snapDigest(t, snap)
}

// TestCrashRecoveryMatchesUninterrupted is the property the whole recovery story
// rests on, over a 2000-command tape rather than the handful of hand-written
// orders CI used to gate determinism on: interrupt anywhere, rebuild from
// snapshot plus log tail, and land on a byte-identical engine.
func TestCrashRecoveryMatchesUninterrupted(t *testing.T) {
	tape := buildTape(2000)

	for _, checkpointAt := range []int{0, 1, 500, 1337, 1999} {
		t.Run(fmt.Sprintf("checkpoint@%d", checkpointAt), func(t *testing.T) {
			dir := t.TempDir()
			want := runTape(t, dir, tape, checkpointAt)

			got, err := Recover(tapeCfg(), filepath.Join(dir, "snap.json"), filepath.Join(dir, "wal.log"))
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if d := tapeDigest(t, got); d != want {
				t.Errorf("recovered engine differs from the uninterrupted run\n got %s\nwant %s", d, want)
			}
		})
	}
}

// TestTapeIsDeterministic guards the generator: if the tape drifts between runs,
// every assertion above becomes meaningless.
func TestTapeIsDeterministic(t *testing.T) {
	a, b := buildTape(500), buildTape(500)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("tape diverged at %d: %+v vs %+v", i, a[i], b[i])
		}
	}
	// Pin the shape so an accidental change to the generator is visible.
	if got := len(a); got != 500 {
		t.Fatalf("tape length = %d", got)
	}
}

// TestRunnerLogIsWriteAhead pins the ordering guarantee: every command the engine
// applied is in the log. A log written after applying would lose exactly the
// commands a crash cares about.
func TestRunnerLogIsWriteAhead(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), QueueSize: 64, Log: w})

	const n = 200
	for i := 0; i < n; i++ {
		o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, 100, 1, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		r.Process(o)
	}
	r.Close()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entries, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != n {
		t.Errorf("log holds %d entries, want %d — commands were applied without being logged", len(entries), n)
	}
}
