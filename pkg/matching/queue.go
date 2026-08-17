package matching

import "github.com/intrepidkarthi/orderbook/pkg/types"

// cmdKind enumerates the mutating operations a Runner serialises onto its single
// matching goroutine.
type cmdKind uint8

const (
	cmdSubmit cmdKind = iota
	cmdCancel
	cmdStop
	cmdOCO
	cmdIceberg
	cmdPegged
	cmdTrailing
	cmdHalt
	cmdResume
	cmdCancelOnly
	cmdSetMark
	cmdBust
	cmdCancelAll
	cmdReduce
	cmdReplace
	cmdOpenOrders
	cmdTrailingCount
	cmdExpireDue
	cmdSetPhase
	cmdIndicative
	cmdCheckpoint

	// cmdKindCount is a sentinel: keep it last. It exists so the durability guard
	// in command_alphabet_test.go can ENUMERATE this block rather than restate it.
	// Three mutating commands — Reduce, Halt and SetPhase — were added here and
	// never added to logCommand, each of them silently, each of them found only
	// after a restart lost something. A kind added above this line without a
	// classification is now a test failure at the moment the constant is written,
	// which is the only point at which the author still remembers whether it
	// changes state a recovery must reproduce. See docs/JOURNAL-COMPLETENESS.md §4.4.
	cmdKindCount
)

// command is one unit of work for the matching goroutine. Exactly one payload
// field is meaningful per kind. reply, if non-nil, receives the outcome once the
// command has been applied.
type command struct {
	kind     cmdKind
	order    *types.Order
	stop     *types.StopOrder
	oco      *types.OCOOrder
	iceberg  *types.IcebergOrder
	pegged   *types.PeggedOrder
	trailing *types.TrailingStop
	replace  *types.Order // the replacement order for cmdReplace
	phase    EngineState  // target phase for cmdSetPhase
	cancelID int64
	// resolve, when set, names the target order ON THE MATCHING GOROUTINE, at the
	// moment the command is applied, instead of on the caller's goroutine before it
	// is enqueued.
	//
	// The difference is the whole point. A client names its order by its own
	// identifier, and the mapping from that to an engine order id only exists once the
	// engine has accepted the order. Resolving ahead of the queue asks that question
	// too early: with commands waiting, a cancel could be refused as "no such order"
	// while the Enter that creates it was still queued two positions ahead of it. The
	// enqueue order was always right — the reference gateway is careful to enqueue on
	// its read loop precisely so a cancel cannot overtake its own Enter — but the
	// lookup happened before the queue, so the ordering guarantee protected the wrong
	// step.
	//
	// It is called exactly once, before the command is journalled, so the log records
	// the resolved id and a replay needs no resolver. It must not block: it runs where
	// the venue matches.
	resolve   func() (int64, bool)
	reduceQty int64
	userID    string
	// bustReason is the operator's free-text reason for a cmdBust. The trade id
	// travels in cancelID, which every id-bearing command already uses.
	bustReason string
	snapOut    chan *EngineSnapshot
	reply      chan cmdReply
}

// cmdReply is the result of applying a command. match is set for order-submitting
// commands; order/err are set for a cancel.
type cmdReply struct {
	match      *MatchResult
	order      *types.Order
	orders     []*types.Order
	trades     []*types.Trade
	count      int
	indicative Indicative
	err        error
}
