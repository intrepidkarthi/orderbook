package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// pkgs are the packages the gate builds, with the iteration count every one of
// their benchmarks runs at. Fixed work, never time-based: a time-based b.N measures
// a different book on a faster machine.
var pkgs = []struct {
	path string
	n    string
}{
	{"internal/benchgate", "5x"},
	{"pkg/matching", "300000x"},
	{"pkg/orderbook", "500000x"},
}

// nOverride is the iteration count for a benchmark whose op is much heavier than
// its package's.
var nOverride = map[string]string{
	"BenchmarkLatency_MassCancelBurst": "200x",
}

// timingGated are the benchmarks whose time may fail a build. Everything else in
// the gate is compared on allocations only: their book depends on b.N, or they are
// tails, or they are not what the gate is for (docs/BENCH-GATE.md §5.2).
var timingGated = []string{
	"BenchmarkTapeReplay/sink=nil",
	"BenchmarkEngine_MatchInto",
	"BenchmarkEngine_CancelReplaceInto",
	"BenchmarkOrderBook_CancelReplace",
	"BenchmarkOrderBook_LevelChurn",
}

// allocGated names the families compared on allocations, by prefix.
var allocGated = regexp.MustCompile(`^Benchmark(TapeReplay|OrderBook_|Engine_|Latency_)`)

// notGated is excluded from both: it measures the machine's core count.
var notGated = map[string]bool{"BenchmarkShards_Scaling": true}

const budget = 20 * time.Minute

// tree is one side of the comparison: a checkout and the test binaries built from it.
type tree struct {
	arm  string
	dir  string
	sha  string
	bins map[string]string // package path -> test binary
	goV  map[string]string // package path -> Go version the binary was built with
}

// Report is bench-result.json (docs/BENCH-GATE.md §6).
type Report struct {
	Mode         string            `json:"mode"`
	Enforce      bool              `json:"enforce"`
	BaseSHA      string            `json:"base_sha"`
	HeadSHA      string            `json:"head_sha"`
	Round1Order  string            `json:"round1_order"`
	Rounds       int               `json:"rounds"`
	Benchtime    map[string]string `json:"benchtime"`
	TapeFile     string            `json:"tape_file"`
	TapeSHA256   string            `json:"tape_sha256"`
	DigestBase   string            `json:"digest_base"`
	DigestHead   string            `json:"digest_head"`
	SourceHashes map[string]string `json:"source_hashes"`
	Machine      Machine           `json:"machine"`
	Retried      []string          `json:"retried"`
	Verdicts     []Verdict         `json:"verdicts"`
	Invocations  []Invocation      `json:"invocations"`
	Notes        []string          `json:"notes"`
	Failed       bool              `json:"failed"`
}

func compare(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	baseDir := fs.String("base-dir", "", "checkout of the base revision")
	headDir := fs.String("head-dir", ".", "checkout of the head revision")
	enforce := fs.Bool("enforce", false, "exit non-zero on a timing or allocation failure")
	out := fs.String("out", "bench-result.json", "where to write the report")
	mode := fs.String("mode", "compare", "recorded in the report: compare, aa or anchor")
	only := fs.String("only", "", "regexp: restrict to matching benchmarks (local use)")
	fs.Parse(args)
	if *baseDir == "" {
		fmt.Fprintln(os.Stderr, "benchgate: -base-dir is required")
		return 2
	}

	deadline := time.Now().Add(budget)
	rep := &Report{Mode: *mode, Enforce: *enforce, Rounds: rounds, Benchtime: map[string]string{},
		SourceHashes: map[string]string{}, Machine: machine()}
	work, err := os.MkdirTemp("", "benchgate-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer os.RemoveAll(work)

	base := &tree{arm: "base", dir: *baseDir, bins: map[string]string{}, goV: map[string]string{}}
	head := &tree{arm: "head", dir: *headDir, bins: map[string]string{}, goV: map[string]string{}}
	for _, t := range []*tree{base, head} {
		t.sha = revision(t.dir)
		for _, p := range pkgs {
			bin := filepath.Join(work, t.arm+"-"+strings.ReplaceAll(p.path, "/", "_")+".test")
			cmd := exec.Command("go", "test", "-c", "-o", bin, "./"+p.path)
			cmd.Dir = t.dir
			if msg, err := cmd.CombinedOutput(); err != nil {
				rep.Notes = append(rep.Notes, fmt.Sprintf("%s: %s did not build: %s", t.arm, p.path, firstLine(msg)))
				continue
			}
			t.bins[p.path] = bin
			if bi, err := buildinfo.ReadFile(bin); err == nil {
				t.goV[p.path] = bi.GoVersion
			}
		}
	}
	rep.BaseSHA, rep.HeadSHA = base.sha, head.sha
	rep.Machine.GoVersionBase, rep.Machine.GoVersionHead = anyValue(base.goV), anyValue(head.goV)
	sameToolchain := rep.Machine.GoVersionBase == rep.Machine.GoVersionHead

	// Both arms replay BASE's tape, which base can always parse, and each prints the
	// digest it produced. If they differ, matching behaviour on the tape changed, and
	// timing a different computation is not a comparison.
	tape, _ := filepath.Abs(filepath.Join(base.dir, "internal/benchgate/testdata/bench-v1.obt"))
	baseTape = tape
	rep.TapeFile = "bench-v1.obt (base)"
	if b, err := os.ReadFile(tape); err == nil {
		sum := sha256.Sum256(b)
		rep.TapeSHA256 = hex.EncodeToString(sum[:])
		rep.DigestBase = printDigest(base, tape)
		rep.DigestHead = printDigest(head, tape)
	}

	// Round 1's order comes from the head revision, so it is recorded and not chosen.
	clean := strings.TrimSuffix(head.sha, "-dirty")
	headFirst := clean != "" && strings.ContainsRune("13579bdf", rune(clean[len(clean)-1]))
	rep.Round1Order = "ABBA (base first)"
	if headFirst {
		rep.Round1Order = "BAAB (head first)"
	}

	names := benchmarks(base, head)
	re, _ := regexp.Compile(*only)
	for _, name := range names {
		if *only != "" && !re.MatchString(name.full) {
			continue
		}
		v := Verdict{Bench: name.full}
		rep.Benchtime[name.full] = name.n
		if why := classify(name, base, head, sameToolchain); why != "" {
			v.Timing, v.Allocations = why, why
		} else {
			hb, hh := sourceHash(base.dir, name), sourceHash(head.dir, name)
			rep.SourceHashes[name.full] = hh
			changed := hb != hh
			isTiming := contains(timingGated, name.full)
			var invs []Invocation
			digestBlocks := name.full == timingGated[0] &&
				(rep.DigestBase != rep.DigestHead || rep.DigestBase == "unknown")
			if isTiming && !changed && !digestBlocks {
				invs = runRounds(base, head, name, headFirst, deadline)
				judgeTiming(&v, pairUp(invs))
				if v.Timing == "inconclusive" && time.Now().Before(deadline) {
					rep.Retried = append(rep.Retried, name.full)
					invs = runRounds(base, head, name, !headFirst, deadline)
					v = Verdict{Bench: name.full}
					judgeTiming(&v, pairUp(invs))
				}
			} else {
				switch {
				case !isTiming:
					v.Timing = "not gated"
				case changed:
					v.Timing = "not compared: benchmark changed"
				case rep.DigestBase == "unknown" || rep.DigestHead == "unknown":
					v.Timing = "not compared: digest unknown"
				default:
					v.Timing = "not compared: digest differs"
				}
				// Three per arm, interleaved: a single run's count carries a few
				// allocations of runtime noise, enough to tip an integer-divided
				// allocs/op across a boundary; the median of three does not move.
				for r := 1; r <= 3; r++ {
					for i, arm := range []*tree{base, head} {
						invs = append(invs, invoke(arm, name, r, i, deadline))
					}
				}
			}
			judgeAllocs(&v, invs)
			if changed {
				v.Allocations = "not compared: benchmark changed"
			}
			rep.Invocations = append(rep.Invocations, invs...)
		}
		if v.Timing == "fail" || v.Allocations == "fail" || v.Timing == "missing" {
			rep.Failed = true
		}
		rep.Verdicts = append(rep.Verdicts, v)
	}
	rep.Machine.LoadAfter = loadavg()

	if b, err := json.MarshalIndent(rep, "", "  "); err == nil {
		os.WriteFile(*out, b, 0o644)
	}
	summary := renderSummary(rep)
	fmt.Print(summary)
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			io.WriteString(f, summary)
			f.Close()
		}
	}
	if *enforce && rep.Failed {
		return 1
	}
	return 0
}

// classify says why a benchmark cannot be compared at all, or "" when it can. A
// benchmark only head has is new; one only base has is missing, and missing fails
// under -enforce, because deleting a gated benchmark is one way to pass the gate.
func classify(b bench, base, head *tree, sameToolchain bool) string {
	switch {
	case !sameToolchain:
		return "not compared: different Go versions"
	case base.bins[b.pkg] == "" || !b.inBase:
		return "new"
	case head.bins[b.pkg] == "" || !b.inHead:
		return "missing"
	}
	return ""
}

// bench is one benchmark the gate knows about.
type bench struct {
	pkg, top, sub, full, n string
	inBase, inHead         bool
}

// benchmarks lists every gated benchmark in either tree.
func benchmarks(base, head *tree) []bench {
	seen := map[string]*bench{}
	var order []string
	for _, t := range []*tree{base, head} {
		for _, p := range pkgs {
			bin := t.bins[p.path]
			if bin == "" {
				continue
			}
			cmd := exec.Command(bin, "-test.list", "^Benchmark")
			cmd.Dir = filepath.Join(t.dir, p.path)
			outb, err := cmd.Output()
			if err != nil {
				continue
			}
			for _, top := range strings.Fields(string(outb)) {
				if !allocGated.MatchString(top) || notGated[top] {
					continue
				}
				full, sub := top, ""
				if top == "BenchmarkTapeReplay" {
					full, sub = "BenchmarkTapeReplay/sink=nil", "sink=nil"
				}
				b := seen[full]
				if b == nil {
					n := p.n
					if o, ok := nOverride[top]; ok {
						n = o
					}
					b = &bench{pkg: p.path, top: top, sub: sub, full: full, n: n}
					seen[full] = b
					order = append(order, full)
				}
				if t.arm == "base" {
					b.inBase = true
				} else {
					b.inHead = true
				}
			}
		}
	}
	sort.Strings(order)
	out := make([]bench, 0, len(order))
	for _, k := range order {
		out = append(out, *seen[k])
	}
	return out
}

// runRounds runs 10 rounds, A B B A on odd rounds and B A A B on even ones, flipped
// when head goes first.
func runRounds(base, head *tree, b bench, headFirst bool, deadline time.Time) []Invocation {
	var invs []Invocation
	for r := 1; r <= rounds; r++ {
		order := []*tree{base, head, head, base}
		if (r%2 == 0) != headFirst {
			order = []*tree{head, base, base, head}
		}
		for slot, t := range order {
			invs = append(invs, invoke(t, b, r, slot, deadline))
		}
	}
	return invs
}

// invoke runs one benchmark once, probes first.
func invoke(t *tree, b bench, round, slot int, deadline time.Time) Invocation {
	v := Invocation{Bench: b.full, Arm: t.arm, Round: round, Slot: slot}
	if time.Now().After(deadline) {
		v.Failed = true
		return v
	}
	v.ProbeALU = float64(aluProbe().Nanoseconds())
	v.ProbeMem = float64(memProbe().Nanoseconds())
	pattern := "^" + b.top + "$"
	if b.sub != "" {
		pattern += "/^" + regexp.QuoteMeta(b.sub) + "$"
	}
	cmd := exec.Command(t.bins[b.pkg], "-test.run", "^$", "-test.bench", pattern,
		"-test.benchtime", b.n, "-test.count", "1", "-test.benchmem")
	cmd.Dir = filepath.Join(t.dir, b.pkg)
	// Both arms replay base's tape; see compare.
	cmd.Env = append(os.Environ(), "BENCHGATE_TAPE="+baseTape)
	outb, err := cmd.CombinedOutput()
	if err != nil {
		v.Failed = true
		return v
	}
	if b.sub == "" && !strings.Contains(b.top, "/") {
		// A top-level benchmark with sub-benchmarks prints several lines; allocation
		// comparison then takes their sum, and timing is never asked of one.
		if r, err := parseOutput(string(outb)); err == nil {
			v.NsPerOp, v.Allocs = r.NsPerOp, r.Allocs
			return v
		}
		total, ok := sumAllocs(string(outb))
		v.Allocs, v.Failed = total, !ok
		return v
	}
	r, err := parseOutput(string(outb))
	if err != nil {
		v.Failed = true
		return v
	}
	v.NsPerOp, v.Allocs = r.NsPerOp, r.Allocs
	return v
}

// baseTape is the tape both arms replay; set once in compare.
var baseTape string

func sumAllocs(out string) (int64, bool) {
	var total int64
	n := 0
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || !strings.HasPrefix(f[0], "Benchmark") {
			continue
		}
		for i := 2; i+1 < len(f); i += 2 {
			if f[i+1] == "allocs/op" {
				var x int64
				if _, err := fmt.Sscan(f[i], &x); err == nil {
					total += x
					n++
				}
			}
		}
	}
	return total, n > 0
}

// printDigest asks a tree's benchgate test binary for the full-chain digest it
// produces on the given tape. A base too old to have the hook gives "unknown".
func printDigest(t *tree, tapePath string) string {
	bin := t.bins["internal/benchgate"]
	if bin == "" {
		return "unknown"
	}
	cmd := exec.Command(bin, "-test.run", "^TestPrintDigest$", "-test.v")
	cmd.Dir = filepath.Join(t.dir, "internal/benchgate")
	cmd.Env = append(os.Environ(), "BENCHGATE_TAPE="+tapePath, "BENCHGATE_PRINT_DIGEST=1")
	outb, _ := cmd.CombinedOutput()
	if m := regexp.MustCompile(`OBDG full ([0-9a-f]{64})`).FindSubmatch(outb); m != nil {
		return string(m[1])
	}
	return "unknown"
}

// sourceHash fingerprints the file that defines a benchmark, plus, for the tape
// replay, every file of internal/benchgate. A benchmark whose source differs
// between the arms is not the same measurement and is not compared.
func sourceHash(dir string, b bench) string {
	h := sha256.New()
	pkgDir := filepath.Join(dir, b.pkg)
	files, _ := filepath.Glob(filepath.Join(pkgDir, "*.go"))
	sort.Strings(files)
	needle := []byte("func " + b.top + "(")
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if b.pkg == "internal/benchgate" || bytes.Contains(src, needle) {
			h.Write([]byte(filepath.Base(f)))
			h.Write(src)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func revision(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	sha := strings.TrimSpace(string(out))
	if st, _ := exec.Command("git", "-C", dir, "status", "--porcelain", "--untracked-files=no").Output(); len(bytes.TrimSpace(st)) > 0 {
		sha += "-dirty"
	}
	return sha
}

func anyValue(m map[string]string) string {
	for _, p := range pkgs {
		if v, ok := m[p.path]; ok {
			return v
		}
	}
	return ""
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Machine is what the run happened on (docs/BENCH-GATE.md §6).
type Machine struct {
	GoVersionBase string `json:"go_version_base"`
	GoVersionHead string `json:"go_version_head"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	GOMAXPROCS    int    `json:"gomaxprocs"`
	NumCPU        int    `json:"num_cpu"`
	GOGC          string `json:"gogc"`
	GOMEMLIMIT    string `json:"gomemlimit"`
	CPUModel      string `json:"cpu_model"`
	Kernel        string `json:"kernel"`
	RunnerImage   string `json:"runner_image"`
	RunnerVersion string `json:"runner_image_version"`
	LoadBefore    string `json:"loadavg_before"`
	LoadAfter     string `json:"loadavg_after"`
}

func machine() Machine {
	m := Machine{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GOMAXPROCS: runtime.GOMAXPROCS(0), NumCPU: runtime.NumCPU(),
		GOGC: os.Getenv("GOGC"), GOMEMLIMIT: os.Getenv("GOMEMLIMIT"),
		RunnerImage: envOr("ImageOS", "unknown"), RunnerVersion: envOr("ImageVersion", "unknown"),
		LoadBefore: loadavg(),
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
				m.CPUModel = strings.TrimSpace(v)
				break
			}
		}
	} else if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		m.CPUModel = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		m.Kernel = strings.TrimSpace(string(out))
	}
	return m
}

func loadavg() string {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		return strings.Join(strings.Fields(string(b))[:3], " ")
	}
	if out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil {
		return strings.Trim(strings.TrimSpace(string(out)), "{} ")
	}
	return "unknown"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// renderSummary is the table a person reads: in the job log and the step summary.
func renderSummary(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Benchmark gate (%s%s)\n\n", r.Mode, map[bool]string{true: ", enforce", false: ", report only"}[r.Enforce])
	fmt.Fprintf(&b, "base `%s` · head `%s` · %s · %s %s/%s, %d CPUs · load %s → %s\n\n",
		short(r.BaseSHA), short(r.HeadSHA), r.Round1Order, r.Machine.CPUModel, r.Machine.GOOS, r.Machine.GOARCH,
		r.Machine.NumCPU, r.Machine.LoadBefore, r.Machine.LoadAfter)
	if r.DigestBase != r.DigestHead {
		fmt.Fprintf(&b, "Digest on base's tape differs: base `%s`, head `%s`. TapeReplay timing is not compared.\n\n", short(r.DigestBase), short(r.DigestHead))
	}
	b.WriteString("| benchmark | timing | median | k-th | tainted | allocs base → head | allocations |\n|---|---|---:|---:|---:|---|---|\n")
	for _, v := range r.Verdicts {
		med, kth := "", ""
		if v.Median > 0 {
			med, kth = fmt.Sprintf("%.3f", v.Median), fmt.Sprintf("%.3f", v.KthRatio)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %d → %d | %s |\n", v.Bench, v.Timing, med, kth, v.Tainted, v.AllocsBase, v.AllocsHead, v.Allocations)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "\n- %s", n)
	}
	if len(r.Retried) > 0 {
		fmt.Fprintf(&b, "\n- retried once after an inconclusive run: %s", strings.Join(r.Retried, ", "))
	}
	verdict := "pass"
	if r.Failed {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "\n\n**%s**. Thresholds are targets until the A/A calibration in docs/BENCH-GATE.md §9.4 has run.\n", verdict)
	return b.String()
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
