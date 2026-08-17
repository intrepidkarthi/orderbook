package refmatch_test

// The independence rule, enforced rather than promised.
//
// A model that shares code with the thing it checks proves nothing. pkg/types is the
// tempting exception and it is refused: types.Order.Fill is where
// "filled + remaining == quantity" is maintained, and a model that calls it inherits
// whatever that arithmetic does — so the invariant the harness would then "check" is
// one both sides get from the same seven lines.
//
// A prose rule about imports is a rule that survives until the first afternoon
// someone needs types.Side. This parses the files.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowed is the whole permitted non-standard import set: these two packages, and
// each other.
var allowed = map[string]bool{
	"github.com/intrepidkarthi/orderbook/internal/refmatch": true,
	"github.com/intrepidkarthi/orderbook/internal/tape":     true,
}

func TestReferenceMatcherImportsNothing(t *testing.T) {
	for _, dir := range []string{".", "../tape"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir %s: %v", dir, err)
		}
		checked := 0
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			checked++
			for _, imp := range f.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: unquoting import %s: %v", path, imp.Path.Value, err)
				}
				// A standard-library path has no dot in its first segment.
				if !strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
					continue
				}
				if allowed[p] {
					continue
				}
				t.Errorf("%s imports %q. internal/refmatch and internal/tape may import the standard "+
					"library and each other, and nothing else in this module: a model that shares code "+
					"with the engine it checks is not an oracle. See docs/REFERENCE-MATCHER.md §2.2", path, p)
			}
		}
		if checked == 0 {
			t.Fatalf("no Go files found under %s, so this guard checked nothing", dir)
		}
	}
}
