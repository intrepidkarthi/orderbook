package main

import (
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/observability"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// timedLog is a CommandLog that times every append into a histogram.
//
// # Why the timing is here and not in pkg/wal
//
// A library that reaches for a metrics collector has chosen its embedder's
// monitoring stack. pkg/wal is embeddable without pkg/observability today and stays
// that way; the alternative is a Writer taking a metrics interface, which is a
// dependency in a frozen exported surface for a facility one embedder uses.
//
// It also keeps the measurement out of a critical section that is hard to reason
// about. Writer.append runs entirely under w.mu, and that lock is load-bearing for
// rotation correctness; a timing hook inside it is a place a future change can
// deadlock. Timing the call from out here measures the same interval and cannot.
//
// Somebody embedding pkg/wal will find it exports no latency and may read that as a
// gap. It was declined, and this file is the seam they want: wrap the CommandLog,
// time the call, feed whatever collector they already run.
//
// # The rule that decides where this sits
//
// The append histogram never contains an fsync, and the sync histogram contains
// nothing but one.
//
// So under -sync-every-command the composition is syncingLog{timedLog{Writer}} and
// never the other way round. Outside, this would measure syncingLog's fsync as part
// of the append, and the append histogram in the mode where durability matters most
// would be a copy of the sync histogram under a different name.
//
// Read wrong, that looks broken under -sync-every-command: the append histogram
// reads ~1.6 µs while the venue moves at milliseconds per command. That is the
// metric refusing to average two costs with different causes and different fixes.
// The append is the venue's own work; the sync is the storage device; a per-command
// venue's true cost is the sum, which the exposition gives one term at a time.
//
// # What it costs
//
// Two time.Now() reads and one Observe, about 80 ns, on the matching goroutine —
// roughly 5% of a 1,625 ns append and 0.4% of the 18,260 ns group-committed write
// path. Zero allocations: time.Time and time.Duration are values, nothing is boxed,
// and the bucket search is a sort.Search over seventeen constants followed by three
// atomic adds.
//
// Sampling was rejected. One-in-N would cut that cost and would lose the one event
// this histogram exists to catch: a rotation costs 12.4 ms inside a single append,
// once every four minutes at the shipped segment size, and a 1-in-1,000 sample sees
// it approximately never. The tail is the whole reason to measure the append.
//
// # It must be exhaustive
//
// Sixteen methods. A decorator that forgets one silently stops timing an entire
// command kind — AppendSetPhase being the obvious candidate, since it is the method
// that was added last. The assertion that catches that is not "count > 0"; it is
// that obgw_wal_append_latency_ns_count equals Writer.Seq(), the log's own count of
// exactly the records that were appended. See TestAppendLatencyCountsEveryCommandKind.
type timedLog struct {
	inner matching.CommandLog
	hist  *observability.Histogram
}

var _ matching.CommandLog = (*timedLog)(nil)

// The measurement is written out in full at every method rather than hidden behind a
// defer, and that is not style. A deferred call carrying an argument allocates under
// the race detector, and an allocation assertion that has to be skipped under -race is
// an assertion that stops holding on the build most likely to be running in CI. Three
// lines, sixteen times, and TestAppendTimingAllocatesNothing holds everywhere.
//
// The interval covers the error return too: an append that failed still spent the
// time, and excluding failures would hide exactly the slow path an operator is looking
// for.

func (t *timedLog) AppendSubmit(o *types.Order) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendSubmit(o)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendCancel(orderID int64, userID string) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendCancel(orderID, userID)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendReduce(orderID, newQty int64, userID string) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendReduce(orderID, newQty, userID)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendReplace(orderID int64, userID string, replacement *types.Order) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendReplace(orderID, userID, replacement)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendCancelAll(userID string) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendCancelAll(userID)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendStop(o *types.StopOrder) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendStop(o)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendOCO(o *types.OCOOrder) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendOCO(o)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendIceberg(ib *types.IcebergOrder) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendIceberg(ib)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendPegged(p *types.PeggedOrder) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendPegged(p)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendTrailing(ts *types.TrailingStop) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendTrailing(ts)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendHalt() (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendHalt()
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendResume() (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendResume()
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendCancelOnly() (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendCancelOnly()
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendSetMark(price int64) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendSetMark(price)
	t.hist.Observe(time.Since(start))
	return seq, err
}

func (t *timedLog) AppendBust(tradeID int64, reason string) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendBust(tradeID, reason)
	t.hist.Observe(time.Since(start))
	return seq, err
}

// AppendSetPhase is the method the exhaustiveness assertion exists for. It was added
// last, it is the one docs/JOURNAL-COMPLETENESS.md §4.2 exists because of, and a
// decorator that forwarded it undecorated would leave every auction transition
// untimed while every other number on the page looked right.
func (t *timedLog) AppendSetPhase(phase matching.EngineState) (int64, error) {
	start := time.Now()
	seq, err := t.inner.AppendSetPhase(phase)
	t.hist.Observe(time.Since(start))
	return seq, err
}
