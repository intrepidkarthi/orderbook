// Package study contains reproducible microstructure experiments that put the
// popular trading claims to the test on controllable, deterministic data.
//
// The first experiment tackles the headline claim from the order-flow-imbalance
// literature and its retail retelling ("the order book predicts the next move").
// It reproduces the distinction the research plan insists on (docs/research-
// roadmap.md §1): OFI's relationship to price is strong *contemporaneously* (over
// the same interval) but far weaker as a *forecast* of the next interval.
package study

import (
	"math/rand"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/signals"
	"github.com/intrepidkarthi/orderbook/pkg/sim"
)

// OFIConfig parameterizes the OFI study.
type OFIConfig struct {
	Symbol       string
	Steps        int
	Seed         int64
	InitialPrice int64 // ticks
	Noise        *sim.NoiseTrader
}

func (c *OFIConfig) applyDefaults() {
	if c.Symbol == "" {
		c.Symbol = "SIM"
	}
	if c.Steps <= 0 {
		c.Steps = 5000
	}
	if c.InitialPrice == 0 {
		c.InitialPrice = 100
	}
	if c.Noise == nil {
		// A deeper resting book than the shallow default: on the integer-tick grid,
		// best-level order flow needs enough price levels around the touch to move
		// the mid cleanly, which is what the OFI↔Δprice regression measures. A
		// 5-tick book quantizes the signal away; ~40 ticks restores it.
		nt := sim.DefaultNoiseTrader("noise")
		nt.MaxOffsetTicks = 40
		c.Noise = nt
	}
}

// OFIResult holds the two regressions that make the point.
type OFIResult struct {
	N                    int     // number of (OFI, Δprice) intervals used
	ContemporaneousR2    float64 // Δprice[i] regressed on OFI[i]
	ContemporaneousSlope float64
	PredictiveR2         float64 // Δprice[i+1] regressed on OFI[i]
	PredictiveSlope      float64
}

// RunOFI drives the simulator, then regresses mid-price change on best-level OFI
// both contemporaneously and one step ahead.
func RunOFI(cfg OFIConfig) OFIResult {
	cfg.applyDefaults()
	rng := rand.New(rand.NewSource(cfg.Seed))
	eng := matching.NewEngine(matching.DefaultConfig(cfg.Symbol))

	snaps := make([]*orderbook.Snapshot, 0, cfg.Steps)
	mids := make([]float64, 0, cfg.Steps)
	lastMid := cfg.InitialPrice

	for step := 0; step < cfg.Steps; step++ {
		if mid, ok := eng.MidPrice(); ok {
			lastMid = mid
		} else if ltp := eng.LastTradePrice(); ltp > 0 {
			lastMid = ltp
		}
		view := sim.View{
			Symbol:   cfg.Symbol,
			Step:     step,
			Snapshot: eng.Snapshot(10),
			Ref:      lastMid,
			HasBook:  eng.OrderCount() > 0,
		}
		for _, o := range cfg.Noise.Act(view, rng) {
			eng.Process(o)
		}

		// Capture the post-step best level and mid. The regression needs the mid at
		// full resolution, so compute it as a float from the best levels rather than
		// the engine's integer-floored MidPrice (which would quantize away half-tick
		// moves and drown the OFI signal in rounding noise).
		sn := eng.Snapshot(1)
		snaps = append(snaps, sn)
		if mid, ok := eng.MidPrice(); ok {
			lastMid = mid
		}
		midF := float64(lastMid)
		if len(sn.Bids) > 0 && len(sn.Asks) > 0 {
			midF = (float64(sn.Bids[0].Price) + float64(sn.Asks[0].Price)) / 2
		}
		mids = append(mids, midF)
	}

	// Per-interval OFI and contemporaneous price change.
	ofi := make([]float64, 0, len(snaps))
	dContemp := make([]float64, 0, len(snaps))
	for i := 1; i < len(snaps); i++ {
		ofi = append(ofi, float64(signals.OFIStep(snaps[i-1], snaps[i])))
		dContemp = append(dContemp, mids[i]-mids[i-1])
	}

	// Predictive: this interval's OFI vs next interval's price change.
	ofiPrev := make([]float64, 0, len(ofi))
	dNext := make([]float64, 0, len(ofi))
	for i := 0; i < len(ofi)-1; i++ {
		ofiPrev = append(ofiPrev, ofi[i])
		dNext = append(dNext, dContemp[i+1])
	}

	cSlope, _, cR2 := signals.LinReg(ofi, dContemp)
	pSlope, _, pR2 := signals.LinReg(ofiPrev, dNext)

	return OFIResult{
		N:                    len(ofi),
		ContemporaneousR2:    cR2,
		ContemporaneousSlope: cSlope,
		PredictiveR2:         pR2,
		PredictiveSlope:      pSlope,
	}
}
