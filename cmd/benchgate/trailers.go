package main

import (
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Accepting a slowdown on purpose (docs/BENCH-GATE.md §5.3). The only way to accept
// one is a commit trailer in the compared range: a PR label or a workflow input is
// never an acceptance, because neither is recorded next to the change it excuses.
const (
	trailerAccept       = "Bench-Accept"
	trailerAcceptAllocs = "Bench-Accept-Allocs"
	trailerGateChange   = "Bench-Gate-Change"

	// maxAcceptRatio is the most any trailer may accept. Above it the slowdown is a
	// change to the document, not to a commit message.
	maxAcceptRatio = 1.5
	// acceptSlack is how much looser than the measured ratio a stated bound may be.
	// A bound of 1.5 on a measured 1.12 would pre-accept the next 30% for free.
	acceptSlack = 1.10
)

var (
	acceptRe       = regexp.MustCompile(`^(\S+) ratio<=([0-9]+(?:\.[0-9]+)?) -- (\S.*)$`)
	acceptAllocsRe = regexp.MustCompile(`^(\S+) \+([0-9]+)/op -- (\S.*)$`)
)

// Acceptance is one parsed trailer.
type Acceptance struct {
	Commit string  `json:"commit"`
	Key    string  `json:"key"`
	Glob   string  `json:"glob,omitempty"`
	Ratio  float64 `json:"ratio,omitempty"`
	Allocs int64   `json:"allocs,omitempty"`
	Reason string  `json:"reason"`
	Used   bool    `json:"used"`
}

// parseTrailer reads one trailer line, "Key: value". Only the three keys are this
// gate's business; any other trailer is not.
func parseTrailer(commit, line string) (*Acceptance, error) {
	key, val, ok := strings.Cut(line, ":")
	if !ok {
		return nil, nil
	}
	key, val = strings.TrimSpace(key), strings.TrimSpace(val)
	switch key {
	case trailerAccept:
		m := acceptRe.FindStringSubmatch(val)
		if m == nil {
			return nil, fmt.Errorf("%s: malformed %s %q (want <name|glob> ratio<=<x> -- <reason>)", short(commit), key, val)
		}
		x, _ := strconv.ParseFloat(m[2], 64)
		if x <= 1 || x > maxAcceptRatio {
			return nil, fmt.Errorf("%s: %s ratio<=%v is outside (1, %.1f]; above that, change docs/BENCH-GATE.md", short(commit), key, x, maxAcceptRatio)
		}
		return &Acceptance{Commit: commit, Key: key, Glob: m[1], Ratio: x, Reason: m[3]}, nil
	case trailerAcceptAllocs:
		m := acceptAllocsRe.FindStringSubmatch(val)
		if m == nil {
			return nil, fmt.Errorf("%s: malformed %s %q (want <name|glob> +<k>/op -- <reason>)", short(commit), key, val)
		}
		k, _ := strconv.ParseInt(m[2], 10, 64)
		if k == 0 {
			return nil, fmt.Errorf("%s: %s +0/op accepts nothing", short(commit), key)
		}
		return &Acceptance{Commit: commit, Key: key, Glob: m[1], Allocs: k, Reason: m[3]}, nil
	case trailerGateChange:
		if val == "" {
			return nil, fmt.Errorf("%s: %s needs a reason", short(commit), key)
		}
		return &Acceptance{Commit: commit, Key: key, Reason: val}, nil
	}
	return nil, nil
}

// matches reports whether a trailer's glob names a benchmark. A bare name matches
// with or without its Benchmark prefix.
func (a *Acceptance) matches(bench string) bool {
	for _, name := range []string{bench, strings.TrimPrefix(bench, "Benchmark")} {
		if ok, _ := path.Match(a.Glob, name); ok {
			return true
		}
	}
	return false
}

// applyAcceptances turns accepted failures into "accepted" verdicts and reports
// which acceptances could not be honoured. A trailer may only cover what was
// measured: a timing bound no tighter than the measured ratio fails, and so does one
// looser than the measured ratio x acceptSlack; an allocation count must equal the
// measured increase exactly.
func applyAcceptances(v *Verdict, accs []*Acceptance) []string {
	var problems []string
	if v.Timing == "fail" {
		for _, a := range accs {
			if a.Key != trailerAccept || !a.matches(v.Bench) {
				continue
			}
			switch {
			case v.Median > a.Ratio:
				problems = append(problems, fmt.Sprintf("%s: measured %.3f exceeds the accepted ratio<=%v (%s)", v.Bench, v.Median, a.Ratio, short(a.Commit)))
			case a.Ratio > v.Median*acceptSlack:
				problems = append(problems, fmt.Sprintf("%s: ratio<=%v is looser than the measured %.3f x %.2f (%s); state what was measured", v.Bench, a.Ratio, v.Median, acceptSlack, short(a.Commit)))
			default:
				a.Used = true
				v.Timing = "accepted: " + a.Reason
			}
			break
		}
	}
	if v.Allocations == "fail" {
		delta := v.AllocsHead - v.AllocsBase
		for _, a := range accs {
			if a.Key != trailerAcceptAllocs || !a.matches(v.Bench) {
				continue
			}
			if a.Allocs != delta {
				problems = append(problems, fmt.Sprintf("%s: accepted +%d/op, measured +%d/op (%s)", v.Bench, a.Allocs, delta, short(a.Commit)))
			} else {
				a.Used = true
				v.Allocations = "accepted: " + a.Reason
			}
			break
		}
	}
	return problems
}

// enforcedTiming names the benchmarks whose TIMING may fail a build, chosen under
// the rule in docs/BENCH-GATE.md §18.2, fixed before its data: over 60 A/A runs the
// largest k-th ratio stayed at or under 1.0167, and a planted ~1.20x slowdown failed
// at least 19 of 20 runs. OrderBook_CancelReplace cleared the A/A criteria and failed
// power (10 of 19 at a plant measured 0.95-1.28x), so its timing stays report-only.
// Allocations are enforced for every gated benchmark regardless.
var enforcedTiming = map[string]bool{
	"BenchmarkEngine_CancelReplaceInto": true,
	"BenchmarkEngine_MatchInto":         true,
	"BenchmarkTapeReplay/sink=nil":      true,
	"BenchmarkOrderBook_LevelChurn":     true,
}

// detectionFloor is what the power studies measured (docs/BENCH-GATE.md §15.2 and
// §19), printed in every summary so a pass is read as what it is.
const detectionFloor = "planted slowdowns failed at least 19 of 20 power runs at medians of 1.196-1.306x (Engine_CancelReplaceInto), " +
	"1.236-1.447x (Engine_MatchInto), 1.166-1.281x (TapeReplay/sink=nil) and 1.259-1.545x (OrderBook_LevelChurn); " +
	"smaller slowdowns, and OrderBook_CancelReplace, are not guarded by timing"

// settle decides whether a verdict fails the run, and labels a timing failure that
// calibration has not cleared to enforce.
func settle(v *Verdict, secondInconclusive bool) bool {
	failed := false
	switch {
	case v.Timing == "fail" && enforcedTiming[v.Bench]:
		failed = true
	case v.Timing == "fail":
		v.Timing = "fail (report only: not calibrated to enforce)"
	case v.Timing == "inconclusive" && secondInconclusive && enforcedTiming[v.Bench]:
		// The retry already ran. Twice unable to judge an enforced benchmark is a
		// failure, or a noisy runner would be a way to pass.
		v.Timing = "inconclusive twice"
		failed = true
	case v.Timing == "missing":
		failed = true
	}
	if v.Allocations == "fail" {
		failed = true
	}
	return failed
}

// readTrailers parses the §5.3 trailers of every commit in base..HEAD, and requires
// Bench-Gate-Change on a range that changes the gate itself.
func readTrailers(headDir, base string) ([]*Acceptance, []string) {
	if base == "" || base == "unknown" {
		return nil, nil
	}
	out, err := exec.Command("git", "-C", headDir, "log", "--format=%H%x1f%(trailers:unfold,only)%x1e", base+"..HEAD").Output()
	if err != nil {
		return nil, []string{fmt.Sprintf("could not read commit trailers in %s..HEAD: %v", short(base), err)}
	}
	var accs []*Acceptance
	var problems []string
	gateChange := false
	for _, rec := range strings.Split(string(out), "\x1e") {
		sha, body, ok := strings.Cut(strings.TrimSpace(rec), "\x1f")
		if !ok {
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			a, err := parseTrailer(sha, line)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			if a != nil {
				accs = append(accs, a)
				if a.Key == trailerGateChange {
					gateChange = true
				}
			}
		}
	}
	changed, _ := exec.Command("git", "-C", headDir, "diff", "--name-only", base, "HEAD", "--",
		"cmd/benchgate", ".github/workflows/bench-gate.yml").Output()
	if len(strings.TrimSpace(string(changed))) > 0 && !gateChange {
		problems = append(problems, fmt.Sprintf("this range changes the gate (%s) and no commit in it carries %s:",
			strings.Join(strings.Fields(string(changed)), ", "), trailerGateChange))
	}
	return accs, problems
}

// trailerCount90d counts the acceptances history carries over the last 90 days. If
// they appear on most changes to the matcher, the threshold is wrong, and this is
// the number that says so.
func trailerCount90d(headDir string) int {
	out, err := exec.Command("git", "-C", headDir, "log", "--since=90.days",
		"--format=%(trailers:key="+trailerAccept+",key="+trailerAcceptAllocs+",valueonly,unfold)").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}
