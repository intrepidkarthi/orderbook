package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// What the venue does when it can no longer write its log.
//
// Before this, nothing. ENOSPC surfaces at the Flush inside Sync, which runs on the
// 20 ms group-commit ticker, whose entire error handling was to log and continue. So
// a full disk produced a venue that kept accepting orders, kept acknowledging them,
// kept matching them and stopped journalling — fifty log lines a second, /readyz
// still green, and the write-ahead property, which is the reason the package exists,
// silently gone. Every acknowledgement after the first failed sync was a lie, and
// the venue was the only party that could have known.

// TestDiskFullHaltsRatherThanAcknowledging induces a real filesystem failure rather
// than simulating its symptoms: the log's directory is made unwritable while the
// venue trades, so the next rotation cannot create a segment.
//
// A read-only directory is not literally ENOSPC, and that is deliberate — it is the
// one storage failure a unit test can induce portably without root, and it enters
// the writer through exactly the same door: an error from the filesystem on the path
// that makes a record durable. What is asserted is what happens next, which is the
// part that was wrong.
func TestDiskFullHaltsRatherThanAcknowledging(t *testing.T) {
	dir := t.TempDir()
	cfg := durableConfig(t)
	cfg.WALPath = filepath.Join(dir, "obgw.wal")
	cfg.SnapshotPath = filepath.Join(dir, "obgw.snap")
	cfg.AdminAddr = "127.0.0.1:0"
	// Small segments so a rotation is attempted within a handful of orders.
	cfg.WALSegmentBytes = 4 << 10
	srv := durableServer(t, cfg)
	defer srv.Close()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	// Trade normally first, so the failure below is a transition rather than a venue
	// that never worked.
	c.enter("ok1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("the venue was not healthy before the disk failure")
	}
	if code, body := adminGet(t, srv, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d %q before the failure", code, body)
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// Push until the log has to rotate, which it now cannot do.
	var refusedAt int
	for i := 0; i < 400; i++ {
		id := "x" + itoa(i)
		c.enter(id, wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100+int64(i%7), 10)
		b, ok := c.awaitType(t, wire.MsgRejected, 500*time.Millisecond)
		if ok {
			rej, err := wire.DecodeRejected(b)
			if err != nil {
				t.Fatalf("DecodeRejected: %v", err)
			}
			if rej.Reason != wire.ReasonHalted {
				t.Fatalf("order after the log failed was rejected with reason %d, want ReasonHalted (%d)", rej.Reason, wire.ReasonHalted)
			}
			refusedAt = i
			break
		}
	}
	if refusedAt == 0 {
		t.Fatal("the venue kept accepting orders after its log stopped being writable — " +
			"every one of those acknowledgements is a command that was never journalled")
	}

	// Readiness follows, within a couple of sync ticks, and never clears.
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := adminGet(t, srv, "/readyz")
		if code == http.StatusServiceUnavailable {
			if !containsAll(body, "log") {
				t.Errorf("/readyz says %q; it must name the log as the reason", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz stayed %d after the log stopped being writable — the orchestrator would leave this node in rotation", code)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And nothing accepted afterwards exists: every further order is refused.
	for i := 0; i < 5; i++ {
		c.enter("after"+itoa(i), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
		if _, ok := c.awaitType(t, wire.MsgRejected, 2*time.Second); !ok {
			t.Fatalf("order %d after the failure was not refused — the writer is retrying instead of latching", i)
		}
	}

	// Restore the directory. The venue must NOT come back on its own: clearing a
	// durability failure takes a restart, which is where an operator gets to decide
	// whether the disk is actually fixed rather than transiently less full.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if code, _ := adminGet(t, srv, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d after the disk problem cleared; a latched durability failure must survive until a restart", code)
	}
	c.enter("recovered", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgRejected, 2*time.Second); !ok {
		t.Error("the venue resumed accepting orders once the directory was writable again, without a restart")
	}
}

// TestStopWaterGoesCancelOnly — below the stop-water mark the venue refuses new
// orders and still accepts cancels.
//
// Cancel-only rather than halt, and the reason is the client's rather than the
// venue's: stopping EVERYTHING leaves participants holding positions they cannot
// withdraw, on a venue whose disk is about to fill. Cancel-only lets them get flat
// and removes the largest source of log growth. It reuses a state that already
// exists on the wire — orderbook_phase 2, ReasonHalted for a refused new order —
// rather than inventing a reason byte a trading client would have to learn.
func TestStopWaterGoesCancelOnly(t *testing.T) {
	cfg := durableConfig(t)
	cfg.AdminAddr = "127.0.0.1:0"
	cfg.CheckpointEvery = 50 * time.Millisecond
	// A threshold no filesystem can satisfy, which is the whole of the injection:
	// the sampled free space is real, the number it is compared against is not.
	cfg.WALMinFreeStop = 1 << 62
	cfg.WALMinFree = 1 << 62
	if _, ok := wal.FreeBytes(cfg.WALPath); !ok {
		t.Skip("this platform cannot report free disk space, so the thresholds do not fire")
	}
	srv := durableServer(t, cfg)
	defer srv.Close()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	// One resting order placed before the threshold fires, so there is something to
	// cancel afterwards.
	c.enter("resting", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("the venue refused an order before the stop-water mark could fire")
	}

	// The checkpoint tick samples free space and goes cancel-only. Wait for the state
	// rather than for a rejection: a client that keeps sending while polling with
	// short read deadlines desynchronises its own stream, and the thing under test is
	// the transition, not the race.
	// Observed through the venue's own flag rather than through Engine.State, which
	// is a plain field read on the matching goroutine's data and is not safe to poll
	// from a test — a pre-existing property of pkg/matching that -race finds the
	// moment anything changes state while anything else is looking.
	deadline := time.Now().Add(5 * time.Second)
	for !srv.walStopped.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the venue never went cancel-only below the stop-water mark")
		}
		time.Sleep(10 * time.Millisecond)
	}

	c.enter("new1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	b, ok := c.awaitType(t, wire.MsgRejected, 3*time.Second)
	if !ok {
		t.Fatal("the venue kept accepting new orders below the stop-water mark")
	}
	rej, err := wire.DecodeRejected(b)
	if err != nil {
		t.Fatalf("DecodeRejected: %v", err)
	}
	if rej.Reason != wire.ReasonHalted {
		t.Fatalf("a new order below the stop-water mark was rejected with reason %d, want ReasonHalted (%d).\n"+
			"Reusing an existing client-visible state is only a defensible alternative to a new reason byte if the\n"+
			"existing one actually reaches the client.", rej.Reason, wire.ReasonHalted)
	}

	// And the metric an operator watches says the same thing.
	if code, body := adminGet(t, srv, "/metrics"); code != 200 || !containsAll(body, "orderbook_phase") {
		t.Errorf("/metrics = %d without orderbook_phase", code)
	}

	// A cancel still works, which is the entire argument for cancel-only over halt.
	c.cancel("resting")
	if _, ok := c.awaitType(t, wire.MsgCanceled, 3*time.Second); !ok {
		t.Error("a cancel was refused below the stop-water mark — participants cannot get flat, " +
			"which is the one thing cancel-only exists to allow")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
