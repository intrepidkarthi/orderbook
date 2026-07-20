// Package marketdata models the input side of a market — order flow — and the
// tools to record and replay it deterministically.
//
// The engine guarantees that the same ordered input stream produces byte-
// identical trades and book state (docs/SPEC.md §6.4). This package makes that
// guarantee usable: record the order flow that drove one run, then Replay it
// through a fresh engine and confirm the outcome reproduces. Digest turns a
// trade sequence into a stable fingerprint for golden-file comparisons.
package marketdata

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Stream is a replayable sequence of order-flow "templates". Each entry holds an
// order in its initial (unfilled) state; replaying clones it so the stream can
// be reused across runs without carrying prior fill state.
type Stream struct {
	orders []*types.Order
}

// NewStream builds a stream from the given orders (snapshotted to their initial
// state).
func NewStream(orders ...*types.Order) *Stream {
	s := &Stream{}
	for _, o := range orders {
		s.Append(o)
	}
	return s
}

// Append adds an order to the stream, capturing its initial state.
func (s *Stream) Append(o *types.Order) { s.orders = append(s.orders, o.Fresh()) }

// Len returns the number of orders in the stream.
func (s *Stream) Len() int { return len(s.orders) }

// Orders returns fresh copies of the stream's orders (safe to submit/mutate).
func (s *Stream) Orders() []*types.Order {
	out := make([]*types.Order, len(s.orders))
	for i, o := range s.orders {
		out[i] = o.Fresh()
	}
	return out
}

// Replay submits a fresh copy of every order in the stream through eng, in order,
// and returns all trades produced.
func Replay(eng *matching.Engine, s *Stream) []*types.Trade {
	var trades []*types.Trade
	for _, o := range s.orders {
		res := eng.Process(o.Fresh())
		trades = append(trades, res.Trades...)
	}
	return trades
}

// Recorder wraps a matching engine, forwarding orders to it while recording the
// order flow (as a replayable Stream) and the resulting trade tape.
type Recorder struct {
	eng    *matching.Engine
	stream *Stream
	trades []*types.Trade
}

// NewRecorder returns a recorder over eng.
func NewRecorder(eng *matching.Engine) *Recorder {
	return &Recorder{eng: eng, stream: &Stream{}}
}

// Process records the order (in its pre-match state) and forwards it to the
// engine, appending any resulting trades to the tape.
func (r *Recorder) Process(o *types.Order) *matching.MatchResult {
	r.stream.Append(o) // Append snapshots initial state before matching mutates o
	res := r.eng.Process(o)
	r.trades = append(r.trades, res.Trades...)
	return res
}

// Stream returns the recorded order flow.
func (r *Recorder) Stream() *Stream { return r.stream }

// Trades returns the recorded trade tape.
func (r *Recorder) Trades() []*types.Trade { return r.trades }

// Digest returns a stable hex fingerprint of a trade sequence, using only the
// deterministic, semantically meaningful fields (sequence, price, quantity,
// taker side, maker/taker order ids) — not the per-run UUID trade id or wall
// clock. Two runs of the same input stream yield the same digest.
func Digest(trades []*types.Trade) string {
	var b strings.Builder
	for _, t := range trades {
		b.WriteString(strconv.FormatUint(t.SequenceNum, 10))
		b.WriteByte('|')
		b.WriteString(t.Price.String())
		b.WriteByte('|')
		b.WriteString(t.Quantity.String())
		b.WriteByte('|')
		b.WriteString(string(t.TakerSide))
		b.WriteByte('|')
		b.WriteString(t.MakerOrderID)
		b.WriteByte('|')
		b.WriteString(t.TakerOrderID)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
