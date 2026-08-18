package wal

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The wrapper-record guards: docs/ICEBERG-DURABILITY.md §6.
//
// The iceberg's lost reserve was found by a reviewer reading two files, and the
// question that actually mattered — IS THE ICEBERG THE ONLY ONE? — took a probe
// built outside the repository to answer. Nothing in the suite could answer it,
// because nothing in the suite ever recovered a wrapper order FROM THE LOG ALONE and
// compared the result with the engine that wrote it.
//
// So the audit is promoted from a probe to three standing guards, each catching the
// same defect at a different depth:
//
//	Rule 13  TestEveryWrapperRecordRebuildsItsOrder    the symptom, per EntryKind
//	Rule 14  TestNoOrderWrapperConstructorMutatesItsOrder (pkg/types)  the cause
//	Rule 15  TestEveryWrapperFieldIsAccounted          the field nobody logged
//
// Rule 13 makes no assumption about WHY a record might be insufficient. It catches
// constructor mutation, state derived at construction, an unlogged scalar, and
// whatever the sixth wrapper does that none of the five do.

// wrapperRow is one round trip: a tape that logs and applies a wrapper order, and a
// probe run afterwards on both the live engine and the one rebuilt from the log.
//
// The probe is the half that matters. A resting wrapper compared by digest proves
// only that the record round-tripped its own fields; a probe that TRADES against it
// is what notices a reserve that is not there, because the reserve is invisible in
// the book by construction.
type wrapperRow struct {
	kind EntryKind
	name string
	// tape logs and applies the commands, in order, through both w and eng. Every
	// command must be appended to w before it is applied to eng, exactly as
	// matching.Runner does it: the point of the row is that the log is sufficient.
	tape func(t *testing.T, w *Writer, eng *matching.Engine)
}

// plainKinds are the EntryKinds that carry no wrapper — a command whose whole
// content is scalars and, at most, one ordinary order. Each needs a citation rather
// than a row, and the citation is what stops "plain" being the default answer for a
// kind nobody thought about.
var plainKinds = map[EntryKind]string{
	KindSubmit:     "an ordinary order; Entry.Order is the command as the client sent it",
	KindCancel:     "an order id and a user; no order is rebuilt",
	KindReduce:     "an order id, a user and the new TOTAL quantity",
	KindCancelAll:  "a user id",
	KindReplace:    "an order id, a user and an ordinary replacement order",
	KindHalt:       "no payload",
	KindResume:     "no payload",
	KindCancelOnly: "no payload",
	KindSetMark:    "a price in ticks",
	KindBust:       "a trade id and an operator's reason",
	KindSetPhase:   "a phase NAME (docs/JOURNAL-COMPLETENESS.md §4.1)",
}

// wrapperRows is the enumeration §1 of docs/ICEBERG-DURABILITY.md ran by hand.
//
// Two of them deliberately let the wrapper's state MOVE before the log is closed —
// the stop fires mid-tape and the trailing stop ratchets — because a resting wrapper
// proves much less: `triggered` and the ratchet are in no record at all, and they
// come back only because the log carries the trades that produced them. That is
// exactly the property a hidden reserve does not have.
func wrapperRows() []wrapperRow {
	return []wrapperRow{
		{
			kind: KindStop,
			name: "stop, fired mid-tape",
			tape: func(t *testing.T, w *Writer, eng *matching.Engine) {
				logSubmit(t, w, eng, wrapperOrder(t, "u1", types.SideBuy, types.OrderTypeLimit, 95, 10))
				stop, err := types.NewStopOrder(
					wrapperOrder(t, "u2", types.SideSell, types.OrderTypeMarket, 0, 4), 95)
				if err != nil {
					t.Fatalf("NewStopOrder: %v", err)
				}
				if _, err := w.AppendStop(stop); err != nil {
					t.Fatalf("AppendStop: %v", err)
				}
				eng.ProcessStop(stop)
				// Prints at 95, which fires the stop inside the same command.
				logSubmit(t, w, eng, wrapperOrder(t, "u3", types.SideSell, types.OrderTypeLimit, 95, 1))
				if !stop.IsTriggered() {
					t.Fatal("setup: the stop did not fire, so this row is testing a resting stop")
				}
			},
		},
		{
			kind: KindStop,
			name: "stop-limit, resting",
			tape: func(t *testing.T, w *Writer, eng *matching.Engine) {
				logSubmit(t, w, eng, wrapperOrder(t, "u1", types.SideBuy, types.OrderTypeLimit, 100, 5))
				stop, err := types.NewStopOrder(
					wrapperOrder(t, "u2", types.SideSell, types.OrderTypeLimit, 90, 6), 92)
				if err != nil {
					t.Fatalf("NewStopOrder: %v", err)
				}
				if _, err := w.AppendStop(stop); err != nil {
					t.Fatalf("AppendStop: %v", err)
				}
				eng.ProcessStop(stop)
			},
		},
		{
			kind: KindOCO,
			name: "OCO, both legs resting",
			tape: func(t *testing.T, w *Writer, eng *matching.Engine) {
				logSubmit(t, w, eng, wrapperOrder(t, "u1", types.SideBuy, types.OrderTypeLimit, 100, 5))
				stop, err := types.NewStopOrder(
					wrapperOrder(t, "u2", types.SideSell, types.OrderTypeMarket, 0, 5), 90)
				if err != nil {
					t.Fatalf("NewStopOrder: %v", err)
				}
				oco, err := types.NewOCOOrder(
					wrapperOrder(t, "u2", types.SideSell, types.OrderTypeLimit, 110, 5), stop)
				if err != nil {
					t.Fatalf("NewOCOOrder: %v", err)
				}
				if _, err := w.AppendOCO(oco); err != nil {
					t.Fatalf("AppendOCO: %v", err)
				}
				eng.ProcessOCO(oco)
			},
		},
		{
			kind: KindIceberg,
			name: "iceberg, nine lots shown three",
			tape: func(t *testing.T, w *Writer, eng *matching.Engine) {
				ib, err := types.NewIcebergOrder(
					wrapperOrder(t, "u1", types.SideSell, types.OrderTypeLimit, 100, 9), 3)
				if err != nil {
					t.Fatalf("NewIcebergOrder: %v", err)
				}
				if _, err := w.AppendIceberg(ib); err != nil {
					t.Fatalf("AppendIceberg: %v", err)
				}
				eng.ProcessIceberg(ib)
			},
		},
		{
			kind: KindPegged,
			name: "pegged to the bid, offset +2",
			tape: func(t *testing.T, w *Writer, eng *matching.Engine) {
				logSubmit(t, w, eng, wrapperOrder(t, "u1", types.SideBuy, types.OrderTypeLimit, 100, 5))
				p, err := types.NewPeggedOrder(
					wrapperOrder(t, "u2", types.SideSell, types.OrderTypeLimit, 1, 4), types.PegToBid, 2)
				if err != nil {
					t.Fatalf("NewPeggedOrder: %v", err)
				}
				if _, err := w.AppendPegged(p); err != nil {
					t.Fatalf("AppendPegged: %v", err)
				}
				eng.ProcessPegged(p)
			},
		},
		{
			kind: KindTrailing,
			name: "trailing stop, ratcheted before the log closed",
			tape: func(t *testing.T, w *Writer, eng *matching.Engine) {
				logSubmit(t, w, eng, wrapperOrder(t, "u1", types.SideBuy, types.OrderTypeLimit, 100, 10))
				ts, err := types.NewTrailingStop(
					wrapperOrder(t, "u2", types.SideSell, types.OrderTypeMarket, 0, 4), 5)
				if err != nil {
					t.Fatalf("NewTrailingStop: %v", err)
				}
				if _, err := w.AppendTrailing(ts); err != nil {
					t.Fatalf("AppendTrailing: %v", err)
				}
				eng.ProcessTrailingStop(ts)
				// Three prints, each one ratcheting the stop.
				for i := 0; i < 3; i++ {
					logSubmit(t, w, eng, wrapperOrder(t, "u3", types.SideSell, types.OrderTypeLimit, 100, 1))
				}
				if st := ts.State(); !st.Initialized {
					t.Fatal("setup: the trailing stop never observed a price, so its ratchet has not moved")
				}
			},
		},
	}
}

// TestEveryWrapperRecordRebuildsItsOrder is Rule 13.
//
// For every EntryKind that carries a wrapper: log the command, apply it live, close
// the log, recover FROM THE LOG ALONE, then run the same probe against both engines
// and require the trades and the snapshot digests to agree.
//
// A new EntryKind with no classification fails at the moment its constant is
// written, which is the device entryKindCount was introduced for.
func TestEveryWrapperRecordRebuildsItsOrder(t *testing.T) {
	covered := map[EntryKind]bool{}
	for _, row := range wrapperRows() {
		covered[row.kind] = true
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			walPath := filepath.Join(dir, "wrapper.wal")

			w, err := Open(walPath)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			live := matching.NewEngine(tapeCfg())
			row.tape(t, w, live)
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			recovered, err := Recover(tapeCfg(), "", walPath)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}

			liveTrades := sweepProbe(t, live)
			recTrades := sweepProbe(t, recovered)
			if !reflect.DeepEqual(liveTrades, recTrades) {
				t.Errorf("a %s recovered FROM THE LOG ALONE prints a different tape than the venue "+
					"that wrote it.\n live: %v\n  log: %v\nThe record for EntryKind %d does not carry "+
					"enough to rebuild the order the engine was given. docs/ICEBERG-DURABILITY.md §6.",
					row.name, liveTrades, recTrades, row.kind)
			}
			wantDigest := live.TakeSnapshot().Digest()
			if got := recovered.TakeSnapshot().Digest(); got != wantDigest {
				t.Errorf("a %s recovered FROM THE LOG ALONE holds a different book.\n got %s\nwant %s\n"+
					"docs/ICEBERG-DURABILITY.md §6.", row.name, got, wantDigest)
			}
		})
	}

	for k := KindSubmit; k < entryKindCount; k++ {
		why, plain := plainKinds[k]
		switch {
		case plain && covered[k]:
			t.Errorf("EntryKind %d is classified plain (%q) AND has a wrapper row. One of the two is "+
				"stale; a kind is one or the other", k, why)
		case !plain && !covered[k]:
			t.Errorf("EntryKind %d is neither classified plain nor given a wrapper round-trip row. "+
				"If it carries a wrapper order, add a row to wrapperRows and prove the record rebuilds "+
				"it from the log alone; if it does not, add it to plainKinds with a one-line citation. "+
				"docs/ICEBERG-DURABILITY.md §6, Rule 13", k)
		}
	}
	for k := range plainKinds {
		if k < KindSubmit || k >= entryKindCount {
			t.Errorf("plainKinds holds EntryKind %d, outside the declared block — a stale entry", k)
		}
	}
}

// TestEveryWrapperFieldIsAccounted is Rule 15: the field-level form of the audit.
//
// Every exported field of every wrapper type is logged, re-derived at replay, or
// carried only by a snapshot — and which one it is has to be written down. A new
// field fails until somebody says. Mechanical where the compiler can enumerate (the
// fields) and human where it cannot (the classification), which is the same honest
// split docs/SEMANTICS-VERSION.md §5.5 makes about its alphabet guard.
func TestEveryWrapperFieldIsAccounted(t *testing.T) {
	// value: "logged: <Entry field>", "derived: <how>", or "snapshotOnly: <why>".
	accounted := map[string]map[string]string{
		"StopOrder": {
			"Order":     "logged: Entry.Order",
			"StopPrice": "logged: Entry.StopPrice",
		},
		"OCOOrder": {
			"Primary": "logged: Entry.Order",
			"Stop":    "logged: Entry.StopOrder and Entry.StopPrice",
		},
		"IcebergOrder": {
			"Order":      "logged: Entry.Order, at the CLIENT'S total (docs/ICEBERG-DURABILITY.md §3)",
			"DisplayQty": "logged: Entry.DisplayQty",
			"Hidden":     "logged: Entry.TotalQty minus Entry.DisplayQty — the field this document exists for",
			"JitterBps":  "derived: Config.IcebergPeakJitter, set by ProcessIceberg on every entry, live and replayed alike",
		},
		"PeggedOrder": {
			"Order":  "logged: Entry.Order",
			"Ref":    "logged: Entry.PegRef",
			"Offset": "logged: Entry.PegOffset",
		},
		"TrailingStop": {
			"Order": "logged: Entry.Order",
			"Trail": "logged: Entry.Trail",
		},
	}
	wrappers := []any{
		types.StopOrder{}, types.OCOOrder{}, types.IcebergOrder{},
		types.PeggedOrder{}, types.TrailingStop{},
	}
	for _, w := range wrappers {
		rt := reflect.TypeOf(w)
		want := accounted[rt.Name()]
		if want == nil {
			t.Errorf("wrapper type %s has no field classification at all", rt.Name())
			continue
		}
		seen := map[string]bool{}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			seen[f.Name] = true
			if _, ok := want[f.Name]; !ok {
				t.Errorf("%s.%s is not classified. Say which it is — logged (name the Entry field), "+
					"derived at replay (say from what), or snapshot-only (say why a log cannot carry it) "+
					"— and if it is LOGGED, prove it with a wrapper row in wrapperRows. "+
					"docs/ICEBERG-DURABILITY.md §6, Rule 15", rt.Name(), f.Name)
			}
		}
		for name := range want {
			if !seen[name] {
				t.Errorf("%s.%s is classified but no longer exists — a stale classification", rt.Name(), name)
			}
		}
	}
}

// sweepProbe trades against whatever the engine holds, from both sides, and returns
// the prints. Hitting the book is the only way to see a reserve that is not there:
// an iceberg with no hidden quantity looks identical to a full one until somebody
// tries to buy it.
//
// The BUY goes first and the two sides are different users. Both matter: a sell that
// rested at 1 would be the buy's first counterparty and the sweep would never reach
// the book it came to measure, and one user on both sides would be cancelled by
// self-trade prevention instead of printing.
func sweepProbe(t *testing.T, eng *matching.Engine) []string {
	t.Helper()
	var out []string
	for _, o := range []*types.Order{
		wrapperOrder(t, "probeB", types.SideBuy, types.OrderTypeLimit, 1000, 100),
		wrapperOrder(t, "probeS", types.SideSell, types.OrderTypeLimit, 1, 100),
	} {
		for _, tr := range eng.Process(o).Trades {
			out = append(out, fmt.Sprintf("%s %d@%d maker=%d taker=%d",
				tr.TakerSide, tr.Quantity, tr.Price, tr.MakerOrderID, tr.TakerOrderID))
		}
	}
	return out
}

func wrapperOrder(t *testing.T, user string, side types.Side, typ types.OrderType, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, typ, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// logSubmit appends before it applies — write-ahead, exactly as matching.Runner does
// it, because a row that applied first would be testing a log it did not depend on.
func logSubmit(t *testing.T, w *Writer, eng *matching.Engine, o *types.Order) {
	t.Helper()
	if _, err := w.AppendSubmit(o); err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	eng.Process(o)
}
