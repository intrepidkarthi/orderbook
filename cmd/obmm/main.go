// Command obmm runs an Avellaneda–Stoikov market maker through the backtest
// harness against synthetic noise flow and prints its scorecard.
//
//	go run ./cmd/obmm
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/intrepidkarthi/orderbook/pkg/backtest"
	"github.com/intrepidkarthi/orderbook/pkg/strategy"
)

func main() {
	params := strategy.ASParams{Gamma: 0.1, Kappa: 1.5, Sigma: 0.3}
	as, err := strategy.NewAvellanedaStoikov(params)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	cfg := backtest.Config{
		Symbol:       "SIM",
		Steps:        3000,
		Seed:         1,
		InitialPrice: 100,
		Quoter:       as,
	}
	r := backtest.Run(cfg)

	line := strings.Repeat("─", 52)
	fmt.Println(line)
	fmt.Println("  Avellaneda–Stoikov market-making backtest")
	fmt.Println(line)
	fmt.Printf("  params      γ=%.2f  k=%.2f  σ=%.2f\n", params.Gamma, params.Kappa, params.Sigma)
	fmt.Printf("  steps       %d   (seed %d)\n", cfg.Steps, cfg.Seed)
	fmt.Println(line)
	fmt.Printf("  fills       %d\n", r.Fills)
	fmt.Printf("  volume      %d\n", r.Volume)
	fmt.Printf("  max |inv|   %d\n", r.MaxAbsInventory)
	fmt.Printf("  final inv   %d\n", r.FinalInventory)
	fmt.Printf("  final PnL   %.2f\n", r.FinalPnL)
	fmt.Printf("  Sharpe      %.2f\n", r.Sharpe)
	fmt.Println(line)
	fmt.Printf("  PnL   %s\n", sparkline(r.PnL, 48))
	fmt.Printf("  inv   %s\n", sparkline(r.InventoryPath, 48))
	fmt.Println(line)
	fmt.Println("  Note: benign uninformed flow — the maker captures spread and")
	fmt.Println("  steers inventory to flat. Adverse-selection stress comes later.")
	fmt.Println(line)
}

// sparkline renders a series into a compact unicode bar strip of width buckets.
func sparkline(series []float64, width int) string {
	if len(series) == 0 {
		return ""
	}
	bars := []rune("▁▂▃▄▅▆▇█")

	// Downsample to `width` buckets (mean of each bucket).
	buckets := make([]float64, 0, width)
	n := len(series)
	for i := range width {
		lo := i * n / width
		hi := (i + 1) * n / width
		if hi <= lo {
			hi = lo + 1
		}
		if hi > n {
			hi = n
		}
		var sum float64
		for _, v := range series[lo:hi] {
			sum += v
		}
		buckets = append(buckets, sum/float64(hi-lo))
	}

	min, max := buckets[0], buckets[0]
	for _, v := range buckets {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	var b strings.Builder
	for _, v := range buckets {
		idx := 0
		if span > 0 {
			idx = int((v - min) / span * float64(len(bars)-1))
		}
		if idx < 0 {
			idx = 0
		}
		if idx >= len(bars) {
			idx = len(bars) - 1
		}
		b.WriteRune(bars[idx])
	}
	return b.String()
}
