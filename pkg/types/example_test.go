package types_test

import (
	"fmt"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// The engine works in integer ticks and lots. An Instrument defines the grid and
// converts human decimals to/from ticks at the API boundary — the only place
// decimals appear.
func ExampleInstrument_NewOrder() {
	// BTC-USD priced to the cent (0.01) and sized to the milli-BTC (0.001).
	inst := types.NewInstrument("BTC-USD", dec("0.01"), dec("0.001"))

	o, err := inst.NewOrder("alice", types.SideBuy, types.OrderTypeLimit,
		dec("30000.50"), dec("0.25"), types.TIFGoodTillCancel)
	if err != nil {
		panic(err)
	}

	fmt.Printf("price = %d ticks (%s)\n", o.Price, inst.TicksToPrice(o.Price))
	fmt.Printf("qty   = %d lots (%s)\n", o.Quantity, inst.LotsToQty(o.Quantity))
	// Output:
	// price = 3000050 ticks (30000.5)
	// qty   = 250 lots (0.25)
}

// Prices and quantities round-trip exactly through the tick/lot grid.
func ExampleInstrument_roundTrip() {
	inst := types.NewInstrument("BTC-USD", dec("0.01"), dec("0.001"))

	ticks := inst.PriceToTicks(dec("30000.55"))
	back := inst.TicksToPrice(ticks)
	fmt.Printf("%s -> %d -> %s\n", "30000.55", ticks, back)
	// Output: 30000.55 -> 3000055 -> 30000.55
}
