package matching

import "github.com/intrepidkarthi/orderbook/pkg/types"

// EventKind is the type of an emitted engine event. Accepted/Trade/Canceled form
// an ITCH-style add/execute/delete stream that fully reconstructs the L3 book;
// Rejected reports refused orders. Triggered/Replaced/BookDelta are reserved for
// future emission.
type EventKind uint8

const (
	EventAccepted  EventKind = iota // order entered the book (rested, or began filling)
	EventRejected                   // order refused (with Reason)
	EventTrade                      // an execution (a fill)
	EventCanceled                   // resting order removed by cancel
	EventTriggered                  // reserved: a stop/trailing fired
	EventReplaced                   // reserved: an order was modified in place
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
type EventSink interface {
	OnEvents(events []Event)
}
