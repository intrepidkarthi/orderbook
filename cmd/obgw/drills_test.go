package main

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/observability"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// Drills.
//
// One per entry in docs/RUNBOOKS.md. Each induces the real failure — not a simulation
// of its symptoms — and asserts the signal the runbook tells an operator to look for,
// and where the runbook prescribes a procedure, that the procedure works.
//
// They exist because a rehearsal you perform once is a memory and a rehearsal in CI is
// a capability, and because the runbooks would otherwise rot silently: a reason string
// renamed, a status code changed, a fallback that stopped falling back, and the page an
// operator reads at three in the morning is quietly wrong. These fail when the code and
// the page disagree, which is the only way a document like that stays true.
//
// Writing the runbooks already found one defect — the snapshot had no checksum, and the
// honest procedure would have been "you cannot detect this". These are the same
// exercise carried through to the code.
//
// Coverage of docs/RUNBOOKS.md, so a reader can audit it rather than trust it:
//
//	A torn log                     TestATornLogStillYieldsANameableBook (recovery_test.go)
//	A corrupt log record           TestDrillCorruptLogRecordRefusesToStart
//	A corrupt snapshot             TestDrillCorruptSnapshotProcedureRestoresTheSameBook
//	A stuck matching goroutine     TestDrillAStalledMatcherIsVisibleAndAQuietOneIsNot
//	A mass cancel                  TestDrillAMassCancelIsNotAStall
//	An evicted subscriber          TestMarketDataRejectsAnEvictedCursor (mdserver_test.go),
//	                               TestResumeRefusesEvictedSequence (pkg/orderentry)
//	Publisher dropping batches     TestDrillDroppedBatchesAreVisible
//	Book at its order ceiling      TestDrillTheCeilingRejectionNamesItself

// --- RUNBOOKS.md § A corrupt log record ---------------------------------------

// TestDrillCorruptLogRecordRefusesToStart.
//
// Runbook: "The venue refuses to start: `wal: corrupt record`." The whole procedure
// depends on that refusal — the alternative, truncating at the bad record, silently
// discards everything after it and produces a book that is plausible and wrong.
func TestDrillCorruptLogRecordRefusesToStart(t *testing.T) {
	cfg := durableConfig(t)
	cfg.SnapshotPath = "" // force recovery through the log
	srv := durableServer(t, cfg)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	for _, id := range []string{"a1", "a2", "a3"} {
		c.enter(id, wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
		if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
			t.Fatalf("%s not accepted", id)
		}
	}
	srv.Close()

	// Change a byte inside a complete record, in the middle of the file. This is media
	// corruption, not a torn write: the record is whole and its checksum no longer
	// matches its bytes.
	b, err := os.ReadFile(cfg.WALPath)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	i := strings.Index(string(b), "a2")
	if i < 0 {
		t.Fatal("test premise broken: the second order is not in the log")
	}
	b[i] = 'X'
	if err := os.WriteFile(cfg.WALPath, b, 0o644); err != nil {
		t.Fatalf("write wal: %v", err)
	}

	cfg.Addr = "127.0.0.1:0"
	_, err = NewServer(cfg)
	if !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("NewServer err = %v, want %v — the venue would have started on a truncated book", err, wal.ErrCorrupt)
	}
}

// --- RUNBOOKS.md § A corrupt snapshot -----------------------------------------

// TestDrillCorruptSnapshotProcedureRestoresTheSameBook rehearses the procedure, not
// just the detection.
//
// Runbook: "Delete the snapshot and restart. Recovery falls back to replaying the log
// from the beginning, which is slower but exact." Detection is tested in pkg/wal. What
// is tested here is the sentence an operator will actually act on — that deleting the
// file and restarting gets the same book back.
func TestDrillCorruptSnapshotProcedureRestoresTheSameBook(t *testing.T) {
	cfg := durableConfig(t)
	cfg.CheckpointEvery = 0 // checkpoint explicitly, so the test controls the timing
	srv := durableServer(t, cfg)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	for _, id := range []string{"a1", "a2", "a3", "a4"} {
		c.enter(id, wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
		if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
			t.Fatalf("%s not accepted", id)
		}
	}
	want := srv.runner.OrderCount()
	if want != 4 {
		t.Fatalf("setup: book has %d orders, want 4", want)
	}
	snap, err := srv.runner.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := wal.WriteSnapshot(cfg.SnapshotPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	srv.Close()

	// Corrupt it in a way a JSON parser cannot see: a digit inside a number.
	b, err := os.ReadFile(cfg.SnapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	i := strings.Index(string(b), "100")
	if i < 0 {
		t.Fatal("test premise broken: the price is not in the snapshot")
	}
	b[i] = '9'
	if err := os.WriteFile(cfg.SnapshotPath, b, 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	cfg.Addr = "127.0.0.1:0"
	if _, err := NewServer(cfg); !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("NewServer err = %v, want %v — the venue would have traded on a book that never existed", err, wal.ErrCorrupt)
	}

	// The procedure.
	if err := os.Remove(cfg.SnapshotPath); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	revived, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("the documented procedure did not work: %v", err)
	}
	if err := revived.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = revived.Serve() }()
	defer revived.Close()

	if got := revived.runner.OrderCount(); got != want {
		t.Errorf("replay recovered %d orders, want %d — the fallback is not exact", got, want)
	}
	// And the recovered orders are still nameable, which is what makes the book usable
	// rather than merely present.
	c2 := dial(t, revived)
	c2.mustLogin("alice", "pw1")
	c2.cancel("a1")
	if _, ok := c2.awaitType(t, wire.MsgCanceled, 3*time.Second); !ok {
		t.Error("an order recovered by the fallback could not be cancelled")
	}
}

// --- RUNBOOKS.md § A stuck matching goroutine ---------------------------------

// blockingLog stalls the matching goroutine on its first write, which is exactly the
// failure the runbook describes: "Every command is journalled before it is applied, so
// a write that never returns stops the matcher and looks exactly like this."
type blockingLog struct {
	release chan struct{}
	once    sync.Once
}

func (l *blockingLog) block() {
	l.once.Do(func() { <-l.release })
}
func (l *blockingLog) AppendSubmit(o *types.Order) (int64, error) { l.block(); return 0, nil }
func (l *blockingLog) AppendCancel(int64, string) (int64, error)  { l.block(); return 0, nil }
func (l *blockingLog) AppendReduce(int64, int64, string) (int64, error) {
	l.block()
	return 0, nil
}
func (l *blockingLog) AppendReplace(int64, string, *types.Order) (int64, error) {
	l.block()
	return 0, nil
}
func (l *blockingLog) AppendCancelAll(string) (int64, error)             { l.block(); return 0, nil }
func (l *blockingLog) AppendStop(*types.StopOrder) (int64, error)        { l.block(); return 0, nil }
func (l *blockingLog) AppendOCO(*types.OCOOrder) (int64, error)          { l.block(); return 0, nil }
func (l *blockingLog) AppendIceberg(*types.IcebergOrder) (int64, error)  { l.block(); return 0, nil }
func (l *blockingLog) AppendPegged(*types.PeggedOrder) (int64, error)    { l.block(); return 0, nil }
func (l *blockingLog) AppendTrailing(*types.TrailingStop) (int64, error) { l.block(); return 0, nil }
func (l *blockingLog) AppendHalt() (int64, error)                        { l.block(); return 0, nil }
func (l *blockingLog) AppendResume() (int64, error)                      { l.block(); return 0, nil }
func (l *blockingLog) AppendCancelOnly() (int64, error)                  { l.block(); return 0, nil }
func (l *blockingLog) AppendSetMark(int64) (int64, error)                { l.block(); return 0, nil }
func (l *blockingLog) AppendBust(int64, string) (int64, error)           { l.block(); return 0, nil }

// TestDrillAStalledMatcherIsVisibleAndAQuietOneIsNot induces a real stall and checks
// the signal the runbook is built on.
//
// A stalled matcher reads zero in every rate metric, and so does a quiet market. The
// runbook's whole distinction is that a stall has commands WAITING while the event
// sequence stands still. This drills both halves: the stall must be reported, and an
// idle venue must not be.
func TestDrillAStalledMatcherIsVisibleAndAQuietOneIsNot(t *testing.T) {
	log := &blockingLog{release: make(chan struct{})}
	col := observability.NewCollector()
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = col
	r := matching.NewRunner(matching.RunnerConfig{Engine: cfg, QueueSize: 64, Log: log})
	defer func() {
		close(log.release) // let the matcher finish so Close can drain
		r.Close()
	}()

	srv := &Server{runner: r, metrics: col}
	srv.admin.lastMoved = time.Now()

	// An idle venue: nothing queued, nothing happening. Must not read as a stall
	// however long the sequence has stood still.
	srv.admin.lastMoved = time.Now().Add(-time.Hour)
	if ready, why := srv.readiness(); !ready {
		t.Errorf("an idle venue reported unready: %q", why)
	}

	// Now the real stall: commands go in and the log never returns.
	for i := 0; i < 8; i++ {
		_ = r.TryEnqueue(mkDrillOrder(t, "alice", 100+int64(i)))
	}
	deadline := time.Now().Add(3 * time.Second)
	for r.QueueLen() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r.QueueLen() == 0 {
		t.Fatal("test premise broken: nothing queued behind the blocked log")
	}
	if seq := col.Snapshot().LastEventSeq; seq != 0 {
		t.Fatalf("test premise broken: the matcher emitted event %d while blocked", seq)
	}

	srv.admin.lastMoved = time.Now().Add(-2 * stallWindow)
	ready, why := srv.readiness()
	if ready {
		t.Fatalf("a stalled matcher reported ready: %q", why)
	}
	if !strings.Contains(why, "stalled") {
		t.Errorf("reason = %q; the runbook tells an operator to look for %q", why, "stalled")
	}
}

// TestDrillAMassCancelIsNotAStall is the runbook's "what makes it worse" for the mass
// cancel entry: do not treat the latency spike as a stall and restart. The distinction
// is that during a mass cancel the event sequence is advancing.
func TestDrillAMassCancelIsNotAStall(t *testing.T) {
	col := observability.NewCollector()
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = col
	r := matching.NewRunner(matching.RunnerConfig{Engine: cfg, QueueSize: 4096})
	defer r.Close()

	for i := 0; i < 500; i++ {
		r.Process(mkDrillOrder(t, "alice", 100+int64(i%50)))
	}
	before := col.Snapshot().LastEventSeq

	done, err := r.TryCancelAllAsync("alice")
	if err != nil {
		t.Fatalf("TryCancelAllAsync: %v", err)
	}
	select {
	case n := <-done:
		if n == 0 {
			t.Fatal("mass cancel removed nothing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mass cancel never completed")
	}

	if after := col.Snapshot().LastEventSeq; after <= before {
		t.Errorf("event sequence %d -> %d; a mass cancel must advance it, or it is indistinguishable from a stall",
			before, after)
	}
}

// --- RUNBOOKS.md § The publisher is dropping batches --------------------------

// TestDrillDroppedBatchesAreVisible.
//
// Runbook: "obgw_publisher_dropped_total — any increase" is a data-loss incident. It is
// the most severe signal on the page and the metric did not exist until a soak went
// looking, so the drill is that overflowing the publisher actually moves it.
func TestDrillDroppedBatchesAreVisible(t *testing.T) {
	// A real server, through its own wiring. Registering a gauge by hand in the test
	// would prove the collector works and prove nothing about whether cmd/obgw
	// actually exports this — which is the thing an operator's alert depends on.
	//
	// Serve() is never called, so the publisher's pump never starts and its queue
	// fills. That is not a contrived state: it is what a pump too slow to keep up
	// looks like, held still.
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 8, StreamRing: 16,
	})

	o := mkDrillOrder(t, "alice", 100)
	for i := 0; i < 1<<17; i++ {
		srv.pub.OnEvents([]matching.Event{{Seq: int64(i + 1), Kind: matching.EventAccepted, Order: o}})
		if srv.pub.Dropped() > 0 {
			break
		}
	}
	if srv.pub.Dropped() == 0 {
		t.Fatal("test premise broken: nothing was dropped")
	}

	var buf strings.Builder
	if err := srv.metrics.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "obgw_publisher_dropped_total") {
		t.Fatalf("the metric the runbook names is not exported by cmd/obgw at all:\n%s", out)
	}
	if strings.Contains(out, "obgw_publisher_dropped_total 0\n") {
		t.Error("the gauge reads zero after a real overflow; the data loss would be invisible")
	}
}

// --- RUNBOOKS.md § The book is at its order ceiling ---------------------------

// TestDrillTheCeilingRejectionNamesItself pins the string the runbook quotes.
//
// Runbook: alert on `orderbook_rejections_total{reason="order book has reached maximum
// capacity"}`. A label an operator greps for is part of the interface, and renaming the
// error would silently break every dashboard built on it.
func TestDrillTheCeilingRejectionNamesItself(t *testing.T) {
	col := observability.NewCollector()
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = col
	cfg.MaxOrders = 4
	e := matching.NewEngine(cfg)

	for i := 0; i < 10; i++ {
		e.Process(mkDrillOrder(t, "alice", 100+int64(i)))
	}
	if got := e.OrderCount(); got != 4 {
		t.Fatalf("book holds %d orders, want the ceiling of 4", got)
	}

	var buf strings.Builder
	if err := col.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	want := `orderbook_rejections_total{reason="order book has reached maximum capacity"}`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("the runbook tells an operator to alert on\n  %s\nand the exposition does not contain it:\n%s",
			want, buf.String())
	}
}

func mkDrillOrder(t *testing.T, user string, price int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", types.SideBuy, types.OrderTypeLimit, price, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}
