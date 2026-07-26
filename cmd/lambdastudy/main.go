// Command lambdastudy runs the Kyle's-lambda price-impact experiments: what λ
// emerges from a real book, how it scales with depth, and what it costs to
// execute a large order all at once instead of gradually.
//
//	go run ./cmd/lambdastudy
//
// Full write-up: docs/research/kyle-lambda.md
package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/intrepidkarthi/orderbook/pkg/study"
)

// seeds is fixed so every run of this command reproduces the write-up exactly.
var seeds = []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

// pad left-aligns s in a field n runes wide. fmt's %-Ns pads by bytes, which
// misaligns every column header containing λ, ², or ·.
func pad(s string, n int) string {
	if d := n - utf8.RuneCountInString(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s + " "
}

func header(cols []string, widths []int) {
	var b strings.Builder
	b.WriteString("     ")
	for i, c := range cols {
		b.WriteString(pad(c, widths[i]))
	}
	fmt.Println(strings.TrimRight(b.String(), " "))
}

func main() {
	rule := strings.Repeat("─", 74)

	fmt.Println(rule)
	fmt.Println("  Kyle's λ — price impact per unit of order flow")
	fmt.Println("  ΔP = λ·y, where y is signed aggressor volume (Kyle 1985)")
	fmt.Println(rule)

	calibration()
	emergent()
	depthSweep()
	execution(rule)
}

// 1. Does the estimator work at all?
func calibration() {
	fmt.Println("\n  1. Estimator calibration — inject a known λ, recover it")
	header([]string{"true λ", "recovered", "R²", "noise σ"}, []int{13, 13, 11, 8})
	for _, noise := range []float64{2, 5, 20} {
		fit := study.RunLambdaCalibration(study.CalibrationConfig{
			Lambda: 0.25, NoiseSD: noise, N: 4000, Seed: 1,
		})
		fmt.Printf("     %-13.4f%-13.4f%-11.4f%.0f\n", 0.25, fit.Lambda, fit.R2, noise)
	}
	fmt.Println("     Circular by construction — this validates the ruler, not the market.")
}

// 2. What λ does a real book produce, with nothing configured?
func emergent() {
	fmt.Println("\n  2. Emergent λ — no λ configured; it falls out of the book")
	header([]string{"seed", "λ (ticks/lot)", "R²", "depth", "trades"}, []int{9, 15, 11, 11, 8})
	for _, seed := range seeds[:5] {
		r := study.RunKyleLambda(study.KyleConfig{Steps: 5000, Seed: seed})
		fmt.Printf("     %-9d%-15.5f%-11.4f%-11.1f%d\n",
			seed, r.Fit.Lambda, r.Fit.R2, r.AvgDepth, r.Trades)
	}
}

// 3. Kyle's central prediction: λ ∝ 1/depth.
func depthSweep() {
	fmt.Println("\n  3. λ versus depth — Kyle predicts λ ∝ 1/depth, so λ·depth is flat")
	header([]string{"scale", "depth (lots)", "λ (ticks/lot)", "R²", "λ·depth"},
		[]int{9, 15, 16, 11, 9})
	for _, p := range study.RunKyleDepth(study.KyleConfig{Steps: 5000, Seed: 1}, []int{1, 2, 4, 8}) {
		fmt.Printf("     %-9d%-15.1f%-16.5f%-11.4f%.1f\n",
			p.SizeScale, p.AvgDepth, p.Fit.Lambda, p.Fit.R2, p.LambdaXDepth)
	}
}

// 4. What impact costs in practice: one block versus a schedule.
func execution(rule string) {
	fmt.Println("\n  4. Execution — 400 lots as one block vs 10 slices, same book and seed")

	var blockSlip, slicedSlip, blockReal, slicedReal, blockPerm, slicedPerm float64
	var blockUnfilled, slicedUnfilled int64
	cheaper := 0

	for _, seed := range seeds {
		r := study.RunExecution(study.ExecutionConfig{
			Seed: seed, Quantity: 400, Slices: 10, SliceGap: 20,
		})
		blockSlip += r.Block.SlipPerLot
		slicedSlip += r.Sliced.SlipPerLot
		blockReal += r.Block.RealizedImpact
		slicedReal += r.Sliced.RealizedImpact
		blockPerm += r.Block.PermanentImpact
		slicedPerm += r.Sliced.PermanentImpact
		blockUnfilled += r.Block.Unfilled
		slicedUnfilled += r.Sliced.Unfilled
		if r.Sliced.SlipPerLot < r.Block.SlipPerLot {
			cheaper++
		}
	}

	n := float64(len(seeds))
	fmt.Printf("     averaged over %d seeds, ticks unless noted\n\n", len(seeds))
	fmt.Printf("     %-26s%-14s%s\n", "", "block", "sliced")
	fmt.Printf("     %-26s%-14.2f%.2f\n", "slippage per lot", blockSlip/n, slicedSlip/n)
	fmt.Printf("     %-26s%-14.2f%.2f\n", "realized impact", blockReal/n, slicedReal/n)
	fmt.Printf("     %-26s%-14.2f%.2f\n", "permanent impact", blockPerm/n, slicedPerm/n)
	fmt.Printf("     %-26s%-14d%d\n", "unfilled lots (total)", blockUnfilled, slicedUnfilled)
	fmt.Printf("\n     Slicing was cheaper in %d of %d runs.\n", cheaper, len(seeds))

	fmt.Println(rule)
	fmt.Println("  Verdict: λ is a property of liquidity, not of the asset — it falls")
	fmt.Println("  roughly as 1/depth. Executing all at once pays that impact in full")
	fmt.Println("  and can exhaust the book outright; spreading the same quantity pays")
	fmt.Println("  less and completes. Permanent impact is unchanged either way — the")
	fmt.Println("  same volume ultimately trades. Slicing avoids the temporary cost,")
	fmt.Println("  not the permanent one.")
	fmt.Println(rule)
}
