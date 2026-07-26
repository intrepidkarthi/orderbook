package study

import (
	"math"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/sim"
)

func TestRunLambdaCalibration_RecoversInjectedLambda(t *testing.T) {
	const want = 0.25
	fit := RunLambdaCalibration(CalibrationConfig{Lambda: want, NoiseSD: 5, N: 2000, Seed: 1})

	if math.Abs(fit.Lambda-want) > 0.02 {
		t.Errorf("Lambda = %.4f, want within 0.02 of %.2f", fit.Lambda, want)
	}
	if !(fit.R2 > 0.8) {
		t.Errorf("R² = %.4f, want > 0.8 at this noise level", fit.R2)
	}
	if fit.N != 2000 {
		t.Errorf("N = %d, want 2000", fit.N)
	}
}

// More noise must cost R² without dragging λ off the truth — an estimator that
// quietly biased the slope under noise would invalidate every measurement below.
func TestRunLambdaCalibration_NoiseCostsR2NotLambda(t *testing.T) {
	const want = 0.25
	clean := RunLambdaCalibration(CalibrationConfig{Lambda: want, NoiseSD: 2, N: 4000, Seed: 2})
	noisy := RunLambdaCalibration(CalibrationConfig{Lambda: want, NoiseSD: 20, N: 4000, Seed: 2})

	if !(clean.R2 > noisy.R2) {
		t.Errorf("R² should fall with noise: clean %.4f, noisy %.4f", clean.R2, noisy.R2)
	}
	for _, c := range []struct {
		name string
		fit  float64
	}{{"clean", clean.Lambda}, {"noisy", noisy.Lambda}} {
		if math.Abs(c.fit-want) > 0.05 {
			t.Errorf("%s Lambda = %.4f, want within 0.05 of %.2f", c.name, c.fit, want)
		}
	}
}

func TestRunKyleLambda_Deterministic(t *testing.T) {
	a := RunKyleLambda(KyleConfig{Steps: 1500, Seed: 3})
	b := RunKyleLambda(KyleConfig{Steps: 1500, Seed: 3})
	if a != b {
		t.Errorf("non-deterministic:\n a = %+v\n b = %+v", a, b)
	}
}

// The emergent λ must be positive: net buying pressure pushes the price up. A
// negative λ would mean aggressive buys make the price fall, which would point
// at a sign error rather than a market phenomenon.
func TestRunKyleLambda_PositiveAndExplanatory(t *testing.T) {
	r := RunKyleLambda(KyleConfig{Steps: 5000, Seed: 1})

	if !(r.Fit.Lambda > 0) {
		t.Errorf("Lambda = %.5f, want positive", r.Fit.Lambda)
	}
	if !(r.Fit.R2 > 0.05) {
		t.Errorf("R² = %.4f, want > 0.05 — flow should explain some of the move", r.Fit.R2)
	}
	if r.Fit.N != 5000 {
		t.Errorf("N = %d, want one pair per sampled step (5000)", r.Fit.N)
	}
	if !(r.AvgDepth > 0) {
		t.Errorf("AvgDepth = %.1f, want positive", r.AvgDepth)
	}
	if r.Trades == 0 {
		t.Error("no trades observed; the regression would be measuring nothing")
	}
}

// Kyle's central prediction: liquidity is the inverse of price impact. Deeper
// books must absorb the same flow with less price movement.
func TestRunKyleDepth_LambdaFallsAsDepthRises(t *testing.T) {
	pts := RunKyleDepth(KyleConfig{Steps: 3000, Seed: 1}, []int{1, 2, 4, 8})
	if len(pts) != 4 {
		t.Fatalf("got %d points, want 4", len(pts))
	}

	for i := 1; i < len(pts); i++ {
		if !(pts[i].AvgDepth > pts[i-1].AvgDepth) {
			t.Errorf("depth not increasing at scale %d: %.1f then %.1f",
				pts[i].SizeScale, pts[i-1].AvgDepth, pts[i].AvgDepth)
		}
		if !(pts[i].Fit.Lambda < pts[i-1].Fit.Lambda) {
			t.Errorf("λ not falling as depth rises at scale %d: %.5f then %.5f",
				pts[i].SizeScale, pts[i-1].Fit.Lambda, pts[i].Fit.Lambda)
		}
	}

	// λ ∝ 1/depth implies λ·depth is roughly scale-invariant. This is a loose
	// band on purpose: an integer-tick book quantizes small moves, so exact
	// proportionality is not expected.
	first, last := pts[0].LambdaXDepth, pts[len(pts)-1].LambdaXDepth
	if ratio := last / first; ratio < 0.5 || ratio > 2 {
		t.Errorf("λ·depth moved by %.2fx across the sweep (%.2f → %.2f); "+
			"λ ∝ 1/depth looks broken", ratio, first, last)
	}
}

func TestRunKyleDepth_DoesNotMutateCallersAgent(t *testing.T) {
	nt := sim.DefaultNoiseTrader("noise")
	nt.MaxOffsetTicks = 40
	minBefore, maxBefore := nt.MinSize, nt.MaxSize

	RunKyleDepth(KyleConfig{Steps: 300, Seed: 1, Noise: nt}, []int{2, 4})

	if nt.MinSize != minBefore || nt.MaxSize != maxBefore {
		t.Errorf("caller's agent mutated: sizes %d..%d became %d..%d",
			minBefore, maxBefore, nt.MinSize, nt.MaxSize)
	}
}

func TestRunKyleDepth_SkipsNonPositiveScales(t *testing.T) {
	pts := RunKyleDepth(KyleConfig{Steps: 300, Seed: 1}, []int{0, -3, 1})
	if len(pts) != 1 || pts[0].SizeScale != 1 {
		t.Errorf("got %d points %+v, want only the scale-1 rung", len(pts), pts)
	}
}

func TestKyleConfig_DefaultsProduceAFit(t *testing.T) {
	r := RunKyleLambda(KyleConfig{Steps: 500})
	if r.Fit.N == 0 {
		t.Error("zero-value config (bar Steps) produced no fit")
	}
}
