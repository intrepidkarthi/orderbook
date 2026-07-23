package auction

import (
	"errors"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// ErrAuctionClosed is returned when submitting to or cancelling in a session that
// has already uncrossed.
var ErrAuctionClosed = errors.New("auction: session already closed")

// AuctionSession runs a call auction: orders accumulate during a call phase, an
// indicative clearing price is published continuously, and the book uncrosses
// once at a scheduled close into a single uniform-price print. It is the
// mechanism behind the opening auction, the closing auction, and
// halt/circuit-breaker recovery — the cancel-only → auction → continuous path a
// venue restarts through (see docs/EXCHANGE-ARCHITECTURE.md).
//
// A randomized close (see RandomizedClose) is the structural defence against
// marking / banging the close: you cannot aim aggression at a print whose exact
// timing you do not control (Athena "Gravy", SEC 2014). Clearing at one uniform
// price also removes the intra-batch latency race and sandwich/JIT ordering games
// that continuous time-priority exposes (Budish–Cramton–Shim, QJE 2015).
//
// A session is single-writer, like the engine: drive it from one goroutine (or a
// Runner-style command queue). It is not a drop-in for the continuous Engine;
// pair them — run an auction to open or to recover from a halt, then feed the
// unfilled remainder into continuous matching.
type AuctionSession struct {
	symbol  string
	closeAt time.Time
	closed  bool
	seq     int64
	orders  []*types.Order         // insertion order (the allocation tie-break)
	byID    map[int64]*types.Order // live orders, for cancel + indicative
	result  BatchResult
}

// NewAuctionSession opens a call auction for symbol that uncrosses at closeAt (as
// measured by the clock the caller passes to Close). Use RandomizedClose to
// compute closeAt when you want the close time unpredictable.
func NewAuctionSession(symbol string, closeAt time.Time) *AuctionSession {
	return &AuctionSession{symbol: symbol, closeAt: closeAt, byID: make(map[int64]*types.Order)}
}

// Submit accumulates a limit order into the call phase. An order arriving without
// an id (ID==0) is assigned a session-local one so it can be cancelled; when the
// unfilled remainder later flows into the continuous engine it is reassigned an
// engine id there.
func (s *AuctionSession) Submit(o *types.Order) error {
	if s.closed {
		return ErrAuctionClosed
	}
	if o == nil {
		return types.ErrNilOrder
	}
	if o.ID == 0 {
		s.seq++
		o.ID = s.seq
	}
	s.orders = append(s.orders, o)
	s.byID[o.ID] = o
	return nil
}

// Cancel withdraws an order from the call phase before the uncross.
func (s *AuctionSession) Cancel(orderID int64) error {
	if s.closed {
		return ErrAuctionClosed
	}
	if _, ok := s.byID[orderID]; !ok {
		return types.ErrOrderNotFound
	}
	delete(s.byID, orderID)
	return nil
}

// live returns the still-active orders in insertion order.
func (s *AuctionSession) live() []*types.Order {
	out := make([]*types.Order, 0, len(s.byID))
	for _, o := range s.orders {
		if _, ok := s.byID[o.ID]; ok && o.RemainingQty > 0 {
			out = append(out, o)
		}
	}
	return out
}

// Indicative reports the clearing price and matched volume the auction would
// print if it uncrossed now — the "indicative equilibrium price" real venues
// publish during the call so participants can react to imbalance. It does not
// mutate any order.
func (s *AuctionSession) Indicative() Result {
	var bids, asks []Level
	for _, o := range s.live() {
		if o.Side == types.SideBuy {
			bids = append(bids, Level{Price: o.Price, Qty: o.RemainingQty})
		} else {
			asks = append(asks, Level{Price: o.Price, Qty: o.RemainingQty})
		}
	}
	return Uncross(bids, asks)
}

// Close uncrosses the auction if now has reached the scheduled close and it has
// not already cleared, filling every crossing order at the single clearing price
// and returning the result with ok=true. Before the close time (or after it has
// already cleared) it returns ok=false and does nothing. The unfilled remainder
// stays on the orders and can be forwarded to continuous matching.
func (s *AuctionSession) Close(now time.Time) (BatchResult, bool) {
	if s.closed || now.Before(s.closeAt) {
		return BatchResult{}, false
	}
	b := NewBatchAuction(s.symbol)
	for _, o := range s.live() {
		b.Add(o)
	}
	s.result = b.Cross()
	s.closed = true
	return s.result, true
}

// Closed reports whether the auction has uncrossed.
func (s *AuctionSession) Closed() bool { return s.closed }

// CloseAt returns the scheduled uncross time.
func (s *AuctionSession) CloseAt() time.Time { return s.closeAt }

// Unfilled returns the orders (or remainders) that did not fully trade in the
// uncross — the interest that would carry into continuous trading.
func (s *AuctionSession) Unfilled() []*types.Order {
	return s.live()
}

// RandomizedClose returns a close time jittered deterministically within
// [base, base+window) from seed. The exact close is unpredictable to participants
// (defeating marking/banging the close) yet fully reproducible on replay, so it
// preserves engine determinism. Seed from a pre-committed, ungameable source (a
// block hash, a VRF output, an engine sequence number) — never from a value a
// participant can grind. A non-positive window returns base unchanged.
func RandomizedClose(base time.Time, window time.Duration, seed uint64) time.Time {
	if window <= 0 {
		return base
	}
	// splitmix64 scramble: a well-distributed, deterministic jitter with no global
	// RNG (Math/rand is banned in the deterministic core).
	z := seed + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return base.Add(time.Duration(z % uint64(window)))
}
