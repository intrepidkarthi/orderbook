package signals

import "testing"

func TestLinReg_PerfectLine(t *testing.T) {
	x := []float64{0, 1, 2, 3, 4}
	y := []float64{1, 3, 5, 7, 9} // y = 2x + 1
	slope, intercept, r2 := LinReg(x, y)
	if !approx(slope, 2) || !approx(intercept, 1) || !approx(r2, 1) {
		t.Errorf("got slope=%v intercept=%v r2=%v; want 2,1,1", slope, intercept, r2)
	}
}

func TestLinReg_NoisyHasPartialR2(t *testing.T) {
	x := []float64{0, 1, 2, 3, 4, 5}
	y := []float64{1, 2, 1.5, 3, 2.5, 4} // upward but noisy
	_, _, r2 := LinReg(x, y)
	if !(r2 > 0 && r2 < 1) {
		t.Errorf("noisy fit r2 = %v, want strictly between 0 and 1", r2)
	}
}

func TestLinReg_Degenerate(t *testing.T) {
	if _, _, r2 := LinReg([]float64{1}, []float64{1}); r2 != 0 {
		t.Error("n<2 should give r2=0")
	}
	if _, _, r2 := LinReg([]float64{2, 2, 2}, []float64{1, 2, 3}); r2 != 0 {
		t.Error("zero-variance x should give r2=0")
	}
	if _, _, r2 := LinReg([]float64{1, 2, 3}, []float64{5, 5, 5}); r2 != 0 {
		t.Error("constant y should give r2=0")
	}
	if _, _, r2 := LinReg([]float64{1, 2}, []float64{1}); r2 != 0 {
		t.Error("mismatched lengths should give r2=0")
	}
}
