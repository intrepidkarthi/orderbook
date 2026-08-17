package matching

// SemanticsVersion identifies an EQUIVALENCE CLASS OF BUILDS: two builds share a
// semantics version if and only if, for every command sequence and every engine
// configuration, they produce the same trades, the same events, the same verdicts
// and the same book.
//
// It is neither of the two version numbers this repository already has, and
// confusing it with either has a named failure mode:
//
//   - A RELEASE version ("which build is this") changes every release, so using one
//     as the stamp refuses logs that would replay identically, forever. The response
//     to a check that cries wolf is a permanent override in the unit file, and then
//     the real mismatch three releases later is accepted silently.
//   - A FORMAT version ("can this reader parse these bytes") is blind here. The
//     three behaviour changes this constant was introduced for are byte-identical on
//     disk: the same Entry, the same framing, the same CRC.
//
// It is written into every log segment's header, because a log is a PROGRAM that
// recovery re-executes, and into every snapshot, because a snapshot is a STATE. The
// segment header is the gate: wal.Recover refuses when it is about to replay records
// from a segment whose declared semantics is not this build's. The snapshot's copy is
// a report and never a gate — restoring a book an older build actually had is the
// upgrade procedure, not an error.
//
// Zero means UNKNOWN — a segment or snapshot written before the stamp existed. It is
// not "1", not "compatible" and not equal to any version including itself; see
// docs/SEMANTICS-VERSION.md §4 for why the honest answer beats the convenient one.
//
// The rule for changing it is not a matter of judgement, because "did matching
// behaviour change?" is a question two engineers answer differently on a Friday:
//
//	The version changes if and only if internal/semcheck/testdata/semantics.txt
//	changes.
//
// That golden is a rendering of what this engine does with a fixed corpus of
// commands, and it is enforced in both directions. A body diff with an unchanged
// version fails; SEMCHECK_UPDATE=1 refuses to write the golden unless this constant
// is strictly greater than the one recorded in it; and a bump with an identical body
// fails too, because a version that fires when nothing changed is the useless version
// the first paragraph rejects.
//
// The registry of what each value means is docs/SEMANTICS-VERSION.md §1.2:
//
//	0  every build up to and including v0.25.0 — unknown, and NOT the semantics of
//	   any build
//	1  this release onward — includes the three matching changes in CHANGELOG.md's
//	   Unreleased/Changed section (a rejected fill-or-kill no longer moves
//	   LastTradePrice; a rejected command's batch carries the events describing state
//	   that was not rolled back; pro-rata no longer skips a taker's own liquidity)
//
// It is an integer and only ever increases, so a refusal can say which direction the
// mismatch runs and an operator can look up a range in the changelog.
const SemanticsVersion = 1
