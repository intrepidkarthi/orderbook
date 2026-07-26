package study

import (
	"math/rand"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/signals"
	"github.com/intrepidkarthi/orderbook/pkg/sim"
)

// floatMid returns the mid price as a float so half-tick moves survive. The
// engine's integer MidPrice floors, which quantizes away exactly the small moves
// a price-impact regression is trying to measure. Falls back to the last trade
// when the book is one-sided.
func floatMid(eng *matching.Engine) (float64, bool) {
	bid, _, hasBid := eng.BestBid()
	ask, _, hasAsk := eng.BestAsk()
	if hasBid && hasAsk {
		return (float64(bid) + float64(ask)) / 2, true
	}
	if ltp := eng.LastTradePrice(); ltp > 0 {
		return float64(ltp), true
	}
	return 0, false
}

// touchDepth sums the resting lots across the top n levels of both sides — the
// liquidity actually available to absorb an aggressive order.
func touchDepth(eng *matching.Engine, levels int) float64 {
	sn := eng.Snapshot(levels)
	var total int64
	for _, l := range sn.Bids {
		total += l.Quantity
	}
	for _, l := range sn.Asks {
		total += l.Quantity
	}
	return float64(total)
}

// CalibrationConfig parameterizes the estimator-validation experiment.
type CalibrationConfig struct {
	Lambda  float64 // the true λ to inject
	NoiseSD float64 // standard deviation of the additive price noise (ticks)
	N       int     // number of synthetic intervals
	Seed    int64
}

// RunLambdaCalibration builds a synthetic path ΔP = λ·y + ε from a *known* λ and
// re-estimates it.
//
// This validates the instrument, nothing more. Recovering a λ that was injected
// by construction is circular and is not evidence about markets — it is here so
// that every λ measured below can be trusted to be a property of the book rather
// than an artefact of the regression (docs/research-roadmap.md §2).
func RunLambdaCalibration(cfg CalibrationConfig) signals.LambdaFit {
	if cfg.N <= 0 {
		cfg.N = 1000
	}
	rng := rand.New(rand.NewSource(cfg.Seed))

	flow := make([]float64, cfg.N)
	dPrice := make([]float64, cfg.N)
	for i := range flow {
		flow[i] = rng.NormFloat64() * 50
		dPrice[i] = cfg.Lambda*flow[i] + rng.NormFloat64()*cfg.NoiseSD
	}
	return signals.EstimateLambda(flow, dPrice)
}

// KyleConfig parameterizes the emergent-λ experiments.
type KyleConfig struct {
	Symbol       string
	Steps        int
	Warmup       int // steps run before sampling, to build a two-sided book
	Seed         int64
	InitialPrice int64 // ticks
	DepthLevels  int   // levels per side counted as "depth at the touch"
	Noise        *sim.NoiseTrader
}

func (c *KyleConfig) applyDefaults() {
	if c.Symbol == "" {
		c.Symbol = "SIM"
	}
	if c.Steps <= 0 {
		c.Steps = 5000
	}
	if c.Warmup <= 0 {
		c.Warmup = 200
	}
	if c.InitialPrice == 0 {
		c.InitialPrice = 1000
	}
	if c.DepthLevels <= 0 {
		c.DepthLevels = 5
	}
	if c.Noise == nil {
		// The same reasoning as the OFI study: on an integer-tick grid a shallow
		// book quantizes small price moves away. A wider band of resting levels
		// leaves room for flow to move the mid by measurable amounts.
		nt := sim.DefaultNoiseTrader("noise")
		nt.MaxOffsetTicks = 40
		c.Noise = nt
	}
}

// KyleResult is a fitted price-impact relation plus the liquidity it was
// measured against.
type KyleResult struct {
	Fit      signals.LambdaFit
	AvgDepth float64 // mean resting lots across DepthLevels levels per side
	Trades   int     // executions observed over the sampled window
}

// RunKyleLambda measures the λ that *emerges* from a real matching engine. No λ
// is configured anywhere: it falls out of the book's depth and the matching
// rules. Each step contributes one (signed flow, Δmid) pair, and the pairs are
// regressed per Kyle (1985).
func RunKyleLambda(cfg KyleConfig) KyleResult {
	cfg.applyDefaults()
	rng := rand.New(rand.NewSource(cfg.Seed))
	eng := matching.NewEngine(matching.DefaultConfig(cfg.Symbol))

	flow := make([]float64, 0, cfg.Steps)
	dMid := make([]float64, 0, cfg.Steps)
	var depthSum float64
	var depthN, tradeCount int

	ref := cfg.InitialPrice
	for step := 0; step < cfg.Warmup+cfg.Steps; step++ {
		if mid, ok := eng.MidPrice(); ok {
			ref = mid
		} else if ltp := eng.LastTradePrice(); ltp > 0 {
			ref = ltp
		}

		before, hadMid := floatMid(eng)
		sampling := step >= cfg.Warmup

		view := sim.View{
			Symbol:   cfg.Symbol,
			Step:     step,
			Snapshot: eng.Snapshot(10),
			Ref:      ref,
			HasBook:  eng.OrderCount() > 0,
		}

		var y int64
		for _, o := range cfg.Noise.Act(view, rng) {
			r := eng.Process(o)
			y += signals.SignedFlow(r.Trades)
			tradeCount += len(r.Trades)
		}

		after, hasMid := floatMid(eng)
		if sampling && hadMid && hasMid {
			flow = append(flow, float64(y))
			dMid = append(dMid, after-before)
			depthSum += touchDepth(eng, cfg.DepthLevels)
			depthN++
		}
	}

	res := KyleResult{Fit: signals.EstimateLambda(flow, dMid), Trades: tradeCount}
	if depthN > 0 {
		res.AvgDepth = depthSum / float64(depthN)
	}
	return res
}

// DepthPoint is one rung of the λ-versus-depth sweep.
type DepthPoint struct {
	SizeScale    int     // multiplier applied to noise-trader order sizes
	AvgDepth     float64 // measured resting lots at the touch
	Fit          signals.LambdaFit
	LambdaXDepth float64 // λ · depth — roughly constant if λ ∝ 1/depth holds
}

// RunKyleDepth re-estimates λ across books of increasing depth. Depth is varied
// by scaling the noise traders' order sizes, then *measured* rather than assumed,
// so the x-axis is observed liquidity and not the knob that produced it.
//
// Kyle predicts λ ∝ 1/depth, which makes λ·depth the quantity to watch: flat
// means the relation holds.
func RunKyleDepth(cfg KyleConfig, scales []int) []DepthPoint {
	if len(scales) == 0 {
		scales = []int{1, 2, 4, 8}
	}
	out := make([]DepthPoint, 0, len(scales))

	for _, scale := range scales {
		if scale <= 0 {
			continue
		}
		sub := cfg
		sub.applyDefaults()

		scaled := *sub.Noise // copy: never mutate the caller's agent
		scaled.MinSize *= scale
		scaled.MaxSize *= scale
		sub.Noise = &scaled

		r := RunKyleLambda(sub)
		out = append(out, DepthPoint{
			SizeScale:    scale,
			AvgDepth:     r.AvgDepth,
			Fit:          r.Fit,
			LambdaXDepth: r.Fit.Lambda * r.AvgDepth,
		})
	}
	return out
}
