package matching

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// digestEngine builds an engine holding every kind of state the digest claims to
// cover reachable through the plain command API: resting depth both sides, a
// partial fill, a cancel, and a pending stop.
func digestEngine(t *testing.T) *Engine {
	t.Helper()
	e := newEngine()
	e.Process(lim(t, "maker1", types.SideSell, 101, 5))
	e.Process(lim(t, "maker2", types.SideSell, 102, 3))
	e.Process(lim(t, "maker3", types.SideBuy, 99, 4))
	e.Process(lim(t, "taker", types.SideBuy, 101, 2)) // partial fill of maker1
	dead := lim(t, "maker4", types.SideBuy, 98, 1)
	e.Process(dead)
	if _, err := e.Cancel(dead.ID, "maker4"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	stop, err := types.NewStopOrder(lim(t, "stopper", types.SideSell, 97, 2), 98)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	e.ProcessStop(stop)
	return e
}

func cloneSnapshot(t *testing.T, snap *EngineSnapshot) *EngineSnapshot {
	t.Helper()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var c EngineSnapshot
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return &c
}

// TestDigestAgreesForTheSameCommands is the property replication stands on: two
// engines that applied the same tape have the same digest, even though their
// orders were created at different wall-clock times.
func TestDigestAgreesForTheSameCommands(t *testing.T) {
	a := digestEngine(t)
	time.Sleep(2 * time.Millisecond) // ensure the second run stamps different times
	b := digestEngine(t)
	da, db := a.TakeSnapshot().Digest(), b.TakeSnapshot().Digest()
	if da != db {
		t.Errorf("same commands, different digests:\n  a %s\n  b %s", da, db)
	}
}

// TestDigestAgreesAcrossRestore — snapshot, restore into a fresh engine, snapshot
// again: the digest must survive the round trip, or a follower bootstrapped from
// a primary's snapshot starts life already "diverged".
func TestDigestAgreesAcrossRestore(t *testing.T) {
	e := digestEngine(t)
	snap := e.TakeSnapshot()
	restored, err := RestoreEngine(Config{}, snap)
	if err != nil {
		t.Fatalf("RestoreEngine: %v", err)
	}
	if d1, d2 := snap.Digest(), restored.TakeSnapshot().Digest(); d1 != d2 {
		t.Errorf("digest did not survive restore:\n  before %s\n  after  %s", d1, d2)
	}
}

// TestDigestIgnoresWallClockButNothingElse — the normalised fields are exactly
// the ones a legitimate replay is allowed to differ in. Everything else must
// move the digest, so each meaningful perturbation is asserted to.
func TestDigestIgnoresWallClockButNothingElse(t *testing.T) {
	base := digestEngine(t).TakeSnapshot()
	want := base.Digest()

	// Legitimately different between an original and its replay: no change.
	clock := cloneSnapshot(t, base)
	clock.WALSeq = 9999
	clock.PausedUntil = time.Now()
	clock.Guard.Start = time.Now()
	clock.Orders[0].CreatedAt = time.Now()
	clock.Orders[0].UpdatedAt = time.Now()
	clock.Stops[0].Order.CreatedAt = time.Now()
	if got := clock.Digest(); got != want {
		t.Errorf("a wall-clock difference moved the digest")
	}

	// Divergence: every one of these must move it.
	perturb := map[string]func(*EngineSnapshot){
		"resting quantity": func(s *EngineSnapshot) { s.Orders[0].RemainingQty++ },
		"resting price":    func(s *EngineSnapshot) { s.Orders[0].Price++ },
		"an extra order":   func(s *EngineSnapshot) { s.Orders = append(s.Orders, s.Orders[0]) },
		"a missing stop":   func(s *EngineSnapshot) { s.Stops = nil },
		"order sequence":   func(s *EngineSnapshot) { s.Seq++ },
		"trade sequence":   func(s *EngineSnapshot) { s.TradeSeq++ },
		"event sequence":   func(s *EngineSnapshot) { s.EventSeq++ },
		"last trade price": func(s *EngineSnapshot) { s.LastTradePrice++ },
		"mark price":       func(s *EngineSnapshot) { s.MarkPrice++ },
		"engine state":     func(s *EngineSnapshot) { s.State = StateCancelOnly },
		"dedup guard":      func(s *EngineSnapshot) { s.DedupKeys = append(s.DedupKeys, "ghost") },
		"guard window":     func(s *EngineSnapshot) { s.Guard.Trades++ },
	}
	for name, mutate := range perturb {
		c := cloneSnapshot(t, base)
		mutate(c)
		if c.Digest() == want {
			t.Errorf("%s changed and the digest did not — divergence this digest cannot see", name)
		}
	}
}

// TestDigestDoesNotMutateTheSnapshot — the test helper this was promoted from
// normalised in place, which a public method must not: a caller who digests a
// snapshot and then writes it to disk must write the snapshot, not the
// normalisation.
func TestDigestDoesNotMutateTheSnapshot(t *testing.T) {
	snap := digestEngine(t).TakeSnapshot()
	snap.WALSeq = 42 // give the normaliser something it would visibly destroy
	before, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	snap.Digest()
	after, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Error("Digest modified its receiver")
	}
}
