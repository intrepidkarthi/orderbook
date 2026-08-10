package main

import (
	"github.com/intrepidkarthi/orderbook/pkg/matching"
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
type syncingLog struct{ w *wal.Writer }

var _ matching.CommandLog = (*syncingLog)(nil)

// sync runs after a successful append. A sync failure is reported as the
// append's error: a record that is not on disk has not been logged, whatever
// the buffer believes, and the Runner halts the engine rather than letting it
// run ahead of its own journal.
func (s *syncingLog) sync(seq int64, err error) (int64, error) {
	if err != nil {
		return seq, err
	}
	if err := s.w.Sync(); err != nil {
		return seq, err
	}
	return seq, nil
}

func (s *syncingLog) AppendSubmit(o *types.Order) (int64, error) {
	return s.sync(s.w.AppendSubmit(o))
}

func (s *syncingLog) AppendCancel(orderID int64, userID string) (int64, error) {
	return s.sync(s.w.AppendCancel(orderID, userID))
}

func (s *syncingLog) AppendReduce(orderID, newQty int64, userID string) (int64, error) {
	return s.sync(s.w.AppendReduce(orderID, newQty, userID))
}

func (s *syncingLog) AppendReplace(orderID int64, userID string, replacement *types.Order) (int64, error) {
	return s.sync(s.w.AppendReplace(orderID, userID, replacement))
}

func (s *syncingLog) AppendCancelAll(userID string) (int64, error) {
	return s.sync(s.w.AppendCancelAll(userID))
}

func (s *syncingLog) AppendStop(o *types.StopOrder) (int64, error) {
	return s.sync(s.w.AppendStop(o))
}

func (s *syncingLog) AppendOCO(o *types.OCOOrder) (int64, error) {
	return s.sync(s.w.AppendOCO(o))
}

func (s *syncingLog) AppendIceberg(ib *types.IcebergOrder) (int64, error) {
	return s.sync(s.w.AppendIceberg(ib))
}

func (s *syncingLog) AppendPegged(p *types.PeggedOrder) (int64, error) {
	return s.sync(s.w.AppendPegged(p))
}

func (s *syncingLog) AppendTrailing(ts *types.TrailingStop) (int64, error) {
	return s.sync(s.w.AppendTrailing(ts))
}

// The control commands sync too. They are rare enough that the cost is
// irrelevant, and they are the commands whose loss is least recoverable by
// inspection: a missing order shows up in a client's open-order query, a missing
// halt shows up as a venue trading when the operator believed it stopped.

func (s *syncingLog) AppendHalt() (int64, error) { return s.sync(s.w.AppendHalt()) }

func (s *syncingLog) AppendResume() (int64, error) { return s.sync(s.w.AppendResume()) }

func (s *syncingLog) AppendCancelOnly() (int64, error) { return s.sync(s.w.AppendCancelOnly()) }

func (s *syncingLog) AppendSetMark(price int64) (int64, error) {
	return s.sync(s.w.AppendSetMark(price))
}

func (s *syncingLog) AppendBust(tradeID int64, reason string) (int64, error) {
	return s.sync(s.w.AppendBust(tradeID, reason))
}
