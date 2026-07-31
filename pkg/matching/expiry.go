package matching

import (
	"container/heap"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Time-in-force expiry: DAY and GTD.
//
// # Why a heap rather than a sweep
//
// The obvious implementation walks the book each command looking for expired orders.
// That is O(book) per command on a path whose published p50 is 83ns, and it would put
// a full scan of a 200,000-order book in front of every cancel — the tail-latency
// numbers this repository publishes would stop being true the day it shipped.
//
// Instead the deadlines live in a min-heap. The per-command cost is one comparison
// against the earliest deadline, and work happens only when something has actually
// expired.
//
// # Why the deadline is resolved on intake
//
// A DAY order's deadline is computed once, when the engine accepts it, from the
// configured session close — and then stored on the order. It is not re-derived at
// expiry time. That is what makes replay exact: the log carries the order with its
// resolved deadline, so a recovery expires it at the same instant the live engine did,
// rather than at whatever the session close happens to be when the log is replayed.
//
// # What expiry is not
//
// It is not a cancel by the client, so it does not go through Cancel and is not
// subject to MinRestingTime. An anti-spoofing floor that could hold an order past its
// own stated lifetime would be inventing liquidity the client never offered. The
// Canceled event carries ErrOrderExpired so a consumer can tell the two apart.

// expiryItem is one pending deadline. The order id rather than the order, because the
// order may already be gone — filled or cancelled — by the time the deadline arrives,
// and holding a pointer would keep it alive for nothing.
type expiryItem struct {
	deadline time.Time
	orderID  int64
}

// expiryQueue is a min-heap of deadlines, earliest first.
type expiryQueue []expiryItem

func (q expiryQueue) Len() int           { return len(q) }
func (q expiryQueue) Less(i, j int) bool { return q[i].deadline.Before(q[j].deadline) }
func (q expiryQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *expiryQueue) Push(x any)        { *q = append(*q, x.(expiryItem)) }
func (q *expiryQueue) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	*q = old[:n-1]
	return it
}

// resolveExpiry works out an order's deadline and validates it, returning the reason
// it cannot be accepted if there is one.
//
// It is called on intake, before the order is matched, so a bad deadline is refused
// rather than discovered when the order fails to expire.
func (e *Engine) resolveExpiry(o *types.Order) error {
	switch o.TimeInForce {
	case types.TIFDay:
		if e.config.SessionClose == nil {
			return types.ErrNoSessionClose
		}
		close := e.config.SessionClose()
		if close.IsZero() {
			return types.ErrNoSessionClose
		}
		// A DAY order arriving after the close has no session left to rest in. Refused
		// rather than accepted and expired on the next command, which would be a
		// confusing accept-then-cancel for something that was never viable.
		if !close.After(e.commandNow()) {
			return types.ErrInvalidExpiry
		}
		o.ExpiresAt = close.UTC()
	case types.TIFGoodTillDate:
		if o.ExpiresAt.IsZero() {
			return types.ErrInvalidExpiry
		}
		if !o.ExpiresAt.After(e.commandNow()) {
			return types.ErrInvalidExpiry
		}
		o.ExpiresAt = o.ExpiresAt.UTC()
	}
	return nil
}

// trackExpiry registers a resting order's deadline. Only orders that actually rested
// are tracked: one that filled on arrival has no deadline to reach.
func (e *Engine) trackExpiry(o *types.Order) {
	if o == nil || !o.TimeInForce.Expiring() || o.ExpiresAt.IsZero() {
		return
	}
	if o.RemainingQty <= 0 || !o.IsActive() {
		return
	}
	heap.Push(&e.expiries, expiryItem{deadline: o.ExpiresAt, orderID: o.ID})
}

// expireDue removes every order whose deadline has passed, emitting a Canceled with
// ErrOrderExpired for each.
//
// Called at the top of every command, where the cost when nothing is due is a single
// comparison. Orders that have already left the book pop off the heap and are
// discarded: the heap is a schedule, not a second index, and it is allowed to hold
// stale entries because checking them is cheaper than keeping them in step.
func (e *Engine) expireDue() {
	if len(e.expiries) == 0 {
		return
	}
	now := e.commandNow()
	for len(e.expiries) > 0 && !e.expiries[0].deadline.After(now) {
		item := heap.Pop(&e.expiries).(expiryItem)
		o, ok := e.book.Get(item.orderID)
		if !ok || !o.IsActive() {
			continue // already filled or cancelled; the deadline is moot
		}
		// Deliberately not e.Cancel: expiry is the order's own stated lifetime
		// running out, not a client cancel, so MinRestingTime must not hold it past
		// the moment it was supposed to end.
		if err := o.Cancel(); err != nil {
			continue
		}
		o.UpdatedAt = now
		_, _ = e.book.Remove(o.ID)
		delete(e.icebergOrders, o.ID)
		e.cancelOCOCounterpart(o.ID)
		e.emitCancelReason(o, types.ErrOrderExpired)
	}
	e.flushPending()
}

// PendingExpiries reports how many deadlines are scheduled, including entries for
// orders that have already left the book. Exposed for tests and for an operator
// wanting to see that the schedule is not growing without bound.
//
// Single-writer: call from the engine's goroutine, or via the Runner.
func (e *Engine) PendingExpiries() int { return len(e.expiries) }
