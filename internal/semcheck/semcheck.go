// Package semcheck renders what the matching engine DOES with a fixed corpus of
// commands, so a change to it has to be deliberate and has to move
// matching.SemanticsVersion.
//
// It is internal/apicheck's trick applied to behaviour instead of to signatures.
// apicheck freezes the exported surface into a golden file and fails when the file
// moves; it does not prevent a breaking change and does not try, it makes one
// impossible to ship without a human reading a diff. semcheck freezes the OUTCOMES —
// verdicts, trades, published events, book state and snapshot digests — with one
// addition apicheck does not need: regeneration is gated on the version constant
// having moved.
//
// # Why the differential harness cannot do this job
//
// The obvious idea is to let pkg/matching's TestDifferentialTape carry it: it already
// compares the engine against internal/refmatch after every command, so a behaviour
// change turns it red. It cannot, for a reason docs/REFERENCE-MATCHER.md §2.2 records
// and the changelog repeats under Known limitation: THE MODEL IS EDITED IN THE SAME
// COMMIT AS THE ENGINE. It has to be — three of the fixes in this release required
// exactly that. A harness whose oracle is updated by the same person in the same
// change cannot detect that the change happened.
//
//	TestDifferentialTape   is the engine RIGHT?     oracle: internal/refmatch, edited in the same commit
//	internal/semcheck      did the engine CHANGE?   oracle: the previous release's recorded behaviour,
//	                                                which only moves with a version bump
//
// Both are needed and they will sometimes disagree, which is not a contradiction: they
// answer different questions with different oracles, and only one of them has an
// oracle the same commit is allowed to edit.
//
// # Why the digest alone cannot do it either
//
// matching.EngineSnapshot.Digest is this repository's existing notion of "the same
// state" and it is a component of the fingerprint, but it cannot be the whole of it:
// it sees only what a snapshot carries. A change that rejects a command instead of
// accepting it and immediately cancelling it leaves the same book, the same digest and
// a completely different event stream and verdict — which is defect B of
// docs/DIFFERENTIAL-FINDINGS.md almost exactly. So the fingerprint is
// OBSERVATION-based, with the digest inside it.
//
// # The public API only
//
// The corpus is driven through matching's exported surface and nothing else, with a
// deterministic clock. It must see what a consumer sees: a fingerprint with access to
// internals would move on refactors no client could observe, which is the false-alarm
// failure mode. The residual — a change invisible to every consumer at the command
// where it happens — is caught anyway at the later command where it becomes visible,
// because the fingerprint runs over whole tapes rather than isolated commands.
package semcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Kind is a command the corpus can issue. There is one for every wal.EntryKind, which
// is not a coincidence: the journal records exactly the commands that change the book,
// so "the corpus reaches every EntryKind" is a property pkg/wal can assert against its
// own entryKindCount sentinel without this package importing it.
type Kind uint8

const (
	Submit Kind = iota
	Cancel
	Reduce
	CancelAll
	Stop
	OCO
	Iceberg
	Pegged
	Trailing
	Replace
	Halt
	Resume
	CancelOnly
	SetMark
	Bust
	SetPhase
	// ExpireDue is not a journalled command — expiry is a consequence of the clock,
	// not of an operator — but it is decided behaviour and a change to it is a
	// semantics change, so the corpus can ask for it.
	ExpireDue
	kindCount
)

// Name is the stable label a Kind carries into the golden file and into the coverage
// report. It is deliberately the same vocabulary wal.EntryKind uses, so the guard in
// pkg/wal that asserts this corpus reaches every journalled kind can be written
// without a second translation layer.
func (k Kind) Name() string {
	switch k {
	case Submit:
		return "SUBMIT"
	case Cancel:
		return "CANCEL"
	case Reduce:
		return "REDUCE"
	case CancelAll:
		return "CANCEL_ALL"
	case Stop:
		return "STOP"
	case OCO:
		return "OCO"
	case Iceberg:
		return "ICEBERG"
	case Pegged:
		return "PEGGED"
	case Trailing:
		return "TRAILING"
	case Replace:
		return "REPLACE"
	case Halt:
		return "HALT"
	case Resume:
		return "RESUME"
	case CancelOnly:
		return "CANCEL_ONLY"
	case SetMark:
		return "SET_MARK"
	case Bust:
		return "BUST"
	case SetPhase:
		return "SET_PHASE"
	case ExpireDue:
		return "EXPIRE_DUE"
	}
	return fmt.Sprintf("KIND(%d)", uint8(k))
}

// Cmd is one command in a corpus script.
//
// Target names an earlier command by its POSITION in the script rather than by an
// index into the ids issued so far, which is the same deletion-closed shape
// internal/tape uses and for the same reason: a command naming a position that
// produced no order is a well-formed command with a predictable rejection, where a
// positional index into a list of ids silently retargets.
type Cmd struct {
	Pos  int
	Kind Kind

	// The order payload, used by Submit, Replace and every conditional kind.
	User          string
	Sell          bool
	Type          types.OrderType
	TIF           types.TimeInForce
	Price         int64
	Qty           int64
	PostOnly      bool
	STP           string
	TradeGroup    int64
	Privileged    bool
	ClientOrderID string
	ExpiresIn     time.Duration

	Target     int
	NewQty     int64
	StopPrice  int64
	StopSell   bool
	StopPrice2 int64 // OCO's stop leg limit price
	DisplayQty int64
	PegRef     string
	PegOffset  int64
	Trail      int64
	MarkPrice  int64
	TradeRef   int   // Bust: the position whose Nth printed trade is annulled
	TradeNth   int   // Bust: which of that command's trades (1-based)
	Phase      uint8 // SetPhase: an index into phaseOrder

	// Note is free text appended to the rendered line. It is for the reader of a
	// diff, and it is part of the golden, so changing one is a golden change with no
	// behaviour change — which Rule 22 refuses. Use it sparingly and permanently.
	Note string
}

// Scenario is one corpus entry: a named engine configuration and the script driven
// through it.
type Scenario struct {
	Name   string
	Config func() matching.Config
	Script []Cmd
}

// Coverage is what a fingerprint run REACHED, by outcome rather than by draw.
//
// Reached is the load-bearing word. An order type that is generated and never fills
// proves nothing about the fill path, so every counter here is incremented from an
// observed outcome — a verdict, a published event, a printed trade — and never from
// the command that asked for it.
type Coverage struct {
	// Commands counts each Kind the corpus issued and the engine accepted as that
	// kind (a Submit rejected by the type layer before it reached the engine is not
	// counted). Keyed by Kind.Name.
	Commands map[string]int
	// Events counts published events by matching.EventKind, as an integer so the
	// guard can enumerate the enum rather than a list of names.
	Events map[int]int
	// OrderTypes, TIFs and STPModes count the string-typed axes, by the value that
	// actually reached a resting or matching outcome.
	OrderTypes map[string]int
	TIFs       map[string]int
	STPModes   map[string]int
	// Rejections counts refused commands by the stable name of the sentinel they
	// carried. An unmapped reason lands under UNMAPPED, which is loud in a diff.
	Rejections map[string]int
	// Verdicts counts terminal order statuses.
	Verdicts map[string]int
	// Policies counts commands run under each allocation policy.
	Policies map[string]int
	// Trades, AuctionTrades, Triggers, Refills and Busts are the aggregate
	// behaviours a coverage regression would silently drain away.
	Trades        int
	AuctionTrades int
	Triggers      int
	Refills       int
	Busts         int
	Uncrosses     int
	// IcebergRestores and CascadeTerminals are the two paths docs/PINNED-DEFECTS.md
	// §6.1 measured the corpus was blind to, counted so it cannot go blind again.
	// Both were zero before that slice extended tierTwo, which is exactly why
	// neither of the defects it fixed moved a line of the golden.
	//
	// IcebergRestores counts a fill-or-kill REFUSED across a resting iceberg it was
	// priced into, which left that iceberg's reserve and refill counter exactly as
	// it found them. A restore is invisible by design — that is the property the fix
	// delivers — so what is counted is the SETUP being reached and the iceberg
	// coming back untouched, which is as much as the public API can see. The
	// correctness assertion is the golden line; this is the guard that keeps the
	// command in the corpus.
	//
	// CascadeTerminals counts a TRIGGERED and an ACCEPTED for one order id followed,
	// in the same command, by a CANCELED for it — a stop fired from inside another
	// command's walk whose own order then ended without resting. A stop that fires
	// ON ARRIVAL cannot be counted here and must not be: emitResult puts its
	// ACCEPTED before the TRIGGERED, which is how the two paths are told apart.
	IcebergRestores  int
	CascadeTerminals int
}

func newCoverage() *Coverage {
	return &Coverage{
		Commands: map[string]int{}, Events: map[int]int{},
		OrderTypes: map[string]int{}, TIFs: map[string]int{}, STPModes: map[string]int{},
		Rejections: map[string]int{}, Verdicts: map[string]int{}, Policies: map[string]int{},
	}
}

// Clock is the corpus's only source of time, and it steps rather than reads. The
// engine has to be deterministic by construction for any of this to mean anything;
// a live clock here would make the golden a function of when it was generated.
func Clock() func() time.Time {
	base := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	var n int64
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Millisecond)
	}
}

// Symbol is the instrument every scenario trades. One symbol, because the fingerprint
// is about the matcher and not about the venue.
const Symbol = "SEM"

// recorder collects the engine's published event stream per command.
type recorder struct {
	cur []matching.Event
}

func (r *recorder) OnEvents(evs []matching.Event) {
	// The slice and the pointers inside it are valid only for the duration of the
	// call, so everything the render needs is copied out here.
	for _, e := range evs {
		c := e
		if e.Order != nil {
			o := *e.Order
			c.Order = &o
		}
		if e.Trade != nil {
			t := *e.Trade
			c.Trade = &t
		}
		r.cur = append(r.cur, c)
	}
}

func (r *recorder) take() []matching.Event {
	out := r.cur
	r.cur = nil
	return out
}

// run drives one scenario and appends its rendered lines to out.
func run(sc Scenario, out *strings.Builder, cov *Coverage) error {
	cfg := sc.Config()
	rec := &recorder{}
	cfg.EventSink = rec
	cfg.Symbol = Symbol
	eng := matching.NewEngine(cfg)

	policy := "fifo"
	if cfg.ProRata {
		policy = "prorata"
	}

	fmt.Fprintf(out, "\n== %s (%s, stp=%s, maxOrders=%d, shard=%d)\n",
		sc.Name, policy, defaultSTP(cfg), cfg.MaxOrders, cfg.ShardIndex)

	// ids maps a script POSITION to the engine order id it produced, and trades maps
	// a position to the trade ids it printed, so a bust can name a print without the
	// script hard-coding an id the allocator chose.
	ids := map[int]int64{}
	printed := map[int][]int64{}
	// Refill counts are read back off the SNAPSHOT rather than off the iceberg the
	// script submitted, because a refill happens later, on somebody else's taking
	// command. Per-id maxima, because an iceberg that is exhausted or cancelled
	// leaves the snapshot and would otherwise take its count with it.
	refills := map[int64]int64{}
	// The previous command's snapshot, so a command can be compared against the
	// state it found rather than only against the state it left. An iceberg RESTORE
	// leaves no trace by design, so "unchanged across a refusal" is the only shape
	// of it the public API can see at all.
	var prev *matching.EngineSnapshot

	for _, c := range sc.Script {
		cov.Policies[policy]++
		line, snap, err := apply(sc.Name, eng, c, ids, printed, refills, prev, rec, cov)
		if err != nil {
			return fmt.Errorf("%s: command %d (%s): %w", sc.Name, c.Pos, c.Kind.Name(), err)
		}
		prev = snap
		out.WriteString(line)
		out.WriteByte('\n')
	}
	for _, n := range refills {
		cov.Refills += int(n)
	}
	return nil
}

func defaultSTP(cfg matching.Config) string {
	if cfg.SelfTradePrevention == "" {
		return string(matching.STPCancelNewest)
	}
	return string(cfg.SelfTradePrevention)
}

// phaseOrder is the phase vocabulary a SetPhase command draws from. It is a declared
// list and it is exactly the hole §5.5 of docs/SEMANTICS-VERSION.md names: a new
// EngineState is invisible here until somebody adds a line. matching's own
// TestEngineStateNamesRoundTrip is what keeps the names total over the block.
var phaseOrder = []matching.EngineState{
	matching.StateOpen,
	matching.StatePreOpen,
	matching.StateClosingAuction,
	matching.StateHalted,
	matching.StateCancelOnly,
	matching.StateClosed,
}

// apply issues one command and renders the whole of what the engine did about it.
func apply(scenario string, eng *matching.Engine, c Cmd, ids map[int]int64, printed map[int][]int64,
	refills map[int64]int64, prev *matching.EngineSnapshot, rec *recorder, cov *Coverage) (string, *matching.EngineSnapshot, error) {
	var (
		verdict string
		trades  []*types.Trade
		auction []types.Trade
	)

	switch c.Kind {
	case Submit:
		o, err := buildOrder(c)
		if err != nil {
			return "", nil, err
		}
		res := eng.Process(o)
		ids[c.Pos] = res.Order.ID
		verdict, trades = resultVerdict(res), res.Trades

	case Replace:
		repl, err := buildOrder(c)
		if err != nil {
			return "", nil, err
		}
		res, rerr := eng.Replace(ids[c.Target], c.User, repl)
		if rerr != nil {
			verdict = "REJECTED:" + rejectName(rerr)
		} else {
			ids[c.Pos] = res.Order.ID
			verdict, trades = resultVerdict(res), res.Trades
		}

	case Cancel:
		_, err := eng.Cancel(ids[c.Target], c.User)
		verdict = simpleVerdict("CANCELLED", err)

	case Reduce:
		_, err := eng.Reduce(ids[c.Target], c.NewQty, c.User)
		verdict = simpleVerdict("REDUCED", err)

	case CancelAll:
		verdict = fmt.Sprintf("CANCELLED n=%d", len(eng.CancelAllForUser(c.User)))

	case Halt:
		eng.Halt()
		verdict = "OK"

	case Resume:
		eng.Resume()
		verdict = "OK"

	case CancelOnly:
		eng.SetCancelOnly()
		verdict = "OK"

	case SetMark:
		verdict = simpleVerdict("OK", eng.SetMarkPrice(c.MarkPrice))

	case SetPhase:
		auction = eng.SetPhase(phaseOrder[int(c.Phase)%len(phaseOrder)])
		verdict = fmt.Sprintf("OK uncross=%d", len(auction))
		if len(auction) > 0 {
			cov.Uncrosses++
			cov.AuctionTrades += len(auction)
		}

	case Bust:
		id := int64(0)
		if ts := printed[c.TradeRef]; len(ts) >= c.TradeNth && c.TradeNth > 0 {
			id = ts[c.TradeNth-1]
		}
		err := eng.Bust(id, "semcheck")
		verdict = simpleVerdict("BUSTED", err)
		if err == nil {
			cov.Busts++
		}

	case ExpireDue:
		eng.ExpireDue()
		verdict = "OK"

	case Stop:
		o, err := buildOrder(c)
		if err != nil {
			return "", nil, err
		}
		s, err := types.NewStopOrder(o, c.StopPrice)
		if err != nil {
			return "", nil, err
		}
		res := eng.ProcessStop(s)
		ids[c.Pos] = res.Order.ID
		verdict, trades = resultVerdict(res), res.Trades

	case OCO:
		primary, err := buildOrder(c)
		if err != nil {
			return "", nil, err
		}
		leg := c
		leg.Sell = c.StopSell
		leg.Price = c.StopPrice2
		legOrder, err := buildOrder(leg)
		if err != nil {
			return "", nil, err
		}
		s, err := types.NewStopOrder(legOrder, c.StopPrice)
		if err != nil {
			return "", nil, err
		}
		pair, err := types.NewOCOOrder(primary, s)
		if err != nil {
			return "", nil, err
		}
		res := eng.ProcessOCO(pair)
		ids[c.Pos] = res.Order.ID
		verdict, trades = resultVerdict(res), res.Trades

	case Iceberg:
		o, err := buildOrder(c)
		if err != nil {
			return "", nil, err
		}
		ib, err := types.NewIcebergOrder(o, c.DisplayQty)
		if err != nil {
			return "", nil, err
		}
		res := eng.ProcessIceberg(ib)
		ids[c.Pos] = res.Order.ID
		verdict, trades = resultVerdict(res), res.Trades

	case Pegged:
		o, err := buildOrder(c)
		if err != nil {
			return "", nil, err
		}
		p, err := types.NewPeggedOrder(o, types.PegReference(c.PegRef), c.PegOffset)
		if err != nil {
			return "", nil, err
		}
		res := eng.ProcessPegged(p)
		ids[c.Pos] = res.Order.ID
		verdict, trades = resultVerdict(res), res.Trades

	case Trailing:
		o, err := buildOrder(c)
		if err != nil {
			return "", nil, err
		}
		ts, err := types.NewTrailingStop(o, c.Trail)
		if err != nil {
			return "", nil, err
		}
		res := eng.ProcessTrailingStop(ts)
		ids[c.Pos] = res.Order.ID
		verdict, trades = resultVerdict(res), res.Trades

	default:
		return "", nil, fmt.Errorf("no driver for kind %s", c.Kind.Name())
	}

	cov.Commands[c.Kind.Name()]++
	if strings.HasPrefix(verdict, "REJECTED") {
		cov.Rejections[strings.TrimPrefix(verdict, "REJECTED:")]++
	}
	cov.Verdicts[strings.FieldsFunc(verdict, func(r rune) bool { return r == ' ' || r == ':' })[0]]++

	events := rec.take()
	for _, e := range events {
		cov.Events[int(e.Kind)]++
		// The axes are counted from an ACCEPTED or REPLACED order, which is an
		// outcome — the order reached the book — rather than from the command that
		// asked for it.
		if e.Order != nil && (e.Kind == matching.EventAccepted || e.Kind == matching.EventReplaced) {
			cov.OrderTypes[string(e.Order.Type)]++
			cov.TIFs[string(e.Order.TimeInForce)]++
		}
		if e.Kind == matching.EventTriggered {
			cov.Triggers++
		}
	}
	cov.CascadeTerminals += cascadeTerminals(events)
	cov.Trades += len(trades)
	if len(trades) > 0 {
		var out []int64
		for _, t := range trades {
			out = append(out, t.ID)
			// An STP mode counts when a trade PRINTED under it, or (below) when it
			// removed a maker; either way from an outcome.
			if c.STP != "" {
				cov.STPModes[c.STP]++
			}
		}
		printed[c.Pos] = out
	}
	// The three cancelling modes never print, so they are counted from the verdict
	// and the cancellation events instead.
	if c.STP != "" && len(trades) == 0 {
		for _, e := range events {
			if e.Kind == matching.EventCanceled || e.Kind == matching.EventReplaced || e.Kind == matching.EventRejected {
				cov.STPModes[c.STP]++
				break
			}
		}
	}

	snap := eng.TakeSnapshot()
	for _, ib := range snap.Icebergs {
		if ib.Refills > refills[ib.OrderID] {
			refills[ib.OrderID] = ib.Refills
		}
	}
	if icebergRestored(c, verdict, prev, snap) {
		cov.IcebergRestores++
	}
	return renderLine(scenario, c, verdict, trades, auction, events, eng, snap), snap, nil
}

// icebergRestored reports whether this command was a fill-or-kill REFUSED while
// priced into a resting iceberg that came back with its reserve and refill counter
// exactly as they were.
//
// It cannot assert the restore happened, and says so rather than pretending: the
// restore's whole point is that a rejected command leaves no trace, so from outside
// the engine "untouched" is the only shape it has. What this does assert is that the
// corpus still REACHES the setup — an iceberg resting, a fill-or-kill crossing it,
// a refusal — which is what the guard in semantics_test.go needs and what the
// golden's own line then pins exactly.
func icebergRestored(c Cmd, verdict string, prev, snap *matching.EngineSnapshot) bool {
	if prev == nil || c.TIF != types.TIFFillOrKill || verdict != "REJECTED:FOK_CANNOT_FILL" {
		return false
	}
	for _, was := range prev.Icebergs {
		slice := restingOrder(prev, was.OrderID)
		if slice == nil || (slice.Side == types.SideSell) == c.Sell {
			continue
		}
		// The taker has to have been priced into it, or the walk never met it.
		if c.Sell && c.Price > slice.Price {
			continue
		}
		if !c.Sell && c.Price < slice.Price {
			continue
		}
		for _, now := range snap.Icebergs {
			if now.OrderID == was.OrderID && now.Hidden == was.Hidden && now.Refills == was.Refills {
				return true
			}
		}
	}
	return false
}

func restingOrder(snap *matching.EngineSnapshot, id int64) *types.Order {
	for _, o := range snap.Orders {
		if o.ID == id {
			return o
		}
	}
	return nil
}

// cascadeTerminals counts the orders this command announced TRIGGERED, then
// ACCEPTED, then CANCELED — a stop fired from inside another command's walk whose
// own order ended without resting.
//
// The ordering test is what tells a cascade-fired stop from one that fires on
// ARRIVAL, and it is not incidental: a stop that fires on arrival is announced
// through emitResult, which composes the order's own ACCEPTED at the FRONT of the
// batch, ahead of the TRIGGERED that emitTriggered recorded during the walk. So
// requiring TRIGGERED before ACCEPTED excludes exactly the path this counter is not
// about.
func cascadeTerminals(events []matching.Event) int {
	var n int
	triggered := map[int64]bool{}
	live := map[int64]bool{}
	for _, e := range events {
		switch e.Kind {
		case matching.EventTriggered:
			triggered[e.OrderID] = true
		case matching.EventAccepted:
			if triggered[e.OrderID] {
				live[e.OrderID] = true
			}
		case matching.EventCanceled:
			if live[e.OrderID] {
				n++
				live[e.OrderID] = false
			}
		}
	}
	return n
}

func buildOrder(c Cmd) (*types.Order, error) {
	side := types.SideBuy
	if c.Sell {
		side = types.SideSell
	}
	ot := c.Type
	if ot == "" {
		ot = types.OrderTypeLimit
	}
	tif := c.TIF
	if tif == "" {
		tif = types.TIFGoodTillCancel
	}
	o, err := types.NewOrder(c.User, Symbol, side, ot, c.Price, c.Qty, tif)
	if err != nil {
		return nil, err
	}
	o.PostOnly = c.PostOnly
	o.STPMode = c.STP
	o.TradeGroupID = c.TradeGroup
	o.Privileged = c.Privileged
	o.ClientOrderID = c.ClientOrderID
	if c.ExpiresIn > 0 {
		o.ExpiresAt = time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC).Add(c.ExpiresIn)
	}
	return o, nil
}

func resultVerdict(res *matching.MatchResult) string {
	if res.RejectionReason != nil {
		return "REJECTED:" + rejectName(res.RejectionReason)
	}
	return string(res.Status)
}

func simpleVerdict(ok string, err error) string {
	if err == nil {
		return ok
	}
	return "REJECTED:" + rejectName(err)
}

// sentinels maps every refusal the corpus can produce to a STABLE NAME. Error strings
// are not a contract and must not be in the golden: rewording one would move the
// fingerprint with no behaviour change, which Rule 22 refuses and which would train
// people to bump for nothing.
//
// A reason with no entry renders as UNMAPPED with its text, which is loud in a diff
// and is the right outcome: a new refusal the corpus can reach is a behaviour the
// fingerprint should be naming.
var sentinels = []struct {
	err  error
	name string
}{
	{types.ErrTradingHalted, "TRADING_HALTED"},
	{types.ErrNewOrdersHalted, "NEW_ORDERS_HALTED"},
	{types.ErrPostOnlyWouldCross, "POST_ONLY_WOULD_CROSS"},
	{types.ErrFOKCannotFill, "FOK_CANNOT_FILL"},
	{types.ErrMarketOrderNoLiquidity, "MARKET_NO_LIQUIDITY"},
	{types.ErrOrderBookFull, "ORDER_BOOK_FULL"},
	{types.ErrOrderNotFound, "ORDER_NOT_FOUND"},
	{types.ErrOrderNotActive, "ORDER_NOT_ACTIVE"},
	{types.ErrInvalidQuantity, "INVALID_QUANTITY"},
	{types.ErrNilOrder, "NIL_ORDER"},
	{types.ErrNotionalOverflow, "NOTIONAL_OVERFLOW"},
	{types.ErrSelfTradeNotAllowed, "SELF_TRADE_NOT_ALLOWED"},
	{types.ErrPriceOutsideBand, "PRICE_OUTSIDE_BAND"},
	{types.ErrOrderTypeDisabled, "ORDER_TYPE_DISABLED"},
	{types.ErrOrderExpired, "ORDER_EXPIRED"},
	{types.ErrNoSessionClose, "NO_SESSION_CLOSE"},
	{types.ErrInvalidExpiry, "INVALID_EXPIRY"},
	{types.ErrPegReferenceUnavailable, "PEG_REFERENCE_UNAVAILABLE"},
	{types.ErrCancelTooSoon, "CANCEL_TOO_SOON"},
	{types.ErrMarkStepTooLarge, "MARK_STEP_TOO_LARGE"},
	{types.ErrMarkDepthTooThin, "MARK_DEPTH_TOO_THIN"},
	{types.ErrOrderExceedsMaxQty, "ORDER_EXCEEDS_MAX_QTY"},
	{types.ErrOrderExceedsMaxNotional, "ORDER_EXCEEDS_MAX_NOTIONAL"},
	{types.ErrOrderBelowMinQty, "ORDER_BELOW_MIN_QTY"},
	{types.ErrOrderBelowMinNotional, "ORDER_BELOW_MIN_NOTIONAL"},
	{types.ErrTooManyOrders, "TOO_MANY_ORDERS"},
	{types.ErrDuplicateClientOrderID, "DUPLICATE_CLIENT_ORDER_ID"},
	{types.ErrInvalidStopPrice, "INVALID_STOP_PRICE"},
	{matching.ErrUnknownTrade, "UNKNOWN_TRADE"},
	{matching.ErrAlreadyBusted, "ALREADY_BUSTED"},
}

func rejectName(err error) string {
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			return s.name
		}
	}
	return "UNMAPPED(" + err.Error() + ")"
}

// renderLine is one command's whole observable outcome, on one line.
//
// One line per command is a deliberate trade. A multi-line block reads better in
// isolation; a single line makes the diff say WHICH COMMAND'S OUTCOME MOVED, which is
// what turns "the fingerprint changed" into "the pro-rata walk now cancels the taker",
// and that reading is the entire point of keeping a golden at all.
func renderLine(scenario string, c Cmd, verdict string, trades []*types.Trade, auction []types.Trade,
	events []matching.Event, eng *matching.Engine, snap *matching.EngineSnapshot) string {

	var b strings.Builder
	// The scenario name is on EVERY line and not only in the section header, because
	// the failure this file exists to produce prints a handful of moved lines out of
	// context. A line that does not say which engine configuration produced it turns
	// "the fingerprint changed" back into a hunt.
	fmt.Fprintf(&b, "%s/%04d %-11s %-24s | %-28s |", scenario, c.Pos, c.Kind.Name(), describe(c), verdict)

	b.WriteString(" T[")
	for i, t := range trades {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d:%dx%d b%d s%d m%d t%d %s seq%d", t.ID, t.Price, t.Quantity,
			t.BuyOrderID, t.SellOrderID, t.MakerOrderID, t.TakerOrderID, sideName(t.TakerSide), t.SequenceNum)
		if t.Auction {
			b.WriteString(" auc")
		}
	}
	for i := range auction {
		t := auction[i]
		if i > 0 || len(trades) > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d:%dx%d b%d s%d %s seq%d auc", t.ID, t.Price, t.Quantity,
			t.BuyOrderID, t.SellOrderID, sideName(t.TakerSide), t.SequenceNum)
	}
	b.WriteString("] E[")
	for i, e := range events {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s#%d", e.Kind, e.OrderID)
		if e.Reason != nil {
			b.WriteString("/" + rejectName(e.Reason))
		}
		if e.Kind == matching.EventTrade && e.Trade != nil {
			fmt.Fprintf(&b, "/%dx%d", e.Trade.Price, e.Trade.Quantity)
		}
		if e.Kind == matching.EventBusted {
			fmt.Fprintf(&b, "/t%d", e.TradeID)
		}
		if e.Order != nil {
			fmt.Fprintf(&b, "/%s/%d", e.Order.Status, e.Order.FilledQty)
		}
	}
	b.WriteString("] ")

	bid, bidQty, haveBid := eng.Book().BestBid()
	ask, askQty, haveAsk := eng.Book().BestAsk()
	fmt.Fprintf(&b, "| %s last=%d mark=%d bid=%s ask=%s n=%d oid=%d tid=%d eid=%d | %s",
		eng.State(), eng.LastTradePrice(), eng.MarkPrice(),
		levelText(bid, bidQty, haveBid), levelText(ask, askQty, haveAsk),
		eng.OrderCount(), snap.Seq, snap.TradeSeq, snap.EventSeq, shortDigest(snap))
	if c.Note != "" {
		b.WriteString("  # " + c.Note)
	}
	return b.String()
}

func levelText(price, qty int64, ok bool) string {
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%d:%d", price, qty)
}

func sideName(s types.Side) string {
	if s == types.SideSell {
		return "S"
	}
	return "B"
}

// shortDigest is the first sixteen hex characters of EngineSnapshot.Digest.
//
// Sixteen rather than sixty-four because the golden is read by people: sixty-four
// characters on every line of a hundred-kilobyte file buys collision resistance
// nothing here needs (this is change detection, not a commitment) at the cost of the
// line being readable at all.
func shortDigest(snap *matching.EngineSnapshot) string {
	return snap.Digest()[:16]
}

func describe(c Cmd) string {
	switch c.Kind {
	case Submit, Stop, OCO, Iceberg, Pegged, Trailing, Replace:
		side := "B"
		if c.Sell {
			side = "S"
		}
		ot := string(c.Type)
		if ot == "" {
			ot = "LIMIT"
		}
		tif := string(c.TIF)
		if tif == "" {
			tif = "GTC"
		}
		s := fmt.Sprintf("%s %s %s %dx%d %s", c.User, side, ot[:1], c.Price, c.Qty, tif)
		if c.PostOnly {
			s += "+PO"
		}
		if c.STP != "" {
			s += "+" + c.STP
		}
		if c.TradeGroup != 0 {
			s += fmt.Sprintf("+g%d", c.TradeGroup)
		}
		if c.Privileged {
			s += "+priv"
		}
		if c.Kind == Replace {
			s += fmt.Sprintf(" <-%d", c.Target)
		}
		if c.StopPrice != 0 {
			s += fmt.Sprintf(" stop@%d", c.StopPrice)
		}
		if c.DisplayQty != 0 {
			s += fmt.Sprintf(" disp%d", c.DisplayQty)
		}
		if c.PegRef != "" {
			s += fmt.Sprintf(" peg%s%+d", c.PegRef, c.PegOffset)
		}
		if c.Trail != 0 {
			s += fmt.Sprintf(" trail%d", c.Trail)
		}
		return s
	case Cancel:
		return fmt.Sprintf("%s <-%d", c.User, c.Target)
	case Reduce:
		return fmt.Sprintf("%s <-%d to %d", c.User, c.Target, c.NewQty)
	case CancelAll:
		return c.User
	case SetMark:
		return fmt.Sprintf("%d", c.MarkPrice)
	case SetPhase:
		return phaseOrder[int(c.Phase)%len(phaseOrder)].String()
	case Bust:
		return fmt.Sprintf("<-%d#%d", c.TradeRef, c.TradeNth)
	}
	return ""
}

// Render drives the whole corpus and returns the golden's body plus what it reached.
//
// The first line of the returned text is the version this body was recorded at. It is
// deliberately part of the file rather than kept beside it: the two travel together
// through every copy, diff and review, and the rule that ties them — a body change
// demands a version change and vice versa — is only checkable if both are in one
// place.
func Render() (string, *Coverage, error) {
	cov := newCoverage()
	var b strings.Builder
	fmt.Fprintf(&b, "semantics %d\n", matching.SemanticsVersion)
	b.WriteString("# The behaviour fingerprint. See docs/SEMANTICS-VERSION.md §5.\n")
	b.WriteString("# scenario/pos kind command | verdict | T[trades] E[events] | state last mark bid ask counters | digest\n")

	for _, sc := range Corpus() {
		if err := run(sc, &b, cov); err != nil {
			return "", nil, err
		}
	}
	renderCoverage(&b, cov)
	return b.String(), cov, nil
}

// renderCoverage closes the file with aggregate counts.
//
// It is here because a coverage regression that leaves every individual line
// untouched still has to show up in the diff. A profile change that quietly stops
// running the closing uncross moves no command's outcome and drains the auction count
// to zero, and this block is the only thing that would say so.
func renderCoverage(b *strings.Builder, cov *Coverage) {
	b.WriteString("\n== coverage\n")
	writeCounts(b, "commands", cov.Commands)
	writeIntCounts(b, "events", cov.Events, func(k int) string { return matching.EventKind(k).String() })
	writeCounts(b, "verdicts", cov.Verdicts)
	writeCounts(b, "rejections", cov.Rejections)
	writeCounts(b, "orderTypes", cov.OrderTypes)
	writeCounts(b, "timeInForce", cov.TIFs)
	writeCounts(b, "stpModes", cov.STPModes)
	writeCounts(b, "policies", cov.Policies)
	fmt.Fprintf(b, "trades %d\nauctionTrades %d\nuncrosses %d\ntriggers %d\nrefills %d\nbusts %d\n",
		cov.Trades, cov.AuctionTrades, cov.Uncrosses, cov.Triggers, cov.Refills, cov.Busts)
	fmt.Fprintf(b, "icebergRestores %d\ncascadeTerminals %d\n", cov.IcebergRestores, cov.CascadeTerminals)
}

func writeCounts(b *strings.Builder, label string, m map[string]int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(b, "%s:", label)
	for _, k := range keys {
		fmt.Fprintf(b, " %s=%d", k, m[k])
	}
	b.WriteByte('\n')
}

func writeIntCounts(b *strings.Builder, label string, m map[int]int, name func(int) string) {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	fmt.Fprintf(b, "%s:", label)
	for _, k := range keys {
		fmt.Fprintf(b, " %s=%d", name(k), m[k])
	}
	b.WriteByte('\n')
}

// Digest is a stable hash of a rendered body, used only by the determinism check.
func Digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
