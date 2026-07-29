package matching

import (
	"errors"
	"sync"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// ErrQueueFull is returned by the non-blocking submit paths (TrySubmit,
// TrySubmitAsync) when the command queue has no free capacity — the caller should
// shed or reject the new order. The blocking paths (Process, Cancel, ...) wait for
// space instead, so cancels always get through: use TrySubmit for sheddable new
// liquidity and Cancel for the reliable cancel path under overload.
var ErrQueueFull = errors.New("matching: runner command queue is full")

// ErrShuttingDown is reported by every submit path once Close has been called.
// Producers get a refusal instead of a panic, so a server does not have to
// guarantee that all N connection goroutines have stopped before it shuts the
// matcher down — a guarantee no real ingress can actually make.
var ErrShuttingDown = errors.New("matching: runner is shutting down")

// shutdownResult is the refusal handed back to an order-bearing submit that
// arrived after the fence closed. It is a rejection rather than a nil result so
// callers can treat it exactly like any other rejected order.
func shutdownResult(order *types.Order) *MatchResult {
	if order != nil {
		order.Status = types.OrderStatusRejected
	}
	return &MatchResult{Order: order, Status: types.OrderStatusRejected, RejectionReason: ErrShuttingDown}
}

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
	done   chan struct{} // closed when the matching goroutine has exited
	quit   chan struct{} // the shutdown fence; closed by Close
	once   sync.Once     // Close is idempotent
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
		quit:   make(chan struct{}),
	}
	go r.loop()
	return r
}

// loop is the single matching goroutine: it owns the engine and applies commands
// in FIFO order until the shutdown fence closes, then drains whatever was already
// accepted so no producer is left waiting on a reply that never comes.
func (r *Runner) loop() {
	defer close(r.done)
	for {
		select {
		case cmd := <-r.queue:
			r.dispatch(cmd)
		case <-r.quit:
			for {
				select {
				case cmd := <-r.queue:
					r.dispatch(cmd)
				default:
					return
				}
			}
		}
	}
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
	case cmdSetMark:
		// cancelID reused as the int64 payload; a rejected step (ErrMarkStepTooLarge)
		// simply leaves the mark unchanged on this async path.
		_ = r.engine.SetMarkPrice(cmd.cancelID)
	}
	if cmd.reply != nil {
		cmd.reply <- rep
	}
}

// send enqueues cmd and blocks until the matching goroutine has applied it,
// reporting ok=false if the runner is shutting down. It never panics and never
// blocks forever: the fence is checked before enqueueing, while waiting for queue
// space, and while waiting for the reply.
func (r *Runner) send(cmd command) (cmdReply, bool) {
	reply := make(chan cmdReply, 1)
	cmd.reply = reply

	select {
	case <-r.quit:
		return cmdReply{}, false
	default:
	}

	select {
	case r.queue <- cmd:
	case <-r.quit:
		return cmdReply{}, false
	}

	select {
	case rep := <-reply:
		return rep, true
	case <-r.done:
		// The loop exited. It drains before doing so, so a reply may already be
		// waiting — select above picks randomly between two ready cases, and
		// losing an applied command's result here would be a silent lie.
		select {
		case rep := <-reply:
			return rep, true
		default:
			return cmdReply{}, false
		}
	}
}

// --- synchronous API (mirrors Engine; safe for concurrent producers) ---

// Process submits a limit/market order and waits for its result. After Close it
// returns a rejection carrying ErrShuttingDown rather than panicking.
func (r *Runner) Process(order *types.Order) *MatchResult {
	rep, ok := r.send(command{kind: cmdSubmit, order: order})
	if !ok {
		return shutdownResult(order)
	}
	return rep.match
}

// Cancel removes a resting order (or pending stop) belonging to userID.
func (r *Runner) Cancel(orderID int64, userID string) (*types.Order, error) {
	rep, ok := r.send(command{kind: cmdCancel, cancelID: orderID, userID: userID})
	if !ok {
		return nil, ErrShuttingDown
	}
	return rep.order, rep.err
}

// ProcessStop submits a stop / stop-limit order.
func (r *Runner) ProcessStop(s *types.StopOrder) *MatchResult {
	rep, ok := r.send(command{kind: cmdStop, stop: s})
	if !ok {
		return shutdownResult(s.Order)
	}
	return rep.match
}

// ProcessOCO submits a one-cancels-other pair.
func (r *Runner) ProcessOCO(o *types.OCOOrder) *MatchResult {
	rep, ok := r.send(command{kind: cmdOCO, oco: o})
	if !ok {
		return shutdownResult(o.Primary)
	}
	return rep.match
}

// ProcessIceberg submits an iceberg order.
func (r *Runner) ProcessIceberg(ib *types.IcebergOrder) *MatchResult {
	rep, ok := r.send(command{kind: cmdIceberg, iceberg: ib})
	if !ok {
		return shutdownResult(ib.Order)
	}
	return rep.match
}

// ProcessPegged submits a pegged order.
func (r *Runner) ProcessPegged(p *types.PeggedOrder) *MatchResult {
	rep, ok := r.send(command{kind: cmdPegged, pegged: p})
	if !ok {
		return shutdownResult(p.Order)
	}
	return rep.match
}

// ProcessTrailingStop submits a trailing stop.
func (r *Runner) ProcessTrailingStop(ts *types.TrailingStop) *MatchResult {
	rep, ok := r.send(command{kind: cmdTrailing, trailing: ts})
	if !ok {
		return shutdownResult(ts.Order)
	}
	return rep.match
}

// Halt suspends trading until Resume.
func (r *Runner) Halt() { r.send(command{kind: cmdHalt}) }

// SetCancelOnly puts the engine in cancel-only mode (cancels accepted, new
// liquidity rejected).
func (r *Runner) SetCancelOnly() { r.send(command{kind: cmdCancelOnly}) }

// Resume returns the engine to normal trading.
func (r *Runner) Resume() { r.send(command{kind: cmdResume}) }

// SetMarkPrice sets the external mark/index reference (ticks) the price band uses.
func (r *Runner) SetMarkPrice(price int64) { r.send(command{kind: cmdSetMark, cancelID: price}) }

// --- asynchronous submit ---

// SubmitAsync enqueues an order without blocking the producer on matching and
// returns a channel that receives the *MatchResult once it is applied. Use it to
// pipeline submissions; the returned channel is buffered so it never blocks the
// matching goroutine.
func (r *Runner) SubmitAsync(order *types.Order) <-chan *MatchResult {
	out := make(chan *MatchResult, 1)
	reply := make(chan cmdReply, 1)
	select {
	case r.queue <- command{kind: cmdSubmit, order: order, reply: reply}:
	case <-r.quit:
		out <- shutdownResult(order)
		return out
	}
	go func() {
		select {
		case rep := <-reply:
			out <- rep.match
		case <-r.done:
			select {
			case rep := <-reply:
				out <- rep.match
			default:
				out <- shutdownResult(order)
			}
		}
	}()
	return out
}

// TrySubmit submits an order without waiting for queue space: if the command
// queue is full it returns ErrQueueFull immediately (shed the order) rather than
// blocking. On success it waits for and returns the result. This is the
// bounded-backpressure path for new liquidity under overload; Cancel keeps its
// blocking path so cancels are never shed.
func (r *Runner) TrySubmit(order *types.Order) (*MatchResult, error) {
	select {
	case <-r.quit:
		return nil, ErrShuttingDown
	default:
	}
	reply := make(chan cmdReply, 1)
	select {
	case r.queue <- command{kind: cmdSubmit, order: order, reply: reply}:
		select {
		case rep := <-reply:
			return rep.match, nil
		case <-r.done:
			select {
			case rep := <-reply:
				return rep.match, nil
			default:
				return nil, ErrShuttingDown
			}
		}
	default:
		return nil, ErrQueueFull
	}
}

// TrySubmitAsync is the non-blocking async submit: it enqueues without waiting and
// returns a channel for the result, or ErrQueueFull if the queue is full.
func (r *Runner) TrySubmitAsync(order *types.Order) (<-chan *MatchResult, error) {
	select {
	case <-r.quit:
		return nil, ErrShuttingDown
	default:
	}
	reply := make(chan cmdReply, 1)
	select {
	case r.queue <- command{kind: cmdSubmit, order: order, reply: reply}:
		out := make(chan *MatchResult, 1)
		go func() {
			select {
			case rep := <-reply:
				out <- rep.match
			case <-r.done:
				select {
				case rep := <-reply:
					out <- rep.match
				default:
					out <- shutdownResult(order)
				}
			}
		}()
		return out, nil
	default:
		return nil, ErrQueueFull
	}
}

// QueueLen reports the number of commands currently buffered in the queue — a
// backpressure gauge to export as a metric.
func (r *Runner) QueueLen() int { return len(r.queue) }

// QueueCap reports the queue's capacity.
func (r *Runner) QueueCap() int { return cap(r.queue) }

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

// Close stops the matching goroutine after the commands already accepted into the
// queue have been applied, and waits for it to exit. It is idempotent and safe to
// call while producers are still running: submissions racing with it are refused
// with ErrShuttingDown rather than panicking.
//
// It closes a separate fence rather than the command queue itself. Closing the
// queue from the consumer side panicked any producer mid-send, which made correct
// shutdown require proving that every producer had already stopped — not a
// property a server with a goroutine per connection can establish.
func (r *Runner) Close() {
	r.once.Do(func() { close(r.quit) })
	<-r.done
}
