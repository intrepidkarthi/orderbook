package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestARecoveredRunnerCheckpointsAtTheLogPositionItWasGiven.
//
// A checkpoint's WALSeq is the contract between one incarnation of a venue and the
// next: it says which records the snapshot already contains, and the following
// recovery replays only what is above it. The Runner is what stamps it, from
// lastApplied, and lastApplied started at zero for every Runner however the engine
// it was handed came to exist.
//
// So a Runner built over a RECOVERED engine — the whole point of NewRunnerFor —
// stamped a snapshot holding the complete book with a position covering none of it,
// for as long as it took the first command to arrive. One checkpoint inside that
// window is enough, and the checkpoint cadence is measured in tens of seconds.
//
// The assertion is a checkpoint taken with no command applied, which is the only
// state in which the seed is the only thing the answer can come from.
func TestARecoveredRunnerCheckpointsAtTheLogPositionItWasGiven(t *testing.T) {
	eng := NewEngine(DefaultConfig("X"))
	eng.Process(mustLimit(t, "mm", types.SideBuy, 100, 10))

	r := NewRunnerFor(eng, RunnerConfig{LastApplied: 41})
	defer r.Close()

	snap, err := r.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if snap.WALSeq != 41 {
		t.Errorf("the first checkpoint stamped WALSeq %d, want 41 — the recovered engine's log position was "+
			"dropped on the way into the Runner, so this snapshot claims to cover nothing and the next "+
			"recovery replays the whole log on top of it", snap.WALSeq)
	}
	if snap.Orders == nil || len(snap.Orders) != 1 {
		t.Fatalf("the snapshot holds %d orders, want the recovered one", len(snap.Orders))
	}
}

// TestAppliedCommandsStillOverrideTheSeed — the seed is a starting point, not a
// floor. Once the Runner has journalled a command it stamps that command's sequence,
// and a stale seed must not survive it.
func TestAppliedCommandsStillOverrideTheSeed(t *testing.T) {
	log := &countingLog{seq: 90}
	r := NewRunnerFor(NewEngine(DefaultConfig("X")), RunnerConfig{LastApplied: 41, Log: log})
	defer r.Close()

	r.Process(mustLimit(t, "mm", types.SideBuy, 100, 10))
	snap, err := r.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if snap.WALSeq != 91 {
		t.Errorf("WALSeq = %d, want 91 — the sequence of the command actually applied", snap.WALSeq)
	}
}

// TestAFreshRunnerStartsAtZero — the zero value has to keep meaning "the beginning
// of the log", because that is what NewRunner and a promoted follower opening a
// brand-new log both rely on.
func TestAFreshRunnerStartsAtZero(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	defer r.Close()

	snap, err := r.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if snap.WALSeq != 0 {
		t.Errorf("WALSeq = %d, want 0", snap.WALSeq)
	}
}

func mustLimit(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}
