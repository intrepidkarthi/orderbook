package tape

import (
	"reflect"
	"strings"
	"testing"
)

// TestLCGVectorIsPinned pins the generator's PRIMITIVE rather than any tape it
// produces.
//
// Pinning generated tapes would make every intentional alphabet extension a
// golden-file churn, and a golden that churns is a golden people regenerate without
// reading. Pinning the arithmetic catches an accidental change to it and lets the
// alphabet grow freely.
func TestLCGVectorIsPinned(t *testing.T) {
	want := []uint64{
		4218421949066224, 7344397812226852, 4197670589265441, 8620003363418412,
		3524272289308783, 4285149302479595, 2014889980214031, 4269892039521284,
	}
	got := Vector(0x5EED1234, len(want))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the generator's stream changed:\n got %v\nwant %v\n\nIf this is intentional, every "+
			"tape in the repository is now a different tape and the change has to be argued, not pasted.", got, want)
	}
}

// TestGenIsDeterministic guards the whole harness: if the tape drifts between runs,
// every assertion built on it becomes meaningless.
func TestGenIsDeterministic(t *testing.T) {
	for _, p := range []Profile{Differential, ProRata, Recovery, Bench} {
		a, b := Gen(p, 7, 300), Gen(p, 7, 300)
		if len(a) != 300 || len(b) != 300 {
			t.Fatalf("%s: Gen returned %d and %d commands, want 300", p.Name, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("%s: tape diverged at %d: %+v vs %+v", p.Name, i, a[i], b[i])
			}
		}
	}
}

// TestDifferentSeedsAreDifferentTapes is the other half: a generator that ignored
// its seed would pass the determinism test perfectly.
func TestDifferentSeedsAreDifferentTapes(t *testing.T) {
	a, b := Gen(Differential, 1, 100), Gen(Differential, 2, 100)
	if reflect.DeepEqual(a, b) {
		t.Fatal("two seeds produced the same tape; the sweep's seeds are decoration")
	}
}

// TestPhasesLandWhereTheProfileSaysThey guards the property the boundary sweeps
// depend on: whatever the tape length, the same session runs.
func TestPhasesLandWhereTheProfileSaysThey(t *testing.T) {
	for _, n := range []int{200, 400, 2000} {
		cmds := Gen(Recovery, 0x5EED1234, n)
		for i, want := range Recovery.Phases {
			if i >= n {
				continue
			}
			if cmds[i].Kind != SetPhase || cmds[i].Phase != want {
				t.Fatalf("n=%d: command %d is %s/%d, want SetPhase/%d", n, i, cmds[i].Kind, cmds[i].Phase, want)
			}
		}
	}
}

// TestLiteralIsPasteable checks the shrinker's output is what it claims to be: a
// compilable Go literal. It is asserted because the whole value of shrinking is that
// somebody pastes the result into a regression test, and a literal that does not
// compile turns a five-minute fix back into a debugging session.
func TestLiteralIsPasteable(t *testing.T) {
	got := Literal(Gen(Differential, 3, 5))
	if !strings.HasPrefix(got, "[]tape.Cmd{\n") || !strings.HasSuffix(got, "}") {
		t.Fatalf("literal is not a slice literal:\n%s", got)
	}
	if strings.Count(got, "\n") != 6 { // header plus five commands
		t.Fatalf("literal has the wrong number of lines for five commands:\n%s", got)
	}
	if !strings.Contains(got, "Kind: tape.") {
		t.Fatalf("literal does not name its kinds:\n%s", got)
	}
}

// TestShrinkKeepsTheClass is the shrinker's own contract: it must refuse a reduction
// that changes the divergence class, and it must SAY it saw one.
func TestShrinkKeepsTheClass(t *testing.T) {
	cmds := Gen(Differential, 5, 40)
	pred := func(cand []Cmd) (string, bool) {
		hasA, hasB := false, false
		for _, c := range cand {
			if c.Pos == 10 {
				hasA = true
			}
			if c.Pos == 30 {
				hasB = true
			}
		}
		switch {
		case hasA:
			return "classA", true
		case hasB:
			return "classB", true
		}
		return "", false
	}
	got := Shrink(cmds, "classA", pred, 500)
	if len(got.Cmds) != 1 || got.Cmds[0].Pos != 10 {
		t.Fatalf("shrunk to %d commands, want the single command at Pos 10: %s", len(got.Cmds), Literal(got.Cmds))
	}
	if got.Drift != "classB" {
		t.Fatalf("the shrinker saw a classB candidate and did not report the drift (got %q)", got.Drift)
	}

	// And the sabotage: weakened to "still diverges", it is free to land on the
	// other bug entirely.
	weak := Shrink(cmds, "", pred, 500)
	if len(weak.Cmds) != 1 {
		t.Fatalf("the weakened shrinker did not reach a single command: %s", Literal(weak.Cmds))
	}
}

// TestEveryKindIsNamed keeps Kind.String honest, so a shrunk tape literal cannot
// print "Kind(7)" and stop being pasteable.
func TestEveryKindIsNamed(t *testing.T) {
	for k := Kind(0); k < KindCount; k++ {
		if strings.HasPrefix(k.String(), "Kind(") {
			t.Errorf("Kind %d has no name, so a shrunk tape naming it will not compile", k)
		}
	}
}

// TestPortableKeepsDrawCount is the guard ExoticDamp has, applied to Portable: the
// knob may change what a command SAYS, never how many draws produced it. Turn it off
// and every field the rule does not touch must come out identical, command for
// command. A Portable that skipped a draw instead of discarding it would shift the
// whole stream after the first exotic order, and this fails at that command.
func TestPortableKeepsDrawCount(t *testing.T) {
	plain := Bench
	plain.Portable = false
	for _, seed := range []uint64{1, 7, 0x5EED1234} {
		a, b := Gen(Bench, seed, 5000), Gen(plain, seed, 5000)
		for i := range a {
			x, y := a[i], b[i]
			x.Sell, y.Sell = false, false
			x.STP, y.STP = 0, 0
			x.Privileged, y.Privileged = false, false
			if x != y {
				t.Fatalf("seed %#x: Portable moved the stream at command %d:\n portable %+v\n    plain %+v", seed, i, a[i], b[i])
			}
		}
	}
}

// TestPortableTapeHasNoSelfCrossUser checks the property Portable exists for: no
// account ever appears on both sides, so no matcher needs self-trade prevention to
// replay the tape. It also checks the tape is still worth replaying, because a rule
// that made every order a buy would pass the first half trivially.
func TestPortableTapeHasNoSelfCrossUser(t *testing.T) {
	side := map[string]bool{}
	var buys, sells, market, postOnly, ioc, fok int
	for _, c := range Gen(Bench, 0x5EED1234, 50000) {
		if c.Kind != Submit && c.Kind != Replace {
			continue
		}
		if c.STP != 0 || c.Privileged {
			t.Fatalf("command %d carries STP %d / privileged %v on a Portable tape", c.Pos, c.STP, c.Privileged)
		}
		if s, seen := side[c.User]; seen && s != c.Sell {
			t.Fatalf("command %d: %s trades both sides", c.Pos, c.User)
		}
		side[c.User] = c.Sell
		if c.Sell {
			sells++
		} else {
			buys++
		}
		switch {
		case c.MarketOrd:
			market++
		case c.PostOnly:
			postOnly++
		}
		switch c.TIF {
		case 1:
			ioc++
		case 2:
			fok++
		}
	}
	if buys == 0 || sells == 0 || market == 0 || postOnly == 0 || ioc == 0 || fok == 0 {
		t.Fatalf("Portable tape lost part of its alphabet: buys %d sells %d market %d post-only %d IOC %d FOK %d",
			buys, sells, market, postOnly, ioc, fok)
	}
}
