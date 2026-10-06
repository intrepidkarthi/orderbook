package benchgate

import (
	"fmt"

	"github.com/intrepidkarthi/orderbook/internal/refmatch"
	"github.com/intrepidkarthi/orderbook/internal/tape"
)

// refShard is the shard refmatch runs as. The engine runs as shard 0, so the two
// assign different order ids to the same order (an id's high bits name its shard).
// That is deliberate: the output digest names orders by tape position, and an
// encoder that leaked an engine id into it would make the two sides disagree.
const refShard = 1

// refDriver replays a tape through internal/refmatch, the reference matcher.
type refDriver struct {
	m   *refmatch.Model
	ids []int64 // tape position -> the order id refmatch gave it, 0 if none
}

func newRefDriver(maxOrders int64, n int) *refDriver {
	return &refDriver{
		m:   refmatch.New(refmatch.Config{Symbol: "BENCH", MaxOrders: int(maxOrders), ShardIndex: refShard}),
		ids: make([]int64, n),
	}
}

// apply runs one command. Only the four kinds obtape 1 can say are accepted.
func (d *refDriver) apply(c tape.Cmd) (refmatch.Result, error) {
	switch c.Kind {
	case tape.Submit:
		r := d.m.Submit(refOrder(c))
		d.ids[c.Pos] = r.Events[0].OrderID
		return r, nil
	case tape.Cancel:
		return d.m.Cancel(d.ids[c.Target], c.User), nil
	case tape.Reduce:
		return d.m.Reduce(d.ids[c.Target], c.NewQty, c.User), nil
	case tape.Replace:
		before := d.m.NextOrderID()
		r := d.m.Replace(d.ids[c.Target], c.User, refOrder(c))
		if d.m.NextOrderID() != before {
			d.ids[c.Pos] = before
		}
		return r, nil
	}
	return refmatch.Result{}, fmt.Errorf("command %d: kind %s is not in obtape 1", c.Pos, c.Kind)
}

// refOrder translates one tape order. obtape 1 carries no STP mode, trade group or
// privileged flag, so none is set.
func refOrder(c tape.Cmd) refmatch.Order {
	o := refmatch.Order{User: c.User, Price: c.Price, Qty: c.Qty, PostOnly: c.PostOnly}
	if c.Sell {
		o.Side = refmatch.Sell
	}
	if c.MarketOrd {
		o.Type = refmatch.Market
	}
	switch c.TIF {
	case 1:
		o.TIF = refmatch.IOC
	case 2:
		o.TIF = refmatch.FOK
	}
	return o
}

// refStatus and refReason translate refmatch's verdict into the format's codes
// through explicit switches, so a reordered enum in refmatch cannot quietly change
// what a digest says.
func refStatus(s refmatch.Status) (uint8, error) {
	switch s {
	case refmatch.StatusNA:
		return stNA, nil
	case refmatch.StatusNew:
		return stNew, nil
	case refmatch.StatusPartiallyFilled:
		return stPartiallyFilled, nil
	case refmatch.StatusFilled:
		return stFilled, nil
	case refmatch.StatusCancelled:
		return stCancelled, nil
	case refmatch.StatusRejected:
		return stRejected, nil
	case refmatch.StatusReduced:
		return stReduced, nil
	}
	return 0, fmt.Errorf("obdg: refmatch status %d has no format code", s)
}

func refReason(r refmatch.Reject) (uint8, error) {
	switch r {
	case refmatch.RejectNone:
		return 0, nil
	case refmatch.RejectTradingHalted:
		return 1, nil
	case refmatch.RejectNewOrdersHalted:
		return 2, nil
	case refmatch.RejectPostOnlyWouldCross:
		return 3, nil
	case refmatch.RejectFOKCannotFill:
		return 4, nil
	case refmatch.RejectMarketNoLiquidity:
		return 5, nil
	case refmatch.RejectOrderBookFull:
		return 6, nil
	case refmatch.RejectOrderNotFound:
		return 7, nil
	case refmatch.RejectOrderNotActive:
		return 8, nil
	case refmatch.RejectInvalidQuantity:
		return 9, nil
	case refmatch.RejectNilOrder:
		return 10, nil
	case refmatch.RejectNotionalOverflow:
		return 11, nil
	}
	return 0, fmt.Errorf("obdg: refmatch reason %d has no format code", r)
}

// refNamer turns refmatch's ids into tape positions, by the same rule the engine
// driver uses: a new order is named by the first Accepted or Rejected event that
// mentions it, and anything else naming an unseen id is a hard error.
type refNamer struct{ pos map[int64]uint64 }

func (n *refNamer) trade(t refmatch.Trade) (dTrade, error) {
	maker, ok1 := n.pos[t.MakerOrderID]
	taker, ok2 := n.pos[t.TakerOrderID]
	if !ok1 || !ok2 {
		return dTrade{}, fmt.Errorf("trade names order %d or %d, never seen", t.MakerOrderID, t.TakerOrderID)
	}
	agg := byte('B')
	if t.TakerSide == refmatch.Sell {
		agg = 'S'
	}
	return dTrade{price: t.Price, qty: t.Qty, maker: ref{pos: maker}, taker: ref{pos: taker}, aggressor: agg}, nil
}

// outcome runs one command and names its result by position.
//
// refmatch's trade EVENTS carry price and quantity but not the maker, so each one is
// paired with the next entry of the command's returned trade list, in order, and the
// pair must agree on price and quantity. That is a check on refmatch, not a
// convenience: the two lists are its own two sources for the same prints.
func (d *refDriver) outcome(c tape.Cmd, n *refNamer) (outcome, error) {
	r, err := d.apply(c)
	if err != nil {
		return outcome{}, err
	}
	out := outcome{pos: uint64(c.Pos)}
	if out.status, err = refStatus(r.Verdict.Status); err != nil {
		return out, err
	}
	if out.reason, err = refReason(r.Verdict.Reason); err != nil {
		return out, err
	}
	next := 0
	for _, e := range r.Events {
		switch e.Kind {
		case refmatch.EvAccepted, refmatch.EvRejected:
			if _, seen := n.pos[e.OrderID]; !seen {
				n.pos[e.OrderID] = uint64(c.Pos)
			}
			rf := ref{pos: n.pos[e.OrderID]}
			if e.Kind == refmatch.EvAccepted {
				out.events = append(out.events, dEvent{tag: tagAccepted, ref: rf})
				continue
			}
			code, err := refReason(e.Reason)
			if err != nil {
				return out, err
			}
			out.events = append(out.events, dEvent{tag: tagRejected, ref: rf, reason: code})
		case refmatch.EvCanceled, refmatch.EvReplaced:
			p, seen := n.pos[e.OrderID]
			if !seen {
				return out, fmt.Errorf("command %d: event names order %d, never seen", c.Pos, e.OrderID)
			}
			if e.Kind == refmatch.EvCanceled && e.Reason != refmatch.RejectNone {
				return out, fmt.Errorf("command %d: a cancel carries a reason, which OBDG v1 cannot say", c.Pos)
			}
			tag := byte(tagReplaced)
			if e.Kind == refmatch.EvCanceled {
				tag = tagCanceled
			}
			out.events = append(out.events, dEvent{tag: tag, ref: ref{pos: p}})
		case refmatch.EvTrade:
			if next >= len(r.Trades) {
				return out, fmt.Errorf("command %d: more trade events than returned trades", c.Pos)
			}
			t := r.Trades[next]
			next++
			if t.Price != e.Price || t.Qty != e.Qty {
				return out, fmt.Errorf("command %d: trade event %d@%d disagrees with returned trade %d@%d",
					c.Pos, e.Qty, e.Price, t.Qty, t.Price)
			}
			dt, err := n.trade(t)
			if err != nil {
				return out, err
			}
			out.events = append(out.events, dEvent{tag: tagTrade, trade: dt})
		default:
			return out, fmt.Errorf("command %d: event kind %d is outside OBDG v1", c.Pos, e.Kind)
		}
	}
	if next != len(r.Trades) {
		return out, fmt.Errorf("command %d: %d returned trades, %d trade events", c.Pos, len(r.Trades), next)
	}
	for _, t := range r.Trades {
		dt, err := n.trade(t)
		if err != nil {
			return out, err
		}
		out.trades = append(out.trades, dt)
	}
	return out, nil
}

func (d *refDriver) terminal(n *refNamer) (terminal, error) {
	var t terminal
	for _, o := range d.m.Book() {
		p, ok := n.pos[o.ID]
		if !ok {
			return t, fmt.Errorf("resting order %d was never seen", o.ID)
		}
		t.book = append(t.book, restingOrder{ref: ref{pos: p}, sell: o.Side == refmatch.Sell,
			price: o.Price, qty: o.Qty, filled: o.Filled})
	}
	t.lastTrade = d.m.LastTradePrice()
	return t, nil
}
