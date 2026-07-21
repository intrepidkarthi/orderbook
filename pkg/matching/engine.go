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
package matching

import (
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
	icebergOrders map[string]*types.IcebergOrder
	ocoByOrderID  map[string]*types.OCOOrder // both legs' ids map to the pair
	orderSeq      uint64
	tradeSeq      uint64
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
		icebergOrders: make(map[string]*types.IcebergOrder),
		ocoByOrderID:  make(map[string]*types.OCOOrder),
	}
}

// Process runs one order through the engine: it crosses against the book, rests
// or rejects the remainder per type/TIF, then fires any stop orders whose
// trigger price the resulting trades reached.
func (e *Engine) Process(order *types.Order) *MatchResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.orderSeq++
	order.SequenceNum = e.orderSeq

	res := e.settle(order)
	e.cascadeStops(res)
	return res
}

// settle matches order and applies market/TIF resting rules. It assumes the
// engine lock is held and the order's sequence number is already assigned.
func (e *Engine) settle(order *types.Order) *MatchResult {
	res := &MatchResult{Order: order}

	// Post-only orders must rest as makers; reject if they would take.
	if order.PostOnly && e.wouldCross(order) {
		order.Status = types.OrderStatusRejected
		res.Status = types.OrderStatusRejected
		res.RejectionReason = types.ErrPostOnlyWouldCross
		return res
	}

	trades, makerOrders := e.match(order)
	res.Trades = trades

	// Market orders never rest.
	if order.Type == types.OrderTypeMarket {
		if !order.RemainingQty.IsZero() {
			order.Status = types.OrderStatusCancelled
			res.Status = types.OrderStatusCancelled
			if len(trades) == 0 {
				res.RejectionReason = types.ErrMarketOrderNoLiquidity
			}
		} else {
			res.Status = types.OrderStatusFilled
		}
		return res
	}

	// Limit orders by time-in-force.
	switch order.TimeInForce {
	case types.TIFImmediateOrCancel:
		// Whatever couldn't fill immediately is cancelled (never rests).
		if !order.RemainingQty.IsZero() && order.Status != types.OrderStatusCancelled {
			order.Status = types.OrderStatusCancelled
		}
		res.Status = order.Status

	case types.TIFFillOrKill:
		// All-or-nothing: if it didn't fully fill, unwind every trade and reject.
		if !order.IsFilled() {
			for _, tr := range trades {
				e.reverseTrade(tr, makerOrders)
			}
			order.Status = types.OrderStatusRejected
			res.Status = types.OrderStatusRejected
			res.RejectionReason = types.ErrFOKCannotFill
			res.Trades = nil
			return res
		}
		res.Status = types.OrderStatusFilled

	default: // GTC
		// Rest any active remainder on the book.
		if order.IsActive() && !order.IsFilled() {
			if err := e.book.Add(order); err != nil {
				order.Status = types.OrderStatusRejected
				res.Status = types.OrderStatusRejected
				res.RejectionReason = err
				return res
			}
		}
		res.Status = order.Status
	}

	return res
}

// cascadeStops fires any stop orders whose trigger price the latest trade
// reached, settling each and appending its trades to res. It repeats until no
// new stops fire — a triggered stop's own trades may trigger further stops —
// bounded by maxStopCascade.
func (e *Engine) cascadeStops(res *MatchResult) {
	for range maxStopCascade {
		mp := e.book.LastTradePrice()
		if !mp.IsPositive() {
			return
		}
		fired := e.stopBook.CheckTriggers(mp)
		if len(fired) == 0 {
			return
		}
		for _, s := range fired {
			// If this stop is an OCO leg, cancel its primary before it executes.
			e.cancelOCOCounterpart(s.Order.ID)
			r := e.settle(s.Order)
			res.Trades = append(res.Trades, r.Trades...)
		}
	}
}

// ProcessStop submits a stop (or stop-limit) order. If the market has already
// reached the stop it fires immediately; otherwise it rests off-book until a
// trade reaches the trigger price.
func (e *Engine) ProcessStop(stop *types.StopOrder) *MatchResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.submitStopLocked(stop)
}

// submitStopLocked is the body of ProcessStop; it assumes the engine lock is
// held so it can be reused by ProcessOCO.
func (e *Engine) submitStopLocked(stop *types.StopOrder) *MatchResult {
	e.orderSeq++
	stop.Order.SequenceNum = e.orderSeq

	if mp := e.book.LastTradePrice(); mp.IsPositive() && stop.ShouldTrigger(mp) {
		stop.Trigger()
		res := e.settle(stop.Order)
		e.cascadeStops(res)
		return res
	}

	stop.Order.Status = types.OrderStatusPendingTrigger
	e.stopBook.Add(stop)
	return &MatchResult{Order: stop.Order, Status: types.OrderStatusPendingTrigger}
}

// ProcessOCO submits a one-cancels-other pair: the primary limit is posted, and
// if it does not complete immediately the stop is posted too. Whichever leg
// completes first cancels the other (handled in match/cascadeStops via the OCO
// registry).
func (e *Engine) ProcessOCO(oco *types.OCOOrder) *MatchResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.ocoByOrderID[oco.Primary.ID] = oco
	e.ocoByOrderID[oco.Stop.Order.ID] = oco

	e.orderSeq++
	oco.Primary.SequenceNum = e.orderSeq
	res := e.settle(oco.Primary)
	e.cascadeStops(res)

	// Primary already done ⇒ the stop is never posted.
	if oco.Primary.IsFilled() {
		e.dropOCO(oco)
		return res
	}

	// Otherwise post the stop; if it fires on entry, cancel the resting primary.
	e.submitStopLocked(oco.Stop)
	if oco.Stop.IsTriggered() {
		e.cancelOCOCounterpart(oco.Stop.Order.ID)
	}
	return res
}

// cancelOCOCounterpart cancels the other leg of the OCO that legID belongs to
// (removing it from the book or stop book) and drops the pairing. No-op if legID
// is not part of a live OCO.
func (e *Engine) cancelOCOCounterpart(legID string) {
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

	e.orderSeq++
	ib.Order.SequenceNum = e.orderSeq
	e.icebergOrders[ib.Order.ID] = ib

	res := e.settle(ib.Order)
	// If the slice fully crossed on entry, keep refilling and re-settling until
	// it rests or the total is exhausted.
	for ib.Order.IsFilled() && !ib.IsFullyFilled() {
		if !ib.Refill() {
			break
		}
		r := e.settle(ib.Order)
		res.Trades = append(res.Trades, r.Trades...)
	}
	if ib.IsFullyFilled() {
		delete(e.icebergOrders, ib.Order.ID)
		res.Status = types.OrderStatusFilled
	}
	e.cascadeStops(res)
	return res
}

// match crosses taker against the resting book by price–time priority. It
// returns the trades produced and, only for FOK takers, the maker orders touched
// (so a failed FOK can be reversed). Trades print at the maker's resting price.
func (e *Engine) match(taker *types.Order) ([]*types.Trade, map[string]*types.Order) {
	var trades []*types.Trade
	var makerOrders map[string]*types.Order
	trackMakers := taker.TimeInForce == types.TIFFillOrKill

	for !taker.RemainingQty.IsZero() {
		var maker *types.Order
		if taker.Side == types.SideBuy {
			maker = e.book.PeekBestAskOrder()
			if maker == nil {
				break
			}
			// A limit buy only crosses asks at or below its price.
			if taker.Type == types.OrderTypeLimit && taker.Price.LessThan(maker.Price) {
				break
			}
		} else {
			maker = e.book.PeekBestBidOrder()
			if maker == nil {
				break
			}
			// A limit sell only crosses bids at or above its price.
			if taker.Type == types.OrderTypeLimit && taker.Price.GreaterThan(maker.Price) {
				break
			}
		}

		// Self-trade prevention.
		if taker.UserID == maker.UserID {
			switch e.config.SelfTradePrevention {
			case STPCancelNewest:
				taker.Status = types.OrderStatusCancelled
				return e.finish(trades), makerOrders
			case STPCancelOldest:
				_ = maker.Cancel()
				_, _ = e.book.Remove(maker.ID)
				continue
			case STPCancelBoth:
				taker.Status = types.OrderStatusCancelled
				_ = maker.Cancel()
				_, _ = e.book.Remove(maker.ID)
				return e.finish(trades), makerOrders
			case STPAllow:
				// fall through and trade
			}
		}

		qty := decimal.Min(taker.RemainingQty, maker.RemainingQty)
		tr := e.executeTrade(taker, maker, maker.Price, qty)
		trades = append(trades, tr)
		if trackMakers {
			if makerOrders == nil {
				makerOrders = make(map[string]*types.Order)
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

	return e.finish(trades), makerOrders
}

// wouldCross reports whether a limit order would immediately take liquidity
// (used to reject post-only orders). Non-limit orders always "cross".
func (e *Engine) wouldCross(order *types.Order) bool {
	if order.Type != types.OrderTypeLimit {
		return true
	}
	if order.Side == types.SideBuy {
		if ask, _, ok := e.book.BestAsk(); ok && order.Price.GreaterThanOrEqual(ask) {
			return true
		}
		return false
	}
	if bid, _, ok := e.book.BestBid(); ok && order.Price.LessThanOrEqual(bid) {
		return true
	}
	return false
}

// finish records the last trade price if any trades occurred and returns the
// trade slice unchanged (a small helper to keep the match loop's early returns
// consistent).
func (e *Engine) finish(trades []*types.Trade) []*types.Trade {
	if len(trades) > 0 {
		e.book.SetLastTradePrice(trades[len(trades)-1].Price)
	}
	return trades
}

// executeTrade fills both sides, sequences the trade, and builds it at price.
func (e *Engine) executeTrade(taker, maker *types.Order, price, qty decimal.Decimal) *types.Trade {
	_ = taker.Fill(qty)
	_ = maker.Fill(qty)
	e.tradeSeq++

	var buy, sell *types.Order
	if taker.Side == types.SideBuy {
		buy, sell = taker, maker
	} else {
		buy, sell = maker, taker
	}
	tr := types.NewTrade(e.config.Symbol, price, qty, buy, sell, taker.Side)
	tr.SequenceNum = e.tradeSeq
	return tr
}

// reverseTrade unwinds a single trade against a maker (FOK failure path),
// restoring the maker's quantities, its resting level total, and re-adding it to
// the book if it had been fully consumed.
func (e *Engine) reverseTrade(tr *types.Trade, makerOrders map[string]*types.Order) {
	maker, ok := makerOrders[tr.MakerOrderID]
	if !ok {
		maker, ok = e.book.Get(tr.MakerOrderID)
	}
	if !ok {
		return
	}

	maker.RemainingQty = maker.RemainingQty.Add(tr.Quantity)
	maker.FilledQty = maker.FilledQty.Sub(tr.Quantity)
	if maker.FilledQty.IsZero() {
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
func (e *Engine) Cancel(orderID, userID string) (*types.Order, error) {
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

	return nil, types.ErrOrderNotFound
}

// --- read-only accessors (delegate to the book) ---

// Book returns the underlying order book (read model for signals/UI).
func (e *Engine) Book() *orderbook.OrderBook { return e.book }

// StopBook returns the underlying stop book.
func (e *Engine) StopBook() *orderbook.StopBook { return e.stopBook }

// PendingStopCount returns the number of resting stop orders.
func (e *Engine) PendingStopCount() int { return e.stopBook.Count() }

// BestBid returns the best bid price and aggregate quantity.
func (e *Engine) BestBid() (price, qty decimal.Decimal, ok bool) { return e.book.BestBid() }

// BestAsk returns the best ask price and aggregate quantity.
func (e *Engine) BestAsk() (price, qty decimal.Decimal, ok bool) { return e.book.BestAsk() }

// Spread returns best ask − best bid.
func (e *Engine) Spread() (decimal.Decimal, bool) { return e.book.Spread() }

// MidPrice returns (best bid + best ask) / 2.
func (e *Engine) MidPrice() (decimal.Decimal, bool) { return e.book.MidPrice() }

// LastTradePrice returns the most recent execution price.
func (e *Engine) LastTradePrice() decimal.Decimal { return e.book.LastTradePrice() }

// OrderCount returns the number of resting orders.
func (e *Engine) OrderCount() int { return e.book.Count() }

// Snapshot returns a top-of-book view to the given depth.
func (e *Engine) Snapshot(depth int) *orderbook.Snapshot { return e.book.Snapshot(depth) }
