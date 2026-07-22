package matching

import (
	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Runner drives an Engine from a single matching goroutine, fed by an MPSC
// command queue. It is the concurrency front for the engine: many producers may
// submit orders at once, but matching itself runs lock-free on the owning
// goroutine, applying commands in the order they were enqueued. This is the
// single-writer (LMAX-style) model — the only synchronisation on the submit path
// is the queue hand-off, not a lock around the book.
//
// Determinism is preserved per command sequence: the same ordered stream of
// commands produces byte-identical trades, so a recorded command log replays
// exactly (see package marketdata). Read accessors delegate to the engine's book
// and stop book, which carry their own locks, so market-data reads are safe to
// call concurrently without going through the queue.
//
// The queue is backed by a Go channel first; the command/dispatch split leaves
// room to swap in a lock-free ring buffer later without touching callers.
type Runner struct {
	engine *Engine
	queue  chan command
	done   chan struct{}
}

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	Engine    Config
	QueueSize int // command buffer capacity; 0 => 1024
}

// NewRunner builds a Runner over a fresh Engine and starts its matching
// goroutine. Call Close to stop it.
func NewRunner(cfg RunnerConfig) *Runner {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	r := &Runner{
		engine: NewEngine(cfg.Engine),
		queue:  make(chan command, cfg.QueueSize),
		done:   make(chan struct{}),
	}
	go r.loop()
	return r
}

// loop is the single matching goroutine: it owns the engine and applies commands
// in FIFO order until the queue is closed.
func (r *Runner) loop() {
	for cmd := range r.queue {
		r.dispatch(cmd)
	}
	close(r.done)
}

func (r *Runner) dispatch(cmd command) {
	var rep cmdReply
	switch cmd.kind {
	case cmdSubmit:
		rep.match = r.engine.Process(cmd.order)
	case cmdCancel:
		rep.order, rep.err = r.engine.Cancel(cmd.cancelID, cmd.userID)
	case cmdStop:
		rep.match = r.engine.ProcessStop(cmd.stop)
	case cmdOCO:
		rep.match = r.engine.ProcessOCO(cmd.oco)
	case cmdIceberg:
		rep.match = r.engine.ProcessIceberg(cmd.iceberg)
	case cmdPegged:
		rep.match = r.engine.ProcessPegged(cmd.pegged)
	case cmdTrailing:
		rep.match = r.engine.ProcessTrailingStop(cmd.trailing)
	case cmdHalt:
		r.engine.Halt()
	case cmdResume:
		r.engine.Resume()
	case cmdCancelOnly:
		r.engine.SetCancelOnly()
	}
	if cmd.reply != nil {
		cmd.reply <- rep
	}
}

// send enqueues cmd and blocks until the matching goroutine has applied it.
func (r *Runner) send(cmd command) cmdReply {
	reply := make(chan cmdReply, 1)
	cmd.reply = reply
	r.queue <- cmd
	return <-reply
}

// --- synchronous API (mirrors Engine; safe for concurrent producers) ---

// Process submits a limit/market order and waits for its result.
func (r *Runner) Process(order *types.Order) *MatchResult {
	return r.send(command{kind: cmdSubmit, order: order}).match
}

// Cancel removes a resting order (or pending stop) belonging to userID.
func (r *Runner) Cancel(orderID int64, userID string) (*types.Order, error) {
	rep := r.send(command{kind: cmdCancel, cancelID: orderID, userID: userID})
	return rep.order, rep.err
}

// ProcessStop submits a stop / stop-limit order.
func (r *Runner) ProcessStop(s *types.StopOrder) *MatchResult {
	return r.send(command{kind: cmdStop, stop: s}).match
}

// ProcessOCO submits a one-cancels-other pair.
func (r *Runner) ProcessOCO(o *types.OCOOrder) *MatchResult {
	return r.send(command{kind: cmdOCO, oco: o}).match
}

// ProcessIceberg submits an iceberg order.
func (r *Runner) ProcessIceberg(ib *types.IcebergOrder) *MatchResult {
	return r.send(command{kind: cmdIceberg, iceberg: ib}).match
}

// ProcessPegged submits a pegged order.
func (r *Runner) ProcessPegged(p *types.PeggedOrder) *MatchResult {
	return r.send(command{kind: cmdPegged, pegged: p}).match
}

// ProcessTrailingStop submits a trailing stop.
func (r *Runner) ProcessTrailingStop(ts *types.TrailingStop) *MatchResult {
	return r.send(command{kind: cmdTrailing, trailing: ts}).match
}

// Halt suspends trading until Resume.
func (r *Runner) Halt() { r.send(command{kind: cmdHalt}) }

// SetCancelOnly puts the engine in cancel-only mode (cancels accepted, new
// liquidity rejected).
func (r *Runner) SetCancelOnly() { r.send(command{kind: cmdCancelOnly}) }

// Resume returns the engine to normal trading.
func (r *Runner) Resume() { r.send(command{kind: cmdResume}) }

// --- asynchronous submit ---

// SubmitAsync enqueues an order without blocking the producer on matching and
// returns a channel that receives the *MatchResult once it is applied. Use it to
// pipeline submissions; the returned channel is buffered so it never blocks the
// matching goroutine.
func (r *Runner) SubmitAsync(order *types.Order) <-chan *MatchResult {
	out := make(chan *MatchResult, 1)
	reply := make(chan cmdReply, 1)
	r.queue <- command{kind: cmdSubmit, order: order, reply: reply}
	go func() { out <- (<-reply).match }()
	return out
}

// --- read accessors (delegate to the engine's independently-locked books) ---

// BestBid returns the best bid price (ticks) and aggregate quantity (lots).
func (r *Runner) BestBid() (price, qty int64, ok bool) { return r.engine.BestBid() }

// BestAsk returns the best ask price (ticks) and aggregate quantity (lots).
func (r *Runner) BestAsk() (price, qty int64, ok bool) { return r.engine.BestAsk() }

// Spread returns best ask − best bid (ticks).
func (r *Runner) Spread() (int64, bool) { return r.engine.Spread() }

// MidPrice returns (best bid + best ask) / 2 (ticks, floored).
func (r *Runner) MidPrice() (int64, bool) { return r.engine.MidPrice() }

// LastTradePrice returns the most recent execution price (ticks).
func (r *Runner) LastTradePrice() int64 { return r.engine.LastTradePrice() }

// OrderCount returns the number of resting orders.
func (r *Runner) OrderCount() int { return r.engine.OrderCount() }

// PendingStopCount returns the number of resting stop orders.
func (r *Runner) PendingStopCount() int { return r.engine.PendingStopCount() }

// Snapshot returns a top-of-book view to the given depth.
func (r *Runner) Snapshot(depth int) *orderbook.Snapshot { return r.engine.Snapshot(depth) }

// Close stops the matching goroutine after all queued commands have been
// applied. It is safe to call once; further submissions will panic on the closed
// queue, so stop producing before calling Close.
func (r *Runner) Close() {
	close(r.queue)
	<-r.done
}
