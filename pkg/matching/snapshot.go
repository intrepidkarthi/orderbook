package matching

import (
	"fmt"
	"sort"
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
// Also captured, because each is recovered state rather than a live-only
// convenience: the client-order-id duplicate guard; iceberg hidden reserves and
// refill counts; OCO leg pairings; trailing stops with their ratchet; the
// external mark price; and the self-output guardrail's window accounting.
//
// Most of those were previously omitted, which made restore quietly lossy in
// several directions at once — a trailing stop vanished outright (it lives only
// in the engine's map, never in the book), an iceberg came back as a bare
// displayed slice with its reserve gone, an OCO pair came back as two
// independent orders either of which could fill, a zero mark price let the first
// post-recovery mark update through both manipulation clamps, and a reset
// guardrail window handed back a full budget at exactly the wrong moment.
type EngineSnapshot struct {
	Symbol         string
	Seq            int64 // engine order sequence as of the snapshot
	TradeSeq       int64
	EventSeq       int64
	LastTradePrice int64
	MarkPrice      int64 // external mark/index reference (0 => use last trade)
	State          EngineState
	PausedUntil    time.Time          // active band-breach pause deadline (zero if none)
	Orders         []*types.Order     // resting book, price-then-time order
	Stops          []*types.StopOrder // pending stops
	DedupKeys      []string           // accepted client-order-id keys, oldest first
	Icebergs       []IcebergEntry     // hidden reserves behind resting displayed slices
	OCOs           []OCOEntry         // leg pairings, by id
	Trailing       []TrailingEntry    // pending trailing stops, carried whole
	Guard          GuardWindow        // self-output guardrail window accounting
}

// GuardWindow is the in-flight state of the self-output guardrail's rolling
// window. Restoring it matters because a reset window is an open one: a restart
// would otherwise hand back a full trade and notional budget immediately, which
// is the opposite of what a guardrail that has just tripped should do.
type GuardWindow struct {
	Start    time.Time
	Trades   int
	Notional int64
}

// IcebergEntry is an iceberg's reserve, keyed by the id of its displayed slice.
// The slice itself is in Orders; on restore the two are re-linked by id so the
// engine's map and the book share one *types.Order, which is what makes a fill
// against the slice refill the reserve.
type IcebergEntry struct {
	OrderID    int64
	DisplayQty int64
	Hidden     int64
	JitterBps  int64
	Refills    int64
}

// OCOEntry is a one-cancels-other pairing. Both legs are already in Orders and
// Stops; only the association between them needs carrying, and losing it is what
// let both legs fill independently after a restore.
type OCOEntry struct {
	PrimaryID int64
	StopID    int64
}

// TrailingEntry is a pending trailing stop. Unlike icebergs and OCO legs, a
// trailing stop is held only in the engine's map and never rests in the book or
// stop book, so the order is carried whole here rather than referenced by id.
type TrailingEntry struct {
	Order *types.Order
	Trail int64
	State types.TrailingState
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
		MarkPrice:      e.markPrice,
		State:          e.state,
		PausedUntil:    e.pausedUntil,
		Guard:          GuardWindow{Start: e.windowStart, Trades: e.windowTrades, Notional: e.windowNotional},
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
	snap.DedupKeys = e.dedupKeysChronological()

	for id, ib := range e.icebergOrders {
		snap.Icebergs = append(snap.Icebergs, IcebergEntry{
			OrderID:    id,
			DisplayQty: ib.DisplayQty,
			Hidden:     ib.Hidden,
			JitterBps:  ib.JitterBps,
			Refills:    ib.Refills(),
		})
	}
	sort.Slice(snap.Icebergs, func(i, j int) bool { return snap.Icebergs[i].OrderID < snap.Icebergs[j].OrderID })

	// Both legs map to the same pair, so dedupe on the primary's id.
	seenOCO := make(map[int64]struct{}, len(e.ocoByOrderID))
	for _, oco := range e.ocoByOrderID {
		if _, dup := seenOCO[oco.Primary.ID]; dup {
			continue
		}
		seenOCO[oco.Primary.ID] = struct{}{}
		snap.OCOs = append(snap.OCOs, OCOEntry{PrimaryID: oco.Primary.ID, StopID: oco.Stop.Order.ID})
	}
	sort.Slice(snap.OCOs, func(i, j int) bool { return snap.OCOs[i].PrimaryID < snap.OCOs[j].PrimaryID })

	for _, ts := range e.trailingStops {
		snap.Trailing = append(snap.Trailing, TrailingEntry{
			Order: copyOrder(ts.Order),
			Trail: ts.Trail,
			State: ts.State(),
		})
	}
	sort.Slice(snap.Trailing, func(i, j int) bool { return snap.Trailing[i].Order.ID < snap.Trailing[j].Order.ID })

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
	// Keep the pointers we actually inserted: the iceberg and OCO registries must
	// share one *types.Order with the book, or a fill against a displayed slice
	// would not refill its reserve and a filled OCO leg would not cancel its twin.
	restedOrders := make(map[int64]*types.Order, len(snap.Orders))
	for _, o := range snap.Orders {
		c := copyOrder(o)
		if err := e.book.Add(c); err != nil {
			return fmt.Errorf("LoadSnapshot: re-adding order %d: %w", o.ID, err)
		}
		restedOrders[c.ID] = c
	}
	restedStops := make(map[int64]*types.StopOrder, len(snap.Stops))
	for _, s := range snap.Stops {
		ns, err := types.NewStopOrder(copyOrder(s.Order), s.StopPrice)
		if err != nil {
			return fmt.Errorf("LoadSnapshot: re-adding stop %d: %w", s.Order.ID, err)
		}
		e.stopBook.Add(ns)
		restedStops[ns.Order.ID] = ns
	}
	if snap.LastTradePrice > 0 {
		e.book.SetLastTradePrice(snap.LastTradePrice)
	}
	for _, k := range snap.DedupKeys {
		e.recordDedupKey(k) // oldest first, so a smaller ring keeps the newest
	}

	for _, ib := range snap.Icebergs {
		o, ok := restedOrders[ib.OrderID]
		if !ok {
			return fmt.Errorf("LoadSnapshot: iceberg %d has no resting displayed slice", ib.OrderID)
		}
		r := &types.IcebergOrder{Order: o, DisplayQty: ib.DisplayQty, Hidden: ib.Hidden, JitterBps: ib.JitterBps}
		r.RestoreRefills(ib.Refills)
		e.icebergOrders[ib.OrderID] = r
	}
	for _, pair := range snap.OCOs {
		primary, ok := restedOrders[pair.PrimaryID]
		if !ok {
			return fmt.Errorf("LoadSnapshot: OCO primary %d is not resting", pair.PrimaryID)
		}
		stop, ok := restedStops[pair.StopID]
		if !ok {
			return fmt.Errorf("LoadSnapshot: OCO stop %d is not pending", pair.StopID)
		}
		oco := &types.OCOOrder{Primary: primary, Stop: stop}
		e.ocoByOrderID[pair.PrimaryID] = oco
		e.ocoByOrderID[pair.StopID] = oco
	}
	for _, te := range snap.Trailing {
		ts, err := types.NewTrailingStop(copyOrder(te.Order), te.Trail)
		if err != nil {
			return fmt.Errorf("LoadSnapshot: re-adding trailing stop %d: %w", te.Order.ID, err)
		}
		ts.RestoreState(te.State)
		e.trailingStops[ts.Order.ID] = ts
	}

	e.orderSeq = snap.Seq
	e.tradeSeq = snap.TradeSeq
	e.eventSeq = snap.EventSeq
	e.markPrice = snap.MarkPrice
	e.windowStart = snap.Guard.Start
	e.windowTrades = snap.Guard.Trades
	e.windowNotional = snap.Guard.Notional
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
