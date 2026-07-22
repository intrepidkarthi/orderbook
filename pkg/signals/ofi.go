package signals

import (
	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
)

// OFIStep computes the best-level order-flow-imbalance contribution e_n between
// two consecutive book snapshots, following Cont, Kukanov & Stoikov (2014):
//
//	e_n = ΔV^bid − ΔV^ask
//
// where, comparing previous (P, Q) to current (P', Q') at the best level:
//
//	ΔV^bid =  Q'          if P' > P   (bid stepped up: fresh buy size)
//	          Q' − Q      if P' = P   (same level: net change)
//	         −Q           if P' < P   (bid stepped down: size pulled)
//
//	ΔV^ask = −Q           if P' > P   (ask stepped up: size pulled)
//	          Q' − Q      if P' = P
//	          Q'          if P' < P   (ask stepped down: fresh sell size)
//
// Positive e_n reflects net buy pressure at the touch, negative net sell
// pressure. A side missing from either snapshot contributes 0. Quantities are
// integer lots, so e_n is exact.
func OFIStep(prev, cur *orderbook.Snapshot) int64 {
	var e int64
	if pb, ok := bestBid(prev); ok {
		if cb, ok := bestBid(cur); ok {
			e += bidDelta(pb, cb)
		}
	}
	if pa, ok := bestAsk(prev); ok {
		if ca, ok := bestAsk(cur); ok {
			e -= askDelta(pa, ca)
		}
	}
	return e
}

func bidDelta(prev, cur orderbook.SnapshotLevel) int64 {
	switch {
	case cur.Price > prev.Price:
		return cur.Quantity
	case cur.Price == prev.Price:
		return cur.Quantity - prev.Quantity
	default:
		return -prev.Quantity
	}
}

func askDelta(prev, cur orderbook.SnapshotLevel) int64 {
	switch {
	case cur.Price > prev.Price:
		return -prev.Quantity
	case cur.Price == prev.Price:
		return cur.Quantity - prev.Quantity
	default:
		return cur.Quantity
	}
}

func bestBid(s *orderbook.Snapshot) (orderbook.SnapshotLevel, bool) {
	if s == nil || len(s.Bids) == 0 {
		return orderbook.SnapshotLevel{}, false
	}
	return s.Bids[0], true
}

func bestAsk(s *orderbook.Snapshot) (orderbook.SnapshotLevel, bool) {
	if s == nil || len(s.Asks) == 0 {
		return orderbook.SnapshotLevel{}, false
	}
	return s.Asks[0], true
}

// OFI accumulates order-flow imbalance across a stream of snapshots. Feed it
// successive snapshots with Observe; it maintains the previous observation and a
// running cumulative OFI (the windowed quantity that the CKS regression uses
// against price change). Not safe for concurrent use.
type OFI struct {
	prev *orderbook.Snapshot
	cum  int64
}

// NewOFI returns an empty accumulator.
func NewOFI() *OFI { return &OFI{} }

// Observe folds one snapshot into the accumulator and returns the per-step e_n.
// The very first snapshot only primes the previous state and returns 0.
func (o *OFI) Observe(cur *orderbook.Snapshot) float64 {
	if o.prev == nil {
		o.prev = cur
		return 0
	}
	e := OFIStep(o.prev, cur)
	o.cum += e
	o.prev = cur
	return float64(e)
}

// Cumulative returns the running sum of e_n since the last Reset.
func (o *OFI) Cumulative() float64 { return float64(o.cum) }

// CumulativeExact returns the running sum as an exact integer (lots).
func (o *OFI) CumulativeExact() int64 { return o.cum }

// Reset clears the previous observation and cumulative total.
func (o *OFI) Reset() {
	o.prev = nil
	o.cum = 0
}
