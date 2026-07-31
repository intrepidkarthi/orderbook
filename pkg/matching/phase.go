package matching

import (
	"github.com/intrepidkarthi/orderbook/pkg/auction"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Trading phases, and the uncross that joins pre-open to continuous trading.
//
// # What a phase is, and what it is not
//
// The engine knows which phase it is in and what that phase permits. It does not know
// when phases change: there is no calendar here, no scheduled open, no holiday table.
// The venue calls SetPhase, because the trading calendar is a venue's business and
// embedding one would force every embedder to accept somebody else's — the same
// reasoning that keeps consensus out of this repository.
//
// # Pre-open
//
// Orders are accepted and not matched, so the book may legitimately cross: a bid above
// an ask simply rests there. That is the point. Everything that accumulates is then
// resolved at a single price by Uncross, which is what an opening auction is.
//
// This is the one place the engine deliberately holds a crossed book. Everywhere else
// a crossed book would be a bug.
//
// # The uncross
//
// One clearing price, chosen to maximise executed volume — the standard call-auction
// rule, already implemented in pkg/auction and reused here rather than rewritten.
// Every buy at or above it and every sell at or below it trades, in price-time
// priority, all at that one price.
//
// Auction prints carry Trade.Auction. In an auction both sides were resting and there
// is no aggressor, so TakerSide is not meaningful on them — a consumer inferring order
// flow from aggressor side has to exclude them rather than treat the convention as
// data.

// restOrder places an order on the book without matching it. Used only in pre-open,
// where a crossed book is the intended state.
func (e *Engine) restOrder(order *types.Order) error {
	if !order.IsActive() || order.IsFilled() {
		return nil
	}
	if err := e.book.Add(order); err != nil {
		return err
	}
	e.emitAdd(order)
	e.flushPending()
	return nil
}

// SetPhase moves the engine to a new trading phase, announcing the change.
//
// Moving from pre-open to open runs the uncross first, so the crossed book that
// pre-open accumulated is resolved before continuous trading begins — a venue must
// never open onto a book where a bid sits above an ask.
//
// Returns the trades the uncross produced, which is nothing unless that is the
// transition being made. Single-writer: call from the engine's goroutine, or via
// Runner.SetPhase.
func (e *Engine) SetPhase(phase EngineState) []types.Trade {
	if e.state == phase {
		return nil
	}
	var trades []types.Trade
	if e.state == StatePreOpen && phase == StateOpen {
		trades = e.Uncross(nil)
	}
	e.state = phase
	switch phase {
	case StateHalted:
		e.emitStateChange(EventHalted, "")
	case StateCancelOnly:
		e.emitStateChange(EventCancelOnly, "")
	case StateOpen:
		e.emitStateChange(EventResumed, "")
	case StatePreOpen, StateClosed:
		// Both refuse new liquidity in their own way and neither is a halt. They reuse
		// the cancel-only kind rather than getting one each: a consumer needs to know
		// it cannot trade, and Engine.State names the phase exactly for anything that
		// needs more than that.
		e.emitStateChange(EventCancelOnly, "")
	}
	return trades
}

// Uncross resolves a crossed book at a single clearing price, appending the resulting
// trades to dst.
//
// It is the opening auction, and it is safe to call in any phase: if nothing crosses,
// nothing happens. The clearing price maximises executed volume, with ties broken
// deterministically by pkg/auction so two runs of the same book produce the same
// result — which matters, because this runs during replay too.
//
// Allocation is price-time priority within the clearing price, the same rule the
// continuous book uses. A venue that allocated an auction differently from its
// continuous session would be two venues.
func (e *Engine) Uncross(dst []types.Trade) []types.Trade {
	e.now = e.clock().UTC()
	snap := e.book.Snapshot(1 << 20)
	bids := make([]auction.Level, 0, len(snap.Bids))
	for _, l := range snap.Bids {
		bids = append(bids, auction.Level{Price: l.Price, Qty: l.Quantity})
	}
	asks := make([]auction.Level, 0, len(snap.Asks))
	for _, l := range snap.Asks {
		asks = append(asks, auction.Level{Price: l.Price, Qty: l.Quantity})
	}
	res := auction.Uncross(bids, asks)
	if !res.HasClearing || res.Volume <= 0 {
		return dst
	}

	price := res.ClearingPrice
	remaining := res.Volume
	for remaining > 0 {
		buy, ok := e.bestEligible(types.SideBuy, price)
		if !ok {
			break
		}
		sell, ok := e.bestEligible(types.SideSell, price)
		if !ok {
			break
		}
		// A participant matching itself in an auction is still a self-trade; the
		// configured policy applies rather than being quietly suspended because this
		// is an auction.
		if buy.UserID == sell.UserID && e.config.SelfTradePrevention != STPAllow {
			// Remove the newer of the two and continue, which is the auction analogue
			// of cancel-newest and avoids an infinite loop on a self-crossed pair.
			victim := buy
			if sell.ID > buy.ID {
				victim = sell
			}
			_ = victim.Cancel()
			victim.UpdatedAt = e.now
			_, _ = e.book.Remove(victim.ID)
			e.emitCancelReason(victim, types.ErrSelfTradeNotAllowed)
			continue
		}

		qty := min64(buy.RemainingQty, sell.RemainingQty)
		if qty > remaining {
			qty = remaining
		}
		if qty <= 0 {
			break
		}
		before := len(dst)
		dst = e.executeTrade(buy, sell, price, qty, dst)
		for i := before; i < len(dst); i++ {
			dst[i].Auction = true
		}
		remaining -= qty
		e.settleAuctionSide(buy, qty)
		e.settleAuctionSide(sell, qty)
	}
	e.flushPending()
	return dst
}

// bestEligible returns the highest-priority resting order on side that is willing to
// trade at price. O(1) via the book's own front-of-level accessor: walking Orders()
// would allocate and scan the whole book per fill, making an uncross O(book^2) at
// exactly the moment a venue is trying to open.
func (e *Engine) bestEligible(side types.Side, price int64) (*types.Order, bool) {
	o, ok := e.book.FrontOrder(side)
	if !ok {
		return nil, false
	}
	if side == types.SideBuy && o.Price < price {
		return nil, false // the best bid will not pay the clearing price
	}
	if side == types.SideSell && o.Price > price {
		return nil, false // the best ask will not accept it
	}
	return o, true
}

// settleAuctionSide removes a fully filled order and keeps the level aggregate in step
// for one that was only partly filled.
//
// filled is what this trade took. executeTrade has already reduced RemainingQty on the
// order; the level total is separate bookkeeping and has to be told, exactly as the
// continuous path tells it.
func (e *Engine) settleAuctionSide(o *types.Order, filled int64) {
	if o.IsFilled() {
		_, _ = e.book.Remove(o.ID)
		e.cancelOCOCounterpart(o.ID)
		return
	}
	e.book.UpdateOrderQuantity(o.ID, filled)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
