package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassify(t *testing.T) {
	both := &tree{bins: map[string]string{"pkg/x": "bin"}}
	none := &tree{bins: map[string]string{}}
	b := bench{pkg: "pkg/x", inBase: true, inHead: true}
	if got := classify(b, both, both, true); got != "" {
		t.Errorf("a benchmark in both trees: %q", got)
	}
	if got := classify(bench{pkg: "pkg/x", inHead: true}, both, both, true); got != "new" {
		t.Errorf("only in head: %q", got)
	}
	if got := classify(bench{pkg: "pkg/x", inBase: true}, both, both, true); got != "missing" {
		t.Errorf("only in base: %q", got)
	}
	if got := classify(b, both, none, true); got != "missing" {
		t.Errorf("head's package did not build: %q", got)
	}
	if got := classify(b, both, both, false); got != "not compared: different Go versions" {
		t.Errorf("different toolchains: %q", got)
	}
}

// TestSourceHash: a benchmark whose defining file differs between the arms is a
// different measurement, and an edit elsewhere in the package does not count.
func TestSourceHash(t *testing.T) {
	mk := func(files map[string]string) string {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "pkg/x"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, src := range files {
			if err := os.WriteFile(filepath.Join(dir, "pkg/x", name), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	b := bench{pkg: "pkg/x", top: "BenchmarkA"}
	a := mk(map[string]string{"a_test.go": "func BenchmarkA(b *testing.B) {}", "other.go": "var x = 1"})
	same := mk(map[string]string{"a_test.go": "func BenchmarkA(b *testing.B) {}", "other.go": "var x = 2"})
	edited := mk(map[string]string{"a_test.go": "func BenchmarkA(b *testing.B) { _ = 1 }", "other.go": "var x = 1"})
	if sourceHash(a, b) != sourceHash(same, b) {
		t.Error("an edit outside the benchmark's file changed its identity")
	}
	if sourceHash(a, b) == sourceHash(edited, b) {
		t.Error("an edit to the benchmark itself did not change its identity")
	}
}
