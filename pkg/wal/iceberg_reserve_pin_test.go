package wal

import (
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestLogOnlyRecoveryLosesAnIcebergsReserve is an INVERTED PIN: it now asserts the
// opposite of its name, and it keeps the name deliberately.
//
// It used to pin a defect — to assert today's WRONG behaviour on purpose, with the
// sentence a fix had to come and delete written on the assertion itself. That fix has
// landed (docs/ICEBERG-DURABILITY.md), so the sentence is gone and every assertion is
// turned over: the journalled record now states the total, the log-only recovery
// rebuilds the reserve, and the buy of nine prints nine.
//
// The name survives the inversion for the reason
// pkg/matching/differential_findings_test.go:12-17 gives: renaming it hides the
// promise being kept. A reader who finds this test by searching for the defect
// finds the assertion that it cannot come back.
//
// WHAT THE DEFECT WAS. types.NewIcebergOrder shrinks Order.Quantity to the DISPLAY
// size at construction time (iceberg.go:34-41) and holds the rest in
// IcebergOrder.Hidden. By the time anything can be journalled that has already
// happened, so AppendIceberg — which logged ib.Order and ib.DisplayQty — wrote
// `qty=3 display=3` for an order the client sent as nine lots shown three, and a
// replay rebuilt it with hidden = 3 - 3 = 0. A recovery from the JOURNAL ALONE
// reconstructed every iceberg on the venue with an empty reserve, and the client's
// remaining size was not merely undisplayed — it was GONE. A recovery that started
// from a snapshot was unaffected, which is what the other half of this file holds.
//
// WHAT FIXED IT. AppendIceberg logs a COPY of the order stating the client's total,
// plus Entry.TotalQty as the witness that a record was written by a build that knew
// the difference — a record without it is refused by Recover rather than replayed
// into a wrong book (ErrIcebergReserveUnknown). docs/PINNED-DEFECTS.md §13.7.
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

	// Rule 2: the writer logs a COPY. The engine is matching against that exact
	// pointer, and a writer that restored the total on the LIVE order would rest nine
	// lots showing nine — the defect inverted and worse.
	if ib.Order.Quantity != 3 || ib.Order.RemainingQty != 3 || ib.Hidden != 6 {
		t.Fatalf("AppendIceberg mutated the LIVE order: it now reads qty=%d remaining=%d hidden=%d, "+
			"want 3, 3 and 6. The engine is matching against that pointer. "+
			"docs/ICEBERG-DURABILITY.md §3.2 Rule 2",
			ib.Order.Quantity, ib.Order.RemainingQty, ib.Hidden)
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
	if logged.Order.Quantity != 9 || logged.Order.RemainingQty != 9 || logged.DisplayQty != 3 || logged.TotalQty != 9 {
		t.Fatalf("the journalled iceberg reads qty=%d remaining=%d display=%d total=%d, want 9, 9, 3 and 9 — "+
			"the order as the CLIENT SENT IT, plus the witness that says so. If qty is 3 the writer has "+
			"gone back to logging the display slice and the reserve is being thrown away again; if total "+
			"is 0 the witness is gone and a pre-fix record can no longer be told from a post-fix one. "+
			"docs/ICEBERG-DURABILITY.md §3.",
			logged.Order.Quantity, logged.Order.RemainingQty, logged.DisplayQty, logged.TotalQty)
	}

	// The symptom that was: recovery from the log alone.
	recovered, err := Recover(matching.DefaultConfig("W"), "", walPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if snap := recovered.TakeSnapshot(); len(snap.Icebergs) != 1 || snap.Icebergs[0].Hidden != 6 {
		t.Fatalf("the log-only recovered engine holds %+v, want exactly one iceberg with Hidden 6",
			snap.Icebergs)
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
		t.Fatalf("a buy of 9 against the log-only recovered book printed %d lots, want 9. If this is 3, "+
			"the record has stopped carrying the client's total and the reserve is gone again — every "+
			"command after a trade against such an order replays against a different book. "+
			"docs/ICEBERG-DURABILITY.md §2.", got)
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
	if got != want {
		t.Fatalf("the log-only recovered book printed %d lots where the live venue printed %d", got, want)
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
