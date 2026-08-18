package wal

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The gate: what recovery does with a record that is intact and INSUFFICIENT.
// docs/ICEBERG-DURABILITY.md §4, deliverables 7 to 10.
//
// Every fixture here is HAND-BUILT, and it has to be: this build cannot write a
// KindIceberg record without a total, which is the whole point of the witness field.
// The records below are byte-for-byte what the previous four releases wrote.

// preFixIceberg is the record the old writer produced for "iceberg, nine lots, show
// three": the display slice in Order.Quantity, no total anywhere, and nothing in it
// that says the reserve is missing rather than genuinely zero.
func preFixIceberg(t *testing.T, seq int64, user, clOrdID string, display int64) Entry {
	t.Helper()
	o := wrapperOrder(t, user, types.SideSell, types.OrderTypeLimit, 100, display)
	o.ClientOrderID = clOrdID
	return Entry{Seq: seq, Kind: KindIceberg, Order: o, DisplayQty: display}
}

// postFixIceberg is what this build writes for the same command.
func postFixIceberg(t *testing.T, seq int64, total, display int64) Entry {
	t.Helper()
	o := wrapperOrder(t, "u1", types.SideSell, types.OrderTypeLimit, 100, total)
	return Entry{Seq: seq, Kind: KindIceberg, Order: o, DisplayQty: display, TotalQty: total}
}

// TestPreFixIcebergRecordInTheReplaySetRefuses is deliverable 7.
//
// The record is intact, correctly framed, its checksum passes and its sequence is
// contiguous. It is refused anyway, because replaying it rebuilds a client's nine
// lots as three and every command after a trade against it replays against a
// different book (TestALostReserveInventsTradesAndFlipsTheBook measures what that
// costs). The message has to name the records and their orders: the operator's next
// action is to cancel them and call their owners.
func TestPreFixIcebergRecordInTheReplaySetRefuses(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "prefix.wal")
	writeIcebergLog(t, walPath, []Entry{preFixIceberg(t, 1, "u17", "ACME-8841", 3)})

	eng, rep, err := RecoverWithOptions(tapeCfg(), "", walPath, RecoverOptions{})
	if !errors.Is(err, ErrIcebergReserveUnknown) {
		t.Fatalf("Recover err = %v, want ErrIcebergReserveUnknown — a record that cannot state its "+
			"reserve was replayed into the book", err)
	}
	if eng != nil {
		t.Error("a refused recovery returned an engine; the caller could serve it")
	}
	if rep.IcebergsWithoutReserve != 1 {
		t.Errorf("report says %d lossy iceberg records, want 1 — the count is what an operator is "+
			"asked to name back", rep.IcebergsWithoutReserve)
	}
	if rep.IcebergReserveLossAccepted {
		t.Error("the report claims the loss was accepted, and it was refused")
	}
	for _, want := range []string{
		"sequence 1",                 // which record
		"u17/ACME-8841",              // whose order
		"displays 3",                 // what it does say
		"-wal-accept-iceberg-loss 1", // the route, with the count already filled in
		"Starting the PREVIOUS build does not help",
		"docs/RUNBOOKS.md",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not contain %q. An operator meeting this at 3am needs the "+
				"record, the order and the route forward:\n%v", want, err)
		}
	}
}

// TestPreFixIcebergRecordBehindTheSnapshotStartsTheVenue is deliverable 8, and it is
// the half that keeps the check credible.
//
// A covered record contributes nothing to the recovered book — RestoreAfter drops it
// by sequence whatever it contains — so refusing on it would be refusing on a file
// that could be deleted with no effect. That is the same move
// docs/BOUNDED-RECOVERY.md §5.2 and docs/SEMANTICS-VERSION.md §3.1 already made, and
// it is what makes "a venue that checkpointed starts with no ceremony" true.
func TestPreFixIcebergRecordBehindTheSnapshotStartsTheVenue(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "covered.wal")
	snapPath := filepath.Join(dir, "covered.snap")
	writeIcebergLog(t, walPath, []Entry{
		preFixIceberg(t, 1, "u17", "ACME-8841", 3),
		{Seq: 2, Kind: KindSubmit, Order: wrapperOrder(t, "u2", types.SideBuy, types.OrderTypeLimit, 90, 4)},
	})

	// A snapshot of the book those two records produce, stamped past both of them.
	// The reserve is IN the snapshot, which is why this venue is fine.
	base := matching.NewEngine(tapeCfg())
	ib, err := types.NewIcebergOrder(wrapperOrder(t, "u17", types.SideSell, types.OrderTypeLimit, 100, 9), 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	base.ProcessIceberg(ib)
	base.Process(wrapperOrder(t, "u2", types.SideBuy, types.OrderTypeLimit, 90, 4))
	snap := base.TakeSnapshot()
	snap.WALSeq = 2
	if err := WriteSnapshot(snapPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	eng, rep, err := RecoverWithOptions(tapeCfg(), snapPath, walPath, RecoverOptions{})
	if err != nil {
		t.Fatalf("Recover refused a lossy record the snapshot already covers: %v\n"+
			"A covered record is read, CRC-verified, skipped and never applied, so refusing on it is "+
			"an outage manufactured by a check. docs/ICEBERG-DURABILITY.md §4.2", err)
	}
	if got, want := eng.TakeSnapshot().Digest(), base.TakeSnapshot().Digest(); got != want {
		t.Errorf("the recovered book is not the snapshot's\n got %s\nwant %s", got, want)
	}
	if rep.IcebergsWithoutReserve != 0 {
		t.Errorf("report counts %d lossy records, want 0: the count is over the records that would be "+
			"REPLAYED, never over the file. docs/ICEBERG-DURABILITY.md §4.5", rep.IcebergsWithoutReserve)
	}
	if rep.IcebergReserveLossAccepted {
		t.Error("nothing was accepted, because nothing was refused")
	}
}

// TestAcceptIcebergLossRequiresTheExactCount is deliverable 9.
//
// The override is a COUNT, and it must be exact. N−1 and N+1 both refuse and both say
// the real number, because the property that matters is that the flag goes stale: an
// operator who pasted it into a unit file during one incident finds it refusing the
// next time the log changes, which is when the decision has to be made again.
func TestAcceptIcebergLossRequiresTheExactCount(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "three.wal")
	writeIcebergLog(t, walPath, []Entry{
		preFixIceberg(t, 1, "u17", "ACME-8841", 3),
		preFixIceberg(t, 2, "u04", "BLU-113", 50),
		{Seq: 3, Kind: KindSubmit, Order: wrapperOrder(t, "u2", types.SideBuy, types.OrderTypeLimit, 90, 4)},
	})

	for _, n := range []int{0, 1, 3, 7} {
		_, rep, err := RecoverWithOptions(tapeCfg(), "", walPath, RecoverOptions{AcceptIcebergsWithoutReserve: n})
		if !errors.Is(err, ErrIcebergReserveUnknown) {
			t.Fatalf("-wal-accept-iceberg-loss %d was accepted for a log holding 2 such records; err = %v", n, err)
		}
		if rep.IcebergReserveLossAccepted {
			t.Errorf("-wal-accept-iceberg-loss %d refused, and the report says the loss was accepted", n)
		}
		if n > 0 && !strings.Contains(err.Error(), "would replay 2 such") {
			t.Errorf("-wal-accept-iceberg-loss %d refused without naming the real count:\n%v", n, err)
		}
	}

	eng, rep, err := RecoverWithOptions(tapeCfg(), "", walPath, RecoverOptions{AcceptIcebergsWithoutReserve: 2})
	if err != nil {
		t.Fatalf("the exact count was refused: %v", err)
	}
	if !rep.IcebergReserveLossAccepted || rep.IcebergsWithoutReserve != 2 {
		t.Errorf("report is {IcebergsWithoutReserve:%d IcebergReserveLossAccepted:%v}, want {2 true} — "+
			"this is the field to alert on", rep.IcebergsWithoutReserve, rep.IcebergReserveLossAccepted)
	}
	// The loss is real and the venue was told: both orders came back at their display
	// size with nothing behind them, which is why route 2 of the message says to
	// cancel them and tell their owners to re-enter.
	if n := eng.OrderCount(); n != 3 {
		t.Errorf("the accepted recovery rested %d orders, want 3", n)
	}
	snap := eng.TakeSnapshot()
	if len(snap.Icebergs) != 2 {
		t.Fatalf("the accepted recovery holds %d icebergs, want 2", len(snap.Icebergs))
	}
	for _, e := range snap.Icebergs {
		if e.Hidden != 0 {
			t.Errorf("an accepted lossy record rebuilt with Hidden %d; the reserve was never on the "+
				"disk, so accepting the loss cannot invent it", e.Hidden)
		}
	}
	if _, qty, _ := eng.BestAsk(); qty != 53 {
		t.Errorf("the accepted icebergs show %d at the offer, want 53 — each rebuilt as an ordinary "+
			"order of its DISPLAY size (3 and 50)", qty)
	}

	// And the flag left behind in a unit file must not refuse a log that no longer has
	// the problem. The gate fires if and only if a record would be APPLIED, so a clean
	// log starts with the flag set — it simply stops meaning anything, which is the
	// half of "it goes stale" that keeps a venue running.
	clean := t.TempDir()
	cleanPath := filepath.Join(clean, "clean.wal")
	writeIcebergLog(t, cleanPath, []Entry{postFixIceberg(t, 1, 9, 3)})
	_, rep, err = RecoverWithOptions(tapeCfg(), "", cleanPath, RecoverOptions{AcceptIcebergsWithoutReserve: 2})
	if err != nil {
		t.Fatalf("a stale -wal-accept-iceberg-loss 2 refused a log with no lossy records: %v", err)
	}
	if rep.IcebergsWithoutReserve != 0 || rep.IcebergReserveLossAccepted {
		t.Errorf("report is {IcebergsWithoutReserve:%d IcebergReserveLossAccepted:%v} on a clean log, "+
			"want {0 false} — nothing was refused, so nothing was accepted",
			rep.IcebergsWithoutReserve, rep.IcebergReserveLossAccepted)
	}
}

// TestAcceptIcebergLossRelaxesNothingElse is Rule 11. An operator reaching for one
// permission during an incident must not acquire a second.
func TestAcceptIcebergLossRelaxesNothingElse(t *testing.T) {
	t.Run("ErrCorrupt", func(t *testing.T) {
		dir := t.TempDir()
		walPath := filepath.Join(dir, "corrupt.wal")
		writeIcebergLog(t, walPath, []Entry{
			preFixIceberg(t, 1, "u17", "ACME-8841", 3),
			{Seq: 2, Kind: KindSubmit, Order: wrapperOrder(t, "u2", types.SideBuy, types.OrderTypeLimit, 90, 4)},
		})
		// Flip a byte inside the second record's payload, leaving its framing intact.
		raw := readFile(t, segPath(walPath, 1))
		off := SegHeaderBytesV3
		off += 8 + int(binary.BigEndian.Uint32(raw[off:off+4]))
		raw[off+8] ^= 0xff
		writeFile(t, segPath(walPath, 1), raw)

		if _, _, err := RecoverWithOptions(tapeCfg(), "", walPath,
			RecoverOptions{AcceptIcebergsWithoutReserve: 1}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("err = %v, want ErrCorrupt — accepting an iceberg loss must not accept media "+
				"corruption as well", err)
		}
	})

	t.Run("ErrLogGap", func(t *testing.T) {
		dir := t.TempDir()
		stem := filepath.Join(dir, "gap.wal")
		writeFile(t, stem, segHeader(markerBase))
		writeFile(t, segPath(stem, 1), append(segHeaderV3(1, matching.SemanticsVersion),
			framedRecords(t, []Entry{preFixIceberg(t, 1, "u17", "ACME-8841", 3)}, true)...))
		// Base 5 with nothing between: sequences 2..4 are in no file this venue holds.
		writeFile(t, segPath(stem, 5), append(segHeaderV3(5, matching.SemanticsVersion),
			framedRecords(t, []Entry{{Seq: 5, Kind: KindHalt}}, true)...))

		if _, _, err := RecoverWithOptions(tapeCfg(), "", stem,
			RecoverOptions{AcceptIcebergsWithoutReserve: 1}); !errors.Is(err, ErrLogGap) {
			t.Fatalf("err = %v, want ErrLogGap — accepting an iceberg loss must not accept missing "+
				"commands as well", err)
		}
	})
}

// TestDiagnosticReadersNeverRefuseALossyIceberg is Rule 9.
//
// docs/BOUNDED-RECOVERY.md §9.1 settled where a refusal belongs. cmd/obgw calls
// Recover and then Open on the same path, so a stricter Open turns a benign file into
// an outage by having two readers of the same bytes disagree; ReadAll is the
// diagnostic RUNBOOKS sends an operator to, and a diagnostic that refuses to show you
// the file during an incident is not a diagnostic; and RestoreAfter takes entries
// rather than files — it is what examples/replication/follower.go applies, where
// refusing would stop a follower rather than inform anyone.
func TestDiagnosticReadersNeverRefuseALossyIceberg(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "diag.wal")
	lossy := preFixIceberg(t, 1, "u17", "ACME-8841", 3)
	writeIcebergLog(t, walPath, []Entry{lossy})

	ents, err := ReadAll(walPath)
	if err != nil {
		t.Fatalf("ReadAll refused a lossy record: %v — this is the reader an operator is sent to", err)
	}
	if len(ents) != 1 || ents[0].Kind != KindIceberg {
		t.Fatalf("ReadAll returned %d entries, want the one iceberg record", len(ents))
	}
	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open refused a lossy record: %v — Recover and Open read the same bytes and must "+
			"not disagree about them", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eng := matching.NewEngine(tapeCfg())
	RestoreAfter(eng, ents, 0)
	if eng.OrderCount() != 1 {
		t.Errorf("RestoreAfter applied %d orders, want 1 — it takes entries, not files, and a "+
			"follower that refused would stop rather than inform anyone", eng.OrderCount())
	}
}

// TestARecordThatContradictsItselfIsTreatedAsStatingNothing is Rule 5.
//
// This package cannot write one, so a record whose TotalQty and Order.Quantity
// disagree was hand-edited, forged, or written by something else claiming to be this
// format. A reader that picked one of the two numbers would be guessing which of two
// contradictory statements about the same quantity to believe, so it is routed into
// the gate instead — the fail-safe direction.
func TestARecordThatContradictsItselfIsTreatedAsStatingNothing(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "forged.wal")
	forged := postFixIceberg(t, 1, 9, 3)
	forged.TotalQty = 7 // says nine lots in the order and seven in the witness
	writeIcebergLog(t, walPath, []Entry{forged})

	if _, _, err := RecoverWithOptions(tapeCfg(), "", walPath, RecoverOptions{}); !errors.Is(err, ErrIcebergReserveUnknown) {
		t.Fatalf("Recover err = %v, want ErrIcebergReserveUnknown — a record that states two different "+
			"totals states neither. docs/ICEBERG-DURABILITY.md §3.2 Rule 5", err)
	}
	// And under the override it reconstructs by Rule 4's expression, unchanged: no
	// arithmetic is invented for a forged record.
	eng, _, err := RecoverWithOptions(tapeCfg(), "", walPath, RecoverOptions{AcceptIcebergsWithoutReserve: 1})
	if err != nil {
		t.Fatalf("the exact count was refused: %v", err)
	}
	if snap := eng.TakeSnapshot(); len(snap.Icebergs) != 1 || snap.Icebergs[0].Hidden != 4 {
		t.Errorf("the accepted record rebuilt %+v, want one iceberg with Hidden 4 (TotalQty 7 less the "+
			"display of 3) — the reconstruction is Rule 4's expression whatever the gate decided",
			snap.Icebergs)
	}
}

// TestEveryFramingRecoversAnIcebergAndRefusesALossyOne is deliverable 10 and Rule 7.
//
// The gate is on the decoded RECORD, so it is framing-independent by construction —
// but "by construction" is an argument, not a test. All four shapes this build reads
// carry the same JSON payload, and all four must behave identically: recover a
// post-fix record to the digest the live venue had, and refuse a pre-fix one.
//
// A header-level check would have had to be invented three times and would still have
// missed the single-file case.
func TestEveryFramingRecoversAnIcebergAndRefusesALossyOne(t *testing.T) {
	// The oracle: what the venue that wrote the record actually held.
	live := matching.NewEngine(tapeCfg())
	ib, err := types.NewIcebergOrder(wrapperOrder(t, "u1", types.SideSell, types.OrderTypeLimit, 100, 9), 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	live.ProcessIceberg(ib)
	wantDigest := live.TakeSnapshot().Digest()

	for _, f := range framings() {
		t.Run(f.name, func(t *testing.T) {
			good := t.TempDir()
			goodPath, opts := f.build(t, good, []Entry{postFixIceberg(t, 1, 9, 3)})
			eng, rep, err := RecoverWithOptions(tapeCfg(), "", goodPath, opts)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got := eng.TakeSnapshot().Digest(); got != wantDigest {
				t.Errorf("a post-fix iceberg record in %s recovers to a different book\n got %s\nwant %s",
					f.name, got, wantDigest)
			}
			if rep.IcebergsWithoutReserve != 0 {
				t.Errorf("a post-fix record counted as lossy in %s", f.name)
			}

			bad := t.TempDir()
			badPath, opts := f.build(t, bad, []Entry{preFixIceberg(t, 1, "u17", "ACME-8841", 3)})
			_, rep, err = RecoverWithOptions(tapeCfg(), "", badPath, opts)
			if !errors.Is(err, ErrIcebergReserveUnknown) {
				t.Fatalf("a pre-fix iceberg record in %s was not refused: err = %v", f.name, err)
			}
			if rep.IcebergsWithoutReserve != 1 {
				t.Errorf("%s reports %d lossy records, want 1 — the count must not depend on the "+
					"framing, or the same commands report different numbers depending on which format "+
					"they were written in. docs/ICEBERG-DURABILITY.md §4.5", f.name, rep.IcebergsWithoutReserve)
			}
			opts.AcceptIcebergsWithoutReserve = 1
			if _, _, err := RecoverWithOptions(tapeCfg(), "", badPath, opts); err != nil {
				t.Errorf("%s refused the exact count: %v", f.name, err)
			}
		})
	}
}

// framing is one of the four shapes this build reads.
type framing struct {
	name string
	// build writes entries into a log of this shape and returns its path, plus the
	// options a recovery needs for reasons that have nothing to do with icebergs —
	// every shape but the newest declares no matching semantics, and the semantics
	// gate would refuse them first. Naming 0 there says what each case is NOT about
	// and leaves the iceberg assertions to speak for themselves.
	build func(t *testing.T, dir string, entries []Entry) (string, RecoverOptions)
}

func framings() []framing {
	preStamp := RecoverOptions{AcceptSemantics: []int{0}}
	return []framing{
		{
			name: "v1 headerless",
			build: func(t *testing.T, dir string, entries []Entry) (string, RecoverOptions) {
				p := filepath.Join(dir, "v1.wal")
				writeFile(t, p, framedRecords(t, entries, false))
				return p, preStamp
			},
		},
		{
			name: "OBWAL-1 single file",
			build: func(t *testing.T, dir string, entries []Entry) (string, RecoverOptions) {
				p := filepath.Join(dir, "v2.wal")
				writeFile(t, p, append([]byte(Magic), framedRecords(t, entries, true)...))
				return p, preStamp
			},
		},
		{
			name: "OBWAL-2 segment set",
			build: func(t *testing.T, dir string, entries []Entry) (string, RecoverOptions) {
				stem := filepath.Join(dir, "v2seg.wal")
				writeFile(t, stem, segHeader(markerBase))
				writeFile(t, segPath(stem, 1), append(segHeader(1), framedRecords(t, entries, true)...))
				return stem, preStamp
			},
		},
		{
			name: "OBWAL-3 segment set",
			build: func(t *testing.T, dir string, entries []Entry) (string, RecoverOptions) {
				stem := filepath.Join(dir, "v3seg.wal")
				writeFile(t, stem, segHeader(markerBase))
				writeFile(t, segPath(stem, 1), append(segHeaderV3(1, matching.SemanticsVersion),
					framedRecords(t, entries, true)...))
				return stem, RecoverOptions{}
			},
		},
	}
}

// writeIcebergLog writes entries as the set this build creates: a marker and one
// OBWAL\x03 segment based at 1. It is the shape every other test in this file uses,
// so the gate is exercised on the format a venue actually has on disk.
func writeIcebergLog(t *testing.T, stem string, entries []Entry) {
	t.Helper()
	writeFile(t, stem, segHeader(markerBase))
	writeFile(t, segPath(stem, 1), append(segHeaderV3(1, matching.SemanticsVersion),
		framedRecords(t, entries, true)...))
}

// framedRecords frames entries as records: [len][crc][payload], or [len][payload]
// for the headerless v1 shape that predates checksums.
func framedRecords(t *testing.T, entries []Entry, checksummed bool) []byte {
	t.Helper()
	var raw []byte
	for _, e := range entries {
		payload, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		raw = binary.BigEndian.AppendUint32(raw, uint32(len(payload)))
		if checksummed {
			raw = binary.BigEndian.AppendUint32(raw, crc32.Checksum(payload, crcTable))
		}
		raw = append(raw, payload...)
	}
	return raw
}

// TestAPostFixIcebergRecordCostsFourteenBytesAndNothingElse is docs/ICEBERG-DURABILITY.md
// §3.3, checked by comparing written bytes rather than by inspection.
//
// The claim is not "TotalQty is small". It is that adding it left every record of
// every OTHER kind byte-identical, so no golden moves and no archived segment is
// reinterpreted — the same argument Entry.Phase made, checked the same way.
func TestAPostFixIcebergRecordCostsFourteenBytesAndNothingElse(t *testing.T) {
	withTotal := len(framedRecords(t, []Entry{postFixIceberg(t, 41, 9, 3)}, true))
	without := postFixIceberg(t, 41, 9, 3)
	without.TotalQty = 0
	without.Order.Quantity = 3
	without.Order.RemainingQty = 3
	if got := withTotal - len(framedRecords(t, []Entry{without}, true)); got != 14 {
		t.Errorf("a KindIceberg record grew by %d bytes, want 14 (measured 311 -> 325). If this has "+
			"moved, docs/ICEBERG-DURABILITY.md §3.3's cost is stale", got)
	}

	// Every other kind: zero bytes, checked over the whole sample set.
	for k, e := range entryKindSamples(t) {
		if k == KindIceberg {
			continue
		}
		e.Seq = 41
		raw := framedRecords(t, []Entry{e}, true)
		if strings.Contains(string(raw), "total_qty") {
			t.Errorf("EntryKind %d emits total_qty. omitempty is what keeps every pre-existing record "+
				"byte-identical: %s", k, raw)
		}
	}
	// And the field is absent from a zero-valued Entry, which is what omitempty buys.
	if b, err := json.Marshal(Entry{Seq: 1, Kind: KindSubmit}); err != nil {
		t.Fatalf("Marshal: %v", err)
	} else if strings.Contains(string(b), "total_qty") {
		t.Errorf("a record with no total emits the key anyway: %s", b)
	}
}

// TestAHandBuiltIcebergRecordStillMeansTheTotal is deliverable 3, and it is what
// makes Rule 4's fallback more than decoration.
//
// entryKindSamples writes {Order: qty 10, DisplayQty: 2} with no TotalQty at all —
// written that way because the total in Quantity is what any reader assumes the field
// means, which is the assumption the writer failed to keep for four releases. Under
// Rule 4 it still restores to a reserve of 8, and NO EXISTING ASSERTION IS WEAKENED
// to achieve it: the fallback is today's expression, unchanged.
//
// It is asserted here rather than inside TestEveryEntryKindReplays because that test
// asks a different question — whether restoreEntry RECOGNISED the kind — and a record
// that is recognised and then dropped for failing NewIcebergOrder's validation passes
// it. Measured: with the fallback removed, that test stays green and this one does
// not.
func TestAHandBuiltIcebergRecordStillMeansTheTotal(t *testing.T) {
	e := entryKindSamples(t)[KindIceberg]
	if e.TotalQty != 0 {
		t.Fatalf("the sample now carries TotalQty %d, so it no longer exercises the fallback. Keep a "+
			"record with the total in Quantity alone somewhere in this package, or a pre-fix log and "+
			"every hand-built record stop being tested", e.TotalQty)
	}
	if e.Order.Quantity != 10 || e.DisplayQty != 2 {
		t.Fatalf("the sample is qty=%d display=%d, want 10 and 2 — this test is pinned to it",
			e.Order.Quantity, e.DisplayQty)
	}

	eng := matching.NewEngine(tapeCfg())
	e.Seq = 1
	RestoreAfter(eng, []Entry{e}, 0)
	snap := eng.TakeSnapshot()
	if len(snap.Icebergs) != 1 || snap.Icebergs[0].Hidden != 8 || snap.Icebergs[0].DisplayQty != 2 {
		t.Fatalf("a hand-built iceberg record restored to %+v, want one iceberg with Hidden 8 and "+
			"DisplayQty 2. Without Rule 4's fallback the total reads as zero, NewIcebergOrder refuses "+
			"the display size, and the record is dropped on the floor — silently, because restoreEntry "+
			"still recognised the kind. docs/ICEBERG-DURABILITY.md §3.2 Rule 4", snap.Icebergs)
	}
}

// TestACoveredLossyIcebergIsNotCountedEvenWhenTheWalkFallsBack is the assertion
// docs/ICEBERG-DURABILITY.md §10 row 6 asked for when the sabotage it predicted
// caught nothing.
//
// Moving the gate from the replay set to the whole walk is invisible on an ordinary
// recovery, because walkSegment RETAINS only records past the boundary — the two
// filters agree by accident. They part company in exactly one place: when a record's
// sequence disagrees with the base its segment declares, recovery re-reads the log
// from byte zero (RecoverReport.FellBack) and walk.entries then holds the covered
// prefix as well. In that path the sequence filter is the only thing standing between
// a covered lossy record and a refusal on a file that could be deleted with no
// effect.
//
// Measured before this test existed: sabotage row 6 was green across the whole suite.
func TestACoveredLossyIcebergIsNotCountedEvenWhenTheWalkFallsBack(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "fallback.wal")
	snapPath := filepath.Join(dir, "fallback.snap")

	// One segment declaring base 1, whose third record carries a sequence the header
	// does not imply — the shape a segment restored from another venue produces.
	writeFile(t, stem, segHeader(markerBase))
	writeFile(t, segPath(stem, 1), append(segHeaderV3(1, matching.SemanticsVersion),
		framedRecords(t, []Entry{
			preFixIceberg(t, 1, "u17", "ACME-8841", 3),
			{Seq: 2, Kind: KindSubmit, Order: wrapperOrder(t, "u2", types.SideBuy, types.OrderTypeLimit, 90, 4)},
			{Seq: 4, Kind: KindSubmit, Order: wrapperOrder(t, "u3", types.SideBuy, types.OrderTypeLimit, 91, 5)},
		}, true)...))

	// A snapshot covering the first two records, so the lossy one is behind it.
	base := matching.NewEngine(tapeCfg())
	ib, err := types.NewIcebergOrder(wrapperOrder(t, "u17", types.SideSell, types.OrderTypeLimit, 100, 9), 3)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	base.ProcessIceberg(ib)
	base.Process(wrapperOrder(t, "u2", types.SideBuy, types.OrderTypeLimit, 90, 4))
	snap := base.TakeSnapshot()
	snap.WALSeq = 2
	if err := WriteSnapshot(snapPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	eng, rep, err := RecoverWithOptions(tapeCfg(), snapPath, stem, RecoverOptions{})
	if err != nil {
		t.Fatalf("Recover refused on a covered lossy record after falling back: %v\n"+
			"The gate is over the records that would be REPLAYED. docs/ICEBERG-DURABILITY.md §4.5", err)
	}
	if !rep.FellBack {
		t.Fatal("the fixture did not make recovery fall back, so this test is not exercising the path " +
			"where walk.entries holds the covered prefix — the assertion below would pass for the " +
			"wrong reason")
	}
	if rep.IcebergsWithoutReserve != 0 {
		t.Errorf("report counts %d lossy records after a fallback, want 0 — the covered prefix is in "+
			"walk.entries on this path, and only the sequence filter keeps it out of the gate",
			rep.IcebergsWithoutReserve)
	}
	if eng.OrderCount() != 3 {
		t.Errorf("recovered %d resting orders, want 3 (the snapshot's two plus the one record past it)",
			eng.OrderCount())
	}
}
