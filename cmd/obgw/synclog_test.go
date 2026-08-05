package main

import (
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// TestSyncEveryCommandIsDurableBeforeApply is the property the flag exists for,
// and it is asserted the only way that means anything: read the file from a
// second file descriptor the writer has never flushed to, immediately after the
// append returns. In group-commit mode the record is still in the writer's
// buffer and a reader sees nothing; with the flag it is on disk.
//
// This is what separates "the log is never missing something the book did"
// (true either way, because the append precedes the apply) from "an
// acknowledged order cannot be lost" (true only here). The package comment for
// pkg/wal claimed the second for four releases and only had the first.
func TestSyncEveryCommandIsDurableBeforeApply(t *testing.T) {
	order := func(t *testing.T) *types.Order {
		t.Helper()
		o, err := types.NewOrder("alice", "X", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		return o
	}
	// onDisk reports what a separate reader can see right now, which is the only
	// definition of durable that survives a process death.
	onDisk := func(t *testing.T, path string) int {
		t.Helper()
		entries, err := wal.ReadAll(path)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		return len(entries)
	}

	t.Run("group commit leaves it buffered", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wal.log")
		w, err := wal.Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer w.Close()

		if _, err := w.AppendSubmit(order(t)); err != nil {
			t.Fatalf("AppendSubmit: %v", err)
		}
		if n := onDisk(t, path); n != 0 {
			t.Errorf("a reader saw %d records before any Sync; the test premise (that the default buffers) is wrong", n)
		}
	})

	t.Run("sync-every-command puts it on disk", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wal.log")
		w, err := wal.Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer w.Close()

		sl := &syncingLog{w: w}
		if _, err := sl.AppendSubmit(order(t)); err != nil {
			t.Fatalf("AppendSubmit: %v", err)
		}
		if n := onDisk(t, path); n != 1 {
			t.Errorf("a reader saw %d records straight after the append, want 1 — the flag does not make the record durable", n)
		}
	})

	t.Run("every command kind syncs", func(t *testing.T) {
		// A kind that forgets to sync is the same bug as no flag at all, and it
		// would be invisible on the submit path alone.
		path := filepath.Join(t.TempDir(), "wal.log")
		w, err := wal.Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer w.Close()
		sl := &syncingLog{w: w}

		stop, err := types.NewStopOrder(order(t), 99)
		if err != nil {
			t.Fatalf("NewStopOrder: %v", err)
		}
		ib, err := types.NewIcebergOrder(order(t), 1)
		if err != nil {
			t.Fatalf("NewIcebergOrder: %v", err)
		}
		peg, err := types.NewPeggedOrder(order(t), types.PegToBid, 0)
		if err != nil {
			t.Fatalf("NewPeggedOrder: %v", err)
		}
		trail, err := types.NewTrailingStop(order(t), 5)
		if err != nil {
			t.Fatalf("NewTrailingStop: %v", err)
		}
		stopLeg, err := types.NewStopOrder(order(t), 99)
		if err != nil {
			t.Fatalf("NewStopOrder: %v", err)
		}
		oco, err := types.NewOCOOrder(order(t), stopLeg)
		if err != nil {
			t.Fatalf("NewOCOOrder: %v", err)
		}

		appends := []struct {
			name string
			fn   func() (int64, error)
		}{
			{"submit", func() (int64, error) { return sl.AppendSubmit(order(t)) }},
			{"cancel", func() (int64, error) { return sl.AppendCancel(1, "alice") }},
			{"reduce", func() (int64, error) { return sl.AppendReduce(1, 2, "alice") }},
			{"replace", func() (int64, error) { return sl.AppendReplace(1, "alice", order(t)) }},
			{"cancelAll", func() (int64, error) { return sl.AppendCancelAll("alice") }},
			{"stop", func() (int64, error) { return sl.AppendStop(stop) }},
			{"oco", func() (int64, error) { return sl.AppendOCO(oco) }},
			{"iceberg", func() (int64, error) { return sl.AppendIceberg(ib) }},
			{"pegged", func() (int64, error) { return sl.AppendPegged(peg) }},
			{"trailing", func() (int64, error) { return sl.AppendTrailing(trail) }},
		}
		for i, a := range appends {
			if _, err := a.fn(); err != nil {
				t.Fatalf("%s: %v", a.name, err)
			}
			if n := onDisk(t, path); n != i+1 {
				t.Errorf("after %s a reader saw %d records, want %d — that kind does not sync", a.name, n, i+1)
			}
		}
	})
}
