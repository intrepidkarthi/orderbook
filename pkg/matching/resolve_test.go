package matching

import (
	"errors"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// TestTheResolverRunsAfterEveryEarlierCommand is the property the whole mechanism
// exists for.
//
// A gateway holds only the client's own name for an order. Asking "which order is
// that?" on the gateway's goroutine asks it before the queue, so the answer can be
// "no such order" while the Enter that creates it is still queued ahead of the
// cancel — and a client told its order does not exist does not ask again. The
// resolver runs at the front of the queue instead, where every earlier command has
// already been applied, so the answer is the one the book will act on.
func TestTheResolverRunsAfterEveryEarlierCommand(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 64})
	defer r.Close()

	o := mkOrder("alice", types.SideBuy, 100, 5)
	if err := r.TryEnqueue(o); err != nil {
		t.Fatalf("TryEnqueue: %v", err)
	}

	seen := make(chan int, 1)
	err := r.TryEnqueueCancelBy("alice", func() (int64, bool) {
		// On the matching goroutine, at the front of the queue. The order this cancel
		// names must already be in the book.
		seen <- r.OrderCount()
		return o.ID, true
	})
	if err != nil {
		t.Fatalf("TryEnqueueCancelBy: %v", err)
	}

	select {
	case n := <-seen:
		if n != 1 {
			t.Errorf("the book held %d orders when the cancel was resolved; its own Enter had not been applied", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the resolver never ran")
	}

	deadline := time.Now().Add(3 * time.Second)
	for r.OrderCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := r.OrderCount(); n != 0 {
		t.Errorf("%d orders left resting; the resolved cancel did not remove its order", n)
	}
}

// TestAnUnresolvedCancelCancelsNothing — the resolver saying "no" must leave the
// book alone rather than falling through to a zero order id.
func TestAnUnresolvedCancelCancelsNothing(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 64})
	defer r.Close()

	r.Process(mkOrder("alice", types.SideBuy, 100, 5))
	if n := r.OrderCount(); n != 1 {
		t.Fatalf("setup: book has %d orders, want 1", n)
	}

	called := make(chan struct{}, 1)
	if err := r.TryEnqueueCancelBy("alice", func() (int64, bool) {
		called <- struct{}{}
		return 0, false
	}); err != nil {
		t.Fatalf("TryEnqueueCancelBy: %v", err)
	}
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("the resolver never ran")
	}

	// Give the matcher a command that does resolve, so this test observes a book the
	// unresolved cancel has definitely had its chance at.
	r.Process(mkOrder("bob", types.SideSell, 200, 1))
	if n := r.OrderCount(); n != 2 {
		t.Errorf("book has %d orders, want both; an unresolved cancel removed something", n)
	}
}

// TestAnUnresolvedReduceReportsItRatherThanSucceeding — the async variants carry the
// outcome back, so a client that asked to shrink an order it cannot name must be told
// so rather than left believing the size changed.
func TestAnUnresolvedReduceReportsItRatherThanSucceeding(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 64})
	defer r.Close()

	done, err := r.TryReduceAsyncBy(3, "alice", func() (int64, bool) { return 0, false })
	if err != nil {
		t.Fatalf("TryReduceAsyncBy: %v", err)
	}
	select {
	case got := <-done:
		if !errors.Is(got, types.ErrOrderNotFound) {
			t.Errorf("reduce error = %v, want %v", got, types.ErrOrderNotFound)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the reduce never reported")
	}
}

// TestResolutionHappensBeforeTheCommandIsJournalled — the log has to record the
// engine's order id, not the client's name for it. Recording the name would make a
// replay depend on a mapping that only ever existed inside a gateway process that is
// no longer running.
func TestResolutionHappensBeforeTheCommandIsJournalled(t *testing.T) {
	log := &recordingLog{}
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 64, Log: log})
	defer r.Close()

	o := mkOrder("alice", types.SideBuy, 100, 5)
	r.Process(o)

	done := make(chan struct{})
	if err := r.TryEnqueueCancelBy("alice", func() (int64, bool) {
		defer close(done)
		return o.ID, true
	}); err != nil {
		t.Fatalf("TryEnqueueCancelBy: %v", err)
	}
	<-done
	// Another synchronous command, so the cancel has certainly been journalled.
	r.Process(mkOrder("bob", types.SideSell, 200, 1))

	if got := log.lastCancelID(); got != o.ID {
		t.Errorf("journalled cancel id %d, want the resolved %d", got, o.ID)
	}
}

// recordingLog captures what the runner journals.
type recordingLog struct {
	cancels []int64
}

func (l *recordingLog) AppendSubmit(o *types.Order) (int64, error) { return 0, nil }
func (l *recordingLog) AppendCancel(orderID int64, userID string) (int64, error) {
	l.cancels = append(l.cancels, orderID)
	return 0, nil
}
func (l *recordingLog) AppendReduce(orderID, newQty int64, userID string) (int64, error) {
	return 0, nil
}
func (l *recordingLog) AppendReplace(orderID int64, userID string, replacement *types.Order) (int64, error) {
	return 0, nil
}
func (l *recordingLog) AppendCancelAll(userID string) (int64, error)         { return 0, nil }
func (l *recordingLog) AppendStop(s *types.StopOrder) (int64, error)         { return 0, nil }
func (l *recordingLog) AppendOCO(o *types.OCOOrder) (int64, error)           { return 0, nil }
func (l *recordingLog) AppendIceberg(ib *types.IcebergOrder) (int64, error)  { return 0, nil }
func (l *recordingLog) AppendPegged(p *types.PeggedOrder) (int64, error)     { return 0, nil }
func (l *recordingLog) AppendTrailing(ts *types.TrailingStop) (int64, error) { return 0, nil }
func (l *recordingLog) AppendHalt() (int64, error)                           { return 0, nil }
func (l *recordingLog) AppendResume() (int64, error)                         { return 0, nil }
func (l *recordingLog) AppendCancelOnly() (int64, error)                     { return 0, nil }
func (l *recordingLog) AppendSetMark(price int64) (int64, error)             { return 0, nil }
func (l *recordingLog) AppendBust(tradeID int64, reason string) (int64, error) {
	return 0, nil
}
func (l *recordingLog) AppendSetPhase(phase EngineState) (int64, error) { return 0, nil }
func (l *recordingLog) lastCancelID() int64 {
	if len(l.cancels) == 0 {
		return -1
	}
	return l.cancels[len(l.cancels)-1]
}

// TestAByEnqueueWithoutAResolverIsRefused — a nil resolver would otherwise become a
// command naming order zero: journalled, applied, refused. A real log record for a
// question nobody asked.
func TestAByEnqueueWithoutAResolverIsRefused(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 8})
	defer r.Close()

	if err := r.TryEnqueueCancelBy("alice", nil); !errors.Is(err, ErrNoResolver) {
		t.Errorf("cancel: err = %v, want %v", err, ErrNoResolver)
	}
	if _, err := r.TryReduceAsyncBy(1, "alice", nil); !errors.Is(err, ErrNoResolver) {
		t.Errorf("reduce: err = %v, want %v", err, ErrNoResolver)
	}
	if _, err := r.TryReplaceAsyncBy("alice", mkOrder("alice", types.SideBuy, 100, 1), nil); !errors.Is(err, ErrNoResolver) {
		t.Errorf("replace: err = %v, want %v", err, ErrNoResolver)
	}
}

// TestAResolverThatPanicsDoesNotTakeTheVenueWithIt — the resolver is caller-supplied
// code running on the matching goroutine, which is the one goroutine whose death
// stops the market. A gateway bug must not be a venue outage.
func TestAResolverThatPanicsDoesNotTakeTheVenueWithIt(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 8})
	defer r.Close()

	if err := r.TryEnqueueCancelBy("alice", func() (int64, bool) {
		panic("a gateway bug")
	}); err != nil {
		t.Fatalf("TryEnqueueCancelBy: %v", err)
	}

	// The matcher must still be matching.
	done := make(chan int, 1)
	go func() { r.Process(mkOrder("bob", types.SideBuy, 100, 5)); done <- r.OrderCount() }()
	select {
	case n := <-done:
		if n != 1 {
			t.Errorf("book has %d orders, want 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the matching goroutine died with the resolver")
	}
}

// TestAPanickingResolverIsReportedNotSwallowed — contained is not the same as hidden.
// The async paths must carry it back, or a gateway bug becomes a client whose reduce
// silently did nothing.
func TestAPanickingResolverIsReportedNotSwallowed(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 8})
	defer r.Close()

	done, err := r.TryReduceAsyncBy(1, "alice", func() (int64, bool) { panic("boom") })
	if err != nil {
		t.Fatalf("TryReduceAsyncBy: %v", err)
	}
	select {
	case got := <-done:
		if !errors.Is(got, ErrResolverPanicked) {
			t.Errorf("err = %v, want %v", got, ErrResolverPanicked)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no outcome reported")
	}
}

// TestAPanickingResolverLeavesTheBookUntouched — the resolver runs before the command
// is journalled and before it is applied, so the claim that the book is exactly what it
// was is a claim worth asserting rather than reasoning about.
func TestAPanickingResolverLeavesTheBookUntouched(t *testing.T) {
	logged := &recordingLog{}
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("X"), QueueSize: 8, Log: logged})
	defer r.Close()

	r.Process(mkOrder("alice", types.SideBuy, 100, 5))
	if err := r.TryEnqueueCancelBy("alice", func() (int64, bool) { panic("boom") }); err != nil {
		t.Fatalf("TryEnqueueCancelBy: %v", err)
	}
	// A synchronous command behind it, so the panicking one has certainly been handled.
	r.Process(mkOrder("bob", types.SideSell, 200, 1))

	if n := r.OrderCount(); n != 2 {
		t.Errorf("book has %d orders, want 2; the panicking cancel changed the book", n)
	}
	if got := logged.lastCancelID(); got != -1 {
		t.Errorf("a cancel was journalled (id %d); nothing should have been", got)
	}
}
