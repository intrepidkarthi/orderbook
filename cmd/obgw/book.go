package main

import (
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/intrepidkarthi/orderbook/pkg/gateway"
	"github.com/intrepidkarthi/orderbook/pkg/marketdata"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/observability"
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

	// walFailOnce keeps the halt-and-log for a failed sync to one line rather than
	// fifty a second, which is what made the old log-and-continue easy to ignore.
	walFailOnce sync.Once
	// diskStopped records that this book was put into cancel-only by the stop-water
	// mark, so crossing back and forth does not flap the venue's state on every
	// checkpoint tick.
	//
	// It is ONE-WAY. Nothing clears it when free space recovers, there is no admin
	// endpoint that resumes trading, and the only thing that returns the book to
	// normal is a restart. That is deliberate — a venue oscillating in and out of
	// cancel-only around a threshold is worse for participants than one that stays
	// out until a human has looked — and it is stated here and in docs/RUNBOOKS.md
	// "The disk filled up" because an operator who frees space will otherwise
	// reasonably expect trading to resume.
	diskStopped bool
	// lastRetainSkip is the previous cycle's RetentionResult.Skipped, so the reason a
	// cycle deleted less than the budget asked for is logged when it CHANGES rather
	// than every checkpoint tick. Touched only from checkpointLoop's goroutine.
	lastRetainSkip string

	// snapFailures counts checkpoints this book could not write. Nil when the book
	// is not configured to checkpoint at all. It is the "why" beside the age gauge's
	// "what": a stale age with a FLAT failure count is a checkpoint loop that is not
	// running, which is a different fault with a different fix from one that is
	// running and failing.
	snapFailures *observability.Counter
	// snapMTime is the modification time, in Unix nanoseconds, of the last snapshot
	// file this process actually looked at — zero when it has never seen one.
	//
	// It exists so /readyz can answer without touching a filesystem. The gauge on
	// /metrics stats the file every scrape and can afford to; readiness cannot, and
	// the reason is what readiness DOES. A snapshot on a mount that hangs would make
	// os.Stat block, the probe time out, and an orchestrator kill a book that is
	// holding positions — turning a snapshot-storage problem into a trading outage
	// and inviting exactly the restart a stale snapshot has been making expensive.
	// That is the outcome docs/LAG-AND-SHED.md §7 exists to avoid, arrived at through
	// the probe instead of through the status code.
	//
	// It caches the MTIME rather than the age, which is what makes it safe to let go
	// stale: age is computed from it at read time, so a checkpoint loop that dies
	// stops refreshing this and the age goes on climbing exactly as it should. A
	// cached age would freeze at its last value and report a healthy venue forever.
	snapMTime atomic.Int64
	// recoveredInNs is how long this book took to recover, set once during NewServer
	// and never again. NaN when there is no log to recover from.
	//
	// A gauge rather than a histogram because it is observed once per process: a
	// histogram of one observation reports a quantile that IS the observation, in a
	// bucket that rounds it, with a count of 1. It never moves, and it is not
	// supposed to — the way to read it is max_over_time across restarts, which is
	// where the signal actually is.
	recoveredInNs float64
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
