// The drills from docs/REPLICATION.md §6, run on every CI pass. Each was
// verified to fail against deliberately broken code before it counted: D1/D2
// against a fanout that skips an entry instead of cutting the follower, D3
// against a Promote that skips the applied-sequence accounting, D4 against a
// registry that ignores the incarnation, D5 against a tape with the refusals
// removed, D6 against a fanout that blocks instead of shedding.
package main

import (
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// venue is what a tape drives — satisfied by both *matching.Runner (the
// primary) and *matching.Engine (the uninterrupted control), which is the
// point: the drills compare a replicated venue against a plain engine fed the
// identical commands.
type venue interface {
	Process(*types.Order) *matching.MatchResult
	Cancel(int64, string) (*types.Order, error)
}

func lim(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// tapeStep applies command i (1-based). One step is exactly one journalled
// command, so step index == log sequence — the arithmetic D3 depends on.
// The mix: resting orders on both sides, crossing takers, cancels of earlier
// orders, and two deliberate refusals (D5): a cancel of an order that never
// existed, and a cancel by the wrong owner. Write-ahead journalling records
// refused commands too; replay must refuse them identically.
func tapeStep(t *testing.T, v venue, i int) {
	t.Helper()
	switch {
	case i%13 == 0:
		_, _ = v.Cancel(999999, "ghost") // refused: no such order
	case i%11 == 0:
		_, _ = v.Cancel(int64(i-5), "mallory") // refused: not mallory's order
	case i%7 == 0:
		// Cancel an earlier maker order. It may already be gone (filled or
		// cancelled) — a refusal is fine; it must simply replay as one.
		_, _ = v.Cancel(int64(i-4), "maker")
	case i%3 == 0:
		v.Process(lim(t, "taker", types.SideBuy, 100+int64(i%4), 2))
	case i%2 == 0:
		v.Process(lim(t, "maker", types.SideBuy, 95+int64(i%5), 3))
	default:
		v.Process(lim(t, "maker", types.SideSell, 100+int64(i%5), 3))
	}
}

func runTape(t *testing.T, v venue, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		tapeStep(t, v, i)
	}
}

// controlDigest is the uninterrupted arm: a plain engine, no journal, no wire,
// fed tape steps 1..n.
func controlDigest(t *testing.T, n int) string {
	t.Helper()
	eng := matching.NewEngine(matching.DefaultConfig("BTC-USD"))
	runTape(t, eng, 1, n)
	return eng.TakeSnapshot().Digest()
}

func newPrimary(t *testing.T) *Primary {
	t.Helper()
	p, err := NewPrimary("BTC-USD", filepath.Join(t.TempDir(), "primary.wal"), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewPrimary: %v", err)
	}
	return p
}

// TestDrillD1_TheBooksAgree — a follower tailing from the start ends with the
// digest of an uninterrupted engine fed the same tape. This is the recovery
// suite's equality with the crash replaced by a wire.
func TestDrillD1_TheBooksAgree(t *testing.T) {
	p := newPrimary(t)
	defer p.Close()
	f, err := StartFollower("BTC-USD", p.Addr())
	if err != nil {
		t.Fatalf("StartFollower: %v", err)
	}
	const n = 120
	runTape(t, p.Runner, 1, n)
	if err := f.WaitApplied(int64(n), 10*time.Second); err != nil {
		t.Fatalf("follower never caught up: %v", err)
	}
	got, err := f.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	want := controlDigest(t, n)
	if got != want {
		t.Errorf("follower diverged from the uninterrupted control:\n  follower %s\n  control  %s", got, want)
	}
	if snap, err := p.Runner.Checkpoint(); err != nil || snap.Digest() != want {
		t.Errorf("primary and control disagree (err=%v) — the tape itself is broken", err)
	}
}

// TestDrillD2_MidStreamBootstrap — the case no recovery test covers: the
// snapshot is taken while the primary keeps trading, and the follower joins
// from it. The spec called this the most likely home of the fifth phantom.
func TestDrillD2_MidStreamBootstrap(t *testing.T) {
	p := newPrimary(t)
	defer p.Close()
	const before, after = 45, 75
	runTape(t, p.Runner, 1, before)

	f, err := StartFollower("BTC-USD", p.Addr())
	if err != nil {
		t.Fatalf("StartFollower: %v", err)
	}
	runTape(t, p.Runner, before+1, before+after)

	if err := f.WaitApplied(int64(before+after), 10*time.Second); err != nil {
		t.Fatalf("follower never caught up: %v", err)
	}
	got, err := f.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if want := controlDigest(t, before+after); got != want {
		t.Errorf("mid-stream bootstrap diverged:\n  follower %s\n  control  %s", got, want)
	}
}

// TestDrillD3_PromotionPreservesThePrefix — kill the primary, promote, and the
// promoted book equals an uninterrupted engine fed exactly the first
// Applied-many commands: everything the follower applied is present, nothing
// past it was invented.
func TestDrillD3_PromotionPreservesThePrefix(t *testing.T) {
	dir := t.TempDir()
	p := newPrimary(t)
	f, err := StartFollower("BTC-USD", p.Addr())
	if err != nil {
		p.Close()
		t.Fatalf("StartFollower: %v", err)
	}
	const n = 90
	runTape(t, p.Runner, 1, n)
	p.Close() // the crash — no waiting for the follower first

	promoted, err := f.Promote(filepath.Join(dir, "promoted.wal"), filepath.Join(dir, "promoted.snap"))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	defer promoted.Close()

	applied := f.Applied()
	if applied == 0 {
		t.Fatal("test premise broken: the follower applied nothing")
	}
	snap, err := promoted.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got, want := snap.Digest(), controlDigest(t, int(applied)); got != want {
		t.Errorf("promoted book is not the applied prefix (applied=%d):\n  promoted %s\n  control  %s", applied, got, want)
	}
}

// TestDrillD4_TheIncarnationFence — a client resuming against the promoted
// venue with the dead primary's cursor is refused. The positive control
// matters: a registry that refused everyone would also pass the negative half.
func TestDrillD4_TheIncarnationFence(t *testing.T) {
	promoted := orderentry.NewRegistry("INC-B", 64)
	if _, err := promoted.Resume("INC-A", "client-1", 7); err == nil {
		t.Error("a stale incarnation's cursor was accepted — the fence is fiction")
	}
	if _, err := promoted.Resume("INC-B", "client-1", 0); err != nil {
		t.Errorf("the new incarnation refused its own client: %v", err)
	}
}

// TestDrillD5_RefusalsReplayAsRefusals — the tape's poisoned commands (a cancel
// of a nonexistent order, a cancel by the wrong owner) are journalled
// write-ahead and shipped like everything else. The follower must refuse them
// identically: its applied sequence advances THROUGH them while its book stays
// equal to the control's.
func TestDrillD5_RefusalsReplayAsRefusals(t *testing.T) {
	p := newPrimary(t)
	defer p.Close()
	f, err := StartFollower("BTC-USD", p.Addr())
	if err != nil {
		t.Fatalf("StartFollower: %v", err)
	}
	// Steps 11, 13, 22, 26 … are refusals; run far enough to include several.
	const n = 30
	runTape(t, p.Runner, 1, n)
	if err := f.WaitApplied(int64(n), 10*time.Second); err != nil {
		t.Fatalf("a refused command stalled the feed: %v", err)
	}
	got, err := f.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if want := controlDigest(t, n); got != want {
		t.Errorf("a refusal replayed as something else:\n  follower %s\n  control  %s", got, want)
	}
}

// TestDrillD6_ASlowFollowerIsShedNotWaitedOn — a follower that stops reading
// must cost the matcher nothing: the primary cuts it, the shed counter moves,
// and a healthy follower on the same primary still converges.
func TestDrillD6_ASlowFollowerIsShedNotWaitedOn(t *testing.T) {
	p := newPrimary(t)
	defer p.Close()

	healthy, err := StartFollower("BTC-USD", p.Addr())
	if err != nil {
		t.Fatalf("StartFollower: %v", err)
	}

	// The wedge: subscribes like a follower, then never reads a byte. Its
	// socket fills, the primary's serve loop stalls on the write, its buffer
	// fills, and fanout must cut it rather than wait.
	wedged, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer wedged.Close()
	if err := json.NewEncoder(wedged).Encode(hello{Have: 0}); err != nil {
		t.Fatalf("hello: %v", err)
	}

	// Enough traffic to exhaust the socket buffers plus the ship buffer. Every
	// Process call returning is itself the "matching never blocked" assertion.
	n := 0
	deadline := time.Now().Add(20 * time.Second)
	for p.Shed() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the wedged follower was never shed — fanout is waiting on it")
		}
		n++
		tapeStep(t, p.Runner, n)
	}

	if err := healthy.WaitApplied(int64(n), 10*time.Second); err != nil {
		t.Fatalf("shedding the wedge broke the healthy follower: %v", err)
	}
	got, err := healthy.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if want := controlDigest(t, n); got != want {
		t.Errorf("healthy follower diverged while the wedge was shed:\n  follower %s\n  control  %s", got, want)
	}
	// The lag gauge: both sides of it must be readable, or the loss window is
	// a number nobody can see.
	if p.LogSeq() != int64(n) || healthy.Applied() != int64(n) {
		t.Errorf("lag gauge broken: LogSeq=%d Applied=%d want both %d", p.LogSeq(), healthy.Applied(), n)
	}
}

// TestDrillD3Refuses_APromotedBookWithAKnownDefect — belt for D3's braces: a
// follower that detected a gap must refuse promotion outright. Serving a book
// known to be wrong is strictly worse than serving nothing.
func TestDrillD3Refuses_APromotedBookWithAKnownDefect(t *testing.T) {
	f := &Follower{symbol: "BTC-USD", done: make(chan struct{})}
	f.engine = matching.NewEngine(matching.DefaultConfig("BTC-USD"))
	f.err = errors.New("gap in the feed: applied 7, received 9")
	f.conn = &net.TCPConn{} // Close on the zero value errors harmlessly
	close(f.done)
	if _, err := f.Promote(filepath.Join(t.TempDir(), "w.wal"), filepath.Join(t.TempDir(), "s.snap")); err == nil {
		t.Error("a book with a known defect was promoted")
	}
}
