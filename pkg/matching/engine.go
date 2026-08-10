// Package matching implements a lean, deterministic matching engine over the
// price–time-priority order book in package orderbook.
//
// It is a focused re-implementation of the core matching algorithm from a
// production exchange engine, with the exchange-compliance machinery (WAL,
// anti-manipulation, compliance, events, settlement) deliberately left out —
// those belong to layers above the core library (see docs/SPEC.md §3). What
// remains is the essential loop: take an incoming order, cross it against the
// resting book by price then time, print trades at the maker's price, and rest
// or reject the remainder according to order type and time-in-force.
//
// Prices are integer ticks and quantities integer lots (int64). Orders and
// trades carry engine-assigned monotonic int64 ids. The core matches into a
// caller-supplied trade buffer (see Match) with no per-order/per-trade heap
// allocation; Process wraps it as the ergonomic *MatchResult API.
package matching

import (
	"math"
	"sort"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

// SelfTradePrevention controls what happens when an incoming order would match
// a resting order from the same user.
type SelfTradePrevention string

const (
	// STPCancelNewest cancels the remaining incoming order (the default).
	STPCancelNewest SelfTradePrevention = "CANCEL_NEWEST"
	// STPCancelOldest cancels the resting maker and continues matching.
	STPCancelOldest SelfTradePrevention = "CANCEL_OLDEST"
	// STPCancelBoth cancels both the incoming order and the resting maker.
	STPCancelBoth SelfTradePrevention = "CANCEL_BOTH"
	// STPDecrement reduces both orders by their overlapping quantity without a
	// trade (the smaller side fully cancels, the larger shrinks) and continues —
	// the modern Binance default. Preserves the most liquidity of the four.
	STPDecrement SelfTradePrevention = "DECREMENT"
	// STPAllow permits the self-trade to execute.
	STPAllow SelfTradePrevention = "ALLOW"
)

// EngineState is the engine's trading state. It moves between Open, CancelOnly
// (accept cancels, reject new liquidity — the venue-wind-down state Coinbase
// restarted through: cancel-only → auction → full), and Halted (reject
// everything).
type EngineState uint8

const (
	StateOpen       EngineState = iota // normal trading
	StateCancelOnly                    // cancels only; new orders rejected
	StateHalted                        // all orders rejected
	// StatePreOpen accepts limit orders and does NOT match them, so the book may
	// legitimately cross. The crossed book is resolved by Uncross, which is what an
	// opening auction is. Market orders are refused: there is no price to trade at
	// until the uncross has decided one.
	StatePreOpen
	// StateClosed is after the session: no new liquidity, cancels still accepted so a
	// participant can clear its book before the next session.
	StateClosed
	// StateClosingAuction accumulates orders on top of the continuous book without
	// matching them, exactly as pre-open does, and the transition out of it resolves
	// everything at one closing price. The difference from pre-open is only where it
	// starts: pre-open begins with whatever survived the last session, a closing
	// auction begins with a live, already-uncrossed book.
	StateClosingAuction
)

// accumulating reports whether a phase takes orders without matching them, so the
// book may cross and must be resolved by an uncross on the way out.
func (s EngineState) accumulating() bool {
	return s == StatePreOpen || s == StateClosingAuction
}

// String names the state, for logs and operator tooling.
func (s EngineState) String() string {
	switch s {
	case StateCancelOnly:
		return "CANCEL_ONLY"
	case StateHalted:
		return "HALTED"
	case StatePreOpen:
		return "PRE_OPEN"
	case StateClosingAuction:
		return "CLOSING_AUCTION"
	case StateClosed:
		return "CLOSED"
	default:
		return "OPEN"
	}
}

// Guardrail is an optional self-output safety valve. If the engine prints more
// than MaxTrades trades (or MaxNotional in tick·lot units) within Window, it
// trips itself to Halted — the Knight Capital lesson: guard the engine's *own*
// output, not just incoming prices. Zero MaxTrades and MaxNotional (or zero
// Window) disable it.
type Guardrail struct {
	MaxTrades   int
	MaxNotional int64
	Window      time.Duration
}

func (g Guardrail) enabled() bool {
	return g.Window > 0 && (g.MaxTrades > 0 || g.MaxNotional > 0)
}

// OrderClass names a gate-able family of advanced order types. Listing a class
// in Config.DisabledClasses makes the engine reject that family with
// ErrOrderTypeDisabled — so one buggy exotic type can be switched off without
// halting the whole venue (the ASX combination-order and Binance trailing-stop
// lesson). Plain limit/market orders cannot be disabled.
type OrderClass string

const (
	ClassStop     OrderClass = "STOP"     // stop / stop-limit (ProcessStop)
	ClassIceberg  OrderClass = "ICEBERG"  // iceberg (ProcessIceberg)
	ClassPegged   OrderClass = "PEGGED"   // pegged (ProcessPegged)
	ClassOCO      OrderClass = "OCO"      // one-cancels-other (ProcessOCO)
	ClassTrailing OrderClass = "TRAILING" // trailing stop (ProcessTrailingStop)
)

// Config configures an Engine.
type Config struct {
	Symbol              string
	SelfTradePrevention SelfTradePrevention
	MaxOrders           int
	// ShardIndex partitions this engine's id space so order and trade ids are
	// unique across a multi-symbol venue. Zero — the default, and every
	// single-symbol deployment — leaves ids exactly as they were.
	//
	// It is CONFIG, not state, and that is what makes recovery work: replaying one
	// shard's log reproduces its ids because the index comes back from the venue
	// manifest rather than from the interleaving of other shards. Change it for a
	// symbol that has already traded and every id it issued becomes ambiguous. See
	// docs/MULTI-SYMBOL.md §4.1.
	ShardIndex int
	// PriceBand is a circuit-breaker collar: a limit order priced more than this
	// fraction away from the last trade price is rejected (e.g. 0.10 = ±10%).
	// Zero disables the band. It has no effect until the first trade sets a
	// reference price. It is a decimal fraction applied only in the cold reject
	// path, so the integer hot path is untouched.
	PriceBand decimal.Decimal
	// ProRata selects size-proportional allocation at each price level instead
	// of the default price–time (FIFO) priority. In this mode, self orders are
	// skipped rather than STP-cancelled.
	ProRata bool
	// Clock supplies every timestamp the engine stamps (order Created/Updated,
	// trade Created, book snapshots). nil => time.Now. Injecting a deterministic
	// clock makes replay byte-identical down to the timestamps — the single-writer
	// state transition never reads the wall clock on its own.
	Clock func() time.Time
	// DisabledClasses lists advanced order families to reject (see OrderClass).
	// Use it to feature-flag off a risky/buggy exotic type in production without a
	// redeploy of the whole engine.
	DisabledClasses []OrderClass
	// Guardrail is an optional self-output tripwire (see Guardrail). Zero value
	// disables it.
	Guardrail Guardrail
	// EventSink, if set, receives the engine's ordered event stream (see
	// EventSink). nil => no events, zero hot-path overhead.
	EventSink EventSink

	// --- Pre-trade risk & anti-manipulation admission controls ---
	// These gate the live ingress path only (they are bypassed on deterministic
	// replay, which trusts the already-accepted command log). All default to
	// zero = disabled, and Privileged (liquidation/ADL) orders are exempt from
	// the size caps and the minimum resting time. See docs/THREAT-MODEL.md.

	// MaxOrderQty rejects any single order larger than this many lots — a
	// fat-finger / fat-order guard that complements the aggregate Guardrail.
	// Zero disables it.
	MaxOrderQty int64
	// MaxOrderNotional rejects any single limit order whose price × quantity (in
	// tick·lot units) exceeds this. Market orders carry no ex-ante price and are
	// bounded by MaxOrderQty only. Zero disables it. Regardless of this cap, an
	// order whose notional overflows int64 is always rejected.
	MaxOrderNotional int64
	// MinRestingTime is an anti-spoofing / anti-flicker control: a resting book
	// order cannot be cancelled until it has rested at least this long (measured
	// by the engine clock from placement). A cancel arriving sooner is rejected
	// with ErrCancelTooSoon; the order stays live and is cancellable once the
	// minimum elapses. Zero disables it. This targets the JPMorgan/Coscia
	// spoofing pattern (post size, pull it before it can fill).
	MinRestingTime time.Duration
	// SessionClose reports when the current trading session ends, which is what a
	// TIFDay order expires at. Nil means the venue has not declared a session, and a
	// DAY order is then refused with ErrNoSessionClose rather than treated as GTC —
	// silently making an order immortal is the opposite of what the client asked for.
	//
	// It is a function rather than a fixed time so a venue can roll it daily without
	// rebuilding the engine.
	SessionClose func() time.Time
	// MaxMarkStep bounds how far a single SetMarkPrice update may move the mark
	// from its current value, as a fraction (e.g. 0.20 = ±20%). A larger jump is
	// rejected with ErrMarkStepTooLarge, so a thin-book oracle pump cannot drag
	// the price band with it (the Mango / Hyperliquid-JELLY lesson). Zero
	// disables the guard. The first mark (from 0) is always accepted.
	MaxMarkStep decimal.Decimal
	// MinMarkDepth, when > 0, requires the resting book to hold at least this many
	// lots within MarkDepthBand of a proposed mark before SetMarkPrice will move to
	// it — the depth-backed complement to MaxMarkStep. Where MaxMarkStep bounds one
	// jump, this bounds a *patient* drag: a mark can only track prices real
	// liquidity supports, so a thin-book pump (even a slow, in-step one) cannot lead
	// the mark. A move to an under-supported price is rejected with
	// ErrMarkDepthTooThin. The first mark and clearing to 0 are always accepted.
	MinMarkDepth int64
	// MarkDepthBand is the fraction around a proposed mark within which MinMarkDepth
	// resting depth is counted (e.g. 0.02 = ±2%). Zero falls back to MaxMarkStep,
	// then PriceBand; if none is set the whole book counts.
	MarkDepthBand decimal.Decimal
	// MinOrderQty / MinOrderNotional reject dust: an order below the size or (for
	// limits) notional floor is rejected with ErrOrderBelowMinQty /
	// ErrOrderBelowMinNotional. Dust floods bloat the book and degrade latency
	// (the resource-exhaustion vector). Zero disables each.
	MinOrderQty      int64
	MinOrderNotional int64
	// MaxOrdersPerAccount caps how many resting orders one UserID may hold at
	// once; a submit that would exceed it is rejected with ErrTooManyOrders. This
	// bounds per-account book footprint against a dust/quote-stuffing flood. Zero
	// disables it. Privileged orders are exempt.
	MaxOrdersPerAccount int
	// DedupClientOrderIDs, when > 0, rejects a submit whose (UserID, ClientOrderID)
	// was seen among the last DedupClientOrderIDs distinct such keys, with
	// ErrDuplicateClientOrderID — near-term protection against a replayed or
	// duplicated NewOrder double-booking (the FIX PossDup class). Orders with an
	// empty ClientOrderID are never deduped. Bounded memory (a ring of the most
	// recent keys); full session idempotency is a gateway concern. Bypassed on
	// replay.
	DedupClientOrderIDs int
	// MaxForceTradeQty caps the quantity of a single ForceTrade call (liquidation
	// / ADL print); a larger forced trade is rejected with ErrForceTradeTooLarge,
	// so the risk layer must liquidate in chunks rather than sweep the book in one
	// print (the incremental-liquidation lesson). Zero disables the cap.
	MaxForceTradeQty int64
	// BandBreachPause, when > 0, turns a price-band breach into a timed trading
	// pause: the breaching order is still rejected, and the engine moves to Halted
	// until the injected clock advances BandBreachPause, then auto-resumes to Open
	// on the next order (LULD-style pause-and-reopen instead of a bare reject).
	// Zero keeps the plain reject behaviour.
	BandBreachPause time.Duration
	// IcebergPeakJitter, when > 0, varies each refilled iceberg slice by up to this
	// fraction around its display size (e.g. 0.3 = ±30%), so a watcher cannot
	// fingerprint a hidden reserve by its fixed reload size (iceberg detection /
	// pinging). The variation is deterministic in the order id and refill count, so
	// replay stays exact. Zero shows a fixed display size.
	IcebergPeakJitter decimal.Decimal
}

// DefaultConfig returns a sensible configuration for a symbol.
func DefaultConfig(symbol string) Config {
	return Config{
		Symbol:              symbol,
		SelfTradePrevention: STPCancelNewest,
		MaxOrders:           100_000,
	}
}

// MatchResult is the outcome of processing one order.
type MatchResult struct {
	Order           *types.Order
	Trades          []*types.Trade
	Status          types.OrderStatus
	RejectionReason error
}

// Engine matches orders for a single symbol. It is a single writer: its mutating
// methods (Match, Process, Cancel, Process*) own the book with no internal lock,
// so they must not be called concurrently — drive the engine from one goroutine,
// or wrap it in a Runner (engine_loop.go) that serialises concurrent producers
// through a command queue. Read accessors that delegate to the order book or stop
// book (BestBid, Snapshot, LastTradePrice, PendingStopCount, ...) are guarded by
// those structures' own locks and are safe to call concurrently.
type Engine struct {
	config        Config
	book          *orderbook.OrderBook
	stopBook      *orderbook.StopBook
	icebergOrders map[int64]*types.IcebergOrder
	ocoByOrderID  map[int64]*types.OCOOrder // both legs' ids map to the pair
	trailingStops map[int64]*types.TrailingStop
	// expiries schedules TIFDay and TIFGoodTillDate deadlines. See expiry.go.
	expiries expiryQueue
	// now is the instant of the command being applied, stamped once by nextID and
	// reused by every fill and event it produces. Single-writer, so a field is
	// safe; see commandNow.
	now           time.Time
	state         EngineState
	bandEnabled   bool  // config.PriceBand > 0, precomputed to keep decimal off the hot path
	markStepEnab  bool  // config.MaxMarkStep > 0, precomputed
	icebergJitBps int64 // config.IcebergPeakJitter in basis points, precomputed
	replaying     bool  // replay/bootstrap mode: bypass live-ingress admission controls
	orderSeq      int64
	tradeSeq      int64
	clock         func() time.Time
	disabled      map[OrderClass]bool
	markPrice     int64 // external mark/index reference for the band (0 => use last trade)

	// self-output guardrail window accounting
	guard          Guardrail
	windowStart    time.Time
	windowTrades   int
	windowNotional int64

	// busted holds the annulled prints, by trade id. Nil until the first bust,
	// because the overwhelmingly common case is a venue that never busts anything
	// and should not carry a map for it.
	busted map[int64]BustRecord

	// ordered event stream
	sink     EventSink
	eventSeq int64
	eventBuf []Event
	// pending holds the events produced *during* a command — trades as they
	// execute, and iceberg slices as they reload — in the order they actually
	// happened. Emission used to collect trades and publish them after the fact,
	// which put a refilled slice's announcement before the trades that consumed
	// the previous slice, so a consumer saw executions against an order it had
	// been told was finished.
	pending []Event

	// client-order-id dedup (near-term replay guard): a ring of the most recent
	// (user,clientID) keys plus a set for O(1) lookup.
	dedupSeen map[string]struct{}
	dedupRing []string
	dedupPos  int

	// timed band-breach pause: the engine auto-resumes to Open once the clock
	// reaches pausedUntil (zero => no pending auto-resume).
	pausedUntil time.Time
}

// maxStopCascade bounds how many rounds of stop triggering a single order may
// set off, a safety net against a pathological trigger loop.
const maxStopCascade = 1000

// NewEngine constructs an engine and its underlying book.
func NewEngine(config Config) *Engine {
	if config.SelfTradePrevention == "" {
		config.SelfTradePrevention = STPCancelNewest
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	var disabled map[OrderClass]bool
	if len(config.DisabledClasses) > 0 {
		disabled = make(map[OrderClass]bool, len(config.DisabledClasses))
		for _, c := range config.DisabledClasses {
			disabled[c] = true
		}
	}
	return &Engine{
		config: config,
		book: orderbook.New(orderbook.Config{
			Symbol:    config.Symbol,
			MaxOrders: config.MaxOrders,
			Clock:     config.Clock,
		}),
		stopBook:      orderbook.NewStopBook(config.Symbol),
		icebergOrders: make(map[int64]*types.IcebergOrder),
		ocoByOrderID:  make(map[int64]*types.OCOOrder),
		trailingStops: make(map[int64]*types.TrailingStop),
		// Resolve the band-enabled flag once (a decimal compare) so the per-order
		// hot path never touches decimal for the common band-disabled case.
		bandEnabled:   config.PriceBand.GreaterThan(decimal.Zero),
		markStepEnab:  config.MaxMarkStep.GreaterThan(decimal.Zero),
		icebergJitBps: config.IcebergPeakJitter.Mul(decimal.NewFromInt(10000)).IntPart(),
		clock:         config.Clock,
		disabled:      disabled,
		guard:         config.Guardrail,
		sink:          config.EventSink,
	}
}

// SetEventSink attaches (or clears) the event sink after construction. Recovery
// needs it: an engine must replay its log with no sink, so a lifetime of
// historical events is not republished to live consumers, and only then start
// publishing.
//
// Call it before the engine is handed to a Runner; it is not safe to change the
// sink while the matching goroutine is running.
func (e *Engine) SetEventSink(s EventSink) { e.sink = s }

// SetReplaying toggles replay/bootstrap mode. It suppresses exactly the controls
// that depend on wall-clock time — the minimum resting time and the band-breach
// pause — because those would be re-evaluated against replay-time timestamps and
// would wrongly reject commands the live engine accepted.
//
// It does NOT suppress the deterministic admission checks. The command log is
// written write-ahead and so records commands as submitted rather than as
// accepted; the per-order caps, the notional overflow guard and the duplicate
// client-order-id check are pure functions of the order, the configuration and
// the replayed book, so re-running them reproduces the live outcome. Skipping
// them let an order the live engine rejected rest on the recovered book.
//
// The corollary is that configuration must not change across a recovery. Tighten
// a cap between the crash and the restart and replay will legitimately reject
// commands that were accepted before it, producing a book that differs from the
// one that crashed — correctly, but not identically.
//
// Recovery paths (see pkg/wal Restore) wrap replay in SetReplaying(true) /
// SetReplaying(false). The deterministic matching itself is unaffected.
func (e *Engine) SetReplaying(v bool) { e.replaying = v }

// checkedMul multiplies two non-negative int64 values, reporting ok=false on
// overflow. Used to bound order notional before it can wrap.
func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if p/b != a || p < 0 {
		return 0, false
	}
	return p, true
}

// saturatingAdd adds two non-negative int64 values, clamping to MaxInt64 on
// overflow instead of wrapping — so the guardrail's windowed notional can only
// ever over-count (trip sooner), never wrap to a small value and miss a trip.
func saturatingAdd(a, b int64) int64 {
	s := a + b
	if s < a { // overflow of two non-negatives
		return math.MaxInt64
	}
	return s
}

// checkOrderCaps enforces the optional per-order size/notional limits and always
// rejects an order whose notional overflows int64. Privileged (liquidation/ADL)
// orders bypass the configured caps but are still overflow-checked.
//
// These run during replay too. The command log is written write-ahead, so it
// records commands as SUBMITTED, not as accepted — an order the live engine
// rejected is in the log like any other. Every check here is a deterministic
// function of the submitted order, the configuration and the replayed book, so
// re-running them reproduces the live decision exactly; skipping them instead
// rested live-rejected orders on the recovered book and silently diverged it from
// the engine that crashed.
//
// The overflow guard in particular is an arithmetic invariant rather than ingress
// policy: a corrupt or hand-edited log entry must not be able to replay a
// notional that wraps int64 into the book.
//
// The genuinely time-dependent controls are not here — minimum resting time and
// the band-breach pause gate on e.replaying at their own call sites, because
// re-evaluating those against replay-time timestamps would wrongly reject
// commands the live engine accepted.
func (e *Engine) checkOrderCaps(order *types.Order) error {
	priv := order.Privileged
	if !priv && e.config.MinOrderQty > 0 && order.Quantity < e.config.MinOrderQty {
		return types.ErrOrderBelowMinQty
	}
	if !priv && e.config.MaxOrderQty > 0 && order.Quantity > e.config.MaxOrderQty {
		return types.ErrOrderExceedsMaxQty
	}
	if order.Type == types.OrderTypeLimit {
		notional, ok := checkedMul(order.Price, order.Quantity)
		if !ok {
			return types.ErrNotionalOverflow
		}
		if !priv && e.config.MinOrderNotional > 0 && notional < e.config.MinOrderNotional {
			return types.ErrOrderBelowMinNotional
		}
		if !priv && e.config.MaxOrderNotional > 0 && notional > e.config.MaxOrderNotional {
			return types.ErrOrderExceedsMaxNotional
		}
	}
	if !priv && e.config.MaxOrdersPerAccount > 0 &&
		e.book.OrdersByUser(order.UserID) >= e.config.MaxOrdersPerAccount {
		return types.ErrTooManyOrders
	}
	if e.isDuplicate(order) {
		return types.ErrDuplicateClientOrderID
	}
	return nil
}

// dedupKey builds the dedup key for an order, or "" if it has no client id.
func dedupKey(order *types.Order) string {
	if order.ClientOrderID == "" {
		return ""
	}
	return order.UserID + "\x00" + order.ClientOrderID
}

// isDuplicate reports whether the order's (user, client-order-id) is among the
// most recent DedupClientOrderIDs accepted keys. Check-only: the key is not
// recorded until the order is actually accepted (see recordClientOrderID), so a
// rejected order stays resubmittable under the same client id.
func (e *Engine) isDuplicate(order *types.Order) bool {
	if e.config.DedupClientOrderIDs <= 0 {
		return false
	}
	key := dedupKey(order)
	if key == "" {
		return false
	}
	_, ok := e.dedupSeen[key]
	return ok
}

// recordClientOrderID remembers an accepted order's client-order-id key, evicting
// the oldest when the bounded ring is full.
//
// This runs during replay as well as live. The guard is recovered state, not a
// live-only convenience: an order accepted before the crash must still be a
// duplicate after it, or a client resending across a venue restart — exactly when
// resends are most likely — double-books.
func (e *Engine) recordClientOrderID(order *types.Order) {
	if e.config.DedupClientOrderIDs <= 0 {
		return
	}
	if key := dedupKey(order); key != "" {
		e.recordDedupKey(key)
	}
}

// recordDedupKey inserts a already-built key into the bounded ring, evicting the
// oldest when full. Split out from recordClientOrderID so snapshot restore can
// re-seed the guard from EngineSnapshot.DedupKeys without synthesising orders.
func (e *Engine) recordDedupKey(key string) {
	if e.config.DedupClientOrderIDs <= 0 || key == "" {
		return
	}
	if e.dedupRing == nil {
		e.dedupRing = make([]string, e.config.DedupClientOrderIDs)
		e.dedupSeen = make(map[string]struct{}, e.config.DedupClientOrderIDs)
	}
	if _, dup := e.dedupSeen[key]; dup {
		return // already tracked; re-seeding must not consume two ring slots
	}
	if old := e.dedupRing[e.dedupPos]; old != "" {
		delete(e.dedupSeen, old)
	}
	e.dedupRing[e.dedupPos] = key
	e.dedupSeen[key] = struct{}{}
	e.dedupPos = (e.dedupPos + 1) % len(e.dedupRing)
}

// dedupKeysChronological returns the tracked keys oldest-first. dedupPos is the
// next write slot, so it is also the oldest entry; walk forward and wrap.
func (e *Engine) dedupKeysChronological() []string {
	if len(e.dedupRing) == 0 {
		return nil
	}
	keys := make([]string, 0, len(e.dedupSeen))
	for i := 0; i < len(e.dedupRing); i++ {
		if k := e.dedupRing[(e.dedupPos+i)%len(e.dedupRing)]; k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// nextID assigns the order a monotonic engine id if it does not already carry
// one, and returns it. Orders enter with ID==0 (see types.NewOrder); replayed
// orders reset to 0 via Fresh, so ids are reproducible in submission order.
func (e *Engine) nextID(order *types.Order) int64 {
	if order.ID == 0 {
		e.orderSeq++
		order.ID = ComposeID(e.config.ShardIndex, e.orderSeq)
	}
	// The engine is the single writer that owns time: it stamps the authoritative
	// timestamps on intake from its injected clock, so replay is reproducible.
	//
	// The clock is read ONCE here and cached for the rest of the command, which is
	// both faster and more deterministic. time.Now is ~27ns on darwin/arm64, and
	// reading it again per fill made wall-clock reads 46% of the match path in a
	// CPU profile. It also meant two events from the same command could carry
	// different instants, which is a worse story for replay than one instant per
	// command.
	e.now = e.clock().UTC()
	if order.CreatedAt.IsZero() {
		order.CreatedAt = e.now
	}
	order.UpdatedAt = e.now
	return order.ID
}

// commandNow returns the timestamp for the command in progress. nextID stamps it
// on intake; this is the safety net for any path that reaches a fill without one,
// so a missing stamp is a clock read rather than a zero time.
func (e *Engine) commandNow() time.Time {
	if e.now.IsZero() {
		e.now = e.clock().UTC()
	}
	return e.now
}

// Match is the zero-allocation entry point: it settles order against the book
// and appends the resulting trades — as values — to dst, returning the extended
// slice, the order's final status, and any rejection reason. Pass a reusable
// slice (e.g. buf[:0]) and no heap allocation occurs on the hot path: book nodes
// and levels are pooled and trades land in the caller's buffer. Trades from any
// stop orders the fill triggers are appended too. This is the low-latency path;
// Process wraps it for callers that prefer a *MatchResult.
func (e *Engine) Match(order *types.Order, dst []types.Trade) ([]types.Trade, types.OrderStatus, error) {
	start := len(dst)
	e.nextID(order)
	// Anything whose deadline has passed leaves before this order is matched, so it
	// cannot trade against liquidity that should no longer exist. Guarded at the call
	// site rather than inside: a venue using neither DAY nor GTD should pay a length
	// check, not a function call, on its hottest path.
	if len(e.expiries) > 0 {
		e.expireDue()
	}
	// Resolve the deadline before matching, so a DAY order with no session or a GTD
	// order already in the past is refused rather than discovered later when it fails
	// to expire. Guarded for the same reason: most orders are not dated.
	if order.TimeInForce.Expiring() {
		if reason := e.resolveExpiry(order); reason != nil {
			order.Status = types.OrderStatusRejected
			e.emitResult(order, nil, types.OrderStatusRejected, reason)
			return dst, types.OrderStatusRejected, reason
		}
	}
	dst, status, reason := e.settleInto(order, dst)
	if reason == nil {
		e.recordClientOrderID(order) // remember only accepted orders
		if order.TimeInForce.Expiring() {
			e.trackExpiry(order) // only what actually rested gets a deadline
		}
	}
	dst = e.cascadeStops(dst)
	e.emitResult(order, dst[start:], status, reason)
	return dst, status, reason
}

// ExpireDue removes every order whose time-in-force deadline has passed.
//
// The engine expires lazily: a deadline is checked whenever a command arrives, so in
// a quiet market an expired order stays in the book until something happens. That is
// invisible to anyone trading — the order is gone before it could match — but it is
// visible to a market-data subscriber, which would see depth that should have left.
// An embedder that cares drives this on a ticker; cmd/obgw does.
//
// Single-writer: call from the engine's goroutine, or use Runner.ExpireDue.
func (e *Engine) ExpireDue() {
	e.now = e.clock().UTC()
	e.expireDue()
}

// Process runs one order through the engine and returns a *MatchResult. It is the
// convenience wrapper over Match; latency-sensitive callers should use Match with
// a reused buffer to avoid the result allocation.
func (e *Engine) Process(order *types.Order) *MatchResult {
	trades, status, reason := e.Match(order, nil)
	return toMatchResult(order, trades, status, reason)
}

// toMatchResult wraps a value-trade slice as a *MatchResult, pointing the result
// slice into the (call-owned, stable) trade buffer — one slice allocation, not
// one per trade.
func toMatchResult(order *types.Order, dst []types.Trade, status types.OrderStatus, reason error) *MatchResult {
	res := &MatchResult{Order: order, Status: status, RejectionReason: reason}
	if len(dst) > 0 {
		res.Trades = make([]*types.Trade, len(dst))
		for i := range dst {
			res.Trades[i] = &dst[i]
		}
	}
	return res
}

// emitResult publishes the ordered events for one processed order — an
// Accepted (or Rejected) for the order itself, then a Trade per fill it produced
// (including fills from any stops it triggered). No-op without a sink, so the hot
// path stays zero-overhead. The event batch reuses an engine-owned buffer.
func (e *Engine) emitResult(order *types.Order, trades []types.Trade, status types.OrderStatus, reason error) {
	if e.sink == nil {
		return
	}
	// A rejection means nothing happened: an FOK that could not fill has already
	// had its trades reversed, so publishing them would announce executions that
	// were undone.
	if status == types.OrderStatusRejected {
		e.pending = e.pending[:0]
	}
	buf := e.eventBuf[:0]
	if status == types.OrderStatusRejected {
		buf = append(buf, Event{Kind: EventRejected, OrderID: order.ID, UserID: order.UserID, Order: order, Reason: reason})
	} else {
		buf = append(buf, Event{Kind: EventAccepted, OrderID: order.ID, UserID: order.UserID, Order: order})
	}
	buf = append(buf, e.pending...)
	e.pending = e.pending[:0]
	// An order that ends Cancelled was announced as ACCEPTED above but never
	// rested: an IOC or market remainder, or a taker cancelled by self-trade
	// prevention. Without a matching delete a consumer reconstructs it as live
	// forever, so close it out here.
	if status == types.OrderStatusCancelled {
		buf = append(buf, Event{Kind: EventCanceled, OrderID: order.ID, UserID: order.UserID, Order: order})
	}
	e.eventBuf = e.stampAndPublish(buf)
}

// stampAndPublish numbers a composed batch and hands it to the sink. Sequence
// numbers are assigned here, not when each event is recorded: a trade is recorded
// mid-match while the submitted order's own event is only composed afterwards, so
// numbering at record time produced a stream whose Seq ran backwards.
func (e *Engine) stampAndPublish(buf []Event) []Event {
	for i := range buf {
		e.eventSeq++
		buf[i].Seq = e.eventSeq
	}
	e.sink.OnEvents(buf)
	return buf
}

// emitAdd publishes a single Accepted for an order (re-)entering the book outside
// the normal submit path — today, an iceberg slice refilled from its reserve.
// Without it the reserve reloads invisibly: consumers saw the slice fill to zero,
// concluded the order was done, and then had to discard every later fill of a
// still-live order as referencing something unknown.
func (e *Engine) emitAdd(order *types.Order) {
	if e.sink == nil {
		return
	}
	e.pending = append(e.pending, Event{Kind: EventAccepted, OrderID: order.ID, UserID: order.UserID, Order: order})
}

// emitCancel publishes a single Canceled event for a removed resting order.
func (e *Engine) emitCancel(order *types.Order) {
	if e.sink == nil {
		return
	}
	e.pending = append(e.pending, Event{Kind: EventCanceled, OrderID: order.ID, UserID: order.UserID, Order: order})
}

// emitCancelReason publishes a Canceled carrying why, so a consumer can tell an
// expiry from a cancel the client issued.
func (e *Engine) emitCancelReason(order *types.Order, reason error) {
	if e.sink == nil {
		return
	}
	e.pending = append(e.pending, Event{
		Kind: EventCanceled, OrderID: order.ID, UserID: order.UserID, Order: order, Reason: reason,
	})
}

// emitTriggered announces that a conditional order's trigger has been reached and it
// is about to enter the book as a live order.
//
// It carries the order so a consumer can see which one fired without keeping its own
// map of pending stops.
func (e *Engine) emitTriggered(order *types.Order) {
	if e.sink == nil {
		return
	}
	e.pending = append(e.pending, Event{Kind: EventTriggered, OrderID: order.ID, UserID: order.UserID, Order: order})
}

// emitReplaced announces an in-place size change that no trade explains — today,
// a maker shrunk by self-trade-prevention DECREMENT.
func (e *Engine) emitReplaced(order *types.Order) {
	if e.sink == nil {
		return
	}
	e.pending = append(e.pending, Event{Kind: EventReplaced, OrderID: order.ID, UserID: order.UserID, Order: order})
}

// emitTerminalIfDone closes out an order that settled without resting — a stop
// fired by a cascade whose remainder was cancelled rather than booked.
func (e *Engine) emitTerminalIfDone(order *types.Order) {
	if e.sink == nil || order.Status != types.OrderStatusCancelled {
		return
	}
	e.pending = append(e.pending, Event{Kind: EventCanceled, OrderID: order.ID, UserID: order.UserID, Order: order})
}

// flushPending publishes any batch built outside a submit path — the standalone
// Cancel, where there is no emitResult to compose the events into.
func (e *Engine) flushPending() {
	if e.sink == nil || len(e.pending) == 0 {
		return
	}
	buf := append(e.eventBuf[:0], e.pending...)
	e.pending = e.pending[:0]
	e.eventBuf = e.stampAndPublish(buf)
}

// emitStateChange publishes a Halted/Resumed event (guardrail trip, band-breach
// pause, or auto-resume) so operators can page on it. UserID, when set, is the
// order whose processing triggered the change.
func (e *Engine) emitStateChange(kind EventKind, userID string) {
	if e.sink == nil {
		return
	}
	e.eventSeq++
	e.eventBuf = append(e.eventBuf[:0], Event{Seq: e.eventSeq, Kind: kind, UserID: userID})
	e.sink.OnEvents(e.eventBuf)
}

// maybeAutoResume lifts a timed band-breach pause once the clock reaches
// pausedUntil, returning the engine to Open and emitting a Resumed event. Manual
// halts (pausedUntil zero) are never auto-resumed.
func (e *Engine) maybeAutoResume() {
	if e.pausedUntil.IsZero() || e.state != StateHalted {
		return
	}
	if !e.clock().Before(e.pausedUntil) {
		e.state = StateOpen
		e.pausedUntil = time.Time{}
		e.emitStateChange(EventResumed, "")
	}
}

// rejectDisabled builds a rejection for an order whose class is feature-flagged
// off (Config.DisabledClasses), without touching the book, and emits it.
func (e *Engine) rejectDisabled(order *types.Order) *MatchResult {
	order.Status = types.OrderStatusRejected
	e.emitResult(order, nil, types.OrderStatusRejected, types.ErrOrderTypeDisabled)
	return &MatchResult{Order: order, Status: types.OrderStatusRejected, RejectionReason: types.ErrOrderTypeDisabled}
}

// settleInto matches order and applies market/TIF resting rules, appending trades
// to dst. It assumes the engine lock is held and the order's id is assigned, and
// returns the extended buffer, the order's final status, and any rejection reason.
func (e *Engine) settleInto(order *types.Order, dst []types.Trade) ([]types.Trade, types.OrderStatus, error) {
	// Lift a timed band-breach pause whose clock has elapsed, then apply circuit
	// breakers: engine state, then a limit price outside the collar.
	e.maybeAutoResume()
	switch e.state {
	case StateHalted:
		order.Status = types.OrderStatusRejected
		return dst, types.OrderStatusRejected, types.ErrTradingHalted
	case StateCancelOnly, StateClosed:
		order.Status = types.OrderStatusRejected
		return dst, types.OrderStatusRejected, types.ErrNewOrdersHalted
	case StatePreOpen, StateClosingAuction:
		// An accumulating phase takes orders and does not match them. A market order has no price
		// to rest at and no price to trade at until the uncross decides one, so it is
		// refused rather than held and executed at whatever the auction produces —
		// which is not what an unpriced order asked for.
		if order.Type == types.OrderTypeMarket {
			order.Status = types.OrderStatusRejected
			return dst, types.OrderStatusRejected, types.ErrNewOrdersHalted
		}
		if err := e.checkOrderCaps(order); err != nil {
			order.Status = types.OrderStatusRejected
			return dst, types.OrderStatusRejected, err
		}
		if err := e.restOrder(order); err != nil {
			order.Status = types.OrderStatusRejected
			return dst, types.OrderStatusRejected, err
		}
		return dst, order.Status, nil
	}
	// Pre-trade risk caps (fat-finger size/notional + int64 overflow guard).
	if err := e.checkOrderCaps(order); err != nil {
		order.Status = types.OrderStatusRejected
		return dst, types.OrderStatusRejected, err
	}
	if order.Type == types.OrderTypeLimit && !order.Privileged && e.outsideBand(order.Price) {
		order.Status = types.OrderStatusRejected
		// Optionally escalate a breach into a timed trading pause (LULD-style):
		// the breaching order is still rejected, and trading halts until the clock
		// advances BandBreachPause, then auto-resumes.
		if e.config.BandBreachPause > 0 && !e.replaying {
			e.state = StateHalted
			e.pausedUntil = e.clock().Add(e.config.BandBreachPause)
			e.emitStateChange(EventHalted, order.UserID)
		}
		return dst, types.OrderStatusRejected, types.ErrPriceOutsideBand
	}
	// Post-only orders must rest as makers; reject if they would take.
	if order.PostOnly && e.wouldCross(order) {
		order.Status = types.OrderStatusRejected
		return dst, types.OrderStatusRejected, types.ErrPostOnlyWouldCross
	}

	start := len(dst)
	dst, makerOrders := e.match(order, dst)

	// Market orders never rest.
	if order.Type == types.OrderTypeMarket {
		if order.RemainingQty != 0 {
			order.Status = types.OrderStatusCancelled
			var reason error
			if len(dst) == start { // this order printed nothing
				reason = types.ErrMarketOrderNoLiquidity
			}
			return dst, types.OrderStatusCancelled, reason
		}
		return dst, types.OrderStatusFilled, nil
	}

	// Limit orders by time-in-force.
	switch order.TimeInForce {
	case types.TIFImmediateOrCancel:
		// Whatever couldn't fill immediately is cancelled (never rests).
		if order.RemainingQty != 0 && order.Status != types.OrderStatusCancelled {
			order.Status = types.OrderStatusCancelled
		}
		return dst, order.Status, nil

	case types.TIFFillOrKill:
		// All-or-nothing: if it didn't fully fill, unwind every trade and reject.
		if !order.IsFilled() {
			for i := start; i < len(dst); i++ {
				e.reverseTrade(dst[i], makerOrders)
			}
			order.Status = types.OrderStatusRejected
			return dst[:start], types.OrderStatusRejected, types.ErrFOKCannotFill
		}
		return dst, types.OrderStatusFilled, nil

	default: // GTC
		// Rest any active remainder on the book.
		if order.IsActive() && !order.IsFilled() {
			if err := e.book.Add(order); err != nil {
				order.Status = types.OrderStatusRejected
				return dst, types.OrderStatusRejected, err
			}
		}
		return dst, order.Status, nil
	}
}

// cascadeStops fires any stop orders whose trigger price the latest trade
// reached, settling each and appending its trades to dst. It repeats until no
// new stops fire — a triggered stop's own trades may trigger further stops —
// bounded by maxStopCascade.
func (e *Engine) cascadeStops(dst []types.Trade) []types.Trade {
	// Nothing conditional is resting, so there is nothing to cascade. Without this
	// the cascade ran after every match on every venue, taking two locks, walking
	// the stop map and calling a reflection-based sort over an empty slice — 23% of
	// the match path in a CPU profile, for venues that use no stop orders at all.
	if e.stopBook.Count() == 0 && len(e.trailingStops) == 0 {
		return dst
	}
	for range maxStopCascade {
		mp := e.book.LastTradePrice()
		if mp <= 0 {
			return dst
		}
		fired := e.stopBook.CheckTriggers(mp)
		trailing := e.checkTrailingStops(mp)
		if len(fired) == 0 && len(trailing) == 0 {
			return dst
		}
		for _, s := range fired {
			// If this stop is an OCO leg, cancel its primary before it executes.
			e.cancelOCOCounterpart(s.Order.ID)
			// Triggered before Accepted: a consumer that saw only the Accepted would
			// have no way to tell this order from one a client had just submitted, and
			// "a stop fired" is the event a risk system actually wants to see.
			e.emitTriggered(s.Order)
			e.emitAdd(s.Order) // the stop is now a live order; announce it before it trades
			dst, _, _ = e.settleInto(s.Order, dst)
			e.emitTerminalIfDone(s.Order)
		}
		for _, ts := range trailing {
			e.emitTriggered(ts.Order)
			e.emitAdd(ts.Order)
			dst, _, _ = e.settleInto(ts.Order, dst)
			e.emitTerminalIfDone(ts.Order)
		}
	}
	return dst
}

// ProcessStop submits a stop (or stop-limit) order. If the market has already
// reached the stop it fires immediately; otherwise it rests off-book until a
// trade reaches the trigger price.
func (e *Engine) ProcessStop(stop *types.StopOrder) *MatchResult {
	if e.disabled[ClassStop] {
		return e.rejectDisabled(stop.Order)
	}
	dst, status, reason := e.submitStopInto(stop, nil)
	e.emitResult(stop.Order, dst, status, reason)
	return toMatchResult(stop.Order, dst, status, reason)
}

// submitStopInto is the body of ProcessStop; it assumes the engine lock is held
// so it can be reused by ProcessOCO. Trades are appended to dst.
func (e *Engine) submitStopInto(stop *types.StopOrder, dst []types.Trade) ([]types.Trade, types.OrderStatus, error) {
	e.nextID(stop.Order)

	if mp := e.book.LastTradePrice(); mp > 0 && stop.ShouldTrigger(mp) {
		stop.Trigger()
		// A stop that fires on arrival is still a stop that fired, and a consumer
		// should not have to infer that from the absence of a PendingTrigger.
		e.emitTriggered(stop.Order)
		var status types.OrderStatus
		var reason error
		dst, status, reason = e.settleInto(stop.Order, dst)
		dst = e.cascadeStops(dst)
		return dst, status, reason
	}

	stop.Order.Status = types.OrderStatusPendingTrigger
	e.stopBook.Add(stop)
	return dst, types.OrderStatusPendingTrigger, nil
}

// ProcessPegged resolves a pegged order's price from the current book reference
// (plus its offset) and submits it as a limit. It is rejected if the reference
// price is unavailable or the resolved price is non-positive.
func (e *Engine) ProcessPegged(p *types.PeggedOrder) *MatchResult {
	if e.disabled[ClassPegged] {
		return e.rejectDisabled(p.Order)
	}
	ref, ok := e.pegReference(p.Ref)
	if !ok {
		p.Order.Status = types.OrderStatusRejected
		e.emitResult(p.Order, nil, types.OrderStatusRejected, types.ErrPegReferenceUnavailable)
		return &MatchResult{Order: p.Order, Status: types.OrderStatusRejected, RejectionReason: types.ErrPegReferenceUnavailable}
	}
	price := ref + p.Offset
	if price <= 0 {
		p.Order.Status = types.OrderStatusRejected
		e.emitResult(p.Order, nil, types.OrderStatusRejected, types.ErrInvalidPrice)
		return &MatchResult{Order: p.Order, Status: types.OrderStatusRejected, RejectionReason: types.ErrInvalidPrice}
	}
	p.Order.Price = price
	p.Order.Type = types.OrderTypeLimit

	e.nextID(p.Order)
	dst, status, reason := e.settleInto(p.Order, nil)
	dst = e.cascadeStops(dst)
	e.emitResult(p.Order, dst, status, reason)
	return toMatchResult(p.Order, dst, status, reason)
}

func (e *Engine) pegReference(ref types.PegReference) (int64, bool) {
	switch ref {
	case types.PegToBid:
		if p, _, ok := e.book.BestBid(); ok {
			return p, true
		}
	case types.PegToAsk:
		if p, _, ok := e.book.BestAsk(); ok {
			return p, true
		}
	case types.PegToMid:
		if m, ok := e.book.MidPrice(); ok {
			return m, true
		}
	}
	return 0, false
}

// ProcessTrailingStop submits a trailing stop. It seeds its trail from the
// current market and either fires immediately or rests, ratcheting as trades
// move the market (handled in cascadeStops).
func (e *Engine) ProcessTrailingStop(ts *types.TrailingStop) *MatchResult {
	if e.disabled[ClassTrailing] {
		return e.rejectDisabled(ts.Order)
	}
	e.nextID(ts.Order)

	mp := e.book.LastTradePrice()
	if mp > 0 {
		ts.Observe(mp)
		if ts.ShouldTrigger(mp) {
			ts.Trigger()
			dst, status, reason := e.settleInto(ts.Order, nil)
			dst = e.cascadeStops(dst)
			e.emitResult(ts.Order, dst, status, reason)
			return toMatchResult(ts.Order, dst, status, reason)
		}
	}
	ts.Order.Status = types.OrderStatusPendingTrigger
	e.trailingStops[ts.Order.ID] = ts
	e.emitResult(ts.Order, nil, types.OrderStatusPendingTrigger, nil)
	return &MatchResult{Order: ts.Order, Status: types.OrderStatusPendingTrigger}
}

// checkTrailingStops ratchets every live trailing stop against marketPrice and
// returns (in deterministic id order) those that now fire, removing them.
func (e *Engine) checkTrailingStops(marketPrice int64) []*types.TrailingStop {
	var fired []*types.TrailingStop
	for _, ts := range e.trailingStops {
		ts.Observe(marketPrice)
		if ts.ShouldTrigger(marketPrice) {
			fired = append(fired, ts)
		}
	}
	sort.Slice(fired, func(i, j int) bool {
		return fired[i].Order.ID < fired[j].Order.ID
	})
	for _, ts := range fired {
		ts.Trigger()
		delete(e.trailingStops, ts.Order.ID)
	}
	return fired
}

// ProcessOCO submits a one-cancels-other pair: the primary limit is posted, and
// if it does not complete immediately the stop is posted too. Whichever leg
// completes first cancels the other (handled in match/cascadeStops via the OCO
// registry).
func (e *Engine) ProcessOCO(oco *types.OCOOrder) *MatchResult {
	if e.disabled[ClassOCO] {
		return e.rejectDisabled(oco.Primary)
	}
	// Ids must be assigned before registering the legs, since the registry is
	// keyed by id (orders no longer arrive with a pre-set id).
	e.nextID(oco.Primary)
	e.nextID(oco.Stop.Order)
	e.ocoByOrderID[oco.Primary.ID] = oco
	e.ocoByOrderID[oco.Stop.Order.ID] = oco

	dst, status, reason := e.settleInto(oco.Primary, nil)
	dst = e.cascadeStops(dst)

	// Primary already done ⇒ the stop is never posted.
	if oco.Primary.IsFilled() {
		e.dropOCO(oco)
		e.emitResult(oco.Primary, dst, status, reason)
		return toMatchResult(oco.Primary, dst, status, reason)
	}

	// Publish the primary before submitting the stop. If the stop fires on entry
	// it cancels the primary, and emitting afterwards would report an order that
	// is already dead as ACCEPTED.
	e.emitResult(oco.Primary, dst, status, reason)

	// Then post the stop. Its return values are not discardable: if it triggers on
	// entry it settles through the book, filling and removing real makers and
	// moving the last trade price. Dropping them lost those executions outright —
	// the counterparty's fill reached neither the event stream nor any result —
	// and left the stop leg with an engine id no consumer had ever been told
	// about, so its later fills referenced an unknown order. Reported separately
	// from the primary's result, exactly as ProcessStop reports a bare stop.
	stopTrades, stopStatus, stopReason := e.submitStopInto(oco.Stop, nil)
	if oco.Stop.IsTriggered() {
		e.cancelOCOCounterpart(oco.Stop.Order.ID)
	}
	e.emitResult(oco.Stop.Order, stopTrades, stopStatus, stopReason)

	return toMatchResult(oco.Primary, dst, status, reason)
}

// cancelOCOCounterpart cancels the other leg of the OCO that legID belongs to
// (removing it from the book or stop book) and drops the pairing. No-op if legID
// is not part of a live OCO.
func (e *Engine) cancelOCOCounterpart(legID int64) {
	oco, ok := e.ocoByOrderID[legID]
	if !ok {
		return
	}
	otherID := oco.Primary.ID
	if legID == oco.Primary.ID {
		otherID = oco.Stop.Order.ID
	}
	if o, exists := e.book.Get(otherID); exists {
		_ = o.Cancel()
		_, _ = e.book.Remove(otherID)
		e.emitCancel(o)
	} else if s, exists := e.stopBook.Get(otherID); exists {
		_ = s.Order.Cancel()
		e.stopBook.Remove(otherID)
		e.emitCancel(s.Order)
	}
	e.dropOCO(oco)
}

func (e *Engine) dropOCO(oco *types.OCOOrder) {
	delete(e.ocoByOrderID, oco.Primary.ID)
	delete(e.ocoByOrderID, oco.Stop.Order.ID)
}

// ProcessIceberg submits an iceberg order. Only its display slice is ever
// visible in the book; as slices are consumed they refill from the hidden
// reserve until the total is worked off.
func (e *Engine) ProcessIceberg(ib *types.IcebergOrder) *MatchResult {
	if e.disabled[ClassIceberg] {
		return e.rejectDisabled(ib.Order)
	}
	e.nextID(ib.Order)
	ib.JitterBps = e.icebergJitBps // deterministic reload-size jitter (anti-sniffing)
	e.icebergOrders[ib.Order.ID] = ib

	dst, status, reason := e.settleInto(ib.Order, nil)
	// If the slice fully crossed on entry, keep refilling and re-settling until
	// it rests or the total is exhausted.
	for ib.Order.IsFilled() && !ib.IsFullyFilled() {
		if !ib.Refill() {
			break
		}
		dst, _, _ = e.settleInto(ib.Order, dst)
	}
	if ib.IsFullyFilled() {
		delete(e.icebergOrders, ib.Order.ID)
		status = types.OrderStatusFilled
	}
	dst = e.cascadeStops(dst)
	e.emitResult(ib.Order, dst, status, reason)
	return toMatchResult(ib.Order, dst, status, reason)
}

// match crosses taker against the resting book by price–time priority, appending
// value trades to dst. It returns the extended buffer and, only for FOK takers,
// the maker orders touched (so a failed FOK can be reversed). Trades print at the
// maker's resting price.
func (e *Engine) match(taker *types.Order, dst []types.Trade) ([]types.Trade, map[int64]*types.Order) {
	if e.config.ProRata {
		return e.matchProRata(taker, dst)
	}
	var makerOrders map[int64]*types.Order
	trackMakers := taker.TimeInForce == types.TIFFillOrKill
	start := len(dst)

	for taker.RemainingQty != 0 {
		var maker *types.Order
		if taker.Side == types.SideBuy {
			maker = e.book.PeekBestAskOrder()
			if maker == nil {
				break
			}
			// A limit buy only crosses asks at or below its price.
			if taker.Type == types.OrderTypeLimit && taker.Price < maker.Price {
				break
			}
		} else {
			maker = e.book.PeekBestBidOrder()
			if maker == nil {
				break
			}
			// A limit sell only crosses bids at or above its price.
			if taker.Type == types.OrderTypeLimit && taker.Price > maker.Price {
				break
			}
		}

		// Self-trade prevention (the taker's mode decides).
		if e.isSelfMatch(taker, maker) {
			switch e.takerSTP(taker) {
			case STPCancelNewest:
				taker.Status = types.OrderStatusCancelled
				return e.recordLast(dst, start), makerOrders
			case STPCancelOldest:
				_ = maker.Cancel()
				_, _ = e.book.Remove(maker.ID)
				e.emitCancel(maker)
				continue
			case STPCancelBoth:
				taker.Status = types.OrderStatusCancelled
				_ = maker.Cancel()
				_, _ = e.book.Remove(maker.ID)
				e.emitCancel(maker)
				return e.recordLast(dst, start), makerOrders
			case STPDecrement:
				// Reduce both by the overlap with no trade; the smaller side fully
				// cancels, the larger shrinks; then continue matching the taker.
				e.decrement(taker, maker)
				if maker.RemainingQty == 0 {
					_, _ = e.book.Remove(maker.ID)
					e.emitCancel(maker)
				} else {
					// Shrunk in place, keeping queue position: there is no trade to
					// infer the new size from, so without this a consumer's
					// remaining-quantity accounting stays permanently wrong.
					e.emitReplaced(maker)
				}
				if taker.RemainingQty == 0 {
					taker.Status = types.OrderStatusCancelled
					return e.recordLast(dst, start), makerOrders
				}
				continue
			case STPAllow:
				// fall through and trade
			}
		}

		qty := min(taker.RemainingQty, maker.RemainingQty)
		dst = e.executeTrade(taker, maker, maker.Price, qty, dst)
		if trackMakers {
			if makerOrders == nil {
				makerOrders = make(map[int64]*types.Order)
			}
			makerOrders[maker.ID] = maker
		}

		if maker.IsFilled() {
			_, _ = e.book.Remove(maker.ID)
			// If the consumed maker was an iceberg's visible slice, refill it
			// (the refilled slice re-enters at the back of its price level).
			if ib, ok := e.icebergOrders[maker.ID]; ok {
				if ib.Refill() {
					_ = e.book.Add(ib.Order)
					e.emitAdd(ib.Order)
				} else {
					delete(e.icebergOrders, maker.ID)
				}
			}
			// If the filled maker was an OCO primary, cancel its stop leg.
			e.cancelOCOCounterpart(maker.ID)
		} else {
			e.book.UpdateOrderQuantity(maker.ID, qty)
		}
	}

	return e.recordLast(dst, start), makerOrders
}

// isSelfMatch reports whether taker and maker should be self-trade-prevented: same
// user, or a shared non-zero trade group. Privileged (e.g. liquidation) takers are
// exempt so their own orders are never self-blocked.
func (e *Engine) isSelfMatch(taker, maker *types.Order) bool {
	if taker.Privileged {
		return false
	}
	if taker.UserID == maker.UserID {
		return true
	}
	return taker.TradeGroupID != 0 && taker.TradeGroupID == maker.TradeGroupID
}

// takerSTP is the self-trade-prevention mode that governs this match — the taker's
// per-order STPMode if set, else the engine default (the taker's mode decides).
func (e *Engine) takerSTP(taker *types.Order) SelfTradePrevention {
	if taker.STPMode != "" {
		return SelfTradePrevention(taker.STPMode)
	}
	return e.config.SelfTradePrevention
}

// decrement applies STPDecrement: reduce both orders by their overlap (quantity and
// remaining, keeping filled), printing no trade, and shrink the maker's resting
// level aggregate accordingly.
func (e *Engine) decrement(taker, maker *types.Order) {
	d := min(taker.RemainingQty, maker.RemainingQty)
	taker.Quantity -= d
	taker.RemainingQty -= d
	maker.Quantity -= d
	maker.RemainingQty -= d
	e.book.UpdateOrderQuantity(maker.ID, d)
}

// outsideBand reports whether price is beyond the circuit-breaker collar around
// the last trade price. Disabled when the band is zero or no trade has printed.
// The band is a decimal fraction; this runs only on limit-order entry (cold).
func (e *Engine) outsideBand(price int64) bool {
	if !e.bandEnabled {
		return false
	}
	// Prefer an externally-supplied, risk-clamped mark/index reference over the raw
	// last trade, so a thin-book wick can't move the collar (the Hyperliquid JELLY
	// lesson). Falls back to last trade when no mark is set.
	ref := e.markPrice
	if ref <= 0 {
		ref = e.book.LastTradePrice()
	}
	if ref <= 0 {
		return false
	}
	refDec := decimal.NewFromInt(ref)
	lo := refDec.Mul(decimal.NewFromInt(1).Sub(e.config.PriceBand))
	hi := refDec.Mul(decimal.NewFromInt(1).Add(e.config.PriceBand))
	pd := decimal.NewFromInt(price)
	return pd.LessThan(lo) || pd.GreaterThan(hi)
}

// Halt suspends trading; every subsequent order is rejected until Resume.
func (e *Engine) Halt() {
	// Announce the transition, not the call. An operator halting an already-halted
	// venue should not produce a second Halted, and a consumer counting them would
	// otherwise see an event that describes nothing.
	//
	// This used to emit nothing at all: only the AUTOMATIC transitions (a guardrail
	// trip, a band-breach pause) told anyone. So the one halt a venue most needs to
	// broadcast — the operator deliberately stopping trading — reached no consumer,
	// no market-data feed and no client, which is the opposite of the intended
	// priority.
	if e.state == StateHalted {
		return
	}
	e.state = StateHalted
	e.emitStateChange(EventHalted, "")
}

// SetCancelOnly puts the engine in cancel-only mode: cancels are accepted but new
// liquidity is rejected (ErrNewOrdersHalted). Used to wind a venue down under
// stress before a full halt or auction reopen.
func (e *Engine) SetCancelOnly() {
	if e.state == StateCancelOnly {
		return
	}
	e.state = StateCancelOnly
	// Its own kind rather than Halted: a subscriber told the venue is halted when it
	// is actually accepting cancels would draw the wrong conclusion about whether it
	// can still get out of a position.
	e.emitStateChange(EventCancelOnly, "")
}

// Resume returns the engine to normal (Open) trading.
func (e *Engine) Resume() {
	if e.state == StateOpen {
		return
	}
	e.state = StateOpen
	e.emitStateChange(EventResumed, "")
}

// State reports the current trading state.
func (e *Engine) State() EngineState { return e.state }

// Bust annuls a published trade. See bust.go.

// SetMarkPrice sets the external mark/index reference (in ticks) the price band is
// evaluated against. The risk layer computes it (e.g. index + clamped basis) and
// feeds it here; a value <= 0 clears it and the band falls back to the last trade
// price. Call from the single writer (or via Runner.SetMarkPrice).
//
// If Config.MaxMarkStep is set, an update that would move the mark more than that
// fraction away from its current value is rejected with ErrMarkStepTooLarge and
// the mark is left unchanged — a thin-book oracle pump then cannot drag the price
// band with it (the Mango / Hyperliquid-JELLY lesson). The first mark (from an
// unset 0) and clearing the mark (to 0) are always accepted. A negative price is
// rejected with ErrInvalidPrice.
func (e *Engine) SetMarkPrice(price int64) error {
	if price < 0 {
		return types.ErrInvalidPrice
	}
	if e.markStepEnab && e.markPrice > 0 && price > 0 {
		cur := decimal.NewFromInt(e.markPrice)
		if decimal.NewFromInt(price).Sub(cur).Abs().GreaterThan(cur.Mul(e.config.MaxMarkStep)) {
			return types.ErrMarkStepTooLarge
		}
	}
	// Depth-backed bound: a mark *move* must be supported by resting liquidity near
	// the new price, so a thin-book pump cannot lead the mark even in small steps.
	// Like the step guard, it applies only once a mark is established.
	if e.config.MinMarkDepth > 0 && price > 0 && e.markPrice > 0 && price != e.markPrice {
		band := e.markDepthBand()
		lo, hi := price-band, price+band
		if e.book.DepthWithin(lo, hi) < e.config.MinMarkDepth {
			return types.ErrMarkDepthTooThin
		}
	}
	e.markPrice = price
	return nil
}

// markDepthBand is the half-width (in ticks) of the window around a proposed mark
// within which resting depth is counted, from MarkDepthBand (else MaxMarkStep,
// else PriceBand). A zero fraction widens the window to the whole book.
func (e *Engine) markDepthBand() int64 {
	frac := e.config.MarkDepthBand
	if frac.IsZero() {
		frac = e.config.MaxMarkStep
	}
	if frac.IsZero() {
		frac = e.config.PriceBand
	}
	if frac.IsZero() {
		return math.MaxInt64 // no fraction configured: count the whole book
	}
	// ticks = mark * frac, evaluated off the current mark (or the new price if unset).
	ref := e.markPrice
	if ref <= 0 {
		ref = e.book.LastTradePrice()
	}
	if ref <= 0 {
		return math.MaxInt64
	}
	return decimal.NewFromInt(ref).Mul(frac).IntPart()
}

// MarkPrice returns the current mark reference (0 if unset).
func (e *Engine) MarkPrice() int64 { return e.markPrice }

// IsHalted reports whether trading is fully halted.
func (e *Engine) IsHalted() bool {
	return e.state == StateHalted
}

// wouldCross reports whether a limit order would immediately take liquidity
// (used to reject post-only orders). Non-limit orders always "cross".
func (e *Engine) wouldCross(order *types.Order) bool {
	if order.Type != types.OrderTypeLimit {
		return true
	}
	if order.Side == types.SideBuy {
		if ask, _, ok := e.book.BestAsk(); ok && order.Price >= ask {
			return true
		}
		return false
	}
	if bid, _, ok := e.book.BestBid(); ok && order.Price <= bid {
		return true
	}
	return false
}

// matchProRata crosses taker against the book allocating each price level's
// fills in proportion to resting size, rather than by time priority. Self orders
// are skipped. Trades print at the maker's price and are appended to dst.
func (e *Engine) matchProRata(taker *types.Order, dst []types.Trade) ([]types.Trade, map[int64]*types.Order) {
	var makerOrders map[int64]*types.Order
	trackMakers := taker.TimeInForce == types.TIFFillOrKill
	start := len(dst)

	for taker.RemainingQty != 0 {
		var price int64
		var oppSide types.Side
		if taker.Side == types.SideBuy {
			p, _, ok := e.book.BestAsk()
			if !ok || (taker.Type == types.OrderTypeLimit && taker.Price < p) {
				break
			}
			price, oppSide = p, types.SideSell
		} else {
			p, _, ok := e.book.BestBid()
			if !ok || (taker.Type == types.OrderTypeLimit && taker.Price > p) {
				break
			}
			price, oppSide = p, types.SideBuy
		}

		// Eligible resting orders at this level (excluding the taker's own).
		eligible := make([]*types.Order, 0)
		var total int64
		for _, o := range e.book.GetOrdersAtPrice(oppSide, price) {
			if e.isSelfMatch(taker, o) {
				continue
			}
			eligible = append(eligible, o)
			total += o.RemainingQty
		}
		if total == 0 {
			break // only self liquidity here; stop
		}

		q := min(taker.RemainingQty, total)
		allocs := proRataAllocate(eligible, q)
		for i, maker := range eligible {
			a := allocs[i]
			if a <= 0 {
				continue
			}
			dst = e.executeTrade(taker, maker, price, a, dst)
			if trackMakers {
				if makerOrders == nil {
					makerOrders = make(map[int64]*types.Order)
				}
				makerOrders[maker.ID] = maker
			}
			if maker.IsFilled() {
				_, _ = e.book.Remove(maker.ID)
				if ib, ok := e.icebergOrders[maker.ID]; ok {
					if ib.Refill() {
						_ = e.book.Add(ib.Order)
						e.emitAdd(ib.Order)
					} else {
						delete(e.icebergOrders, maker.ID)
					}
				}
				e.cancelOCOCounterpart(maker.ID)
			} else {
				e.book.UpdateOrderQuantity(maker.ID, a)
			}
		}
		// If the level wasn't fully consumed, the taker is filled ⇒ loop ends.
	}

	return e.recordLast(dst, start), makerOrders
}

// proRataAllocate splits q across orders in proportion to their remaining size,
// capping each at its size and distributing any integer remainder greedily so
// the allocations sum to exactly q.
func proRataAllocate(orders []*types.Order, q int64) []int64 {
	var total int64
	for _, o := range orders {
		total += o.RemainingQty
	}
	allocs := make([]int64, len(orders))
	var allocated int64
	for i, o := range orders {
		a := min(q*o.RemainingQty/total, o.RemainingQty)
		allocs[i] = a
		allocated += a
	}
	leftover := q - allocated
	for i, o := range orders {
		if leftover <= 0 {
			break
		}
		spare := o.RemainingQty - allocs[i]
		add := min(spare, leftover)
		if add > 0 {
			allocs[i] += add
			leftover -= add
		}
	}
	return allocs
}

// recordLast sets the last trade price from the final trade appended since start
// (if any) and returns dst unchanged.
func (e *Engine) recordLast(dst []types.Trade, start int) []types.Trade {
	if len(dst) > start {
		e.book.SetLastTradePrice(dst[len(dst)-1].Price)
	}
	return dst
}

// executeTrade fills both sides, sequences the trade, and appends it (as a value)
// to dst at price — no per-trade heap allocation.
func (e *Engine) executeTrade(taker, maker *types.Order, price, qty int64, dst []types.Trade) []types.Trade {
	_ = taker.Fill(qty)
	_ = maker.Fill(qty)
	e.tradeSeq++

	// The command's instant, not a fresh clock read: every fill a single order
	// causes shares one timestamp.
	now := e.commandNow()
	taker.UpdatedAt = now
	maker.UpdatedAt = now

	// Self-output guardrail: trip to Halted if trade/notional volume in the
	// current window exceeds the configured cap (the trip takes effect on the next
	// order — the current order's own quantity is already bounded).
	if e.guard.enabled() {
		if e.windowStart.IsZero() || now.Sub(e.windowStart) >= e.guard.Window {
			e.windowStart, e.windowTrades, e.windowNotional = now, 0, 0
		}
		e.windowTrades++
		prod, ok := checkedMul(price, qty)
		if !ok {
			prod = math.MaxInt64
		}
		e.windowNotional = saturatingAdd(e.windowNotional, prod)
		tripped := (e.guard.MaxTrades > 0 && e.windowTrades > e.guard.MaxTrades) ||
			(e.guard.MaxNotional > 0 && e.windowNotional > e.guard.MaxNotional)
		if tripped && e.state != StateHalted {
			e.state = StateHalted
			e.emitStateChange(EventHalted, "") // page operators on the Knight tripwire
		}
	}

	var buy, sell *types.Order
	if taker.Side == types.SideBuy {
		buy, sell = taker, maker
	} else {
		buy, sell = maker, taker
	}
	tr := types.NewTradeValue(e.config.Symbol, price, qty, buy, sell, taker.Side)
	// Trade ids are partitioned like order ids, and for a sharper reason: a bust
	// names a trade by id alone on both wire edges, so at a multi-symbol venue an
	// unpartitioned trade id would annul an ambiguous print. SequenceNum stays the
	// raw per-shard counter — it is a position in this engine's tape, not a name.
	tr.ID = ComposeID(e.config.ShardIndex, e.tradeSeq)
	tr.SequenceNum = e.tradeSeq
	tr.CreatedAt = now
	dst = append(dst, tr)
	e.pendTrade(&dst[len(dst)-1])
	return dst
}

// pendTrade records an execution at the moment it happens.
func (e *Engine) pendTrade(tr *types.Trade) {
	if e.sink == nil {
		return
	}
	e.pending = append(e.pending, Event{Kind: EventTrade, OrderID: tr.TakerOrderID, Trade: tr})
}

// ForceTrade injects a trade between two orders at price for qty — a privileged
// forced match for the risk layer's liquidation / auto-deleveraging (ADL) logic,
// used after it has selected a counterparty (e.g. ranked by profit × leverage) and
// a fillable/bankruptcy price. It bypasses price-time matching, STP, and the band,
// and does not touch the book (positions are off-book risk state). Both orders are
// filled, the trade is sequenced and emitted on the event stream, and returned.
// qty must be positive and within each order's remaining quantity.
func (e *Engine) ForceTrade(taker, maker *types.Order, price, qty int64) (*types.Trade, error) {
	if qty <= 0 || qty > taker.RemainingQty || qty > maker.RemainingQty {
		return nil, types.ErrInvalidQuantity
	}
	// Bound a single forced print so a liquidation must be chunked rather than
	// sweep the book in one trade (the incremental-liquidation lesson).
	if e.config.MaxForceTradeQty > 0 && qty > e.config.MaxForceTradeQty {
		return nil, types.ErrForceTradeTooLarge
	}
	e.nextID(taker)
	e.nextID(maker)
	dst := e.executeTrade(taker, maker, price, qty, nil)
	tr := dst[0]
	if e.sink != nil {
		e.eventSeq++
		e.eventBuf = append(e.eventBuf[:0], Event{Seq: e.eventSeq, Kind: EventTrade, OrderID: tr.TakerOrderID, Trade: &tr})
		e.sink.OnEvents(e.eventBuf)
	}
	return &tr, nil
}

// reverseTrade unwinds a single trade against a maker (FOK failure path),
// restoring the maker's quantities, its resting level total, and re-adding it to
// the book if it had been fully consumed.
func (e *Engine) reverseTrade(tr types.Trade, makerOrders map[int64]*types.Order) {
	maker, ok := makerOrders[tr.MakerOrderID]
	if !ok {
		maker, ok = e.book.Get(tr.MakerOrderID)
	}
	if !ok {
		return
	}

	maker.RemainingQty += tr.Quantity
	maker.FilledQty -= tr.Quantity
	if maker.FilledQty == 0 {
		maker.Status = types.OrderStatusNew
	} else {
		maker.Status = types.OrderStatusPartiallyFilled
	}

	if _, inBook := e.book.Get(maker.ID); inBook {
		// Defensive: a still-resting maker was only partially consumed, so
		// restore its level's aggregate quantity. The current FOK-only caller
		// never reaches this branch — a partial maker implies the taker was
		// fully filled, i.e. FOK success — but keeping it makes reverseTrade
		// correct for reuse by any future reversal path.
		e.book.RestoreOrderQuantity(maker.ID, tr.Quantity)
	} else {
		// Was fully consumed and removed: put it back (Add uses RemainingQty).
		_ = e.book.Add(maker)
	}
}

// Reduce shrinks a resting order in place, keeping its queue position, and
// returns the order.
//
// This is the one order-entry operation a gateway provably cannot build on the
// outside. Cancel-then-new is the obvious substitute and it is wrong: it sends
// the order to the back of its price level, which for a market maker managing
// size is a material loss. Retaining priority requires mutating the order where
// it rests, which only the goroutine that owns the book may do.
//
// It is a reduction only. A size INCREASE, or a price change, correctly forfeits
// priority — a resting order that could grow ahead of the queue would let a
// participant reserve a place in line — so those belong in the gateway as
// cancel-then-new, and are rejected here rather than silently reinterpreted.
//
// newQty is the new TOTAL quantity, matching how the order was submitted. An
// order already filled beyond newQty cannot shrink to it; that returns
// ErrInvalidQuantity rather than quietly clamping, since the caller's model of
// the order is wrong and it should find out.
//
// MinRestingTime is enforced, returning ErrCancelTooSoon — a reduce is a partial
// withdrawal of displayed size, so exempting it would leave the anti-spoofing
// floor guarding one verb and not the other.
func (e *Engine) Reduce(orderID int64, newQty int64, userID string) (*types.Order, error) {
	order, exists := e.book.Get(orderID)
	if !exists {
		return nil, types.ErrOrderNotFound
	}
	if order.UserID != userID {
		// Same response as a missing order: a probe must not be able to tell
		// "not yours" from "does not exist".
		return nil, types.ErrOrderNotFound
	}
	if !order.IsActive() {
		return nil, types.ErrOrderNotActive
	}
	if newQty <= 0 || newQty >= order.Quantity {
		return nil, types.ErrInvalidQuantity
	}
	if newQty <= order.FilledQty {
		return nil, types.ErrInvalidQuantity
	}
	// Minimum resting time applies here exactly as it does to a cancel, and for
	// the same reason: this control exists to stop displayed size being withdrawn
	// before it can fill, and a reduce from 1000 lots to 1 withdraws 999 of them.
	// Enforcing it on Cancel alone would leave the Coscia pattern intact behind a
	// different verb — which mattered little while only an embedder could call
	// this, and matters a great deal now that a client can.
	//
	// Checked after the quantity validation so an impossible request always gets
	// ErrInvalidQuantity rather than an error that depends on the clock. Bypassed
	// in replay and for privileged (liquidation) orders, as Cancel is.
	if e.config.MinRestingTime > 0 && !e.replaying && !order.Privileged {
		if e.clock().UTC().Sub(order.CreatedAt) < e.config.MinRestingTime {
			return nil, types.ErrCancelTooSoon
		}
	}

	released := order.Quantity - newQty
	order.Quantity = newQty
	order.RemainingQty = newQty - order.FilledQty
	order.UpdatedAt = e.clock().UTC()
	// The level's aggregate must lose exactly what the order gave up, or depth
	// drifts from the sum of its orders.
	e.book.UpdateOrderQuantity(orderID, released)

	e.emitReplaced(order)
	e.flushPending()
	return order, nil
}

// Replace cancels a resting order and submits a replacement in one command, so no
// other order can interleave between the two.
//
// It is the atomic form of cancel-then-new, and it exists because the two-message
// sequence leaves the client naked in between: if its connection dies in the gap it
// cannot tell whether it holds zero orders or one.
//
// Queue priority is forfeited by design — an order that could reprice or grow in
// place would let a participant reserve a place in line. Use Reduce for a same-price
// size reduction, which keeps priority.
//
// The failure semantics are asymmetric on purpose:
//
//   - If the original cannot be cancelled — missing, not this user's, inactive, or
//     inside MinRestingTime — the replacement is NOT submitted and the error is
//     returned. A client replacing an order it no longer holds did not ask to open a
//     new position, and entering one would double its exposure at the worst moment.
//   - If the cancel succeeds and the replacement is then refused, the client holds
//     neither. That is reported by the Canceled and Rejected events of this same
//     command, so it is immediately visible rather than discovered later. Restoring
//     the original instead would hand it back at the tail of the queue without
//     saying so, and a silent loss of priority is worse than a reported refusal.
func (e *Engine) Replace(orderID int64, userID string, replacement *types.Order) (*MatchResult, error) {
	if replacement == nil {
		return nil, types.ErrNilOrder
	}
	// Cancel first, and only proceed if it actually happened. Cancel enforces
	// ownership and MinRestingTime, so a replace cannot be used to sidestep the
	// anti-spoofing floor that a bare cancel obeys.
	if _, err := e.Cancel(orderID, userID); err != nil {
		return nil, err
	}
	return e.Process(replacement), nil
}

// OpenOrdersFor returns deep copies of every resting order belonging to userID,
// in book order. Copies, because the originals are engine-owned and the matching
// goroutine keeps mutating them.
//
// This is the authoritative answer to "what do I have live?" — taken from the
// book itself rather than from any consumer's shadow view, which is the point: a
// client asks precisely because it no longer trusts its own picture.
//
// Pending stops and trailing stops are excluded. They are not resting orders and
// a client reconciling its book should not see them as such; report them
// separately if you need to.
func (e *Engine) OpenOrdersFor(userID string) []*types.Order {
	var out []*types.Order
	for _, o := range e.book.Orders() {
		if o.UserID == userID && o.IsActive() {
			out = append(out, copyOrder(o))
		}
	}
	return out
}

// RestingOrders returns deep copies of every active resting order, across all
// accounts, in book order.
//
// It exists for the layer above to rebuild whatever per-order state it keeps after
// a recovery — a session layer's client-order-id index, say. Without it, an engine
// restored from a snapshot and log tail holds orders that nothing outside can name.
//
// Copies, for the same reason OpenOrdersFor makes them: the originals are
// engine-owned. Like OpenOrdersFor, this excludes pending stops and trailing stops,
// which are not resting orders.
func (e *Engine) RestingOrders() []*types.Order {
	orders := e.book.Orders()
	out := make([]*types.Order, 0, len(orders))
	for _, o := range orders {
		if o.IsActive() {
			out = append(out, copyOrder(o))
		}
	}
	return out
}

// CancelAllForUser cancels every resting order, pending stop and trailing stop
// belonging to userID, returning what it removed. This is the operator kill
// switch, so it deliberately ignores MinRestingTime: an anti-spoofing floor that
// blocked an operator from pulling a participant's book would be a liability, not
// a control.
//
// It must run on the goroutine that owns the engine. Cancelling account-wide from
// outside the writer races every concurrent submit, which is exactly the moment
// you are trying to stop.
func (e *Engine) CancelAllForUser(userID string) []*types.Order {
	var out []*types.Order
	now := e.clock().UTC()

	for _, o := range e.book.Orders() {
		if o.UserID != userID || !o.IsActive() {
			continue
		}
		if err := o.Cancel(); err != nil {
			continue
		}
		o.UpdatedAt = now
		_, _ = e.book.Remove(o.ID)
		delete(e.icebergOrders, o.ID)
		e.cancelOCOCounterpart(o.ID)
		e.emitCancel(o)
		out = append(out, o)
	}
	for _, s := range e.stopBook.All() {
		if s.Order.UserID != userID {
			continue
		}
		e.stopBook.Remove(s.Order.ID)
		if err := s.Order.Cancel(); err != nil {
			continue
		}
		s.Order.UpdatedAt = now
		e.emitCancel(s.Order)
		out = append(out, s.Order)
	}
	for id, ts := range e.trailingStops {
		if ts.Order.UserID != userID {
			continue
		}
		delete(e.trailingStops, id)
		if err := ts.Order.Cancel(); err != nil {
			continue
		}
		ts.Order.UpdatedAt = now
		e.emitCancel(ts.Order)
		out = append(out, ts.Order)
	}
	e.flushPending()
	return out
}

// Cancel removes a resting order (or a pending stop) if it belongs to userID and
// is still active.
func (e *Engine) Cancel(orderID int64, userID string) (*types.Order, error) {
	// Guarded, not called. Reading the clock costs ~27ns on this hardware and a cancel
	// is ~35ns, so an unconditional stamp here would have roughly doubled the cost of
	// the single most common operation in real order flow — to service a schedule that
	// is empty on any venue not using DAY or GTD.
	if len(e.expiries) > 0 {
		e.now = e.clock().UTC()
		e.expireDue()
	}
	now := e.clock().UTC()

	if order, exists := e.book.Get(orderID); exists {
		if order.UserID != userID {
			return nil, types.ErrOrderNotFound
		}
		if !order.IsActive() {
			return nil, types.ErrOrderNotActive
		}
		// Minimum resting time: a live cancel arriving before the order has
		// rested long enough is rejected (anti-spoofing). Bypassed in replay
		// (the log already reflects an accepted cancel) and for privileged orders.
		if e.config.MinRestingTime > 0 && !e.replaying && !order.Privileged {
			if now.Sub(order.CreatedAt) < e.config.MinRestingTime {
				return nil, types.ErrCancelTooSoon
			}
		}
		if err := order.Cancel(); err != nil {
			return nil, err
		}
		order.UpdatedAt = now
		_, _ = e.book.Remove(orderID)
		delete(e.icebergOrders, orderID) // no-op for non-iceberg orders
		e.emitCancel(order)
		e.flushPending()
		return order, nil
	}

	if s, exists := e.stopBook.Get(orderID); exists {
		if s.Order.UserID != userID {
			return nil, types.ErrOrderNotFound
		}
		e.stopBook.Remove(orderID)
		_ = s.Order.Cancel()
		s.Order.UpdatedAt = now
		e.emitCancel(s.Order)
		e.flushPending()
		return s.Order, nil
	}

	if ts, exists := e.trailingStops[orderID]; exists {
		if ts.Order.UserID != userID {
			return nil, types.ErrOrderNotFound
		}
		delete(e.trailingStops, orderID)
		_ = ts.Order.Cancel()
		ts.Order.UpdatedAt = now
		e.emitCancel(ts.Order)
		e.flushPending()
		return ts.Order, nil
	}

	return nil, types.ErrOrderNotFound
}

// --- read-only accessors (delegate to the book) ---

// Book returns the underlying order book (read model for signals/UI).
func (e *Engine) Book() *orderbook.OrderBook { return e.book }

// StopBook returns the underlying stop book.
func (e *Engine) StopBook() *orderbook.StopBook { return e.stopBook }

// PendingStopCount returns the number of resting stop orders.
func (e *Engine) PendingStopCount() int { return e.stopBook.Count() }

// TrailingStopCount returns the number of resting trailing stops.
// It must be called from the goroutine that owns the engine: trailing stops live in
// a plain map with no lock of its own, unlike the book and stop book. Use
// Runner.TrailingStopCount from anywhere else.
func (e *Engine) TrailingStopCount() int {
	return len(e.trailingStops)
}

// BestBid returns the best bid price (ticks) and aggregate quantity (lots).
func (e *Engine) BestBid() (price, qty int64, ok bool) { return e.book.BestBid() }

// BestAsk returns the best ask price (ticks) and aggregate quantity (lots).
func (e *Engine) BestAsk() (price, qty int64, ok bool) { return e.book.BestAsk() }

// Spread returns best ask − best bid (ticks).
func (e *Engine) Spread() (int64, bool) { return e.book.Spread() }

// MidPrice returns (best bid + best ask) / 2 (ticks, floored).
func (e *Engine) MidPrice() (int64, bool) { return e.book.MidPrice() }

// LastTradePrice returns the most recent execution price (ticks).
func (e *Engine) LastTradePrice() int64 { return e.book.LastTradePrice() }

// OrderCount returns the number of resting orders.
func (e *Engine) OrderCount() int { return e.book.Count() }

// Snapshot returns a top-of-book view to the given depth.
func (e *Engine) Snapshot(depth int) *orderbook.Snapshot { return e.book.Snapshot(depth) }
