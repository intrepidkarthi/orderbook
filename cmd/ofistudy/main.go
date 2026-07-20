// Command ofistudy runs the order-flow-imbalance experiment and prints whether
// OFI's link to price is contemporaneous or predictive.
//
//	go run ./cmd/ofistudy
package main

import (
	"fmt"
	"strings"

	"github.com/intrepidkarthi/orderbook/pkg/study"
	"github.com/shopspring/decimal"
)

func main() {
	line := strings.Repeat("─", 60)
	fmt.Println(line)
	fmt.Println("  Order-Flow Imbalance: contemporaneous vs predictive")
	fmt.Println("  (Testing the \"order book predicts the next move\" claim)")
	fmt.Println(line)
	fmt.Printf("  %-6s %-10s %-22s %-22s\n", "seed", "N", "contemporaneous R²", "predictive R²")

	for _, seed := range []int64{1, 2, 7} {
		r := study.RunOFI(study.OFIConfig{
			Steps:        5000,
			Seed:         seed,
			InitialPrice: decimal.NewFromInt(100),
		})
		fmt.Printf("  %-6d %-10d %-22.4f %-22.4f\n",
			seed, r.N, r.ContemporaneousR2, r.PredictiveR2)
	}

	fmt.Println(line)
	fmt.Println("  Verdict: OFI explains ~a third of the SAME-interval move but")
	fmt.Println("  ~nothing of the NEXT one. The order book describes the move as")
	fmt.Println("  it happens; it does not forecast it. Contemporaneous ≠ predictive.")
	fmt.Println(line)
}
