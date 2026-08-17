package matching

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	Symbol   string
	Seq      int64 // engine order sequence as of the snapshot
	TradeSeq int64
	EventSeq int64
	// WALSeq is the command-log sequence of the last command APPLIED to the engine
	// when this snapshot was taken, so recovery can replay only the tail after it.
	// It is the applied sequence, not the appended one: the log is written
	// write-ahead, so an entry can exist on disk that the engine has not yet
	// processed, and treating the writer's latest sequence as the checkpoint would
	// skip exactly those commands. Zero means unknown — replay the whole log.
	WALSeq int64
	// Semantics is SemanticsVersion of the build that produced this state. Zero
	// means a snapshot written before the stamp existed.
	//
	// It is a REPORT and never a gate. A snapshot is not replayed, so restoring a
	// book an older build actually had into a newer engine is the documented upgrade
	// procedure rather than an error; refusing on it would refuse the procedure.
	//
	// It is excluded from Digest for the same reason WALSeq is — it is provenance,
	// not book state — and for a second reason that decides it: the enforcement in
	// internal/semcheck fingerprints snapshot digests, so a Semantics inside the
	// digest would make every bump satisfy its own evidence and the forcing function
	// would be circular. See docs/SEMANTICS-VERSION.md §2.4.
	Semantics      int `json:"semantics,omitempty"`
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
	// Busted is the annulled-print registry, in trade-id order. It is in the
	// snapshot, and therefore in the Digest, because two engines that applied the
	// same commands are only equal if they also agree on what settled — a bust
	// registry outside the digest would let a primary and its follower disagree
	// about which trades stand and still compare identical.
	//
	// It grows without bound by construction. That is tolerable because busting is
	// rare and each record is small, and it is stated here rather than discovered:
	// a venue that busts routinely needs a retention rule, and this is where it
	// would go. See docs/TRADE-BUST.md §6.
	Busted []BustRecord
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
		Symbol: e.config.Symbol,
		// Stamped from the constant rather than from a literal, so the field cannot
		// drift away from the number the log gate compares against.
		Semantics:      SemanticsVersion,
		Seq:            e.orderSeq,
		TradeSeq:       e.tradeSeq,
		EventSeq:       e.eventSeq,
		LastTradePrice: e.book.LastTradePrice(),
		MarkPrice:      e.markPrice,
		State:          e.state,
		PausedUntil:    e.pausedUntil,
		Guard:          GuardWindow{Start: e.windowStart, Trades: e.windowTrades, Notional: e.windowNotional},
		Busted:         e.bustRecords(),
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
	e.restoreBusts(snap.Busted)
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

// Digest fingerprints everything recovery must reproduce — the resting book in
// price-time order, pending stops, iceberg reserves, OCO pairings, trailing
// stops with their ratchet, the duplicate guard, the mark price, the engine
// state, and all three sequence counters. Two engines that applied the same
// commands have equal digests; two engines whose books have diverged do not.
// It exists so that "the follower has the same book" is a comparison either
// side of a wire can actually make — the recovery tests' assertion, promoted
// from a test helper to a contract (see docs/REPLICATION.md).
//
// Wall-clock fields are normalised away: order timestamps, the band-breach
// pause deadline, the guardrail window start, and the instant a bust was
// recorded. A replayed order is stamped with the clock at replay time, not the
// clock at original submission, so an engine rebuilt from the log is legitimately
// not byte-identical in its timestamps — a property of replaying a command log,
// not a divergence. A bust replayed from the log tail is stamped the same way,
// and a bust restored from a snapshot keeps its original instant, so the two
// paths disagree on At by construction; WHICH trades are busted is the part that
// must match, and that is what the digest compares. WALSeq is excluded too: it is
// a log position, not book state, and a follower's checkpoint cadence is its own
// business.
//
// The digest is a COMPARISON fingerprint, not a commitment: it hashes the
// snapshot's JSON encoding, so it is stable between processes running the same
// release and nothing stronger. Renaming a field or reordering the struct
// changes every digest, deliberately without ceremony — pinning a canonical
// encoding would turn every snapshot change into a wire-format negotiation,
// for a value whose only job is to be equal or not equal right now.
//
// The receiver is not modified. The snapshot's own deep-copy guarantee is what
// makes that cheap to promise: Orders, Stops and Trailing already own their
// orders, so normalising onto shallow copies here cannot reach live engine
// state.
func (s *EngineSnapshot) Digest() string {
	n := *s
	n.WALSeq = 0
	// Semantics is normalised away for two reasons, and the second is the one that
	// decides it. Two engines whose books are identical must compare equal even when
	// one of them is a build ahead, or the digest stops being usable for the thing it
	// exists for. And internal/semcheck's fingerprint contains snapshot digests, so a
	// Semantics inside the digest would let every bump satisfy its own evidence —
	// bump, fingerprint moves, regenerate, done — and the rule that a bump with no
	// observable change is refused would be unenforceable. See
	// docs/SEMANTICS-VERSION.md §2.4.
	n.Semantics = 0
	n.PausedUntil = time.Time{}
	n.Guard.Start = time.Time{}
	n.Orders = normalisedOrders(s.Orders)
	n.Stops = make([]*types.StopOrder, 0, len(s.Stops))
	for _, st := range s.Stops {
		c := *st
		c.Order = normalisedOrder(st.Order)
		n.Stops = append(n.Stops, &c)
	}
	n.Trailing = make([]TrailingEntry, len(s.Trailing))
	for i, te := range s.Trailing {
		te.Order = normalisedOrder(te.Order)
		n.Trailing[i] = te
	}
	if len(s.Busted) > 0 {
		n.Busted = make([]BustRecord, len(s.Busted))
		for i, b := range s.Busted {
			b.At = time.Time{}
			n.Busted[i] = b
		}
	}
	b, err := json.Marshal(&n)
	if err != nil {
		// Every field of EngineSnapshot is plain data; an error here is a defect
		// in this package (a func or channel added to the snapshot), not a
		// runtime condition a caller could handle.
		panic(fmt.Sprintf("matching: EngineSnapshot not marshalable: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func normalisedOrders(orders []*types.Order) []*types.Order {
	out := make([]*types.Order, 0, len(orders))
	for _, o := range orders {
		out = append(out, normalisedOrder(o))
	}
	return out
}

func normalisedOrder(o *types.Order) *types.Order {
	if o == nil {
		return nil
	}
	c := *o
	c.CreatedAt = time.Time{}
	c.UpdatedAt = time.Time{}
	return &c
}
