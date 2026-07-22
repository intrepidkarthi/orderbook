package matching_test

import (
	"fmt"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// order is a tiny helper for the examples: prices are integer ticks, sizes lots.
func order(user string, side types.Side, price, qty int64) *types.Order {
	o, err := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		panic(err)
	}
	return o
}

// A resting sell, then a crossing buy that trades against it at the maker's price.
func ExampleEngine() {
	e := matching.NewEngine(matching.DefaultConfig("BTC-USD"))

	e.Process(order("mm", types.SideSell, 100, 5)) // resting ask @100

	res := e.Process(order("taker", types.SideBuy, 101, 3)) // buys 3, prints at 100
	tr := res.Trades[0]
	fmt.Printf("status=%s trades=%d price=%d qty=%d\n", res.Status, len(res.Trades), tr.Price, tr.Quantity)

	bid, _, _ := e.BestBid()
	ask, askQty, _ := e.BestAsk()
	fmt.Printf("book: bid=%d ask=%d x %d\n", bid, ask, askQty)
	// Output:
	// status=FILLED trades=1 price=100 qty=3
	// book: bid=0 ask=100 x 2
}

// Match is the zero-allocation entry point: pass a reusable buffer and trades are
// appended into it as values, with no per-order heap allocation.
func ExampleEngine_Match() {
	e := matching.NewEngine(matching.DefaultConfig("X"))
	buf := make([]types.Trade, 0, 8) // reused across calls

	buf, _, _ = e.Match(order("mm", types.SideSell, 100, 5), buf[:0])
	buf, status, _ := e.Match(order("t", types.SideBuy, 100, 2), buf[:0])

	fmt.Printf("status=%s trades=%d price=%d\n", status, len(buf), buf[0].Price)
	// Output: status=FILLED trades=1 price=100
}

// The Runner fronts the single-writer engine with a command queue so many
// producers can submit concurrently; Process enqueues and waits for the result.
func ExampleRunner() {
	r := matching.NewRunner(matching.RunnerConfig{Engine: matching.DefaultConfig("X")})
	defer r.Close()

	r.Process(order("mm", types.SideSell, 100, 5))
	res := r.Process(order("t", types.SideBuy, 100, 3))

	fmt.Printf("status=%s trades=%d\n", res.Status, len(res.Trades))
	// Output: status=FILLED trades=1
}
