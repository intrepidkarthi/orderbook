package signals

// LinReg fits y = slope·x + intercept by ordinary least squares and returns the
// slope, intercept, and R² (coefficient of determination, the fraction of the
// variance in y explained by x). It returns zeros when there are fewer than two
// points, the lengths differ, or x has no variance.
//
// R² here is the square of the Pearson correlation — the quantity the OFI
// literature reports. Note it measures variance explained, not directional
// hit-rate, and says nothing on its own about whether the relationship is
// contemporaneous or predictive.
func LinReg(x, y []float64) (slope, intercept, r2 float64) {
	n := len(x)
	if n < 2 || n != len(y) {
		return 0, 0, 0
	}

	var sx, sy float64
	for i := range n {
		sx += x[i]
		sy += y[i]
	}
	mx := sx / float64(n)
	my := sy / float64(n)

	var sxx, sxy, syy float64
	for i := range n {
		dx := x[i] - mx
		dy := y[i] - my
		sxx += dx * dx
		sxy += dx * dy
		syy += dy * dy
	}

	if sxx == 0 {
		return 0, my, 0 // x has no variance: nothing to regress on
	}
	slope = sxy / sxx
	intercept = my - slope*mx
	if syy == 0 {
		return slope, intercept, 0 // y is constant
	}
	r2 = (sxy * sxy) / (sxx * syy)
	return slope, intercept, r2
}
