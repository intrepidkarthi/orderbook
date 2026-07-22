package matching

import "testing"

// TestZeroAllocHotPath is a regression gate: the steady-state submit/cancel/match
// hot path must allocate nothing on the heap. A GC pause from a stray allocation
// is exactly what stalls a single-writer engine, so this is enforced in CI, not
// left to the benchmark. It runs the zero-alloc benchmarks and asserts 0
// allocs/op — the property that distinguishes this engine from decimal-based or
// slice-returning matchers.
func TestZeroAllocHotPath(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*testing.B)
	}{
		{"Match (maker+taker+trade)", BenchmarkEngine_MatchInto},
		{"CancelReplace (MM churn)", BenchmarkEngine_CancelReplaceInto},
	}
	for _, c := range cases {
		r := testing.Benchmark(c.fn)
		if a := r.AllocsPerOp(); a != 0 {
			t.Errorf("%s: %d allocs/op (%d B/op), want 0", c.name, a, r.AllocedBytesPerOp())
		}
	}
}
