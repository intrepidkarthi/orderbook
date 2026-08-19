package semcheck

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/intrepidkarthi/orderbook/internal/tape"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The corpus is three things, and the third is the one that keeps the other two
// honest.
//
//  1. THE DIFFERENTIAL PROFILES at fixed seeds. They are the alphabet the reference
//     matcher is compared against, so they are the paths this repository has the most
//     evidence about, and tape.Gen is a pure function of (profile, seed, n) whose
//     primitive is pinned by a golden of its own.
//  2. THE RECOVERY PROFILE, which carries phase transitions and therefore the opening
//     and closing uncrosses — behaviour no continuous tape reaches.
//  3. A HAND-WRITTEN TIER-2 SCRIPT, because generated tapes do not reach stops, OCO,
//     icebergs, trailing stops, pegged orders, busts, mark prices, band breaches or
//     expiry, and every one of those is decided behaviour a change could move.
//
// The third exists because Rule 22 of docs/SEMANTICS-VERSION.md makes the corpus
// load-bearing: a bump has to be JUSTIFIED by a body change, so a behaviour the corpus
// never reaches is a behaviour nobody can bump for. Dropping the tier-2 script does
// not fail any single line of the golden — it fails the coverage guard, and it makes a
// stop-trigger change invisible. That is §5.5's boundary, and it is demonstrated in
// the sabotage runs rather than asserted.

// Corpus is the whole fingerprint input, in a fixed order.
func Corpus() []Scenario {
	base := func() matching.Config {
		c := matching.DefaultConfig(Symbol)
		c.Clock = Clock()
		return c
	}
	return []Scenario{
		{
			// Price-time allocation, shard 0. The baseline the reference matcher is
			// swept against.
			Name:   "fifo",
			Config: base,
			Script: fromTape(tape.Differential, []uint64{1, 2, 3}, 120),
		},
		{
			// Pro-rata allocation on a non-zero shard. This is the scenario the whole
			// slice exists for: the change that opened the gap was pro-rata no longer
			// skipping a taker's own liquidity, and reverting it must move a line
			// here.
			Name: "prorata-shard7",
			Config: func() matching.Config {
				c := base()
				c.ProRata = true
				c.ShardIndex = 7
				return c
			},
			Script: fromTape(tape.ProRata, []uint64{1, 4}, 120),
		},
		{
			// A book cap small enough to be REACHED, and DECREMENT as the venue
			// default rather than a per-order mode. MaxOrders changes a verdict — an
			// order that has already traded and then finds no room is rejected with
			// its prints standing — so a profile that never hits the cap is not
			// exercising it.
			Name: "capped-decrement-shard3",
			Config: func() matching.Config {
				c := base()
				c.MaxOrders = 8
				c.SelfTradePrevention = matching.STPDecrement
				c.ShardIndex = 3
				return c
			},
			Script: fromTape(tape.Differential, []uint64{7, 11}, 120),
		},
		{
			// Phase transitions, and therefore the opening and closing uncrosses.
			Name:   "auction",
			Config: base,
			Script: fromTape(tape.Recovery, []uint64{1}, 200),
		},
		{
			// Tier 2: everything a generated tape cannot draw.
			Name: "conditional",
			Config: func() matching.Config {
				c := base()
				// A session close, so DAY is accepted rather than refused, and a
				// mark-step bound loose enough that the script's marks land.
				c.SessionClose = func() time.Time {
					return time.Date(2026, 1, 2, 17, 0, 0, 0, time.UTC)
				}
				c.DedupClientOrderIDs = 8
				return c
			},
			Script: tierTwo(),
		},
		{
			// The admission controls, on their own, because they change verdicts and
			// nothing else in the corpus turns them on. A band that is REACHED, a
			// per-order size cap, a dust floor and a per-account cap.
			Name: "guarded",
			Config: func() matching.Config {
				c := base()
				c.PriceBand = pct(10)
				c.MaxOrderQty = 40
				c.MinOrderQty = 2
				c.MaxOrdersPerAccount = 3
				return c
			},
			Script: guarded(),
		},
		{
			// The NOTIONAL half of the same admission surface, in a scenario of its
			// own rather than as more lines in `guarded`.
			//
			// It exists because two independent reviews of the size-cap fix found the
			// same hole and it was real: `guarded` sets no notional caps, so reverting
			// checkOrderCaps's checkedMul from the client's total back to the displayed
			// slice — reopening BOTH notional controls and the int64 overflow guard,
			// the one an operator cannot configure off — left this whole file GREEN.
			// Three unit tests fired and the fingerprint did not, which is exactly the
			// state Rule 22 exists to make impossible: a change to what a control
			// measures that no bump could be justified by.
			//
			// docs/ICEBERG-ADMISSION.md §12 predicted this and offered "a second
			// scenario with the notional caps set, and the cost is a wider diff". This
			// is that scenario, and it is appended last so the cost is only its own
			// lines: no existing scenario's ids, event ids or digests move.
			//
			// Separate rather than merged into `guarded` because the two size caps
			// there would decide most of these commands before the notional pair saw
			// them — an order big enough to breach a notional ceiling is usually big
			// enough to breach a lot ceiling first — and a case decided by the wrong
			// control is a case that measures nothing.
			Name: "notional",
			Config: func() matching.Config {
				c := base()
				c.MinOrderNotional = 500
				c.MaxOrderNotional = 5000
				return c
			},
			Script: notionalGuarded(),
		},
	}
}

// fromTape converts the shared generator's commands into this package's vocabulary,
// concatenating the named seeds.
//
// The generator is internal/tape and not a second one written here, for the reason
// that package's doc comment gives: two generators means two alphabets, means one of
// them is extended and the other is not.
func fromTape(p tape.Profile, seeds []uint64, n int) []Cmd {
	var out []Cmd
	pos := 0
	for _, seed := range seeds {
		for _, c := range tape.Gen(p, seed, n) {
			out = append(out, convert(c, pos))
			pos++
		}
	}
	return out
}

func convert(c tape.Cmd, pos int) Cmd {
	out := Cmd{
		Pos: pos, User: c.User, Sell: c.Sell, Price: c.Price, Qty: c.Qty,
		PostOnly: c.PostOnly, TradeGroup: c.TradeGroup, Privileged: c.Privileged,
		NewQty: c.NewQty,
	}
	// Target is the earlier command's POSITION, and the tape's positions are
	// per-seed while this script's are cumulative, so the offset of the current seed
	// has to travel with it. pos - c.Pos is exactly that offset.
	out.Target = pos - c.Pos + c.Target
	if c.MarketOrd {
		out.Type = types.OrderTypeMarket
	} else {
		out.Type = types.OrderTypeLimit
	}
	switch c.TIF {
	case 1:
		out.TIF = types.TIFImmediateOrCancel
	case 2:
		out.TIF = types.TIFFillOrKill
	default:
		out.TIF = types.TIFGoodTillCancel
	}
	switch c.STP {
	case 1:
		out.STP = string(matching.STPCancelNewest)
	case 2:
		out.STP = string(matching.STPCancelOldest)
	case 3:
		out.STP = string(matching.STPCancelBoth)
	case 4:
		out.STP = string(matching.STPDecrement)
	case 5:
		out.STP = string(matching.STPAllow)
	}
	switch c.Kind {
	case tape.Submit:
		out.Kind = Submit
	case tape.Cancel:
		out.Kind = Cancel
	case tape.Reduce:
		out.Kind = Reduce
	case tape.Replace:
		out.Kind = Replace
	case tape.CancelAll:
		out.Kind = CancelAll
	case tape.Halt:
		out.Kind = Halt
	case tape.Resume:
		out.Kind = Resume
	case tape.CancelOnly:
		out.Kind = CancelOnly
	case tape.SetPhase:
		out.Kind = SetPhase
		out.Phase = uint8(c.Phase)
	}
	return out
}

// tierTwo is the hand-written script: the conditional order families, the mark price,
// busts and expiry.
//
// Nothing forces someone adding a new conditional behaviour to add a scenario here,
// and docs/SEMANTICS-VERSION.md §5.5 says so rather than leaving it for a review to
// find. That residual is the reason this list is short enough to read.
func tierTwo() []Cmd {
	var s scriptBuilder

	// A book to work against.
	askA := s.add(Cmd{Kind: Submit, User: "m1", Sell: true, Price: 105, Qty: 10})
	s.add(Cmd{Kind: Submit, User: "m1", Sell: true, Price: 106, Qty: 10})
	bidA := s.add(Cmd{Kind: Submit, User: "m2", Price: 95, Qty: 10})
	s.add(Cmd{Kind: Submit, User: "m2", Price: 94, Qty: 10})

	// A trade, so LastTradePrice is set and everything that keys off it is live.
	first := s.add(Cmd{Kind: Submit, User: "t1", Price: 105, Qty: 4, Note: "sets the last trade price"})

	// A bust of a print that stands, then a bust of the same print again, then a
	// bust of a trade id that never existed. Three outcomes, one command each.
	s.add(Cmd{Kind: Bust, TradeRef: first, TradeNth: 1})
	s.add(Cmd{Kind: Bust, TradeRef: first, TradeNth: 1, Note: "already busted"})
	s.add(Cmd{Kind: Bust, TradeRef: -1, TradeNth: 1, Note: "no such trade"})

	// The mark price: accepted, then cleared.
	s.add(Cmd{Kind: SetMark, MarkPrice: 104})
	s.add(Cmd{Kind: SetMark, MarkPrice: 0})

	// A stop that rests, and the trade that fires it. The trigger is the whole point:
	// a stop that is submitted and never fires proves nothing about triggering.
	s.add(Cmd{Kind: Stop, User: "s1", Sell: true, Price: 90, Qty: 3, StopPrice: 96,
		Note: "rests until the market trades down through 96"})
	s.add(Cmd{Kind: Submit, User: "t2", Sell: true, Price: 95, Qty: 2, Note: "prints at 95, firing the stop"})

	// A stop that fires immediately on submission, which is a different path.
	s.add(Cmd{Kind: Stop, User: "s2", Price: 110, Qty: 2, StopPrice: 90,
		Note: "already through its trigger"})

	// Stops sitting EXACTLY ON the last traded price, both sides. That price is the
	// only one at which the trigger COMPARISON is decidable, and a script whose stops
	// are always strictly through their trigger passes unchanged with >= relaxed to >
	// — a live behaviour change nothing would then report. This is the boundary
	// docs/SEMANTICS-VERSION.md §5.5 names, closed rather than left open, and it was
	// found by sabotage 13 rather than by inspection.
	//
	// The last traded price is pinned to 101 here rather than inherited from the
	// commands above, so an edit further up cannot quietly move the stops off the
	// boundary while every assertion still passes.
	s.add(Cmd{Kind: Submit, User: "b1", Sell: true, Price: 101, Qty: 2})
	s.add(Cmd{Kind: Submit, User: "b2", Price: 101, Qty: 2, Note: "pins the last traded price at 101"})
	s.add(Cmd{Kind: Stop, User: "s3", Sell: true, Price: 80, Qty: 1, StopPrice: 101,
		Note: "trigger equals the last traded price exactly: <= fires it, < rests it"})
	s.add(Cmd{Kind: Stop, User: "s4", Price: 120, Qty: 1, StopPrice: 101,
		Note: "the buy side of the same boundary"})

	// OCO in both directions: the primary filling must kill the stop leg, and the
	// stop firing must kill the primary.
	oco1 := s.add(Cmd{Kind: OCO, User: "o1", Sell: true, Price: 108, Qty: 5,
		StopPrice: 92, StopSell: true, StopPrice2: 88})
	s.add(Cmd{Kind: Submit, User: "t3", Price: 108, Qty: 5, Note: "fills the OCO primary"})
	s.add(Cmd{Kind: Cancel, User: "o1", Target: oco1, Note: "the pair is already gone"})

	oco2 := s.add(Cmd{Kind: OCO, User: "o2", Price: 80, Qty: 5,
		StopPrice: 107, StopSell: false, StopPrice2: 112})
	s.add(Cmd{Kind: Submit, User: "t4", Price: 107, Qty: 1, Note: "fires the OCO stop leg"})
	s.add(Cmd{Kind: Cancel, User: "o2", Target: oco2})

	// An iceberg, and enough taking to refill it more than once. A refill that never
	// happens leaves the reserve accounting untested.
	ice := s.add(Cmd{Kind: Iceberg, User: "i1", Sell: true, Price: 103, Qty: 12, DisplayQty: 3})
	s.add(Cmd{Kind: Submit, User: "t5", Price: 103, Qty: 3, Note: "exhausts the displayed slice"})
	s.add(Cmd{Kind: Submit, User: "t6", Price: 103, Qty: 3, Note: "and the refill"})
	s.add(Cmd{Kind: Submit, User: "t7", Price: 103, Qty: 2})
	s.add(Cmd{Kind: Cancel, User: "i1", Target: ice})

	// Pegged orders against all three references, which is the only place PegToMid
	// and PegToAsk are reached at all.
	s.add(Cmd{Kind: Pegged, User: "p1", Price: 1, Qty: 4, PegRef: string(types.PegToBid), PegOffset: -1})
	s.add(Cmd{Kind: Pegged, User: "p2", Sell: true, Price: 1, Qty: 4, PegRef: string(types.PegToAsk), PegOffset: 1})
	s.add(Cmd{Kind: Pegged, User: "p3", Price: 1, Qty: 4, PegRef: string(types.PegToMid), PegOffset: 0})

	// A trailing stop, and the ratchet: the market has to move the right way and then
	// back through the trail for the stop to fire.
	s.add(Cmd{Kind: Trailing, User: "r1", Sell: true, Price: 1, Qty: 3, Trail: 4})
	s.add(Cmd{Kind: Submit, User: "t8", Sell: true, Price: 106, Qty: 1})
	s.add(Cmd{Kind: Submit, User: "t9", Price: 106, Qty: 1, Note: "ratchets the trail up"})
	s.add(Cmd{Kind: Submit, User: "m3", Price: 99, Qty: 6})
	s.add(Cmd{Kind: Submit, User: "t10", Sell: true, Price: 99, Qty: 1, Note: "trades back through the trail"})

	// Expiry: a DAY order (needs the session close), a GTD order, and the sweep.
	s.add(Cmd{Kind: Submit, User: "e1", Price: 90, Qty: 2, TIF: types.TIFDay})
	s.add(Cmd{Kind: Submit, User: "e2", Price: 89, Qty: 2, TIF: types.TIFGoodTillDate, ExpiresIn: time.Minute})
	s.add(Cmd{Kind: Submit, User: "e3", Price: 88, Qty: 2, TIF: types.TIFGoodTillDate, ExpiresIn: -time.Minute,
		Note: "already past its deadline"})
	s.add(Cmd{Kind: ExpireDue})

	// The duplicate client-order-id guard: the same key twice.
	s.add(Cmd{Kind: Submit, User: "d1", Price: 93, Qty: 1, ClientOrderID: "abc"})
	s.add(Cmd{Kind: Submit, User: "d1", Price: 93, Qty: 1, ClientOrderID: "abc", Note: "duplicate"})

	// Market, IOC and FOK against a known book, plus post-only crossing.
	s.add(Cmd{Kind: Submit, User: "x1", Type: types.OrderTypeMarket, Qty: 2})
	s.add(Cmd{Kind: Submit, User: "x2", Sell: true, Price: 1, Qty: 3, TIF: types.TIFImmediateOrCancel})
	s.add(Cmd{Kind: Submit, User: "x3", Price: 200, Qty: 500, TIF: types.TIFFillOrKill,
		Note: "cannot fill; the rejected print must not move the last price"})
	s.add(Cmd{Kind: Submit, User: "x4", Sell: true, Price: 90, Qty: 1, PostOnly: true, Note: "would cross"})

	// All five self-trade-prevention modes, one command each, against the account's
	// own resting liquidity — the path pro-rata changed.
	for _, mode := range []matching.SelfTradePrevention{
		matching.STPCancelNewest, matching.STPCancelOldest,
		matching.STPCancelBoth, matching.STPDecrement, matching.STPAllow,
	} {
		u := "stp" + string(mode)[:3]
		s.add(Cmd{Kind: Submit, User: u, Sell: true, Price: 101, Qty: 4, STP: string(mode)})
		s.add(Cmd{Kind: Submit, User: u, Price: 101, Qty: 4, STP: string(mode)})
	}

	// The venue's own controls, so HALTED, RESUMED and CANCEL_ONLY are published.
	s.add(Cmd{Kind: Halt})
	s.add(Cmd{Kind: Submit, User: "h1", Price: 96, Qty: 1, Note: "refused while halted"})
	s.add(Cmd{Kind: Resume})
	s.add(Cmd{Kind: CancelOnly})
	s.add(Cmd{Kind: Submit, User: "h2", Price: 96, Qty: 1, Note: "refused in cancel-only"})
	s.add(Cmd{Kind: Cancel, User: "m2", Target: bidA, Note: "cancels are still accepted"})
	s.add(Cmd{Kind: Resume})

	// A phase round trip, so the uncross runs against a book that already has depth.
	s.add(Cmd{Kind: SetPhase, Phase: 1}) // pre-open
	s.add(Cmd{Kind: Submit, User: "a1", Price: 104, Qty: 6})
	s.add(Cmd{Kind: Submit, User: "a2", Sell: true, Price: 100, Qty: 6})
	s.add(Cmd{Kind: SetPhase, Phase: 0}) // open: the uncross prints
	s.add(Cmd{Kind: SetPhase, Phase: 2}) // closing auction
	s.add(Cmd{Kind: Submit, User: "a3", Price: 107, Qty: 3})
	s.add(Cmd{Kind: Submit, User: "a4", Sell: true, Price: 102, Qty: 3})
	s.add(Cmd{Kind: SetPhase, Phase: 0})
	s.add(Cmd{Kind: SetPhase, Phase: 5}) // closed
	s.add(Cmd{Kind: Submit, User: "z1", Price: 96, Qty: 1, Note: "refused after the close"})
	s.add(Cmd{Kind: SetPhase, Phase: 3}) // halted, reached as a phase rather than a command
	s.add(Cmd{Kind: SetPhase, Phase: 4}) // cancel-only, likewise
	s.add(Cmd{Kind: SetPhase, Phase: 0})

	// A replace that keeps queue position and one that is refused, plus a cancel-all.
	rep := s.add(Cmd{Kind: Submit, User: "q1", Price: 97, Qty: 8})
	s.add(Cmd{Kind: Replace, User: "q1", Target: rep, Price: 97, Qty: 4})
	s.add(Cmd{Kind: Replace, User: "q2", Target: rep, Price: 97, Qty: 2, Note: "not the owner"})
	s.add(Cmd{Kind: Reduce, User: "q1", Target: rep, NewQty: 1})
	s.add(Cmd{Kind: CancelAll, User: "q1"})
	s.add(Cmd{Kind: Cancel, User: "m1", Target: askA})

	// APPENDED, NEVER INSERTED. An insertion mid-script shifts every later line's
	// order id, event id and digest and turns a reviewable diff into a thousand-line
	// rewrite nobody reads. Appended, the diff is legible.
	//
	// These seven close the two blind spots docs/PINNED-DEFECTS.md §6.1 measured:
	// the script above reaches icebergs and reaches stops, but no fill-or-kill in it
	// ever meets an iceberg and no stop it fires ever fails — so neither of the two
	// defects that slice fixed was in the fingerprint, and under Rule 22 neither
	// could have been bumped for.

	// A fill-or-kill that exhausts an iceberg's reserve and then fails.
	ice2 := s.add(Cmd{Kind: Iceberg, User: "i2", Sell: true, Price: 102, Qty: 9, DisplayQty: 3})
	s.add(Cmd{Kind: Submit, User: "t11", Price: 102, Qty: 20, TIF: types.TIFFillOrKill,
		Note: "consumes every slice, cannot fill, and is reversed"})
	// Not padding: this is the assertion that the RESTORED RESERVE STILL WORKS, and
	// it is the line that moves if a restore repairs the numbers and leaves the
	// iceberg deregistered.
	s.add(Cmd{Kind: Submit, User: "t12", Price: 102, Qty: 3, Note: "the restored reserve still refills"})
	s.add(Cmd{Kind: Cancel, User: "i2", Target: ice2})

	// A stop fired by a CASCADE whose own order is then refused. The stop paths above
	// all fire on arrival or fire and fill; this is the one that fires from inside
	// another command's walk and is then turned away.
	s.add(Cmd{Kind: Stop, User: "s5", Price: 200, Qty: 50, TIF: types.TIFFillOrKill, StopPrice: 105,
		Note: "its own order cannot fill"})
	s.add(Cmd{Kind: Submit, User: "k1", Sell: true, Price: 105, Qty: 1})
	s.add(Cmd{Kind: Submit, User: "k2", Price: 105, Qty: 1, Note: "prints at 105, firing a stop that is refused"})

	// And the QUEUE ORDER a refusal must not rewrite, which the four lines above
	// cannot see: i2 is alone at 102, so any restore order at all produces the same
	// digest, and a review of the same change found the ordering wrong with this
	// fingerprint green. Here an older maker rests in FRONT of the iceberg at one
	// price, a fill-or-kill takes them both and then fails, and the one-lot buy after
	// it names the maker that kept priority — visible on the line as `m<id>`, and in
	// the snapshot digest, which carries the book in price-then-time order.
	// docs/PINNED-DEFECTS.md §13.6.
	qFirst := s.add(Cmd{Kind: Submit, User: "q9", Sell: true, Price: 103, Qty: 5, Note: "rests ahead of the iceberg"})
	ice3 := s.add(Cmd{Kind: Iceberg, User: "i3", Sell: true, Price: 103, Qty: 9, DisplayQty: 3})
	s.add(Cmd{Kind: Submit, User: "t13", Price: 103, Qty: 20, TIF: types.TIFFillOrKill,
		Note: "takes the maker and every slice, cannot fill, and is reversed"})
	s.add(Cmd{Kind: Submit, User: "t14", Price: 103, Qty: 1, Note: "prints against whichever order kept priority"})
	s.add(Cmd{Kind: Cancel, User: "q9", Target: qFirst})
	s.add(Cmd{Kind: Cancel, User: "i3", Target: ice3})

	return s.cmds
}

// guarded is the admission-control script. Every one of these knobs changes a VERDICT,
// so each is a semantics surface of its own, and none of the generated profiles turns
// any of them on.
func guarded() []Cmd {
	var s scriptBuilder
	s.add(Cmd{Kind: Submit, User: "g1", Sell: true, Price: 100, Qty: 5})
	s.add(Cmd{Kind: Submit, User: "g2", Price: 100, Qty: 5, Note: "sets the band reference"})
	s.add(Cmd{Kind: Submit, User: "g3", Price: 150, Qty: 2, Note: "outside a ±10% band"})
	privBid := s.add(Cmd{Kind: Submit, User: "g3", Price: 150, Qty: 2, Privileged: true, Note: "privileged: exempt"})
	s.add(Cmd{Kind: Submit, User: "g4", Price: 99, Qty: 100, Note: "over MaxOrderQty"})
	s.add(Cmd{Kind: Submit, User: "g4", Price: 99, Qty: 1, Note: "under MinOrderQty"})
	s.add(Cmd{Kind: Submit, User: "g5", Price: 98, Qty: 2})
	s.add(Cmd{Kind: Submit, User: "g5", Price: 97, Qty: 2})
	s.add(Cmd{Kind: Submit, User: "g5", Price: 96, Qty: 2})
	s.add(Cmd{Kind: Submit, User: "g5", Price: 95, Qty: 2, Note: "over MaxOrdersPerAccount"})
	s.add(Cmd{Kind: CancelAll, User: "g5"})

	// APPENDED, NEVER INSERTED, for the reason tierTwo's own appendix gives: an
	// insertion shifts every later id, event id and digest and turns a reviewable
	// diff into a rewrite nobody reads.
	//
	// These close the blind spot docs/ICEBERG-ADMISSION.md §7 measured. `conditional`
	// was the only scenario with icebergs and it configures no caps; `guarded` was the
	// only scenario with caps and it had no iceberg. So no scenario had both, the
	// golden was byte-identical after a change to what the CAPS MEASURE, and under
	// Rule 22 nobody could have bumped for it — docs/PINNED-DEFECTS.md §6.1 recurring
	// in the same file, one scenario over.
	//
	// The caps are already configured here, so no existing verdict moves.
	//
	// The cancel first is plumbing and not a case: g3's privileged bid is still
	// resting at 150, and every iceberg below is a sell that would cross it, print at
	// 150 and drag the ±10% band reference up with it — after which the cases would
	// be refused for the BAND rather than decided by the caps, which is not what they
	// are here to measure.
	s.add(Cmd{Kind: Cancel, User: "g3", Target: privBid, Note: "clears the band reference's only maker"})

	// Over the cap by its TOTAL and under it by its slice: the fat-finger reject a
	// client used to switch off by choosing displayQty.
	s.add(Cmd{Kind: Iceberg, User: "g6", Sell: true, Price: 100, Qty: 100, DisplayQty: 3,
		Note: "100 lots over MaxOrderQty=40, shown 3"})
	// The control that must NOT move: a genuinely tiny iceberg is still dust, under
	// either rule. A fix that measured anything but the total would move this line.
	s.add(Cmd{Kind: Iceberg, User: "g6", Sell: true, Price: 100, Qty: 1, DisplayQty: 1,
		Note: "one lot, under MinOrderQty=2 by its total AND its slice"})
	// Thirty lots shown one: refused as dust before, admitted for what it is now,
	// and a one-lot slice rests BELOW the floor because the floor judged the order.
	s.add(Cmd{Kind: Iceberg, User: "g6", Sell: true, Price: 100, Qty: 30, DisplayQty: 1,
		Note: "thirty lots shown one, fifteen times over the dust floor"})
	// A taker, so the refill path runs under a configured floor: every refilled
	// slice is smaller than MinOrderQty and none of them is re-admitted.
	s.add(Cmd{Kind: Submit, User: "g7", Price: 100, Qty: 10, Note: "works the reserve, refilling below the floor"})
	// The boundary, and the second control that must not move: exactly at the cap.
	s.add(Cmd{Kind: Iceberg, User: "g6", Sell: true, Price: 101, Qty: 40, DisplayQty: 5,
		Note: "exactly MaxOrderQty"})
	// One over it.
	s.add(Cmd{Kind: Iceberg, User: "g6", Sell: true, Price: 102, Qty: 41, DisplayQty: 5,
		Note: "one lot over MaxOrderQty"})
	s.add(Cmd{Kind: CancelAll, User: "g6"})
	return s.cmds
}

// scriptBuilder keeps Pos and the position a later command names in agreement, which
// is the one thing a hand-written script gets wrong when it is edited.
type scriptBuilder struct{ cmds []Cmd }

func (s *scriptBuilder) add(c Cmd) int {
	c.Pos = len(s.cmds)
	s.cmds = append(s.cmds, c)
	return c.Pos
}

// pct is a percentage as the decimal fraction the engine's collar knobs take.
func pct(n int) decimal.Decimal {
	return decimal.RequireFromString(fmt.Sprintf("0.%02d", n))
}

// notionalGuarded is the notional admission script. Every command in it is decided
// by MinOrderNotional, MaxOrderNotional or the int64 overflow guard, which is the
// point: docs/ICEBERG-ADMISSION.md §3.2 applies one rule to five checks, and before
// this scenario the fingerprint could see two of them.
//
// The three plain orders first are the CONTROLS. Each iceberg case below is the same
// notional as one of them, and the rule under test is that the two get the same
// verdict — an iceberg is not a different kind of order to the venue's risk limits,
// it is a different way of showing one.
func notionalGuarded() []Cmd {
	var s scriptBuilder

	// Controls: a plain order inside the window, one under the floor, one over the
	// ceiling. Prices are 100, so notional is a hundred times the lot count.
	s.add(Cmd{Kind: Submit, User: "n1", Sell: true, Price: 100, Qty: 9, Note: "900 of notional, inside [500, 5000]"})
	s.add(Cmd{Kind: Submit, User: "n1", Sell: true, Price: 100, Qty: 4, Note: "400, under MinOrderNotional"})
	s.add(Cmd{Kind: Submit, User: "n1", Sell: true, Price: 100, Qty: 51, Note: "5100, over MaxOrderNotional"})

	// MaxOrderNotional evaded by the display size: sixty lots at 100 is 6000 and does
	// not fit, and a slice of ten is 1000 and does fit. This line is REJECTED because
	// the cap weighs the client's total; weighed against the slice it RESTED.
	s.add(Cmd{Kind: Iceberg, User: "n2", Sell: true, Price: 100, Qty: 60, DisplayQty: 10,
		Note: "6000 total over MaxOrderNotional, 1000 slice under it"})

	// MinOrderNotional refusing real size: fifty lots at 100 is 5000 — exactly the
	// ceiling and ten times the floor — shown three lots, which is 300 and is dust.
	// This line is ACCEPTED because the floor weighs the client's total; weighed
	// against the slice it was REFUSED. It doubles as the at-the-ceiling boundary.
	s.add(Cmd{Kind: Iceberg, User: "n2", Sell: true, Price: 100, Qty: 50, DisplayQty: 3,
		Note: "5000 total exactly at MaxOrderNotional, 300 slice under MinOrderNotional"})

	// One lot over the ceiling, so the boundary is pinned from both sides.
	s.add(Cmd{Kind: Iceberg, User: "n2", Sell: true, Price: 100, Qty: 51, DisplayQty: 3,
		Note: "5100, one lot over MaxOrderNotional"})

	// The control that must NOT move: dust by its total AND by its slice, refused
	// under either rule. A fix that measured anything other than the client's total
	// would move this line, and a fix that measured nothing would move it too.
	s.add(Cmd{Kind: Iceberg, User: "n2", Sell: true, Price: 100, Qty: 4, DisplayQty: 2,
		Note: "400 total and 200 slice, under MinOrderNotional either way"})

	// THE OVERFLOW GUARD, which is the reason this scenario is worth its diff. It has
	// no Config knob, Privileged orders do not bypass it, and checkOrderCaps's own
	// comment calls it an arithmetic invariant rather than ingress policy — a corrupt
	// or hand-edited log entry must not replay a notional that wraps int64 into the
	// book. Measured against the displayed slice it was CLIENT-SELECTABLE: price ×
	// 10 is 1000 and passes, so this order RESTED, and price × its real size does not
	// fit in an int64 at all.
	//
	// §12 feared such an order would "dominate every aggregate in the golden". It
	// does not, and that is a fact about rejection rather than about the number: the
	// order never rests and never prints, so it contributes one line, one REJECTED
	// event and one rejection counter, exactly like every other refusal here.
	s.add(Cmd{Kind: Iceberg, User: "n3", Sell: true, Price: 100, Qty: 92233720368547759, DisplayQty: 10,
		Note: "price x total overflows int64, price x slice is 1000 and does not"})

	// A taker that works the accepted reserve down. It clears n1's resting nine
	// first — the two are at the same price and the plain order is in front — and
	// then takes five slices of the iceberg. Every one of those slices is 300 of
	// notional, well under the floor, and not one is re-admitted: Rule 3 for the
	// notional pair, which the quantity cases in `guarded` cannot show.
	s.add(Cmd{Kind: Submit, User: "n4", Price: 100, Qty: 24, Note: "clears the plain nine, then five slices of 300 notional refill below the floor"})

	s.add(Cmd{Kind: CancelAll, User: "n2"})
	return s.cmds
}
