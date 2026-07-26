package signals

import (
	"math"
	"testing"
)

func TestWilsonInterval_BracketsTheRate(t *testing.T) {
	in := WilsonInterval(55, 100, Z95)

	if !approx(in.Rate, 0.55) {
		t.Errorf("Rate = %v, want 0.55", in.Rate)
	}
	if !(in.Lo < in.Rate && in.Rate < in.Hi) {
		t.Errorf("interval [%v, %v] does not bracket rate %v", in.Lo, in.Hi, in.Rate)
	}
	if in.N != 100 {
		t.Errorf("N = %d, want 100", in.N)
	}
	// Against the published Wilson value for 55/100 at 95%.
	if math.Abs(in.Lo-0.4517) > 0.001 || math.Abs(in.Hi-0.6438) > 0.001 {
		t.Errorf("interval [%.4f, %.4f], want approximately [0.4517, 0.6438]", in.Lo, in.Hi)
	}
}

// The reason for using Wilson rather than the normal approximation: at the
// extremes the normal interval has zero width and can leave [0,1] entirely.
// Wilson must stay inside the unit interval and must not claim certainty.
func TestWilsonInterval_ExtremesStayBoundedAndUncertain(t *testing.T) {
	all := WilsonInterval(10, 10, Z95)
	if all.Hi > 1 {
		t.Errorf("Hi = %v, must not exceed 1", all.Hi)
	}
	if all.Lo >= 1 {
		t.Errorf("Lo = %v: 10/10 is not certainty, the interval must stay open", all.Lo)
	}

	none := WilsonInterval(0, 10, Z95)
	if none.Lo < 0 {
		t.Errorf("Lo = %v, must not go below 0", none.Lo)
	}
	if none.Hi <= 0 {
		t.Errorf("Hi = %v: 0/10 is not impossibility, the interval must stay open", none.Hi)
	}
}

func TestWilsonInterval_NarrowsWithSampleSize(t *testing.T) {
	small := WilsonInterval(55, 100, Z95)
	large := WilsonInterval(5500, 10000, Z95)

	if !((large.Hi - large.Lo) < (small.Hi - small.Lo)) {
		t.Errorf("width did not shrink with n: %v at n=100, %v at n=10000",
			small.Hi-small.Lo, large.Hi-large.Lo)
	}
}

func TestWilsonInterval_Degenerate(t *testing.T) {
	if got := WilsonInterval(5, 0, Z95); got != (Interval{}) {
		t.Errorf("n=0 gave %+v, want zero Interval", got)
	}
	if got := WilsonInterval(-5, 10, Z95); got.Rate != 0 {
		t.Errorf("negative successes gave rate %v, want clamped to 0", got.Rate)
	}
	if got := WilsonInterval(50, 10, Z95); got.Rate != 1 {
		t.Errorf("successes above n gave rate %v, want clamped to 1", got.Rate)
	}
}

func TestInterval_Overlaps(t *testing.T) {
	a := WilsonInterval(55, 100, Z95)   // roughly [0.45, 0.64]
	b := WilsonInterval(54, 100, Z95)   // roughly [0.44, 0.63]
	c := WilsonInterval(950, 1000, Z95) // roughly [0.93, 0.96]

	if !a.Overlaps(b) {
		t.Error("55/100 and 54/100 must overlap; they are indistinguishable")
	}
	if a.Overlaps(c) {
		t.Error("55/100 and 950/1000 must not overlap")
	}
	if !a.Overlaps(a) {
		t.Error("an interval must overlap itself")
	}
}
