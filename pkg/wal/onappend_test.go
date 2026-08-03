package wal

import (
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestOnAppendSeesTheFileStream — the hook's promise is that a subscriber sees
// the same stream a reader of the file would: same entries, same order, same
// sequence numbers. Asserted by comparing the hook's capture against ReadAll.
func TestOnAppendSeesTheFileStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var hooked []Entry
	w.SetOnAppend(func(e Entry) { hooked = append(hooked, e) })

	if _, err := w.AppendSubmit(newOrder(t, "alice", types.SideBuy, 100, 5)); err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	if _, err := w.AppendCancel(1, "alice"); err != nil {
		t.Fatalf("AppendCancel: %v", err)
	}
	if _, err := w.AppendReduce(2, 3, "bob"); err != nil {
		t.Fatalf("AppendReduce: %v", err)
	}

	// The hook fires on append, BEFORE durability: nothing has been synced yet,
	// and the subscriber already holds all three entries. That is the documented
	// caveat, asserted so it cannot silently become "fires on Sync" later.
	if len(hooked) != 3 {
		t.Fatalf("hook saw %d entries before Sync, want 3", len(hooked))
	}

	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	onDisk, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(onDisk) != len(hooked) {
		t.Fatalf("file has %d entries, hook saw %d", len(onDisk), len(hooked))
	}
	for i := range onDisk {
		if onDisk[i].Seq != hooked[i].Seq || onDisk[i].Kind != hooked[i].Kind {
			t.Errorf("entry %d: file (seq=%d kind=%d) != hook (seq=%d kind=%d)",
				i, onDisk[i].Seq, onDisk[i].Kind, hooked[i].Seq, hooked[i].Kind)
		}
		if int64(i+1) != hooked[i].Seq {
			t.Errorf("hook entry %d has seq %d; the stream must be gapless from 1", i, hooked[i].Seq)
		}
	}
}

// TestOnAppendIsNotCalledForARefusedAppend — an entry that never reached the
// log must never reach a subscriber, or the follower holds a command the
// primary does not.
func TestOnAppendIsNotCalledForARefusedAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	calls := 0
	w.SetOnAppend(func(Entry) { calls++ })

	// An order whose UserID is grown past MaxRecordBytes cannot be marshalled
	// into a legal record; the append is refused.
	huge := newOrder(t, "alice", types.SideBuy, 100, 5)
	huge.UserID = string(make([]byte, MaxRecordBytes+1))
	if _, err := w.AppendSubmit(huge); err == nil {
		t.Fatal("test premise broken: an oversized record was accepted")
	}
	if calls != 0 {
		t.Errorf("hook fired %d time(s) for a refused append", calls)
	}
}

// TestOnAppendNilClearsAndDefaultIsSilent — appends must work with no hook set,
// and clearing must actually clear.
func TestOnAppendNilClearsAndDefaultIsSilent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	if _, err := w.AppendCancel(1, "alice"); err != nil {
		t.Fatalf("append with no hook: %v", err)
	}
	calls := 0
	w.SetOnAppend(func(Entry) { calls++ })
	if _, err := w.AppendCancel(2, "alice"); err != nil {
		t.Fatalf("append with hook: %v", err)
	}
	w.SetOnAppend(nil)
	if _, err := w.AppendCancel(3, "alice"); err != nil {
		t.Fatalf("append after clear: %v", err)
	}
	if calls != 1 {
		t.Errorf("hook called %d time(s), want exactly 1 (only while set)", calls)
	}
}
