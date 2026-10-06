package main

import (
	"strings"
	"testing"
)

func reportWith(verdicts ...Verdict) *Report { return &Report{Verdicts: verdicts} }

// runsOf builds n A/A reports in which bench has the given medians (cycled) and
// verdict, and every other timing-gated benchmark passes at 1.000.
func runsOf(n int, bench, timing string, medians ...float64) []*Report {
	var out []*Report
	for i := 0; i < n; i++ {
		var vs []Verdict
		for _, name := range timingGated {
			v := Verdict{Bench: name, Timing: "pass", Median: 1.0, Allocations: "pass"}
			if name == bench {
				v.Timing, v.Median = timing, medians[i%len(medians)]
			}
			vs = append(vs, v)
		}
		out = append(out, reportWith(vs...))
	}
	return out
}

func decision(cs []Calibration, bench string) Calibration {
	for _, c := range cs {
		if c.Bench == bench {
			return c
		}
	}
	return Calibration{}
}

func TestCalibrateEnforcesAQuietBenchmark(t *testing.T) {
	c := decision(calibrate(runsOf(60, timingGated[0], "pass", 0.99, 1.01, 1.02)), timingGated[0])
	if !c.Enforce || c.Runs != 60 {
		t.Fatalf("a quiet benchmark: %+v", c)
	}
}

// TestCalibrateRule is §14.2's three criteria, each broken alone.
func TestCalibrateRule(t *testing.T) {
	b := timingGated[1]
	oneFail := runsOf(60, b, "pass", 1.0)
	oneFail[7].Verdicts[1].Timing = "fail"
	if c := decision(calibrate(oneFail), b); c.Enforce || !strings.Contains(c.Missed, "failed in 1") {
		t.Errorf("one failed A/A run: %+v", c)
	}
	inc := runsOf(60, b, "pass", 1.0)
	for i := 0; i < 7; i++ {
		inc[i].Verdicts[1].Timing = "inconclusive"
	}
	if c := decision(calibrate(inc), b); c.Enforce || !strings.Contains(c.Missed, "inconclusive in 7") {
		t.Errorf("7 inconclusive runs: %+v", c)
	}
	six := runsOf(60, b, "pass", 1.0)
	for i := 0; i < 6; i++ {
		six[i].Verdicts[1].Timing = "inconclusive"
	}
	if c := decision(calibrate(six), b); !c.Enforce {
		t.Errorf("6 inconclusive runs is within the limit: %+v", c)
	}
	// The maximum |median-1| of 60 is the nearest-rank p99: 0.087 gives 1.261 > 1.10.
	if c := decision(calibrate(runsOf(60, b, "pass", 1.0, 1.0, 1.087)), b); c.Enforce || c.P99Dev < 0.0869 {
		t.Errorf("a median of 1.087 among 60: %+v", c)
	}
	// 0.033 gives 1.099: inside.
	if c := decision(calibrate(runsOf(60, b, "pass", 0.967, 1.033)), b); !c.Enforce {
		t.Errorf("a spread of 0.033: %+v", c)
	}
}

func TestNearestRank(t *testing.T) {
	xs := make([]float64, 60)
	for i := range xs {
		xs[i] = float64(i + 1)
	}
	if got := nearestRank(xs, 0.99); got != 60 {
		t.Fatalf("p99 of 1..60 by nearest rank = %v, want 60 (the maximum)", got)
	}
	if got := nearestRank(xs, 0.5); got != 30 {
		t.Fatalf("p50 of 1..60 = %v, want 30", got)
	}
}
