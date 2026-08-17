package wal

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The replay half of the durability guard.
//
// docs/JOURNAL-COMPLETENESS.md §4.4 specifies a guard over the command alphabet,
// and pkg/matching/command_alphabet_test.go builds it: every cmdKind is classified,
// and every journalled kind must reach a CommandLog method. That covers the first
// link of the chain and the spec stops there. The chain is longer:
//
//	cmdKind -> CommandLog method -> wal.EntryKind -> restoreEntry arm
//
// JOURNALLED IS NOT THE SAME AS REPLAYED. A kind can have a cmdKind, a table entry,
// a CommandLog method, a logCommand case, an EntryKind and a Writer method, and
// still have no arm in restoreEntry — in which case the venue writes a durable
// record that recovery silently discards, and the command-side guard reports the
// alphabet complete while a restart loses the command anyway. That is the same
// failure as the one this whole document is about, moved one link along.
//
// Every EntryKind declared today does have an arm; this is a hole for the NEXT
// kind, which is precisely when it is cheap to close.

// entryKindSamples is a representative record of every EntryKind. Like the
// classification table in pkg/matching it must name them all, and the sentinel is
// what makes "all" enumerable instead of a list someone has to remember to extend.
func entryKindSamples(t *testing.T) map[EntryKind]Entry {
	t.Helper()
	order := func(side types.Side, price, qty int64) *types.Order {
		o, err := types.NewOrder("u", "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		return o
	}
	return map[EntryKind]Entry{
		KindSubmit:     {Kind: KindSubmit, Order: order(types.SideBuy, 100, 5)},
		KindCancel:     {Kind: KindCancel, CancelID: 1, UserID: "u"},
		KindReduce:     {Kind: KindReduce, CancelID: 1, NewQty: 2, UserID: "u"},
		KindCancelAll:  {Kind: KindCancelAll, UserID: "u"},
		KindStop:       {Kind: KindStop, Order: order(types.SideBuy, 100, 5), StopPrice: 105},
		KindOCO:        {Kind: KindOCO, Order: order(types.SideSell, 110, 5), StopOrder: order(types.SideSell, 90, 5), StopPrice: 90},
		KindIceberg:    {Kind: KindIceberg, Order: order(types.SideBuy, 100, 10), DisplayQty: 2},
		KindPegged:     {Kind: KindPegged, Order: order(types.SideBuy, 100, 5), PegRef: string(types.PegToBid)},
		KindTrailing:   {Kind: KindTrailing, Order: order(types.SideSell, 100, 5), Trail: 5},
		KindReplace:    {Kind: KindReplace, CancelID: 1, UserID: "u", Order: order(types.SideBuy, 99, 4)},
		KindHalt:       {Kind: KindHalt},
		KindResume:     {Kind: KindResume},
		KindCancelOnly: {Kind: KindCancelOnly},
		KindSetMark:    {Kind: KindSetMark, MarkPrice: 100},
		KindBust:       {Kind: KindBust, TradeID: 1, BustReason: "erroneous order entry"},
		KindSetPhase:   {Kind: KindSetPhase, Phase: matching.StatePreOpen.String()},
	}
}

// TestEveryEntryKindReplays enumerates the EntryKind block and requires each kind
// to be recognised by restoreEntry.
//
// Adding a kind the writer can produce and the reader ignores becomes a failure at
// the moment the constant is written, rather than a divergence discovered after a
// restart — which is what the three escapes this document is about all cost.
func TestEveryEntryKindReplays(t *testing.T) {
	samples := entryKindSamples(t)

	// KindSubmit is 1, not 0: the block is `iota + 1` so that a zero-valued Entry
	// is not a valid Submit.
	for k := KindSubmit; k < entryKindCount; k++ {
		e, ok := samples[k]
		if !ok {
			t.Errorf("EntryKind %d has no sample in entryKindSamples: add one, and make sure the "+
				"kind has an arm in restoreEntry. See docs/JOURNAL-COMPLETENESS.md §8", k)
			continue
		}
		if e.Kind != k {
			t.Errorf("the sample filed under EntryKind %d carries Kind %d", k, e.Kind)
			continue
		}

		eng := matching.NewEngine(tapeCfg())
		if !restoreEntry(eng, e) {
			t.Errorf("EntryKind %d has no arm in restoreEntry, so a record of it is written durably "+
				"and silently discarded on recovery — the command survives the crash in the log and "+
				"not in the engine", k)
		}
	}

	for k := range samples {
		if k < KindSubmit || k >= entryKindCount {
			t.Errorf("entryKindSamples holds EntryKind %d, which is outside the declared block — a stale entry", k)
		}
	}
}

// TestUnknownEntryKindIsSkippedNotApplied pins the other side of restoreEntry's
// boolean, which is a real behaviour and not just a test hook.
//
// A record written by a NEWER build reaching an older reader must be skipped, so
// the follower diverges detectably (EngineSnapshot.State is in the digest) rather
// than refusing to start or guessing at the payload. The test above makes sure that
// skip never quietly covers for a kind this build writes itself.
func TestUnknownEntryKindIsSkippedNotApplied(t *testing.T) {
	eng := matching.NewEngine(tapeCfg())
	before := eng.TakeSnapshot().Digest()

	future := Entry{Seq: 1, Kind: entryKindCount + 40, Phase: "AUCTION_FREEZE"}
	if restoreEntry(eng, future) {
		t.Fatal("restoreEntry claims to have handled a kind it has never heard of")
	}
	if got := eng.TakeSnapshot().Digest(); got != before {
		t.Errorf("an unrecognised record changed the engine\n got %s\nwant %s", got, before)
	}

	// And through the public entry point, which is how a real older reader meets it.
	RestoreAfter(eng, []Entry{future}, 0)
	if got := eng.TakeSnapshot().Digest(); got != before {
		t.Errorf("RestoreAfter applied an unrecognised record\n got %s\nwant %s", got, before)
	}
}

// TestSetPhaseRecordRoundTrips is deliverable 1: the record survives the file.
//
// It also pins the two format promises the phase field makes — that the phase is
// stored as a NAME, and that adding the field left every pre-existing record
// byte-identical (omitempty), so no golden file moves and an old log replays
// exactly as before.
func TestSetPhaseRecordRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/wal.log"

	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	phases := []matching.EngineState{
		matching.StatePreOpen, matching.StateOpen, matching.StateClosingAuction,
		matching.StateClosed, matching.StateHalted, matching.StateCancelOnly,
	}
	for _, p := range phases {
		if _, err := w.AppendSetPhase(p); err != nil {
			t.Fatalf("AppendSetPhase(%s): %v", p, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entries, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != len(phases) {
		t.Fatalf("read %d records, wrote %d", len(entries), len(phases))
	}
	for i, e := range entries {
		if e.Kind != KindSetPhase {
			t.Errorf("record %d is kind %d, want KindSetPhase", i, e.Kind)
		}
		if e.Phase != phases[i].String() {
			t.Errorf("record %d carries phase %q, want %q — the log stores the NAME, so that "+
				"reordering the EngineState block cannot reinterpret an archived segment",
				i, e.Phase, phases[i].String())
		}
		got, err := matching.ParseEngineState(e.Phase)
		if err != nil {
			t.Errorf("record %d: ParseEngineState(%q): %v", i, e.Phase, err)
			continue
		}
		if got != phases[i] {
			t.Errorf("record %d round-trips to %s, want %s", i, got, phases[i])
		}
	}
}
