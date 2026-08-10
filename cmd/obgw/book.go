package main

import (
	"path/filepath"
	"sync"

	"github.com/intrepidkarthi/orderbook/pkg/gateway"
	"github.com/intrepidkarthi/orderbook/pkg/marketdata"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// A symbolBook is one instrument: its own matching goroutine, its own command log, its
// own market-data feed and its own rate gate. Nothing is shared between books
// except the id space, which is partitioned rather than centralised so that
// sharing it costs no coordination (docs/MULTI-SYMBOL.md §4.1).
//
// This is the whole shape of multi-symbol here. There is no venue-wide sequence
// and no venue-wide consistent instant, because buying either needs a
// serialisation point every command passes through — the bottleneck sharding
// exists to remove.
type symbolBook struct {
	symbol     string
	shardIndex int
	runner     *matching.Runner
	feed       *marketdata.Feed
	wal        *wal.Writer
	gate       *gateway.Gateway
}

// bookSet is the venue's set of instruments, with the two lookups the gateway needs:
// by symbol, for an order that names one, and by shard index, for a command that
// names an order id instead.
type bookSet struct {
	mu      sync.RWMutex
	bySym   map[string]*symbolBook
	byShard map[int]*symbolBook
	order   []string // insertion order; the first is the default for single-symbol callers
}

func newBookSet() *bookSet {
	return &bookSet{bySym: map[string]*symbolBook{}, byShard: map[int]*symbolBook{}}
}

func (b *bookSet) add(bk *symbolBook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bySym[bk.symbol] = bk
	b.byShard[bk.shardIndex] = bk
	b.order = append(b.order, bk.symbol)
}

// The lookups below tolerate a nil receiver. A Server assembled field-by-field —
// which the drills do, to present a wedged matcher without wedging a real venue —
// has no book set, and a venue-wide accessor that panicked on one would make the
// gateway harder to test than the thing it is testing.
func (b *bookSet) bySymbol(symbol string) *symbolBook {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.bySym[symbol]
}

// byOrderID routes a command that names an engine order id, using the shard field
// the id carries. This is the arithmetic the partitioned id space buys: no lookup
// table on the path, and no way for a command to reach the wrong book.
func (b *bookSet) byOrderID(id int64) *symbolBook {
	shard, _ := matching.SplitID(id)
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.byShard[shard]
}

// all returns every book in a stable order, so venue-wide operations — a mass
// cancel, an open-orders query, shutdown — visit them the same way every time.
func (b *bookSet) all() []*symbolBook {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*symbolBook, 0, len(b.order))
	for _, sym := range b.order {
		out = append(out, b.bySym[sym])
	}
	return out
}

// first is the book a single-symbol venue has, and the one the accessors that
// predate multi-symbol resolve to.
func (b *bookSet) first() *symbolBook {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.order) == 0 {
		return nil
	}
	return b.bySym[b.order[0]]
}

func (b *bookSet) symbols() []string {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, len(b.order))
	copy(out, b.order)
	return out
}

// paths resolves where one symbol's log and snapshot live.
//
// A single-symbol venue keeps WALPath and SnapshotPath exactly as they were, so
// every existing deployment, runbook and test is untouched by this feature
// existing. A multi-symbol venue uses DataDir, because deriving N paths from one
// by string munging is the kind of cleverness that turns into a support question.
func (cfg *Config) paths(symbol string) (walPath, snapPath string) {
	if len(cfg.Symbols) <= 1 {
		return cfg.WALPath, cfg.SnapshotPath
	}
	if cfg.DataDir == "" {
		return "", ""
	}
	return filepath.Join(cfg.DataDir, symbol+".wal"), filepath.Join(cfg.DataDir, symbol+".snap")
}

// manifestPath is where the symbol-to-shard mapping lives. Losing it makes every
// id the venue ever issued ambiguous, so it sits beside the logs it explains.
func (cfg *Config) manifestPath() string {
	if cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(cfg.DataDir, "venue.json")
}
