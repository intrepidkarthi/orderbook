package matching

import (
	"errors"
	"sort"
	"time"
)

// Trade bust: annulling a print that has already been published.
//
// The design and the reasoning behind every restriction here are in
// docs/TRADE-BUST.md. The short version, because it is the part that surprises
// people: a bust does not rewind anything. The book at bust time is not the book
// at trade time, and no amount of care makes it so.

// ErrUnknownTrade reports a bust of a trade id this engine never issued.
var ErrUnknownTrade = errors.New("matching: unknown trade id")

// ErrAlreadyBusted reports a second bust of the same trade. It is an error rather
// than a no-op deliberately: see Engine.Bust.
var ErrAlreadyBusted = errors.New("matching: trade already busted")

// BustRecord is why a print was annulled, retained so a subscriber that joins
// afterwards can still be told. Reason is free text for humans and audit trails —
// the engine never interprets it.
type BustRecord struct {
	TradeID int64     `json:"trade_id"`
	Reason  string    `json:"reason,omitempty"`
	At      time.Time `json:"at"`
}

// Bust annuls a published trade: it records that the print will not settle and
// emits EventBusted. It does NOT touch the book, and the four things it
// deliberately leaves alone are set out in docs/TRADE-BUST.md §2 — the orders are
// not re-rested, the stops the print fired stay fired, LastTradePrice is not
// rewound, and the event that reported the trade is not amended. A bust arrives
// after the market has moved, and each of those "undos" would be a second wrong
// rather than a correction of the first.
//
// Validation is identity-only. The engine knows how many trades it has printed, so
// an id it never issued is ErrUnknownTrade — but it does not retain the trades
// themselves (a venue's uptime multiplied by its print rate is not a thing to hold
// in memory), so it cannot check price, size or counterparty. Whoever kept the tape
// validates the economics; this call will not pretend it did.
//
// A repeat bust of the same trade is ErrAlreadyBusted rather than a silent no-op,
// which departs from the Halt transition rule on purpose: a duplicate halt is an
// operator being redundant, a duplicate bust is usually an operator busting the
// wrong id, and a venue annulling trades is the last place to swallow that.
//
// Call from the single writer, or via Runner.Bust so it is journalled.
func (e *Engine) Bust(tradeID int64, reason string) error {
	// Validation splits the id rather than comparing it whole, which is what makes
	// this correct at a multi-symbol venue: a trade id carries the shard that
	// issued it, so a bust aimed at another symbol's print is refused here instead
	// of annulling whichever local trade happened to share the low bits.
	shard, seq := SplitID(tradeID)
	if tradeID <= 0 || shard != e.config.ShardIndex || seq <= 0 || seq > e.tradeSeq {
		return ErrUnknownTrade
	}
	if _, dup := e.busted[tradeID]; dup {
		return ErrAlreadyBusted
	}
	if e.busted == nil {
		e.busted = make(map[int64]BustRecord)
	}
	e.busted[tradeID] = BustRecord{TradeID: tradeID, Reason: reason, At: e.clock()}
	e.emitBust(tradeID, reason)
	return nil
}

// IsBusted reports whether a trade has been annulled.
func (e *Engine) IsBusted(tradeID int64) bool {
	_, ok := e.busted[tradeID]
	return ok
}

// BustCount reports how many trades have been annulled, for operators and metrics.
func (e *Engine) BustCount() int { return len(e.busted) }

// bustRecords returns the registry in trade-id order. Sorted because it goes into
// the snapshot and therefore the digest, and Go's map iteration order is random:
// an unsorted slice would make two engines in identical states digest differently
// on most runs, which is the exact failure the digest exists to detect.
func (e *Engine) bustRecords() []BustRecord {
	if len(e.busted) == 0 {
		return nil
	}
	out := make([]BustRecord, 0, len(e.busted))
	for _, r := range e.busted {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TradeID < out[j].TradeID })
	return out
}

// restoreBusts rebuilds the registry from a snapshot.
func (e *Engine) restoreBusts(records []BustRecord) {
	if len(records) == 0 {
		e.busted = nil
		return
	}
	e.busted = make(map[int64]BustRecord, len(records))
	for _, r := range records {
		e.busted[r.TradeID] = r
	}
}

// emitBust publishes the annulment on the same ordered stream as the print it
// annuls. Appended, never a rewrite: the tape a follower replays has to stay
// byte-identical to the tape the primary produced, which rules out going back and
// amending the EventTrade.
func (e *Engine) emitBust(tradeID int64, reason string) {
	if e.sink == nil {
		return
	}
	e.eventSeq++
	e.eventBuf = append(e.eventBuf[:0], Event{
		Seq: e.eventSeq, Kind: EventBusted, TradeID: tradeID, BustReason: reason,
	})
	e.sink.OnEvents(e.eventBuf)
}
