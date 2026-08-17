package semcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

const goldenPath = "testdata/semantics.txt"

// TestMatchingSemanticsAreFrozen is the enforcement, and the whole slice stands or
// falls on it.
//
// A version somebody has to REMEMBER to bump is worse than no version at all, because
// it promises detection it does not provide: the first two people to touch pkg/matching
// will remember, the third will not, and from that day the stamp on every segment says
// "these two logs replay identically" while they do not. So the number is not
// maintained by discipline. It is maintained by this test.
//
// Four outcomes, and the third is the one with teeth:
//
//  1. Body identical, version identical — pass. Nothing changed.
//  2. Body CHANGED, version unchanged — FAIL, and the failure does not offer
//     regeneration as its first option. Either the change was not supposed to alter
//     behaviour, in which case the diff below says where it did, or it was, and the
//     number has to move.
//  3. Regeneration with SEMCHECK_UPDATE=1 while the version is unchanged — REFUSED.
//     This is the tooth. The path of least resistance for somebody who has changed
//     behaviour and hit outcome 2 is to regenerate, and regeneration is precisely
//     where the bump is demanded. There is no sequence of commands that produces a
//     green tree with changed behaviour and an unchanged number.
//  4. Version bumped, body IDENTICAL — FAIL, which looks backwards and is not. A bump
//     that changes nothing observable is a false alarm; false alarms are what teach an
//     operator to put -wal-accept-semantics in the unit file permanently; and a stamp
//     that fires on every release is the useless version docs/SEMANTICS-VERSION.md §1.1
//     opens by rejecting. The route forward when you believe you changed behaviour the
//     corpus cannot see is to EXTEND THE CORPUS until it can — and if no scenario can
//     be written in which the change is observable, then by §2.1's definition it is not
//     a semantics change and does not need a bump.
//
// Regenerate with: SEMCHECK_UPDATE=1 go test ./internal/semcheck/
// Then read the diff before committing it. That reading is the whole point.
func TestMatchingSemanticsAreFrozen(t *testing.T) {
	got, _, err := Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	wantBytes, readErr := os.ReadFile(goldenPath)
	haveGolden := readErr == nil
	want := string(wantBytes)

	goldenVersion := -1
	if haveGolden {
		goldenVersion = versionOf(t, want)
	}
	sameBody := haveGolden && bodyOf(want) == bodyOf(got)

	if os.Getenv("SEMCHECK_UPDATE") == "1" {
		if haveGolden && matching.SemanticsVersion <= goldenVersion {
			t.Fatalf(`REFUSED to regenerate %s.

matching.SemanticsVersion is %d and the golden was recorded at %d, so this
regeneration would record a change in matching behaviour under a number that
says nothing changed. Every log segment and every snapshot this build writes
carries that number, and wal.Recover uses it to decide whether a journal
written by another build can be replayed. Recording new behaviour under the
old number makes recovery start CONFIDENTLY on a log it should have refused.

Raise matching.SemanticsVersion to %d, add its row to
docs/SEMANTICS-VERSION.md §1.2 naming the changelog entries it covers, and
regenerate again.`, goldenPath, matching.SemanticsVersion, goldenVersion, goldenVersion+1)
		}
		if haveGolden && sameBody && matching.SemanticsVersion > goldenVersion {
			t.Fatalf(`REFUSED to regenerate %s.

matching.SemanticsVersion moved from %d to %d and the corpus produces exactly
the same outcomes it did before — same verdicts, same trades, same events,
same digests. A version that changes when nothing observable changed is a
false alarm, and a stamp that fires on every release is one operators learn
to override permanently; the next real mismatch is then accepted silently by
a flag nobody remembers adding. docs/SEMANTICS-VERSION.md §5.4 Rule 22.

If you believe you changed behaviour this corpus cannot see, EXTEND THE
CORPUS in corpus.go until it can, and regenerate then. If no scenario can be
written in which the change is observable, it is not a semantics change (§2.1)
and the constant should go back to %d.`, goldenPath, goldenVersion, matching.SemanticsVersion, goldenVersion)
		}
		if !haveGolden {
			// The one supported way to change the RENDERER without changing behaviour
			// is to delete the golden and regenerate, and it is deliberately loud: a
			// recreated golden is a whole-file diff in review, which is the only thing
			// standing between a reformat and a behaviour change wearing one. Never do
			// it to make a failing build pass.
			t.Logf("no golden at %s — writing one at semantics %d WITHOUT any baseline to compare against. "+
				"The version rule cannot be enforced on a file that does not exist, so the diff is the only "+
				"check: read the whole of it.", goldenPath, matching.SemanticsVersion)
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		t.Logf("regenerated %s at semantics %d (%d lines)", goldenPath, matching.SemanticsVersion, strings.Count(got, "\n"))
		return
	}

	if !haveGolden {
		t.Fatalf("missing %s: %v (regenerate with SEMCHECK_UPDATE=1)", goldenPath, readErr)
	}
	if got == want {
		return
	}

	if sameBody {
		// Only the version line moved. That is outcome 4 arriving through a plain
		// test run rather than through a regeneration.
		t.Fatalf("matching.SemanticsVersion is %d and %s records %d, but the engine's behaviour on the whole "+
			"corpus is unchanged.\n\nA version that fires when nothing changed refuses logs that would replay "+
			"identically, and the response to a check that cries wolf is a permanent override. Extend the corpus "+
			"until the change is observable, or put the constant back. docs/SEMANTICS-VERSION.md §5.4 Rule 22.",
			matching.SemanticsVersion, goldenPath, goldenVersion)
	}

	var b strings.Builder
	b.WriteString("MATCHING BEHAVIOUR CHANGED.\n\n")
	added, removed := lineDiff(want, got)
	b.WriteString(sample("was", removed))
	b.WriteString(sample("now", added))
	if matching.SemanticsVersion == goldenVersion {
		fmt.Fprintf(&b, "\nmatching.SemanticsVersion is still %d.\n\n"+
			"Either this change was not supposed to alter what the engine does with a command — in which case\n"+
			"find out why it did, and the lines above say where — or it was, and the number has to move. Every\n"+
			"log segment and every snapshot carries that number, and wal.Recover uses it to decide whether a\n"+
			"journal written by another build can be replayed into this one.\n\n"+
			"If the change is intended:\n"+
			"  1. Raise matching.SemanticsVersion in pkg/matching/semantics.go to %d.\n"+
			"  2. Add its row to docs/SEMANTICS-VERSION.md §1.2, naming the changelog entries it covers.\n"+
			"  3. SEMCHECK_UPDATE=1 go test ./internal/semcheck/ and read the diff.\n",
			goldenVersion, goldenVersion+1)
	} else {
		fmt.Fprintf(&b, "\nmatching.SemanticsVersion moved from %d to %d, which is the right half of the change.\n"+
			"Regenerate and read the diff:  SEMCHECK_UPDATE=1 go test ./internal/semcheck/\n",
			goldenVersion, matching.SemanticsVersion)
	}
	t.Fatal(b.String())
}

// sample prints at most a handful of moved lines. A behaviour change usually moves
// hundreds — every command after the first divergence — and a failure that prints all
// of them is a failure nobody reads. The FIRST few are the ones that say what moved.
func sample(label string, lines []string) string {
	const max = 6
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d lines):\n", label, len(lines))
	for i, l := range lines {
		if i == max {
			fmt.Fprintf(&b, "  ... and %d more\n", len(lines)-max)
			break
		}
		b.WriteString("  " + l + "\n")
	}
	return b.String()
}

// lineDiff reports the first lines that differ, in file order, which is what names the
// command whose outcome moved.
func lineDiff(want, got string) (added, removed []string) {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	n := len(w)
	if len(g) < n {
		n = len(g)
	}
	for i := 0; i < n; i++ {
		if w[i] != g[i] {
			removed = append(removed, w[i])
			added = append(added, g[i])
		}
	}
	for i := n; i < len(w); i++ {
		removed = append(removed, w[i])
	}
	for i := n; i < len(g); i++ {
		added = append(added, g[i])
	}
	return added, removed
}

// versionOf reads the version off the golden's first line. It is in the file rather
// than beside it so the two travel together through every copy, diff and review.
func versionOf(tb testing.TB, body string) int {
	tb.Helper()
	first, _, _ := strings.Cut(body, "\n")
	v, ok := strings.CutPrefix(first, "semantics ")
	if !ok {
		tb.Fatalf("%s does not begin with a version line; first line is %q", goldenPath, first)
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		tb.Fatalf("%s declares an unreadable version %q: %v", goldenPath, v, err)
	}
	return n
}

// bodyOf is the golden without its version line, which is what "did behaviour change"
// is asked of.
//
// Splitting them is what makes the four outcomes distinguishable at all. With the
// version inside the compared text, a bump would move the body by itself and every
// bump would satisfy its own evidence — the same circularity
// docs/SEMANTICS-VERSION.md §2.4 keeps Semantics out of EngineSnapshot.Digest to
// avoid, in a second place.
func bodyOf(s string) string {
	_, rest, _ := strings.Cut(s, "\n")
	return rest
}

// TestTheFingerprintIsDeterministic runs the corpus twice in one process and requires
// identity.
//
// It is aimed at exactly two things: a clock leaking in, and a map iterated in Go's
// randomised order. Either would make the golden a function of when it was generated
// rather than of what the engine does, and the failure would look like a behaviour
// change to whoever hit it next — sending them to bump a version for nothing, which is
// the false alarm Rule 22 exists to refuse.
func TestTheFingerprintIsDeterministic(t *testing.T) {
	a, _, err := Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	b, _, err := Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if a != b {
		added, removed := lineDiff(a, b)
		t.Fatalf("two runs of the same corpus in one process disagree — the fingerprint is not a function of the "+
			"engine alone. A wall clock or a map iteration has leaked in.\n%s%s",
			sample("run 1", removed), sample("run 2", added))
	}
	if Digest(a) != Digest(b) {
		t.Fatal("digests disagree on identical bodies")
	}
}

// TestTheGoldenNamesThisBuildsSemantics is the small, cheap half of the freeze: the
// number in the file and the number in the code must be the same, whatever the body
// says. It fails on a bump that was never regenerated, which is the state a
// half-finished change leaves behind, and it says so in one line rather than through a
// hundred-line body diff.
func TestTheGoldenNamesThisBuildsSemantics(t *testing.T) {
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", goldenPath, err)
	}
	if got := versionOf(t, string(raw)); got != matching.SemanticsVersion {
		t.Errorf("%s records semantics %d and matching.SemanticsVersion is %d.\n"+
			"Regenerate with SEMCHECK_UPDATE=1 and read the diff; if the body does not move, the bump has "+
			"nothing behind it (§5.4 Rule 22).", goldenPath, got, matching.SemanticsVersion)
	}
}

// TestTheFingerprintReachesEveryDecidedBehaviour is the corpus's own guard.
//
// Rule 22 makes the corpus load-bearing: a change on a path the corpus never reaches
// cannot be bumped for, so a corpus that quietly narrows turns the whole mechanism off
// without failing anything. The two halves of this guard are NOT equally strong and
// pretending otherwise is how a guard reports coverage it does not have.
//
// COMPILER-ENUMERABLE, therefore mechanical. matching.EventKind is a uint8 iota block
// with an eventKindCount sentinel at the end of it, and matching's own
// TestEventKindNamesAreTotalOverTheBlock pins String as a faithful enumerator of that
// block — so the loop below walks the ENUM rather than a list written here, and a kind
// added to it fails this test the moment it is declared. This package's own Kind block
// has the same treatment through kindCount.
//
// DECLARED-LIST, therefore only as good as the list. types.OrderType,
// types.TimeInForce and matching.SelfTradePrevention are STRING types, so there is no
// set the compiler can iterate and no sentinel that can be added to one. The lists
// below are hand-written, and a new value declared in pkg/types is invisible here
// until somebody adds a line. That is a real hole; it is the same hole the wire codec
// and the alphabet guards have; and docs/SEMANTICS-VERSION.md §5.5 states it rather
// than leaving a review to find it.
func TestTheFingerprintReachesEveryDecidedBehaviour(t *testing.T) {
	_, cov, err := Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Every command kind this package can issue was issued, which is what makes the
	// pkg/wal guard against entryKindCount meaningful: that test maps these names
	// onto wal.EntryKind, so a kind missing here would be missing there too.
	for k := Kind(0); k < kindCount; k++ {
		if cov.Commands[k.Name()] == 0 {
			t.Errorf("the corpus never issued %s; a change to it would move no line of the golden", k.Name())
		}
	}

	// Every event kind except the deprecated one. EventBookDelta is declared,
	// deprecated and deliberately never emitted (see its note in pkg/matching/event.go),
	// so it is a NAMED exception with a citation rather than a catch-all.
	for k := 0; matching.EventKind(k).String() != "UNKNOWN"; k++ {
		if matching.EventKind(k) == matching.EventBookDelta {
			if cov.Events[k] != 0 {
				t.Errorf("EventBookDelta was emitted %d times; it is documented as never emitted", cov.Events[k])
			}
			continue
		}
		if cov.Events[k] == 0 {
			t.Errorf("the corpus never published %s; a change to when it is emitted would move no line",
				matching.EventKind(k))
		}
	}

	// The string-typed axes, from a declared list. See the doc comment: this half is
	// only as good as the list.
	for _, ot := range []string{"LIMIT", "MARKET"} {
		if cov.OrderTypes[ot] == 0 {
			t.Errorf("no order of type %s reached the book", ot)
		}
	}
	// The resting time-in-force values, from an accepted order. IOC and FOK never
	// rest, so they cannot be counted this way and are asserted by outcome below.
	for _, tif := range []string{"GTC", "DAY", "GTD"} {
		if cov.TIFs[tif] == 0 {
			t.Errorf("no %s order reached the book", tif)
		}
	}
	if cov.Verdicts["REJECTED"] == 0 || cov.Rejections["FOK_CANNOT_FILL"] == 0 {
		t.Error("the corpus never reached a fill-or-kill that could not fill — the path whose LastTradePrice " +
			"handling is one of the three changes semantics 1 records")
	}
	if cov.Rejections["MARKET_NO_LIQUIDITY"] == 0 {
		t.Error("the corpus never reached a market order with no liquidity")
	}
	for _, mode := range []matching.SelfTradePrevention{
		matching.STPCancelNewest, matching.STPCancelOldest,
		matching.STPCancelBoth, matching.STPDecrement, matching.STPAllow,
	} {
		if cov.STPModes[string(mode)] == 0 {
			t.Errorf("self-trade prevention mode %s was never decided on an actual self-match", mode)
		}
	}

	// Both allocation policies, because the change that opened this whole gap was in
	// one of them and a corpus running only the other could not have seen it.
	for _, p := range []string{"fifo", "prorata"} {
		if cov.Policies[p] == 0 {
			t.Errorf("no scenario ran under %s allocation", p)
		}
	}

	// And the aggregate behaviours a coverage regression drains away while every
	// individual line stays put.
	for _, c := range []struct {
		name string
		got  int
	}{
		{"trades", cov.Trades},
		{"auction trades", cov.AuctionTrades},
		{"uncrosses", cov.Uncrosses},
		{"stop triggers", cov.Triggers},
		{"iceberg refills", cov.Refills},
		{"busts", cov.Busts},
	} {
		if c.got == 0 {
			t.Errorf("the corpus reached zero %s", c.name)
		}
	}

	// An unmapped rejection means the corpus reached a refusal this package cannot
	// name, so the golden is carrying an error STRING — which moves when somebody
	// rewords a message and demands a bump for nothing.
	for name, n := range cov.Rejections {
		if strings.HasPrefix(name, "UNMAPPED") {
			t.Errorf("the corpus reached %d refusals with no stable name: %s\n"+
				"Add the sentinel to semcheck.sentinels; error strings must not be in the golden.", n, name)
		}
	}
}

// TestTheCorpusIsNotTrivial is a floor under the other tests. A corpus of nothing
// passes the freeze, passes determinism, and proves nothing; a golden that shrank by
// an order of magnitude is a coverage regression wearing a green tick.
func TestTheCorpusIsNotTrivial(t *testing.T) {
	body, _, err := Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	const floor = 800
	if n := strings.Count(body, "\n"); n < floor {
		t.Errorf("the fingerprint is %d lines, below the %d-line floor — the corpus has shrunk", n, floor)
	}
	if len(Corpus()) < 5 {
		t.Errorf("the corpus is %d scenarios; the tier-2 script and both allocation policies are not optional",
			len(Corpus()))
	}
}

func TestGoldenLivesWhereItSaysItDoes(t *testing.T) {
	if _, err := os.Stat(filepath.Dir(goldenPath)); err != nil {
		t.Fatalf("testdata is missing: %v", err)
	}
}
