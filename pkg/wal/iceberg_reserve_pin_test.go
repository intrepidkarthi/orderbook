package wal

import (
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestLogOnlyRecoveryLosesAnIcebergsReserve PINS a defect. It asserts today's WRONG
// behaviour, on purpose, and the sentence a fix has to come and delete is on the
// assertion itself: if this test starts failing because the reserve came back, the
// defect has been FIXED and this test should be inverted, keeping its name.
//
// A pin keeps its name after inversion for the reason
// pkg/matching/differential_findings_test.go:12-17 gives: renaming it hides the
// promise being kept.
//
// THE DEFECT. types.NewIcebergOrder shrinks Order.Quantity to the DISPLAY size at
// construction time (iceberg.go:34-41) and holds the rest in IcebergOrder.Hidden. By
// the time anything can be journalled that has already happened, so AppendIceberg —
// which logs ib.Order and ib.DisplayQty — writes `qty=3 display=3` for an order the
// client sent as nine lots shown three. A replay rebuilds it with
// NewIcebergOrder(entry.Order, entry.DisplayQty) and computes hidden = 3 - 3 = 0.
//
// So a recovery from the JOURNAL ALONE reconstructs every iceberg on the venue with
// an empty reserve, and the client's remaining size is not merely displayed — it is
// GONE. A recovery that starts from a snapshot is unaffected, because EngineSnapshot
// carries Hidden and Refills per iceberg; the tail is then replayed onto a book that
// already has the reserve.
//
// It is pre-existing — reproduced identically on the commit before
// docs/PINNED-DEFECTS.md's two fixes, with no fill-or-kill involved anywhere — and it
// is pinned rather than fixed because it is a question about what a KindIceberg
// entry's Quantity field MEANS on the wire, and about what a build should do when it
// meets a log written under the old meaning. That is a journal-compatibility argument
// with its own document to write, not a rider on a matching fix.
// docs/PINNED-DEFECTS.md §13.7 and §9.
//
// Its consequence for the matching fix that found it: on a log-only replay the reserve
// never exists, so a fill-or-kill cannot consume a second slice and the restored-
// iceberg path is not reached at all. CHANGELOG.md bounds the claim accordingly.
func TestLogOnlyRecoveryLosesAnIcebergsReserve(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "iceberg.wal")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	live := matching.NewEngine(matching.DefaultConfig("W"))

	base, err := types.NewOrder("u1", "W", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	ib, err := types.NewIcebergOrder(base, 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	if ib.Hidden != 6 {
		t.Fatalf("setup: the live iceberg holds %d in reserve, want 6", ib.Hidden)
	}
	if _, err := w.AppendIceberg(ib); err != nil {
		t.Fatalf("AppendIceberg: %v", err)
	}
	live.ProcessIceberg(ib)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// What actually reached the journal. This is the root of it, asserted directly so
	// a reader does not have to infer the cause from the symptom.
	ents, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var logged *Entry
	for i := range ents {
		if ents[i].Kind == KindIceberg {
			logged = &ents[i]
		}
	}
	if logged == nil {
		t.Fatalf("no KindIceberg entry in %d journal entries", len(ents))
	}
	if logged.Order.Quantity != 3 || logged.DisplayQty != 3 {
		t.Fatalf("the journalled iceberg reads qty=%d display=%d, want 3 and 3. IF THIS NOW READS "+
			"qty=9 THE DEFECT HAS BEEN FIXED: AppendIceberg has started logging the TOTAL, a replay "+
			"can compute the reserve, and this test should be inverted keeping its name. "+
			"docs/PINNED-DEFECTS.md §13.7.", logged.Order.Quantity, logged.DisplayQty)
	}

	// The symptom: recovery from the log alone.
	recovered, err := Recover(matching.DefaultConfig("W"), "", walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	buy, err := types.NewOrder("u2", "W", types.SideBuy, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	var got int64
	for _, tr := range recovered.Process(buy).Trades {
		got += tr.Quantity
	}
	if got != 3 {
		t.Fatalf("a buy of 9 against the log-only recovered book printed %d lots, want 3 (the display "+
			"slice, with no reserve behind it). IF THIS IS NOW 9 THE DEFECT HAS BEEN FIXED and this "+
			"test should be inverted keeping its name. docs/PINNED-DEFECTS.md §13.7.", got)
	}

	// The control, in the same test so the two are never read apart: the SAME order on
	// the live engine sells all nine, because the reserve is there.
	control, err := types.NewOrder("u2", "W", types.SideBuy, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	var want int64
	for _, tr := range live.Process(control).Trades {
		want += tr.Quantity
	}
	if want != 9 {
		t.Fatalf("the LIVE engine printed %d lots for the same buy, want 9. The control has broken, so "+
			"this test no longer measures what it claims to.", want)
	}
}

// TestSnapshotRecoveryKeepsAnIcebergsReserve is the other half of the pin above, and
// it is what makes that one a bounded statement rather than "iceberg recovery is
// broken". A snapshot carries Hidden and Refills, so a recovery that starts from one
// reconstructs the reserve exactly and the journal tail replays onto a book that
// already has it.
//
// It also holds the boundary itself: if a fix to the pin above ever lands, this test
// must still pass, and if it starts failing the fix broke the path that was working.
func TestSnapshotRecoveryKeepsAnIcebergsReserve(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "iceberg-snap.wal")
	snapPath := filepath.Join(dir, "iceberg.snap")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e := matching.NewEngine(matching.DefaultConfig("W"))

	base, err := types.NewOrder("u1", "W", types.SideSell, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	ib, err := types.NewIcebergOrder(base, 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	if _, err := w.AppendIceberg(ib); err != nil {
		t.Fatalf("AppendIceberg: %v", err)
	}
	e.ProcessIceberg(ib)

	if err := WriteSnapshot(snapPath, e.TakeSnapshot()); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recovered, err := Recover(matching.DefaultConfig("W"), snapPath, walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	buy, err := types.NewOrder("u2", "W", types.SideBuy, types.OrderTypeLimit, 100, 9, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	var got int64
	for _, tr := range recovered.Process(buy).Trades {
		got += tr.Quantity
	}
	if got != 9 {
		t.Fatalf("a buy of 9 against the snapshot-recovered book printed %d lots, want 9 — the "+
			"snapshot carries Hidden and Refills, so the reserve must survive. "+
			"docs/PINNED-DEFECTS.md §13.7.", got)
	}
}
