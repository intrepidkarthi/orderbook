package study

import (
	"math"
	"math/rand"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/signals"
	"github.com/intrepidkarthi/orderbook/pkg/sim"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// FlowConfig parameterizes the order-flow experiments (docs/research-roadmap.md
// §4).
type FlowConfig struct {
	Symbol       string
	Steps        int
	Warmup       int
	Seed         int64
	InitialPrice int64 // ticks
	Window       int   // steps per divergence / absorption window
	Horizon      int   // steps ahead over which an outcome is judged
	Noise        *sim.NoiseTrader
}

func (c *FlowConfig) applyDefaults() {
	if c.Symbol == "" {
		c.Symbol = "SIM"
	}
	if c.Steps <= 0 {
		c.Steps = 20000
	}
	if c.Warmup <= 0 {
		c.Warmup = 200
	}
	if c.InitialPrice == 0 {
		c.InitialPrice = 1000
	}
	if c.Window <= 0 {
		c.Window = 20
	}
	if c.Horizon <= 0 {
		c.Horizon = 20
	}
	if c.Noise == nil {
		nt := sim.DefaultNoiseTrader("noise")
		nt.MaxOffsetTicks = 40
		c.Noise = nt
	}
}

// flowTrade is one execution together with the quote context an inference rule
// needs to classify it.
type flowTrade struct {
	Price     int64
	MidBefore float64 // prevailing mid immediately before the trade
	TrueSide  types.Side
	Qty       int64
}

// flowRun is a single simulator pass, shared by every experiment below so they
// all describe the same market rather than three differently-seeded ones.
type flowRun struct {
	trades    []flowTrade
	stepMid   []float64 // mid at the end of each sampled step
	stepDelta []int64   // true signed aggressor volume within each sampled step
}

// driveFlow runs the simulator once, recording per-trade quote context and
// per-step price and delta.
func driveFlow(cfg FlowConfig) flowRun {
	rng := rand.New(rand.NewSource(cfg.Seed))
	eng := matching.NewEngine(matching.DefaultConfig(cfg.Symbol))

	run := flowRun{
		stepMid:   make([]float64, 0, cfg.Steps),
		stepDelta: make([]int64, 0, cfg.Steps),
	}
	ref := cfg.InitialPrice

	for step := 0; step < cfg.Warmup+cfg.Steps; step++ {
		if mid, ok := eng.MidPrice(); ok {
			ref = mid
		} else if ltp := eng.LastTradePrice(); ltp > 0 {
			ref = ltp
		}
		sampling := step >= cfg.Warmup

		view := sim.View{
			Symbol:   cfg.Symbol,
			Step:     step,
			Snapshot: eng.Snapshot(10),
			Ref:      ref,
			HasBook:  eng.OrderCount() > 0,
		}

		var delta int64
		for _, o := range cfg.Noise.Act(view, rng) {
			// The quote prevailing *before* the order is what Lee-Ready classifies
			// against, so it must be captured per order rather than per step.
			midBefore, _ := floatMid(eng)

			for _, tr := range eng.Process(o).Trades {
				delta += signedQty(tr)
				if sampling {
					run.trades = append(run.trades, flowTrade{
						Price:     tr.Price,
						MidBefore: midBefore,
						TrueSide:  tr.TakerSide,
						Qty:       tr.Quantity,
					})
				}
			}
		}

		if mid, ok := floatMid(eng); ok && sampling {
			run.stepMid = append(run.stepMid, mid)
			run.stepDelta = append(run.stepDelta, delta)
		}
	}
	return run
}

func signedQty(t *types.Trade) int64 {
	if t.TakerSide == types.SideBuy {
		return t.Quantity
	}
	return -t.Quantity
}

// InferenceResult reports how badly the standard aggressor-inference rules
// mislabel flow, and — the part that matters — what that does to CVD.
type InferenceResult struct {
	Trades int

	TickAccuracy     signals.Interval
	LeeReadyAccuracy signals.Interval

	TrueCVD     int64
	TickCVD     int64
	LeeReadyCVD int64

	// CVD error as a fraction of the true series' final magnitude. This is the
	// number a practitioner actually feels: nobody trades a per-trade label, they
	// trade the cumulative series built from thousands of them.
	TickCVDError     float64
	LeeReadyCVDError float64

	// R² between the inferred and true CVD *paths*, not just their endpoints.
	TickPathR2     float64
	LeeReadyPathR2 float64
}

// RunInference classifies every simulated trade with the tick rule and with
// Lee-Ready, scores both against the engine's ground truth, and propagates the
// errors into CVD.
//
// This experiment is only possible because the engine assigns TakerSide. No
// market-data feed publishes it at any granularity, so on real data there is
// nothing to check the inference against (docs/research-roadmap.md §0).
func RunInference(cfg FlowConfig) InferenceResult {
	cfg.applyDefaults()
	run := driveFlow(cfg)

	var tickRight, leeRight int
	var trueCVD, tickCVD, leeCVD int64

	truePath := make([]float64, 0, len(run.trades))
	tickPath := make([]float64, 0, len(run.trades))
	leePath := make([]float64, 0, len(run.trades))

	prevPrice := cfg.InitialPrice
	tickPrev, leePrev := types.SideBuy, types.SideBuy

	for _, t := range run.trades {
		tickSide := signals.TickRuleSide(t.Price, prevPrice, tickPrev)
		leeSide := signals.LeeReadySide(t.Price, t.MidBefore, prevPrice, leePrev)

		if tickSide == t.TrueSide {
			tickRight++
		}
		if leeSide == t.TrueSide {
			leeRight++
		}

		trueCVD += signedBy(t.TrueSide, t.Qty)
		tickCVD += signedBy(tickSide, t.Qty)
		leeCVD += signedBy(leeSide, t.Qty)

		truePath = append(truePath, float64(trueCVD))
		tickPath = append(tickPath, float64(tickCVD))
		leePath = append(leePath, float64(leeCVD))

		prevPrice, tickPrev, leePrev = t.Price, tickSide, leeSide
	}

	n := len(run.trades)
	res := InferenceResult{
		Trades:           n,
		TickAccuracy:     signals.WilsonInterval(tickRight, n, signals.Z95),
		LeeReadyAccuracy: signals.WilsonInterval(leeRight, n, signals.Z95),
		TrueCVD:          trueCVD,
		TickCVD:          tickCVD,
		LeeReadyCVD:      leeCVD,
	}

	if scale := math.Abs(float64(trueCVD)); scale > 0 {
		res.TickCVDError = math.Abs(float64(tickCVD-trueCVD)) / scale
		res.LeeReadyCVDError = math.Abs(float64(leeCVD-trueCVD)) / scale
	}
	_, _, res.TickPathR2 = signals.LinReg(tickPath, truePath)
	_, _, res.LeeReadyPathR2 = signals.LinReg(leePath, truePath)

	return res
}

func signedBy(side types.Side, qty int64) int64 {
	if side == types.SideBuy {
		return qty
	}
	return -qty
}

// SignalResult is a conditional outcome rate reported the only way it means
// anything: beside the unconditional base rate over the same data.
//
// The raw counts are carried alongside the intervals so results from several
// seeds can be pooled exactly (see PoolSignals). Averaging rates across runs
// would be wrong — it weights a run with thirty episodes the same as one with
// three hundred.
type SignalResult struct {
	Episodes   int              // times the signal fired
	SignalRate signals.Interval // outcome rate following the signal
	BaseRate   signals.Interval // outcome rate over all windows
	Edge       float64          // SignalRate.Rate − BaseRate.Rate
	Indistinct bool             // true when the intervals overlap

	SignalHits, SignalN int // raw counts behind SignalRate
	BaseHits, BaseN     int // raw counts behind BaseRate
}

// newSignalResult builds a result from raw counts.
func newSignalResult(sigHits, sigN, baseHits, baseN int) SignalResult {
	sig := signals.WilsonInterval(sigHits, sigN, signals.Z95)
	base := signals.WilsonInterval(baseHits, baseN, signals.Z95)
	return SignalResult{
		Episodes:   sigN,
		SignalRate: sig,
		BaseRate:   base,
		Edge:       sig.Rate - base.Rate,
		Indistinct: sig.Overlaps(base),
		SignalHits: sigHits,
		SignalN:    sigN,
		BaseHits:   baseHits,
		BaseN:      baseN,
	}
}

// PoolSignals combines per-seed results by summing their underlying counts.
//
// A single 20,000-step run yields only a few hundred signal episodes, which
// leaves a confidence interval far too wide to separate a small edge from
// nothing. Pooling across seeds is how the question gets answered instead of
// shrugged at.
func PoolSignals(results []SignalResult) SignalResult {
	var sigHits, sigN, baseHits, baseN int
	for _, r := range results {
		sigHits += r.SignalHits
		sigN += r.SignalN
		baseHits += r.BaseHits
		baseN += r.BaseN
	}
	return newSignalResult(sigHits, sigN, baseHits, baseN)
}

// DivergenceResult separates two questions that are easy to conflate.
//
// Divergence answers "does the signal beat the base rate?". PriceOnly answers
// the question that actually decides whether CVD earns its place: strip the CVD
// condition out, keep only "price made a new extreme", and see whether the
// remaining signal does just as well. A divergence detector that merely
// rediscovers mean reversion in price would pass the first test and fail this
// one.
type DivergenceResult struct {
	Divergence SignalResult // full CVD divergence, against the base rate
	PriceOnly  SignalResult // the same directional call with the CVD condition removed
	CVDAdds    bool         // true when the two hit-rates are distinguishable
}

// RunDivergence tests the claim that a CVD divergence precedes a reversal.
//
// A bearish divergence (price makes a higher high, cumulative delta does not) is
// scored a hit when the mid is lower one horizon later; a bullish divergence when
// it is higher. The base rate is the same outcome measured over every window,
// signal or not — without it, a 55% hit-rate reads as an edge when the market
// fell 54% of the time regardless.
func RunDivergence(cfg FlowConfig) DivergenceResult {
	cfg.applyDefaults()
	run := driveFlow(cfg)

	windows := windowExtremes(run, cfg.Window)
	if len(windows) < 2 {
		return DivergenceResult{}
	}

	var divHits, nBear, nBull int
	var poHits, poBear, poBull int
	var downHits, upHits, baseN int

	for i := 1; i < len(windows); i++ {
		prev, w := windows[i-1].ext, windows[i].ext
		end := windows[i].endStep
		if end+cfg.Horizon >= len(run.stepMid) {
			break
		}
		future := run.stepMid[end+cfg.Horizon]
		now := run.stepMid[end]
		fell, rose := future < now, future > now

		// Both directions are tracked unconditionally, because the signal predicts
		// down sometimes and up others.
		baseN++
		if fell {
			downHits++
		}
		if rose {
			upHits++
		}

		switch signals.Divergence(prev, w) {
		case signals.BearishDivergence:
			nBear++
			if fell {
				divHits++
			}
		case signals.BullishDivergence:
			nBull++
			if rose {
				divHits++
			}
		}

		// The control: the identical directional call with the CVD condition
		// dropped, keeping only "price made a new extreme". Same precedence as
		// Divergence, so the two differ in exactly one condition.
		switch {
		case w.PriceHigh > prev.PriceHigh:
			poBear++
			if fell {
				poHits++
			}
		case w.PriceLow < prev.PriceLow:
			poBull++
			if rose {
				poHits++
			}
		}
	}

	if baseN == 0 {
		return DivergenceResult{}
	}

	// The base rate has to be built from the same mix of directional calls the
	// signal actually made. Comparing a signal that is part bearish and part
	// bullish against P(price falls) alone would credit or penalise it for nothing
	// more than the market's drift.
	pDown := float64(downHits) / float64(baseN)
	pUp := float64(upHits) / float64(baseN)

	mixedBase := func(bear, bull int) int {
		n := bear + bull
		if n == 0 {
			return 0
		}
		rate := (float64(bear)*pDown + float64(bull)*pUp) / float64(n)
		return int(math.Round(rate * float64(baseN)))
	}

	div := newSignalResult(divHits, nBear+nBull, mixedBase(nBear, nBull), baseN)
	po := newSignalResult(poHits, poBear+poBull, mixedBase(poBear, poBull), baseN)

	return DivergenceResult{
		Divergence: div,
		PriceOnly:  po,
		CVDAdds:    !div.SignalRate.Overlaps(po.SignalRate),
	}
}

// RunAbsorption tests the squeeze reading of absorption: when heavy aggressive
// flow is eaten without moving price, does price then travel *against* that flow?
//
// Absorption itself is a real mechanic — RunSqueezeDemo constructs one
// deliberately. This measures something different and much harder: whether
// spotting one tells you anything about what happens next.
func RunAbsorption(cfg FlowConfig) SignalResult {
	cfg.applyDefaults()
	run := driveFlow(cfg)

	det := signals.AbsorptionConfig{
		MinDelta:  absorptionMinDelta(run, cfg.Window),
		MaxMove:   1,
		MinTrades: 1,
	}

	var sigHits, sigN, baseHits, baseN int

	for start := 0; start+cfg.Window+cfg.Horizon < len(run.stepMid); start += cfg.Window {
		end := start + cfg.Window
		var delta int64
		for _, d := range run.stepDelta[start:end] {
			delta += d
		}
		move := int64(run.stepMid[end] - run.stepMid[start])
		future := run.stepMid[end+cfg.Horizon]

		// The squeeze reading: absorbed selling should be followed by a rise, and
		// absorbed buying by a fall. The base rate is the same directional question
		// asked of every window.
		baseN++
		if (delta < 0 && future > run.stepMid[end]) || (delta >= 0 && future < run.stepMid[end]) {
			baseHits++
		}

		if det.Absorbed(delta, move, cfg.Window) {
			sigN++
			if (delta < 0 && future > run.stepMid[end]) || (delta >= 0 && future < run.stepMid[end]) {
				sigHits++
			}
		}
	}

	return newSignalResult(sigHits, sigN, baseHits, baseN)
}

// absorptionMinDelta sets the "heavy flow" threshold from the run's own
// distribution — the 75th percentile of absolute window delta — rather than a
// magic constant that would mean different things at different noise settings.
func absorptionMinDelta(run flowRun, window int) int64 {
	var mags []int64
	for start := 0; start+window < len(run.stepDelta); start += window {
		var delta int64
		for _, d := range run.stepDelta[start : start+window] {
			delta += d
		}
		if delta < 0 {
			delta = -delta
		}
		mags = append(mags, delta)
	}
	if len(mags) == 0 {
		return 1
	}
	sortInt64(mags)
	return mags[len(mags)*3/4]
}

func sortInt64(v []int64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

type windowExt struct {
	ext     signals.Extremes
	endStep int
}

// windowExtremes reduces the run to per-window price and CVD extremes.
func windowExtremes(run flowRun, window int) []windowExt {
	var out []windowExt
	var cvd float64

	for start := 0; start+window <= len(run.stepMid); start += window {
		e := signals.Extremes{
			PriceHigh: math.Inf(-1), PriceLow: math.Inf(1),
			CVDHigh: math.Inf(-1), CVDLow: math.Inf(1),
		}
		for i := start; i < start+window; i++ {
			cvd += float64(run.stepDelta[i])
			e.PriceHigh = math.Max(e.PriceHigh, run.stepMid[i])
			e.PriceLow = math.Min(e.PriceLow, run.stepMid[i])
			e.CVDHigh = math.Max(e.CVDHigh, cvd)
			e.CVDLow = math.Min(e.CVDLow, cvd)
		}
		out = append(out, windowExt{ext: e, endStep: start + window - 1})
	}
	return out
}
