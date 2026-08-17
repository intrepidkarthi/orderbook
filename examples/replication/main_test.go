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
	"github.com/intrepidkarthi/orderbook/pkg/wal"
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

	wedgeAddr := wedged.LocalAddr().String()

	// Enough traffic to exhaust the socket buffers plus the ship buffer. Every
	// Process call returning is itself the "matching never blocked" assertion.
	//
	// The tape is paced against the healthy follower, and that is not politeness
	// to the test — it is the difference between this drill asserting what it
	// claims and asserting something else about one run in twelve. A follower
	// that actually applies commands is SLOWER than a wedged socket, which merely
	// fills a kernel buffer and costs the primary nothing until it is full. Driven
	// flat out, the healthy follower's own ship buffer overflows first and IT is
	// the one shed; the drill then saw a non-zero Shed(), assumed it was the
	// wedge, and reported "shedding the wedge broke the healthy follower" — the
	// opposite of what had happened. Keeping the healthy follower within its
	// buffer leaves exactly one candidate for the shed, which is the point.
	n := 0
	deadline := time.Now().Add(20 * time.Second)
	for !shedIncludes(p, wedgeAddr) {
		if time.Now().After(deadline) {
			t.Fatalf("the wedged follower was never shed — fanout is waiting on it (shed=%d, peers=%v)",
				p.Shed(), p.ShedPeers())
		}
		n++
		tapeStep(t, p.Runner, n)
		if n%256 == 0 {
			if err := healthy.WaitApplied(int64(n), 10*time.Second); err != nil {
				t.Fatalf("the healthy follower fell behind before the wedge was shed: %v", err)
			}
		}
	}

	if err := healthy.WaitApplied(int64(n), 10*time.Second); err != nil {
		t.Fatalf("shedding the wedge broke the healthy follower: %v (shed peers %v)", err, p.ShedPeers())
	}
	// And the healthy follower is not among the casualties, which is the half of
	// "shed, not waited on" that a bare counter cannot express.
	for _, peer := range p.ShedPeers() {
		if peer != wedgeAddr {
			t.Errorf("a follower other than the wedge was shed: %s (wedge is %s)", peer, wedgeAddr)
		}
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

// shedIncludes reports whether addr is among the followers the primary has cut.
func shedIncludes(p *Primary, addr string) bool {
	for _, peer := range p.ShedPeers() {
		if peer == addr {
			return true
		}
	}
	return false
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

// bustVenue is a venue that can also annul a print. Both *matching.Runner and
// *matching.Engine satisfy it, which is what lets the drill below drive the
// replicated venue and the uninterrupted control through one function.
type bustVenue interface {
	venue
	Bust(int64, string) error
}

// TestDrillD7_ABustReplicates — the drill docs/TRADE-BUST.md deliverable #3 asks
// for: a primary annuls a print mid-stream and the follower ends up agreeing.
//
// It is a real test of the digest rather than of the wire. A bust changes no
// order, so a follower that dropped the record entirely would still have a
// byte-identical BOOK — and would disagree with the primary about which trades
// settled, forever, silently. The only thing that catches it is the bust registry
// being inside the digest, which is why it is.
func TestDrillD7_ABustReplicates(t *testing.T) {
	const before, after = 40, 40
	const bustTradeID = 3 // the tape has printed well past this by step 40

	drive := func(t *testing.T, v bustVenue) {
		t.Helper()
		runTape(t, v, 1, before)
		if err := v.Bust(bustTradeID, "erroneous order entry"); err != nil {
			t.Fatalf("Bust: %v", err)
		}
		runTape(t, v, before+1, before+after)
	}

	p := newPrimary(t)
	defer p.Close()
	f, err := StartFollower("BTC-USD", p.Addr())
	if err != nil {
		t.Fatalf("StartFollower: %v", err)
	}

	drive(t, p.Runner)

	// before + the bust + after: the bust is a journalled command like any other,
	// so it occupies a log sequence of its own.
	const total = before + 1 + after
	if err := f.WaitApplied(total, 10*time.Second); err != nil {
		t.Fatalf("follower never caught up: %v", err)
	}

	control := matching.NewEngine(matching.DefaultConfig("BTC-USD"))
	drive(t, control)
	want := control.TakeSnapshot().Digest()

	got, err := f.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if got != want {
		t.Errorf("follower diverged from the control across a bust:\n  follower %s\n  control  %s", got, want)
	}
	if snap, err := p.Runner.Checkpoint(); err != nil || snap.Digest() != want {
		t.Errorf("primary and control disagree (err=%v) — the bust is not deterministic", err)
	}

	// And the annulment is not merely counted: the right trade is the busted one.
	if !control.IsBusted(bustTradeID) {
		t.Fatal("the control never busted anything — the drill proves nothing")
	}
}

// TestDrillD8_AMultiSymbolVenueReplicates — docs/MULTI-SYMBOL.md deliverable #6.
//
// Two symbols, two shards, two logs, two followers, one venue. The drill is
// deliberately shaped by the decision in MULTI-SYMBOL §2: there is no order across
// symbols, so there is nothing to synchronise here and the assertion is per symbol.
// What it does prove is that the pieces compose — a shard's index reaches its
// follower, each follower digest-matches an uninterrupted engine fed the same
// commands, and the two symbols' ids stay disjoint across the wire.
//
// If a follower were started with the wrong shard index its book would hold the
// same orders under different numbers, and the digest would catch it. That is
// asserted below rather than assumed.
func TestDrillD8_AMultiSymbolVenueReplicates(t *testing.T) {
	man := matching.NewManifest(filepath.Join(t.TempDir(), "venue.json"))
	symbols := []string{"BTC-USD", "ETH-USD"}
	const n = 80

	type arm struct {
		cfg      matching.Config
		primary  *Primary
		follower *Follower
	}
	arms := map[string]*arm{}

	for _, sym := range symbols {
		idx, err := man.IndexFor(sym)
		if err != nil {
			t.Fatalf("IndexFor(%s): %v", sym, err)
		}
		cfg := matching.DefaultConfig(sym)
		cfg.ShardIndex = idx

		p, err := NewPrimaryFor(cfg, filepath.Join(t.TempDir(), sym+".wal"), "127.0.0.1:0")
		if err != nil {
			t.Fatalf("NewPrimaryFor(%s): %v", sym, err)
		}
		defer p.Close()
		f, err := StartFollowerFor(cfg, p.Addr())
		if err != nil {
			t.Fatalf("StartFollowerFor(%s): %v", sym, err)
		}
		arms[sym] = &arm{cfg: cfg, primary: p, follower: f}
	}

	// Interleave the two symbols, which is what a real venue does and what a
	// centralised id counter would have made non-replayable.
	for i := 1; i <= n; i++ {
		for _, sym := range symbols {
			tapeStep(t, arms[sym].primary.Runner, i)
		}
	}

	ids := map[int64]string{}
	for _, sym := range symbols {
		a := arms[sym]
		if err := a.follower.WaitApplied(int64(n), 10*time.Second); err != nil {
			t.Fatalf("%s follower never caught up: %v", sym, err)
		}
		got, err := a.follower.Digest()
		if err != nil {
			t.Fatalf("%s Digest: %v", sym, err)
		}

		control := matching.NewEngine(a.cfg)
		runTape(t, control, 1, n)
		if want := control.TakeSnapshot().Digest(); got != want {
			t.Errorf("%s follower diverged from its control:\n  follower %s\n  control  %s", sym, got, want)
		}

		// Every id this symbol issued belongs to its shard, and to no other.
		for _, o := range control.TakeSnapshot().Orders {
			if shard, _ := matching.SplitID(o.ID); shard != a.cfg.ShardIndex {
				t.Errorf("%s order %d carries shard %d, want %d", sym, o.ID, shard, a.cfg.ShardIndex)
			}
			if prev, dup := ids[o.ID]; dup {
				t.Errorf("id %d issued by both %s and %s", o.ID, prev, sym)
			}
			ids[o.ID] = sym
		}
	}
	if len(ids) == 0 {
		t.Fatal("the tape left no resting orders — the drill proves nothing")
	}
}

// TestDrillD8Refuses_AFollowerOnTheWrongShard — the belt for D8's braces. A
// follower carrying the wrong shard index rebuilds the same orders under different
// numbers, and the digest must notice. Without partitioned ids there would be
// nothing to notice.
func TestDrillD8Refuses_AFollowerOnTheWrongShard(t *testing.T) {
	man := matching.NewManifest(filepath.Join(t.TempDir(), "venue.json"))
	idx, err := man.IndexFor("BTC-USD")
	if err != nil {
		t.Fatal(err)
	}
	cfg := matching.DefaultConfig("BTC-USD")
	cfg.ShardIndex = idx

	p, err := NewPrimaryFor(cfg, filepath.Join(t.TempDir(), "p.wal"), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	wrong := cfg
	wrong.ShardIndex = idx + 1
	f, err := StartFollowerFor(wrong, p.Addr())
	if err != nil {
		t.Fatal(err)
	}

	const n = 40
	runTape(t, p.Runner, 1, n)
	if err := f.WaitApplied(int64(n), 10*time.Second); err != nil {
		t.Fatalf("follower never caught up: %v", err)
	}
	got, err := f.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	snap, err := p.Runner.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got == snap.Digest() {
		t.Error("a follower on the wrong shard index matched the primary — ids are not partitioned after all")
	}
}

// TestDrillD9_AFollowerReconnectsAcrossARotation — the primary's log is now a set
// of segments and older ones get deleted, so "catch a reconnecting follower up from
// the file" has two new ways to go wrong and both are silent.
//
// A catch-up that read only the stem would ship nothing at all once the log had
// rotated. A catch-up that shipped whatever segments happen to remain would start
// above the follower's position, and the follower's gap check would kill it with
// "gap in the feed" — a protocol error raised against the one source that is
// supposed to be authoritative, every time the primary rotated.
func TestDrillD9_AFollowerReconnectsAcrossARotation(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "primary.wal")
	p, err := NewPrimaryWith(matching.DefaultConfig("BTC-USD"), walPath, "127.0.0.1:0",
		wal.Options{MaxSegmentBytes: 8 << 10})
	if err != nil {
		t.Fatalf("NewPrimaryWith: %v", err)
	}
	defer p.Close()

	const n = 400
	runTape(t, p.Runner, 1, n)
	if info, err := wal.Stat(walPath); err != nil {
		t.Fatalf("Stat: %v", err)
	} else if info.Segments < 3 {
		t.Fatalf("the primary's log is %d segments; this drill is not exercising rotation", info.Segments)
	}

	// A follower that has applied 100 reconnects. It must be caught up with records,
	// starting at exactly 101, across every segment boundary in between.
	const have = 100
	first, records, gotSnapshot := reconnect(t, p, have)
	if gotSnapshot {
		t.Fatalf("a follower inside the retained log was bootstrapped with a snapshot rather than caught up")
	}
	if first != have+1 {
		t.Fatalf("catch-up started at sequence %d, want %d — the follower would report a gap in the feed", first, have+1)
	}
	if records != n-have {
		t.Errorf("catch-up shipped %d records, want %d", records, n-have)
	}
}

// TestDrillD9_AFollowerBelowTheFloorIsBootstrapped — retention has deleted the
// records this follower is asking for. The primary must answer with a snapshot,
// which is the same answer market data already gives an evicted subscriber, and not
// with the records it happens to still have.
func TestDrillD9_AFollowerBelowTheFloorIsBootstrapped(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "primary.wal")
	snapPath := filepath.Join(dir, "primary.snap")
	opts := wal.Options{MaxSegmentBytes: 8 << 10, RetainBytes: 1, MinSegments: -1}
	p, err := NewPrimaryWith(matching.DefaultConfig("BTC-USD"), walPath, "127.0.0.1:0", opts)
	if err != nil {
		t.Fatalf("NewPrimaryWith: %v", err)
	}
	defer p.Close()

	const n = 400
	runTape(t, p.Runner, 1, n)
	// Checkpoint and retain, exactly as a venue's checkpoint loop does.
	snap, err := p.Runner.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := wal.WriteSnapshot(snapPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	res, err := wal.Retain(walPath, snapPath, opts)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if len(res.Deleted) == 0 {
		t.Fatalf("retention deleted nothing, so no follower can be below the floor")
	}
	if res.Floor <= 2 {
		t.Fatalf("the floor is at %d; nothing is below it", res.Floor)
	}

	// A follower at sequence 1 needs record 2, which is gone.
	_, _, gotSnapshot := reconnect(t, p, 1)
	if !gotSnapshot {
		t.Fatalf("a follower below the retention floor (%d) was shipped records instead of a snapshot.\n"+
			"Those records start above the sequence it asked for, so it would terminate with \"gap in the feed\" —\n"+
			"a protocol error raised against the primary because the primary deleted the answer.", res.Floor)
	}
}

// reconnect speaks the follower's opening handshake by hand and reports what came
// back: the sequence of the first record, how many records arrived, and whether the
// primary answered with a snapshot instead.
func reconnect(t *testing.T, p *Primary, have int64) (first int64, records int, gotSnapshot bool) {
	t.Helper()
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(hello{Have: have}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	dec := json.NewDecoder(conn)
	for {
		var fr frame
		if err := dec.Decode(&fr); err != nil {
			return first, records, gotSnapshot
		}
		if fr.Snapshot != nil {
			gotSnapshot = true
			return first, records, gotSnapshot
		}
		var e wal.Entry
		if err := json.Unmarshal(fr.Record, &e); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if records == 0 {
			first = e.Seq
		}
		records++
		// The catch-up is finished when it has reached the live tail; there is no
		// framing that says so, so stop at the primary's current sequence.
		if e.Seq >= p.LogSeq() {
			return first, records, gotSnapshot
		}
	}
}
