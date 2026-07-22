// Basic example: create an engine, rest a sell order, then cross it with a buy
// and observe the trade and the resulting book.
//
// It also shows the Instrument boundary: humans think in decimals (30,000.00 and
// 0.5 BTC), the engine works in integer ticks and lots. The Instrument converts
// at the edge; everything inside is exact integer arithmetic.
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
	// BTC-USD priced to the cent, sized to the milli-BTC.
	inst := types.NewInstrument("BTC-USD", dec("0.01"), dec("0.001"))
	eng := matching.NewEngine(matching.DefaultConfig(inst.Symbol))

	// A resting sell (maker): 0.5 @ 30,000.
	ask, _ := inst.NewOrder("alice", types.SideSell, types.OrderTypeLimit,
		dec("30000"), dec("0.5"), types.TIFGoodTillCancel)
	eng.Process(ask)

	// A crossing buy (taker): 0.3 @ 30,000 — trades against the resting sell.
	bid, _ := inst.NewOrder("bob", types.SideBuy, types.OrderTypeLimit,
		dec("30000"), dec("0.3"), types.TIFGoodTillCancel)
	res := eng.Process(bid)

	for _, t := range res.Trades {
		fmt.Printf("trade: %s @ %s  (taker: %s)\n",
			inst.LotsToQty(t.Quantity), inst.TicksToPrice(t.Price), t.TakerSide)
	}

	// 0.2 of the sell remains resting.
	if px, qty, ok := eng.BestAsk(); ok {
		fmt.Printf("book:  best ask %s x %s\n", inst.TicksToPrice(px), inst.LotsToQty(qty))
	}
}
