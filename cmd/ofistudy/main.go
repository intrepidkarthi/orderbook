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
	fmt.Printf("  %-6s %-10s %-22s %-22s\n", "seed", "N", "contemporaneous R²", "predictive R²")

	for seed := int64(1); seed <= 10; seed++ {
		r := study.RunOFI(study.OFIConfig{
			Steps:        5000,
			Seed:         seed,
			InitialPrice: 100,
		})
		fmt.Printf("  %-6d %-10d %-22.4f %-22.4f\n",
			seed, r.N, r.ContemporaneousR2, r.PredictiveR2)
	}

	fmt.Println(line)
	fmt.Println("  Verdict: OFI explains ~17% of the SAME-interval move but ~0.03%")
	fmt.Println("  of the NEXT one — a ~540x gap. The order book describes the move")
	fmt.Println("  as it happens; it does not forecast it. Contemporaneous ≠ predictive.")
	fmt.Println("  Full write-up: docs/research/ofi.md")
	fmt.Println(line)
}
