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
)

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
	// MaxMarkStep bounds how far a single SetMarkPrice update may move the mark
	// from its current value, as a fraction (e.g. 0.20 = ±20%). A larger jump is
	// rejected with ErrMarkStepTooLarge, so a thin-book oracle pump cannot drag
	// the price band with it (the Mango / Hyperliquid-JELLY lesson). Zero
	// disables the guard. The first mark (from 0) is always accepted.
	MaxMarkStep decimal.Decimal
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
	state         EngineState
	bandEnabled   bool // config.PriceBand > 0, precomputed to keep decimal off the hot path
	markStepEnab  bool // config.MaxMarkStep > 0, precomputed
	replaying     bool // replay/bootstrap mode: bypass live-ingress admission controls
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

	// ordered event stream
	sink     EventSink
	eventSeq int64
	eventBuf []Event
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
		bandEnabled:  config.PriceBand.GreaterThan(decimal.Zero),
		markStepEnab: config.MaxMarkStep.GreaterThan(decimal.Zero),
		clock:        config.Clock,
		disabled:     disabled,
		guard:        config.Guardrail,
		sink:         config.EventSink,
	}
}

// SetReplaying toggles replay/bootstrap mode. In replay mode the engine bypasses
// the live-ingress admission controls (minimum resting time and the per-order
// size caps) because the command log it is replaying already reflects commands
// that passed those checks live — re-litigating them against replay-time
// timestamps would wrongly reject an accepted cancel and diverge the recovered
// book. Recovery paths (see pkg/wal Restore) wrap replay in SetReplaying(true) /
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
// orders bypass the configured caps but are still overflow-checked. Bypassed in
// replay mode.
func (e *Engine) checkOrderCaps(order *types.Order) error {
	if e.replaying {
		return nil
	}
	if e.config.MaxOrderQty > 0 && !order.Privileged && order.Quantity > e.config.MaxOrderQty {
		return types.ErrOrderExceedsMaxQty
	}
	if order.Type == types.OrderTypeLimit {
		notional, ok := checkedMul(order.Price, order.Quantity)
		if !ok {
			return types.ErrNotionalOverflow
		}
		if e.config.MaxOrderNotional > 0 && !order.Privileged && notional > e.config.MaxOrderNotional {
			return types.ErrOrderExceedsMaxNotional
		}
	}
	return nil
}

// nextID assigns the order a monotonic engine id if it does not already carry
// one, and returns it. Orders enter with ID==0 (see types.NewOrder); replayed
// orders reset to 0 via Fresh, so ids are reproducible in submission order.
func (e *Engine) nextID(order *types.Order) int64 {
	if order.ID == 0 {
		e.orderSeq++
		order.ID = e.orderSeq
	}
	// The engine is the single writer that owns time: it stamps the authoritative
	// timestamps on intake from its injected clock, so replay is reproducible.
	now := e.clock().UTC()
	if order.CreatedAt.IsZero() {
		order.CreatedAt = now
	}
	order.UpdatedAt = now
	return order.ID
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
	dst, status, reason := e.settleInto(order, dst)
	dst = e.cascadeStops(dst)
	e.emitResult(order, dst[start:], status, reason)
	return dst, status, reason
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
	buf := e.eventBuf[:0]
	e.eventSeq++
	if status == types.OrderStatusRejected {
		buf = append(buf, Event{Seq: e.eventSeq, Kind: EventRejected, OrderID: order.ID, UserID: order.UserID, Order: order, Reason: reason})
	} else {
		buf = append(buf, Event{Seq: e.eventSeq, Kind: EventAccepted, OrderID: order.ID, UserID: order.UserID, Order: order})
	}
	for i := range trades {
		e.eventSeq++
		buf = append(buf, Event{Seq: e.eventSeq, Kind: EventTrade, OrderID: trades[i].TakerOrderID, Trade: &trades[i]})
	}
	e.eventBuf = buf
	e.sink.OnEvents(buf)
}

// emitCancel publishes a single Canceled event for a removed resting order.
func (e *Engine) emitCancel(order *types.Order) {
	if e.sink == nil {
		return
	}
	e.eventSeq++
	e.eventBuf = append(e.eventBuf[:0], Event{Seq: e.eventSeq, Kind: EventCanceled, OrderID: order.ID, UserID: order.UserID, Order: order})
	e.sink.OnEvents(e.eventBuf)
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
	// Circuit breakers: engine state, then a limit price outside the collar.
	switch e.state {
	case StateHalted:
		order.Status = types.OrderStatusRejected
		return dst, types.OrderStatusRejected, types.ErrTradingHalted
	case StateCancelOnly:
		order.Status = types.OrderStatusRejected
		return dst, types.OrderStatusRejected, types.ErrNewOrdersHalted
	}
	// Pre-trade risk caps (fat-finger size/notional + int64 overflow guard).
	if err := e.checkOrderCaps(order); err != nil {
		order.Status = types.OrderStatusRejected
		return dst, types.OrderStatusRejected, err
	}
	if order.Type == types.OrderTypeLimit && !order.Privileged && e.outsideBand(order.Price) {
		order.Status = types.OrderStatusRejected
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
			dst, _, _ = e.settleInto(s.Order, dst)
		}
		for _, ts := range trailing {
			dst, _, _ = e.settleInto(ts.Order, dst)
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

	// Otherwise post the stop; if it fires on entry, cancel the resting primary.
	// Any trades the stop prints on entry are reported when it is observed, not
	// folded into the primary's result.
	e.submitStopInto(oco.Stop, nil)
	if oco.Stop.IsTriggered() {
		e.cancelOCOCounterpart(oco.Stop.Order.ID)
	}
	e.emitResult(oco.Primary, dst, status, reason)
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
	} else if s, exists := e.stopBook.Get(otherID); exists {
		_ = s.Order.Cancel()
		e.stopBook.Remove(otherID)
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
				continue
			case STPCancelBoth:
				taker.Status = types.OrderStatusCancelled
				_ = maker.Cancel()
				_, _ = e.book.Remove(maker.ID)
				return e.recordLast(dst, start), makerOrders
			case STPDecrement:
				// Reduce both by the overlap with no trade; the smaller side fully
				// cancels, the larger shrinks; then continue matching the taker.
				e.decrement(taker, maker)
				if maker.RemainingQty == 0 {
					_, _ = e.book.Remove(maker.ID)
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
	e.state = StateHalted
}

// SetCancelOnly puts the engine in cancel-only mode: cancels are accepted but new
// liquidity is rejected (ErrNewOrdersHalted). Used to wind a venue down under
// stress before a full halt or auction reopen.
func (e *Engine) SetCancelOnly() {
	e.state = StateCancelOnly
}

// Resume returns the engine to normal (Open) trading.
func (e *Engine) Resume() {
	e.state = StateOpen
}

// State reports the current trading state.
func (e *Engine) State() EngineState { return e.state }

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
	e.markPrice = price
	return nil
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

	now := e.clock().UTC()
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
		if (e.guard.MaxTrades > 0 && e.windowTrades > e.guard.MaxTrades) ||
			(e.guard.MaxNotional > 0 && e.windowNotional > e.guard.MaxNotional) {
			e.state = StateHalted
		}
	}

	var buy, sell *types.Order
	if taker.Side == types.SideBuy {
		buy, sell = taker, maker
	} else {
		buy, sell = maker, taker
	}
	tr := types.NewTradeValue(e.config.Symbol, price, qty, buy, sell, taker.Side)
	tr.ID = e.tradeSeq
	tr.SequenceNum = e.tradeSeq
	tr.CreatedAt = now
	return append(dst, tr)
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

// Cancel removes a resting order (or a pending stop) if it belongs to userID and
// is still active.
func (e *Engine) Cancel(orderID int64, userID string) (*types.Order, error) {
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
