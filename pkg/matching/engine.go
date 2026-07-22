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
	"sort"
	"sync"

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
	// STPAllow permits the self-trade to execute.
	STPAllow SelfTradePrevention = "ALLOW"
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

// Engine matches orders for a single symbol. It is safe for concurrent use; all
// state mutation is serialized behind a mutex, preserving determinism.
type Engine struct {
	mu            sync.Mutex
	config        Config
	book          *orderbook.OrderBook
	stopBook      *orderbook.StopBook
	icebergOrders map[int64]*types.IcebergOrder
	ocoByOrderID  map[int64]*types.OCOOrder // both legs' ids map to the pair
	trailingStops map[int64]*types.TrailingStop
	halted        bool
	bandEnabled   bool // config.PriceBand > 0, precomputed to keep decimal off the hot path
	orderSeq      int64
	tradeSeq      int64
}

// maxStopCascade bounds how many rounds of stop triggering a single order may
// set off, a safety net against a pathological trigger loop.
const maxStopCascade = 1000

// NewEngine constructs an engine and its underlying book.
func NewEngine(config Config) *Engine {
	if config.SelfTradePrevention == "" {
		config.SelfTradePrevention = STPCancelNewest
	}
	return &Engine{
		config: config,
		book: orderbook.New(orderbook.Config{
			Symbol:    config.Symbol,
			MaxOrders: config.MaxOrders,
		}),
		stopBook:      orderbook.NewStopBook(config.Symbol),
		icebergOrders: make(map[int64]*types.IcebergOrder),
		ocoByOrderID:  make(map[int64]*types.OCOOrder),
		trailingStops: make(map[int64]*types.TrailingStop),
		// Resolve the band-enabled flag once (a decimal compare) so the per-order
		// hot path never touches decimal for the common band-disabled case.
		bandEnabled: config.PriceBand.GreaterThan(decimal.Zero),
	}
}

// nextID assigns the order a monotonic engine id if it does not already carry
// one, and returns it. Orders enter with ID==0 (see types.NewOrder); replayed
// orders reset to 0 via Fresh, so ids are reproducible in submission order.
func (e *Engine) nextID(order *types.Order) int64 {
	if order.ID == 0 {
		e.orderSeq++
		order.ID = e.orderSeq
	}
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
	e.mu.Lock()
	defer e.mu.Unlock()

	e.nextID(order)
	dst, status, reason := e.settleInto(order, dst)
	dst = e.cascadeStops(dst)
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

// settleInto matches order and applies market/TIF resting rules, appending trades
// to dst. It assumes the engine lock is held and the order's id is assigned, and
// returns the extended buffer, the order's final status, and any rejection reason.
func (e *Engine) settleInto(order *types.Order, dst []types.Trade) ([]types.Trade, types.OrderStatus, error) {
	// Circuit breakers: halted market, or a limit price outside the collar.
	if e.halted {
		order.Status = types.OrderStatusRejected
		return dst, types.OrderStatusRejected, types.ErrTradingHalted
	}
	if order.Type == types.OrderTypeLimit && e.outsideBand(order.Price) {
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
	e.mu.Lock()
	defer e.mu.Unlock()
	dst, status, reason := e.submitStopInto(stop, nil)
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
	e.mu.Lock()
	defer e.mu.Unlock()

	ref, ok := e.pegReference(p.Ref)
	if !ok {
		p.Order.Status = types.OrderStatusRejected
		return &MatchResult{Order: p.Order, Status: types.OrderStatusRejected, RejectionReason: types.ErrPegReferenceUnavailable}
	}
	price := ref + p.Offset
	if price <= 0 {
		p.Order.Status = types.OrderStatusRejected
		return &MatchResult{Order: p.Order, Status: types.OrderStatusRejected, RejectionReason: types.ErrInvalidPrice}
	}
	p.Order.Price = price
	p.Order.Type = types.OrderTypeLimit

	e.nextID(p.Order)
	dst, status, reason := e.settleInto(p.Order, nil)
	dst = e.cascadeStops(dst)
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
	e.mu.Lock()
	defer e.mu.Unlock()

	e.nextID(ts.Order)

	mp := e.book.LastTradePrice()
	if mp > 0 {
		ts.Observe(mp)
		if ts.ShouldTrigger(mp) {
			ts.Trigger()
			dst, status, reason := e.settleInto(ts.Order, nil)
			dst = e.cascadeStops(dst)
			return toMatchResult(ts.Order, dst, status, reason)
		}
	}
	ts.Order.Status = types.OrderStatusPendingTrigger
	e.trailingStops[ts.Order.ID] = ts
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
	e.mu.Lock()
	defer e.mu.Unlock()

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
		return toMatchResult(oco.Primary, dst, status, reason)
	}

	// Otherwise post the stop; if it fires on entry, cancel the resting primary.
	// Any trades the stop prints on entry are reported when it is observed, not
	// folded into the primary's result.
	e.submitStopInto(oco.Stop, nil)
	if oco.Stop.IsTriggered() {
		e.cancelOCOCounterpart(oco.Stop.Order.ID)
	}
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
	e.mu.Lock()
	defer e.mu.Unlock()

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

		// Self-trade prevention.
		if taker.UserID == maker.UserID {
			switch e.config.SelfTradePrevention {
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

// outsideBand reports whether price is beyond the circuit-breaker collar around
// the last trade price. Disabled when the band is zero or no trade has printed.
// The band is a decimal fraction; this runs only on limit-order entry (cold).
func (e *Engine) outsideBand(price int64) bool {
	if !e.bandEnabled {
		return false
	}
	ref := e.book.LastTradePrice()
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
	e.mu.Lock()
	defer e.mu.Unlock()
	e.halted = true
}

// Resume lifts a trading halt.
func (e *Engine) Resume() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.halted = false
}

// IsHalted reports whether trading is currently halted.
func (e *Engine) IsHalted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.halted
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
			if o.UserID == taker.UserID {
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

	var buy, sell *types.Order
	if taker.Side == types.SideBuy {
		buy, sell = taker, maker
	} else {
		buy, sell = maker, taker
	}
	tr := types.NewTradeValue(e.config.Symbol, price, qty, buy, sell, taker.Side)
	tr.ID = e.tradeSeq
	tr.SequenceNum = e.tradeSeq
	return append(dst, tr)
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
	e.mu.Lock()
	defer e.mu.Unlock()

	if order, exists := e.book.Get(orderID); exists {
		if order.UserID != userID {
			return nil, types.ErrOrderNotFound
		}
		if !order.IsActive() {
			return nil, types.ErrOrderNotActive
		}
		if err := order.Cancel(); err != nil {
			return nil, err
		}
		_, _ = e.book.Remove(orderID)
		delete(e.icebergOrders, orderID) // no-op for non-iceberg orders
		return order, nil
	}

	if s, exists := e.stopBook.Get(orderID); exists {
		if s.Order.UserID != userID {
			return nil, types.ErrOrderNotFound
		}
		e.stopBook.Remove(orderID)
		_ = s.Order.Cancel()
		return s.Order, nil
	}

	if ts, exists := e.trailingStops[orderID]; exists {
		if ts.Order.UserID != userID {
			return nil, types.ErrOrderNotFound
		}
		delete(e.trailingStops, orderID)
		_ = ts.Order.Cancel()
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
	e.mu.Lock()
	defer e.mu.Unlock()
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
