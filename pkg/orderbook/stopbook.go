package orderbook

import (
	"cmp"
	"slices"
	"sync"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// StopBook holds resting stop orders off the main book until their trigger price
// is reached. It is driven by the matching engine, which feeds it the latest
// trade price after every match.
type StopBook struct {
	mu     sync.RWMutex
	symbol string
	orders map[int64]*types.StopOrder // keyed by underlying order id
}

// NewStopBook returns an empty stop book.
func NewStopBook(symbol string) *StopBook {
	return &StopBook{symbol: symbol, orders: make(map[int64]*types.StopOrder)}
}

// Add stores a pending stop order.
func (sb *StopBook) Add(s *types.StopOrder) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.orders[s.Order.ID] = s
}

// Remove deletes a pending stop by underlying order id.
func (sb *StopBook) Remove(id int64) bool {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if _, ok := sb.orders[id]; ok {
		delete(sb.orders, id)
		return true
	}
	return false
}

// Get returns a pending stop by underlying order id.
func (sb *StopBook) Get(id int64) (*types.StopOrder, bool) {
	sb.mu.RLock()
	defer sb.mu.RUnlock()
	s, ok := sb.orders[id]
	return s, ok
}

// Count returns the number of pending stops.
func (sb *StopBook) Count() int {
	sb.mu.RLock()
	defer sb.mu.RUnlock()
	return len(sb.orders)
}

// All returns every pending stop in deterministic (underlying id) order — for
// snapshotting the off-book stop state.
func (sb *StopBook) All() []*types.StopOrder {
	sb.mu.RLock()
	defer sb.mu.RUnlock()
	out := make([]*types.StopOrder, 0, len(sb.orders))
	for _, s := range sb.orders {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *types.StopOrder) int {
		return cmp.Compare(a.Order.ID, b.Order.ID)
	})
	return out
}

// CheckTriggers returns the stops that fire at marketPrice (ticks), in
// deterministic order (by underlying order id, which is the entry sequence),
// marking them triggered and removing them from the book. Map iteration order is
// not relied upon.
func (sb *StopBook) CheckTriggers(marketPrice int64) []*types.StopOrder {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	if len(sb.orders) == 0 {
		return nil
	}

	var fired []*types.StopOrder
	for _, s := range sb.orders {
		if s.ShouldTrigger(marketPrice) {
			fired = append(fired, s)
		}
	}
	// slices.SortFunc rather than sort.Slice: the latter goes through reflection
	// (reflectlite.Swapper) and costs ~8ns even when the slice is empty, which it
	// usually is. Sorting is still required — map iteration order is random and
	// firing order must be deterministic for replay.
	slices.SortFunc(fired, func(a, b *types.StopOrder) int {
		return cmp.Compare(a.Order.ID, b.Order.ID)
	})
	for _, s := range fired {
		s.Trigger()
		delete(sb.orders, s.Order.ID)
	}
	return fired
}
