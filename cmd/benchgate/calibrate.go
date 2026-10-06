package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Calibration is what §14.2 decides from, per timing-gated benchmark.
type Calibration struct {
	Bench        string    `json:"bench"`
	Runs         int       `json:"runs"`
	Failed       int       `json:"failed"`
	Inconclusive int       `json:"inconclusive"`
	AllocFailed  int       `json:"alloc_failed"`
	Medians      []float64 `json:"medians"`
	P99Dev       float64   `json:"p99_dev"` // nearest-rank p99 of |median - 1|
	Enforce      bool      `json:"enforce"`
	Missed       string    `json:"missed,omitempty"` // the first criterion it missed
}

// calibrationRule is docs/BENCH-GATE.md §14.2, fixed before the data existed.
const (
	maxInconclusiveRuns = 6
	devMultiplier       = 3.0
)

// calibrate reads every bench-result.json under dir and applies §14.2.
func calibrate(reports []*Report) []Calibration {
	byBench := map[string]*Calibration{}
	for _, name := range timingGated {
		byBench[name] = &Calibration{Bench: name}
	}
	for _, r := range reports {
		for _, v := range r.Verdicts {
			c, ok := byBench[v.Bench]
			if !ok {
				continue
			}
			c.Runs++
			switch v.Timing {
			case "fail":
				c.Failed++
			case "inconclusive":
				c.Inconclusive++
			}
			if v.Allocations == "fail" {
				c.AllocFailed++
			}
			if v.Median > 0 {
				c.Medians = append(c.Medians, v.Median)
			}
		}
	}
	out := make([]Calibration, 0, len(timingGated))
	for _, name := range timingGated {
		c := byBench[name]
		devs := make([]float64, 0, len(c.Medians))
		for _, m := range c.Medians {
			devs = append(devs, math.Abs(m-1))
		}
		c.P99Dev = nearestRank(devs, 0.99)
		switch {
		case c.Runs == 0:
			c.Missed = "no runs"
		case c.Failed > 0:
			c.Missed = fmt.Sprintf("failed in %d of %d A/A runs", c.Failed, c.Runs)
		case c.Inconclusive > maxInconclusiveRuns:
			c.Missed = fmt.Sprintf("inconclusive in %d runs (limit %d)", c.Inconclusive, maxInconclusiveRuns)
		case failMedian < 1+devMultiplier*c.P99Dev:
			c.Missed = fmt.Sprintf("p99 abs(median-1) %.4f: 1 + 3x that is %.3f, above the %.2f threshold", c.P99Dev, 1+devMultiplier*c.P99Dev, failMedian)
		default:
			c.Enforce = true
		}
		out = append(out, *c)
	}
	return out
}

// nearestRank is the nearest-rank percentile: the ceil(p*n)-th smallest value.
func nearestRank(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	k := int(math.Ceil(p * float64(len(s))))
	if k < 1 {
		k = 1
	}
	return s[k-1]
}

func loadReports(dir string) ([]*Report, error) {
	var out []*Report
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) != "bench-result.json" {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r Report
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		out = append(out, &r)
		return nil
	})
	return out, err
}

func runCalibrate(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: benchgate calibrate <dir of bench-result.json files>")
		return 2
	}
	reports, err := loadReports(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Calibration over %d A/A runs (docs/BENCH-GATE.md §14.2)\n\n", len(reports))
	b.WriteString("| benchmark | runs | failed | inconclusive | alloc failed | median range | p99 abs(median-1) | decision |\n|---|---:|---:|---:|---:|---|---:|---|\n")
	for _, c := range calibrate(reports) {
		lo, hi := "", ""
		if len(c.Medians) > 0 {
			s := append([]float64(nil), c.Medians...)
			sort.Float64s(s)
			lo, hi = fmt.Sprintf("%.3f", s[0]), fmt.Sprintf("%.3f", s[len(s)-1])
		}
		decision := "enforce"
		if !c.Enforce {
			decision = "report only: " + c.Missed
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %s – %s | %.4f | %s |\n", c.Bench, c.Runs, c.Failed, c.Inconclusive, c.AllocFailed, lo, hi, c.P99Dev, decision)
	}
	fmt.Print(b.String())
	return 0
}
