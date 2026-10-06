package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTrailer(t *testing.T) {
	good := map[string]Acceptance{
		"Bench-Accept: Engine_MatchInto ratio<=1.15 -- pooled nodes traded for a smaller book": {Key: trailerAccept, Glob: "Engine_MatchInto", Ratio: 1.15},
		"Bench-Accept-Allocs: Engine_* +1/op -- the result struct escapes now":                 {Key: trailerAcceptAllocs, Glob: "Engine_*", Allocs: 1},
		"Bench-Gate-Change: lower the taint limit after calibration":                           {Key: trailerGateChange},
	}
	for line, want := range good {
		a, err := parseTrailer("c0ffee", line)
		if err != nil || a == nil || a.Key != want.Key || a.Glob != want.Glob || a.Ratio != want.Ratio || a.Allocs != want.Allocs {
			t.Errorf("%q: got %+v, %v", line, a, err)
		}
	}
	bad := []string{
		"Bench-Accept: Engine_MatchInto ratio<=1.15",          // no reason
		"Bench-Accept: Engine_MatchInto ratio<=1.6 -- big",    // above the 1.5 cap
		"Bench-Accept: Engine_MatchInto ratio<=0.9 -- faster", // not a slowdown
		"Bench-Accept: Engine_MatchInto <=1.1 -- typo",
		"Bench-Accept-Allocs: Engine_* +0/op -- nothing",
		"Bench-Accept-Allocs: Engine_* 1/op -- no plus",
		"Bench-Gate-Change:",
	}
	for _, line := range bad {
		if _, err := parseTrailer("c0ffee", line); err == nil {
			t.Errorf("%q was accepted", line)
		}
	}
	if a, err := parseTrailer("c0ffee", "Signed-off-by: someone"); a != nil || err != nil {
		t.Errorf("an unrelated trailer was read as the gate's: %+v %v", a, err)
	}
}

// TestApplyAcceptances: a trailer covers what was measured and nothing else.
func TestApplyAcceptances(t *testing.T) {
	acc := func(x float64) []*Acceptance {
		return []*Acceptance{{Key: trailerAccept, Glob: "Engine_MatchInto", Ratio: x, Reason: "why"}}
	}
	cases := []struct {
		name     string
		median   float64
		bound    float64
		accepted bool
	}{
		{"covers the measurement", 1.12, 1.15, true},
		{"tighter than measured", 1.20, 1.15, false},
		{"looser than measured x 1.10", 1.12, 1.30, false},
	}
	for _, tc := range cases {
		v := Verdict{Bench: "BenchmarkEngine_MatchInto", Timing: "fail", Median: tc.median}
		problems := applyAcceptances(&v, acc(tc.bound))
		if got := strings.HasPrefix(v.Timing, "accepted"); got != tc.accepted || (len(problems) == 0) != tc.accepted {
			t.Errorf("%s: timing %q, problems %v", tc.name, v.Timing, problems)
		}
	}
	allocs := []*Acceptance{{Key: trailerAcceptAllocs, Glob: "Engine_*", Allocs: 1, Reason: "why"}}
	v := Verdict{Bench: "BenchmarkEngine_Match", Allocations: "fail", AllocsBase: 4, AllocsHead: 6}
	if p := applyAcceptances(&v, allocs); len(p) == 0 || v.Allocations != "fail" {
		t.Errorf("+1/op accepted a measured +2/op: %q %v", v.Allocations, p)
	}
	// The over-generous direction: a trailer may not pre-accept more than was measured.
	generous := []*Acceptance{{Key: trailerAcceptAllocs, Glob: "Engine_*", Allocs: 3, Reason: "why"}}
	v = Verdict{Bench: "BenchmarkEngine_Match", Allocations: "fail", AllocsBase: 4, AllocsHead: 5}
	if p := applyAcceptances(&v, generous); len(p) == 0 || v.Allocations != "fail" {
		t.Errorf("+3/op accepted a measured +1/op: %q %v", v.Allocations, p)
	}
	v = Verdict{Bench: "BenchmarkEngine_Match", Allocations: "fail", AllocsBase: 4, AllocsHead: 5}
	if p := applyAcceptances(&v, allocs); len(p) != 0 || !strings.HasPrefix(v.Allocations, "accepted") {
		t.Errorf("+1/op did not accept a measured +1/op: %q %v", v.Allocations, p)
	}
}

func TestSettle(t *testing.T) {
	old := enforcedTiming
	defer func() { enforcedTiming = old }()
	enforcedTiming = map[string]bool{"BenchmarkA": true}

	v := Verdict{Bench: "BenchmarkA", Timing: "fail", Allocations: "pass"}
	if !settle(&v, false) {
		t.Error("an enforced timing failure did not fail")
	}
	v = Verdict{Bench: "BenchmarkB", Timing: "fail", Allocations: "pass"}
	if settle(&v, false) || !strings.Contains(v.Timing, "report only") {
		t.Errorf("a timing failure calibration did not clear failed the run: %q", v.Timing)
	}
	v = Verdict{Bench: "BenchmarkA", Timing: "inconclusive", Allocations: "pass"}
	if settle(&v, false) {
		t.Error("a first inconclusive failed the run")
	}
	v = Verdict{Bench: "BenchmarkA", Timing: "inconclusive", Allocations: "pass"}
	if !settle(&v, true) || v.Timing != "inconclusive twice" {
		t.Errorf("a second inconclusive on an enforced benchmark: %q", v.Timing)
	}
	v = Verdict{Bench: "BenchmarkB", Timing: "pass", Allocations: "fail"}
	if !settle(&v, false) {
		t.Error("an allocation failure on a report-only benchmark did not fail; allocations are always enforced")
	}
}

// TestReadTrailers runs against a real repository: trailers are read from every
// commit in the range, and a range that changes the gate without saying so fails.
func TestReadTrailers(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("pkg/x.go", "package x\n")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")

	write("pkg/x.go", "package x\n// slower\n")
	git("commit", "-q", "-am", "slower on purpose\n\nBench-Accept: Engine_MatchInto ratio<=1.15 -- measured and wanted")
	accs, problems := readTrailers(dir, base)
	if len(problems) != 0 || len(accs) != 1 || accs[0].Ratio != 1.15 {
		t.Fatalf("one acceptance: %+v %v", accs, problems)
	}

	write("cmd/benchgate/judge.go", "package main\n")
	git("add", ".")
	git("commit", "-q", "-m", "touch the gate")
	if _, problems := readTrailers(dir, base); len(problems) != 1 || !strings.Contains(problems[0], "changes the gate") {
		t.Fatalf("a gate change without its trailer: %v", problems)
	}
	write("cmd/benchgate/judge.go", "package main\n// again\n")
	git("commit", "-q", "-am", "say so\n\nBench-Gate-Change: tighten nothing, a comment")
	if _, problems := readTrailers(dir, base); len(problems) != 0 {
		t.Fatalf("a gate change with its trailer: %v", problems)
	}
	write("pkg/x.go", "package x\n// malformed\n")
	git("commit", "-q", "-am", "oops\n\nBench-Accept: everything")
	if _, problems := readTrailers(dir, base); len(problems) != 1 || !strings.Contains(problems[0], "malformed") {
		t.Fatalf("a malformed trailer: %v", problems)
	}
}
