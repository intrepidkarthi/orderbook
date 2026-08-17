package tape

// Delta debugging by deletion.
//
// It works at all only because a tape is deletion-closed: every subsequence of a
// valid tape is a valid tape, because a command names an earlier order by the
// POSITION of the submit that created it rather than by an index into the ids
// accumulated so far. Deleting a submit leaves later commands naming a position
// that produced no order, which is a well-formed command with a predictable
// rejection that both sides model. So deletion never rewrites the meaning of what
// remains — and "cancel something that isn't there" gets swept for free rather than
// needing its own generator branch.

// Shrunk is the outcome of a shrink.
type Shrunk struct {
	// Cmds is the reduced tape.
	Cmds []Cmd
	// Calls is how many times the predicate ran.
	Calls int
	// Drift, when non-empty, is a divergence class the shrinker saw on a candidate
	// it therefore refused. A shrink that changed the bug is information, not an
	// error, so it is reported rather than swallowed.
	Drift string
}

// Shrink repeatedly deletes contiguous chunks, keeping a deletion only while the
// reduced tape still diverges WITH THE SAME CLASS.
//
// The predicate is "same class", not "still fails" and not "fails identically at
// the same index". "Still fails" lets the shrinker wander onto a different bug and
// report a minimal tape for something that was not the failure; "identically at the
// same index" refuses almost every reduction, because deleting a command shifts
// every later index.
//
// The budget is a predicate-call count, not a wall clock. A time-budgeted shrink
// produces a different minimal tape on a loaded machine, and a failure message that
// varies with machine load is a failure message people learn to distrust.
//
// want == "" weakens the predicate to "still diverges", which is the sabotage this
// design is meant to be measured against rather than a mode anyone should use.
func Shrink(cmds []Cmd, want string, pred func([]Cmd) (string, bool), budget int) Shrunk {
	cur := append([]Cmd{}, cmds...)
	out := Shrunk{}

	chunk := len(cur) / 2
	if chunk < 1 {
		chunk = 1
	}
	// before is the length at the start of the current unit pass, so the loop can
	// tell a pass that deleted something from one that did not.
	before := len(cur)
	for chunk >= 1 {
		i := 0
		for i+chunk <= len(cur) {
			if out.Calls >= budget {
				out.Cmds = cur
				return out
			}
			cand := make([]Cmd, 0, len(cur)-chunk)
			cand = append(cand, cur[:i]...)
			cand = append(cand, cur[i+chunk:]...)
			out.Calls++
			class, diverges := pred(cand)
			switch {
			case diverges && (want == "" || class == want):
				// Keep the deletion. i stays where it is: the next chunk has
				// shifted into the window we just emptied.
				cur = cand
			case diverges:
				out.Drift = class
				i += chunk
			default:
				i += chunk
			}
		}
		if chunk == 1 {
			// The unit pass runs to a FIXPOINT rather than once.
			//
			// One pass does not give 1-minimality, because the predicate is not
			// deletion-monotone: a command tested early and kept — because at that
			// moment removing it changed the divergence class — is never retested
			// after a later deletion has made it redundant. Classic ddmin repeats the
			// unit pass until a pass deletes nothing, and so does this.
			//
			// In practice the tapes checked here were already 1-minimal after one
			// pass, so this is a robustness fix rather than a repair of an observed
			// bad reproduction. It is still worth having: the shrunk tape is pasted
			// into a regression test by hand, and every extra command in it is a
			// command a reader has to decide is irrelevant.
			if len(cur) == before {
				break
			}
			before = len(cur)
			continue
		}
		chunk /= 2
	}
	out.Cmds = cur
	return out
}
