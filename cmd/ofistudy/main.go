// Command ofistudy runs the order-flow-imbalance experiment and prints whether
// OFI's link to price is contemporaneous or predictive.
//
//	go run ./cmd/ofistudy
package main

import (
	"fmt"
	"strings"

	"github.com/intrepidkarthi/orderbook/pkg/study"
)

func main() {
	line := strings.Repeat("─", 60)
	fmt.Println(line)
	fmt.Println("  Order-Flow Imbalance: contemporaneous vs predictive")
	fmt.Println("  (Testing the \"order book predicts the next move\" claim)")
	fmt.Println(line)
	fmt.Printf("  %-6s %-8s %-14s %-12s %-14s %-12s\n",
		"seed", "N", "contemp R²", "pred R²", "contemp slope", "pred slope")

	const seeds = 10
	var sumContemp, sumPred float64
	for seed := int64(1); seed <= seeds; seed++ {
		r := study.RunOFI(study.OFIConfig{
			Steps:        5000,
			Seed:         seed,
			InitialPrice: 100,
		})
		fmt.Printf("  %-6d %-8d %-14.4f %-12.4f %+-14.5f %+-12.5f\n",
			seed, r.N, r.ContemporaneousR2, r.PredictiveR2,
			r.ContemporaneousSlope, r.PredictiveSlope)
		sumContemp += r.ContemporaneousR2
		sumPred += r.PredictiveR2
	}
	meanContemp := sumContemp / seeds
	meanPred := sumPred / seeds
	fmt.Printf("  %-6s %-8s %-14.4f %-12.4f\n", "mean", "", meanContemp, meanPred)

	fmt.Println(line)
	// Computed, not written down. The verdict used to carry the numbers as literal
	// text and drifted from its own output the moment the measurement changed — which
	// is exactly the failure the rest of this repository spends its time removing.
	gap := 0.0
	if meanPred > 0 {
		gap = meanContemp / meanPred
	}
	fmt.Printf("  Verdict: OFI explains ~%.0f%% of the SAME-interval move but ~%.2f%%\n",
		meanContemp*100, meanPred*100)
	fmt.Printf("  of the NEXT one — a ~%.0fx gap. The order book describes the move\n", gap)
	fmt.Println("  as it happens; it does not forecast it. Contemporaneous ≠ predictive.")
	fmt.Println("  Full write-up: docs/research/ofi.md")
	fmt.Println(line)
}
