package matching

import (
	"errors"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func redOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// TestReduceKeepsQueuePosition is the entire reason this operation exists. If it
// did not hold, a gateway could implement the same thing with cancel-then-new and
// the engine would not need to expose anything.
func TestReduceKeepsQueuePosition(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))

	first := redOrder(t, "mm", types.SideBuy, 100, 10)
	second := redOrder(t, "other", types.SideBuy, 100, 10)
	e.Process(first)
	e.Process(second)

	if _, err := e.Reduce(first.ID, 4, "mm"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}

	// A seller taking 4 must hit the reduced order first, not the one behind it.
	res := e.Process(redOrder(t, "taker", types.SideSell, 100, 4))
	if len(res.Trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(res.Trades))
	}
	if got := res.Trades[0].MakerOrderID; got != first.ID {
		t.Errorf("traded against order %d, want %d — the reduced order lost its place in the queue", got, first.ID)
	}
}

// TestReduceUpdatesAggregateDepth — level depth must equal the sum of its orders,
// or every market-data consumer sees size that is not there.
func TestReduceUpdatesAggregateDepth(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)

	if _, _, ok := e.BestBid(); !ok {
		t.Fatal("no bid")
	}
	if _, err := e.Reduce(o.ID, 3, "mm"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	_, qty, ok := e.BestBid()
	if !ok {
		t.Fatal("bid vanished after reduce")
	}
	if qty != 3 {
		t.Errorf("aggregate depth = %d, want 3", qty)
	}
	if o.RemainingQty != 3 {
		t.Errorf("order remaining = %d, want 3", o.RemainingQty)
	}
}

// TestReduceAfterPartialFill — newQty is the new total, so an order that has
// already filled 4 of 10 and reduces to 6 has 2 left.
func TestReduceAfterPartialFill(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)
	e.Process(redOrder(t, "taker", types.SideSell, 100, 4))

	if o.FilledQty != 4 {
		t.Fatalf("setup: filled %d, want 4", o.FilledQty)
	}
	if _, err := e.Reduce(o.ID, 6, "mm"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if o.RemainingQty != 2 {
		t.Errorf("remaining = %d, want 2 (6 total minus 4 already filled)", o.RemainingQty)
	}
}

// TestReduceRejectsIncreaseAndPriceGames — growing in place would let a
// participant reserve a place in the queue and then claim it.
func TestReduceRejectsIncrease(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)

	for _, q := range []int64{10, 11, 0, -1} {
		if _, err := e.Reduce(o.ID, q, "mm"); err != types.ErrInvalidQuantity {
			t.Errorf("Reduce to %d: err = %v, want ErrInvalidQuantity", q, err)
		}
	}
	if o.Quantity != 10 {
		t.Errorf("quantity changed to %d despite a rejected reduce", o.Quantity)
	}
}

// TestReduceBelowFilledIsRejected — clamping silently would leave the caller's
// model of the order wrong with no way to notice.
func TestReduceBelowFilledIsRejected(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)
	e.Process(redOrder(t, "taker", types.SideSell, 100, 6))

	if _, err := e.Reduce(o.ID, 5, "mm"); err != types.ErrInvalidQuantity {
		t.Errorf("reduce below filled quantity: err = %v, want ErrInvalidQuantity", err)
	}
}

// TestReduceRejectsAnotherAccount, and does so indistinguishably from a missing
// order: a probe must not be able to learn that someone else's order exists.
func TestReduceRejectsAnotherAccount(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)

	_, errOther := e.Reduce(o.ID, 5, "someone-else")
	_, errMissing := e.Reduce(999999, 5, "someone-else")
	if errOther != types.ErrOrderNotFound {
		t.Errorf("reducing another account's order: err = %v, want ErrOrderNotFound", errOther)
	}
	if errOther != errMissing {
		t.Errorf("a probe can distinguish an existing order (%v) from a missing one (%v)", errOther, errMissing)
	}
	if o.Quantity != 10 {
		t.Error("another account's reduce changed the order")
	}
}

// TestReduceAnnouncesReplaced — a consumer must learn the new size, since no
// trade explains the change.
func TestReduceAnnouncesReplaced(t *testing.T) {
	sink := &ocoSink{}
	cfg := DefaultConfig("X")
	cfg.EventSink = sink
	e := NewEngine(cfg)

	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)
	if _, err := e.Reduce(o.ID, 4, "mm"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if !sink.sawOrder(EventReplaced, o.ID) {
		t.Error("no Replaced event; a consumer's size accounting would stay wrong")
	}
}

// TestReduceRespectsMinRestingTime — MinRestingTime targets the Coscia pattern:
// post size, pull it before it can fill. A reduce from 1000 lots to 1 withdraws
// 999 of them, so a floor that guarded Cancel and not Reduce would leave the whole
// pattern available behind a different verb. It mattered little while only an
// embedder could call Reduce; it matters now that the wire carries it.
func TestReduceRespectsMinRestingTime(t *testing.T) {
	cfg := DefaultConfig("X")
	cfg.MinRestingTime = time.Hour
	e := NewEngine(cfg)

	o := redOrder(t, "spoofer", types.SideBuy, 100, 1000)
	e.Process(o)

	// The cancel route is closed...
	if _, err := e.Cancel(o.ID, "spoofer"); !errors.Is(err, types.ErrCancelTooSoon) {
		t.Fatalf("Cancel err = %v, want ErrCancelTooSoon — the premise of this test", err)
	}
	// ...and so must the reduce route be.
	if _, err := e.Reduce(o.ID, 1, "spoofer"); !errors.Is(err, types.ErrCancelTooSoon) {
		t.Errorf("Reduce err = %v, want ErrCancelTooSoon — 999 lots of displayed size were withdrawn inside the resting floor", err)
	}
	if _, qty, _ := e.BestBid(); qty != 1000 {
		t.Errorf("depth = %d, want 1000 — the refused reduce still shrank the book", qty)
	}
}

// TestReduceRejectsIncreaseBeforeCheckingTheClock — an impossible request must
// always get the same answer, not one that depends on how long the order has
// rested. A clock-dependent error is a client bug that reproduces only sometimes.
func TestReduceRejectsIncreaseBeforeCheckingTheClock(t *testing.T) {
	cfg := DefaultConfig("X")
	cfg.MinRestingTime = time.Hour
	e := NewEngine(cfg)

	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	e.Process(o)

	if _, err := e.Reduce(o.ID, 50, "mm"); !errors.Is(err, types.ErrInvalidQuantity) {
		t.Errorf("err = %v, want ErrInvalidQuantity", err)
	}
}

// TestPrivilegedReduceIgnoresRestingFloor — the floor must not block a
// liquidation, for the same reason the operator kill switch ignores it.
func TestPrivilegedReduceIgnoresRestingFloor(t *testing.T) {
	cfg := DefaultConfig("X")
	cfg.MinRestingTime = time.Hour
	e := NewEngine(cfg)

	o := redOrder(t, "liq", types.SideBuy, 100, 1000)
	o.Privileged = true
	e.Process(o)

	if _, err := e.Reduce(o.ID, 10, "liq"); err != nil {
		t.Errorf("Reduce: %v — the resting floor blocked a privileged order", err)
	}
}

// TestReplayReduceIgnoresRestingFloor — replay must not re-litigate a command the
// log already records as accepted. Re-checking it against replay-time timestamps
// would refuse an accepted reduce and diverge the recovered book.
func TestReplayReduceIgnoresRestingFloor(t *testing.T) {
	cfg := DefaultConfig("X")
	cfg.MinRestingTime = time.Hour
	e := NewEngine(cfg)
	e.SetReplaying(true)
	defer e.SetReplaying(false)

	o := redOrder(t, "mm", types.SideBuy, 100, 1000)
	e.Process(o)

	if _, err := e.Reduce(o.ID, 10, "mm"); err != nil {
		t.Errorf("Reduce during replay: %v", err)
	}
}

// TestRunnerReduce drives it through the concurrency front.
func TestRunnerReduce(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	defer r.Close()

	res := r.Process(redOrder(t, "mm", types.SideBuy, 100, 10))
	if res == nil || res.Order == nil {
		t.Fatal("submit failed")
	}
	got, err := r.Reduce(res.Order.ID, 4, "mm")
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if got.Quantity != 4 {
		t.Errorf("quantity = %d, want 4", got.Quantity)
	}
	if _, qty, _ := r.BestBid(); qty != 4 {
		t.Errorf("aggregate depth = %d, want 4", qty)
	}
}

// TestTryReduceAsyncReportsSuccess — the channel carries nil, not a zero value a
// caller has to interpret.
func TestTryReduceAsyncReportsSuccess(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	defer r.Close()

	res := r.Process(redOrder(t, "mm", types.SideBuy, 100, 10))
	done, err := r.TryReduceAsync(res.Order.ID, 4, "mm")
	if err != nil {
		t.Fatalf("TryReduceAsync: %v", err)
	}
	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatalf("reduce failed: %v", rerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no outcome; a client would wait forever for a reply")
	}
	if _, qty, _ := r.BestBid(); qty != 4 {
		t.Errorf("aggregate depth = %d, want 4", qty)
	}
}

// TestTryReduceAsyncReportsFailure is the reason this path exists rather than a
// fire-and-forget enqueue. A reduce can fail for a reason the client caused and
// can correct, and silence is indistinguishable from a reduce still in flight.
func TestTryReduceAsyncReportsFailure(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	defer r.Close()

	res := r.Process(redOrder(t, "mm", types.SideBuy, 100, 10))

	cases := map[string]struct {
		id     int64
		qty    int64
		user   string
		reason error
	}{
		"an increase":       {res.Order.ID, 50, "mm", types.ErrInvalidQuantity},
		"another account":   {res.Order.ID, 4, "someone-else", types.ErrOrderNotFound},
		"no such order":     {res.Order.ID + 999, 4, "mm", types.ErrOrderNotFound},
		"same size is a no": {res.Order.ID, 10, "mm", types.ErrInvalidQuantity},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			done, err := r.TryReduceAsync(c.id, c.qty, c.user)
			if err != nil {
				t.Fatalf("TryReduceAsync: %v", err)
			}
			select {
			case rerr := <-done:
				if !errors.Is(rerr, c.reason) {
					t.Errorf("err = %v, want %v", rerr, c.reason)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no outcome reported")
			}
		})
	}
	// The order must be untouched by every refusal above.
	if _, qty, _ := r.BestBid(); qty != 10 {
		t.Errorf("aggregate depth = %d, want 10 — a refused reduce changed the book", qty)
	}
}

// TestTryReduceAsyncEnqueuesInOrder is why the enqueue is synchronous while only
// the wait is not. A reduce dispatched from its own goroutine could overtake the
// order it names and be refused for an order that does not exist yet.
func TestTryReduceAsyncEnqueuesInOrder(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 64})
	defer r.Close()

	// Submit fire-and-forget, then reduce immediately, exactly as a connection's
	// read loop does when both messages arrive in one burst.
	o := redOrder(t, "mm", types.SideBuy, 100, 10)
	if err := r.TryEnqueue(o); err != nil {
		t.Fatalf("TryEnqueue: %v", err)
	}
	// The id is assigned by the engine, so a real ingress resolves it from its own
	// registry; here the order carries it once accepted. Wait for that, then reduce
	// with no further synchronisation.
	deadline := time.Now().Add(2 * time.Second)
	for r.OrderCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	done, err := r.TryReduceAsync(o.ID, 4, "mm")
	if err != nil {
		t.Fatalf("TryReduceAsync: %v", err)
	}
	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatalf("reduce lost the race with its own order: %v", rerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no outcome reported")
	}
}

// TestTryReduceAsyncAfterCloseIsRefused — a producer racing shutdown gets an
// error, not a channel that never delivers.
func TestTryReduceAsyncAfterCloseIsRefused(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X")})
	res := r.Process(redOrder(t, "mm", types.SideBuy, 100, 10))
	id := res.Order.ID
	r.Close()

	if _, err := r.TryReduceAsync(id, 4, "mm"); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("err = %v, want ErrShuttingDown", err)
	}
}

// TestReduceIsWrittenToTheLog — the durability of this command is proven end to
// end in pkg/wal; this pins the seam itself, since a CommandLog that is never
// called is the shape the bug took.
func TestReduceIsWrittenToTheLog(t *testing.T) {
	log := &countingLog{}
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), Log: log})
	defer r.Close()

	res := r.Process(redOrder(t, "mm", types.SideBuy, 100, 10))
	if _, err := r.Reduce(res.Order.ID, 4, "mm"); err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if _, err := r.CancelAllForUser("mm"); err != nil {
		t.Fatalf("CancelAllForUser: %v", err)
	}
	if log.reduces != 1 {
		t.Errorf("log saw %d reduces, want 1 — the book changed without the log knowing", log.reduces)
	}
	if log.cancelAlls != 1 {
		t.Errorf("log saw %d cancel-alls, want 1", log.cancelAlls)
	}
}

// countingLog counts what the Runner appends. It deliberately implements the whole
// interface rather than embedding a partial one, so adding a mutating command
// without logging it fails to compile here.
type countingLog struct {
	submits, cancels, reduces, cancelAlls     int
	stops, ocos, icebergs, peggeds, trailings int
	replaces                                  int
	seq                                       int64
}

func (l *countingLog) next() (int64, error) { l.seq++; return l.seq, nil }

func (l *countingLog) AppendSubmit(*types.Order) (int64, error) { l.submits++; return l.next() }

func (l *countingLog) AppendCancel(int64, string) (int64, error) { l.cancels++; return l.next() }

func (l *countingLog) AppendReduce(int64, int64, string) (int64, error) { l.reduces++; return l.next() }

func (l *countingLog) AppendCancelAll(string) (int64, error) { l.cancelAlls++; return l.next() }

func (l *countingLog) AppendReplace(int64, string, *types.Order) (int64, error) {
	l.replaces++
	return l.next()
}

func (l *countingLog) AppendStop(*types.StopOrder) (int64, error) { l.stops++; return l.next() }

func (l *countingLog) AppendOCO(*types.OCOOrder) (int64, error) { l.ocos++; return l.next() }

func (l *countingLog) AppendIceberg(*types.IcebergOrder) (int64, error) {
	l.icebergs++
	return l.next()
}

func (l *countingLog) AppendPegged(*types.PeggedOrder) (int64, error) { l.peggeds++; return l.next() }

func (l *countingLog) AppendTrailing(*types.TrailingStop) (int64, error) {
	l.trailings++
	return l.next()
}

// TestReplaceHonoursMinRestingTime — a replace cancels displayed size, so the
// anti-spoofing floor must apply to it too. A verb that escaped it would leave the
// control guarding two routes out of three: Cancel, Reduce, and then a Replace that
// pulls the order anyway.
func TestReplaceHonoursMinRestingTime(t *testing.T) {
	cfg := DefaultConfig("X")
	cfg.MinRestingTime = time.Hour
	e := NewEngine(cfg)

	o := redOrder(t, "spoofer", types.SideBuy, 100, 1000)
	e.Process(o)

	repl := redOrder(t, "spoofer", types.SideBuy, 90, 1)
	if _, err := e.Replace(o.ID, "spoofer", repl); !errors.Is(err, types.ErrCancelTooSoon) {
		t.Errorf("err = %v, want ErrCancelTooSoon", err)
	}
	// And nothing changed: the original still rests and the replacement was not
	// entered.
	if got := e.OrderCount(); got != 1 {
		t.Errorf("book holds %d orders, want 1", got)
	}
	if _, qty, _ := e.BestBid(); qty != 1000 {
		t.Errorf("depth = %d, want the original 1000", qty)
	}
}

// TestReplaceOfAMissingOrderEntersNothing pins the asymmetry: if the cancel half
// cannot happen, the replacement must not be entered. A client replacing an order it
// no longer holds did not ask to open a new position.
func TestReplaceOfAMissingOrderEntersNothing(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	repl := redOrder(t, "mm", types.SideBuy, 100, 5)
	if _, err := e.Replace(999, "mm", repl); !errors.Is(err, types.ErrOrderNotFound) {
		t.Errorf("err = %v, want ErrOrderNotFound", err)
	}
	if got := e.OrderCount(); got != 0 {
		t.Errorf("book holds %d orders — a replacement was entered for an order that never existed", got)
	}
}

// TestReplaceCannotCrossAccounts — Replace goes through Cancel, which checks
// ownership, so it inherits the guard rather than reimplementing it.
func TestReplaceCannotCrossAccounts(t *testing.T) {
	e := NewEngine(DefaultConfig("X"))
	victim := redOrder(t, "victim", types.SideBuy, 100, 10)
	e.Process(victim)

	repl := redOrder(t, "attacker", types.SideBuy, 1, 1)
	if _, err := e.Replace(victim.ID, "attacker", repl); !errors.Is(err, types.ErrOrderNotFound) {
		t.Errorf("err = %v, want ErrOrderNotFound", err)
	}
	if _, qty, _ := e.BestBid(); qty != 10 {
		t.Errorf("depth = %d, want the victim's untouched 10", qty)
	}
}

// TestReplaceIsLoggedAsOneCommand — the log must record it as a single step. Two
// records, a cancel and a submit, would let replay apply one without the other.
func TestReplaceIsLoggedAsOneCommand(t *testing.T) {
	log := &countingLog{}
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), Log: log})
	defer r.Close()

	res := r.Process(redOrder(t, "mm", types.SideBuy, 100, 10))
	repl := redOrder(t, "mm", types.SideBuy, 105, 12)
	done, err := r.TryReplaceAsync(res.Order.ID, "mm", repl)
	if err != nil {
		t.Fatalf("TryReplaceAsync: %v", err)
	}
	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatalf("replace failed: %v", rerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no outcome reported")
	}
	if log.replaces != 1 {
		t.Errorf("log saw %d replaces, want 1", log.replaces)
	}
	if log.cancels != 0 {
		t.Errorf("log saw %d separate cancels; a replace must be one record, not two", log.cancels)
	}
}
