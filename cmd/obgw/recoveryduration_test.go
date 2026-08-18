package main

import (
	"math"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// Recovery duration — docs/LAG-AND-SHED.md §8.
//
// Bounded recovery and log rotation were built to make a restart cheap and were
// measured in BenchmarkRestartWithRetention. A venue that has just restarted knows
// exactly how long its own recovery took and threw the number away.
//
// The threshold on it is the one signal in that document with no measured normal,
// because "normal" is whatever this deployment's book size and retained log make it.
// What it has instead is a policy number and a TREND — 2× the previous restart — and
// the trend is the real alert: recovery cost is invisible until the worst moment,
// because a venue that never restarts never notices.

// buildRecoveryFixture writes a log of n records whose replay leaves resting orders
// behind, so the recovery this measures has both terms a real one has: O(log) to read
// and replay, and O(book) for the two adoptions that follow.
func buildRecoveryFixture(t *testing.T, path string, resting, extra int) {
	t.Helper()
	w, err := wal.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < resting; i++ {
		o, err := types.NewOrder("alice", "X", types.SideBuy, types.OrderTypeLimit,
			int64(1+i%4000), 10, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		o.ClientOrderID = "r" + itoa(i)
		if _, err := w.AppendSubmit(o); err != nil {
			t.Fatalf("AppendSubmit: %v", err)
		}
	}
	// Records that replay cheaply and rest nothing, so the log is twice the book.
	for i := 0; i < extra; i++ {
		if _, err := w.AppendSetMark(int64(100 + i%50)); err != nil {
			t.Fatalf("AppendSetMark: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// recoverAloneNanos is how long wal.Recover takes on this fixture with no session
// index and no feed to seed — the narrow interval, which is what timing only
// wal.Recover would report.
func recoverAloneNanos(t *testing.T, path string) int64 {
	t.Helper()
	eng := matching.DefaultConfig("X")
	eng.DedupClientOrderIDs = 4096
	start := time.Now()
	if _, err := wal.Recover(eng, "", path); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return time.Since(start).Nanoseconds()
}

func median(xs []int64) int64 {
	sort.Slice(xs, func(a, b int) bool { return xs[a] < xs[b] })
	return xs[len(xs)/2]
}

// TestRecoveryDurationIsReported is deliverable 19, and the third assertion is the
// one that matters: the gauge must exceed what wal.Recover alone costs on the same
// fixture.
//
// The operator's question is "how long was my venue down", not "how long did the WAL
// package take". reg.Adopt and feed.Adopt are both O(book) — at a 5,000-order book
// they are around a fifth of the recovery — so measuring the narrow interval would
// produce a number reliably SMALLER than the truth, which is the worst kind of wrong
// for a figure that feeds a recovery time objective.
//
// Both sides are measured five times and compared as medians, INTERLEAVED so a drift
// in the machine's state moves both together. The margin being asserted is real work
// and not noise, but it is a fifth of a 20 ms measurement, and a single sample of
// either side is not a number to build an assertion on.
func TestRecoveryDurationIsReported(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "obgw.wal")
	// No snapshot, so the whole log is replayed and the recovery is the real thing
	// rather than a restore plus a short tail.
	buildRecoveryFixture(t, walPath, 5000, 5000)

	cfg := Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 64, StreamRing: 8192, RatePerSec: 1e6, Burst: 1e6,
		WALPath: walPath,
	}

	const rounds = 5
	bares := make([]int64, 0, rounds)
	reported := make([]int64, 0, rounds)
	var wall int64
	for i := 0; i < rounds; i++ {
		bares = append(bares, recoverAloneNanos(t, walPath))

		start := time.Now()
		srv := mustServer(t, cfg)
		elapsed := time.Since(start).Nanoseconds()

		if n := srv.books.first().runner.OrderCount(); n != 5000 {
			t.Fatalf("recovered %d resting orders, want 5000 — the fixture is not what this claims to measure", n)
		}
		got, ok := gaugeFor(t, srv, recoveryDurationMetric, "X")
		if !ok {
			t.Fatal("a venue that recovered from a log reports no recovery duration")
		}
		if got <= 0 {
			t.Fatalf("recovery duration = %v, want a positive number of nanoseconds", got)
		}
		if int64(got) > elapsed {
			t.Errorf("recovery duration %.0f ns exceeds the %d ns the test measured around NewServer", got, elapsed)
		}
		reported = append(reported, int64(got))
		wall = elapsed
		srv.Close()
	}

	bare, mine := median(bares), median(reported)
	if mine <= bare {
		t.Errorf("recovery duration %d ns is not above the %d ns wal.Recover alone costs on the same fixture — "+
			"the two Adopt calls are outside the measured interval, so the venue is reporting a downtime "+
			"reliably shorter than the one it had", mine, bare)
	}
	t.Logf("recovery %d ns; wal.Recover alone %d ns (ratio %.2f); last wall clock around NewServer %d ns",
		mine, bare, float64(mine)/float64(bare), wall)
}

// TestRecoveryDurationReportsWhatItCannotKnow is deliverable 20.
func TestRecoveryDurationReportsWhatItCannotKnow(t *testing.T) {
	t.Run("no log reads NaN", func(t *testing.T) {
		// Zero would read as "recovered instantly", which is the one answer that is
		// never true: a venue with no log did not recover at all.
		srv := testServer(t)
		got, ok := gaugeFor(t, srv, recoveryDurationMetric, "X")
		if !ok {
			t.Fatal("the series is absent for a venue with no log")
		}
		if !math.IsNaN(got) {
			t.Errorf("recovery duration = %v with no log configured, want NaN", got)
		}
	})

	t.Run("two books report two series whose sum is the venue total", func(t *testing.T) {
		dir := t.TempDir()
		buildRecoveryFixture(t, filepath.Join(dir, "AAA.wal"), 400, 400)
		buildRecoveryFixture(t, filepath.Join(dir, "BBB.wal"), 400, 400)

		start := time.Now()
		srv := mustServer(t, Config{
			Addr: "127.0.0.1:0", Symbols: []string{"AAA", "BBB"}, DataDir: dir, Incarnation: "INC1",
			Accounts:      map[string]string{"alice": "pw1"},
			OutboundDepth: 64, StreamRing: 8192, RatePerSec: 1e6, Burst: 1e6,
		})
		wall := time.Since(start).Nanoseconds()
		defer srv.Close()

		a, okA := gaugeFor(t, srv, recoveryDurationMetric, "AAA")
		b, okB := gaugeFor(t, srv, recoveryDurationMetric, "BBB")
		if !okA || !okB {
			t.Fatalf("per-symbol series missing: AAA=%v BBB=%v", okA, okB)
		}
		if a <= 0 || b <= 0 {
			t.Fatalf("AAA=%v BBB=%v; both books recovered from a real log", a, b)
		}
		// Books recover SERIALLY, in the configuration's symbol order, which is what
		// makes sum() the venue's downtime rather than an approximation of it — and
		// is why this is one labelled family instead of a per-book metric plus a
		// separate venue total. Two names for one number is how they drift.
		if int64(a+b) > wall {
			t.Errorf("sum of per-book recoveries %.0f ns exceeds the %d ns the whole of NewServer took; "+
				"they cannot have overlapped", a+b, wall)
		}
		t.Logf("AAA %.0f ns + BBB %.0f ns = %.0f ns of a %d ns startup", a, b, a+b, wall)
	})
}
