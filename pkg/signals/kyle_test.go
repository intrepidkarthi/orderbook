package signals

import (
	"math"
	"math/rand"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func trade(side types.Side, qty int64) *types.Trade {
	return &types.Trade{Quantity: qty, TakerSide: side}
}

func TestSignedFlow_NetsAggressorVolume(t *testing.T) {
	trades := []*types.Trade{
		trade(types.SideBuy, 10),
		trade(types.SideSell, 4),
		trade(types.SideBuy, 1),
	}
	if got := SignedFlow(trades); got != 7 {
		t.Errorf("SignedFlow = %d, want 7 (10 - 4 + 1)", got)
	}
}

func TestSignedFlow_BalancedFlowIsZero(t *testing.T) {
	trades := []*types.Trade{
		trade(types.SideBuy, 5),
		trade(types.SideSell, 5),
	}
	if got := SignedFlow(trades); got != 0 {
		t.Errorf("SignedFlow = %d, want 0: equal buy and sell aggression nets out", got)
	}
}

func TestSignedFlow_EmptyAndNilSafe(t *testing.T) {
	if got := SignedFlow(nil); got != 0 {
		t.Errorf("SignedFlow(nil) = %d, want 0", got)
	}
	if got := SignedFlow([]*types.Trade{nil, trade(types.SideBuy, 3), nil}); got != 3 {
		t.Errorf("SignedFlow with nil entries = %d, want 3", got)
	}
}

// A noiseless path built as ΔP = λ·y must return exactly λ. This is the
// calibration check the study's experiment 1 rests on.
func TestEstimateLambda_RecoversKnownLambda(t *testing.T) {
	const lambda = 0.25
	flow := []float64{-40, -10, 0, 5, 30, 100}
	dPrice := make([]float64, len(flow))
	for i, y := range flow {
		dPrice[i] = lambda * y
	}

	fit := EstimateLambda(flow, dPrice)
	if !approx(fit.Lambda, lambda) {
		t.Errorf("Lambda = %v, want %v", fit.Lambda, lambda)
	}
	if !approx(fit.R2, 1) {
		t.Errorf("R2 = %v, want 1 on a noiseless path", fit.R2)
	}
	if fit.N != len(flow) {
		t.Errorf("N = %d, want %d", fit.N, len(flow))
	}
}

// With noise added, λ should stay near the truth while R² falls below 1 — the
// estimator must degrade honestly rather than reporting a perfect fit.
func TestEstimateLambda_NoiseDegradesR2NotLambda(t *testing.T) {
	const lambda = 0.5
	rng := rand.New(rand.NewSource(7))

	flow := make([]float64, 400)
	dPrice := make([]float64, len(flow))
	for i := range flow {
		flow[i] = rng.NormFloat64() * 50
		dPrice[i] = lambda*flow[i] + rng.NormFloat64()*5
	}

	fit := EstimateLambda(flow, dPrice)
	if math.Abs(fit.Lambda-lambda) > 0.05 {
		t.Errorf("Lambda = %v, want within 0.05 of %v", fit.Lambda, lambda)
	}
	if !(fit.R2 > 0.5 && fit.R2 < 1) {
		t.Errorf("R2 = %v, want strictly between 0.5 and 1 with noise present", fit.R2)
	}
}

// A book so deep that flow never moves the price has λ = 0. That is a real
// result, not a failure: infinite liquidity absorbs any order.
func TestEstimateLambda_NoImpactGivesZeroLambda(t *testing.T) {
	flow := []float64{-100, -20, 15, 60}
	dPrice := []float64{0, 0, 0, 0}

	fit := EstimateLambda(flow, dPrice)
	if fit.Lambda != 0 {
		t.Errorf("Lambda = %v, want 0 when flow never moves price", fit.Lambda)
	}
	if fit.R2 != 0 {
		t.Errorf("R2 = %v, want 0 when there is no variance to explain", fit.R2)
	}
}

func TestEstimateLambda_Degenerate(t *testing.T) {
	cases := []struct {
		name         string
		flow, dPrice []float64
	}{
		{"mismatched lengths", []float64{1, 2, 3}, []float64{1, 2}},
		{"single point", []float64{1}, []float64{1}},
		{"empty", nil, nil},
		{"zero-variance flow", []float64{4, 4, 4}, []float64{1, 2, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fit := EstimateLambda(tc.flow, tc.dPrice)
			if fit.Lambda != 0 || fit.R2 != 0 {
				t.Errorf("got %+v, want zero Lambda and R2", fit)
			}
		})
	}
}

// N must report 0 for a degenerate fit so a caller cannot mistake "no data" for
// "a real fit over N intervals".
func TestEstimateLambda_DegenerateReportsZeroN(t *testing.T) {
	if fit := EstimateLambda([]float64{1, 2, 3}, []float64{1, 2}); fit.N != 0 {
		t.Errorf("N = %d on mismatched input, want 0", fit.N)
	}
}
