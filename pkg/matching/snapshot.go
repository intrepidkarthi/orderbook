package matching

import (
	"fmt"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// EngineSnapshot is a point-in-time, sequence-keyed copy of an engine's state,
// sufficient to restore it and resume by replaying the command log recorded after
// Seq — the standard bootstrap/recovery primitive (a dYdX validator loads a
// snapshot then applies later blocks; a venue loads a snapshot then replays its
// WAL tail). Taking snapshots bounds replay time to O(recent) instead of
// O(all-history).
//
// It captures the resting book, the pending stops, and the engine counters. All
// contained orders are deep copies, so the snapshot is independent of the live
// engine and is JSON/gob-encodable.
//
// Not captured: iceberg hidden reserves, trailing-stop ratchet state, and OCO
// pairings (their internal state is private to package types). Those conditional
// orders are recovered by replaying the command log rather than from the
// snapshot; take snapshots at points where they are quiescent, or replay from an
// earlier snapshot.
type EngineSnapshot struct {
	Symbol         string
	Seq            int64 // engine order sequence as of the snapshot
	TradeSeq       int64
	EventSeq       int64
	LastTradePrice int64
	State          EngineState
	PausedUntil    time.Time          // active band-breach pause deadline (zero if none)
	Orders         []*types.Order     // resting book, price-then-time order
	Stops          []*types.StopOrder // pending stops
}

func copyOrder(o *types.Order) *types.Order {
	c := *o
	return &c
}

// TakeSnapshot captures the engine's current state as a restorable, sequence-keyed
// snapshot (see EngineSnapshot for what is and isn't captured).
func (e *Engine) TakeSnapshot() *EngineSnapshot {
	snap := &EngineSnapshot{
		Symbol:         e.config.Symbol,
		Seq:            e.orderSeq,
		TradeSeq:       e.tradeSeq,
		EventSeq:       e.eventSeq,
		LastTradePrice: e.book.LastTradePrice(),
		State:          e.state,
		PausedUntil:    e.pausedUntil,
	}
	for _, o := range e.book.Orders() {
		snap.Orders = append(snap.Orders, copyOrder(o))
	}
	for _, s := range e.stopBook.All() {
		// Deep-copy so the snapshot is independent of the live engine (a pending
		// stop has triggered=false, faithfully reconstructed by NewStopOrder).
		ns, err := types.NewStopOrder(copyOrder(s.Order), s.StopPrice)
		if err != nil {
			continue // unreachable for a resting stop (order non-nil, price > 0)
		}
		snap.Stops = append(snap.Stops, ns)
	}
	return snap
}

// LoadSnapshot restores a snapshot into this engine, which must be fresh (empty
// book, no orders processed). Afterwards the engine is byte-identical to the one
// the snapshot was taken from, and continues assigning ids after Seq — so
// replaying the command log recorded after the snapshot reproduces the live
// engine exactly.
func (e *Engine) LoadSnapshot(snap *EngineSnapshot) error {
	if e.orderSeq != 0 || e.book.Count() != 0 {
		return fmt.Errorf("LoadSnapshot: engine is not fresh (orderSeq=%d, resting=%d)", e.orderSeq, e.book.Count())
	}
	if snap.Symbol != e.config.Symbol {
		return fmt.Errorf("LoadSnapshot: symbol mismatch (snapshot %q vs engine %q)", snap.Symbol, e.config.Symbol)
	}
	for _, o := range snap.Orders {
		if err := e.book.Add(copyOrder(o)); err != nil {
			return fmt.Errorf("LoadSnapshot: re-adding order %d: %w", o.ID, err)
		}
	}
	for _, s := range snap.Stops {
		ns, err := types.NewStopOrder(copyOrder(s.Order), s.StopPrice)
		if err != nil {
			return fmt.Errorf("LoadSnapshot: re-adding stop %d: %w", s.Order.ID, err)
		}
		e.stopBook.Add(ns)
	}
	if snap.LastTradePrice > 0 {
		e.book.SetLastTradePrice(snap.LastTradePrice)
	}
	e.orderSeq = snap.Seq
	e.tradeSeq = snap.TradeSeq
	e.eventSeq = snap.EventSeq
	e.state = snap.State
	e.pausedUntil = snap.PausedUntil
	return nil
}

// RestoreEngine builds a fresh engine from config and loads snap into it. If
// config.Symbol is empty it is taken from the snapshot.
func RestoreEngine(config Config, snap *EngineSnapshot) (*Engine, error) {
	if config.Symbol == "" {
		config.Symbol = snap.Symbol
	}
	e := NewEngine(config)
	if err := e.LoadSnapshot(snap); err != nil {
		return nil, err
	}
	return e, nil
}
