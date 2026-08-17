// Package refmatch is a reference implementation of this venue's continuous-session
// matching semantics, written to be READ rather than to be fast.
//
// It exists to be an oracle for the optimised engine in pkg/matching, and it is only
// worth something if it shares no code with the thing it checks. So it imports
// nothing outside the standard library — not pkg/types, not pkg/orderbook — declares
// its own small enums and its own plain-int64 order and trade structs, and does its
// own arithmetic. In particular it does NOT call types.Order.Fill: that is where
// "filled + remaining == quantity" is maintained, and an invariant both sides get
// from the same seven lines is an invariant nothing is checking.
// TestReferenceMatcherImportsNothing enforces the rule mechanically.
//
// The representation is two sorted slices per book, scanned linearly and re-sorted
// on insert. Cancel is a linear search. Depth is a fold. There is no index, no pool,
// no id→node map and no free list. That is the point and not a weakness: the
// production book maintains a level aggregate incrementally through push/unlink with
// a per-node "contributed" value that is deliberately not the order's live remaining
// quantity, and this model has no aggregate to maintain — it sums the orders that are
// actually there. Comparing a maintained total against an independently computed one
// is the only thing in this repository that can catch the two drifting apart on a
// generated command tape.
//
// It must stay O(n) per command and must never be optimised. If the differential
// harness gets slow the answer is shorter tapes or fewer seeds; a model with an index
// has the engine's bug class.
//
// Scope is the "tier 1" alphabet of docs/REFERENCE-MATCHER.md §2.3: limit and market
// orders; GTC, IOC and FOK; post-only, privileged, trade-group and per-order STP
// mode; submit, cancel, reduce, replace and cancel-all-for-user; halt, resume and
// cancel-only; all five self-trade-prevention modes; price-time and pro-rata
// allocation; and the book-size cap. Time-based time-in-force, stops, OCO, icebergs,
// pegged and trailing orders, the auction, trade busts and every admission control
// are deliberately out (§2.4), and the differential harness's configuration guard is
// what keeps those absences visible.
package refmatch

import "sort"

// --- vocabulary -------------------------------------------------------------
//
// The model's own enums, not the production string constants. Sharing those would
// be the first step of sharing the semantics attached to them.

// Side is the direction of an order.
type Side uint8

const (
	Buy Side = iota
	Sell
)

// OrdType is how an order interacts with the book.
type OrdType uint8

const (
	Limit OrdType = iota
	Market
)

// TIF is how long an order stays live. Tier 1 models the three that are pure
// functions of the book; DAY and GTD are functions of a clock and are deferred.
type TIF uint8

const (
	GTC TIF = iota
	IOC
	FOK
)

// STP is a self-trade-prevention mode. Unset means "use the venue default".
type STP uint8

const (
	STPUnset STP = iota
	CancelNewest
	CancelOldest
	CancelBoth
	Decrement
	Allow
)

// Status is an order's terminal verdict for one command.
type Status uint8

const (
	// StatusNA is the verdict of a command that is not about one order — a halt,
	// a resume, a cancel-only transition.
	StatusNA Status = iota
	StatusNew
	StatusPartiallyFilled
	StatusFilled
	StatusCancelled
	StatusRejected
	// StatusReduced is the verdict of a successful in-place size reduction. The
	// production API reports it as (order, nil) rather than as an order status, so
	// the model gives it a name instead of borrowing one that means something else.
	StatusReduced
)

// Reject names why a command was refused. It is an enum and not an error string
// because an error string is not a contract; which sentinel was returned is.
type Reject uint8

const (
	RejectNone Reject = iota
	RejectTradingHalted
	RejectNewOrdersHalted
	RejectPostOnlyWouldCross
	RejectFOKCannotFill
	RejectMarketNoLiquidity
	RejectOrderBookFull
	RejectOrderNotFound
	RejectOrderNotActive
	RejectInvalidQuantity
	RejectNilOrder
	RejectNotionalOverflow
)

// State is the venue's trading state. Tier 1 models the three continuous-session
// states; the accumulating auction phases are deferred (§2.4).
type State uint8

const (
	Open State = iota
	CancelOnlyState
	Halted
)

// EventKind is one entry kind of the ordered event stream.
type EventKind uint8

const (
	EvAccepted EventKind = iota
	EvRejected
	EvTrade
	EvCanceled
	EvReplaced
	EvHalted
	EvResumed
	EvCancelOnly
)

// --- observable values ------------------------------------------------------

// Order is a submission: what a client asks for, before the venue names it.
type Order struct {
	User       string
	Side       Side
	Type       OrdType
	Price      int64
	Qty        int64
	TIF        TIF
	PostOnly   bool
	STPMode    STP
	TradeGroup int64
	Privileged bool
}

// Resting is one order as it sits in the book. Qty is the CURRENT total, which is
// not the submitted total: a self-trade-prevention decrement and a reduce both
// shrink it.
type Resting struct {
	ID        int64
	User      string
	Side      Side
	Price     int64
	Qty       int64
	Filled    int64
	Remaining int64
	PostOnly  bool
}

// Trade is one execution. It prints at the maker's resting price.
type Trade struct {
	ID           int64
	Symbol       string
	Price        int64
	Qty          int64
	BuyOrderID   int64
	SellOrderID  int64
	BuyerUser    string
	SellerUser   string
	MakerOrderID int64
	TakerOrderID int64
	TakerSide    Side
	Seq          int64
	Auction      bool
}

// Level is one aggregated price level: the L2 view.
type Level struct {
	Side     Side
	Price    int64
	TotalQty int64
	Count    int
}

// Event is one entry of the ordered event stream. Sequence numbers are not here:
// the model does not assign them, and the harness pins them by asserting the
// engine's own stream is dense from 1 and strictly monotonic, which given a
// matching ordered list leaves exactly one possible value per event.
//
// Price and Qty are the payload of an EvTrade and zero on every other kind. They are
// here because without them the "ordered event stream" being compared was really an
// ordered list of (kind, subject, user, reason), and the numbers a consumer actually
// reads off a fill were unchecked: attaching a corrupted Trade to every trade event
// (price + 7, quantity doubled) left the whole of pkg/matching green, and was caught
// only by gateway and marketdata tests that read the payload downstream. The
// harness's own trade comparison comes from MatchResult.Trades, a DIFFERENT source
// than the event payload, so the two could disagree indefinitely.
type Event struct {
	Kind    EventKind
	OrderID int64
	User    string
	Reason  Reject
	Price   int64
	Qty     int64
}

// Verdict is a command's outcome: the status, plus the reason when refused.
type Verdict struct {
	Status Status
	Reason Reject
}

// Observation is everything both sides must agree on after one command. It is a
// plain comparable value — no pointers, no time.Time — and the harness compares it
// WHOLE with reflect.DeepEqual rather than field by field, because a field-list
// comparator is exactly where a future field silently stops being checked. A field
// that legitimately cannot be compared does not get skipped in the comparator; it
// does not go in this struct at all.
type Observation struct {
	Verdict        Verdict
	Trades         []Trade
	Book           []Resting
	Levels         []Level
	Events         []Event
	State          State
	LastTradePrice int64
	NextOrderID    int64
	NextTradeID    int64
}

// Result is one command's own output, before it is folded into an Observation.
type Result struct {
	Verdict Verdict
	Trades  []Trade
	Events  []Event
}

// --- the model ---------------------------------------------------------------

// Config is the subset of venue configuration tier 1 models. Every other knob the
// production engine has must be at its disabling value when the harness runs, and
// the harness has a reflection guard that fails if one is not.
type Config struct {
	Symbol     string
	STP        STP // the venue default, used when an order names no mode
	ProRata    bool
	MaxOrders  int
	ShardIndex int
}

// order is a live order inside the model. arrival is its position in the global
// arrival sequence, which is what "time priority" means here.
type order struct {
	Resting
	Type       OrdType
	TIF        TIF
	STPMode    STP
	TradeGroup int64
	Privileged bool
	Status     Status
	arrival    int64
}

// Model is a single-symbol reference matcher.
type Model struct {
	cfg  Config
	bids []*order // price descending, then arrival ascending
	asks []*order // price ascending,  then arrival ascending

	arrival  int64
	orderSeq int64
	tradeSeq int64
	last     int64
	state    State

	// stpDecisions counts, per mode, how many times self-trade prevention actually
	// DECIDED a match. It is instrumentation for a guard, not state the matcher
	// reads: nothing in this package branches on it.
	//
	// It exists because the obvious guard — "every STP mode appears on the tape" —
	// counts draws, and a mode that is drawn but never meets its own resting
	// liquidity is a mode the sweep reports as exercised and never exercises. That
	// is the same kind-count-instead-of-outcome mistake the reduce incident in
	// internal/tape cost a round to find.
	stpDecisions [6]int

	// trades and pending accumulate one command's output. pending is the event
	// stream as it happens DURING matching; the terminal events are composed around
	// it afterwards, which is the order a consumer sees.
	trades  []Trade
	pending []Event
	// touched holds the makers this command traded with, by id, so a fill-or-kill
	// reversal can reach one that has already been lifted out of the book.
	touched map[int64]*order
}

// New builds an empty model.
func New(cfg Config) *Model {
	if cfg.STP == STPUnset {
		cfg.STP = CancelNewest
	}
	if cfg.MaxOrders == 0 {
		cfg.MaxOrders = 100_000
	}
	return &Model{cfg: cfg, state: Open}
}

// seqBits is the width of the per-shard counter in a venue-wide id: the low 48
// bits are the counter and the bits above them name the shard, so shard 0 composes
// to the sequence itself. Written from the rule rather than by calling the
// engine's ComposeID, which is the whole point of this package.
const seqBits = 48

func composeID(shard int, seq int64) int64 { return int64(shard)<<seqBits | seq }

// NextOrderID is the id the next submitted order will be given.
func (m *Model) NextOrderID() int64 { return composeID(m.cfg.ShardIndex, m.orderSeq+1) }

// NextTradeID is the id the next print will be given.
func (m *Model) NextTradeID() int64 { return composeID(m.cfg.ShardIndex, m.tradeSeq+1) }

// State reports the trading state.
func (m *Model) State() State { return m.state }

// STPDecisions reports how many times each self-trade-prevention mode actually
// decided a match, indexed by STP. A guard uses it to assert the sweep REACHES every
// mode rather than merely drawing it.
func (m *Model) STPDecisions() [6]int { return m.stpDecisions }

// LastTradePrice is the price of the most recent print.
//
// It is deliberately NOT unwound when a fill-or-kill order fails and its trades are
// reversed: the reference price a venue quotes moved when the print happened, and
// putting it back would be a second rule nobody wrote down. See §9(a) of
// docs/REFERENCE-MATCHER.md — the model takes a position here, and if the engine's
// position differs, one of them is a defect.
func (m *Model) LastTradePrice() int64 { return m.last }

// Book returns the whole resting book as a ranked L3 list: bids best-price-first
// then asks best-price-first, and within a price level in arrival order.
//
// Rank, not membership. Membership alone is satisfied by a reduce that re-queues, a
// level that pushes at the head and a refill that lands in the wrong place.
func (m *Model) Book() []Resting {
	out := make([]Resting, 0, len(m.bids)+len(m.asks))
	for _, o := range m.bids {
		out = append(out, o.Resting)
	}
	for _, o := range m.asks {
		out = append(out, o.Resting)
	}
	return out
}

// Levels returns the L2 aggregate SUMMED from the resting orders, best price first
// per side. The engine's side of this comparison comes from a total it maintains
// incrementally; that asymmetry is the entire value of the assertion and collapsing
// it into two sums would destroy it.
func (m *Model) Levels() []Level {
	out := make([]Level, 0, 8)
	fold := func(side Side, orders []*order) {
		for i := 0; i < len(orders); {
			p := orders[i].Price
			l := Level{Side: side, Price: p}
			for ; i < len(orders) && orders[i].Price == p; i++ {
				l.TotalQty += orders[i].Remaining
				l.Count++
			}
			out = append(out, l)
		}
	}
	fold(Buy, m.bids)
	fold(Sell, m.asks)
	return out
}

// Observe folds the model's state and one command's result into an Observation.
// Both sides build this value through their own constructor and the two are
// compared whole; the normalisation that keeps nil and empty slices from being a
// false difference lives here and in the engine adapter, identically, rather than
// as an exception inside a comparator.
func (m *Model) Observe(r Result) Observation {
	return Observation{
		Verdict:        r.Verdict,
		Trades:         nonNilTrades(r.Trades),
		Book:           m.Book(),
		Levels:         m.Levels(),
		Events:         nonNilEvents(r.Events),
		State:          m.state,
		LastTradePrice: m.last,
		NextOrderID:    m.NextOrderID(),
		NextTradeID:    m.NextTradeID(),
	}
}

func nonNilTrades(t []Trade) []Trade {
	if t == nil {
		return []Trade{}
	}
	return t
}

func nonNilEvents(e []Event) []Event {
	if e == nil {
		return []Event{}
	}
	return e
}

// --- book primitives ---------------------------------------------------------

func (m *Model) side(s Side) *[]*order {
	if s == Buy {
		return &m.bids
	}
	return &m.asks
}

// rest inserts o and restores the side's ordering. Appending then sorting is
// O(n log n) where the engine is O(log P); that is the trade this package exists
// to make.
func (m *Model) rest(o *order) {
	m.arrival++
	o.arrival = m.arrival
	m.insert(o)
}

// insert places o without touching its arrival number, so a restored order keeps
// the priority it had.
func (m *Model) insert(o *order) {
	s := m.side(o.Side)
	*s = append(*s, o)
	orders := *s
	desc := o.Side == Buy
	sort.SliceStable(orders, func(i, j int) bool {
		if orders[i].Price != orders[j].Price {
			if desc {
				return orders[i].Price > orders[j].Price
			}
			return orders[i].Price < orders[j].Price
		}
		return orders[i].arrival < orders[j].arrival
	})
}

// find locates a resting order by id, linearly.
func (m *Model) find(id int64) *order {
	for _, o := range m.bids {
		if o.ID == id {
			return o
		}
	}
	for _, o := range m.asks {
		if o.ID == id {
			return o
		}
	}
	return nil
}

func (m *Model) remove(id int64) {
	for _, s := range []*[]*order{&m.bids, &m.asks} {
		for i, o := range *s {
			if o.ID == id {
				*s = append((*s)[:i], (*s)[i+1:]...)
				return
			}
		}
	}
}

func (m *Model) count() int { return len(m.bids) + len(m.asks) }

// ordersAt returns the resting orders on a side at exactly price, in arrival
// order.
func (m *Model) ordersAt(s Side, price int64) []*order {
	var out []*order
	for _, o := range *m.side(s) {
		if o.Price == price {
			out = append(out, o)
		}
	}
	return out
}

func (o *order) active() bool {
	return o.Status == StatusNew || o.Status == StatusPartiallyFilled
}

func (o *order) fill(q int64) {
	o.Filled += q
	o.Remaining -= q
	if o.Remaining == 0 {
		o.Status = StatusFilled
	} else {
		o.Status = StatusPartiallyFilled
	}
}

// --- event helpers -----------------------------------------------------------

func (m *Model) emit(k EventKind, o *order) {
	m.pending = append(m.pending, Event{Kind: k, OrderID: o.ID, User: o.User})
}

func (m *Model) begin() {
	m.trades = nil
	m.pending = nil
	m.touched = make(map[int64]*order)
}

// flush takes the events recorded during a command that composes no terminal
// event of its own — a bare cancel, a reduce, a cancel-all.
func (m *Model) flush() []Event {
	evs := m.pending
	m.pending = nil
	return evs
}

// --- matching ----------------------------------------------------------------

// selfMatch reports whether taker and maker must not trade with each other: the
// same account, or a shared non-zero trade group. A privileged taker is exempt, so
// a liquidation is never self-blocked.
func (m *Model) selfMatch(taker, maker *order) bool {
	if taker.Privileged {
		return false
	}
	if taker.User == maker.User {
		return true
	}
	return taker.TradeGroup != 0 && taker.TradeGroup == maker.TradeGroup
}

// takerSTP is the mode governing this match: the taker's own if it named one, else
// the venue default. The taker's mode decides, as real venues do.
func (m *Model) takerSTP(taker *order) STP {
	if taker.STPMode != STPUnset {
		return taker.STPMode
	}
	return m.cfg.STP
}

// execute prints one trade at price for qty, filling both sides.
func (m *Model) execute(taker, maker *order, price, qty int64) {
	taker.fill(qty)
	maker.fill(qty)
	m.touched[maker.ID] = maker
	m.tradeSeq++

	buy, sell := taker, maker
	if taker.Side == Sell {
		buy, sell = maker, taker
	}
	m.trades = append(m.trades, Trade{
		ID:           composeID(m.cfg.ShardIndex, m.tradeSeq),
		Symbol:       m.cfg.Symbol,
		Price:        price,
		Qty:          qty,
		BuyOrderID:   buy.ID,
		SellOrderID:  sell.ID,
		BuyerUser:    buy.User,
		SellerUser:   sell.User,
		MakerOrderID: maker.ID,
		TakerOrderID: taker.ID,
		TakerSide:    taker.Side,
		Seq:          m.tradeSeq,
	})
	m.pending = append(m.pending, Event{Kind: EvTrade, OrderID: taker.ID, Price: price, Qty: qty})
	m.last = price
}

// matchFIFO walks the opposite book by price then arrival time, printing at the
// maker's resting price, until the taker is exhausted or the next maker is out of
// the taker's limit.
func (m *Model) matchFIFO(taker *order) {
	for taker.Remaining != 0 {
		var maker *order
		if taker.Side == Buy {
			if len(m.asks) == 0 {
				return
			}
			maker = m.asks[0]
			// A limit buy only crosses asks at or below its price.
			if taker.Type == Limit && taker.Price < maker.Price {
				return
			}
		} else {
			if len(m.bids) == 0 {
				return
			}
			maker = m.bids[0]
			// A limit sell only crosses bids at or above its price.
			if taker.Type == Limit && taker.Price > maker.Price {
				return
			}
		}

		if m.selfMatch(taker, maker) {
			m.stpDecisions[m.takerSTP(taker)]++
			switch m.takerSTP(taker) {
			case CancelNewest:
				taker.Status = StatusCancelled
				return
			case CancelOldest:
				maker.Status = StatusCancelled
				m.remove(maker.ID)
				m.emit(EvCanceled, maker)
				continue
			case CancelBoth:
				taker.Status = StatusCancelled
				maker.Status = StatusCancelled
				m.remove(maker.ID)
				m.emit(EvCanceled, maker)
				return
			case Decrement:
				// Both sides lose their overlap with no trade to explain it: the
				// smaller fully cancels, the larger shrinks in place and keeps its
				// queue position.
				d := min(taker.Remaining, maker.Remaining)
				taker.Qty -= d
				taker.Remaining -= d
				maker.Qty -= d
				maker.Remaining -= d
				if maker.Remaining == 0 {
					m.remove(maker.ID)
					m.emit(EvCanceled, maker)
				} else {
					m.emit(EvReplaced, maker)
				}
				if taker.Remaining == 0 {
					taker.Status = StatusCancelled
					return
				}
				continue
			case Allow:
				// fall through and trade
			}
		}

		m.execute(taker, maker, maker.Price, min(taker.Remaining, maker.Remaining))
		if maker.Remaining == 0 {
			m.remove(maker.ID)
		}
	}
}

// matchProRata allocates each price level in proportion to resting size instead of
// by arrival order. Self orders are skipped rather than STP-cancelled.
//
// The remainder rule is arbitrary — nothing in microstructure forces it, and a
// different venue would round differently — so it is specified in prose at
// docs/REFERENCE-MATCHER.md §2.3 and this implements that paragraph rather than
// transcribing the engine's loop.
func (m *Model) matchProRata(taker *order) {
	for taker.Remaining != 0 {
		var price int64
		var opp Side
		if taker.Side == Buy {
			if len(m.asks) == 0 {
				return
			}
			price, opp = m.asks[0].Price, Sell
			if taker.Type == Limit && taker.Price < price {
				return
			}
		} else {
			if len(m.bids) == 0 {
				return
			}
			price, opp = m.bids[0].Price, Buy
			if taker.Type == Limit && taker.Price > price {
				return
			}
		}

		var eligible []*order
		var total int64
		for _, o := range m.ordersAt(opp, price) {
			if m.selfMatch(taker, o) {
				continue
			}
			eligible = append(eligible, o)
			total += o.Remaining
		}
		// A level holding only the taker's own liquidity ends the walk rather than
		// skipping to the next level.
		if total == 0 {
			return
		}

		q := min(taker.Remaining, total)
		allocs := proRata(eligible, q)
		for i, maker := range eligible {
			if allocs[i] <= 0 {
				continue
			}
			m.execute(taker, maker, price, allocs[i])
			if maker.Remaining == 0 {
				m.remove(maker.ID)
			}
		}
	}
}

// proRata allocates q across orders in arrival order: each is provisionally given
// min(q * remaining_i / total, remaining_i) by truncating integer division, and the
// shortfall is then handed out one order at a time in arrival order, each taking as
// much as its unallocated remainder allows.
func proRata(orders []*order, q int64) []int64 {
	var total int64
	for _, o := range orders {
		total += o.Remaining
	}
	allocs := make([]int64, len(orders))
	var given int64
	for i, o := range orders {
		a := q * o.Remaining / total
		if a > o.Remaining {
			a = o.Remaining
		}
		allocs[i] = a
		given += a
	}
	short := q - given
	for i, o := range orders {
		if short <= 0 {
			break
		}
		add := min(o.Remaining-allocs[i], short)
		if add > 0 {
			allocs[i] += add
			short -= add
		}
	}
	return allocs
}

// wouldCross reports whether a limit order would take liquidity immediately, which
// is what a post-only order is refused for. A non-limit order always crosses.
func (m *Model) wouldCross(o *order) bool {
	if o.Type != Limit {
		return true
	}
	if o.Side == Buy {
		return len(m.asks) > 0 && o.Price >= m.asks[0].Price
	}
	return len(m.bids) > 0 && o.Price <= m.bids[0].Price
}

// reverseFrom unwinds every print from index start onward — the fill-or-kill
// failure path. Makers get their quantity back; one that had been fully consumed
// comes back into the book, and it comes back at the TAIL of its level, because
// nothing has kept its old place. A maker that self-trade-prevention removed is not
// restored: it was never traded with, so there is no print naming it to reverse.
//
// The trade sequence counter is NOT rewound. A counter that goes backwards is a
// counter that can name two different prints with one id, so a rejected FOK burns
// trade ids that no trade will ever carry.
func (m *Model) reverseFrom(start int) {
	for i := start; i < len(m.trades); i++ {
		tr := m.trades[i]
		maker, ok := m.touched[tr.MakerOrderID]
		if !ok {
			continue
		}
		maker.Remaining += tr.Qty
		maker.Filled -= tr.Qty
		if maker.Filled == 0 {
			maker.Status = StatusNew
		} else {
			maker.Status = StatusPartiallyFilled
		}
		if m.find(maker.ID) == nil {
			// Fully consumed and lifted out: it comes back, at the back of the
			// queue, because nothing kept its old place.
			m.rest(maker)
		}
	}
}
