package main

import (
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// TestStageHistogramsCountTheSession: after an entered order, a crossing order and
// a cancel, every command has one queue and one match observation, and the publisher
// has timed the batches it fanned out (docs/STAGES.md).
func TestStageHistogramsCountTheSession(t *testing.T) {
	srv := adminServer(t)
	maker := dial(t, srv)
	maker.mustLogin("alice", "pw1")
	maker.enter("m1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := maker.awaitType(t, wire.MsgAccepted, 2*time.Second); !ok {
		t.Fatal("maker not accepted")
	}
	taker := dial(t, srv)
	taker.mustLogin("bob", "pw2")
	taker.enter("t1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 4)
	if _, ok := taker.awaitType(t, wire.MsgExecuted, 2*time.Second); !ok {
		t.Fatal("no fill")
	}
	maker.cancel("m1")
	if _, ok := maker.awaitType(t, wire.MsgCanceled, 2*time.Second); !ok {
		t.Fatal("cancel not acknowledged")
	}

	_, body := adminGet(t, srv, "/metrics")
	const commands = 3
	for _, m := range []string{stageQueueMetric, stageMatchMetric} {
		if got := metricValue(t, body, m+"_count"); got != commands {
			t.Errorf("%s_count = %v, want one per command (%d)", m, got, commands)
		}
	}
	for _, m := range []string{stagePublishWaitMetric, stagePublishFanoutMetric} {
		if got := metricValue(t, body, m+"_count"); got < 1 {
			t.Errorf("%s_count = %v; the publisher fanned out batches and timed none", m, got)
		}
	}
	if q, mt := metricValue(t, body, stageQueueMetric+"_sum"), metricValue(t, body, stageMatchMetric+"_sum"); q <= 0 || mt <= 0 {
		t.Errorf("queue sum %v, match sum %v: a stage that took no time at all was not timed", q, mt)
	}
}
