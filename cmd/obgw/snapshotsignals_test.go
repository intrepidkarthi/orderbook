package main

import (
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// Snapshot age, duration and failure — docs/LAG-AND-SHED.md §6 and §7.
//
// Before this a failed checkpoint wrote `obgw: %s checkpoint: %v` and continued. No
// gauge moved, no counter incremented, /readyz was unaffected. M14 listed snapshot
// failure as one of three undefined degraded behaviours, and it was undefined in the
// specific sense that NOTHING OBSERVABLE CHANGED.
//
// It is defined here, and the definition is deliberately not the obvious symmetry
// with the WAL-failure path: the venue keeps trading, keeps reporting ready, and says
// so in the readiness body. §7 argues that; TestDrillCheckpointFailureKeepsTrading is
// what stops somebody "fixing" it back.

// gaugeFor reads one labelled series out of the exposition. Through the page rather
// than through the closure, because a gauge that is computed correctly and never
// rendered is the other way this fails.
func gaugeFor(t *testing.T, srv *Server, metric, symbol string) (float64, bool) {
	t.Helper()
	var buf strings.Builder
	if err := srv.metrics.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	prefix := metric + `{symbol="` + symbol + `"} `
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimPrefix(line, prefix), 64)
		if err != nil {
			t.Fatalf("unparseable reading %q: %v", line, err)
		}
		return v, true
	}
	return 0, false
}

// snapFailureCount reads one book's failure counter off the handle it holds.
func snapFailureCount(t *testing.T, srv *Server, symbol string) int64 {
	t.Helper()
	b := srv.books.bySymbol(symbol)
	if b == nil {
		t.Fatalf("no book named %q", symbol)
	}
	if b.snapFailures == nil {
		t.Fatalf("%s has no failure counter; it was not configured to checkpoint", symbol)
	}
	return b.snapFailures.Value()
}

// checkpointingVenue puts the log and the snapshot in SEPARATE directories, so a
// test can break the snapshot's without breaking the log's — which would halt the
// venue and prove something else entirely.
func checkpointingVenue(t *testing.T, every time.Duration) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snap")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cfg := Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1", "bob": "pw2"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
		AdminAddr:       "127.0.0.1:0",
		WALPath:         filepath.Join(dir, "obgw.wal"),
		SnapshotPath:    filepath.Join(snapDir, "obgw.snap"),
		CheckpointEvery: every,
	}
	return cfg, snapDir
}

// TestSnapshotAgeIsNaNWithoutASnapshotPath is deliverable 16, in all three of its
// readings — including the two an operator would otherwise report as bugs.
func TestSnapshotAgeIsNaNWithoutASnapshotPath(t *testing.T) {
	t.Run("not asked to checkpoint reads NaN", func(t *testing.T) {
		// NaN rather than zero for the reason the best-bid gauge is NaN: zero is a
		// legal age, and a monitoring system cannot tell a missing snapshot from one
		// written this instant. NaN also never satisfies a > comparison, so a venue
		// with checkpointing deliberately off does not page.
		srv := testServer(t)
		got, ok := gaugeFor(t, srv, snapshotAgeMetric, "X")
		if !ok {
			t.Fatal("the series is absent; a dashboard cannot show a venue that does not checkpoint")
		}
		if !math.IsNaN(got) {
			t.Errorf("age = %v, want NaN — zero would read as a snapshot written this instant", got)
		}
	})

	t.Run("asked and none landed yet counts from process start", func(t *testing.T) {
		// The other half of the rule: a venue that WAS asked to checkpoint and never
		// has must cross the same threshold at the same time a stalled loop would.
		cfg, _ := checkpointingVenue(t, 0) // configured, but the loop never runs
		srv := durableServer(t, cfg)
		defer srv.Close()

		first, ok := gaugeFor(t, srv, snapshotAgeMetric, "X")
		if !ok {
			t.Fatal("no series for a configured-but-absent snapshot")
		}
		if math.IsNaN(first) || first < 0 || first > 5 {
			t.Fatalf("age = %v just after start, want a small non-negative number", first)
		}
		time.Sleep(150 * time.Millisecond)
		second, _ := gaugeFor(t, srv, snapshotAgeMetric, "X")
		if second <= first {
			t.Errorf("age went %v -> %v; a venue asked to checkpoint and never doing so must age", first, second)
		}
	})

	t.Run("a backwards clock reads negative and is not clamped", func(t *testing.T) {
		// Clamping to zero would report the freshest possible snapshot at exactly the
		// moment the host's clock is wrong. M14 records that this venue has no
		// clock-offset signal of any kind, and a negative age here is the only one it
		// will have — not a substitute for one, and not claimed as one.
		cfg, _ := checkpointingVenue(t, 0)
		srv := durableServer(t, cfg)
		defer srv.Close()

		if err := os.WriteFile(cfg.SnapshotPath, []byte("not a real snapshot"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(cfg.SnapshotPath, future, future); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		got, _ := gaugeFor(t, srv, snapshotAgeMetric, "X")
		if got > -3000 {
			t.Errorf("age = %v for a file stamped an hour in the future, want about -3600 — "+
				"a clamp here hides a clock problem behind the freshest possible reading", got)
		}
	})
}

// TestSnapshotAgeSurvivesARestart is deliverable 15, and it is the assertion a
// process-local "last successful checkpoint" timestamp cannot pass.
//
// A venue that has just recovered reports the true age of the base it recovered from,
// immediately, before its first checkpoint tick. A timer seeded at process start
// reports zero — the freshest possible reading — for a venue that may be recovering
// from a base two days old, which is the reading an operator would most like to have
// and the one a timer cannot give.
func TestSnapshotAgeSurvivesARestart(t *testing.T) {
	cfg, _ := checkpointingVenue(t, 60*time.Millisecond)
	srv := durableServer(t, cfg)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.SnapshotPath); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(cfg.SnapshotPath); err != nil {
		t.Fatalf("test premise broken: no checkpoint landed: %v", err)
	}
	srv.Close()

	// An hour-old recovery base. Nothing else about the venue changes.
	hourAgo := time.Now().Add(-time.Hour)
	if err := os.Chtimes(cfg.SnapshotPath, hourAgo, hourAgo); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Checkpointing off in the revived venue, so the reading below cannot be a tick
	// that happened to land between the restart and the scrape.
	cfg.Addr = "127.0.0.1:0"
	cfg.AdminAddr = "127.0.0.1:0"
	cfg.CheckpointEvery = 0
	revived := durableServer(t, cfg)
	defer revived.Close()

	got, ok := gaugeFor(t, revived, snapshotAgeMetric, "X")
	if !ok {
		t.Fatal("no snapshot age after a restart")
	}
	if got < 3000 || got > 4200 {
		t.Errorf("age = %v immediately after recovering from an hour-old base, want about 3600 — "+
			"a process-local timer reports 0 here, which is the freshest possible reading for the stalest venue", got)
	}
}

// TestSnapshotAgeClimbsWhenCheckpointsFail is deliverable 14: a REAL failed write,
// not a simulated one, and the book still trading through it.
func TestSnapshotAgeClimbsWhenCheckpointsFail(t *testing.T) {
	const every = 100 * time.Millisecond
	cfg, snapDir := checkpointingVenue(t, every)
	srv := durableServer(t, cfg)
	defer srv.Close()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}
	// A healthy checkpoint first, so what follows is a transition rather than a venue
	// that never checkpointed.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.SnapshotPath); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(cfg.SnapshotPath); err != nil {
		t.Fatalf("test premise broken: no checkpoint landed: %v", err)
	}

	// The snapshot's directory goes read-only. Not literally ENOSPC, and deliberately
	// so — it is the one storage failure a unit test can induce portably without
	// root, and it enters WriteSnapshot through the same door. The LOG's directory is
	// untouched, so this is a checkpoint failure and not a durability failure.
	if err := os.Chmod(snapDir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(snapDir, 0o755) })

	limit := staleSnapshotFactor * every.Seconds()
	deadline = time.Now().Add(10 * time.Second)
	var age float64
	for time.Now().Before(deadline) {
		age, _ = gaugeFor(t, srv, snapshotAgeMetric, "X")
		if age > limit && snapFailureCount(t, srv, "X") > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if age <= limit {
		t.Errorf("snapshot age = %v after the checkpoint stopped landing, want above the %v alert threshold", age, limit)
	}
	if n := snapFailureCount(t, srv, "X"); n == 0 {
		t.Error("no snapshot failures counted; the age says the base is stale and nothing says why")
	}
	// Age and failures answer different questions and the venue needs both: age says
	// the recovery base is stale, failures say why. A stale age with a FLAT failure
	// counter is a checkpoint loop that is not running at all — a different fault
	// with a different fix.
	var buf strings.Builder
	if err := srv.metrics.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if !strings.Contains(buf.String(), snapshotFailuresMetric+`{symbol="X"}`) {
		t.Errorf("the runbook's signal is not on the page:\n%s", buf.String())
	}

	// And the book is still trading, which is the decision §7 makes. A second
	// account, because the venue prevents self-trades and a refusal for that reason
	// would look like the failure this is checking for.
	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("b1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := bob.awaitType(t, wire.MsgExecuted, 3*time.Second); !ok {
		t.Error("the venue stopped matching because it could not checkpoint; every acknowledged command is " +
			"still durable and the previous snapshot is still valid")
	}
}

// TestDrillCheckpointFailureKeepsTrading is deliverable 17 and RUNBOOKS' new
// "Checkpoints have stopped landing" section.
//
// The obvious move is to copy the WAL-failure path, which fails readiness and
// latches. This slice deliberately does not, and the drill is what stops somebody who
// has read only the WAL-failure path from "fixing" it: readiness at a venue does not
// move traffic elsewhere, it stops this book receiving orders while it holds every
// position already in it — and it invites the orchestrator to restart the node, which
// is exactly the restart the failed checkpoint has been making more expensive.
func TestDrillCheckpointFailureKeepsTrading(t *testing.T) {
	const every = 100 * time.Millisecond
	cfg, snapDir := checkpointingVenue(t, every)
	srv := durableServer(t, cfg)
	defer srv.Close()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}
	if code, body := adminGet(t, srv, "/readyz"); code != http.StatusOK || strings.Contains(body, "degraded") {
		t.Fatalf("/readyz = %d %q before the failure", code, body)
	}

	if err := os.Chmod(snapDir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(snapDir, 0o755) })

	deadline := time.Now().Add(10 * time.Second)
	var code int
	var body string
	for time.Now().Before(deadline) {
		code, body = adminGet(t, srv, "/readyz")
		if strings.Contains(body, "degraded") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code != http.StatusOK {
		t.Errorf("/readyz = %d %q — a venue that cannot checkpoint must stay READY. Taking the book out of "+
			"rotation stops clients entering while it still holds their positions, and pushes the venue toward "+
			"the very restart the failed checkpoint has been making expensive", code, body)
	}
	if !strings.Contains(body, "degraded") {
		t.Fatalf("/readyz body = %q, want a degraded clause — a 200 that says nothing is how this stayed invisible", body)
	}
	// The clause has to name the symbol and the age, or a human checking by hand
	// learns only that something is wrong.
	if !strings.Contains(body, "X checkpoint") || !strings.Contains(body, "failures") {
		t.Errorf("/readyz body = %q, want the symbol and the failure count", body)
	}
	if srv.walFailed.Load() {
		t.Error("walFailed latched on a snapshot failure: a WAL failure means acknowledged commands are not " +
			"durable NOW; a snapshot failure means recovery will be slow LATER, and only one of those stops trading")
	}

	// Still matching, through the whole thing. A second account, since the venue
	// prevents self-trades.
	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	bob.enter("b1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := bob.awaitType(t, wire.MsgExecuted, 3*time.Second); !ok {
		t.Error("no execution while degraded; the venue stopped trading over a recovery-time problem")
	}

	// And the terminal case is the one that is already defined and already drilled:
	// with the snapshot frozen, retention stops deleting, the log grows, and the disk
	// path takes the venue cancel-only. Nothing here needs its own escalation.
	if srv.walStopped.Load() {
		t.Error("the venue went cancel-only over a checkpoint failure; that trades a recovery-time problem for a " +
			"certain trading outage, on a timer, unattended")
	}
}

// TestSnapshotDurationIsObserved is deliverable 18. The exact leg is the second one:
// at a two-book venue where one book's checkpoint always fails, every tick produces
// exactly one success and one failure, so the histogram's count must EQUAL the other
// book's failure count. If a failed write also fed the histogram, it would be double.
func TestSnapshotDurationIsObserved(t *testing.T) {
	t.Run("a healthy venue records what the write cost", func(t *testing.T) {
		cfg, _ := checkpointingVenue(t, 60*time.Millisecond)
		srv := durableServer(t, cfg)
		defer srv.Close()

		c := dial(t, srv)
		c.mustLogin("alice", "pw1")
		for i := 0; i < 20; i++ {
			c.enter("d"+itoa(i), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100+int64(i), 10)
			if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
				t.Fatalf("d%d not accepted", i)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for srv.snapHist.Count() < 3 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if srv.snapHist.Count() < 3 {
			t.Fatalf("%s_count = %d after several ticks", snapshotDurationMetric, srv.snapHist.Count())
		}
		if n := snapFailureCount(t, srv, "X"); n != 0 {
			t.Fatalf("%d failures on a healthy venue", n)
		}

		// The mean, not a quantile: the shared buckets top out at 250 ms, so at a
		// large book the quantile reads exactly 250000000 and means "at least". _sum
		// over _count is exact at any magnitude, which is why the alert threshold is
		// written against it.
		mean := float64(srv.snapHist.Sum()) / float64(srv.snapHist.Count())
		snap, err := srv.books.first().runner.Checkpoint()
		if err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
		start := time.Now()
		if err := wal.WriteSnapshot(filepath.Join(t.TempDir(), "measure.snap"), snap); err != nil {
			t.Fatalf("WriteSnapshot: %v", err)
		}
		measured := float64(time.Since(start).Nanoseconds())

		// Two statistics, and which bound gets which is the point.
		//
		// TOO SMALL is the defect this subtest names — a timer around the wrong thing,
		// or around nothing, reads orders of magnitude under a real write. The mean is
		// right for that: it is exact at any magnitude and an outlier can only push it
		// UP, so a mean that is still far too small is real.
		//
		// TOO LARGE is the direction contention also produces. The mean is the wrong
		// statistic there, and this subtest failed in CI on exactly that: one
		// descheduled write took the mean to 133 ms against a 1.3 ms write, 100x apart,
		// on a venue with nothing wrong with it. The median moves only if the TYPICAL
		// write is slow, which is the thing worth failing on. It is a bucket upper
		// bound rather than an exact value, which an order-of-magnitude band can carry;
		// on a 20-order book the buckets are nowhere near saturating.
		med := float64(srv.snapHist.Quantile(0.5))
		if mean*20 < measured {
			t.Errorf("mean recorded duration %.0f ns against %.0f ns measured by the test; more than an order of "+
				"magnitude too small means this is not timing the write", mean, measured)
		}
		if med > measured*20 {
			t.Errorf("median recorded duration %.0f ns against %.0f ns measured by the test; the typical write "+
				"being that much slower means this is timing more than the write", med, measured)
		}
		t.Logf("snapshot duration median %.0f ns, mean %.0f ns over %d writes; the test's own write took %.0f ns",
			med, mean, srv.snapHist.Count(), measured)
	})

	t.Run("a failed write is counted and never timed", func(t *testing.T) {
		dir := t.TempDir()
		srv := mustServer(t, Config{
			Addr: "127.0.0.1:0", Symbols: []string{"AAA", "BBB"}, DataDir: dir, Incarnation: "INC1",
			Accounts:      map[string]string{"alice": "pw1"},
			OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
			CheckpointEvery: 50 * time.Millisecond,
		})
		// BBB's snapshot path is a DIRECTORY, so its rename can never succeed while
		// AAA's is untouched. One tick, one success, one failure.
		if err := os.MkdirAll(filepath.Join(dir, "BBB.snap"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := srv.Listen(); err != nil {
			t.Fatalf("Listen: %v", err)
		}
		go func() { _ = srv.Serve() }()
		defer srv.Close()

		deadline := time.Now().Add(5 * time.Second)
		for snapFailureCount(t, srv, "BBB") < 4 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		// Stop the loop before comparing, so the two numbers are read from a venue
		// that is no longer moving.
		srv.Close()

		failures := snapFailureCount(t, srv, "BBB")
		if failures < 4 {
			t.Fatalf("only %d failures on a book whose snapshot path is a directory", failures)
		}
		if got := srv.snapHist.Count(); got != failures {
			t.Errorf("%s_count = %d against %d failures on the other book; every tick writes exactly one of "+
				"each, so a failed write is being timed as if it had succeeded", snapshotDurationMetric, got, failures)
		}
		if n := snapFailureCount(t, srv, "AAA"); n != 0 {
			t.Errorf("the healthy book reported %d failures", n)
		}
	})
}

// TestReadinessReadsNoFilesystem proves the readiness probe answers from the cached
// snapshot mtime rather than by statting the snapshot.
//
// /readyz is the check that takes a node out of rotation. Everything else it reads is
// an atomic or a lock this process holds, deliberately: a probe that blocks blocks
// exactly when the venue is in trouble, and an orchestrator whose probe times out
// kills a book that is holding positions. Statting the snapshot put a mount on that
// path — an NFS server that stops answering, or a device that hangs, would have turned
// a snapshot-storage problem into a trading outage and then into the restart that a
// stale snapshot has been making expensive. That is the outcome §7 exists to avoid,
// arrived at through the probe instead of through the status code.
//
// The test cannot hang a filesystem, so it does the next thing that distinguishes the
// two implementations: it makes the file UNSTATABLE and asserts the answer is still
// the file's real age. A readiness path that stats would get ENOENT here and fall back
// to "seconds since process start" — a small number, no degraded clause, and a venue
// reporting healthy at the moment its recovery base has vanished.
func TestReadinessReadsNoFilesystem(t *testing.T) {
	const every = 100 * time.Millisecond
	cfg, snapDir := checkpointingVenue(t, every)
	srv := durableServer(t, cfg)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.SnapshotPath); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(cfg.SnapshotPath); err != nil {
		t.Fatalf("test premise broken: no checkpoint landed: %v", err)
	}
	srv.Close()

	// An hour-old base, then the whole directory goes away. A restart onto it must be
	// degraded from the first probe, without a scrape having happened first and
	// without anything for a stat to find.
	hourAgo := time.Now().Add(-time.Hour)
	if err := os.Chtimes(cfg.SnapshotPath, hourAgo, hourAgo); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	cfg.Addr = "127.0.0.1:0"
	cfg.AdminAddr = "127.0.0.1:0"
	revived := durableServer(t, cfg)
	defer revived.Close()

	if err := os.RemoveAll(snapDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	ready, body := revived.readiness()
	if !ready {
		t.Errorf("/readyz = not ready (%q); a snapshot problem must never fail readiness — see §7", body)
	}
	if !strings.Contains(body, "degraded") {
		t.Errorf("/readyz body = %q, want a degraded clause: the recovery base is an hour old and now absent, "+
			"and a probe that statted would have reported the process's own age instead", body)
	}
	if !strings.Contains(body, "3") { // "X checkpoint 36NNs old"
		t.Errorf("/readyz body = %q, want an age near 3600s read from the cached mtime", body)
	}
}

// TestSnapshotWithoutAWALIsNotPermanentlyDegraded — checkpointLoop is gated on
// s.durable(), so a venue started with -snapshot and no -wal never checkpoints at all.
// Reporting it degraded forever, and crossing the snapshot-age threshold forever, on a
// venue where nothing is wrong and nothing could be, is how a degraded clause and an
// alert both stop being read.
func TestSnapshotWithoutAWALIsNotPermanentlyDegraded(t *testing.T) {
	dir := t.TempDir()
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
		SnapshotPath:    filepath.Join(dir, "obgw.snap"),
		CheckpointEvery: 10 * time.Millisecond,
	})
	defer srv.Close()

	// Well past 3x the interval, with no log and therefore no checkpoint loop.
	srv.startedAt = time.Now().Add(-time.Hour)
	if got := srv.checkpointDegradation(); got != "" {
		t.Errorf("checkpointDegradation() = %q on a venue with no log to checkpoint against; "+
			"the degraded clause and the loop have to agree about whether checkpointing happens at all", got)
	}
	if ready, body := srv.readiness(); !ready || strings.Contains(body, "degraded") {
		t.Errorf("/readyz = %v %q, want ready with no degraded clause", ready, body)
	}
}
