// Command libobook is the matching engine behind a stable C ABI (docs/C-API.md).
//
//	go build -buildmode=c-shared -o libobook.so ./cmd/libobook
//
// produces the library and a libobook.h header. Every function takes integers and
// pointers to caller-owned memory; no Go pointer crosses the boundary.
package main

/*
#include <stdint.h>

typedef struct {
	int32_t kind, reason, aggressor, _pad;
	int64_t order_id, user, price, qty, maker_id, taker_id;
} ob_event;

typedef struct {
	int64_t order_id, user, price, qty, filled;
	int32_t side, _pad;
} ob_order;
*/
import "C"

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func main() {}

// abiVersion is frozen: a change to any signature, struct layout or code value is a
// new version, and ob_abi_version is how a binding refuses a library it was not
// written for.
const abiVersion = 1

// Event kinds, status and reason codes. Status and reason equal the OBDG codes of
// docs/BENCH-GATE.md §3.1 by definition, not by coincidence of Go iota.
const (
	evAccepted = 1
	evRejected = 2
	evTrade    = 3
	evCanceled = 4
	evReplaced = 5

	stNew             = 1
	stPartiallyFilled = 2
	stFilled          = 3
	stCancelled       = 4
	stRejected        = 5
	stReduced         = 6
)

var reasons = []struct {
	err  error
	code int32
}{
	{types.ErrTradingHalted, 1},
	{types.ErrNewOrdersHalted, 2},
	{types.ErrPostOnlyWouldCross, 3},
	{types.ErrFOKCannotFill, 4},
	{types.ErrMarketOrderNoLiquidity, 5},
	{types.ErrOrderBookFull, 6},
	{types.ErrOrderNotFound, 7},
	{types.ErrOrderNotActive, 8},
	{types.ErrInvalidQuantity, 9},
	{types.ErrNilOrder, 10},
	{types.ErrNotionalOverflow, 11},
}

// errUnmapped is returned as -1: a guessed reason code would be worse than none.
var errUnmapped = errors.New("unmapped")

func reasonCode(err error) (int32, error) {
	if err == nil {
		return 0, nil
	}
	for _, r := range reasons {
		if errors.Is(err, r.err) {
			return r.code, nil
		}
	}
	return 0, errUnmapped
}

func statusCode(s types.OrderStatus) (int32, error) {
	switch s {
	case types.OrderStatusNew:
		return stNew, nil
	case types.OrderStatusPartiallyFilled:
		return stPartiallyFilled, nil
	case types.OrderStatusFilled:
		return stFilled, nil
	case types.OrderStatusCancelled:
		return stCancelled, nil
	case types.OrderStatusRejected:
		return stRejected, nil
	}
	return 0, errUnmapped
}

// book is one handle: an engine and the events it has published but nobody has read.
type book struct {
	e      *matching.Engine
	events []matching.Event
	buf    []types.Trade
}

func (b *book) OnEvents(evs []matching.Event) { b.events = append(b.events, evs...) }

var (
	mu      sync.Mutex
	books   = map[int64]*book{}
	nextHdl int64
)

func lookup(h C.int64_t) *book {
	mu.Lock()
	defer mu.Unlock()
	return books[int64(h)]
}

const symbol = "OBK"

//export ob_abi_version
func ob_abi_version() C.int32_t { return abiVersion }

//export ob_new
func ob_new(maxOrders C.int64_t) C.int64_t {
	if maxOrders <= 0 {
		return 0
	}
	b := &book{}
	cfg := matching.DefaultConfig(symbol)
	cfg.MaxOrders = int(maxOrders)
	cfg.EventSink = b
	// A counter clock: nothing the ABI exposes reads time, and a replay must not
	// depend on what the wall clock said.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var n int64
	cfg.Clock = func() time.Time { n++; return base.Add(time.Duration(n)) }
	b.e = matching.NewEngine(cfg)
	mu.Lock()
	defer mu.Unlock()
	nextHdl++
	books[nextHdl] = b
	return C.int64_t(nextHdl)
}

//export ob_free
func ob_free(h C.int64_t) {
	mu.Lock()
	defer mu.Unlock()
	delete(books, int64(h))
}

func userName(u C.int64_t) string { return "u" + strconv.FormatInt(int64(u), 10) }

func userNumber(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "u"), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func newOrder(user C.int64_t, side, typ C.int32_t, price, qty C.int64_t, tif, postOnly C.int32_t) (*types.Order, bool) {
	if side < 0 || side > 1 || typ < 0 || typ > 1 || tif < 0 || tif > 2 || postOnly < 0 || postOnly > 1 {
		return nil, false
	}
	s := types.SideBuy
	if side == 1 {
		s = types.SideSell
	}
	t := types.OrderTypeLimit
	if typ == 1 {
		t = types.OrderTypeMarket
	}
	tf := []types.TimeInForce{types.TIFGoodTillCancel, types.TIFImmediateOrCancel, types.TIFFillOrKill}[tif]
	o, err := types.NewOrder(userName(user), symbol, s, t, int64(price), int64(qty), tf)
	if err != nil {
		return nil, false
	}
	o.PostOnly = postOnly == 1
	return o, true
}

// verdict writes a command's outcome. A rejection is a successful call that says
// "rejected"; only an unmapped status or reason is a failed call.
func verdict(statusOut, reasonOut *C.int32_t, st int32, err error) C.int32_t {
	r, rerr := reasonCode(err)
	if rerr != nil {
		return -1
	}
	*statusOut, *reasonOut = C.int32_t(st), C.int32_t(r)
	return 0
}

//export ob_submit
func ob_submit(h, user C.int64_t, side, typ C.int32_t, price, qty C.int64_t, tif, postOnly C.int32_t,
	orderID *C.int64_t, status, reason *C.int32_t) C.int32_t {
	b := lookup(h)
	if b == nil || orderID == nil || status == nil || reason == nil {
		return -1
	}
	o, ok := newOrder(user, side, typ, price, qty, tif, postOnly)
	if !ok {
		return -1
	}
	var st types.OrderStatus
	var err error
	b.buf, st, err = b.e.Match(o, b.buf[:0])
	code, serr := statusCode(st)
	if serr != nil {
		return -1
	}
	*orderID = C.int64_t(o.ID)
	return verdict(status, reason, code, err)
}

//export ob_cancel
func ob_cancel(h, orderID, user C.int64_t, status, reason *C.int32_t) C.int32_t {
	b := lookup(h)
	if b == nil || status == nil || reason == nil {
		return -1
	}
	_, err := b.e.Cancel(int64(orderID), userName(user))
	st := int32(stCancelled)
	if err != nil {
		st = stRejected
	}
	return verdict(status, reason, st, err)
}

//export ob_reduce
func ob_reduce(h, orderID, newQty, user C.int64_t, status, reason *C.int32_t) C.int32_t {
	b := lookup(h)
	if b == nil || status == nil || reason == nil {
		return -1
	}
	_, err := b.e.Reduce(int64(orderID), int64(newQty), userName(user))
	st := int32(stReduced)
	if err != nil {
		st = stRejected
	}
	return verdict(status, reason, st, err)
}

//export ob_replace
func ob_replace(h, orderID, user C.int64_t, side, typ C.int32_t, price, qty C.int64_t, tif, postOnly C.int32_t,
	newOrderID *C.int64_t, status, reason *C.int32_t) C.int32_t {
	b := lookup(h)
	if b == nil || newOrderID == nil || status == nil || reason == nil {
		return -1
	}
	o, ok := newOrder(user, side, typ, price, qty, tif, postOnly)
	if !ok {
		return -1
	}
	res, err := b.e.Replace(int64(orderID), userName(user), o)
	if err != nil {
		// The cancel half failed, so the replacement never reached the venue.
		*newOrderID = 0
		return verdict(status, reason, stRejected, err)
	}
	code, serr := statusCode(res.Status)
	if serr != nil {
		return -1
	}
	*newOrderID = C.int64_t(res.Order.ID)
	return verdict(status, reason, code, res.RejectionReason)
}

//export ob_events
func ob_events(h C.int64_t, buf *C.ob_event, capacity C.int32_t) C.int32_t {
	b := lookup(h)
	if b == nil || (buf == nil && capacity > 0) || capacity < 0 {
		return -1
	}
	out := unsafe.Slice(buf, int(capacity))
	n := 0
	for n < len(out) && len(b.events) > 0 {
		e := b.events[0]
		var ce C.ob_event
		switch e.Kind {
		case matching.EventAccepted:
			ce.kind = evAccepted
		case matching.EventRejected:
			ce.kind = evRejected
		case matching.EventTrade:
			ce.kind = evTrade
		case matching.EventCanceled:
			ce.kind = evCanceled
		case matching.EventReplaced:
			ce.kind = evReplaced
		default:
			// Halts, phases and busts are outside ABI 1; the ABI has no command
			// that produces them.
			return -1
		}
		r, err := reasonCode(e.Reason)
		if err != nil {
			return -1
		}
		ce.reason = C.int32_t(r)
		ce.order_id = C.int64_t(e.OrderID)
		ce.user = C.int64_t(userNumber(e.UserID))
		if t := e.Trade; t != nil {
			ce.price, ce.qty = C.int64_t(t.Price), C.int64_t(t.Quantity)
			ce.maker_id, ce.taker_id = C.int64_t(t.MakerOrderID), C.int64_t(t.TakerOrderID)
			if t.TakerSide == types.SideSell {
				ce.aggressor = 1
			}
		}
		out[n] = ce
		n++
		b.events = b.events[1:]
	}
	if len(b.events) == 0 {
		b.events = b.events[:0:0]
	}
	return C.int32_t(n)
}

//export ob_book
func ob_book(h C.int64_t, buf *C.ob_order, capacity C.int32_t) C.int32_t {
	b := lookup(h)
	if b == nil || capacity < 0 {
		return -1
	}
	orders := b.e.Book().Orders()
	if len(orders) > int(capacity) {
		return -1
	}
	if len(orders) == 0 {
		return 0
	}
	out := unsafe.Slice(buf, int(capacity))
	for i, o := range orders {
		var co C.ob_order
		co.order_id, co.user = C.int64_t(o.ID), C.int64_t(userNumber(o.UserID))
		co.price, co.qty, co.filled = C.int64_t(o.Price), C.int64_t(o.Quantity), C.int64_t(o.FilledQty)
		if o.Side == types.SideSell {
			co.side = 1
		}
		out[i] = co
	}
	return C.int32_t(len(orders))
}

//export ob_last_trade_price
func ob_last_trade_price(h C.int64_t) C.int64_t {
	b := lookup(h)
	if b == nil {
		return 0
	}
	return C.int64_t(b.e.LastTradePrice())
}
