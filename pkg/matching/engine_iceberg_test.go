package matching

import (
	"errors"
	"math"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func iceberg(t *testing.T, user string, side types.Side, price, total, display int64) *types.IcebergOrder {
	t.Helper()
	ib, err := types.NewIcebergOrder(lim(t, user, side, price, total), display)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	return ib
}

// TestIceberg_JitterConfigConserves: with Config.IcebergPeakJitter set, the reload
// sizes vary but the full hidden quantity still trades — jitter changes the peak
// size, never the total.
func TestIceberg_JitterConfigConserves(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.IcebergPeakJitter = decimal.RequireFromString("0.3") // ±30%
	e := NewEngine(cfg)
	e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 99, 100, 10))

	var traded int64
	// Drain the whole iceberg with small sell orders; count everything that fills.
	for range 200 {
		r := e.Process(marketOrder(t, "taker", types.SideSell, 1))
		for _, tr := range r.Trades {
			traded += tr.Quantity
		}
		if _, _, ok := e.BestBid(); !ok {
			break
		}
	}
	if traded != 100 {
		t.Errorf("jittered iceberg should still trade its full size: traded %d want 100", traded)
	}
}

func TestIceberg_ShowsOnlyDisplaySlice(t *testing.T) {
	e := newEngine()
	e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 99, 10, 3))

	// The book shows only the display slice (3), not the full 10.
	if _, qty, ok := e.BestBid(); !ok || qty != 3 {
		t.Errorf("visible best bid qty = %d, want 3 (rest hidden)", qty)
	}
}

func TestIceberg_RefillsAsConsumed(t *testing.T) {
	e := newEngine()
	e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 99, 10, 3))

	// Consume three display slices (9 total); each refill re-shows 3, 3, then 1.
	for range 3 {
		e.Process(marketOrder(t, "taker", types.SideSell, 3))
	}
	// After 9 consumed, only the final hidden unit (1) remains visible.
	if _, qty, ok := e.BestBid(); !ok || qty != 1 {
		t.Errorf("visible qty after 9 consumed = %d, want 1", qty)
	}
	// Consume the last unit; the iceberg is now fully worked off.
	e.Process(marketOrder(t, "taker", types.SideSell, 1))
	if _, _, ok := e.BestBid(); ok {
		t.Error("book should be empty after the iceberg is fully consumed")
	}
}

func TestIceberg_ImmediateCrossRefills(t *testing.T) {
	e := newEngine()
	// Resting asks: 99×3, 100×3, 101×3 (9 available).
	e.Process(lim(t, "a99", types.SideSell, 99, 3))
	e.Process(lim(t, "a100", types.SideSell, 100, 3))
	e.Process(lim(t, "a101", types.SideSell, 101, 3))

	// An aggressive iceberg buy for 8 (display 3) sweeps 99, 100, and 2 of 101.
	res := e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 101, 8, 3))
	if res.Status != types.OrderStatusFilled {
		t.Fatalf("status = %q, want FILLED", res.Status)
	}
	var total int64
	for _, tr := range res.Trades {
		total += tr.Quantity
	}
	if total != 8 {
		t.Errorf("iceberg bought %d, want 8", total)
	}
	// 1 unit of the 101 ask remains.
	if ask, qty, ok := e.BestAsk(); !ok || ask != 101 || qty != 1 {
		t.Errorf("remaining ask = %d x %d, want 101 x 1", ask, qty)
	}
}

// TestIceberg_RefillGoesToTheBackOfTheQueue is the queue-fairness property of the
// iceberg: a refilled slice is NEW liquidity and joins its price level behind
// everything already resting there. engine.go says so in a comment at the refill
// site ("the refilled slice re-enters at the back of its price level"), and until
// this test that sentence was the only thing asserting it.
//
// That gap mattered because docs/REFERENCE-MATCHER.md §3 credited the differential
// harness's ranked L3 comparison with catching "an iceberg refill that lands in the
// wrong place". It cannot: icebergs are tier 2 (see the commandTier table), so the
// generator never emits one, and the ranked comparison never sees a refill. The
// claim was checked by mutation — re-adding the refilled slice at the HEAD of its
// level, using only the book's existing exported API — and `go test ./...` passed
// across all 23 packages with the iceberg silently jumping the queue. The spec
// sentence has been corrected and this test is what actually holds the property.
//
// It is a fairness rule with money attached. A hidden order that kept its place
// through every refill would take priority it never queued for, which is the
// advantage displaying liquidity is supposed to buy.
func TestIceberg_RefillGoesToTheBackOfTheQueue(t *testing.T) {
	e := newEngine()

	// A 4-lot iceberg showing 2, then a plain 3-lot order joining behind it.
	e.ProcessIceberg(iceberg(t, "whale", types.SideBuy, 100, 4, 2))
	e.Process(lim(t, "joe", types.SideBuy, 100, 3))

	queue := func() []string {
		var users []string
		for _, o := range e.Book().GetOrdersAtPrice(types.SideBuy, 100) {
			users = append(users, o.UserID)
		}
		return users
	}
	if got, want := queue(), []string{"whale", "joe"}; !slicesEqual(got, want) {
		t.Fatalf("queue at 100 before the refill = %v, want %v", got, want)
	}

	// Consume the whole visible slice, which forces a refill of the hidden 2.
	e.Process(marketOrder(t, "taker", types.SideSell, 2))

	// The refilled slice is behind joe, who was already waiting. If this reads
	// [whale joe], the refill kept a queue position it did not earn.
	if got, want := queue(), []string{"joe", "whale"}; !slicesEqual(got, want) {
		t.Fatalf("queue at 100 after the refill = %v, want %v — the refilled slice must join at the BACK "+
			"of its price level, not keep the consumed slice's priority", got, want)
	}

	// And the refill is real liquidity, not just a repositioned marker.
	if _, qty, ok := e.BestBid(); !ok || qty != 5 {
		t.Fatalf("visible depth at the touch = %d, want 5 (joe's 3 plus the refilled 2)", qty)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- the two pins, inverted --------------------------------------------------
//
// The iceberg-durability audit (docs/ICEBERG-DURABILITY.md §1.3) went looking for
// the journal's copy of a defect and found two more consumers of the same cause:
// types.NewIcebergOrder shrinks the order it is handed to the display size, so
// everything downstream of the constructor — including this package's ingress size
// caps — saw the SLICE where the client sent a TOTAL.
//
// They were pinned rather than fixed there because "should an iceberg's cap be its
// total or its slice?" is a venue-policy question with its own answer to justify,
// and because changing what the engine accepts IS a matching-semantics change with
// a corpus extension and a version bump behind it. None of that belonged in a
// journal-format slice. docs/ICEBERG-ADMISSION.md is that answer, and it is Rule 1:
// the per-order size and notional controls measure the quantity the CLIENT's
// command puts to work, not the part of it the venue displays.
//
// Both tests below keep the names they were pinned under, so the pin is visibly the
// thing that was redeemed, and every assertion either survives unchanged or is
// inverted in place — none is dropped.

// TestIcebergEvadesTheMaxOrderSizeCap asserts the OPPOSITE of what its name says,
// which is the mechanism this repository uses for a redeemed pin.
//
// Config.MaxOrderQty is the fat-finger cap: the largest single order the venue will
// accept. A plain sell of 9 with the cap at 5 is refused, and so, now, is the same
// nine lots posted as an iceberg showing 3 — the cap sees ib.TotalRemaining() at
// submission and not the slice. ib.Hidden == 6 is kept from the pin: it used to say
// "the cap was evaded by exactly this much", and it now says what the cap saw.
func TestIcebergEvadesTheMaxOrderSizeCap(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.MaxOrderQty = 5
	e := NewEngine(cfg)

	// The control: nine lots as an ordinary order, refused.
	if res := e.Process(lim(t, "whale", types.SideSell, 100, 9)); res.Status != types.OrderStatusRejected {
		t.Fatalf("a plain sell of 9 under MaxOrderQty=5 ended %s, want REJECTED — the control has "+
			"broken, so this test no longer measures what it claims to", res.Status)
	}

	// The same nine lots, shown three.
	ib := iceberg(t, "whale", types.SideSell, 100, 9, 3)
	res := e.ProcessIceberg(ib)
	if res.Status != types.OrderStatusRejected {
		t.Fatalf("an iceberg of 9 shown 3 ended %s under MaxOrderQty=5, want REJECTED: the cap must "+
			"measure the client's TOTAL, or a client sets displayQty = MaxOrderQty and the control is "+
			"gone. docs/ICEBERG-ADMISSION.md §3.1", res.Status)
	}
	if !errors.Is(res.RejectionReason, types.ErrOrderExceedsMaxQty) {
		t.Fatalf("the refusal reason is %v, want ErrOrderExceedsMaxQty — refused for something else is "+
			"not this control doing its job", res.RejectionReason)
	}
	if ib.Hidden != 6 {
		t.Fatalf("the refused iceberg holds %d in reserve, want 6 — this assertion is kept from the pin: "+
			"it used to say the cap was evaded by exactly the hidden quantity, and it now says the six "+
			"lots the cap could not see are the six it counted", ib.Hidden)
	}
	if _, qty, ok := e.BestAsk(); ok {
		t.Fatalf("the refused iceberg shows %d at the offer, want nothing resting", qty)
	}
}

// TestIcebergIsRefusedForAMinimumItExceeds is the same rule pointing the other way,
// inverted with its name kept for the same reason.
//
// Config.MinOrderQty rejects dust. An iceberg of 90 shown 3 is ninety lots of real
// size — eighteen times the floor — and it is now admitted for what it is. Three
// lots rest at the offer BELOW the floor, which is the rule that will look like a
// bug (docs/ICEBERG-ADMISSION.md §8): the floor is about the order, and the order
// is 90.
func TestIcebergIsRefusedForAMinimumItExceeds(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.MinOrderQty = 5
	e := NewEngine(cfg)

	res := e.ProcessIceberg(iceberg(t, "whale", types.SideSell, 100, 90, 3))
	if res.Status == types.OrderStatusRejected {
		t.Fatalf("an iceberg of 90 shown 3 was refused under MinOrderQty=5 for %v — the floor must "+
			"measure the client's TOTAL, which is eighteen times above it. "+
			"docs/ICEBERG-ADMISSION.md §3.1", res.RejectionReason)
	}
	if res.RejectionReason != nil {
		t.Fatalf("the accepted iceberg carries rejection reason %v, want nil — this assertion is the "+
			"pin's errors.Is check inverted, not deleted", res.RejectionReason)
	}
	if _, qty, ok := e.BestAsk(); !ok || qty != 3 {
		t.Fatalf("the accepted iceberg shows %d at the offer, want 3 — a slice below the dust floor "+
			"rests, because the floor judged the 90-lot command and not the slice", qty)
	}
}

// --- the three checks no pin covered -----------------------------------------
//
// docs/ICEBERG-ADMISSION.md §1.2: the defect is five checks, not two. The two
// notional controls are the same arithmetic on the same wrong number four lines
// further down the same function, and the int64 overflow guard — which has no
// Config knob, which Privileged orders do not bypass, and which checkOrderCaps's
// own comment calls an arithmetic invariant rather than ingress policy — is the
// third pair's third member.

// TestIcebergCannotEvadeTheMaxOrderNotional is deliverable 7's first half: the
// notional cap measured against the slice admitted an order worth 900 under a cap
// of 500, and the client could pick the display size that made it fit.
func TestIcebergCannotEvadeTheMaxOrderNotional(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.MaxOrderNotional = 500
	e := NewEngine(cfg)

	// The control: nine lots at 100 is 900 of notional as an ordinary order, refused.
	if res := e.Process(lim(t, "whale", types.SideSell, 100, 9)); res.Status != types.OrderStatusRejected {
		t.Fatalf("a plain sell of 9 @100 under MaxOrderNotional=500 ended %s, want REJECTED — the "+
			"control has broken", res.Status)
	}

	ib := iceberg(t, "whale", types.SideSell, 100, 9, 3)
	res := e.ProcessIceberg(ib)
	if !errors.Is(res.RejectionReason, types.ErrOrderExceedsMaxNotional) {
		t.Fatalf("an iceberg of 9 @100 shown 3 ended %s/%v under MaxOrderNotional=500, want REJECTED "+
			"with ErrOrderExceedsMaxNotional: price × 3 is 300 and fits, price × the client's 9 is 900 "+
			"and does not", res.Status, res.RejectionReason)
	}
	if ib.Hidden != 6 {
		t.Fatalf("the refused iceberg holds %d in reserve, want 6 — 600 of the notional the cap now "+
			"counts was the part it could not see", ib.Hidden)
	}
	if _, qty, ok := e.BestAsk(); ok {
		t.Fatalf("the refused iceberg shows %d at the offer, want nothing resting", qty)
	}
}

// TestIcebergClearsTheMinimumNotionalItsTotalExceeds is deliverable 7's second half,
// and it is the direction that refuses real size rather than admitting it.
func TestIcebergClearsTheMinimumNotionalItsTotalExceeds(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.MinOrderNotional = 500
	e := NewEngine(cfg)

	res := e.ProcessIceberg(iceberg(t, "whale", types.SideSell, 100, 90, 3))
	if res.RejectionReason != nil {
		t.Fatalf("an iceberg of 90 @100 shown 3 was refused for %v under MinOrderNotional=500 — its "+
			"notional is 9000, eighteen times the floor", res.RejectionReason)
	}
	if _, qty, ok := e.BestAsk(); !ok || qty != 3 {
		t.Fatalf("the accepted iceberg shows %d at the offer, want 3", qty)
	}
}

// TestIcebergCannotEvadeTheNotionalOverflowGuard is deliverable 8, and it is the
// whole protection for the one check in checkOrderCaps an operator cannot configure
// at all: there is no knob to turn it off, Privileged orders do not bypass it, and
// its doc comment claims it is an arithmetic invariant rather than ingress policy.
// An invariant a client steps around by choosing an order type is not an invariant.
//
// Sabotage row 3 of docs/ICEBERG-ADMISSION.md §11 is "fix the four configured caps
// and leave checkedMul on order.Quantity". This test is the only thing that fires.
func TestIcebergCannotEvadeTheNotionalOverflowGuard(t *testing.T) {
	e := newEngine() // no caps configured at all: only the invariant is live

	const price = 100
	var qty int64 = math.MaxInt64/price + 1 // price × qty cannot be represented

	// The control: the same order, plain, is refused for the overflow.
	if res := e.Process(lim(t, "whale", types.SideSell, price, qty)); !errors.Is(res.RejectionReason, types.ErrNotionalOverflow) {
		t.Fatalf("a plain sell of %d @%d ended %s/%v, want REJECTED with ErrNotionalOverflow — the "+
			"control has broken", qty, price, res.Status, res.RejectionReason)
	}

	res := e.ProcessIceberg(iceberg(t, "whale", types.SideSell, price, qty, 3))
	if !errors.Is(res.RejectionReason, types.ErrNotionalOverflow) {
		t.Fatalf("an iceberg of %d @%d shown 3 ended %s/%v, want REJECTED with ErrNotionalOverflow: a "+
			"notional that wraps int64 must not reach the book because three characters of the command "+
			"said `display 3`", qty, price, res.Status, res.RejectionReason)
	}
	if _, _, ok := e.BestAsk(); ok {
		t.Fatal("an order whose notional overflows int64 rested on the book")
	}
}

// --- admission runs once ------------------------------------------------------

// TestIcebergRefillIsNotASecondSubmission is deliverable 4, and it closes the third
// checkOrderCaps defect no pin covered (docs/ICEBERG-ADMISSION.md §1.3).
//
// ProcessIceberg's refill loop re-settled the refilled slice and DISCARDED the
// verdict, so under MinOrderQty=2 an iceberg of 10 shown 3 worked off 3, 3, 3 and
// then had its last lot refused for being below the dust floor — refused inside the
// engine, with the refusal thrown away, no event published, nothing rested, and the
// client told FILLED. One lot of a client's order evaporated.
//
// Rule 3: the ingress controls run when a COMMAND arrives, and a refill is not a
// command. The maker-side refill in match() has never run admission at all, so this
// is also the two refill paths agreeing for the first time.
//
// The depth here is 10 rather than §1.3's 9 so that the tail lot has something to
// trade against and every count is exact: ten lots is the client's whole order.
func TestIcebergRefillIsNotASecondSubmission(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.MinOrderQty = 2
	sink := &findingSink{}
	cfg.EventSink = sink
	e := NewEngine(cfg)

	// Ten lots of resting depth at 100, in two orders that clear the floor themselves.
	e.Process(lim(t, "maker", types.SideSell, 100, 5))
	e.Process(lim(t, "maker", types.SideSell, 100, 5))

	ib := iceberg(t, "whale", types.SideBuy, 100, 10, 3) // slices 3, 3, 3, 1
	res := e.ProcessIceberg(ib)

	var traded int64
	for _, tr := range res.Trades {
		traded += tr.Quantity
	}
	if traded != 10 {
		t.Errorf("the iceberg traded %d lots of the ten it submitted — the tail slice of 1 was refused "+
			"by MinOrderQty=2 and the refusal discarded, which destroys a client's quantity inside the "+
			"engine (docs/ICEBERG-ADMISSION.md §1.3)", traded)
	}
	if ib.Hidden != 0 || ib.Order.RemainingQty != 0 {
		t.Errorf("after the walk the iceberg holds Hidden=%d RemainingQty=%d, want 0 and 0",
			ib.Hidden, ib.Order.RemainingQty)
	}
	if res.Status != types.OrderStatusFilled {
		t.Errorf("status = %s, want FILLED", res.Status)
	}
	for _, ev := range sink.evs {
		if ev.Kind == EventRejected {
			t.Fatalf("a REJECTED reached the event sink for an order the venue admitted: %v (whole "+
				"stream %v)", ev.Reason, kindsFrom(sink.evs))
		}
	}
}

// TestIcebergRefilledTailRestsBelowTheDustFloor is the same rule seen from the other
// end, and it is docs/ICEBERG-ADMISSION.md §8's third row: a refilled slice smaller
// than MinOrderQty rests, because it is the tail of an order that was admitted at 90.
// Refusing an order's own tail leaves the client holding a quantity the venue will
// neither trade nor return.
func TestIcebergRefilledTailRestsBelowTheDustFloor(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.MinOrderQty = 5
	e := NewEngine(cfg)

	e.ProcessIceberg(iceberg(t, "whale", types.SideSell, 100, 10, 3)) // slices 3, 3, 3, 1
	// Nine lots of taking in one order (a taker of 3 would be dust itself), so the
	// fourth slice is the one-lot remainder.
	e.Process(lim(t, "taker", types.SideBuy, 100, 9))
	if _, qty, ok := e.BestAsk(); !ok || qty != 1 {
		t.Fatalf("the refilled tail shows %d at the offer (resting=%v), want 1 — admission judged the "+
			"90-lot command once, and a refill is not a command", qty, ok)
	}
}

// --- the two checks that must NOT move ---------------------------------------
//
// Deliverable 9. §3.3 claims MaxOrdersPerAccount counts orders and the price band
// tests a price, so neither moves with the size caps. Both were measured during the
// audit; these are the assertions that stop them being decoration.

// TestIcebergCountsOnceAgainstMaxOrdersPerAccount: an iceberg is ONE order however
// many slices it shows, and each refill re-adds the same order id. A cap that
// counted slices would refuse a client for the venue's own refill.
func TestIcebergCountsOnceAgainstMaxOrdersPerAccount(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.MaxOrdersPerAccount = 2
	e := NewEngine(cfg)

	if res := e.ProcessIceberg(iceberg(t, "whale", types.SideSell, 100, 90, 3)); res.RejectionReason != nil {
		t.Fatalf("the iceberg was refused for %v", res.RejectionReason)
	}
	if res := e.Process(lim(t, "whale", types.SideSell, 101, 1)); res.Status != types.OrderStatusNew {
		t.Fatalf("the account's second order ended %s, want NEW — the 90-lot iceberg resting must count "+
			"as one order, not as thirty slices", res.Status)
	}
	res := e.Process(lim(t, "whale", types.SideSell, 102, 1))
	if !errors.Is(res.RejectionReason, types.ErrTooManyOrders) {
		t.Fatalf("the account's third order ended %s/%v, want REJECTED with ErrTooManyOrders — the cap "+
			"counts orders and it still counts them", res.Status, res.RejectionReason)
	}
}

// TestIcebergOutsideThePriceBandIsRefusedForTheBand: the collar tests a price, so an
// iceberg meets it exactly as a plain order does, and it is refused for the band and
// not for anything the size caps now measure.
func TestIcebergOutsideThePriceBandIsRefusedForTheBand(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.PriceBand = decimal.RequireFromString("0.10") // ±10%
	e := NewEngine(cfg)

	// A trade at 100, so the band has a reference.
	e.Process(lim(t, "m", types.SideSell, 100, 1))
	e.Process(lim(t, "t", types.SideBuy, 100, 1))

	res := e.ProcessIceberg(iceberg(t, "whale", types.SideSell, 150, 9, 3))
	if !errors.Is(res.RejectionReason, types.ErrPriceOutsideBand) {
		t.Fatalf("an iceberg priced at 150 with the last trade at 100 ended %s/%v, want REJECTED with "+
			"ErrPriceOutsideBand", res.Status, res.RejectionReason)
	}
}

// --- these controls run on replay --------------------------------------------

// TestIcebergAdmissionRunsDuringReplay is load-bearing for docs/ICEBERG-ADMISSION.md
// §6 and for SetReplaying's doc comment, which says at length that the admission
// controls are NOT in the replay bypass set: the journal is written write-ahead, so
// it records commands as SUBMITTED and an order the live engine rejected is in the
// log like any other. Skipping the caps on replay rests live-rejected orders on the
// recovered book.
//
// It is what fires if somebody "fixes" the replay divergence in §6.2 by adding
// `&& !e.replaying` to the caps (sabotage row 8), and it is why the fix needs a
// semantics bump rather than a quiet release: a pre-fix log replayed by this build
// reaches a different verdict, which is exactly what the stamp exists to refuse.
func TestIcebergAdmissionRunsDuringReplay(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.MaxOrderQty = 5
	e := NewEngine(cfg)
	e.SetReplaying(true)

	res := e.ProcessIceberg(iceberg(t, "whale", types.SideSell, 100, 9, 3))
	if !errors.Is(res.RejectionReason, types.ErrOrderExceedsMaxQty) {
		t.Fatalf("replaying an iceberg of 9 shown 3 under MaxOrderQty=5 ended %s/%v, want REJECTED with "+
			"ErrOrderExceedsMaxQty — the size caps are deterministic functions of the order, the config "+
			"and the replayed book, and they run on replay so the recovered book matches the live one",
			res.Status, res.RejectionReason)
	}
	if _, _, ok := e.BestAsk(); ok {
		t.Fatal("the replayed iceberg rested on the recovered book anyway")
	}
}

// --- the registry holds only icebergs that are resting ------------------------

// TestARefusedIcebergLeavesNoSnapshotEntry is docs/ICEBERG-ADMISSION.md §13.4, which
// this slice found on the way and originally deferred.
//
// ProcessIceberg registers the wrapper in e.icebergOrders BEFORE settling, because
// the maker-side refill in match() needs to find it there. It did not undo that when
// the settle refused, so TakeSnapshot wrote an IcebergEntry for an order that is not
// on the book, and LoadSnapshot refuses such a snapshot outright with "iceberg N has
// no resting displayed slice". One refused iceberg and the venue can never load its
// own checkpoint again — not after the next trade, not after a restart, until it is
// rebuilt from an empty book.
//
// It is reported here rather than in PINNED-DEFECTS.md because THIS SLICE MAKES IT
// ROUTINE. Before it, an over-cap iceberg rested; after it, that is the headline
// rejection, and the runbook for the semantics upgrade tells an operator to take a
// checkpoint immediately after starting on the new build. Following documented
// remediation must not stand a venue up that cannot restart.
//
// The row for MaxOrderQty is the one this slice created. The other five are
// pre-existing paths through the same registry-without-undo, and they are in the
// table because the fix is one condition — "did this command leave a slice resting?"
// — and a fix that closed only the new row would have been a fix aimed at the test.
func TestARefusedIcebergLeavesNoSnapshotEntry(t *testing.T) {
	icebergTIF := func(t *testing.T, side types.Side, price, total, display int64, tif types.TimeInForce) *types.IcebergOrder {
		t.Helper()
		o, err := types.NewOrder("whale", "BTC-USD", side, types.OrderTypeLimit, price, total, tif)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		ib, err := types.NewIcebergOrder(o, display)
		if err != nil {
			t.Fatalf("NewIcebergOrder: %v", err)
		}
		return ib
	}

	cases := []struct {
		name  string
		cfg   func(*Config)
		setup func(*testing.T, *Engine)
		order func(*testing.T) *types.IcebergOrder
	}{{
		name:  "over MaxOrderQty",
		cfg:   func(c *Config) { c.MaxOrderQty = 5 },
		order: func(t *testing.T) *types.IcebergOrder { return iceberg(t, "whale", types.SideSell, 100, 9, 3) },
	}, {
		name:  "under MinOrderQty by its total too",
		cfg:   func(c *Config) { c.MinOrderQty = 5 },
		order: func(t *testing.T) *types.IcebergOrder { return iceberg(t, "whale", types.SideSell, 100, 1, 1) },
	}, {
		name:  "over MaxOrderNotional",
		cfg:   func(c *Config) { c.MaxOrderNotional = 500 },
		order: func(t *testing.T) *types.IcebergOrder { return iceberg(t, "whale", types.SideSell, 100, 9, 3) },
	}, {
		name:  "halted venue",
		setup: func(_ *testing.T, e *Engine) { e.Halt() },
		order: func(t *testing.T) *types.IcebergOrder { return iceberg(t, "whale", types.SideSell, 100, 9, 3) },
	}, {
		name: "post-only that would cross",
		setup: func(t *testing.T, e *Engine) {
			e.Process(lim(t, "mm", types.SideBuy, 100, 5))
		},
		order: func(t *testing.T) *types.IcebergOrder {
			ib := iceberg(t, "whale", types.SideSell, 100, 9, 3)
			ib.Order.PostOnly = true
			return ib
		},
	}, {
		name: "fill-or-kill that cannot fill",
		order: func(t *testing.T) *types.IcebergOrder {
			return icebergTIF(t, types.SideSell, 100, 9, 3, types.TIFFillOrKill)
		},
	}, {
		// Not a rejection: an immediate-or-cancel remainder ends CANCELLED, and it
		// leaves no slice resting either. Same condition, same repair.
		name: "immediate-or-cancel with nothing to trade against",
		order: func(t *testing.T) *types.IcebergOrder {
			return icebergTIF(t, types.SideSell, 100, 9, 3, types.TIFImmediateOrCancel)
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig("BTC-USD")
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			e := NewEngine(cfg)
			if tc.setup != nil {
				tc.setup(t, e)
			}

			res := e.ProcessIceberg(tc.order(t))
			if _, _, ok := e.BestAsk(); ok {
				t.Fatalf("this case is supposed to leave no displayed slice resting, and it rested one "+
					"(verdict %s/%v) — the case no longer measures what it claims to", res.Status, res.RejectionReason)
			}

			snap := e.TakeSnapshot()
			if len(snap.Icebergs) != 0 {
				t.Errorf("the snapshot carries %d iceberg entries after a command that ended %s and "+
					"rested nothing, want 0 — an entry for an order that is not on the book is what "+
					"makes the checkpoint unloadable", len(snap.Icebergs), res.Status)
			}

			// The consequence, asserted rather than inferred: this is the failure an
			// operator meets, one restart later.
			fresh := NewEngine(cfg)
			if err := fresh.LoadSnapshot(snap); err != nil {
				t.Fatalf("the venue cannot load its own checkpoint after one iceberg ended %s: %v — "+
					"ProcessIceberg registered the wrapper before settling and did not undo it",
					res.Status, err)
			}

			// And it does not heal: the poisoning used to survive everything that
			// happened afterwards, for the life of the engine.
			e.Process(lim(t, "a", types.SideSell, 101, 4))
			e.Process(lim(t, "b", types.SideBuy, 101, 4))
			later := e.TakeSnapshot()
			if err := NewEngine(cfg).LoadSnapshot(later); err != nil {
				t.Errorf("a later checkpoint is still unloadable: %v", err)
			}
		})
	}
}

// TestAnAdmittedIcebergIsStillInTheSnapshot is the control for the test above: the
// registry cleanup must remove exactly the entries whose slice is not resting, and
// nothing else. An iceberg that rests keeps its reserve across a snapshot round trip.
func TestAnAdmittedIcebergIsStillInTheSnapshot(t *testing.T) {
	cfg := DefaultConfig("BTC-USD")
	cfg.MaxOrderQty = 100
	cfg.MinOrderQty = 5
	e := NewEngine(cfg)

	if res := e.ProcessIceberg(iceberg(t, "whale", types.SideSell, 100, 90, 3)); res.RejectionReason != nil {
		t.Fatalf("the iceberg was refused for %v", res.RejectionReason)
	}
	snap := e.TakeSnapshot()
	if len(snap.Icebergs) != 1 {
		t.Fatalf("the snapshot carries %d iceberg entries for one resting iceberg, want 1 — the "+
			"registry cleanup must not drop an order that IS on the book", len(snap.Icebergs))
	}
	if got := snap.Icebergs[0].Hidden; got != 87 {
		t.Fatalf("the snapshot records %d hidden lots, want 87", got)
	}

	fresh := NewEngine(cfg)
	if err := fresh.LoadSnapshot(snap); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	// The restored engine still refills from the reserve, and still publishes only
	// the slice.
	if _, qty, ok := fresh.BestAsk(); !ok || qty != 3 {
		t.Fatalf("the restored book shows %d at the offer (resting=%v), want the displayed 3", qty, ok)
	}
	fresh.Process(lim(t, "taker", types.SideBuy, 100, 3))
	if _, qty, ok := fresh.BestAsk(); !ok || qty != 3 {
		t.Fatalf("after the restored iceberg's slice was consumed the offer shows %d (resting=%v), "+
			"want a refilled 3 — the reserve did not survive the round trip", qty, ok)
	}
}
