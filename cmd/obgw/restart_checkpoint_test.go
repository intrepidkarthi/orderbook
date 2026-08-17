package main

import (
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// TestTheFirstCheckpointAfterARestartKeepsTheLogPosition.
//
// A restart rebuilds the engine from the snapshot plus the log tail after it, and
// then hands that engine to a Runner. The Runner's lastApplied — the number every
// checkpoint stamps into the snapshot as WALSeq, and the number recovery uses to
// decide which records to replay — started at ZERO, because NewRunnerFor had no way
// to be told where in the log the engine it was handed already stood.
//
// Nothing notices while orders keep arriving: the first command applied after the
// restart sets lastApplied to its own (correct) sequence and the stamp is right from
// then on. The bug needs a restart followed by one checkpoint tick with NO command in
// between, which at the shipped 30-second -checkpoint cadence is an ordinary quiet
// market: out of hours, before the open, a maintenance window, a venue restarted and
// watched for a minute before the flow is pointed back at it.
//
// What lands on disk then is a snapshot holding the COMPLETE book stamped WALSeq 0,
// which reads as "this snapshot covers nothing; replay the log from the beginning".
// The next restart applies the whole log on top of a snapshot that already contains
// it. What is asserted below is that it does not: the stamp, and the count of records
// the following recovery re-applies.
//
// Whether that also DOUBLES the book depends on the duplicate-client-order-id guard,
// which is worth being precise about rather than assuming. cmd/obgw sets
// DedupClientOrderIDs to 4096 and the guard is recovered state that runs during replay
// too, so re-submitted orders are refused as duplicates — while three things hold: the
// ids since the snapshot fit in the ring, the embedder configured a ring at all (the
// pkg/matching zero value is no ring), and the order CARRIES a client order id. The
// wire does not require one, and an order without one is never deduped, which is
// TestAnOrderWithNoClientIDIsNotProtectedFromTheReplay below: 50 orders in, 100 orders
// back, over TCP through this server. So the assertion here is on the log position,
// which is the defect, rather than on a bounded guard happening to hold.
//
// This defect predates segmentation and retention; it reproduces at ffd9a96. With
// -wal-retain set it stops being silent and becomes fatal instead, which is
// TestASnapshotWrittenAfterAQuietRestartStillCoversTheRetainedFloor.
func TestTheFirstCheckpointAfterARestartKeepsTheLogPosition(t *testing.T) {
	cfg := durableConfig(t)
	srv := durableServer(t, cfg)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	const orders = 3
	for i := 0; i < orders; i++ {
		c.enter(clientID(i), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, int64(100+i), 10)
		if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("order %d not accepted", i)
		}
	}
	srv.Close()

	logged, err := wal.ReadAll(cfg.WALPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(logged) != orders {
		t.Fatalf("the log holds %d records, want %d", len(logged), orders)
	}
	lastSeq := logged[len(logged)-1].Seq

	// Restart, and then do nothing at all — no client, no order, no cancel. This is
	// the whole scenario.
	cfg.Addr = "127.0.0.1:0"
	revived := durableServer(t, cfg)
	defer revived.Close()
	if got := revived.runner.OrderCount(); got != orders {
		t.Fatalf("recovered %d orders, want %d", got, orders)
	}

	snap, err := revived.runner.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if snap.WALSeq != lastSeq {
		t.Errorf("the first checkpoint after a restart stamped WALSeq %d, want %d.\n"+
			"The Runner was handed an engine rebuilt from the log and told nothing about where in the log it\n"+
			"stood, so it stamped a complete book with a position that covers none of it. Recovery reads that\n"+
			"as \"replay everything\" and applies the whole log a second time.", snap.WALSeq, lastSeq)
	}
	if err := wal.WriteSnapshot(cfg.SnapshotPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	revived.Close()

	// What the NEXT recovery makes of that pair, measured rather than inferred: the
	// snapshot must cover the whole log, so nothing is left to apply on top of it.
	_, rep, err := wal.RecoverWithReport(matching.DefaultConfig("X"), cfg.SnapshotPath, cfg.WALPath)
	if err != nil {
		t.Fatalf("RecoverWithReport: %v", err)
	}
	if rep.SnapshotSeq != lastSeq {
		t.Errorf("the snapshot on disk is stamped %d, want %d", rep.SnapshotSeq, lastSeq)
	}
	if rep.Applied != 0 {
		t.Errorf("the restart after a quiet checkpoint re-applies %d of the log's %d records on top of a "+
			"snapshot that already contains them, want 0", rep.Applied, len(logged))
	}

	cfg.Addr = "127.0.0.1:0"
	again := durableServer(t, cfg)
	defer again.Close()
	if got := again.runner.OrderCount(); got != orders {
		t.Fatalf("the restart after a quiet checkpoint recovered %d orders, want %d", got, orders)
	}
}

// clientID is a stable per-index client order id; the wire cares only that they
// differ.
func clientID(i int) string { return string(rune('a'+i)) + "-restart" }

// TestASnapshotWrittenAfterAQuietRestartStillCoversTheRetainedFloor is the same
// defect seen through retention, which is what turns it from a wrong book into a
// venue that will not start.
//
// The zeroed stamp does not merely mislead recovery — once -wal-retain has deleted a
// prefix of the set, the floor check compares the snapshot's WALSeq against the base
// of the oldest segment still on disk and refuses, correctly, because the sequences
// between them really are in no file. The snapshot holds the whole book, so the
// refusal is a false positive produced entirely by the stamp; and archival is off by
// default, so the segments it names are gone.
func TestASnapshotWrittenAfterAQuietRestartStillCoversTheRetainedFloor(t *testing.T) {
	cfg := durableConfig(t)
	cfg.WALSegmentBytes = 4 << 10
	cfg.WALRetainBytes = 8 << 10
	srv := durableServer(t, cfg)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	// Enough orders to fill several 4 KiB segments.
	const orders = 120
	for i := 0; i < orders; i++ {
		c.enter(clientID(i%26)+string(rune('A'+i/26)), wire.SideBuy, wire.TypeLimit,
			wire.TIFGoodTillCancel, int64(100+i%50), 10)
		if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("order %d not accepted", i)
		}
	}
	// A checkpoint while the venue is live, then a retention cycle: this is what the
	// checkpoint loop does, run by hand so the test does not wait on a ticker.
	snap, err := srv.runner.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := wal.WriteSnapshot(cfg.SnapshotPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if _, err := wal.Retain(cfg.WALPath, cfg.SnapshotPath, cfg.walOptions()); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	srv.Close()

	cfg.Addr = "127.0.0.1:0"
	revived := durableServer(t, cfg)
	if got := revived.runner.OrderCount(); got != orders {
		t.Fatalf("recovered %d orders, want %d", got, orders)
	}
	// The quiet tick: a checkpoint with no command applied since the restart.
	snap, err = revived.runner.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := wal.WriteSnapshot(cfg.SnapshotPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	revived.Close()

	info, err := wal.Stat(cfg.WALPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Floor <= 1 {
		t.Skipf("retention did not fire (floor %d across %d segments); the fixture is not exercising the floor",
			info.Floor, info.Segments)
	}

	cfg.Addr = "127.0.0.1:0"
	again, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("the venue refuses to start after one quiet checkpoint tick, over a retained set whose "+
			"floor is %d: %v\n"+
			"The snapshot on disk holds the whole book. Only its WALSeq stamp is wrong, and the segments the "+
			"error names have already been deleted.", info.Floor, err)
	}
	defer again.Close()
	if got := again.runner.OrderCount(); got != orders {
		t.Errorf("recovered %d orders, want %d", got, orders)
	}
}

// TestAnOrderWithNoClientIDIsNotProtectedFromTheReplay is the same defect with the one
// thing that was hiding it taken away.
//
// The duplicate-client-order-id ring is what keeps a re-applied log from double-booking
// a venue whose snapshot stamp is wrong, and it is not a general protection: dedupKey
// returns "" for an order with no client id, so such an order is never deduped and is
// simply submitted again. The wire accepts an empty ClOrdID — this test sends one — so
// the path is reachable by an ordinary client over TCP, not only by an embedder who
// left DedupClientOrderIDs at zero.
//
// Measured against the unfixed code: restart 1 recovers 50 orders, the quiet checkpoint
// stamps WALSeq 0, restart 2 recovers 100 orders from a 50-record log. No error, no log
// line, and a book with every order in it twice.
func TestAnOrderWithNoClientIDIsNotProtectedFromTheReplay(t *testing.T) {
	cfg := durableConfig(t)
	srv := durableServer(t, cfg)

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	const orders = 50
	for i := 0; i < orders; i++ {
		c.enter("", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, int64(100+i), 10)
		if _, ok := c.await(t, wire.AcceptedLen, 3*time.Second); !ok {
			t.Fatalf("order %d without a client id was not accepted; the fixture no longer reaches the "+
				"undeduplicated path", i)
		}
	}
	srv.Close()

	cfg.Addr = "127.0.0.1:0"
	revived := durableServer(t, cfg)
	if got := revived.runner.OrderCount(); got != orders {
		t.Fatalf("restart 1 recovered %d orders, want %d", got, orders)
	}
	snap, err := revived.runner.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := wal.WriteSnapshot(cfg.SnapshotPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	revived.Close()

	cfg.Addr = "127.0.0.1:0"
	again := durableServer(t, cfg)
	defer again.Close()
	if got := again.runner.OrderCount(); got != orders {
		t.Fatalf("restart 2 recovered %d orders from a %d-record log, want %d — the log was applied on top "+
			"of a snapshot that already contained it, and nothing deduplicated it because these orders carry "+
			"no client order id", got, orders, orders)
	}
}
