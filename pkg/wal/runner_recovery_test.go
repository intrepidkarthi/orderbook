package wal

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/tape"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The generator that used to live here — tapeCmd, lcg, tapeSide, tapePhases and
// buildTape — now lives in internal/tape, and this package is a consumer.
//
// It moved rather than being copied, and the reason is one this repository has
// already paid for once. Two generators means two alphabets, means one of them is
// extended and the other is not, means the sweep that claims to be the stronger test
// is exercising less than the one it is supposed to dominate. The comment on the old
// applyTapeCmd already said this; the move makes it structural, and the differential
// harness in pkg/matching now draws from the same generator.
//
// The recovery alphabet is deliberately a SUPERSET of the differential one
// (tape.Recovery vs tape.Differential): the replay oracle can check kinds the
// reference model does not model, and narrowing this sweep because the model is
// behind would be the exact trade docs/REFERENCE-MATCHER.md exists to argue against.
//
// "Superset" has two axes — the command KIND and the order PAYLOAD — and this sweep
// shipped satisfying only the first. See tape.Recovery's comment for the measurement.
// walOrder below now carries the whole tier-1 payload onto the journal, and
// TestRecoveryTapeSpeaksTheTierOneAlphabet asserts it by outcome rather than by draw.

// recoveryTape is the boundary sweeps' input: one alphabet, one seed, whatever
// length the caller needs.
func recoveryTape(n int) []tape.Cmd { return tape.Gen(tape.Recovery, 0x5EED1234, n) }

// walPhase maps the tape's own phase enum onto the engine's. The tape may not import
// pkg/matching — it is shared with a reference model that must stay independent of
// it — so the translation lives at each consumer, and this is pkg/wal's.
func walPhase(p tape.Phase) matching.EngineState {
	switch p {
	case tape.PhasePreOpen:
		return matching.StatePreOpen
	case tape.PhaseClosingAuction:
		return matching.StateClosingAuction
	case tape.PhaseHalted:
		return matching.StateHalted
	case tape.PhaseCancelOnly:
		return matching.StateCancelOnly
	case tape.PhaseClosed:
		return matching.StateClosed
	default:
		return matching.StateOpen
	}
}

// walOrder builds the order a Submit or Replace command carries.
func walOrder(t *testing.T, c tape.Cmd) *types.Order {
	t.Helper()
	side := types.SideBuy
	if c.Sell {
		side = types.SideSell
	}
	ot := types.OrderTypeLimit
	if c.MarketOrd {
		ot = types.OrderTypeMarket
	}
	tif := types.TIFGoodTillCancel
	switch c.TIF {
	case 1:
		tif = types.TIFImmediateOrCancel
	case 2:
		tif = types.TIFFillOrKill
	}
	o, err := types.NewOrder(c.User, "X", side, ot, c.Price, c.Qty, tif)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	o.PostOnly = c.PostOnly
	// The rest of the tier-1 payload. These three decide whether self-trade
	// prevention fires at all (STPMode), whether it fires ACROSS accounts
	// (TradeGroupID), and whether it is bypassed (Privileged) — so a boundary sweep
	// that dropped them, as this one silently did, never replays an STP-cancelled
	// maker across a crash. Each has a json tag on types.Order, so each survives the
	// journal; a field that did not would fail this sweep at the first boundary,
	// which is the point.
	o.TradeGroupID = c.TradeGroup
	o.Privileged = c.Privileged
	switch c.STP {
	case 1:
		o.STPMode = string(matching.STPCancelNewest)
	case 2:
		o.STPMode = string(matching.STPCancelOldest)
	case 3:
		o.STPMode = string(matching.STPCancelBoth)
	case 4:
		o.STPMode = string(matching.STPDecrement)
	case 5:
		o.STPMode = string(matching.STPAllow)
	case 0:
	default:
		// No catch-all. An unmapped mode arriving as "venue default" would turn this
		// half of the sweep off for exactly the orders it was widened for.
		t.Fatalf("tape drew STP mode %d, which pkg/wal's driver does not map", c.STP)
	}
	// One client id per tape POSITION, so the duplicate guard sees a fresh key for
	// every submit and a replay of the same tape sees the same keys in the same
	// order.
	o.ClientOrderID = fmt.Sprintf("c%d", c.Pos)
	return o
}

// applyTapeCmd issues one tape command through a Runner, remembering the engine id
// each submit was given so a later command can name it by tape POSITION.
//
// Position, not "index into the ids collected so far", which is what the superseded
// generator used. That difference is what makes a tape deletion-closed: deleting a
// submit leaves later commands naming a position that produced no order, which is a
// well-formed command with a predictable rejection, instead of silently retargeting
// every later cancel onto a different order.
//
// It is one function rather than four copies because the tape's alphabet is
// something that grows: a command kind the drivers disagree about is a boundary
// sweep that exercises less than the recovery test it is supposed to be stronger
// than.
func applyTapeCmd(t *testing.T, r *matching.Runner, c tape.Cmd, ids map[int]int64) {
	t.Helper()
	switch c.Kind {
	case tape.SetPhase:
		r.SetPhase(walPhase(c.Phase))
	case tape.Halt:
		r.Halt()
	case tape.Resume:
		r.Resume()
	case tape.CancelOnly:
		r.SetCancelOnly()
	case tape.CancelAll:
		if _, err := r.CancelAllForUser(c.User); err != nil {
			t.Fatalf("CancelAllForUser: %v", err)
		}
	case tape.Cancel:
		_, _ = r.Cancel(ids[c.Target], c.User)
	case tape.Reduce:
		_, _ = r.Reduce(ids[c.Target], c.NewQty, c.User)
	case tape.Replace:
		repl := walOrder(t, c)
		ch, err := r.TryReplaceAsync(ids[c.Target], c.User, repl)
		if err != nil {
			t.Fatalf("TryReplaceAsync: %v", err)
		}
		<-ch
		// Zero when the original could not be cancelled, in which case the
		// replacement was never submitted and consumed no id.
		if repl.ID != 0 {
			ids[c.Pos] = repl.ID
		}
	case tape.Submit:
		o := walOrder(t, c)
		res := r.Process(o)
		if res != nil && res.Order != nil {
			ids[c.Pos] = res.Order.ID
		}
	default:
		t.Fatalf("no pkg/wal driver for tape kind %s", c.Kind)
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
func runTape(t *testing.T, dir string, cmds []tape.Cmd, checkpointAt int) string {
	t.Helper()
	walPath := filepath.Join(dir, "wal.log")
	snapPath := filepath.Join(dir, "snap.json")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), QueueSize: 4096, Log: w})

	ids := make(map[int]int64, len(cmds))
	for i, c := range cmds {
		applyTapeCmd(t, r, c, ids)
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
	cmds := recoveryTape(2000)

	for _, checkpointAt := range []int{0, 1, 500, 1337, 1999} {
		t.Run(fmt.Sprintf("checkpoint@%d", checkpointAt), func(t *testing.T) {
			dir := t.TempDir()
			want := runTape(t, dir, cmds, checkpointAt)

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

// TestTapeIsDeterministic guards the generator FROM THIS PACKAGE'S SIDE: if the tape
// drifts between runs, every assertion above becomes meaningless. internal/tape has
// its own version of this; the duplication is deliberate, because the thing at risk
// is the tape pkg/wal actually drives, not the generator in the abstract.
func TestTapeIsDeterministic(t *testing.T) {
	a, b := recoveryTape(500), recoveryTape(500)
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
