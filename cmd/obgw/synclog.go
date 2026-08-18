package main

import (
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/observability"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// syncingLog is a CommandLog that fsyncs every record before returning, so a
// command is durable before the engine applies it and therefore before any
// acknowledgement derived from it can reach a client.
//
// # Why this is a flag and not the default
//
// Append runs on the matching goroutine. Syncing here puts a disk write in the
// matching path, so the whole venue moves at the speed of fsync: roughly 210×
// the cost of the group-committed default (docs/BENCHMARKS.md). That is the
// honest price of acknowledgement-after-durability, and for some venues it is
// the right price.
//
// The third answer, which a reference gateway is the wrong place to demonstrate,
// is a commit pipeline: keep group commit, but hold each acknowledgement until
// the sync that covers its record completes, so the disk bounds latency rather
// than throughput. That needs a deferred-ack stage the wire protocol here does
// not have, and building half of one would teach the wrong shape.
//
// The default remains group commit on a 20ms ticker, with the window stated
// rather than hidden — see pkg/wal's package comment.
type syncingLog struct {
	w *wal.Writer
	// inner is what the appends go through, and under the metered configuration it
	// is a timedLog wrapping the same Writer. The nesting is load-bearing and it is
	// this way round: timedLog INSIDE, so the append histogram never contains the
	// fsync below. Wrapped the other way, the append histogram in the one mode where
	// durability matters most would be a copy of the sync histogram under another
	// name. See timedLog's comment and TestAppendLatencyExcludesTheSync.
	//
	// Nil means append straight to the Writer, which is what a test constructing
	// &syncingLog{w: w} gets and what the flag did before there was anything to
	// measure.
	inner matching.CommandLog
	// hist times the fsync this decorator performs, and NOTHING else. Under
	// -sync-every-command the group-commit loop does not run, so this is the only
	// place obgw_wal_sync_latency_ns can come from.
	hist *observability.Histogram
}

var _ matching.CommandLog = (*syncingLog)(nil)

// log is where an append goes.
func (s *syncingLog) log() matching.CommandLog {
	if s.inner != nil {
		return s.inner
	}
	return s.w
}

// sync runs after a successful append. A sync failure is reported as the
// append's error: a record that is not on disk has not been logged, whatever
// the buffer believes, and the Runner halts the engine rather than letting it
// run ahead of its own journal.
func (s *syncingLog) sync(seq int64, err error) (int64, error) {
	if err != nil {
		return seq, err
	}
	if err := s.doSync(); err != nil {
		return seq, err
	}
	return seq, nil
}

// doSync performs the fsync and times it. Failures are observed too: an fsync that
// returned an error still spent the time, and dropping it would trim the tail at the
// exact moment the tail is the story.
func (s *syncingLog) doSync() error {
	if s.hist == nil {
		return s.w.Sync()
	}
	start := time.Now()
	err := s.w.Sync()
	s.hist.Observe(time.Since(start))
	return err
}

func (s *syncingLog) AppendSubmit(o *types.Order) (int64, error) {
	return s.sync(s.log().AppendSubmit(o))
}

func (s *syncingLog) AppendCancel(orderID int64, userID string) (int64, error) {
	return s.sync(s.log().AppendCancel(orderID, userID))
}

func (s *syncingLog) AppendReduce(orderID, newQty int64, userID string) (int64, error) {
	return s.sync(s.log().AppendReduce(orderID, newQty, userID))
}

func (s *syncingLog) AppendReplace(orderID int64, userID string, replacement *types.Order) (int64, error) {
	return s.sync(s.log().AppendReplace(orderID, userID, replacement))
}

func (s *syncingLog) AppendCancelAll(userID string) (int64, error) {
	return s.sync(s.log().AppendCancelAll(userID))
}

func (s *syncingLog) AppendStop(o *types.StopOrder) (int64, error) {
	return s.sync(s.log().AppendStop(o))
}

func (s *syncingLog) AppendOCO(o *types.OCOOrder) (int64, error) {
	return s.sync(s.log().AppendOCO(o))
}

func (s *syncingLog) AppendIceberg(ib *types.IcebergOrder) (int64, error) {
	return s.sync(s.log().AppendIceberg(ib))
}

func (s *syncingLog) AppendPegged(p *types.PeggedOrder) (int64, error) {
	return s.sync(s.log().AppendPegged(p))
}

func (s *syncingLog) AppendTrailing(ts *types.TrailingStop) (int64, error) {
	return s.sync(s.log().AppendTrailing(ts))
}

// The control commands sync too. They are rare enough that the cost is
// irrelevant, and they are the commands whose loss is least recoverable by
// inspection: a missing order shows up in a client's open-order query, a missing
// halt shows up as a venue trading when the operator believed it stopped.

func (s *syncingLog) AppendHalt() (int64, error) { return s.sync(s.log().AppendHalt()) }

func (s *syncingLog) AppendResume() (int64, error) { return s.sync(s.log().AppendResume()) }

func (s *syncingLog) AppendCancelOnly() (int64, error) { return s.sync(s.log().AppendCancelOnly()) }

func (s *syncingLog) AppendSetMark(price int64) (int64, error) {
	return s.sync(s.log().AppendSetMark(price))
}

func (s *syncingLog) AppendBust(tradeID int64, reason string) (int64, error) {
	return s.sync(s.log().AppendBust(tradeID, reason))
}

// AppendSetPhase syncs for the same reason the rest do, and with the sharpest
// version of the argument: a lost phase transition is not one missing command but
// an entire auction, and it is not recoverable by inspection either — the book
// comes back crossed, with prints on subscribers' tapes that the venue's own log
// has no record of.
func (s *syncingLog) AppendSetPhase(phase matching.EngineState) (int64, error) {
	return s.sync(s.log().AppendSetPhase(phase))
}
