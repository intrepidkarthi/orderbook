// Command flowstudy runs the retail order-flow experiments: how badly the
// standard aggressor-inference rules mislabel flow, whether CVD divergences and
// absorption predict anything, and what a constructed absorption episode looks
// like.
//
//	go run ./cmd/flowstudy
//
// Full write-up: docs/research/order-flow.md
package main

import (
	"fmt"
	"strings"

	"github.com/intrepidkarthi/orderbook/pkg/study"
)

// seeds is fixed so every run reproduces the write-up exactly.
var seeds = []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

const steps = 20000

func main() {
	rule := strings.Repeat("─", 76)

	fmt.Println(rule)
	fmt.Println("  Retail order flow — delta, CVD, and absorption")
	fmt.Println("  Testing \"trapped traders\" and \"CVD divergences mark reversals\"")
	fmt.Println(rule)

	inference()
	divergence()
	absorption()
	squeeze()

	fmt.Println(rule)
	fmt.Println("  Verdict: the primitives are real — absorption demonstrably holds price,")
	fmt.Println("  and delta/CVD are well-defined. The tradable claims are not. A CVD")
	fmt.Println("  divergence beats the base rate, but so does \"price made a new high\"")
	fmt.Println("  on its own, and by more — the CVD half adds nothing. Absorption")
	fmt.Println("  predicts nothing at all. And on real data none of it is even measured")
	fmt.Println("  correctly: the tick rule is 94% right per trade and still builds a CVD")
	fmt.Println("  that is wrong by 169% on average, sometimes with the opposite sign.")
	fmt.Println(rule)
}

// 1. How wrong is the inferred aggressor, and what does that do to CVD?
func inference() {
	fmt.Println("\n  1. Aggressor inference vs ground truth")
	fmt.Printf("     %-7s %-11s %-13s %-11s %-11s %s\n",
		"seed", "tick acc", "tick CVD err", "path R²", "true CVD", "tick CVD")

	var sumErr float64
	var signFlips int

	for _, seed := range seeds {
		r := study.RunInference(study.FlowConfig{Steps: steps, Seed: seed})
		sumErr += r.TickCVDError
		if (r.TrueCVD > 0) != (r.TickCVD > 0) {
			signFlips++
		}
		fmt.Printf("     %-7d %-11.4f %-13.2f %-11.4f %-11d %d\n",
			seed, r.TickAccuracy.Rate, r.TickCVDError, r.TickPathR2, r.TrueCVD, r.TickCVD)
	}

	fmt.Printf("\n     Mean tick-rule CVD error: %.0f%% of the true series.\n",
		100*sumErr/float64(len(seeds)))
	fmt.Printf("     Inferred CVD had the WRONG SIGN in %d of %d runs.\n", signFlips, len(seeds))
	fmt.Println("     Lee-Ready scores 100% here — an artifact of this simulator (every")
	fmt.Println("     aggressive order executes at the touch), not a result. See the write-up.")
}

// 2. Do CVD divergences precede reversals — and does the CVD half matter?
func divergence() {
	fmt.Println("\n  2. CVD divergence vs its base rate, and vs price alone")

	var divs, pos []study.SignalResult
	for _, seed := range seeds {
		r := study.RunDivergence(study.FlowConfig{Steps: steps, Seed: seed})
		divs = append(divs, r.Divergence)
		pos = append(pos, r.PriceOnly)
	}
	d, p := study.PoolSignals(divs), study.PoolSignals(pos)

	fmt.Printf("     pooled over %d seeds\n\n", len(seeds))
	printRate("CVD divergence", d)
	printRate("price extreme only", p)
	fmt.Printf("     %-22s %.4f\n", "base rate", d.BaseRate.Rate)

	fmt.Println()
	if d.Indistinct {
		fmt.Println("     Divergence does not separate from its base rate.")
	} else {
		fmt.Printf("     Divergence beats its base rate by %+.1f points.\n", 100*d.Edge)
	}
	if d.SignalRate.Overlaps(p.SignalRate) {
		fmt.Println("     But it is indistinguishable from the price-extreme control, which")
		fmt.Println("     scores higher. Dropping CVD entirely loses nothing — the effect is")
		fmt.Println("     mean reversion after a new extreme, and CVD is decoration.")
	} else {
		fmt.Println("     And it separates from the price-only control, so CVD earns its place.")
	}
}

// 3. Does absorption predict a move against the absorbed flow?
func absorption() {
	fmt.Println("\n  3. Absorption as a predictor")

	var all []study.SignalResult
	for _, seed := range seeds {
		all = append(all, study.RunAbsorption(study.FlowConfig{Steps: steps, Seed: seed}))
	}
	a := study.PoolSignals(all)

	fmt.Printf("     pooled over %d seeds\n\n", len(seeds))
	printRate("after absorption", a)
	fmt.Printf("     %-22s %.4f\n", "base rate", a.BaseRate.Rate)
	fmt.Printf("\n     Edge: %+.1f points. Intervals overlap: %v.\n", 100*a.Edge, a.Indistinct)
	if a.Indistinct {
		fmt.Println("     No detectable edge.")
	}
}

// 4. The mechanism itself, constructed on purpose.
func squeeze() {
	fmt.Println("\n  4. A constructed absorption episode (mechanism, not a strategy)")
	fmt.Printf("     %-7s %-11s %-15s %-16s %s\n",
		"seed", "absorbed", "move w/ wall", "move w/o wall", "reversal leg")

	for _, seed := range seeds[:5] {
		d := study.RunSqueezeDemo(study.SqueezeConfig{Seed: seed})
		fmt.Printf("     %-7d %-11d %-15.1f %-16.1f %+.1f\n",
			seed, d.AbsorbedLots, d.MoveWithWall, d.MoveWithoutWall, d.SqueezeMove)
	}
	fmt.Println("\n     The wall eats the entire sell order and price does not move. The same")
	fmt.Println("     pressure without it drops price by 50+ ticks. Absorption is real — which")
	fmt.Println("     is a different claim from section 3, where spotting one predicted nothing.")
}

func printRate(label string, r study.SignalResult) {
	fmt.Printf("     %-22s %.4f  [%.4f, %.4f]   n=%d\n",
		label, r.SignalRate.Rate, r.SignalRate.Lo, r.SignalRate.Hi, r.Episodes)
}
