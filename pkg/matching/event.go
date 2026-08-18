package matching

import "github.com/intrepidkarthi/orderbook/pkg/types"

// EventKind is the type of an emitted engine event. Accepted/Trade/Canceled/
// Replaced form an ITCH-style add/execute/delete/modify stream that reconstructs
// the L3 book; Rejected reports refused orders. Triggered and BookDelta are
// reserved for future emission.
//
// The reconstruction claim is machine-checked on GENERATED input, which is the
// load-bearing half of this sentence. TestDifferentialTape replays the stream into
// a mirror book and compares it against the engine's own book after every one of
// the 2,240 commands of its 16 tapes; TestEventStreamReconstructsBook does the same
// over a hand-written scenario list, one per order class — limits, market and IOC
// remainders, FOK reversal plain, crossed with self-trade prevention, and crossed
// with an iceberg whose reserve it exhausts, post-only, stops resting, cascade-fired,
// and cascade-fired-then-refused, trailing stops, OCO in both directions, iceberg
// refill and exhaustion, and all five STP modes.
//
// The last two of those are new, and they are new because an exclusion is not closed
// by deleting the sentence that stated it. Each of the two paths this comment used to
// exclude now has a scenario ON THE LIST THIS COMMENT CITES, and each fails against
// the engine before its fix: the iceberg one with the mirror holding 100 lots and the
// engine 300, the cascade one with the mirror holding a 50-lot phantom at 200 the
// engine does not have.
//
// The citation moved because the scenario list ALONE proved this claim true while it
// was false. None of its ~25 scenarios combined fill-or-kill with self-trade
// prevention, which is exactly the combination in which a rejected order's batch was
// dropped and the book lost a maker with nothing on the stream to say so — an
// exhaustive check over an incomplete input space reporting completeness, with the
// report load-bearing (docs/JOURNAL-COMPLETENESS.md §1,
// docs/DIFFERENTIAL-FINDINGS.md §4.6). The generated-path check is what would have
// caught it without anyone predicting it.
//
// The claim is not unconditional and this comment will not say it is, but the two
// exclusions it used to carry are gone rather than restated. A fill-or-kill that
// exhausts an iceberg's reserve and then fails no longer leaves the engine holding an
// order whose displayed size no stream can explain, and a stop fired by a cascade
// whose order is then refused no longer ends in silence: emitTerminalIfDone closes it
// out with a CANCELED carrying the reason. Both were pinned here, both are fixed, and
// the tests that pinned them now assert the opposite under the same names
// (TestFailingFOKCorruptsAnIcebergsReserve, TestCascadeFiredStopRejectedLeavesAPhantom;
// docs/PINNED-DEFECTS.md). An exclusion left standing after its cause is gone is a lie
// in the opposite direction.
//
// ONE condition remains, and it is a condition on the READER rather than an exclusion
// from the claim: the claim holds for a consumer that ignores an Accepted whose
// quantity is zero. Self-trade prevention under DECREMENT can empty an order
// inside the command that created it, and the venue announces it anyway because it was
// accepted before it was emptied. Nothing further is ever published about that order.
// The mirror in runDiff applies this rule, which is why the generated-path check
// passes; a consumer that does not apply it holds a zero-lot phantom forever, and the
// proof on these lines does not cover it. Stated for clients in
// docs/PROTOCOL.md, and open as a question about whether the engine should announce
// such an order at all in docs/DIFFERENTIAL-FINDINGS.md §11.3.
//
// Do not weaken either test to make a change pass; the claim on these lines is only
// worth what they prove.
type EventKind uint8

const (
	EventAccepted   EventKind = iota // order entered the book (rested, or began filling)
	EventRejected                    // order refused (with Reason)
	EventTrade                       // an execution (a fill)
	EventCanceled                    // order removed: cancelled, or terminated without resting
	EventTriggered                   // a stop or trailing stop fired and became a live order
	EventReplaced                    // order changed size in place, keeping queue position
	EventBookDelta                   // DEPRECATED and never emitted; see the note below
	EventHalted                      // engine entered Halted (operator, guardrail trip, or band-breach pause)
	EventResumed                     // engine returned to Open (manual resume, or a band-breach pause elapsed)
	EventCancelOnly                  // engine entered cancel-only: cancels accepted, new liquidity refused
	EventBusted                      // a previously published trade is annulled; the book is NOT rewound

	// eventKindCount is a sentinel: keep it last, and unexported so it never reaches
	// a wire, a file or the frozen surface.
	//
	// It is the entryKindCount treatment (pkg/wal/wal.go) applied to the second enum
	// that needed it. internal/semcheck's corpus guard asserts that the fingerprint
	// run REACHED every event kind, and a guard that enumerates a hand-written list
	// reports coverage of the list rather than of the enum: a kind added here would
	// be invisible to it forever. With this sentinel a new kind fails the guard the
	// moment it is declared, which is the whole point of declaring it.
	//
	// EventBookDelta is the one named exception, because it is declared, deprecated
	// and deliberately never emitted — see the note below.
	eventKindCount
)

func (k EventKind) String() string {
	switch k {
	case EventAccepted:
		return "ACCEPTED"
	case EventRejected:
		return "REJECTED"
	case EventTrade:
		return "TRADE"
	case EventCanceled:
		return "CANCELED"
	case EventTriggered:
		return "TRIGGERED"
	case EventReplaced:
		return "REPLACED"
	case EventCancelOnly:
		return "CANCEL_ONLY"
	case EventBusted:
		return "BUSTED"
	case EventBookDelta:
		return "BOOK_DELTA"
	case EventHalted:
		return "HALTED"
	case EventResumed:
		return "RESUMED"
	default:
		return "UNKNOWN"
	}
}

// Event is one entry in the engine's ordered event stream. Seq is a global
// monotonic engine sequence number — the linchpin adapters map onto market-data
// / drop-copy sequence spaces and use for gap detection, resync, and recovery.
//
// The Order and Trade pointers reference engine- or caller-owned memory that is
// valid only for the duration of the OnEvents call. Copy anything you need to
// retain — do not hold the pointers past the callback.
type Event struct {
	Seq     int64        // monotonic engine event sequence
	Kind    EventKind    // what happened
	OrderID int64        // the subject order (taker for trades; 0 if unassigned)
	UserID  string       // owner of the subject order (empty for trades)
	Order   *types.Order // set for Accepted/Rejected/Canceled
	Trade   *types.Trade // set for Trade
	Reason  error        // set for Rejected
	// TradeID names the annulled print of an EventBusted, and is zero on every
	// other kind — including EventTrade, where the id is on the Trade itself.
	//
	// The asymmetry is the point rather than an oversight. A trade event carries
	// the trade, so the id is reachable; a bust arrives long after, and the engine
	// does not retain the trades it printed, so an id is the only thing it can
	// honestly say. Copying it onto EventTrade as well would give consumers two
	// fields to keep in agreement for no new information.
	TradeID int64
	// BustReason is the operator's free-text reason for an EventBusted (empty
	// elsewhere). It is for humans and audit trails; do not switch on it.
	BustReason string
}

// EventBookDelta is retained only so the numbering of the kinds after it does not
// shift, and it is never emitted.
//
// It was reserved for an aggregated L2 level change, and the engine is the wrong
// place to produce one. L2 is a pure function of L3, and this stream is tested to
// reconstruct the L3 book exactly (TestDifferentialTape's per-command mirror check
// on generated tapes, plus TestEventStreamReconstructsBook's scenario list) —
// so a consumer that wants level deltas can derive them, and marketdata.NewL2Feed
// does. Emitting them from the matching goroutine would add work to the hot path to
// compute something a consumer can compute for itself, off it.
//
// Left declared rather than deleted because a consumer may have persisted these
// values, and removing a constant from the middle of an iota block silently
// renumbers every kind after it.

// EventSink receives batches of engine events in strict Seq order. Implementations
// MUST return quickly and MUST NOT block the matching goroutine — push the batch
// into a ring buffer or channel and return. The events slice and its pointers are
// reused after the call, so copy anything you retain. A nil sink (the default)
// disables emission entirely, keeping the hot path zero-overhead.
//
// Events within a batch are in the order things actually happened: a trade is
// recorded as it executes, so an iceberg slice reloading mid-sweep is announced
// between the trade that emptied the previous slice and the trade that hits the
// new one. Sequence numbers are assigned when the batch is published, so Seq is
// gap-free and monotonic across the whole stream.
//
// OnEvents may be called more than once per submitted command — a guardrail trip
// or a band-breach pause publishes a state change mid-match. Do not build a
// consumer on a one-callback-per-command model.
//
// A REJECTED command's batch is not necessarily one event. A rejection drops only
// the events describing state the engine actually undid — the reversed prints of a
// failed fill-or-kill, and the Accepted announcing a refilled iceberg slice the same
// failure put back — and everything else that walk did stands and is published after
// the Rejected: a maker cancelled or shrunk by self-trade prevention, an OCO leg
// cancelled. A consumer must apply them.
// docs/DIFFERENTIAL-FINDINGS.md §4, docs/PINNED-DEFECTS.md §3.
type EventSink interface {
	OnEvents(events []Event)
}

// MultiSink fans one event stream out to several sinks in order. Config.EventSink
// is a single slot consumed once at construction, so without this, attaching a
// publisher would silently displace whatever sink an embedder already had —
// their audit trail or drop copy would simply stop.
//
// It inherits the EventSink contract: every sink must return promptly, and the
// slice and its pointers are reused after the call, so each sink copies anything
// it retains. A nil entry is skipped.
type MultiSink []EventSink

func (m MultiSink) OnEvents(events []Event) {
	for _, s := range m {
		if s != nil {
			s.OnEvents(events)
		}
	}
}
