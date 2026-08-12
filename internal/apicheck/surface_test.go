package apicheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportedSurfaceIsFrozen is the compatibility freeze, and it works exactly the
// way internal/wire's golden vectors work: the file in testdata is the contract, a
// diff against it is a compatibility event, and regenerating it is a deliberate act
// with a human attached.
//
// It does not prevent a breaking change. Nothing can, and a check that tried would
// be gamed within a week. What it prevents is a breaking change nobody noticed —
// which is the shape every break in this repository has taken so far: five methods
// added to matching.CommandLog, matching.Shards refusing what it used to accept,
// two wire bumps in two days. Each was defensible; none announced itself.
//
// Regenerate with: APICHECK_UPDATE=1 go test ./internal/apicheck/
// Then read the diff before committing it. That reading is the whole point.
func TestExportedSurfaceIsFrozen(t *testing.T) {
	root := repoRoot(t)
	got, err := Render(root)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	path := filepath.Join("testdata", "surface.txt")
	if os.Getenv("APICHECK_UPDATE") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		t.Logf("regenerated %s (%d declarations)", path, strings.Count(got, "\n"))
		return
	}

	wantBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing %s: %v (regenerate with APICHECK_UPDATE=1)", path, err)
	}
	want := string(wantBytes)
	if got == want {
		return
	}

	added, removed := diff(want, got)
	var b strings.Builder
	b.WriteString("the exported surface of pkg/... changed.\n\n")
	if len(removed) > 0 {
		b.WriteString("REMOVED or CHANGED — this breaks code that compiles today:\n")
		for _, l := range removed {
			b.WriteString("  - " + l + "\n")
		}
	}
	if len(added) > 0 {
		b.WriteString("ADDED:\n")
		for _, l := range added {
			b.WriteString("  + " + l + "\n")
		}
		b.WriteString("\nAdding to an exported INTERFACE is a breaking change even though it\n" +
			"reads as an addition: every implementer outside this repository stops\n" +
			"compiling. See docs/COMPATIBILITY.md.\n")
	}
	b.WriteString("\nIf this is intended: APICHECK_UPDATE=1 go test ./internal/apicheck/ and\n" +
		"say so in the changelog under a Changed heading.")
	t.Error(b.String())
}

// TestSurfaceCoversEveryPackage guards the guard. A new directory under pkg/ that
// the golden file does not mention would be an unfrozen public package, and the
// freeze would silently not cover it.
func TestSurfaceCoversEveryPackage(t *testing.T) {
	root := repoRoot(t)
	pkgs, err := Covered(root)
	if err != nil {
		t.Fatalf("Covered: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no packages found under pkg/")
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "surface.txt"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	for _, p := range pkgs {
		if !strings.Contains(string(golden), p+".") {
			t.Errorf("pkg/%s has no entry in the frozen surface; it is public and uncovered", p)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

// diff reports lines present in only one of the two renderings.
func diff(want, got string) (added, removed []string) {
	inWant := map[string]bool{}
	for _, l := range strings.Split(want, "\n") {
		if l != "" {
			inWant[l] = true
		}
	}
	inGot := map[string]bool{}
	for _, l := range strings.Split(got, "\n") {
		if l != "" {
			inGot[l] = true
		}
	}
	for l := range inGot {
		if !inWant[l] {
			added = append(added, l)
		}
	}
	for l := range inWant {
		if !inGot[l] {
			removed = append(removed, l)
		}
	}
	sortStrings(added)
	sortStrings(removed)
	return added, removed
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
