// Command quickstart is the README's quickstart: each call, then the book it leaves.
// Its output is pinned by main_test.go, and the README must show it verbatim.
//
//	go run ./examples/quickstart
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func main() { run(os.Stdout) }

func run(w io.Writer) {
	eng := matching.NewEngine(matching.DefaultConfig("BTC-USD"))
	limit := func(user string, side types.Side, price, qty int64) *types.Order {
		o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		if err != nil {
			panic(err)
		}
		return o
	}
	step := func(call string, res *matching.MatchResult) {
		fmt.Fprintf(w, "%s\n  -> %s", call, res.Status)
		for _, t := range res.Trades {
			fmt.Fprintf(w, ", traded %d @ %d", t.Quantity, t.Price)
		}
		if res.RejectionReason != nil {
			fmt.Fprintf(w, " (%v)", res.RejectionReason)
		}
		fmt.Fprintln(w)
		show(w, eng)
	}

	show(w, eng)
	step(`eng.Process(limit("alice", types.SideSell, 101, 5))`, eng.Process(limit("alice", types.SideSell, 101, 5)))
	step(`eng.Process(limit("bob", types.SideSell, 101, 2))`, eng.Process(limit("bob", types.SideSell, 101, 2)))
	step(`eng.Process(limit("carol", types.SideBuy, 99, 4))`, eng.Process(limit("carol", types.SideBuy, 99, 4)))
	step(`eng.Process(limit("dave", types.SideBuy, 101, 6))`, eng.Process(limit("dave", types.SideBuy, 101, 6)))
	step(`eng.Process(limit("erin", types.SideBuy, 101, 1).AsPostOnly())`, eng.Process(limit("erin", types.SideBuy, 101, 1).AsPostOnly()))
}

// show prints the book: asks above, best ask nearest the spread; bids below. Each
// level lists its orders oldest first, as user:remaining.
func show(w io.Writer, eng *matching.Engine) {
	b := eng.Book()
	asks, bids := b.GetAskLevels(10), b.GetBidLevels(10)
	if len(asks)+len(bids) == 0 {
		fmt.Fprintf(w, "  (empty book)\n\n")
		return
	}
	level := func(side types.Side, l *orderbook.PriceLevel) {
		var q []string
		for _, o := range b.GetOrdersAtPrice(side, l.Price) {
			q = append(q, fmt.Sprintf("%s:%d", o.UserID, o.RemainingQty))
		}
		name := "ask"
		if side == types.SideBuy {
			name = "bid"
		}
		fmt.Fprintf(w, "  %s %4d  %3d  %s\n", name, l.Price, l.TotalQty, strings.Join(q, " "))
	}
	for i := len(asks) - 1; i >= 0; i-- {
		level(types.SideSell, asks[i])
	}
	fmt.Fprintln(w, "  ------------")
	for _, l := range bids {
		level(types.SideBuy, l)
	}
	fmt.Fprintln(w)
}
