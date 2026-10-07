package matching

import (
	"sync"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

type stageLog struct {
	mu    sync.Mutex
	queue []time.Duration
	match []time.Duration
}

func (s *stageLog) observe(q, m time.Duration) {
	s.mu.Lock()
	s.queue = append(s.queue, q)
	s.match = append(s.match, m)
	s.mu.Unlock()
}

func (s *stageLog) snapshot() ([]time.Duration, []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.queue...), append([]time.Duration(nil), s.match...)
}

func stageOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// TestStagesStampEveryEntryPoint drives one command through each way into the
// queue, after the Runner has existed for a while. A path that skipped the enqueue
// stamp would report the Runner's whole age as queue wait, so every queue time must
// sit well under that age, and every command must report exactly once.
func TestStagesStampEveryEntryPoint(t *testing.T) {
	var sl stageLog
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("BTC-USD"), ObserveStages: sl.observe})
	defer r.Close()
	const age = 300 * time.Millisecond
	time.Sleep(age)

	sent := 0
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	a := stageOrder(t, "a", types.SideBuy, 100, 10)
	r.Process(a) // send
	sent++
	<-r.SubmitAsync(stageOrder(t, "a", types.SideBuy, 99, 1))
	sent++
	_, err := r.TrySubmit(stageOrder(t, "a", types.SideBuy, 98, 1))
	must(err)
	sent++
	ch, err := r.TrySubmitAsync(stageOrder(t, "a", types.SideBuy, 97, 1))
	must(err)
	<-ch
	sent++
	must(r.TryEnqueue(stageOrder(t, "a", types.SideBuy, 96, 1)))
	sent++
	_, err = r.Reduce(a.ID, 5, "a")
	must(err)
	sent++
	red, err := r.TryReduceAsync(a.ID, 4, "a")
	must(err)
	<-red
	sent++
	rep, err := r.TryReplaceAsync(a.ID, "a", stageOrder(t, "a", types.SideBuy, 95, 3))
	must(err)
	<-rep
	sent++
	all, err := r.TryCancelAllAsync("a")
	must(err)
	<-all
	sent++
	_, err = r.OpenOrdersFor("a") // also a barrier: everything above has been applied
	must(err)
	sent++

	q, m := sl.snapshot()
	if len(q) != sent || len(m) != sent {
		t.Fatalf("%d commands sent, %d observed", sent, len(q))
	}
	for i := range q {
		if q[i] < 0 || q[i] >= age/2 || m[i] < 0 {
			t.Fatalf("command %d: queue %v, match %v; a queue time near the Runner's age (%v) means its enqueue was never stamped", i, q[i], m[i], age)
		}
	}
}

// slowLog is a CommandLog whose first submit append takes a while, so the command
// behind it waits.
type slowLog struct {
	recordingLog
	once sync.Once
	d    time.Duration
}

func (l *slowLog) AppendSubmit(o *types.Order) (int64, error) {
	l.once.Do(func() { time.Sleep(l.d) })
	return 0, nil
}

// slowSink makes applying a command take a while: the sink runs inside the engine's
// apply, which is the match stage.
type slowSink struct {
	d    time.Duration
	slow bool
}

func (s *slowSink) OnEvents([]Event) {
	if s.slow {
		time.Sleep(s.d)
	}
}

// TestStagesAttributeTheDelayToTheRightStage: a command stuck behind a slow log
// append reports queue time, and a command whose apply is slow reports match time,
// and neither leaks into the other.
func TestStagesAttributeTheDelayToTheRightStage(t *testing.T) {
	const d = 80 * time.Millisecond
	var sl stageLog
	sink := &slowSink{d: d}
	cfg := DefaultConfig("BTC-USD")
	cfg.EventSink = sink
	r := NewRunner(RunnerConfig{Engine: cfg, Log: &slowLog{d: d}, ObserveStages: sl.observe})
	defer r.Close()

	first := r.SubmitAsync(stageOrder(t, "a", types.SideBuy, 100, 1))
	second := r.SubmitAsync(stageOrder(t, "b", types.SideBuy, 99, 1))
	<-first
	<-second
	q, m := sl.snapshot()
	if q[1] < d*3/4 {
		t.Fatalf("the command behind a %v log append waited %v in the queue", d, q[1])
	}
	if m[0] >= d/2 || m[1] >= d/2 {
		t.Fatalf("a slow log append leaked into match time: %v, %v", m[0], m[1])
	}

	sink.slow = true
	r.Process(stageOrder(t, "c", types.SideBuy, 98, 1))
	q, m = sl.snapshot()
	if m[2] < d*3/4 {
		t.Fatalf("a %v apply reported %v of match time", d, m[2])
	}
	if q[2] >= d/2 {
		t.Fatalf("a slow apply leaked into its own queue time: %v", q[2])
	}
}

// TestStagesNilObserverStampsNothing: without an observer no command carries an
// enqueue instant, so the default Runner does no extra work.
func TestStagesNilObserverStampsNothing(t *testing.T) {
	r := NewRunner(RunnerConfig{Engine: DefaultConfig("BTC-USD")})
	defer r.Close()
	if c := r.stamp(command{kind: cmdSubmit}); c.enq != 0 {
		t.Fatalf("enq %d without an observer", c.enq)
	}
}
