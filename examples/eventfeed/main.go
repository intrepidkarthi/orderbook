// Command eventfeed shows how a downstream adapter consumes the engine's ordered
// event stream — the integration seam a FIX/OUCH gateway, a market-data publisher,
// or drop copy would sit on. The matching core stays pure; this consumer just
// translates the sequenced EventSink stream into an execution-report feed and an
// order→deal→position projection (the MetaTrader-style lineage a broker's internal
// ECN needs).
//
//	go run ./examples/eventfeed
package main

import (
	"fmt"
	"sort"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// feed is an EventSink: it prints each event as a sequenced execution report and
// maintains a net position per user (order → deal → position).
type feed struct {
	positions map[string]int64
}

func (f *feed) OnEvents(evs []matching.Event) {
	for _, e := range evs {
		switch e.Kind {
		case matching.EventAccepted:
			fmt.Printf("  seq %-3d ACCEPTED  order %-2d  %-6s %s %d @ %d\n",
				e.Seq, e.OrderID, e.UserID, e.Order.Side, e.Order.RemainingQty, e.Order.Price)
		case matching.EventRejected:
			fmt.Printf("  seq %-3d REJECTED  order %-2d  %-6s (%v)\n", e.Seq, e.OrderID, e.UserID, e.Reason)
		case matching.EventCanceled:
			fmt.Printf("  seq %-3d CANCELED  order %-2d  %s\n", e.Seq, e.OrderID, e.UserID)
		case matching.EventTrade:
			t := e.Trade
			f.positions[t.BuyerUserID] += t.Quantity
			f.positions[t.SellerUserID] -= t.Quantity
			fmt.Printf("  seq %-3d TRADE     %d @ %-4d (%s buys from %s)\n",
				e.Seq, t.Quantity, t.Price, t.BuyerUserID, t.SellerUserID)
		}
	}
}

func main() {
	f := &feed{positions: map[string]int64{}}
	e := matching.NewEngine(matching.Config{Symbol: "BTC-USD", EventSink: f})

	mk := func(user string, side types.Side, price, qty int64) *types.Order {
		o, _ := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		return o
	}

	fmt.Println("event stream (what a FIX/OUCH gateway or drop-copy consumes):")
	e.Process(mk("mm", types.SideSell, 100, 5))
	ask2 := mk("mm", types.SideSell, 101, 5)
	e.Process(ask2)
	e.Process(mk("taker", types.SideBuy, 101, 8))           // crosses 100×5 then 101×3
	e.Process(mk("pm", types.SideBuy, 101, 1).AsPostOnly()) // would cross the remaining ask → rejected
	e.Cancel(ask2.ID, "mm")                                 // pull the resting remainder

	fmt.Println("\norder → deal → position projection:")
	users := make([]string, 0, len(f.positions))
	for u := range f.positions {
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		fmt.Printf("  %-8s net %+d\n", u, f.positions[u])
	}
}
