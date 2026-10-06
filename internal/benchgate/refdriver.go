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
