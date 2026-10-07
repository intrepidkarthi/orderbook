package flash1

import (
	"math"
	"reflect"
	"testing"
)

// rec collects reports.
type rec struct{ got []Report }

func (r *rec) emit(x Report) { r.got = append(r.got, x) }

func (r *rec) take() []Report {
	out := r.got
	r.got = nil
	return out
}

func newRec() (*Adapter, *rec) {
	r := &rec{}
	return New(r.emit), r
}

func want(t *testing.T, step string, got, want []Report) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got %+v\nwant %+v", step, got, want)
	}
}

const (
	buy  = 0
	sell = 1
)

func TestNewOrderAckThenTradesAtTheMakersPrice(t *testing.T) {
	a, r := newRec()
	a.NewOrder(1, 100, 1010, 5, sell, false)
	want(t, "resting sell", r.take(), []Report{{Type: OrderAck, Side: sell, Seq: 1, OrderID: 100, Price: 1010, Qty: 5}})
	a.NewOrder(2, 200, 1012, 3, buy, false)
	want(t, "crossing buy", r.take(), []Report{
		{Type: OrderAck, Side: buy, Seq: 2, OrderID: 200, Price: 1012, Qty: 3},
		{Type: Trade, Seq: 2, OrderID: 100, Price: 1010, Qty: 3, Maker: 100, Taker: 200},
	})
}

func TestIOCResidualIsCancelAcked(t *testing.T) {
	a, r := newRec()
	a.NewOrder(1, 100, 1010, 2, sell, false)
	r.take()
	a.NewOrder(2, 200, 1010, 5, buy, true)
	want(t, "IOC buy 5 against 2", r.take(), []Report{
		{Type: OrderAck, Side: buy, Seq: 2, OrderID: 200, Price: 1010, Qty: 5},
		{Type: Trade, Seq: 2, OrderID: 100, Price: 1010, Qty: 2, Maker: 100, Taker: 200},
		{Type: CancelAck, Side: buy, Seq: 2, OrderID: 200, Price: 1010, Qty: 3},
	})
	if a.BestBid() != math.MinInt64 {
		t.Fatal("an IOC residual rested")
	}
	a.NewOrder(3, 300, 900, 4, buy, true)
	want(t, "IOC that meets nothing", r.take(), []Report{
		{Type: OrderAck, Side: buy, Seq: 3, OrderID: 300, Price: 900, Qty: 4},
		{Type: CancelAck, Side: buy, Seq: 3, OrderID: 300, Price: 900, Qty: 4},
	})
}

func TestCancelAckCarriesTheRemainderAndRejectsWhatIsNotResting(t *testing.T) {
	a, r := newRec()
	a.NewOrder(1, 100, 1010, 5, sell, false)
	a.NewOrder(2, 200, 1010, 2, buy, false)
	r.take()
	a.Cancel(3, 100)
	want(t, "cancel the partly filled sell", r.take(), []Report{{Type: CancelAck, Side: sell, Seq: 3, OrderID: 100, Price: 1010, Qty: 3}})
	a.Cancel(4, 100)
	want(t, "cancel it again", r.take(), []Report{{Type: CancelReject, Seq: 4, OrderID: 100}})
	a.Cancel(5, 999)
	want(t, "cancel an id never seen", r.take(), []Report{{Type: CancelReject, Seq: 5, OrderID: 999}})
	a.Cancel(6, 200)
	want(t, "cancel the buy that fully filled", r.take(), []Report{{Type: CancelReject, Seq: 6, OrderID: 200}})
}

// TestAFilledMakerCannotBeCancelled: the order rested, then filled against a later
// aggressor. The adapter's map still names it; the engine is the one that knows.
func TestAFilledMakerCannotBeCancelled(t *testing.T) {
	a, r := newRec()
	a.NewOrder(1, 100, 1010, 2, sell, false)
	a.NewOrder(2, 200, 1010, 2, buy, false)
	r.take()
	a.Cancel(3, 100)
	want(t, "cancel a fully filled maker", r.take(), []Report{{Type: CancelReject, Seq: 3, OrderID: 100}})
	a.Modify(4, 100, 1011, 1, sell)
	want(t, "modify a fully filled maker", r.take(), []Report{{Type: ModifyReject, Seq: 4, OrderID: 100}})
}

func TestModifyAcksFirstThenTradesUnderItsOwnSequence(t *testing.T) {
	a, r := newRec()
	a.NewOrder(1, 100, 1010, 5, sell, false)
	a.NewOrder(2, 200, 1005, 4, buy, false)
	r.take()
	a.Modify(3, 200, 1010, 6, buy)
	want(t, "reprice the buy through the ask", r.take(), []Report{
		{Type: ModifyAck, Side: buy, Seq: 3, OrderID: 200, Price: 1010, Qty: 6},
		{Type: Trade, Seq: 3, OrderID: 100, Price: 1010, Qty: 5, Maker: 100, Taker: 200},
	})
	if got := a.DepthAt(1010, buy); got != 1 {
		t.Fatalf("the modified buy's remainder: depth %d, want 1", got)
	}
	a.Modify(4, 777, 1000, 1, buy)
	want(t, "modify an id never seen", r.take(), []Report{{Type: ModifyReject, Seq: 4, OrderID: 777}})
}

// TestModifyLosesPriority: two sells at one price, the first is modified to the same
// price, and the next buy trades with the second. Keeping priority on modify would be
// a defensible design, and is not the harness's.
func TestModifyLosesPriority(t *testing.T) {
	a, r := newRec()
	a.NewOrder(1, 100, 1010, 1, sell, false)
	a.NewOrder(2, 101, 1010, 1, sell, false)
	a.Modify(3, 100, 1010, 1, sell)
	r.take()
	a.NewOrder(4, 200, 1010, 1, buy, false)
	got := r.take()
	if len(got) != 2 || got[1].Maker != 101 {
		t.Fatalf("after modifying 100, the buy traded with %+v, want maker 101", got)
	}
	// The modified order still trades under the client's id.
	a.NewOrder(5, 201, 1010, 1, buy, false)
	if got := r.take(); len(got) != 2 || got[1].Maker != 100 || got[1].OrderID != 100 {
		t.Fatalf("the modified order as maker: %+v, want maker 100", got)
	}
}

// TestNoSelfTradePrevention: every order comes from one user, and they must trade.
func TestNoSelfTradePrevention(t *testing.T) {
	a, r := newRec()
	a.NewOrder(1, 100, 1010, 1, sell, false)
	a.NewOrder(2, 200, 1010, 1, buy, false)
	if got := r.take(); len(got) != 3 || got[2].Type != Trade {
		t.Fatalf("one-user orders did not trade: %+v", got)
	}
}

func TestAuditQueries(t *testing.T) {
	a, _ := newRec()
	if a.BestBid() != math.MinInt64 || a.BestAsk() != math.MaxInt64 {
		t.Fatalf("empty book: bid %d ask %d", a.BestBid(), a.BestAsk())
	}
	a.NewOrder(1, 1, 1000, 3, buy, false)
	a.NewOrder(2, 2, 1000, 4, buy, false)
	a.NewOrder(3, 3, 999, 1, buy, false)
	a.NewOrder(4, 4, 1005, 2, sell, false)
	if a.BestBid() != 1000 || a.BestAsk() != 1005 {
		t.Fatalf("bid %d ask %d, want 1000 / 1005", a.BestBid(), a.BestAsk())
	}
	if d := a.DepthAt(1000, buy); d != 7 {
		t.Fatalf("depth at 1000 buy = %d, want 7", d)
	}
	if d := a.DepthAt(1000, sell); d != 0 {
		t.Fatalf("depth at 1000 sell = %d, want 0", d)
	}
	if d := a.DepthAt(1005, sell); d != 2 {
		t.Fatalf("depth at 1005 sell = %d, want 2", d)
	}
}
