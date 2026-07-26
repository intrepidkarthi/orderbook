package study

import (
	"testing"
)

func flowCfg(seed int64) FlowConfig {
	return FlowConfig{Steps: 8000, Seed: seed}
}

func TestRunInference_Deterministic(t *testing.T) {
	a, b := RunInference(flowCfg(1)), RunInference(flowCfg(1))
	if a != b {
		t.Errorf("non-deterministic:\n a = %+v\n b = %+v", a, b)
	}
}

// The headline of §4: per-trade accuracy looks fine while the series built from
// those trades does not. Misclassifications do not cancel, so they accumulate
// into CVD instead of averaging out.
func TestRunInference_AccuracyIsHighButCVDIsNot(t *testing.T) {
	r := RunInference(flowCfg(1))

	if r.Trades == 0 {
		t.Fatal("no trades classified")
	}
	if !(r.TickAccuracy.Rate > 0.9) {
		t.Errorf("tick-rule accuracy = %.4f, expected > 0.9 per trade", r.TickAccuracy.Rate)
	}
	if !(r.TickCVDError > 0.2) {
		t.Errorf("tick CVD error = %.4f; the point of this experiment is that a "+
			"high per-trade accuracy still produces a badly wrong cumulative series",
			r.TickCVDError)
	}
}

// Lee-Ready scores perfectly here, and that is a property of the simulator
// rather than a result: every aggressive order is a market order that executes
// at the touch, so the quote rule cannot be wrong. Real venues have hidden
// liquidity, mid-point executions, and quotes that are stale relative to the
// print — none of which this book reproduces. The test pins the behaviour so the
// day the simulator gains those features, it fails and the write-up gets
// revisited.
func TestRunInference_LeeReadyIsPerfectInThisSimulator(t *testing.T) {
	r := RunInference(flowCfg(1))

	if r.LeeReadyAccuracy.Rate != 1 {
		t.Errorf("Lee-Ready accuracy = %.6f, want exactly 1 in this simulator. "+
			"If the book now produces trades away from the touch, this is no longer "+
			"an artifact and docs/research/order-flow.md must be updated.",
			r.LeeReadyAccuracy.Rate)
	}
	if r.LeeReadyCVD != r.TrueCVD {
		t.Errorf("Lee-Ready CVD %d != true CVD %d despite perfect classification",
			r.LeeReadyCVD, r.TrueCVD)
	}
}

func TestRunDivergence_Deterministic(t *testing.T) {
	a, b := RunDivergence(flowCfg(2)), RunDivergence(flowCfg(2))
	if a != b {
		t.Errorf("non-deterministic:\n a = %+v\n b = %+v", a, b)
	}
}

// A CVD divergence is a price extreme plus an extra condition, so it can never
// fire more often than the price extreme alone. If it did, the control would not
// be a strict relaxation and the comparison between them would be meaningless.
func TestRunDivergence_IsASubsetOfThePriceOnlyControl(t *testing.T) {
	for _, seed := range []int64{1, 2, 3} {
		r := RunDivergence(flowCfg(seed))
		if r.Divergence.Episodes > r.PriceOnly.Episodes {
			t.Errorf("seed %d: divergence fired %d times but the price-only control "+
				"fired %d; the control must be the weaker condition",
				seed, r.Divergence.Episodes, r.PriceOnly.Episodes)
		}
	}
}

func TestRunDivergence_ReportsBothRatesAndCounts(t *testing.T) {
	r := RunDivergence(flowCfg(1))

	if r.Divergence.Episodes == 0 {
		t.Fatal("no divergence episodes detected")
	}
	if r.Divergence.BaseN == 0 {
		t.Error("base rate has no sample; a hit-rate without one means nothing")
	}
	if r.Divergence.SignalHits > r.Divergence.SignalN {
		t.Errorf("hits %d exceed episodes %d", r.Divergence.SignalHits, r.Divergence.SignalN)
	}
	if got := r.Divergence.SignalRate.Rate - r.Divergence.BaseRate.Rate; got != r.Divergence.Edge {
		t.Errorf("Edge = %v, want signal − base = %v", r.Divergence.Edge, got)
	}
}

func TestRunAbsorption_Deterministic(t *testing.T) {
	a, b := RunAbsorption(flowCfg(3)), RunAbsorption(flowCfg(3))
	if a != b {
		t.Errorf("non-deterministic:\n a = %+v\n b = %+v", a, b)
	}
}

func TestRunAbsorption_ProducesAComparableBaseRate(t *testing.T) {
	r := RunAbsorption(flowCfg(1))

	if r.Episodes == 0 {
		t.Fatal("no absorption episodes detected")
	}
	if r.BaseN <= r.SignalN {
		t.Errorf("base sample %d must be larger than the %d signal episodes it "+
			"contains", r.BaseN, r.SignalN)
	}
}

func TestPoolSignals_SumsCountsExactly(t *testing.T) {
	a := newSignalResult(10, 20, 40, 100)
	b := newSignalResult(30, 40, 60, 100)

	got := PoolSignals([]SignalResult{a, b})

	if got.SignalHits != 40 || got.SignalN != 60 {
		t.Errorf("signal counts = %d/%d, want 40/60", got.SignalHits, got.SignalN)
	}
	if got.BaseHits != 100 || got.BaseN != 200 {
		t.Errorf("base counts = %d/%d, want 100/200", got.BaseHits, got.BaseN)
	}
	// Pooling must weight by sample size, not average the rates: (10+30)/(20+40)
	// is 0.667, whereas averaging 0.5 and 0.75 would give 0.625.
	if !approxFloat(got.SignalRate.Rate, 40.0/60.0) {
		t.Errorf("pooled rate = %v, want %v — pooling must not average rates",
			got.SignalRate.Rate, 40.0/60.0)
	}
}

func TestPoolSignals_Empty(t *testing.T) {
	if got := PoolSignals(nil); got.Episodes != 0 {
		t.Errorf("pooling nothing gave %+v, want a zero result", got)
	}
}

func TestRunSqueezeDemo_Deterministic(t *testing.T) {
	cfg := SqueezeConfig{Seed: 1}
	a, b := RunSqueezeDemo(cfg), RunSqueezeDemo(cfg)
	if a != b {
		t.Errorf("non-deterministic:\n a = %+v\n b = %+v", a, b)
	}
}

// The constructed episode is only evidence because of its control arm: the same
// selling pressure without the wall must visibly move the price. "Price held"
// on its own could just be a quiet market.
func TestRunSqueezeDemo_WallAbsorbsWhatWouldOtherwiseMovePrice(t *testing.T) {
	for _, seed := range []int64{1, 2, 3} {
		d := RunSqueezeDemo(SqueezeConfig{Seed: seed})

		if d.AbsorbedLots == 0 {
			t.Errorf("seed %d: the wall absorbed nothing", seed)
		}
		if d.MoveWithoutWall >= 0 {
			t.Errorf("seed %d: selling pressure without a wall moved price %+.2f, "+
				"expected it to fall", seed, d.MoveWithoutWall)
		}
		if !(abs(d.MoveWithWall) < abs(d.MoveWithoutWall)) {
			t.Errorf("seed %d: price moved %+.2f with the wall and %+.2f without; "+
				"absorption means the wall holds price better than its absence",
				seed, d.MoveWithWall, d.MoveWithoutWall)
		}
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func approxFloat(a, b float64) bool { return abs(a-b) < 1e-9 }
