// Command obdemo is a small end-to-end demonstration of the matching engine:
// it seeds a book with resting liquidity, then sends a crossing limit order and
// a market sweep, printing the ladder and the trade tape at each step.
//
//	go run ./cmd/obdemo
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

const symbol = "BTC-USD"

func main() {
	eng := matching.NewEngine(matching.DefaultConfig(symbol))

	fmt.Println(rule())
	fmt.Printf("  %s matching-engine demo\n", symbol)
	fmt.Println(rule())

	// 1) Seed resting liquidity: makers on both sides.
	step("1. Seed the book with resting maker liquidity")
	seed := []struct {
		user       string
		side       types.Side
		price, qty string
	}{
		{"mm1", types.SideSell, "101.0", "3"},
		{"mm2", types.SideSell, "101.5", "2"},
		{"mm3", types.SideSell, "102.0", "5"},
		{"mm4", types.SideBuy, "100.0", "4"},
		{"mm5", types.SideBuy, "99.5", "6"},
		{"mm6", types.SideBuy, "99.0", "3"},
	}
	for _, s := range seed {
		submit(eng, limit(s.user, s.side, s.price, s.qty))
	}
	printBook(eng)

	// 2) A crossing limit buy: takes the two cheapest asks and rests the rest.
	step("2. Aggressive BUY limit 6 @ 101.5 (crosses 101.0 and 101.5)")
	res := submit(eng, limit("taker1", types.SideBuy, "101.5", "6"))
	printTrades(res)
	printBook(eng)

	// 3) A market SELL sweep into the bids.
	step("3. Market SELL 8 (sweeps the bid side)")
	res = submit(eng, market("taker2", types.SideSell, "8"))
	printTrades(res)
	printBook(eng)

	fmt.Println(rule())
}

// --- helpers ---

func limit(user string, side types.Side, price, qty string) *types.Order {
	o, err := types.NewOrder(user, symbol, side, types.OrderTypeLimit, dec(price), dec(qty), types.TIFGoodTillCancel)
	if err != nil {
		fatal(err)
	}
	return o
}

func market(user string, side types.Side, qty string) *types.Order {
	o, err := types.NewOrder(user, symbol, side, types.OrderTypeMarket, decimal.Zero, dec(qty), types.TIFImmediateOrCancel)
	if err != nil {
		fatal(err)
	}
	return o
}

func submit(eng *matching.Engine, o *types.Order) *matching.MatchResult {
	return eng.Process(o)
}

func printTrades(res *matching.MatchResult) {
	if res.RejectionReason != nil {
		fmt.Printf("   result: %s (%v)\n", res.Status, res.RejectionReason)
	}
	if len(res.Trades) == 0 {
		fmt.Println("   trades: (none)")
		return
	}
	fmt.Printf("   trades: %s\n", res.Status)
	for _, tr := range res.Trades {
		fmt.Printf("     • %s @ %-7s qty %-4s  (%s buys from %s)\n",
			symbol, tr.Price, tr.Quantity, tr.BuyerUserID, tr.SellerUserID)
	}
}

func printBook(eng *matching.Engine) {
	snap := eng.Snapshot(10)
	fmt.Println()
	fmt.Printf("   %-9s %-9s\n", "PRICE", "SIZE")
	// Asks: print worst→best so the best ask sits just above the spread.
	for i := len(snap.Asks) - 1; i >= 0; i-- {
		a := snap.Asks[i]
		fmt.Printf("   \033[31m%-9s %-9s\033[0m  ask\n", a.Price, a.Quantity)
	}
	if sp, ok := eng.Spread(); ok {
		mid, _ := eng.MidPrice()
		fmt.Printf("   ── spread %s · mid %s ──\n", sp, mid)
	}
	// Bids: best→worst.
	for _, b := range snap.Bids {
		fmt.Printf("   \033[32m%-9s %-9s\033[0m  bid\n", b.Price, b.Quantity)
	}
	fmt.Println()
}

func step(title string) {
	fmt.Printf("\n%s\n", title)
}

func rule() string { return strings.Repeat("─", 48) }

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
