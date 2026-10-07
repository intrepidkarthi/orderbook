package benchgate

import (
	"errors"
	"fmt"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/tape"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

const benchSymbol = "BENCH"

// engineReasons maps every rejection an obtape 1 tape can reach to its format code.
// It is a copy of pkg/matching's tierOneRejections (differential_test.go), because a
// test file cannot be imported. There is no catch-all: an error that is not here is
// a hard encoder failure, not "other", because "unknown, therefore equal" would turn
// the digest off for exactly the commands that changed.
var engineReasons = []struct {
	err  error
	code uint8
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

func engineReason(err error) (uint8, error) {
	if err == nil {
		return 0, nil
	}
	for _, r := range engineReasons {
		if errors.Is(err, r.err) {
			return r.code, nil
		}
	}
	return 0, fmt.Errorf("obdg: engine error %v has no format code", err)
}

func engineStatus(s types.OrderStatus) (uint8, error) {
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
	return 0, fmt.Errorf("obdg: engine status %s has no format code", s)
}

// eventLog is the engine's sink while a command runs.
type eventLog struct{ evs []matching.Event }

func (l *eventLog) OnEvents(evs []matching.Event) { l.evs = append(l.evs, evs...) }

// counterClock is a deterministic clock. Nothing on an obtape 1 tape reads time, but
// a replay must not depend on what the wall clock said either way.
func counterClock() func() time.Time {
	base := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	var n int64
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Millisecond)
	}
}

// engineDriver replays a tape through pkg/matching and names everything it sees by
// tape position.
type engineDriver struct {
	e         *matching.Engine
	log       *eventLog
	published int              // every event published so far
	ids       []int64          // tape position -> engine order id, 0 if none
	pos       map[int64]uint64 // engine order id -> tape position
	buf       []types.Trade
}

func newEngineDriver(maxOrders int64, n int) *engineDriver {
	cfg := matching.DefaultConfig(benchSymbol)
	cfg.Clock = counterClock()
	cfg.MaxOrders = int(maxOrders)
	log := &eventLog{}
	cfg.EventSink = log
	return &engineDriver{e: matching.NewEngine(cfg), log: log, ids: make([]int64, n), pos: map[int64]uint64{}}
}

func engineOrder(c tape.Cmd) (*types.Order, error) {
	side := types.SideBuy
	if c.Sell {
		side = types.SideSell
	}
	ot := types.OrderTypeLimit
	if c.MarketOrd {
		ot = types.OrderTypeMarket
	}
	tif := types.TIFGoodTillCancel
	switch c.TIF {
	case 1:
		tif = types.TIFImmediateOrCancel
	case 2:
		tif = types.TIFFillOrKill
	}
	o, err := types.NewOrder(c.User, benchSymbol, side, ot, c.Price, c.Qty, tif)
	if err != nil {
		return nil, fmt.Errorf("command %d: %w", c.Pos, err)
	}
	o.PostOnly = c.PostOnly
	return o, nil
}

// apply runs one command and returns its outcome.
func (d *engineDriver) apply(c tape.Cmd) (outcome, error) {
	d.log.evs = d.log.evs[:0]
	out := outcome{pos: uint64(c.Pos)}
	var trades []types.Trade
	var status types.OrderStatus
	var err error
	verdictFromOrder := true

	switch c.Kind {
	case tape.Submit:
		o, berr := engineOrder(c)
		if berr != nil {
			return out, berr
		}
		d.buf, status, err = d.e.Match(o, d.buf[:0])
		trades = d.buf
		d.ids[c.Pos] = o.ID
	case tape.Cancel:
		_, err = d.e.Cancel(d.ids[c.Target], c.User)
		verdictFromOrder, out.status = false, stCancelled
	case tape.Reduce:
		_, err = d.e.Reduce(d.ids[c.Target], c.NewQty, c.User)
		verdictFromOrder, out.status = false, stReduced
	case tape.Replace:
		repl, berr := engineOrder(c)
		if berr != nil {
			return out, berr
		}
		res, rerr := d.e.Replace(d.ids[c.Target], c.User, repl)
		if rerr != nil {
			// The cancel half failed, so the replacement never reached the venue.
			err, verdictFromOrder = rerr, false
			break
		}
		d.ids[c.Pos] = res.Order.ID
		status, err = res.Status, res.RejectionReason
		for _, t := range res.Trades {
			trades = append(trades, *t)
		}
	default:
		return out, fmt.Errorf("command %d: kind %s is not in obtape 1", c.Pos, c.Kind)
	}

	if out.reason, err = engineReason(err); err != nil {
		return out, err
	}
	switch {
	case verdictFromOrder:
		if out.status, err = engineStatus(status); err != nil {
			return out, err
		}
	case out.reason != 0:
		out.status = stRejected
	}

	if err := d.events(c, &out); err != nil {
		return out, err
	}
	for _, t := range trades {
		dt, err := d.trade(t)
		if err != nil {
			return out, fmt.Errorf("command %d returned trade: %v", c.Pos, err)
		}
		out.trades = append(out.trades, dt)
	}
	return out, nil
}

// events names this command's published events by position. A new order is named
// by the first Accepted or Rejected event that mentions its id; any other event
// naming an id never seen is a hard error rather than a guess.
func (d *engineDriver) events(c tape.Cmd, out *outcome) error {
	d.published += len(d.log.evs)
	for _, e := range d.log.evs {
		switch e.Kind {
		case matching.EventAccepted, matching.EventRejected:
			if _, seen := d.pos[e.OrderID]; !seen {
				d.pos[e.OrderID] = uint64(c.Pos)
			}
			r := ref{pos: d.pos[e.OrderID]}
			if e.Kind == matching.EventAccepted {
				out.events = append(out.events, dEvent{tag: tagAccepted, ref: r})
				continue
			}
			code, err := engineReason(e.Reason)
			if err != nil {
				return err
			}
			out.events = append(out.events, dEvent{tag: tagRejected, ref: r, reason: code})
		case matching.EventCanceled, matching.EventReplaced:
			p, seen := d.pos[e.OrderID]
			if !seen {
				return fmt.Errorf("command %d: %s names order %d, never seen", c.Pos, e.Kind, e.OrderID)
			}
			tag := byte(tagReplaced)
			if e.Kind == matching.EventCanceled {
				if e.Reason != nil {
					return fmt.Errorf("command %d: a cancel carries reason %v, which OBDG v1 cannot say", c.Pos, e.Reason)
				}
				tag = tagCanceled
			}
			out.events = append(out.events, dEvent{tag: tag, ref: ref{pos: p}})
		case matching.EventTrade:
			if e.Trade == nil {
				return fmt.Errorf("command %d: a trade event carries no trade", c.Pos)
			}
			dt, err := d.trade(*e.Trade)
			if err != nil {
				return fmt.Errorf("command %d trade event: %v", c.Pos, err)
			}
			out.events = append(out.events, dEvent{tag: tagTrade, trade: dt})
		default:
			return fmt.Errorf("command %d: event %s is outside OBDG v1", c.Pos, e.Kind)
		}
	}
	return nil
}

func (d *engineDriver) trade(t types.Trade) (dTrade, error) {
	maker, ok1 := d.pos[t.MakerOrderID]
	taker, ok2 := d.pos[t.TakerOrderID]
	if !ok1 || !ok2 {
		return dTrade{}, fmt.Errorf("trade names order %d or %d, never seen", t.MakerOrderID, t.TakerOrderID)
	}
	agg := byte('B')
	if t.TakerSide == types.SideSell {
		agg = 'S'
	}
	return dTrade{price: t.Price, qty: t.Quantity, maker: ref{pos: maker}, taker: ref{pos: taker}, aggressor: agg}, nil
}

func (d *engineDriver) terminal() (terminal, error) {
	var t terminal
	for _, o := range d.e.Book().Orders() {
		p, ok := d.pos[o.ID]
		if !ok {
			return t, fmt.Errorf("resting order %d was never seen", o.ID)
		}
		t.book = append(t.book, restingOrder{ref: ref{pos: p}, sell: o.Side == types.SideSell,
			price: o.Price, qty: o.Quantity, filled: o.FilledQty})
	}
	t.lastTrade = d.e.LastTradePrice()
	return t, nil
}
