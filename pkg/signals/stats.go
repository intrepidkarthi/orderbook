package signals

import "math"

// Z95 is the standard normal quantile for a 95% two-sided interval, the usual
// argument to WilsonInterval.
const Z95 = 1.959964

// Interval is a proportion estimate with a confidence interval around it.
type Interval struct {
	Rate float64 // successes / n
	Lo   float64 // lower bound
	Hi   float64 // upper bound
	N    int     // sample size
}

// Overlaps reports whether two intervals overlap. Non-overlapping intervals are
// strong evidence of a real difference; overlapping ones are not evidence of
// sameness, merely a failure to distinguish. Use it to refuse a claim, not to
// establish one.
func (i Interval) Overlaps(o Interval) bool {
	return i.Lo <= o.Hi && o.Lo <= i.Hi
}

// WilsonInterval returns the Wilson score interval for a binomial proportion.
//
// It is used here rather than the textbook normal approximation because the
// normal interval misbehaves badly at exactly the sample sizes this research
// produces: it can extend past 0 or 1, and it collapses to zero width when the
// observed rate is 0 or 1, which would report a handful of lucky hits as
// certainty. Wilson does neither.
//
// z selects the confidence level (Z95 for 95%). A non-positive n returns a zero
// Interval; successes are clamped into [0, n].
func WilsonInterval(successes, n int, z float64) Interval {
	if n <= 0 {
		return Interval{}
	}
	if successes < 0 {
		successes = 0
	}
	if successes > n {
		successes = n
	}

	nf := float64(n)
	p := float64(successes) / nf
	z2 := z * z

	denom := 1 + z2/nf
	centre := (p + z2/(2*nf)) / denom
	margin := (z / denom) * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))

	return Interval{
		Rate: p,
		Lo:   math.Max(0, centre-margin),
		Hi:   math.Min(1, centre+margin),
		N:    n,
	}
}
