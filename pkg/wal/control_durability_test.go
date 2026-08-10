package wal

import (
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

// A control command issued AFTER the last checkpoint has to survive recovery, and
// for four releases none of them did. Runner.logCommand ended with
//
//	default: return // control commands carry no book state; the snapshot covers them
//
// which is true only as of the snapshot. The window between the last checkpoint and
// the crash is exactly the window a write-ahead log exists to cover, and it was the
// one place these commands were not written down — so a venue an operator had
// deliberately halted came back Open, ready to trade, with nobody told.
//
// It is the same reasoning error as the WAL's durability comment corrected in
// v0.20.0: a guarantee stated against the wrong reference point. See
// docs/TRADE-BUST.md §3.5, which found it while looking for somewhere to put a bust.

// controlCase is one control command, how to issue it, and what the engine should
// look like afterwards — live and again after recovery.
type controlCase struct {
	name  string
	issue func(*matching.Runner)
	check func(*matching.EngineSnapshot) (ok bool, got string)
}

func controlCases() []controlCase {
	return []controlCase{
		{
			name:  "halt",
			issue: func(r *matching.Runner) { r.Halt() },
			check: func(s *matching.EngineSnapshot) (bool, string) {
				return s.State == matching.StateHalted, s.State.String()
			},
		},
		{
			name:  "cancel-only",
			issue: func(r *matching.Runner) { r.SetCancelOnly() },
			check: func(s *matching.EngineSnapshot) (bool, string) {
				return s.State == matching.StateCancelOnly, s.State.String()
			},
		},
		{
			// Resume after a halt: the pair has to replay in order, or a venue that
			// was halted and reopened comes back halted — the opposite failure, and
			// just as wrong.
			name:  "halt-then-resume",
			issue: func(r *matching.Runner) { r.Halt(); r.Resume() },
			check: func(s *matching.EngineSnapshot) (bool, string) {
				return s.State == matching.StateOpen, s.State.String()
			},
		},
		{
			name:  "mark-price",
			issue: func(r *matching.Runner) { r.SetMarkPrice(10_500) },
			check: func(s *matching.EngineSnapshot) (bool, string) {
				return s.MarkPrice == 10_500, formatInt(s.MarkPrice)
			},
		},
	}
}

// TestControlCommandsSurviveRecovery checkpoints while the venue is open and normal,
// issues the control command after the checkpoint, and asserts the recovered engine
// agrees with the live one.
func TestControlCommandsSurviveRecovery(t *testing.T) {
	for _, tc := range controlCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			walPath := filepath.Join(dir, "wal.log")
			snapPath := filepath.Join(dir, "snap.json")

			w, err := Open(walPath)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), Log: w})

			// Checkpoint first, so the snapshot cannot be what carries the change.
			snap, err := r.Checkpoint()
			if err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			if err := WriteSnapshot(snapPath, snap); err != nil {
				t.Fatalf("WriteSnapshot: %v", err)
			}

			tc.issue(r)

			live, err := r.Checkpoint()
			if err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			if ok, got := tc.check(live); !ok {
				t.Fatalf("live engine did not take the command: got %s", got)
			}
			r.Close()
			if err := w.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			w.Close()

			eng, err := Recover(tapeCfg(), snapPath, walPath)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if ok, got := tc.check(eng.TakeSnapshot()); !ok {
				t.Errorf("recovered engine lost the %s issued after the checkpoint: got %s", tc.name, got)
			}
		})
	}
}

// TestControlCommandsReplayInOrder pins the ordering, not just the final value. A
// log that recorded control commands but replayed them out of band — all the halts
// first, say — would pass the test above for a single command and still restore a
// venue to a state it was never in.
func TestControlCommandsReplayInOrder(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := matching.NewRunner(matching.RunnerConfig{Engine: tapeCfg(), Log: w})

	// Halted, reopened, halted again, cancel-only: ends cancel-only, and only an
	// in-order replay gets there.
	r.Halt()
	r.Resume()
	r.Halt()
	r.SetCancelOnly()
	r.SetMarkPrice(9_900)
	r.Close()
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	w.Close()

	// No snapshot: the whole log is the tail.
	eng, err := Recover(tapeCfg(), filepath.Join(dir, "absent.json"), walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := eng.TakeSnapshot()
	if got.State != matching.StateCancelOnly {
		t.Errorf("state after replay = %s, want CANCEL_ONLY", got.State)
	}
	if got.MarkPrice != 9_900 {
		t.Errorf("mark price after replay = %d, want 9900", got.MarkPrice)
	}
}

func formatInt(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
