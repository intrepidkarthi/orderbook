package matching

// The differential harness: one generated tape driven through the optimised engine
// and through internal/refmatch, compared after EVERY command.
//
// This file is the only place in the repository allowed to know both vocabularies.
// internal/refmatch declares its own enums and its own order and trade structs
// precisely so it cannot inherit the production semantics it is supposed to be
// checking, and the translation between the two has to live somewhere. It lives
// here, in one reviewable file, rather than spread thinly — spreading it thinly is
// how independence is lost without anyone deciding to lose it.
//
// Equivalence is defined in docs/REFERENCE-MATCHER.md §3 and implemented as a single
// refmatch.Observation value produced by both sides and compared WHOLE with
// reflect.DeepEqual. Not field by field: a field-list comparator is exactly where a
// future field silently stops being compared.
//
// Three things are permitted to differ, and no more. Wall-clock timestamps, because
// the model has no clock and every engine path that reads one FOR A DECISION is
// behind a Config knob that TestHarnessConfigMatchesItsClassification forces to its
// disabling value. Event.Seq values, because the model does not assign them and
// TestEventSequenceIsDenseAndMonotonic plus an elementwise event comparison leave
// exactly one possible value per event. Internal representation, because
// TestDifferentialTape compares a snapshot-restore digest after every command.
// Trade.Symbol and Trade.Auction are deliberately NOT on that list: they are
// constant in tier 1 and they are ASSERTED constant, by being in the compared value
// with the model emitting the constant.

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/refmatch"
	"github.com/intrepidkarthi/orderbook/internal/tape"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// --- the harness configuration ----------------------------------------------

const diffSymbol = "DIFF"

// diffClock is a deterministic step clock. The engine side has to be deterministic
// by construction, and TestHarnessClockIsDeterministic is what holds it to that.
//
// docs/REFERENCE-MATCHER.md §8 row 9 expected TestSameSeedSameObservations to catch
// this clock being replaced with time.Now, and it does not — the sabotage was run and
// the suite stayed green. The reason is the design working, not failing: no timestamp
// is in the Observation and, with every clock-reading Config knob at its disabling
// value, no tier-1 verdict depends on what the clock said, so a live clock cannot
// make an observation stream differ. Row 9 named the wrong guard. This is the right
// one, and it asserts the property directly instead of hoping a downstream
// comparison notices.
func diffClock() func() time.Time {
	base := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	var n int64
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Millisecond)
	}
}

// diffProfile pairs a tape alphabet with the engine and model configurations it is
// driven against.
type diffProfile struct {
	name  string
	tapes tape.Profile
	seeds []uint64
	n     int
	// engine builds the engine configuration. It must stay inside the
	// classification in differential_config_test.go.
	engine func() Config
	model  refmatch.Config
}

// The pro-rata profile used to carry a crossingIsExpected field: a written reason
// the profile could legitimately leave the book crossed, under which runDiff swapped
// checkInvariants' crossed-book assertion for a narrower one. It is gone, and its
// deletion is the point rather than a tidy-up. The narrowing was correct while
// pro-rata skipped a taker's own liquidity and rested the remainder across the
// spread; docs/DIFFERENTIAL-FINDINGS.md §5 deleted the skip, so every profile now
// runs the same full-strength invariant suite. A narrowing left in place after its
// reason is gone is a hole.

// diffProfiles is the committed sweep. It is a fixed set of seeds rather than a
// fuzz campaign so it runs inside `go test ./...` without anyone thinking about it;
// FuzzDifferential is the unbounded half.
func diffProfiles() []diffProfile {
	return []diffProfile{
		{
			// Price-time allocation, shard 0. The baseline.
			name:  "fifo",
			tapes: tape.Differential,
			seeds: []uint64{1, 2, 3, 5, 8, 13, 21, 34},
			n:     140,
			engine: func() Config {
				c := DefaultConfig(diffSymbol)
				c.Clock = diffClock()
				return c
			},
			model: refmatch.Config{Symbol: diffSymbol, STP: refmatch.CancelNewest, MaxOrders: 100_000},
		},
		{
			// Pro-rata allocation, and a NON-ZERO shard index. Shard 0 composes an id
			// to the sequence itself, so a sweep that only ever ran shard 0 would let
			// a broken id composition pass.
			name:  "prorata-shard7",
			tapes: tape.ProRata,
			seeds: []uint64{1, 4, 9, 16, 25},
			n:     140,
			engine: func() Config {
				c := DefaultConfig(diffSymbol)
				c.Clock = diffClock()
				c.ProRata = true
				c.ShardIndex = 7
				return c
			},
			model: refmatch.Config{Symbol: diffSymbol, STP: refmatch.CancelNewest, MaxOrders: 100_000, ProRata: true, ShardIndex: 7},
		},
		{
			// A book-size cap small enough to be REACHED, on shard 3. MaxOrders
			// changes a verdict — an order that has already traded and then finds no
			// room is rejected with its prints standing — so the model implements it,
			// and a profile that never hits the cap would not be exercising that.
			name:  "capped-shard3",
			tapes: tape.Differential,
			seeds: []uint64{7, 11, 42},
			n:     140,
			engine: func() Config {
				c := DefaultConfig(diffSymbol)
				c.Clock = diffClock()
				c.MaxOrders = 8
				c.SelfTradePrevention = STPDecrement
				c.ShardIndex = 3
				return c
			},
			model: refmatch.Config{Symbol: diffSymbol, STP: refmatch.Decrement, MaxOrders: 8, ShardIndex: 3},
		},
	}
}

// --- vocabulary translation ---------------------------------------------------

func mapSide(s types.Side) refmatch.Side {
	if s == types.SideSell {
		return refmatch.Sell
	}
	return refmatch.Buy
}

func mapStatus(s types.OrderStatus) (refmatch.Status, bool) {
	switch s {
	case types.OrderStatusNew:
		return refmatch.StatusNew, true
	case types.OrderStatusPartiallyFilled:
		return refmatch.StatusPartiallyFilled, true
	case types.OrderStatusFilled:
		return refmatch.StatusFilled, true
	case types.OrderStatusCancelled:
		return refmatch.StatusCancelled, true
	case types.OrderStatusRejected:
		return refmatch.StatusRejected, true
	}
	// PendingTrigger is a stop's status and stops are tier 2. Reaching it means the
	// tier boundary is wrong, which is a finding, not a case to add quietly.
	return 0, false
}

func mapEngineState(s EngineState) (refmatch.State, bool) {
	switch s {
	case StateOpen:
		return refmatch.Open, true
	case StateCancelOnly:
		return refmatch.CancelOnlyState, true
	case StateHalted:
		return refmatch.Halted, true
	}
	return 0, false
}

func mapEventKind(k EventKind) (refmatch.EventKind, bool) {
	switch k {
	case EventAccepted:
		return refmatch.EvAccepted, true
	case EventRejected:
		return refmatch.EvRejected, true
	case EventTrade:
		return refmatch.EvTrade, true
	case EventCanceled:
		return refmatch.EvCanceled, true
	case EventReplaced:
		return refmatch.EvReplaced, true
	case EventHalted:
		return refmatch.EvHalted, true
	case EventResumed:
		return refmatch.EvResumed, true
	case EventCancelOnly:
		return refmatch.EvCancelOnly, true
	}
	return 0, false
}

// tierOneRejections is every rejection reason reachable on the tier-1 path, with the
// enum it maps to. Error strings are not a contract; which sentinel came back is.
//
// The map is what TestEveryTierOneRejectionIsMapped asserts totality over. There is
// deliberately no catch-all "other" enum: a new rejection reason arriving as
// "unknown, therefore equal" would turn the oracle off for exactly the commands it
// was added for.
var tierOneRejections = map[error]refmatch.Reject{
	types.ErrTradingHalted:          refmatch.RejectTradingHalted,
	types.ErrNewOrdersHalted:        refmatch.RejectNewOrdersHalted,
	types.ErrPostOnlyWouldCross:     refmatch.RejectPostOnlyWouldCross,
	types.ErrFOKCannotFill:          refmatch.RejectFOKCannotFill,
	types.ErrMarketOrderNoLiquidity: refmatch.RejectMarketNoLiquidity,
	types.ErrOrderBookFull:          refmatch.RejectOrderBookFull,
	types.ErrOrderNotFound:          refmatch.RejectOrderNotFound,
	types.ErrOrderNotActive:         refmatch.RejectOrderNotActive,
	types.ErrInvalidQuantity:        refmatch.RejectInvalidQuantity,
	types.ErrNilOrder:               refmatch.RejectNilOrder,
	types.ErrNotionalOverflow:       refmatch.RejectNotionalOverflow,
}

func mapReject(err error) (refmatch.Reject, bool) {
	if err == nil {
		return refmatch.RejectNone, true
	}
	for sentinel, r := range tierOneRejections {
		if errors.Is(err, sentinel) {
			return r, true
		}
	}
	return 0, false
}

// --- the engine side ----------------------------------------------------------

// eventRecorder turns the engine's event stream into the model's vocabulary,
// keeping the sequence numbers on the side so a separate assertion can pin them.
type eventRecorder struct {
	cur  []refmatch.Event
	seqs []int64
	err  string
}

func (r *eventRecorder) OnEvents(evs []Event) {
	for _, e := range evs {
		r.seqs = append(r.seqs, e.Seq)
		k, ok := mapEventKind(e.Kind)
		if !ok {
			r.err = fmt.Sprintf("event kind %s is outside the tier-1 alphabet", e.Kind)
			continue
		}
		reason, ok := mapReject(e.Reason)
		if !ok {
			r.err = fmt.Sprintf("event carries unmapped reason %v", e.Reason)
			continue
		}
		ev := refmatch.Event{Kind: k, OrderID: e.OrderID, User: e.UserID, Reason: reason}
		// The PAYLOAD of a trade event, taken from the event itself rather than from
		// MatchResult.Trades. Those are two different sources for the same numbers,
		// and until this line only the second was compared — so a corrupted payload
		// reaching subscribers was invisible here.
		if e.Kind == EventTrade {
			if e.Trade == nil {
				r.err = "a trade event carries no trade"
				continue
			}
			ev.Price, ev.Qty = e.Trade.Price, e.Trade.Quantity
		}
		r.cur = append(r.cur, ev)
	}
}

func (r *eventRecorder) take() []refmatch.Event {
	out := r.cur
	r.cur = nil
	if out == nil {
		return []refmatch.Event{}
	}
	return out
}

type engineSide struct {
	e   *Engine
	rec *eventRecorder
	cfg Config
	// mirror rebuilds an L3 book from nothing but the event stream — the same
	// mirrorBook TestEventStreamReconstructsBook drives, attached to the GENERATED
	// tape instead of to a scenario list.
	//
	// It is here because the claim on EventKind's doc comment ("Accepted / Trade /
	// Canceled / Replaced reconstruct the L3 book") was FALSE while being proved
	// true, over ~25 hand-written scenarios none of which combined fill-or-kill with
	// self-trade prevention. That is an exhaustive check over an incomplete input
	// space reporting completeness, with the report load-bearing —
	// docs/JOURNAL-COMPLETENESS.md §1 in a second place. This assertion is what
	// would have caught defect B without anyone predicting it, and it is deliberately
	// NOT a consequence of the model comparison: the model canonised the engine's
	// dropped batch, so the two agreed and both were wrong.
	mirror *mirrorBook
	// ids maps a tape POSITION to the order id that position was given. A command
	// that names a position which produced no order gets id 0, which is an id the
	// engine never issues, so both sides answer "not found".
	ids map[int]int64
}

func newEngineSide(cfg Config) *engineSide {
	rec := &eventRecorder{}
	mirror := newMirror()
	cfg.EventSink = MultiSink{rec, mirror}
	return &engineSide{e: NewEngine(cfg), rec: rec, mirror: mirror, cfg: cfg, ids: map[int]int64{}}
}

// mirrorMatches compares the book rebuilt from the event stream against the
// engine's own, returning the empty string when they agree.
func (s *engineSide) mirrorMatches() string {
	if s.mirror == nil {
		return "" // a side restored from a snapshot; its stream starts mid-book
	}
	if len(s.mirror.gaps) > 0 {
		return fmt.Sprintf("the event stream is not self-describing: %v", s.mirror.gaps)
	}
	got, want := s.mirror.resting(), engineResting(s.e)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		return fmt.Sprintf("a book rebuilt from the event stream alone differs from the engine's own — "+
			"every consumer built on the stream is wrong in the same way\n  from the stream: %v\n"+
			"  engine book:     %v", got, want)
	}
	return ""
}

func (s *engineSide) apply(c tape.Cmd) (refmatch.Verdict, []refmatch.Trade, error) {
	switch c.Kind {
	case tape.Submit:
		o, err := s.buildOrder(c)
		if err != nil {
			return refmatch.Verdict{}, nil, err
		}
		res := s.e.Process(o)
		s.ids[c.Pos] = res.Order.ID
		return s.verdict(res)

	case tape.Cancel:
		_, err := s.e.Cancel(s.ids[c.Target], c.User)
		return s.simple(refmatch.StatusCancelled, err)

	case tape.Reduce:
		_, err := s.e.Reduce(s.ids[c.Target], c.NewQty, c.User)
		return s.simple(refmatch.StatusReduced, err)

	case tape.Replace:
		repl, err := s.buildOrder(c)
		if err != nil {
			return refmatch.Verdict{}, nil, err
		}
		res, rerr := s.e.Replace(s.ids[c.Target], c.User, repl)
		if rerr != nil {
			// The replacement never reached the engine, so it consumed no id.
			r, ok := mapReject(rerr)
			if !ok {
				return refmatch.Verdict{}, nil, fmt.Errorf("unmapped replace rejection %v", rerr)
			}
			return refmatch.Verdict{Status: refmatch.StatusRejected, Reason: r}, nil, nil
		}
		s.ids[c.Pos] = res.Order.ID
		return s.verdict(res)

	case tape.CancelAll:
		s.e.CancelAllForUser(c.User)
		return refmatch.Verdict{Status: refmatch.StatusCancelled}, nil, nil

	case tape.Halt:
		s.e.Halt()
		return refmatch.Verdict{}, nil, nil

	case tape.Resume:
		s.e.Resume()
		return refmatch.Verdict{}, nil, nil

	case tape.CancelOnly:
		s.e.SetCancelOnly()
		return refmatch.Verdict{}, nil, nil
	}
	return refmatch.Verdict{}, nil, fmt.Errorf("tape kind %s is not on the tier-1 engine driver", c.Kind)
}

func (s *engineSide) simple(ok refmatch.Status, err error) (refmatch.Verdict, []refmatch.Trade, error) {
	if err == nil {
		return refmatch.Verdict{Status: ok}, nil, nil
	}
	r, mapped := mapReject(err)
	if !mapped {
		return refmatch.Verdict{}, nil, fmt.Errorf("unmapped rejection %v", err)
	}
	return refmatch.Verdict{Status: refmatch.StatusRejected, Reason: r}, nil, nil
}

func (s *engineSide) verdict(res *MatchResult) (refmatch.Verdict, []refmatch.Trade, error) {
	st, ok := mapStatus(res.Status)
	if !ok {
		return refmatch.Verdict{}, nil, fmt.Errorf("status %s is outside the tier-1 alphabet", res.Status)
	}
	r, ok := mapReject(res.RejectionReason)
	if !ok {
		return refmatch.Verdict{}, nil, fmt.Errorf("unmapped rejection %v", res.RejectionReason)
	}
	out := make([]refmatch.Trade, 0, len(res.Trades))
	for _, t := range res.Trades {
		out = append(out, refmatch.Trade{
			ID: t.ID, Symbol: t.Symbol, Price: t.Price, Qty: t.Quantity,
			BuyOrderID: t.BuyOrderID, SellOrderID: t.SellOrderID,
			BuyerUser: t.BuyerUserID, SellerUser: t.SellerUserID,
			MakerOrderID: t.MakerOrderID, TakerOrderID: t.TakerOrderID,
			TakerSide: mapSide(t.TakerSide), Seq: t.SequenceNum, Auction: t.Auction,
		})
	}
	return refmatch.Verdict{Status: st, Reason: r}, out, nil
}

func (s *engineSide) buildOrder(c tape.Cmd) (*types.Order, error) {
	side := types.SideBuy
	if c.Sell {
		side = types.SideSell
	}
	ot := types.OrderTypeLimit
	if c.MarketOrd {
		ot = types.OrderTypeMarket
	}
	tif := types.TIFGoodTillCancel
	switch c.TIF {
	case 1:
		tif = types.TIFImmediateOrCancel
	case 2:
		tif = types.TIFFillOrKill
	}
	o, err := types.NewOrder(c.User, diffSymbol, side, ot, c.Price, c.Qty, tif)
	if err != nil {
		return nil, fmt.Errorf("tape drew an order the type layer refuses: %w", err)
	}
	o.PostOnly = c.PostOnly
	o.TradeGroupID = c.TradeGroup
	o.Privileged = c.Privileged
	switch c.STP {
	case 1:
		o.STPMode = string(STPCancelNewest)
	case 2:
		o.STPMode = string(STPCancelOldest)
	case 3:
		o.STPMode = string(STPCancelBoth)
	case 4:
		o.STPMode = string(STPDecrement)
	case 5:
		o.STPMode = string(STPAllow)
	}
	return o, nil
}

// engineState folds an engine's VISIBLE state — everything a market-data consumer
// or a restarted process can see — into the state-bearing half of an Observation.
// The command-bearing half (verdict, trades, events) is left zero.
//
// It is a free function on *Engine rather than a method on engineSide because the
// RESTORED engine has to go through exactly this path too, and a restored engine has
// no tape driver. That is not a refactor for tidiness: see restoreMatchesLive.
//
// Levels come from the book's MAINTAINED aggregate; the model's come from a sum of
// its L3. That asymmetry is the whole value of the level assertion and "simplifying"
// it to two sums would destroy it.
func engineState(e *Engine, cfg Config, snap *EngineSnapshot) (refmatch.Observation, error) {
	st, ok := mapEngineState(e.State())
	if !ok {
		return refmatch.Observation{}, fmt.Errorf("engine state %s is outside the tier-1 alphabet", e.State())
	}
	book := make([]refmatch.Resting, 0, e.OrderCount())
	for _, o := range e.Book().Orders() {
		book = append(book, refmatch.Resting{
			ID: o.ID, User: o.UserID, Side: mapSide(o.Side), Price: o.Price,
			Qty: o.Quantity, Filled: o.FilledQty, Remaining: o.RemainingQty, PostOnly: o.PostOnly,
		})
	}
	const allLevels = 256
	levels := make([]refmatch.Level, 0, 8)
	for _, l := range e.Book().GetBidLevels(allLevels) {
		levels = append(levels, refmatch.Level{Side: refmatch.Buy, Price: l.Price, TotalQty: l.TotalQty, Count: l.Count()})
	}
	for _, l := range e.Book().GetAskLevels(allLevels) {
		levels = append(levels, refmatch.Level{Side: refmatch.Sell, Price: l.Price, TotalQty: l.TotalQty, Count: l.Count()})
	}
	return refmatch.Observation{
		Book:           book,
		Levels:         levels,
		State:          st,
		LastTradePrice: e.LastTradePrice(),
		// The counters are observed rather than inferred, so a divergence in id
		// allocation is caught at the command that causes it rather than at the
		// next submit.
		NextOrderID: ComposeID(cfg.ShardIndex, snap.Seq+1),
		NextTradeID: ComposeID(cfg.ShardIndex, snap.TradeSeq+1),
	}, nil
}

// observe folds the engine's state and one command's output into the comparable
// value.
func (s *engineSide) observe(v refmatch.Verdict, trades []refmatch.Trade, snap *EngineSnapshot) (refmatch.Observation, error) {
	o, err := engineState(s.e, s.cfg, snap)
	if err != nil {
		return refmatch.Observation{}, err
	}
	if trades == nil {
		trades = []refmatch.Trade{}
	}
	o.Verdict = v
	o.Trades = trades
	o.Events = s.rec.take()
	return o, nil
}

// stateOnly strips the command-bearing half of an Observation, leaving what a
// restarted process can see.
func stateOnly(o refmatch.Observation) refmatch.Observation {
	o.Verdict = refmatch.Verdict{}
	o.Trades = nil
	o.Events = nil
	return o
}

// --- the model side -----------------------------------------------------------

type modelSide struct {
	m   *refmatch.Model
	ids map[int]int64
}

func newModelSide(cfg refmatch.Config) *modelSide {
	return &modelSide{m: refmatch.New(cfg), ids: map[int]int64{}}
}

func (s *modelSide) apply(c tape.Cmd) (refmatch.Result, error) {
	switch c.Kind {
	case tape.Submit:
		r := s.m.Submit(modelOrder(c))
		s.ids[c.Pos] = r.Events[0].OrderID
		return r, nil
	case tape.Cancel:
		return s.m.Cancel(s.ids[c.Target], c.User), nil
	case tape.Reduce:
		return s.m.Reduce(s.ids[c.Target], c.NewQty, c.User), nil
	case tape.Replace:
		before := s.m.NextOrderID()
		r := s.m.Replace(s.ids[c.Target], c.User, modelOrder(c))
		if s.m.NextOrderID() != before {
			s.ids[c.Pos] = before
		}
		return r, nil
	case tape.CancelAll:
		return s.m.CancelAll(c.User), nil
	case tape.Halt:
		return s.m.Halt(), nil
	case tape.Resume:
		return s.m.Resume(), nil
	case tape.CancelOnly:
		return s.m.SetCancelOnly(), nil
	}
	return refmatch.Result{}, fmt.Errorf("tape kind %s is not on the tier-1 model driver", c.Kind)
}

// modelOrder translates one tape order into the model's vocabulary.
//
// The STP mode goes through an explicit refusing switch rather than a numeric cast,
// matching buildOrder on the engine side. A cast accepts any byte: widening the
// tape's STP range or adding a sixth mode would then leave the ENGINE's switch
// falling through to "venue default" while the model faithfully carried the new
// value, and the harness would report a divergence with no engine defect behind it.
// Every other translation in this file refuses what it does not know; this one used
// not to, which made it the only place the file broke its own rule.
func modelOrder(c tape.Cmd) refmatch.Order {
	var stp refmatch.STP
	switch c.STP {
	case 0:
		stp = refmatch.STPUnset
	case 1:
		stp = refmatch.CancelNewest
	case 2:
		stp = refmatch.CancelOldest
	case 3:
		stp = refmatch.CancelBoth
	case 4:
		stp = refmatch.Decrement
	case 5:
		stp = refmatch.Allow
	default:
		panic(fmt.Sprintf("tape drew STP mode %d, which neither side maps; add it to BOTH "+
			"modelOrder and engineSide.buildOrder or the harness compares a model that "+
			"understands it against an engine that does not", c.STP))
	}
	o := refmatch.Order{
		User: c.User, Price: c.Price, Qty: c.Qty,
		PostOnly: c.PostOnly, TradeGroup: c.TradeGroup, Privileged: c.Privileged,
		STPMode: stp,
	}
	if c.Sell {
		o.Side = refmatch.Sell
	}
	if c.MarketOrd {
		o.Type = refmatch.Market
	}
	switch c.TIF {
	case 1:
		o.TIF = refmatch.IOC
	case 2:
		o.TIF = refmatch.FOK
	}
	return o
}

// --- the run ------------------------------------------------------------------

// diffFailure is the first place the two sides disagreed, or a check that failed on
// the engine side alone.
type diffFailure struct {
	index  int
	cmd    tape.Cmd
	class  string
	engine refmatch.Observation
	model  refmatch.Observation
	note   string
}

// runDiff drives one tape through both sides and returns the first divergence.
//
// full turns on the two per-command checks that are not the model's business: the
// engine's own invariants (a better failure message than a divergence — "the book
// crossed after command 412" is diagnosable, "resting order 7's rank is 3 and should
// be 2" from the same cause is not), and snapshot-restore equivalence. The shrinker
// runs with full off, because it only needs the divergence class and it runs the
// tape hundreds of times.
func runDiff(p diffProfile, cmds []tape.Cmd, full bool, tb testingTB) *diffFailure {
	eng := newEngineSide(p.engine())
	mod := newModelSide(p.model)
	// The two per-command assertions the roadmap listed as absent from the generated
	// path. They are cheap, they are cumulative across the tape, and they are the
	// kind of property a differential comparison does NOT give for free: two
	// implementations that agree can both reissue an id.
	issued := map[int64]int{}   // order id -> how many times the engine assigned it
	traded := map[int64]int64{} // order id -> quantity it has printed, across the tape
	asked := map[int64]int64{}  // order id -> the quantity it was submitted with

	for i, c := range cmds {
		ev, et, err := eng.apply(c)
		if err != nil {
			return &diffFailure{index: i, cmd: c, note: err.Error()}
		}
		mr, err := mod.apply(c)
		if err != nil {
			return &diffFailure{index: i, cmd: c, note: err.Error()}
		}
		if eng.rec.err != "" {
			return &diffFailure{index: i, cmd: c, note: eng.rec.err}
		}

		snap := eng.e.TakeSnapshot()
		got, err := eng.observe(ev, et, snap)
		if err != nil {
			return &diffFailure{index: i, cmd: c, note: err.Error()}
		}
		want := mod.m.Observe(mr)

		if full {
			// Invariants first, then equivalence. An invariant violation is a better
			// failure message than a divergence: "the book crossed after command 412"
			// is diagnosable, "resting order 7's rank is 3 and should be 2" from the
			// same underlying cause is not.
			if tb != nil {
				checkInvariants(tb, eng.e, nil)
			}
			// The event stream must rebuild the engine's own L3 book, checked after
			// every command of every tape rather than on a hand-written scenario
			// list. See engineSide.mirror for why this assertion is here and not
			// only in TestEventStreamReconstructsBook.
			if note := eng.mirrorMatches(); note != "" {
				return &diffFailure{index: i, cmd: c, note: note}
			}
			// "No duplicate order id." An id is a customer-visible name that appears
			// in execution reports, drop copies, cancels and busts; two orders
			// carrying one name is unrecoverable at every layer above this.
			if c.Kind == tape.Submit || c.Kind == tape.Replace {
				if id, ok := eng.ids[c.Pos]; ok && id != 0 {
					issued[id]++
					if issued[id] > 1 {
						return &diffFailure{index: i, cmd: c, note: fmt.Sprintf(
							"order id %d was issued %d times", id, issued[id])}
					}
					asked[id] = c.Qty
				}
			}
			// "Trade quantities balance." Every print is positive, and no order ever
			// prints more than it asked for -- summed across the whole tape, on both
			// sides of every trade, so a maker that is filled twice for its whole size
			// is caught even though each fill on its own looks legal.
			for _, tr := range got.Trades {
				if tr.Qty <= 0 {
					return &diffFailure{index: i, cmd: c, note: fmt.Sprintf(
						"trade %d printed quantity %d", tr.ID, tr.Qty)}
				}
				for _, id := range [2]int64{tr.BuyOrderID, tr.SellOrderID} {
					traded[id] += tr.Qty
					if want, ok := asked[id]; ok && traded[id] > want {
						return &diffFailure{index: i, cmd: c, note: fmt.Sprintf(
							"order %d has printed %d lots in total but was submitted for %d",
							id, traded[id], want)}
					}
				}
			}
		}

		if !reflect.DeepEqual(want, got) {
			return &diffFailure{index: i, cmd: c, class: classify(want, got), engine: got, model: want}
		}

		// Snapshot-restore equivalence comes AFTER the primary comparison, and the
		// order is load-bearing for diagnosis rather than for correctness.
		//
		// It is a second equivalence, not an invariant. Run before the primary one it
		// hijacks the report for every ordinary matching bug: a reduce that re-queues
		// changes the live book's rank, so the snapshot carries the wrong rank, so the
		// restored engine differs too — and the harness would announce a restore
		// problem, via the `note` path, which prints no divergence class and no shrunk
		// tape. Measured on mutation 6: "the RESTORED engine's visible book differs"
		// with no reproduction, instead of class "book-rank" with a three-command
		// literal.
		//
		// After it, this check fires only when the LIVE engine agrees with the model
		// and the restored one does not — which is exactly the restore-specific bug
		// class it exists for, and the only case where its message is the right one.
		if full {
			if note := restoreMatchesLive(p, snap, want); note != "" {
				return &diffFailure{index: i, cmd: c, note: note}
			}
		}
	}
	return nil
}

// TestProRataSelfSkipCrossesTheBook used to pin the defect this harness found on
// its first pro-rata seed, at the smallest size that reproduces it. It now asserts
// the opposite, and the name is kept so the pin is visibly the thing that was
// redeemed.
//
// Two commands. One account rests a sell at 99, then buys 5 at 100. matchProRata
// excluded the taker's own orders from the level's eligible set, found nothing left,
// and ENDED THE WALK — so the remainder rested across the spread and every L1 and L2
// consumer saw bid 100 / ask 99 on a continuous book. It did that in all five STP
// modes, because matchProRata never called takerSTP at all: a venue configured ALLOW
// did not get the self-trade it had asked for and one configured CANCEL_BOTH
// cancelled neither order. Pro-rata was silently overriding the venue's self-trade
// -prevention configuration, which is a larger statement than "pro-rata crosses the
// book".
//
// The decision (docs/DIFFERENTIAL-FINDINGS.md §5) DELETES a rule rather than adding
// one: self-trade prevention applies at a pro-rata level exactly as it applies under
// price-time priority, and the allocation rule has no opinion about who owns what.
// Under the default CANCEL_NEWEST the taker is cancelled, which is what the same two
// commands have always done under FIFO. The specification paragraph the model
// implements is docs/REFERENCE-MATCHER.md §2.3.
//
// The crossingIsExpected exemption on the pro-rata differential profile is deleted
// with it, so checkInvariants now runs at full strength on every profile.
func TestProRataSelfSkipCrossesTheBook(t *testing.T) {
	cfg := DefaultConfig("PR")
	cfg.ProRata = true
	e := NewEngine(cfg)

	sell, err := types.NewOrder("u0", "PR", types.SideSell, types.OrderTypeLimit, 99, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(sell)
	buy, err := types.NewOrder("u0", "PR", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(buy)

	if res.Status != types.OrderStatusCancelled {
		t.Fatalf("the self-crossing buy ended %s, want CANCELLED: the default mode is CANCEL_NEWEST and "+
			"the taker is the newest. If it RESTED, matchProRata is skipping the level again and the "+
			"book below is crossed. docs/DIFFERENTIAL-FINDINGS.md §5.", res.Status)
	}
	bid, _, okBid := e.BestBid()
	ask, _, okAsk := e.BestAsk()
	if okBid && okAsk && bid >= ask {
		t.Fatalf("the book is crossed: bid %d / ask %d on a continuous book", bid, ask)
	}
	if okBid {
		t.Fatalf("the cancelled taker rested anyway (bid %d)", bid)
	}
	if !okAsk || ask != 99 {
		t.Fatalf("the maker should be untouched at 99, got %d (present %t)", ask, okAsk)
	}
	// Price-time priority answers the same two commands identically, which is the
	// whole of the decision: one self-match rule, not two.
	fifo := NewEngine(DefaultConfig("PR"))
	s2, _ := types.NewOrder("u0", "PR", types.SideSell, types.OrderTypeLimit, 99, 5, types.TIFGoodTillCancel)
	fifo.Process(s2)
	b2, _ := types.NewOrder("u0", "PR", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	fres := fifo.Process(b2)
	if fres.Status != res.Status {
		t.Fatalf("pro-rata answered %s and price-time priority answered %s; the allocation policy is "+
			"deciding the self-match again", res.Status, fres.Status)
	}
}

// TestProRata_STPModeDecides is the five-mode table from
// docs/DIFFERENTIAL-FINDINGS.md §5.1's measurement, inverted.
//
// Before the fix every one of the five produced bid 100 / ask 99, zero trades and
// two resting orders, because matchProRata never consulted the mode. Now each mode
// gets the outcome docs/REFERENCE-MATCHER.md §2.3 names for it, and all five leave
// the book uncrossed — which is the assertion that matters most, since a crossed
// continuous book is the first row of that document's invariant table.
func TestProRata_STPModeDecides(t *testing.T) {
	cases := []struct {
		mode      SelfTradePrevention
		status    types.OrderStatus
		trades    int
		resting   int
		makerGone bool
	}{
		// The taker is the newest, so it goes and the maker is untouched.
		{mode: STPCancelNewest, status: types.OrderStatusCancelled, trades: 0, resting: 1},
		// The maker goes, the level re-allocates, nothing is left to trade with, and
		// the taker rests on the now-empty other side.
		{mode: STPCancelOldest, status: types.OrderStatusNew, trades: 0, resting: 1, makerGone: true},
		// Both named, both cancelled.
		{mode: STPCancelBoth, status: types.OrderStatusCancelled, trades: 0, resting: 0, makerGone: true},
		// Equal sizes, so the overlap empties both with no trade to explain it.
		{mode: STPDecrement, status: types.OrderStatusCancelled, trades: 0, resting: 0, makerGone: true},
		// The account asked to be allowed to trade with itself, and now it is.
		{mode: STPAllow, status: types.OrderStatusFilled, trades: 1, resting: 0, makerGone: true},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			cfg := DefaultConfig("PR")
			cfg.ProRata = true
			cfg.SelfTradePrevention = c.mode
			e := NewEngine(cfg)

			sell, err := types.NewOrder("u0", "PR", types.SideSell, types.OrderTypeLimit, 99, 5, types.TIFGoodTillCancel)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			e.Process(sell)
			buy, err := types.NewOrder("u0", "PR", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			res := e.Process(buy)

			if res.Status != c.status {
				t.Errorf("status %s, want %s", res.Status, c.status)
			}
			if len(res.Trades) != c.trades {
				t.Errorf("%d trades, want %d", len(res.Trades), c.trades)
			}
			if e.OrderCount() != c.resting {
				t.Errorf("%d resting orders, want %d", e.OrderCount(), c.resting)
			}
			if _, gone := e.Book().Get(sell.ID); gone == c.makerGone {
				t.Errorf("maker present = %t, want %t", gone, !c.makerGone)
			}
			bid, _, okBid := e.BestBid()
			ask, _, okAsk := e.BestAsk()
			if okBid && okAsk && bid >= ask {
				t.Fatalf("mode %s leaves the book crossed: bid %d / ask %d. Every one of the five used "+
					"to. docs/DIFFERENTIAL-FINDINGS.md §5.1.", c.mode, bid, ask)
			}
		})
	}
}

// TestProRata_ReachesUnrelatedLiquidityUnderCancelOldest is the second half of
// §5.1's measurement: the skip did not only cross the book, it DECLINED AN
// UNRELATED ACCOUNT'S LIQUIDITY on the way.
//
// u0 rests sell 5 @ 99, u1 rests sell 5 @ 100, u0 buys 5 @ 100. Before the fix:
// zero trades, three resting orders, book crossed at 100/99 — u1 was offering
// exactly what u0 was bidding, at a price u0 named, and no trade happened, because
// the walk ended at the touch instead of resolving it.
//
// It is deliberately written under CANCEL_OLDEST and not the default. Under
// CANCEL_NEWEST the taker is cancelled at 99 and the trade with u1 correctly still
// does not happen, so the default would have made this test assert the wrong thing.
func TestProRata_ReachesUnrelatedLiquidityUnderCancelOldest(t *testing.T) {
	cfg := DefaultConfig("PR")
	cfg.ProRata = true
	cfg.SelfTradePrevention = STPCancelOldest
	e := NewEngine(cfg)

	mine, err := types.NewOrder("u0", "PR", types.SideSell, types.OrderTypeLimit, 99, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(mine)
	theirs, err := types.NewOrder("u1", "PR", types.SideSell, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(theirs)

	buy, err := types.NewOrder("u0", "PR", types.SideBuy, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(buy)

	if len(res.Trades) != 1 {
		t.Fatalf("%d trades, want 1: the taker's own order at 99 is removed under CANCEL_OLDEST and the "+
			"walk goes on to u1's offer at 100. docs/DIFFERENTIAL-FINDINGS.md §5.1.", len(res.Trades))
	}
	tr := res.Trades[0]
	if tr.Price != 100 || tr.Quantity != 5 || tr.SellerUserID != "u1" {
		t.Fatalf("the print is %d @ %d against %s, want 5 @ 100 against u1", tr.Quantity, tr.Price, tr.SellerUserID)
	}
	if res.Status != types.OrderStatusFilled {
		t.Fatalf("the taker ended %s, want FILLED", res.Status)
	}
	if e.OrderCount() != 0 {
		t.Fatalf("%d orders still resting, want 0", e.OrderCount())
	}
}

// TestProRata_MixedLevelTradesEligibleBeforeSTPFires holds the one ordering choice
// docs/REFERENCE-MATCHER.md §2.3 makes that is a choice rather than a consequence:
// the level's ELIGIBLE liquidity is allocated first, and self-trade prevention fires
// only when the taker's own order would actually be needed.
//
// Under price-time priority "when the walk reaches it" is answered by queue
// position. Pro-rata has none, so the only definition that does not invent an
// ordering is "the level's eligible liquidity is exhausted and the taker still wants
// more". A level holding BOTH the taker's own order and a stranger's, under
// CANCEL_NEWEST, is the only case that tells that rule apart from applying
// prevention first: allocate-first prints the stranger's lots and then cancels the
// taker, prevention-first prints nothing at all.
func TestProRata_MixedLevelTradesEligibleBeforeSTPFires(t *testing.T) {
	cfg := DefaultConfig("PR")
	cfg.ProRata = true
	cfg.SelfTradePrevention = STPCancelNewest
	e := NewEngine(cfg)

	mine, err := types.NewOrder("u0", "PR", types.SideSell, types.OrderTypeLimit, 100, 4, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(mine)
	theirs, err := types.NewOrder("u1", "PR", types.SideSell, types.OrderTypeLimit, 100, 3, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	e.Process(theirs)

	buy, err := types.NewOrder("u0", "PR", types.SideBuy, types.OrderTypeLimit, 100, 6, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	res := e.Process(buy)

	if len(res.Trades) != 1 {
		t.Fatalf("%d trades, want 1. Zero means self-trade prevention was applied BEFORE the level's "+
			"eligible liquidity was allocated, which refuses a stranger's offer the taker had already "+
			"named a price for. docs/REFERENCE-MATCHER.md §2.3.", len(res.Trades))
	}
	if tr := res.Trades[0]; tr.Quantity != 3 || tr.SellerUserID != "u1" {
		t.Fatalf("the print is %d lots against %s, want 3 against u1", tr.Quantity, tr.SellerUserID)
	}
	if res.Status != types.OrderStatusCancelled {
		t.Fatalf("the taker ended %s, want CANCELLED: it still wanted 3 lots and the only liquidity "+
			"left at the level was its own", res.Status)
	}
	if _, ok := e.Book().Get(mine.ID); !ok {
		t.Fatalf("the taker's own resting order was consumed under CANCEL_NEWEST")
	}
	if bid, _, ok := e.BestBid(); ok {
		t.Fatalf("the cancelled taker rested at %d", bid)
	}
}

// restoreMatchesLive is deliverable 10's fourth missing assertion: snapshot,
// restore into a fresh engine, and compare the result against the MODEL — after
// every command of a generated tape rather than once on a seven-command fixture.
//
// It compares the restored engine's observable BOOK, not just a digest. The first
// version of this function did only `restore(snap).Digest() == snap.Digest()`, and
// that is a snapshot -> restore -> snapshot round-trip: it can only see state that
// EngineSnapshot itself carries, so it is structurally blind to everything
// LoadSnapshot rebuilds. §3.3(c) argued that was harmless because "state that is not
// in the snapshot and not in the observation is state no consumer and no restart can
// see". That argument is false, and the counter-example is the single most widely
// consumed number this venue publishes.
//
// PriceLevel.TotalQty is not in EngineSnapshot — the snapshot carries orders, and
// the aggregate is rebuilt by book.Add on the way in. Adding one line to
// LoadSnapshot that double-counts each restored order into its level makes every
// restored engine serve DOUBLE the true L2 depth at every price, and the whole
// repository stayed green: `go test ./... -count=1` exited 0 over 23 packages,
// because restored.Digest() == snap.Digest() is still true — the digest hashes the
// snapshot the bug does not touch. Live best bid 7, restored best bid 14, tests
// green. The aggregate IS in the observation, but only for the live engine.
//
// So the restored engine is now folded through the same engineState path the live
// one is, and compared against the model. The asymmetry is preserved and doubled:
// the restored engine's levels come from its own maintained aggregate, rebuilt from
// scratch by a different code path than the live engine's incremental one, and both
// are checked against a sum the model computes independently.
func restoreMatchesLive(p diffProfile, snap *EngineSnapshot, want refmatch.Observation) string {
	cfg := p.engine()
	cfg.EventSink = nil
	restored, err := RestoreEngine(cfg, snap)
	if err != nil {
		return fmt.Sprintf("snapshot restore failed: %v", err)
	}
	if got, w := restored.TakeSnapshot().Digest(), snap.Digest(); got != w {
		return fmt.Sprintf("snapshot restore differs from the live engine\n got %s\nwant %s", got, w)
	}
	got, err := engineState(restored, cfg, restored.TakeSnapshot())
	if err != nil {
		return fmt.Sprintf("restored engine: %v", err)
	}
	if w := stateOnly(want); !reflect.DeepEqual(w, got) {
		return fmt.Sprintf("the RESTORED engine's visible book differs from the model, though its snapshot "+
			"digest matches — so the difference is in state LoadSnapshot rebuilds rather than state the "+
			"snapshot carries\n model  %s\n restored %s", describe(w), describe(got))
	}
	return ""
}

// restoreSide wraps an engine restored from snap in a tape driver, inheriting the
// tape-position -> order-id map the live engine had built. The ids matter: a command
// after the fork can name an order submitted before it, and a restored venue has to
// answer for orders it did not itself admit.
func restoreSide(p diffProfile, snap *EngineSnapshot, ids map[int]int64) (*engineSide, error) {
	cfg := p.engine()
	rec := &eventRecorder{}
	cfg.EventSink = rec
	e, err := RestoreEngine(cfg, snap)
	if err != nil {
		return nil, err
	}
	inherited := make(map[int]int64, len(ids))
	for k, v := range ids {
		inherited[k] = v
	}
	return &engineSide{e: e, rec: rec, cfg: cfg, ids: inherited}, nil
}

// TestSnapshotRestoreEqualsUninterruptedExecution is the assertion the roadmap
// carried as ❌ and the summary simultaneously claimed was closed. Both were wrong,
// in opposite directions, and this is what makes the row true.
//
// restoreMatchesLive compares a restored engine's STATE after every command. That is
// necessary and it is not the same property: a restored engine can look right and
// then behave wrong, because restoring rebuilds structures the next command reads —
// the level aggregates, the id-to-node index, the free list, the price containers.
// Nothing anywhere restored a snapshot and then kept TRADING against it.
//
// So: run the tape to a fork point, snapshot, restore into a fresh engine, and drive
// the REMAINDER of the tape through the restored engine alone, comparing the whole
// observation after every command against the model. If a restart is transparent,
// the restored venue's answers from here on are indistinguishable from the ones the
// process would have given had it never stopped.
//
// The model is the stand-in for "uninterrupted execution", and it is a legitimate
// one precisely because TestDifferentialTape has already established that an
// uninterrupted engine agrees with it command by command on these same tapes. Using
// the model rather than a second live engine also keeps the property honest: two
// engines that share a restore bug would agree with each other.
func TestSnapshotRestoreEqualsUninterruptedExecution(t *testing.T) {
	for _, p := range diffProfiles() {
		for _, seed := range p.seeds {
			cmds := tape.Gen(p.tapes, seed, p.n)
			// Three forks: one before the book has built up, one mid-tape, one late
			// enough that the tail is short. A single fork point tests a single shape
			// of book.
			for _, fork := range []int{p.n / 4, p.n / 2, (3 * p.n) / 4} {
				t.Run(fmt.Sprintf("%s/seed=%d/fork=%d", p.name, seed, fork), func(t *testing.T) {
					live := newEngineSide(p.engine())
					mod := newModelSide(p.model)
					for i := 0; i <= fork; i++ {
						if _, _, err := live.apply(cmds[i]); err != nil {
							t.Fatalf("engine at command %d: %v", i, err)
						}
						if _, err := mod.apply(cmds[i]); err != nil {
							t.Fatalf("model at command %d: %v", i, err)
						}
						live.rec.take()
					}

					restored, err := restoreSide(p, live.e.TakeSnapshot(), live.ids)
					if err != nil {
						t.Fatalf("restore at command %d: %v", fork, err)
					}
					if restored.e.OrderCount() == 0 {
						t.Skipf("the book was empty at the fork, so continuing from it asserts nothing")
					}

					for i := fork + 1; i < len(cmds); i++ {
						c := cmds[i]
						mr, err := mod.apply(c)
						if err != nil {
							t.Fatalf("model at command %d: %v", i, err)
						}
						want := mod.m.Observe(mr)

						v, tr, err := restored.apply(c)
						if err != nil {
							t.Fatalf("restored engine at command %d: %v", i, err)
						}
						if restored.rec.err != "" {
							t.Fatalf("restored engine at command %d: %s", i, restored.rec.err)
						}
						got, err := restored.observe(v, tr, restored.e.TakeSnapshot())
						if err != nil {
							t.Fatalf("restored engine at command %d: %v", i, err)
						}
						if !reflect.DeepEqual(want, got) {
							t.Fatalf("a venue restored from a snapshot at command %d answered command %d "+
								"differently from one that never stopped\n"+
								"  divergence class  %s\n  command           %+v\n  model    %s\n  restored %s",
								fork, i, classify(want, got), c, describe(want), describe(got))
						}
					}
				})
			}
		}
	}
}

// classify names the FIELD that first differs, from a fixed enum. The shrinker's
// predicate is "same class", which is what stops it wandering onto a different bug
// and reporting a minimal tape for something that was not the failure.
func classify(want, got refmatch.Observation) string {
	if want.Verdict != got.Verdict {
		return "verdict"
	}
	if len(want.Trades) != len(got.Trades) {
		return "trade-count"
	}
	for i := range want.Trades {
		a, b := want.Trades[i], got.Trades[i]
		switch {
		case a == b:
		case a.Price != b.Price:
			return "trade-price"
		case a.Qty != b.Qty:
			return "trade-quantity"
		case a.MakerOrderID != b.MakerOrderID || a.TakerOrderID != b.TakerOrderID:
			return "trade-order"
		case a.ID != b.ID || a.Seq != b.Seq:
			return "trade-id"
		default:
			return "trade-field"
		}
	}
	if len(want.Book) != len(got.Book) {
		return "book-membership"
	}
	if !sameIDs(want.Book, got.Book) {
		return "order-id"
	}
	for i := range want.Book {
		a, b := want.Book[i], got.Book[i]
		switch {
		case a == b:
		case a.ID != b.ID:
			// Same ids, different order: a rank failure, which is the assertion
			// membership alone would not have made.
			return "book-rank"
		case a.Qty != b.Qty || a.Filled != b.Filled || a.Remaining != b.Remaining:
			return "book-quantity"
		default:
			return "book-field"
		}
	}
	if !reflect.DeepEqual(want.Levels, got.Levels) {
		return "level-aggregate"
	}
	if len(want.Events) != len(got.Events) {
		return "event-count"
	}
	for i := range want.Events {
		a, b := want.Events[i], got.Events[i]
		switch {
		case a == b:
		case a.Kind != b.Kind:
			return "event-kind"
		case a.OrderID != b.OrderID:
			return "order-id"
		case a.Price != b.Price || a.Qty != b.Qty:
			return "event-trade-payload"
		default:
			return "event-field"
		}
	}
	if want.State != got.State {
		return "state"
	}
	if want.LastTradePrice != got.LastTradePrice {
		return "last-trade-price"
	}
	if want.NextOrderID != got.NextOrderID {
		return "order-id"
	}
	if want.NextTradeID != got.NextTradeID {
		return "trade-id"
	}
	return "unclassified"
}

func sameIDs(a, b []refmatch.Resting) bool {
	seen := map[int64]int{}
	for _, r := range a {
		seen[r.ID]++
	}
	for _, r := range b {
		seen[r.ID]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// report shrinks a failing tape and prints the whole deliverable: the seed, the
// profile, the divergence class, the command index, a field-by-field diff, and the
// shrunk tape as a Go literal ready to paste into a regression test.
func report(t *testing.T, p diffProfile, seed uint64, cmds []tape.Cmd, f *diffFailure) {
	t.Helper()
	if f.note != "" {
		t.Fatalf("%s/seed=%d: command %d (%+v): %s", p.name, seed, f.index, f.cmd, f.note)
	}

	const shrinkBudget = 4000
	sh := tape.Shrink(cmds, f.class, func(cand []tape.Cmd) (string, bool) {
		g := runDiff(p, cand, false, nil)
		if g == nil {
			return "", false
		}
		return g.class, true
	}, shrinkBudget)

	final := runDiff(p, sh.Cmds, false, nil)
	drift := ""
	if sh.Drift != "" {
		drift = fmt.Sprintf("\n(the shrinker saw class %q on a candidate it therefore refused)", sh.Drift)
	}
	// Two different indices, and conflating them sends a reader to the wrong command.
	// f.index is the first divergence on the ORIGINAL tape; final.index is the first
	// divergence on the SHRUNK one, which is usually a much smaller number because
	// the shrunk tape is much shorter. Printing the shrunk index against the original
	// length produced messages like "first at command 2 of 140" when the real first
	// divergence on that tape was at command 118.
	mo, eo := f.model, f.engine
	shrunkAt := -1
	if final != nil {
		shrunkAt, mo, eo = final.index, final.model, final.engine
	}
	where := fmt.Sprintf("command %d of %d on the generated tape", f.index, len(cmds))
	if shrunkAt >= 0 {
		where += fmt.Sprintf("; command %d of the %d-command shrunk tape below", shrunkAt, len(sh.Cmds))
	}

	t.Fatalf("differential divergence\n"+
		"  profile           %s\n"+
		"  seed              %d\n"+
		"  divergence class  %s\n"+
		"  first at          %s\n"+
		"  shrunk            %d -> %d commands in %d predicate calls%s\n"+
		"  model  %s\n"+
		"  engine %s\n\n"+
		"reproduce with:\n%s\n",
		p.name, seed, f.class, where, len(cmds), len(sh.Cmds), sh.Calls, drift,
		describe(mo), describe(eo), tape.Literal(sh.Cmds))
}

func describe(o refmatch.Observation) string {
	return fmt.Sprintf("verdict=%+v state=%d last=%d nextOrd=%d nextTrd=%d\n"+
		"         trades=%+v\n         book=%+v\n         levels=%+v\n         events=%+v",
		o.Verdict, o.State, o.LastTradePrice, o.NextOrderID, o.NextTradeID, o.Trades, o.Book, o.Levels, o.Events)
}

// --- the tests ------------------------------------------------------------------

// TestDifferentialTape is the milestone's central ask: a generated tape compared
// against an expected answer, command by command, rather than against a set of
// invariants every self-consistently-wrong matcher also satisfies.
func TestDifferentialTape(t *testing.T) {
	for _, p := range diffProfiles() {
		for _, seed := range p.seeds {
			t.Run(fmt.Sprintf("%s/seed=%d", p.name, seed), func(t *testing.T) {
				cmds := tape.Gen(p.tapes, seed, p.n)
				if f := runDiff(p, cmds, true, t); f != nil {
					report(t, p, seed, cmds, f)
				}
			})
		}
	}
}

// TestSameSeedSameObservations is the guard that catches a map iteration or a live
// clock leaking into EITHER side. It is cheap enough to run always.
//
// "Either side" is now true. The first version drove the model and threw its result
// away, collecting two slices of ENGINE observations — so nondeterminism introduced
// into internal/refmatch would not have been caught here at all. It would have
// surfaced as an intermittent TestDifferentialTape divergence with no test pointing
// at the model, which is the worst way to find it: the natural suspect is the engine.
func TestSameSeedSameObservations(t *testing.T) {
	p := diffProfiles()[0]
	cmds := tape.Gen(p.tapes, 99, 120)

	collect := func() (engine, model []refmatch.Observation) {
		eng := newEngineSide(p.engine())
		mod := newModelSide(p.model)
		for _, c := range cmds {
			v, tr, err := eng.apply(c)
			if err != nil {
				t.Fatalf("engine: %v", err)
			}
			mr, err := mod.apply(c)
			if err != nil {
				t.Fatalf("model: %v", err)
			}
			o, err := eng.observe(v, tr, eng.e.TakeSnapshot())
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			engine = append(engine, o)
			model = append(model, mod.m.Observe(mr))
		}
		return engine, model
	}

	engineA, modelA := collect()
	engineB, modelB := collect()
	for _, side := range []struct {
		name string
		a, b []refmatch.Observation
	}{
		{"engine", engineA, engineB},
		{"model", modelA, modelB},
	} {
		for i := range side.a {
			if !reflect.DeepEqual(side.a[i], side.b[i]) {
				t.Fatalf("the same tape produced different %s observations at command %d:\n%s\nvs\n%s",
					side.name, i, describe(side.a[i]), describe(side.b[i]))
			}
		}
	}
}

// TestHarnessClockIsDeterministic pins the injected clock. Two engines built from
// the same profile must see the same instants in the same order, which time.Now
// cannot do.
//
// It matters because §3.3(a) of docs/REFERENCE-MATCHER.md permits timestamps to
// differ between the two sides only on the argument that nothing tier-1 decides
// depends on them. The moment a future slice models DAY or GTD, the clock becomes
// part of the answer, and a harness that had quietly been running on wall time would
// start failing for a reason nobody could reproduce.
func TestHarnessClockIsDeterministic(t *testing.T) {
	for _, p := range diffProfiles() {
		a, b := p.engine().Clock, p.engine().Clock
		if a == nil || b == nil {
			t.Fatalf("profile %s injects no clock, so the engine reads wall time", p.name)
		}
		for i := 0; i < 8; i++ {
			if x, y := a(), b(); !x.Equal(y) {
				t.Fatalf("profile %s: two engines built from the same profile disagree at clock read %d "+
					"(%s vs %s) — the harness is running on wall time", p.name, i, x, y)
			}
		}
	}
}

// TestEventSequenceIsDenseAndMonotonic is what makes it safe for the model not to
// assign event sequence numbers. Together with the elementwise event comparison in
// the Observation, a list that matches and a numbering that is dense from 1 leave
// exactly one possible value per event — so nothing is actually unchecked.
func TestEventSequenceIsDenseAndMonotonic(t *testing.T) {
	for _, p := range diffProfiles() {
		t.Run(p.name, func(t *testing.T) {
			eng := newEngineSide(p.engine())
			for _, c := range tape.Gen(p.tapes, 3, 200) {
				if _, _, err := eng.apply(c); err != nil {
					t.Fatalf("engine: %v", err)
				}
				eng.rec.take()
			}
			if len(eng.rec.seqs) == 0 {
				t.Fatal("the tape emitted no events, so this asserts nothing")
			}
			for i, s := range eng.rec.seqs {
				if s != int64(i+1) {
					t.Fatalf("event %d carries Seq %d, want %d — the stream is not dense from 1", i, s, i+1)
				}
			}
		})
	}
}

// TestEveryTierOneRejectionIsMapped asserts totality. A rejection reason the map
// does not know would otherwise arrive as "unknown, therefore equal", which is the
// oracle quietly switched off for exactly the command the new reason was added for.
func TestEveryTierOneRejectionIsMapped(t *testing.T) {
	// The tier-1 reachable set, enumerated by hand because "reachable" is a claim
	// about code paths that no reflection can make. Each carries the path that
	// reaches it, so a reviewer can disagree with the sentence rather than a flag.
	reachable := map[error]string{
		types.ErrTradingHalted:          "settleInto, StateHalted",
		types.ErrNewOrdersHalted:        "settleInto, StateCancelOnly",
		types.ErrPostOnlyWouldCross:     "settleInto, post-only across a crossed spread",
		types.ErrFOKCannotFill:          "settleInto, the fill-or-kill unwind",
		types.ErrMarketOrderNoLiquidity: "settleInto, a market order that printed nothing",
		types.ErrOrderBookFull:          "orderbook.Add via the GTC resting branch, under MaxOrders",
		types.ErrOrderNotFound:          "Cancel/Reduce/Replace naming an absent or another account's order",
		types.ErrOrderNotActive:         "Cancel/Reduce on an order that is no longer live",
		types.ErrInvalidQuantity:        "Reduce with a new total that is not a reduction",
		types.ErrNilOrder:               "Replace with no replacement",
		types.ErrNotionalOverflow:       "checkOrderCaps, price x quantity overflowing int64",
	}
	for err, path := range reachable {
		if _, ok := mapReject(err); !ok {
			t.Errorf("%v is reachable on the tier-1 path (%s) and has no enum; a rejection the "+
				"harness cannot name is one it will report as equal", err, path)
		}
	}
	for err := range tierOneRejections {
		if _, ok := reachable[err]; !ok {
			t.Errorf("%v is mapped but is not in the reachable set: either it is reachable and "+
				"the set is stale, or the mapping is dead", err)
		}
	}
	// No catch-all. mapReject must refuse what it does not know.
	if _, ok := mapReject(errors.New("a reason nobody declared")); ok {
		t.Fatal("mapReject accepted an unknown error; a catch-all enum turns the verdict comparison off")
	}
}

// FuzzDifferential is the unbounded half of the campaign. The fuzzer's bytes pick a
// seed and a length, so a failure prints a tape literal rather than a corpus file —
// a corpus file is not a reproduction anyone can read.
//
// It runs with full=true, so the nightly campaign is the same oracle as the committed
// sweep. It used to pass full=false, which quietly made the unbounded half STRICTLY
// WEAKER than the fixed one: no invariant check, no snapshot-restore comparison, no
// duplicate-order-id check and no trade-quantity-balance check — the last two being
// exactly the checks two of the recorded mutations are credited to by name. Dropping
// LastTradePrice in LoadSnapshot failed all 16 sweep subtests and left a full corpus
// run green. A 30-minute campaign that cannot see what a 0.25-second test sees is a
// campaign spent proving the weaker thing.
//
// The cost is real and it is the right trade: full=true takes a snapshot and a
// restore per command, so the fuzzer explores fewer inputs per minute. Fewer inputs
// against the whole oracle beats more inputs against part of it. The shrinker still
// runs with full off — it only needs the divergence class, and it runs the tape
// hundreds of times.
func FuzzDifferential(f *testing.F) {
	f.Add(uint64(1), uint16(40))
	f.Add(uint64(97), uint16(120))
	f.Add(uint64(0xDEADBEEF), uint16(200))
	f.Fuzz(func(t *testing.T, seed uint64, n uint16) {
		length := int(n%256) + 1
		for _, p := range diffProfiles() {
			cmds := tape.Gen(p.tapes, seed, length)
			if fail := runDiff(p, cmds, true, t); fail != nil {
				report(t, p, seed, cmds, fail)
			}
		}
	})
}
