package matching

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// recEvent copies the scalar fields of an Event (the pointers are reused after
// the callback, so a sink must not retain them).
type recEvent struct {
	seq     int64
	kind    EventKind
	orderID int64
}

type recSink struct{ got []recEvent }

func (r *recSink) OnEvents(evs []Event) {
	for _, e := range evs {
		r.got = append(r.got, recEvent{e.Seq, e.Kind, e.OrderID})
	}
}

// TestEventStream checks the engine emits a gap-free, strictly monotonic event
// stream with the expected kinds — the ordered seam adapters map onto market-data
// / drop-copy sequence spaces.
func TestEventStream(t *testing.T) {
	sink := &recSink{}
	e := NewEngine(Config{Symbol: "X", EventSink: sink})

	mk := e.Process(lim(t, "mm", types.SideSell, 100, 5))       // ACCEPTED (rests)
	e.Process(lim(t, "t", types.SideBuy, 100, 3))               // ACCEPTED + TRADE
	e.Process(lim(t, "pm", types.SideBuy, 100, 1).AsPostOnly()) // REJECTED (would cross)
	e.Cancel(mk.Order.ID, "mm")                                 // CANCELED

	want := []EventKind{EventAccepted, EventAccepted, EventTrade, EventRejected, EventCanceled}
	if len(sink.got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(sink.got), len(want), sink.got)
	}
	for i, g := range sink.got {
		if g.seq != int64(i+1) {
			t.Errorf("event %d seq = %d, want %d (must be gap-free monotonic)", i, g.seq, i+1)
		}
		if g.kind != want[i] {
			t.Errorf("event %d kind = %s, want %s", i, g.kind, want[i])
		}
	}
}

// TestEventStream_ViaRunner confirms events also flow when driven through the
// concurrency front, and stay in submission order on the single matching
// goroutine.
func TestEventStream_ViaRunner(t *testing.T) {
	sink := &recSink{}
	r := NewRunner(RunnerConfig{Engine: Config{Symbol: "X", EventSink: sink}})

	r.Process(lim(t, "mm", types.SideSell, 100, 5))
	r.Process(lim(t, "t", types.SideBuy, 100, 2))
	r.Close() // drains the queue before we read

	if len(sink.got) != 3 { // Accepted, Accepted, Trade
		t.Fatalf("got %d events, want 3: %+v", len(sink.got), sink.got)
	}
	for i, g := range sink.got {
		if g.seq != int64(i+1) {
			t.Errorf("event %d seq = %d, want %d", i, g.seq, i+1)
		}
	}
}
