package study

import (
	"testing"
)

func TestRunOFI_ContemporaneousBeatsPredictive(t *testing.T) {
	r := RunOFI(OFIConfig{Steps: 5000, Seed: 1, InitialPrice: 100})

	if r.N != 4999 {
		t.Errorf("N = %d, want 4999", r.N)
	}
	// The whole point: OFI explains a meaningful share of the *same-interval*
	// move ...
	if !(r.ContemporaneousR2 > 0.15) {
		t.Errorf("contemporaneous R² = %.4f, expected clearly > 0.15", r.ContemporaneousR2)
	}
	// ... but essentially nothing about the *next* interval.
	if !(r.PredictiveR2 < 0.05) {
		t.Errorf("predictive R² = %.4f, expected < 0.05", r.PredictiveR2)
	}
	// And the gap is large, not marginal.
	if !(r.ContemporaneousR2 > 5*r.PredictiveR2) {
		t.Errorf("expected contemporaneous R² (%.4f) to dwarf predictive R² (%.4f)",
			r.ContemporaneousR2, r.PredictiveR2)
	}
	// Contemporaneously, more buy pressure ⇒ higher price (positive slope).
	if !(r.ContemporaneousSlope > 0) {
		t.Errorf("contemporaneous slope = %.5f, want positive", r.ContemporaneousSlope)
	}
}

func TestRunOFI_Deterministic(t *testing.T) {
	a := RunOFI(OFIConfig{Steps: 2000, Seed: 3, InitialPrice: 100})
	b := RunOFI(OFIConfig{Steps: 2000, Seed: 3, InitialPrice: 100})
	if a.ContemporaneousR2 != b.ContemporaneousR2 || a.PredictiveR2 != b.PredictiveR2 {
		t.Errorf("non-deterministic: contemp %.6f/%.6f pred %.6f/%.6f",
			a.ContemporaneousR2, b.ContemporaneousR2, a.PredictiveR2, b.PredictiveR2)
	}
}
