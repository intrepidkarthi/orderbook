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
	cmdCancelAll
	cmdReduce
	cmdReplace
	cmdOpenOrders
	cmdTrailingCount
	cmdExpireDue
	cmdCheckpoint
)

// command is one unit of work for the matching goroutine. Exactly one payload
// field is meaningful per kind. reply, if non-nil, receives the outcome once the
// command has been applied.
type command struct {
	kind      cmdKind
	order     *types.Order
	stop      *types.StopOrder
	oco       *types.OCOOrder
	iceberg   *types.IcebergOrder
	pegged    *types.PeggedOrder
	trailing  *types.TrailingStop
	replace   *types.Order // the replacement order for cmdReplace
	cancelID  int64
	reduceQty int64
	userID    string
	snapOut   chan *EngineSnapshot
	reply     chan cmdReply
}

// cmdReply is the result of applying a command. match is set for order-submitting
// commands; order/err are set for a cancel.
type cmdReply struct {
	match  *MatchResult
	order  *types.Order
	orders []*types.Order
	count  int
	err    error
}
