package matching

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Shards routes order flow to one single-writer Runner per symbol. Because each
// symbol is a shared-nothing single writer (its own goroutine and book), distinct
// symbols scale across cores — the canonical way real venues scale a matching
// engine (shard by instrument, never lock one book across threads). It is safe for
// concurrent producers.
//
// Scaling is sublinear, and stops at the core count. BenchmarkShards_Scaling
// measures 2.24x at four books on four cores, and nothing further at six or eight:
// each shard is a PAIR of goroutines — a producer blocked on its reply and the
// matching goroutine draining the queue — so past the core count the machine goes
// into the handoff rather than into matching. Books beyond that buy queue headroom,
// not throughput.
type Shards struct {
	mu        sync.RWMutex
	runners   map[string]*Runner
	newConfig func(symbol string) Config
	newLog    func(symbol string) (CommandLog, error)
	manifest  *Manifest
	queueSize int
	err       error // the first shard-creation failure, sticky
}

// ShardsConfig configures a Shards router.
type ShardsConfig struct {
	// NewConfig returns the engine Config for a symbol the first time it is seen.
	// nil => DefaultConfig(symbol).
	NewConfig func(symbol string) Config
	// QueueSize is each shard Runner's command-queue capacity (0 => default).
	QueueSize int

	// NewLog returns the command log for a symbol's shard, called once when the
	// shard is created. nil leaves shards unjournalled.
	//
	// One log per shard, not one per venue: a shared log serialises every append
	// behind a single lock, which is the bottleneck sharding exists to remove,
	// arriving through the back door. Per-shard logs also make recovery, replay
	// admission and the replication drills the EXISTING single-symbol code paths
	// run N times — so a multi-symbol bug is a bug in a path that already has
	// tests. See docs/MULTI-SYMBOL.md §4.3.
	//
	// Its absence was the finding that turned "a routing layer you write" into a
	// spec: until this field existed there was no way to give a sharded venue a
	// journal, so a sharded venue could not survive a restart at all.
	NewLog func(symbol string) (CommandLog, error)

	// Manifest supplies each symbol's shard index, so order and trade ids are
	// unique across the venue. nil means every shard uses index 0 — correct only
	// for a single-symbol venue, and Shards will refuse a second symbol rather
	// than mint colliding ids.
	Manifest *Manifest
}

// NewShards builds a router. Shards are created lazily on first use per symbol.
func NewShards(cfg ShardsConfig) *Shards {
	if cfg.NewConfig == nil {
		cfg.NewConfig = DefaultConfig
	}
	return &Shards{
		runners:   make(map[string]*Runner),
		newConfig: cfg.NewConfig,
		newLog:    cfg.NewLog,
		manifest:  cfg.Manifest,
		queueSize: cfg.QueueSize,
	}
}

// ErrNoManifest reports a second symbol at a venue with no Manifest. Without one
// every shard would use index 0 and mint ids that collide with every other
// shard's, so this refuses instead: silently issuing duplicate ids is the failure
// that has no later symptom except two orders that cannot be told apart.
var ErrNoManifest = errors.New("matching: a multi-symbol venue needs ShardsConfig.Manifest")

// Err reports the first shard-creation failure, if any. Runner returns nil after
// one, and a venue that ignores this is running with a symbol it cannot journal.
func (s *Shards) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
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
	r, err := s.buildLocked(symbol)
	if err != nil {
		if s.err == nil {
			s.err = err
		}
		return nil
	}
	s.runners[symbol] = r
	return r
}

// buildLocked creates one shard: its index from the manifest, its config, its log.
// Callers hold the write lock.
func (s *Shards) buildLocked(symbol string) (*Runner, error) {
	cfg := RunnerConfig{Engine: s.newConfig(symbol), QueueSize: s.queueSize}

	switch {
	case s.manifest != nil:
		idx, err := s.manifest.IndexFor(symbol)
		if err != nil {
			return nil, fmt.Errorf("shard index for %s: %w", symbol, err)
		}
		cfg.Engine.ShardIndex = idx
	case len(s.runners) > 0:
		return nil, fmt.Errorf("%w: %s would be the second symbol", ErrNoManifest, symbol)
	}

	if s.newLog != nil {
		log, err := s.newLog(symbol)
		if err != nil {
			return nil, fmt.Errorf("command log for %s: %w", symbol, err)
		}
		cfg.Log = log
	}
	return NewRunner(cfg), nil
}

// Process routes an order to its symbol's Runner and returns the result.
//
// A shard that could not be created — no manifest for a second symbol, a log that
// would not open — rejects the order carrying Err(). Rejecting is the only safe
// answer: accepting into a venue with no journal, or with an id that collides with
// another symbol's, would trade now and be undiagnosable later.
func (s *Shards) Process(order *types.Order) *MatchResult {
	r := s.Runner(order.Symbol)
	if r == nil {
		return &MatchResult{Order: order, Status: types.OrderStatusRejected, RejectionReason: s.Err()}
	}
	return r.Process(order)
}

// TrySubmit routes a non-blocking submit to the order's shard (see Runner.TrySubmit).
func (s *Shards) TrySubmit(order *types.Order) (*MatchResult, error) {
	r := s.Runner(order.Symbol)
	if r == nil {
		return nil, s.Err()
	}
	return r.TrySubmit(order)
}

// Cancel routes a cancel to the given symbol's shard.
func (s *Shards) Cancel(symbol string, orderID int64, userID string) (*types.Order, error) {
	r := s.Runner(symbol)
	if r == nil {
		return nil, s.Err()
	}
	return r.Cancel(orderID, userID)
}

// RunnerFor routes by ORDER ID rather than by symbol, using the shard field the id
// carries (docs/MULTI-SYMBOL.md §4.1). It returns nil if no live shard holds that
// index — an id from a symbol this venue has not opened, or a fabricated one.
//
// This is the arithmetic the partitioned id space buys: a cancel names an order,
// and the order names its shard, with no lookup on the path that matters.
func (s *Shards) RunnerFor(orderID int64) *Runner {
	want, _ := SplitID(orderID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.runners {
		if r.ShardIndex() == want {
			return r
		}
	}
	return nil
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
