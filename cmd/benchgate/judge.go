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
	maxTainted    = 2     // more tainted pairs than this and the verdict is inconclusive
	maxMissing    = 2     // likewise for pairs that crashed or produced no result
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
	if v.Tainted > maxTainted || v.Missing > maxMissing || len(clean) == 0 {
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

// judgeAllocs requires head to allocate no more than base, exactly. Counts are
// compared within the job and never against a stored number, because they differ
// by platform.
func judgeAllocs(v *Verdict, invs []Invocation) {
	base, head := int64(-1), int64(-1)
	for _, x := range invs {
		if x.Failed {
			continue
		}
		switch x.Arm {
		case "base":
			if base < 0 || x.Allocs < base {
				base = x.Allocs
			}
		case "head":
			if x.Allocs > head {
				head = x.Allocs
			}
		}
	}
	v.AllocsBase, v.AllocsHead = base, head
	switch {
	case base < 0 || head < 0:
		v.Allocations = "not compared: no result on one side"
	case head > base:
		v.Allocations = "fail"
	default:
		v.Allocations = "pass"
	}
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
