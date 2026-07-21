// Basic example: create an engine, rest a sell order, then cross it with a buy
// and observe the trade and the resulting book.
//
//	go run ./examples/basic
package main

import (
	"fmt"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func main() {
	eng := matching.NewEngine(matching.DefaultConfig("BTC-USD"))

	// A resting sell (maker): 0.5 @ 30,000.
	ask, _ := types.NewOrder("alice", "BTC-USD", types.SideSell, types.OrderTypeLimit,
		dec("30000"), dec("0.5"), types.TIFGoodTillCancel)
	eng.Process(ask)

	// A crossing buy (taker): 0.3 @ 30,000 — trades against the resting sell.
	bid, _ := types.NewOrder("bob", "BTC-USD", types.SideBuy, types.OrderTypeLimit,
		dec("30000"), dec("0.3"), types.TIFGoodTillCancel)
	res := eng.Process(bid)

	for _, t := range res.Trades {
		fmt.Printf("trade: %s @ %s  (taker: %s)\n", t.Quantity, t.Price, t.TakerSide)
	}

	// 0.2 of the sell remains resting.
	if px, qty, ok := eng.BestAsk(); ok {
		fmt.Printf("book:  best ask %s x %s\n", px, qty)
	}
}
