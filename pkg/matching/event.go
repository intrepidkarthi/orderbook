package matching

import "github.com/intrepidkarthi/orderbook/pkg/types"

// EventKind is the type of an emitted engine event. Accepted/Trade/Canceled/
// Replaced form an ITCH-style add/execute/delete/modify stream that reconstructs
// the L3 book; Rejected reports refused orders. Triggered and BookDelta are
// reserved for future emission.
//
// The reconstruction claim is machine-checked: TestEventStreamReconstructsBook
// replays the stream into a mirror book and asserts order-for-order and
// lot-for-lot equality with the engine's own book across every order class —
// limits, market and IOC remainders, FOK reversal, post-only, stops both resting
// and cascade-fired, trailing stops, OCO in both directions, iceberg refill and
// exhaustion, and all five self-trade-prevention modes. Do not weaken that test
// to make a change pass; the claim on this line is only worth what the test
// proves.
type EventKind uint8

const (
	EventAccepted  EventKind = iota // order entered the book (rested, or began filling)
	EventRejected                   // order refused (with Reason)
	EventTrade                      // an execution (a fill)
	EventCanceled                   // order removed: cancelled, or terminated without resting
	EventTriggered                  // reserved: a stop/trailing fired
	EventReplaced                   // order changed size in place, keeping queue position
	EventBookDelta                  // reserved: an aggregated L2 level change
	EventHalted                     // engine entered Halted (guardrail trip or band-breach pause)
	EventResumed                    // engine returned to Open (e.g. band-breach pause elapsed)
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
}

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
type EventSink interface {
	OnEvents(events []Event)
}
