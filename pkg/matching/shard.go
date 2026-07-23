package matching

import (
	"sort"
	"sync"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Shards routes order flow to one single-writer Runner per symbol. Because each
// symbol is a shared-nothing single writer (its own goroutine and book), distinct
// symbols scale linearly across cores — the canonical way real venues scale a
// matching engine (shard by instrument, never lock one book across threads). It is
// safe for concurrent producers.
type Shards struct {
	mu        sync.RWMutex
	runners   map[string]*Runner
	newConfig func(symbol string) Config
	queueSize int
}

// ShardsConfig configures a Shards router.
type ShardsConfig struct {
	// NewConfig returns the engine Config for a symbol the first time it is seen.
	// nil => DefaultConfig(symbol).
	NewConfig func(symbol string) Config
	// QueueSize is each shard Runner's command-queue capacity (0 => default).
	QueueSize int
}

// NewShards builds a router. Shards are created lazily on first use per symbol.
func NewShards(cfg ShardsConfig) *Shards {
	if cfg.NewConfig == nil {
		cfg.NewConfig = DefaultConfig
	}
	return &Shards{
		runners:   make(map[string]*Runner),
		newConfig: cfg.NewConfig,
		queueSize: cfg.QueueSize,
	}
}

// Runner returns the Runner owning symbol, creating it (and starting its matching
// goroutine) on first use.
func (s *Shards) Runner(symbol string) *Runner {
	s.mu.RLock()
	r, ok := s.runners[symbol]
	s.mu.RUnlock()
	if ok {
		return r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok = s.runners[symbol]; ok { // re-check under the write lock
		return r
	}
	r = NewRunner(RunnerConfig{Engine: s.newConfig(symbol), QueueSize: s.queueSize})
	s.runners[symbol] = r
	return r
}

// Process routes an order to its symbol's Runner and returns the result.
func (s *Shards) Process(order *types.Order) *MatchResult {
	return s.Runner(order.Symbol).Process(order)
}

// TrySubmit routes a non-blocking submit to the order's shard (see Runner.TrySubmit).
func (s *Shards) TrySubmit(order *types.Order) (*MatchResult, error) {
	return s.Runner(order.Symbol).TrySubmit(order)
}

// Cancel routes a cancel to the given symbol's shard.
func (s *Shards) Cancel(symbol string, orderID int64, userID string) (*types.Order, error) {
	return s.Runner(symbol).Cancel(orderID, userID)
}

// Symbols returns the symbols that currently have a live shard, sorted.
func (s *Shards) Symbols() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.runners))
	for sym := range s.runners {
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}

// Len returns the number of live shards.
func (s *Shards) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.runners)
}

// Close stops every shard Runner (draining its queue) and clears the router.
func (s *Shards) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runners {
		r.Close()
	}
	s.runners = make(map[string]*Runner)
}
