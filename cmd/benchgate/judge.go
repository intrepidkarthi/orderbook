package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// The judge (docs/BENCH-GATE.md §5.2). It is built from the BASE tree, so a head
// commit cannot loosen the rules that judge it: every number below takes effect
// only once it has landed.
const (
	rounds        = 10    // each round runs A B B A or B A A B: two adjacent pairs
	pairsPerRound = 2     // so 20 pairs per benchmark
	failMedian    = 1.10  // the median head/base ratio must exceed this...
	failBound     = 1.05  // ...and so must a distribution-free lower bound on it
	alpha         = 0.025 // one-sided level of that bound
	taintLimit    = 0.10  // a probe moving more than this between a pair's two runs taints it
	// minClean is the fewest untainted pairs a verdict may rest on. It replaced a
	// cap of 2 tainted pairs: on a loaded machine taint is routine, and the cap made
	// the gate blind to everything, including a measured 7x slowdown, because 7 of
	// 20 pairs were tainted. k is computed for however many clean pairs remain, so
	// the order statistic's guarantee holds on the pairs actually used.
	minClean   = 12
	maxMissing = 2 // pairs that crashed or produced no result
)

// Invocation is one run of one benchmark binary.
type Invocation struct {
	Bench   string  `json:"bench"`
	Arm     string  `json:"arm"` // "base" or "head"
	Round   int     `json:"round"`
	Slot    int     `json:"slot"` // 0..3 within the round
	NsPerOp float64 `json:"ns_per_op"`
	Allocs  int64   `json:"allocs_per_op"`
	// Probe timings taken immediately before the run: a fixed ALU loop and a memory
	// walk. They measure the machine, not the code.
	ProbeALU float64 `json:"probe_alu_ns"`
	ProbeMem float64 `json:"probe_mem_ns"`
	Failed   bool    `json:"failed,omitempty"`
}

// Pair is two adjacent invocations of one benchmark, one per arm, in one round.
type Pair struct {
	Round   int     `json:"round"`
	Ratio   float64 `json:"ratio"` // head / base
	Tainted bool    `json:"tainted"`
	Missing bool    `json:"missing,omitempty"`
}

// Verdict is the judge's answer for one benchmark.
type Verdict struct {
	Bench       string  `json:"bench"`
	Timing      string  `json:"timing"` // fail, faster, pass, inconclusive, or not compared: <why>
	Median      float64 `json:"median,omitempty"`
	K           int     `json:"k,omitempty"`
	KthRatio    float64 `json:"kth_ratio,omitempty"`
	Pairs       []Pair  `json:"pairs,omitempty"`
	Tainted     int     `json:"tainted"`
	Missing     int     `json:"missing"`
	AllocsBase  int64   `json:"allocs_base"`
	AllocsHead  int64   `json:"allocs_head"`
	Allocations string  `json:"allocations"` // pass, fail, or not compared: <why>
}

// pairUp forms each round's two pairs from ADJACENT slots, (0,1) and (2,3). Pairing
// within a round is the whole point: drift between rounds then cancels inside every
// ratio instead of landing in it.
func pairUp(invs []Invocation) []Pair {
	byRound := map[int]map[int]Invocation{}
	for _, v := range invs {
		if byRound[v.Round] == nil {
			byRound[v.Round] = map[int]Invocation{}
		}
		byRound[v.Round][v.Slot] = v
	}
	var pairs []Pair
	for r := 1; r <= rounds; r++ {
		for _, s := range [][2]int{{0, 1}, {2, 3}} {
			a, okA := byRound[r][s[0]]
			b, okB := byRound[r][s[1]]
			if !okA || !okB || a.Failed || b.Failed || a.Arm == b.Arm || a.NsPerOp <= 0 || b.NsPerOp <= 0 {
				pairs = append(pairs, Pair{Round: r, Missing: true})
				continue
			}
			base, head := a, b
			if a.Arm == "head" {
				base, head = b, a
			}
			pairs = append(pairs, Pair{
				Round:   r,
				Ratio:   head.NsPerOp / base.NsPerOp,
				Tainted: drift(a.ProbeALU, b.ProbeALU) > taintLimit || drift(a.ProbeMem, b.ProbeMem) > taintLimit,
			})
		}
	}
	return pairs
}

func drift(x, y float64) float64 {
	if x <= 0 || y <= 0 {
		return math.Inf(1) // a missing probe cannot vouch for the pair
	}
	return math.Abs(x-y) / math.Min(x, y)
}

// orderStatK is the largest k with P(Binom(n, 1/2) <= k-1) <= alpha. The k-th
// smallest of n paired ratios is then a one-sided lower confidence bound on their
// median that assumes nothing about the noise's distribution. For n = 20 it is 6.
func orderStatK(n int) int {
	k := 0
	cdf := 0.0
	for j := 0; j <= n; j++ {
		cdf += binom(n, j) / math.Pow(2, float64(n))
		if cdf > alpha {
			break
		}
		k = j + 1
	}
	return k
}

func binom(n, k int) float64 {
	r := 1.0
	for i := 1; i <= k; i++ {
		r = r * float64(n-k+i) / float64(i)
	}
	return r
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// judgeTiming applies the precedence inconclusive > fail > faster > pass.
// Tainted pairs are left out; too few left over is inconclusive.
func judgeTiming(v *Verdict, pairs []Pair) {
	v.Pairs = pairs
	var clean []float64
	for _, p := range pairs {
		switch {
		case p.Missing:
			v.Missing++
		case p.Tainted:
			v.Tainted++
		default:
			clean = append(clean, p.Ratio)
		}
	}
	if v.Missing > maxMissing || len(clean) < minClean {
		v.Timing = "inconclusive"
		return
	}
	sort.Float64s(clean)
	v.Median = median(clean)
	v.K = orderStatK(len(clean))
	if v.K == 0 {
		v.Timing = "inconclusive"
		return
	}
	v.KthRatio = clean[v.K-1]
	upperBound := clean[len(clean)-v.K] // the k-th largest: an upper bound, for "faster"
	switch {
	case v.Median > failMedian && v.KthRatio > failBound:
		v.Timing = "fail"
	case v.Median < 1/failMedian && upperBound < 1/failBound:
		v.Timing = "faster"
	default:
		v.Timing = "pass"
	}
}

// allocSlack is how far head's median allocation count may exceed base's before it
// fails. Zero everywhere except the tape replay: its op is 50,000 commands, and its
// count is NOT exactly repeatable. Measured on identical code (a local A/A run, 20
// invocations per arm): 9,060 to 9,063 allocations per replay within ONE arm. So an
// exact rule failed identical code, and the slack is twice the measured spread. A
// real regression on that path is not close: one extra allocation per Match call is
// +30,840.
var allocSlack = map[string]int64{
	"BenchmarkTapeReplay/sink=nil":   8,
	"BenchmarkTapeReplay/sink=count": 8, // same replay, same runtime noise: 9,070 to 9,071 measured
}

// wholeLogFloor is the absolute part of a whole-log pkg/wal benchmark's slack. It was
// 16 until ten A/A runs on CI measured the spread: WriteSnapshot read 71 to 106
// allocations per op on identical code, ReadAll moved by 28, and the covered-churn
// recovery by 19. 80 is at least twice the largest of those ranges. One extra
// allocation per record adds more than a thousand on every one of these benchmarks.
const wholeLogFloor = 80

// allocSlackFor is the slack for one benchmark: its entry in allocSlack, or for a
// whole-log pkg/wal benchmark, wholeLogFloor plus 0.01% of base.
func allocSlackFor(bench string, base int64) int64 {
	if wholeLog[bench] {
		return wholeLogFloor + base/10000
	}
	return allocSlack[bench]
}

// judgeAllocs compares the median allocation count of each arm. Counts are compared
// within the job and never against a stored number, because they differ by platform.
func judgeAllocs(v *Verdict, invs []Invocation) {
	var base, head []float64
	for _, x := range invs {
		if x.Failed {
			continue
		}
		switch x.Arm {
		case "base":
			base = append(base, float64(x.Allocs))
		case "head":
			head = append(head, float64(x.Allocs))
		}
	}
	if len(base) == 0 || len(head) == 0 {
		v.AllocsBase, v.AllocsHead = -1, -1
		v.Allocations = "not compared: no result on one side"
		return
	}
	v.AllocsBase, v.AllocsHead = int64(median(base)), int64(median(head))
	if v.AllocsHead > v.AllocsBase+allocSlackFor(v.Bench, v.AllocsBase) {
		v.Allocations = "fail"
		return
	}
	v.Allocations = "pass"
}

// Result is one parsed benchmark line.
type Result struct {
	Name    string
	NsPerOp float64
	Allocs  int64
}

// parseOutput reads the output of ONE invocation of `go test -bench ^Name$`. It
// insists on exactly one result line: an unanchored pattern that matched two
// benchmarks would otherwise compare whichever printed first.
func parseOutput(out string) (Result, error) {
	var found []Result
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasPrefix(f[0], "Benchmark") {
			continue
		}
		if _, err := strconv.Atoi(f[1]); err != nil {
			continue
		}
		r := Result{Name: stripProcs(f[0]), Allocs: -1}
		for i := 2; i+1 < len(f); i += 2 {
			val, unit := f[i], f[i+1]
			switch unit {
			case "ns/op":
				x, err := strconv.ParseFloat(val, 64)
				if err != nil {
					return Result{}, fmt.Errorf("ns/op %q: %v", val, err)
				}
				r.NsPerOp = x
			case "allocs/op":
				x, err := strconv.ParseInt(val, 10, 64)
				if err != nil {
					return Result{}, fmt.Errorf("allocs/op %q: %v", val, err)
				}
				r.Allocs = x
			}
		}
		found = append(found, r)
	}
	switch {
	case len(found) == 0:
		return Result{}, fmt.Errorf("no benchmark result line")
	case len(found) > 1:
		return Result{}, fmt.Errorf("%d result lines from one invocation; the pattern must name exactly one benchmark", len(found))
	}
	if found[0].NsPerOp <= 0 || found[0].Allocs < 0 {
		return Result{}, fmt.Errorf("result line lacks ns/op or allocs/op (run with -benchmem)")
	}
	return found[0], nil
}

// stripProcs removes the -GOMAXPROCS suffix go test appends to a benchmark name.
func stripProcs(name string) string {
	if i := strings.LastIndexByte(name, '-'); i > 0 {
		if _, err := strconv.Atoi(name[i+1:]); err == nil {
			return name[:i]
		}
	}
	return name
}
