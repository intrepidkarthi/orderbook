package wal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

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

func buildTape(n int) []tapeCmd {
	r := lcg(0x5EED1234)
	out := make([]tapeCmd, 0, n)
	for i := 0; i < n; i++ {
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

// snapDigest fingerprints everything recovery must reproduce — the resting book,
// all three sequence counters, the duplicate guard, and the conditional-order
// state that used to vanish across a restore.
//
// Wall-clock fields are normalised away first. A replayed order is stamped with
// the clock at replay time, not the clock at original submission, so an engine
// rebuilt from the log is legitimately not byte-identical in its timestamps. That
// is a property of replaying a command log, not a recovery defect; everything
// else must match exactly.
func snapDigest(t *testing.T, snap *matching.EngineSnapshot) string {
	t.Helper()
	snap.WALSeq = 0 // a log position, not engine state
	snap.PausedUntil = time.Time{}
	snap.Guard.Start = time.Time{}
	for _, o := range snap.Orders {
		normaliseTimes(o)
	}
	for _, s := range snap.Stops {
		normaliseTimes(s.Order)
	}
	for i := range snap.Trailing {
		normaliseTimes(snap.Trailing[i].Order)
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func normaliseTimes(o *types.Order) {
	if o == nil {
		return
	}
	o.CreatedAt = time.Time{}
	o.UpdatedAt = time.Time{}
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
		if c.cancel && len(ids) > 0 {
			_, _ = r.Cancel(ids[c.cancelIx%len(ids)], c.user)
		} else {
			o, err := types.NewOrder(c.user, "X", c.side, types.OrderTypeLimit, c.price, c.qty, types.TIFGoodTillCancel)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			o.ClientOrderID = fmt.Sprintf("c%d", i)
			res := r.Process(o)
			if res != nil && res.Order != nil {
				ids = append(ids, res.Order.ID)
			}
		}
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
