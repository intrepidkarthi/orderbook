package matching

import (
	"errors"
	"fmt"
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

// ErrNoResolver reports a By-suffixed enqueue called without a resolver. It is a
// caller bug, refused at the door rather than turned into a command that names order
// zero — which would be journalled, applied, and correctly refused, leaving a real log
// record for a question nobody asked.
var ErrNoResolver = errors.New("matching: no resolver supplied")

// ErrResolverPanicked reports that a caller-supplied resolver panicked. The command
// was not journalled and not applied; the book is untouched. See safeResolve for why
// this is contained rather than fatal.
var ErrResolverPanicked = errors.New("matching: resolver panicked")

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
	log    CommandLog
	// lastApplied is the log sequence of the last command actually applied. It is
	// only ever touched by the matching goroutine, which is what makes a
	// checkpoint's WALSeq trustworthy: appended-but-unapplied commands are on disk
	// and must still be replayed.
	lastApplied int64
}

// CommandLog is the write-ahead seam: the Runner appends each mutating command
// here and only then applies it, so nothing the engine has done is missing from
// the log. pkg/wal.Writer satisfies it.
//
// Append is called on the matching goroutine, so a slow log slows matching. Use a
// buffered writer and batch the fsync (group commit) rather than syncing per
// command.
//
// Every method here corresponds to a command that mutates ENGINE STATE, which is
// not the same as mutating the book and the difference cost four releases. A
// mutating command missing from this interface is not "not yet logged", it is
// state the log cannot reproduce — which is how Reduce came to be applied but not
// recorded, so a restart brought the order back at its original size, and how an
// operator's halt came to be applied but not recorded, so a restart brought the
// venue back open. See docs/TRADE-BUST.md §3.5 for the second one.
type CommandLog interface {
	AppendSubmit(o *types.Order) (int64, error)
	AppendCancel(orderID int64, userID string) (int64, error)
	AppendReduce(orderID, newQty int64, userID string) (int64, error)
	AppendReplace(orderID int64, userID string, replacement *types.Order) (int64, error)
	AppendCancelAll(userID string) (int64, error)
	AppendStop(s *types.StopOrder) (int64, error)
	AppendOCO(o *types.OCOOrder) (int64, error)
	AppendIceberg(ib *types.IcebergOrder) (int64, error)
	AppendPegged(p *types.PeggedOrder) (int64, error)
	AppendTrailing(ts *types.TrailingStop) (int64, error)

	// Control commands. They touch no order, and that is exactly why they were
	// left out: "carries no book state" was read as "needs no record", when the
	// trading state and the mark price are two of the few things a venue is
	// actually judged on being right about after a restart.
	AppendHalt() (int64, error)
	AppendResume() (int64, error)
	AppendCancelOnly() (int64, error)
	AppendSetMark(price int64) (int64, error)
	AppendBust(tradeID int64, reason string) (int64, error)
}

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	Engine    Config
	QueueSize int // command buffer capacity; 0 => 1024
	// Log, if set, receives every mutating command before it is applied.
	//
	// Beware the typed-nil trap: assigning a nil *T to this interface field gives
	// a non-nil interface holding a nil pointer, which passes a `!= nil` check and
	// then panics on first use. Leave the field unset rather than assigning a
	// possibly-nil concrete value.
	Log CommandLog
	// Replaying starts the engine in replay mode and suppresses logging, so a
	// bootstrap replay does not re-append the log it is reading.
	Replaying bool
}

// NewRunner builds a Runner over a fresh Engine and starts its matching
// goroutine. Call Close to stop it.
func NewRunner(cfg RunnerConfig) *Runner {
	return NewRunnerFor(nil, cfg)
}

// NewRunnerFor builds a Runner over an EXISTING engine, which is what recovery
// needs: an engine rebuilt from a snapshot plus log tail already holds the book,
// and handing NewRunner a bare Config would silently discard it and start empty.
// A nil engine is equivalent to NewRunner.
//
// The engine must not be driven from anywhere else afterwards — the Runner's
// goroutine now owns it.
func NewRunnerFor(eng *Engine, cfg RunnerConfig) *Runner {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if eng == nil {
		eng = NewEngine(cfg.Engine)
	}
	r := &Runner{
		engine: eng,
		queue:  make(chan command, cfg.QueueSize),
		done:   make(chan struct{}),
		quit:   make(chan struct{}),
		log:    cfg.Log,
	}
	if cfg.Replaying {
		r.engine.SetReplaying(true)
		r.log = nil // never re-append the log being replayed
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
	// Resolution first, and before the log: the journal must record the engine order
	// id, not the client's name for it, or a replay would have to reconstruct a
	// mapping that only ever existed in the gateway.
	if cmd.resolve != nil {
		id, ok, panicked := safeResolve(cmd.resolve)
		switch {
		case panicked != nil:
			rep.err = fmt.Errorf("%w: %v", ErrResolverPanicked, panicked)
		case !ok:
			rep.err = types.ErrOrderNotFound
		}
		if rep.err != nil {
			if cmd.reply != nil {
				cmd.reply <- rep
			}
			return
		}
		cmd.cancelID = id
	}
	r.logCommand(cmd)
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
	case cmdBust:
		rep.err = r.engine.Bust(cmd.cancelID, cmd.bustReason)
	case cmdReduce:
		rep.order, rep.err = r.engine.Reduce(cmd.cancelID, cmd.reduceQty, cmd.userID)
	case cmdReplace:
		rep.match, rep.err = r.engine.Replace(cmd.cancelID, cmd.userID, cmd.replace)
	case cmdCancelAll:
		rep.orders = r.engine.CancelAllForUser(cmd.userID)
	case cmdOpenOrders:
		rep.orders = r.engine.OpenOrdersFor(cmd.userID)
	case cmdTrailingCount:
		rep.count = r.engine.TrailingStopCount()
	case cmdExpireDue:
		r.engine.ExpireDue()
	case cmdIndicative:
		rep.indicative = r.engine.IndicativeAuction()
	case cmdSetPhase:
		// Copied to pointers because the engine's slice is a value buffer it reuses;
		// handing it out would let a caller read trades the next command overwrites.
		for _, t := range r.engine.SetPhase(cmd.phase) {
			tc := t
			rep.trades = append(rep.trades, &tc)
		}
	case cmdCheckpoint:
		snap := r.engine.TakeSnapshot()
		snap.WALSeq = r.lastApplied
		cmd.snapOut <- snap
	}
	if cmd.reply != nil {
		cmd.reply <- rep
	}
}

// safeResolve calls a caller-supplied resolver without letting it stop the venue.
//
// This is the only place the runner recovers from a panic, and the exception is
// deliberate. Everywhere else, a panic on the matching goroutine means an engine
// invariant is broken and dying is the correct response — continuing would trade
// against a book whose state nobody can vouch for.
//
// A resolver is different in kind. It is caller code, it answers a question the engine
// did not ask, and it runs BEFORE the command is journalled or applied — so a panic
// inside it says nothing about the book, and the engine's state is exactly what it was.
// The blast radius of not recovering here is the whole market, for a bug in a gateway's
// lookup table. Treating it as "could not resolve" is both safe and the smaller
// failure.
//
// It is not swallowed: the async paths carry ErrResolverPanicked back to the caller.
func safeResolve(fn func() (int64, bool)) (id int64, ok bool, panicked any) {
	defer func() {
		if p := recover(); p != nil {
			id, ok, panicked = 0, false, p
		}
	}()
	id, ok = fn()
	return id, ok, nil
}

// logCommand writes a mutating command to the log before it is applied, and
// records the sequence so a checkpoint can name the log position it is consistent
// with. A log failure halts the engine rather than letting it run ahead of its
// own journal — silently diverging from the record is worse than stopping.
func (r *Runner) logCommand(cmd command) {
	if r.log == nil {
		return
	}
	var (
		seq int64
		err error
	)
	switch cmd.kind {
	case cmdSubmit:
		seq, err = r.log.AppendSubmit(cmd.order)
	case cmdCancel:
		seq, err = r.log.AppendCancel(cmd.cancelID, cmd.userID)
	case cmdReduce:
		seq, err = r.log.AppendReduce(cmd.cancelID, cmd.reduceQty, cmd.userID)
	case cmdReplace:
		seq, err = r.log.AppendReplace(cmd.cancelID, cmd.userID, cmd.replace)
	case cmdCancelAll:
		seq, err = r.log.AppendCancelAll(cmd.userID)
	case cmdStop:
		seq, err = r.log.AppendStop(cmd.stop)
	case cmdOCO:
		seq, err = r.log.AppendOCO(cmd.oco)
	case cmdIceberg:
		seq, err = r.log.AppendIceberg(cmd.iceberg)
	case cmdPegged:
		seq, err = r.log.AppendPegged(cmd.pegged)
	case cmdTrailing:
		seq, err = r.log.AppendTrailing(cmd.trailing)
	case cmdHalt:
		seq, err = r.log.AppendHalt()
	case cmdResume:
		seq, err = r.log.AppendResume()
	case cmdCancelOnly:
		seq, err = r.log.AppendCancelOnly()
	case cmdSetMark:
		seq, err = r.log.AppendSetMark(cmd.cancelID)
	case cmdBust:
		seq, err = r.log.AppendBust(cmd.cancelID, cmd.bustReason)
	default:
		// Genuinely read-only commands: queries, the checkpoint, and ExpireDue,
		// whose effects are cancels the engine derives from a clock replay cannot
		// rewind. Nothing here changes state a recovery must reproduce.
		return
	}
	if err != nil {
		r.engine.Halt()
		return
	}
	r.lastApplied = seq
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

// ShardIndex reports the id-space partition this runner's engine was built with.
func (r *Runner) ShardIndex() int { return r.engine.config.ShardIndex }

// SetMarkPrice sets the external mark/index reference (ticks) the price band uses.
func (r *Runner) SetMarkPrice(price int64) { r.send(command{kind: cmdSetMark, cancelID: price}) }

// Bust annuls a published trade and journals the annulment. It reports the
// engine's verdict — ErrUnknownTrade, ErrAlreadyBusted, or nil — because an
// operator busting a trade needs to know it landed, which is the whole difference
// between this and the fire-and-forget control commands above.
//
// It does not rewind the book. See Engine.Bust and docs/TRADE-BUST.md.
func (r *Runner) Bust(tradeID int64, reason string) error {
	rep, ok := r.send(command{kind: cmdBust, cancelID: tradeID, bustReason: reason})
	if !ok {
		return ErrShuttingDown
	}
	return rep.err
}

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

// Reduce shrinks a resting order in place on the matching goroutine, keeping its
// queue position. See Engine.Reduce.
func (r *Runner) Reduce(orderID, newQty int64, userID string) (*types.Order, error) {
	rep, ok := r.send(command{kind: cmdReduce, cancelID: orderID, reduceQty: newQty, userID: userID})
	if !ok {
		return nil, ErrShuttingDown
	}
	return rep.order, rep.err
}

// OpenOrdersFor returns the account's resting orders, read on the matching
// goroutine so the answer is a coherent instant rather than a smear across
// concurrent updates.
func (r *Runner) OpenOrdersFor(userID string) ([]*types.Order, error) {
	rep, ok := r.send(command{kind: cmdOpenOrders, userID: userID})
	if !ok {
		return nil, ErrShuttingDown
	}
	return rep.orders, nil
}

// CancelAllForUser pulls every resting order, pending stop and trailing stop
// belonging to userID, on the matching goroutine and atomically with respect to
// concurrent submits. This is the operator kill switch; it ignores
// MinRestingTime.
func (r *Runner) CancelAllForUser(userID string) ([]*types.Order, error) {
	rep, ok := r.send(command{kind: cmdCancelAll, userID: userID})
	if !ok {
		return nil, ErrShuttingDown
	}
	return rep.orders, nil
}

// Checkpoint takes a snapshot on the matching goroutine, stamped with the log
// sequence of the last command actually applied. Taking it from outside the
// writer would capture a book mid-command and pair it with a log position that
// does not correspond to it.
func (r *Runner) Checkpoint() (*EngineSnapshot, error) {
	out := make(chan *EngineSnapshot, 1)
	cmd := command{kind: cmdCheckpoint, snapOut: out}

	select {
	case <-r.quit:
		return nil, ErrShuttingDown
	default:
	}
	select {
	case r.queue <- cmd:
	case <-r.quit:
		return nil, ErrShuttingDown
	}
	select {
	case snap := <-out:
		return snap, nil
	case <-r.done:
		select {
		case snap := <-out:
			return snap, nil
		default:
			return nil, ErrShuttingDown
		}
	}
}

// SetReplaying toggles replay mode on the underlying engine. Bootstrap only.
func (r *Runner) SetReplaying(v bool) { r.engine.SetReplaying(v) }

// TryEnqueue submits an order without waiting for it to be applied and without
// waiting for queue space: it returns ErrQueueFull if the queue is full, or
// ErrShuttingDown after Close. Nothing is returned about the outcome.
//
// This is the path a network ingress should use. The synchronous methods hand
// back the engine-owned *types.Order, which the matching goroutine keeps mutating
// — so a connection goroutine that reads any field of it is racing the matcher.
// Fire-and-forget removes that entire class of bug by never exposing the pointer;
// the outcome arrives on the event stream instead, which is the same path every
// other consumer already uses.
func (r *Runner) TryEnqueue(order *types.Order) error {
	return r.tryEnqueue(command{kind: cmdSubmit, order: order})
}

// TryEnqueueCancel is the cancel counterpart of TryEnqueue.
func (r *Runner) TryEnqueueCancel(orderID int64, userID string) error {
	return r.tryEnqueue(command{kind: cmdCancel, cancelID: orderID, userID: userID})
}

// TryEnqueueCancelBy is TryEnqueueCancel for a caller that does not yet know the
// engine's id for the order — a gateway holding only the client's own identifier.
//
// resolve runs on the matching goroutine when the command reaches the front of the
// queue, so every command enqueued before it has already been applied. That is the
// only point at which "does this client's order exist?" has a stable answer: ask it
// on the caller's goroutine instead and a cancel can be refused for an order whose
// Enter is still two places behind it in the same queue, which no client retries
// because it was told the order does not exist.
//
// Two obligations on resolve, both because of where it runs: it must not block, and
// if it wants to tell the client that the order is genuinely unknown it must do so
// without waiting — the reference gateway's reject is a non-blocking send onto a
// bounded per-connection queue, which is the shape to copy.
func (r *Runner) TryEnqueueCancelBy(userID string, resolve func() (int64, bool)) error {
	if resolve == nil {
		// Refused rather than treated as "cancel order 0". A nil resolver is a caller
		// bug, and the alternative is a command that names nothing, is journalled, and
		// is refused by the engine — a real log record for a question nobody asked.
		return ErrNoResolver
	}
	return r.tryEnqueue(command{kind: cmdCancel, userID: userID, resolve: resolve})
}

// TryReplaceAsync enqueues an atomic cancel-and-replace and returns a channel
// carrying its error — nil on success.
//
// Same shape as TryReduceAsync and for the same reasons: enqueued on the caller's
// goroutine so it cannot overtake the order it names, awaited off it so the matcher
// cannot stall a connection's ingress, and only the error crosses the channel
// because the resulting order is engine-owned.
func (r *Runner) TryReplaceAsync(orderID int64, userID string, replacement *types.Order) (<-chan error, error) {
	return r.tryReplaceAsync(orderID, userID, replacement, nil)
}

// TryReplaceAsyncBy is TryReplaceAsync with the target named at apply time. See
// TryEnqueueCancelBy for why that matters.
func (r *Runner) TryReplaceAsyncBy(userID string, replacement *types.Order, resolve func() (int64, bool)) (<-chan error, error) {
	if resolve == nil {
		return nil, ErrNoResolver
	}
	return r.tryReplaceAsync(0, userID, replacement, resolve)
}

func (r *Runner) tryReplaceAsync(orderID int64, userID string, replacement *types.Order, resolve func() (int64, bool)) (<-chan error, error) {
	select {
	case <-r.quit:
		return nil, ErrShuttingDown
	default:
	}
	reply := make(chan cmdReply, 1)
	cmd := command{kind: cmdReplace, cancelID: orderID, userID: userID, replace: replacement, reply: reply, resolve: resolve}
	select {
	case r.queue <- cmd:
	default:
		return nil, ErrQueueFull
	}
	out := make(chan error, 1)
	go func() {
		select {
		case rep := <-reply:
			out <- rep.err
		case <-r.done:
			select {
			case rep := <-reply:
				out <- rep.err
			default:
				out <- ErrShuttingDown
			}
		}
	}()
	return out, nil
}

// TryCancelAllAsync enqueues an account-wide cancel without waiting for it to be
// applied, and returns a channel carrying how many orders were removed.
//
// Same shape and same reasons as TryReduceAsync: the enqueue happens on the
// caller's goroutine so the sweep cannot overtake an order submitted just before
// it, while the wait moves off that goroutine so the matcher cannot stall a
// connection's ingress. Only the count crosses the channel — the removed orders are
// engine-owned, and a client learns which ones they were from the Canceled events
// on its own stream.
func (r *Runner) TryCancelAllAsync(userID string) (<-chan int, error) {
	select {
	case <-r.quit:
		return nil, ErrShuttingDown
	default:
	}
	reply := make(chan cmdReply, 1)
	select {
	case r.queue <- command{kind: cmdCancelAll, userID: userID, reply: reply}:
	default:
		return nil, ErrQueueFull
	}
	out := make(chan int, 1)
	go func() {
		select {
		case rep := <-reply:
			out <- len(rep.orders)
		case <-r.done:
			select {
			case rep := <-reply:
				out <- len(rep.orders)
			default:
				out <- 0
			}
		}
	}()
	return out, nil
}

// TryReduceAsync enqueues a reduce without waiting for it to be applied, and
// returns a channel carrying only its error — nil on success.
//
// It exists because a reduce needs both properties at once, and neither
// fire-and-forget nor the synchronous Reduce has both. Enqueueing happens on the
// caller's goroutine, so a reduce issued after an order stays behind it in the
// queue; a network ingress that dispatched it from a fresh goroutine could have
// the reduce overtake the very order it names. And unlike a cancel, a reduce can
// fail for a reason the client caused and can fix — asking to grow rather than
// shrink, or to shrink below what is already filled — so discarding the outcome
// would leave that client waiting for a reply that is never coming.
//
// Only the error crosses the channel. The applied *types.Order is engine-owned
// and the matching goroutine keeps mutating it, so handing it to a connection
// goroutine would be the race TryEnqueue exists to avoid.
func (r *Runner) TryReduceAsync(orderID, newQty int64, userID string) (<-chan error, error) {
	return r.tryReduceAsync(orderID, newQty, userID, nil)
}

// TryReduceAsyncBy is TryReduceAsync with the target named at apply time. See
// TryEnqueueCancelBy for why that matters.
func (r *Runner) TryReduceAsyncBy(newQty int64, userID string, resolve func() (int64, bool)) (<-chan error, error) {
	if resolve == nil {
		return nil, ErrNoResolver
	}
	return r.tryReduceAsync(0, newQty, userID, resolve)
}

func (r *Runner) tryReduceAsync(orderID, newQty int64, userID string, resolve func() (int64, bool)) (<-chan error, error) {
	select {
	case <-r.quit:
		return nil, ErrShuttingDown
	default:
	}
	reply := make(chan cmdReply, 1)
	cmd := command{kind: cmdReduce, cancelID: orderID, reduceQty: newQty, userID: userID, reply: reply, resolve: resolve}
	select {
	case r.queue <- cmd:
	default:
		return nil, ErrQueueFull
	}
	out := make(chan error, 1)
	go func() {
		select {
		case rep := <-reply:
			out <- rep.err
		case <-r.done:
			// The loop drains before exiting, so an applied reduce may already
			// have replied; reporting a shutdown over a real result would be a
			// silent lie about whether the book changed.
			select {
			case rep := <-reply:
				out <- rep.err
			default:
				out <- ErrShuttingDown
			}
		}
	}()
	return out, nil
}

// tryEnqueue posts a command with no reply channel. dispatch already tolerates a
// nil reply, so nothing downstream needs to change.
func (r *Runner) tryEnqueue(cmd command) error {
	select {
	case <-r.quit:
		return ErrShuttingDown
	default:
	}
	select {
	case r.queue <- cmd:
		return nil
	default:
		return ErrQueueFull
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

// SetPhase moves the venue to a new trading phase and returns any trades the
// transition produced — which is the opening uncross, when moving out of pre-open.
//
// It goes through the queue like any other command, so the phase change is ordered
// against the order flow rather than racing it: an order submitted just before a
// pre-open closes is either in the auction or after it, never ambiguous.
func (r *Runner) SetPhase(phase EngineState) []*types.Trade {
	rep, ok := r.send(command{kind: cmdSetPhase, phase: phase})
	if !ok {
		return nil
	}
	return rep.trades
}

// IndicativeAuction reports what an uncross would produce against the current book.
//
// It goes through the queue rather than reading the book directly: it walks a full
// snapshot, and doing that from a caller's goroutine while the matcher mutates levels
// would be reading a book mid-command.
func (r *Runner) IndicativeAuction() Indicative {
	rep, ok := r.send(command{kind: cmdIndicative})
	if !ok {
		return Indicative{}
	}
	return rep.indicative
}

// Phase reports the venue's current trading phase.
func (r *Runner) Phase() EngineState { return r.engine.State() }

// ExpireDue removes every order whose time-in-force deadline has passed.
//
// The engine expires lazily, on command arrival, so in a quiet market an expired
// order stays in the book until something happens. That is invisible to anyone
// trading — it is gone before it could match — but a market-data subscriber would see
// depth that should have left. Drive this on a ticker if that matters; cmd/obgw does.
//
// It goes through the queue like any other command, so it is ordered against the
// order flow rather than racing it.
func (r *Runner) ExpireDue() {
	r.send(command{kind: cmdExpireDue})
}

// TrailingStopCount returns the number of resting trailing stops. They are held
// separately from the stop book — a trailing stop has no fixed trigger price to key
// on until the market has moved — so PendingStopCount does not include them.
//
// Unlike the other read accessors this goes through the command queue rather than
// straight to the engine. Those delegate to the book and stop book, which carry
// their own locks; trailing stops live in a plain map owned by the matching
// goroutine, so reading it from a caller's goroutine is a data race. The race
// detector caught exactly that when this was first written as a direct delegation.
func (r *Runner) TrailingStopCount() int {
	rep, ok := r.send(command{kind: cmdTrailingCount})
	if !ok {
		return 0
	}
	return rep.count
}

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

// --- fire-and-forget conditional submission ---
//
// The conditional order types had synchronous entry points only, which a network
// ingress must not use: those hand back the engine-owned order, and a connection
// goroutine reading any field of it races the matcher. These are the TryEnqueue
// counterparts, added when the wire learned to carry these order types.

// TryEnqueueStop is the stop/stop-limit counterpart of TryEnqueue.
func (r *Runner) TryEnqueueStop(s *types.StopOrder) error {
	return r.tryEnqueue(command{kind: cmdStop, stop: s})
}

// TryEnqueueOCO is the one-cancels-other counterpart of TryEnqueue.
func (r *Runner) TryEnqueueOCO(o *types.OCOOrder) error {
	return r.tryEnqueue(command{kind: cmdOCO, oco: o})
}

// TryEnqueueIceberg is the iceberg counterpart of TryEnqueue.
func (r *Runner) TryEnqueueIceberg(ib *types.IcebergOrder) error {
	return r.tryEnqueue(command{kind: cmdIceberg, iceberg: ib})
}

// TryEnqueuePegged is the pegged-order counterpart of TryEnqueue.
func (r *Runner) TryEnqueuePegged(p *types.PeggedOrder) error {
	return r.tryEnqueue(command{kind: cmdPegged, pegged: p})
}

// TryEnqueueTrailing is the trailing-stop counterpart of TryEnqueue.
func (r *Runner) TryEnqueueTrailing(ts *types.TrailingStop) error {
	return r.tryEnqueue(command{kind: cmdTrailing, trailing: ts})
}
