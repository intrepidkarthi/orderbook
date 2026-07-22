// Market-maker example: backtest an Avellaneda–Stoikov quoter against synthetic
// order flow and print its scorecard.
//
//	go run ./examples/marketmaker
package main

import (
	"fmt"

	"github.com/intrepidkarthi/orderbook/pkg/backtest"
	"github.com/intrepidkarthi/orderbook/pkg/strategy"
)

func main() {
	as, err := strategy.NewAvellanedaStoikov(strategy.ASParams{Gamma: 0.1, Kappa: 1.5, Sigma: 0.3})
	if err != nil {
		panic(err)
	}

	r := backtest.Run(backtest.Config{
		Symbol:       "SIM",
		Steps:        3000,
		Seed:         1,
		InitialPrice: 100,
		Quoter:       as,
	})

	fmt.Printf("fills=%d  volume=%d  finalPnL=%.2f  sharpe=%.2f  max|inv|=%d\n",
		r.Fills, r.Volume, r.FinalPnL, r.Sharpe, r.MaxAbsInventory)
}
