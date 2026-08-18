package types

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestNoOrderWrapperConstructorMutatesItsOrder is Rule 14 of
// docs/ICEBERG-DURABILITY.md §6: the CAUSE, caught on the day it is introduced.
//
// A constructor that mutates the order it is handed is a constructor whose effect has
// already happened by the time anything can be journalled. Every caller in this
// repository builds the wrapper and THEN hands it to matching.Runner, which logs it
// in logCommand before applying it — so write-ahead ordering cannot help: the damage
// precedes the write-ahead. That is exactly how NewIcebergOrder cost a venue its
// clients' hidden reserves for four releases, with a correct-looking writer, a
// correct-looking reader, and the untrue sentence between them.
//
// The audit that found it checked all six constructors this way and found one. This
// is that audit, standing.
//
// It is a TRIPWIRE, not a proof. A wrapper can round-trip badly with no mutation at
// all — by deriving state at construction that no record carries — which is why
// pkg/wal's TestEveryWrapperRecordRebuildsItsOrder exists and is the assertion that
// actually measures the symptom. This one catches the cause earlier and cheaper.
func TestNoOrderWrapperConstructorMutatesItsOrder(t *testing.T) {
	order := func(t *testing.T, side Side, typ OrderType, price, qty int64) *Order {
		t.Helper()
		o, err := NewOrder("u", "X", side, typ, price, qty, TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		return o
	}

	cases := []struct {
		name      string
		construct func(t *testing.T, o *Order) error
		side      Side
		typ       OrderType
		price     int64
	}{
		{
			name:  "NewStopOrder",
			side:  SideSell,
			typ:   OrderTypeMarket,
			price: 0,
			construct: func(t *testing.T, o *Order) error {
				_, err := NewStopOrder(o, 90)
				return err
			},
		},
		{
			name:  "NewStopOrder (stop-limit)",
			side:  SideSell,
			typ:   OrderTypeLimit,
			price: 90,
			construct: func(t *testing.T, o *Order) error {
				_, err := NewStopOrder(o, 92)
				return err
			},
		},
		{
			name:  "NewOCOOrder (primary leg)",
			side:  SideSell,
			typ:   OrderTypeLimit,
			price: 110,
			construct: func(t *testing.T, o *Order) error {
				s, err := NewStopOrder(order(t, SideSell, OrderTypeMarket, 0, 5), 90)
				if err != nil {
					return err
				}
				_, err = NewOCOOrder(o, s)
				return err
			},
		},
		{
			name:  "NewOCOOrder (stop leg)",
			side:  SideSell,
			typ:   OrderTypeMarket,
			price: 0,
			construct: func(t *testing.T, o *Order) error {
				s, err := NewStopOrder(o, 90)
				if err != nil {
					return err
				}
				_, err = NewOCOOrder(order(t, SideSell, OrderTypeLimit, 110, 5), s)
				return err
			},
		},
		{
			name:  "NewPeggedOrder",
			side:  SideBuy,
			typ:   OrderTypeLimit,
			price: 100,
			construct: func(t *testing.T, o *Order) error {
				_, err := NewPeggedOrder(o, PegToBid, 2)
				return err
			},
		},
		{
			name:  "NewTrailingStop",
			side:  SideSell,
			typ:   OrderTypeMarket,
			price: 0,
			construct: func(t *testing.T, o *Order) error {
				_, err := NewTrailingStop(o, 5)
				return err
			},
		},
	}

	// The enumeration, before the cases, because a hand-written list of constructors
	// is exactly the thing this defect got past once already. The compiler cannot
	// enumerate functions the way it enumerates a struct's fields, so the package's
	// own source is read: every exported New* whose FIRST parameter is a *Order is a
	// wrapper constructor and must have a row below. A sixth wrapper type fails here
	// on the day it is written, which is the entryKindCount device applied to
	// functions.
	checked := map[string]bool{"NewIcebergOrder": true}
	for _, tc := range cases {
		checked[strings.Fields(tc.name)[0]] = true
	}
	for _, name := range orderWrapperConstructors(t) {
		if !checked[name] {
			t.Errorf("%s takes an order and is not checked for mutation. Add a row to this test: if it "+
				"mutates the order it is handed, the log record for it must carry enough to undo the "+
				"mutation, and pkg/wal's TestEveryWrapperRecordRebuildsItsOrder is where you prove it. "+
				"docs/ICEBERG-DURABILITY.md §6, Rule 14", name)
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := order(t, tc.side, tc.typ, tc.price, 9)
			before := *o
			if err := tc.construct(t, o); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !reflect.DeepEqual(before, *o) {
				t.Errorf("%s MUTATED the order it was handed.\nbefore: %+v\n after: %+v\n\n"+
					"If you are adding a constructor that mutates, the LOG RECORD for it must carry "+
					"enough to undo the mutation, and pkg/wal's TestEveryWrapperRecordRebuildsItsOrder "+
					"row is where you prove it. Every caller builds the wrapper and then hands it to a "+
					"Runner, which journals it before applying it — so a constructor's mutation is "+
					"already on the order by the time the write-ahead log sees it, and no ordering "+
					"discipline can recover what it discarded. docs/ICEBERG-DURABILITY.md §6, Rule 14",
					tc.name, before, *o)
			}
		})
	}

	// The one named exception, and the reason this test can name it: the record it
	// produces now carries the total, so the mutation is undone at replay. Without
	// that, the mutation below is a client's hidden reserve deleted by a restart.
	t.Run("NewIcebergOrder (the named exception)", func(t *testing.T) {
		o := order(t, SideSell, OrderTypeLimit, 100, 9)
		ib, err := NewIcebergOrder(o, 3)
		if err != nil {
			t.Fatalf("NewIcebergOrder: %v", err)
		}
		if o.Quantity != 3 || o.RemainingQty != 3 || ib.Hidden != 6 {
			t.Fatalf("NewIcebergOrder has stopped shrinking the order it is handed: order is "+
				"qty=%d remaining=%d with Hidden %d, want 3, 3 and 6. That would be a GOOD change — "+
				"and pkg/wal.AppendIceberg reconstructs the client's total with TotalRemaining(), "+
				"which this test is what documents. Fix both together, and check "+
				"TestEveryWrapperRecordRebuildsItsOrder. docs/ICEBERG-DURABILITY.md §6",
				o.Quantity, o.RemainingQty, ib.Hidden)
		}
		if got := ib.TotalRemaining(); got != 9 {
			t.Fatalf("TotalRemaining is %d, want 9. This is the expression the journal records as the "+
				"client's total; if it stops being the total, the record stops being the command.", got)
		}
	})
}

// orderWrapperConstructors reads this package's own source and returns every
// exported New* whose first parameter is a *Order.
//
// Reading source in a test is unusual and it is the only mechanical option: Go's
// reflection can enumerate a struct's fields but not a package's functions, and the
// alternative — a hand-maintained list of constructors — is precisely the artifact
// that let NewIcebergOrder's mutation go unexamined for four releases. Same trade
// internal/apicheck makes for the exported surface.
func orderWrapperConstructors(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	var out []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !fn.Name.IsExported() || !strings.HasPrefix(fn.Name.Name, "New") {
					continue
				}
				if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
					continue
				}
				star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				if id, ok := star.X.(*ast.Ident); ok && id.Name == "Order" {
					out = append(out, fn.Name.Name)
				}
			}
		}
	}
	if len(out) < 5 {
		t.Fatalf("found %d order-wrapper constructors (%v), want at least the five this package has — "+
			"the enumeration is broken, and a broken enumeration reports coverage of nothing", len(out), out)
	}
	return out
}
