package main

import (
	"strings"
	"testing"
)

// fixture builds 10 rounds of A B B A / B A A B invocations for one benchmark.
// base(r) and head(r) give each round's timings; taint names rounds whose second
// pair's probes moved; missing names rounds whose first pair crashed.
type fixture struct {
	base, head func(r int) float64
	taint      map[int]bool
	missing    map[int]bool
	allocsBase int64
	allocsHead int64
}

func (f fixture) invocations() []Invocation {
	var out []Invocation
	for r := 1; r <= rounds; r++ {
		order := []string{"base", "head", "head", "base"}
		if r%2 == 0 {
			order = []string{"head", "base", "base", "head"}
		}
		for slot, arm := range order {
			v := Invocation{Bench: "BenchmarkX", Arm: arm, Round: r, Slot: slot, ProbeALU: 100, ProbeMem: 100}
			if arm == "base" {
				v.NsPerOp, v.Allocs = f.base(r), f.allocsBase
			} else {
				v.NsPerOp, v.Allocs = f.head(r), f.allocsHead
			}
			if f.taint[r] && slot == 3 {
				v.ProbeALU = 150
			}
			if f.missing[r] && slot == 0 {
				v.Failed = true
			}
			out = append(out, v)
		}
	}
	return out
}

func judge(f fixture) Verdict {
	invs := f.invocations()
	v := Verdict{Bench: "BenchmarkX"}
	judgeTiming(&v, pairUp(invs))
	judgeAllocs(&v, invs)
	return v
}

// noise is a fixed, repeatable +-3% wobble per round.
func noise(r int) float64 { return 1 + 0.03*float64((r*7)%5-2)/2 }

func flat(ns float64) func(int) float64 { return func(r int) float64 { return ns * noise(r) } }

func TestOrderStatK(t *testing.T) {
	for n, want := range map[int]int{20: 6, 18: 5, 10: 2, 5: 0} {
		if got := orderStatK(n); got != want {
			t.Errorf("orderStatK(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestJudgeFailsARealRegression(t *testing.T) {
	v := judge(fixture{base: flat(100), head: func(r int) float64 { return 115 * noise(r+1) }})
	if v.Timing != "fail" {
		t.Fatalf("a 15%% slowdown judged %q (median %.3f, k-th %.3f)", v.Timing, v.Median, v.KthRatio)
	}
}

func TestJudgePassesIdenticalCode(t *testing.T) {
	v := judge(fixture{base: flat(100), head: func(r int) float64 { return 100 * noise(r+2) }})
	if v.Timing != "pass" {
		t.Fatalf("identical code judged %q (median %.3f)", v.Timing, v.Median)
	}
}

// TestJudgeKnowsWhichArmIsWhich: a 2x speedup is "faster", never a failure, and the
// same numbers with the arms swapped are a failure. A judge with the ratio upside
// down reports the regression as faster.
func TestJudgeKnowsWhichArmIsWhich(t *testing.T) {
	quick := fixture{base: flat(200), head: flat(100)}
	if v := judge(quick); v.Timing != "faster" {
		t.Fatalf("a 2x speedup judged %q", v.Timing)
	}
	slow := fixture{base: flat(100), head: flat(200)}
	if v := judge(slow); v.Timing != "fail" {
		t.Fatalf("a 2x slowdown judged %q", v.Timing)
	}
}

// TestJudgeNeedsBothConditions: a high median with a spread too wide for the lower
// bound to clear 1.05 is not a failure.
func TestJudgeNeedsBothConditions(t *testing.T) {
	v := judge(fixture{base: flat(100), head: func(r int) float64 {
		if r <= 4 {
			return 100 // 8 pairs at 1.0: the 6th smallest ratio cannot clear 1.05
		}
		return 125
	}})
	if v.Timing != "pass" {
		t.Fatalf("median %.3f with k-th %.3f judged %q, want pass", v.Median, v.KthRatio, v.Timing)
	}
}

// TestJudgeTaintIsInconclusiveNotFail: when the machine moved under the run, a
// regression-looking result is inconclusive, and inconclusive outranks fail.
func TestJudgeTaintIsInconclusiveNotFail(t *testing.T) {
	v := judge(fixture{base: flat(100), head: flat(150), taint: map[int]bool{1: true, 2: true, 3: true}})
	if v.Timing != "inconclusive" || v.Tainted != 3 {
		t.Fatalf("3 tainted pairs judged %q (tainted %d)", v.Timing, v.Tainted)
	}
}

func TestJudgeMissingIsInconclusive(t *testing.T) {
	v := judge(fixture{base: flat(100), head: flat(100), missing: map[int]bool{1: true, 2: true, 3: true}})
	if v.Timing != "inconclusive" || v.Missing != 3 {
		t.Fatalf("3 missing pairs judged %q (missing %d)", v.Timing, v.Missing)
	}
}

// TestJudgePairsWithinARound: the machine slows by up to 4x between rounds while
// head is a steady 15% slower than base inside every round. Pairing within a round
// cancels the drift and finds the regression; pairing across rounds would bury it.
func TestJudgePairsWithinARound(t *testing.T) {
	speed := func(r int) float64 { return []float64{1, 4, 1.5, 3, 1, 2.5, 4, 1, 3.5, 2}[r-1] }
	v := judge(fixture{
		base: func(r int) float64 { return 100 * speed(r) },
		head: func(r int) float64 { return 115 * speed(r) },
	})
	if v.Timing != "fail" {
		t.Fatalf("a steady 15%% slowdown under round-to-round drift judged %q (median %.3f, k-th %.3f)", v.Timing, v.Median, v.KthRatio)
	}
}

func TestJudgeAllocsAreExactByDefault(t *testing.T) {
	if v := judge(fixture{base: flat(100), head: flat(100), allocsBase: 3, allocsHead: 4}); v.Allocations != "fail" {
		t.Fatalf("one extra allocation per op judged %q", v.Allocations)
	}
	if v := judge(fixture{base: flat(100), head: flat(100), allocsBase: 3, allocsHead: 3}); v.Allocations != "pass" {
		t.Fatalf("equal allocations judged %q", v.Allocations)
	}
	if v := judge(fixture{base: flat(100), head: flat(100), allocsBase: 3, allocsHead: 2}); v.Allocations != "pass" {
		t.Fatalf("fewer allocations judged %q", v.Allocations)
	}
}

// TestJudgeTapeAllocsTolerateMeasuredNoise replays the A/A distribution actually
// measured on identical code, and the one-allocation-per-Match regression actually
// planted: the first must pass and the second must fail.
func TestJudgeTapeAllocsTolerateMeasuredNoise(t *testing.T) {
	measured := map[string][]int64{
		"base": {9060, 9061, 9061, 9061, 9061, 9061, 9061, 9062, 9062, 9062, 9062, 9062, 9062, 9062, 9062, 9062, 9062, 9062, 9062, 9062},
		"head": {9061, 9061, 9061, 9061, 9061, 9061, 9061, 9061, 9061, 9061, 9061, 9061, 9062, 9062, 9062, 9062, 9062, 9062, 9063, 9063},
	}
	build := func(headShift int64) []Invocation {
		var out []Invocation
		for arm, xs := range measured {
			for _, x := range xs {
				if arm == "head" {
					x += headShift
				}
				out = append(out, Invocation{Arm: arm, Allocs: x})
			}
		}
		return out
	}
	v := Verdict{Bench: "BenchmarkTapeReplay/sink=nil"}
	judgeAllocs(&v, build(0))
	if v.Allocations != "pass" {
		t.Fatalf("the measured A/A distribution judged %q (%d -> %d)", v.Allocations, v.AllocsBase, v.AllocsHead)
	}
	v = Verdict{Bench: "BenchmarkTapeReplay/sink=nil"}
	judgeAllocs(&v, build(30840))
	if v.Allocations != "fail" {
		t.Fatalf("one extra allocation per Match judged %q", v.Allocations)
	}
}

func TestParseOutput(t *testing.T) {
	one := "goos: linux\nBenchmarkTapeReplay/sink=nil-4   5   18079450 ns/op   361.6 ns/cmd   483932 B/op   9061 allocs/op\nPASS\n"
	r, err := parseOutput(one)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "BenchmarkTapeReplay/sink=nil" || r.NsPerOp != 18079450 || r.Allocs != 9061 {
		t.Fatalf("parsed %+v", r)
	}
	two := one + "BenchmarkTapeReplayX-4   5   1 ns/op   1 B/op   1 allocs/op\n"
	if _, err := parseOutput(two); err == nil || !strings.Contains(err.Error(), "2 result lines") {
		t.Fatalf("two result lines from one invocation: err %v", err)
	}
	if _, err := parseOutput("BenchmarkX-4  5  10 ns/op\n"); err == nil {
		t.Fatal("a line without allocs/op was accepted; the gate needs -benchmem")
	}
	if _, err := parseOutput("PASS\n"); err == nil {
		t.Fatal("output with no result line was accepted")
	}
}
