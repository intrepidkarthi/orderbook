package wal

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

// The semantics gate: what recovery does when the records it is about to replay
// were produced by a matcher that is not this one.
//
// docs/SEMANTICS-VERSION.md §3 is the reasoning and it is load-bearing, because both
// obvious answers are wrong. WARN AND CONTINUE is today's behaviour with a log line
// on top: the venue still serves a book that never existed, and nobody reads the line
// because the venue started. ALWAYS REFUSE strands a venue whose log is fine, and a
// check that fires when nothing is wrong is a check operators learn to switch off —
// at which point it is worse than the warning, because now nobody is even reading the
// line.
//
// So the gate is on the records recovery is ABOUT TO REPLAY, not on the files it can
// see. A venue that followed the documented upgrade procedure — checkpoint, archive,
// upgrade, restart — has nothing left to apply from the old segment and starts with
// no ceremony at all. The refusal falls on exactly two populations: a venue that
// crashed across the upgrade, and a replay from an archive. Both are cases where the
// alternative is a wrong book.

// ErrSemanticsMismatch reports that recovery would have replayed records written by
// a build whose MATCHING BEHAVIOUR differs from this one's, producing a book the
// venue that wrote them never had.
//
// It is deliberately distinct from ErrCorrupt and ErrLogGap for the same reason those
// two are distinct from each other. Corruption is bytes that changed; a gap is files
// that are missing; this is bytes that are intact and files that are all present and
// a MEANING that moved. Three conditions, three sentinels, three runbook sections.
var ErrSemanticsMismatch = errors.New("wal: matching semantics mismatch")

// RecoverOptions are the deliberate deviations from a default recovery. The zero
// value is the default and is what Recover and RecoverWithReport use.
type RecoverOptions struct {
	// AcceptSemantics lists the semantics versions whose records this recovery will
	// replay IN ADDITION to this build's. Naming zero accepts records from a log
	// written before the stamp existed.
	//
	// It names versions rather than being a boolean, and that is the most important
	// detail in the design. It is about what happens in six months rather than
	// tonight: a -wal-semantics-mismatch-ok goes into a unit file during one incident
	// and stays there for the life of the deployment, so the next mismatch — a
	// different mismatch, for a different reason — is accepted silently by a flag
	// nobody remembers. A named version stops working the moment the number moves
	// again, and fails with a message naming what it accepts and what the log
	// actually carries. The decision has to be re-made because the artifact of the
	// last decision has gone stale, which is the same device internal/apicheck uses
	// when it makes you regenerate a golden and read the diff.
	//
	// It relaxes the semantics gate and NOTHING ELSE. ErrCorrupt, ErrLogGap, the
	// floor check and every structural validation are untouched: an operator reaching
	// for this during an incident must not acquire a second, unrelated permission by
	// accident.
	AcceptSemantics []int

	// AcceptIcebergsWithoutReserve is the number of KindIceberg records this recovery
	// may replay that cannot state their hidden reserve — records written before the
	// total was journalled. It must equal exactly the number found: naming 12 when
	// the log holds 11 or 13 refuses and says so.
	//
	// It is a COUNT rather than a boolean for the reason AcceptSemantics is a list:
	// a boolean goes into a unit file during one incident and stays for the life of
	// the deployment, so the next occurrence — which this build cannot produce, and
	// which therefore means a foreign writer or a downgrade — is accepted silently by
	// a flag nobody remembers. A count goes stale the moment the log changes, and it
	// makes the operator state a quantity they have read.
	//
	// A sequence LIST was the first design and was rejected on ergonomics: an archive
	// replay with 2,000 such records would need 2,000 numbers pasted at 3am, and an
	// operator who cannot use the safe form uses the unsafe one. What the count gives
	// up is stated plainly — it asserts HOW MANY, not WHICH, so a different set of
	// the same size would also pass. The class of damage is identical across the set,
	// which is why that is acceptable here and would not be for ErrLogGap.
	//
	// It relaxes the iceberg gate and NOTHING ELSE — not ErrCorrupt, not ErrLogGap,
	// not the floor check, not the semantics gate. See
	// docs/ICEBERG-DURABILITY.md §4.3 and iceberg_reserve.go.
	AcceptIcebergsWithoutReserve int
}

// accepts reports whether records declaring sem may be replayed by this build.
//
// Semantics 0 means UNKNOWN, not "version zero", so it is never equal to anything —
// including this build's version, and including another file that also declares
// nothing. The only thing that accepts it is naming it. docs/SEMANTICS-VERSION.md §4
// works through the three available answers and why the honest one is the least bad:
// "unknown means compatible" is affirmatively FALSE today, since the three matching
// changes this stamp ships with are exactly what a pre-stamp log did not have, and a
// wrong assumption baked into a durability check is permanent where a one-time
// operational cost is recoverable.
func (o RecoverOptions) accepts(sem int) bool {
	if sem != 0 && sem == matching.SemanticsVersion {
		return true
	}
	for _, v := range o.AcceptSemantics {
		if v == sem {
			return true
		}
	}
	return false
}

// semSpan is one segment whose declared semantics is not this build's, together with
// what of it recovery would have replayed. Everything here is needed by the refusal
// message, and the message is a design requirement rather than polish: an operator
// meeting it at 3am needs both numbers, the file, the sequences and both routes
// forward.
type semSpan struct {
	name  string
	sem   int
	first int64
	last  int64
	// replay is how many of [first, last] sit past the snapshot boundary.
	replay int64
}

// gateSealedSegments refuses before a single record is read, from the directory and
// the headers alone.
//
// A sealed segment Sᵢ spans [baseᵢ, baseᵢ₊₁ − 1] and both numbers come from the
// 16-digit filenames, so "does this segment contribute a record past the snapshot" is
// answerable in the same directory read that already does the gap, overlap and floor
// checks. A mismatch is therefore refused in milliseconds instead of after reading
// gigabytes.
//
// The NEWEST segment is not gated here, and that is not an oversight — see
// gateNewestSegment.
func gateSealedSegments(set *segmentSet, after int64, opts RecoverOptions) error {
	var bad []semSpan
	for i := 0; i+1 < len(set.segs); i++ {
		seg := set.segs[i]
		last := set.segs[i+1].base - 1
		if last <= after || opts.accepts(seg.semantics) {
			continue
		}
		bad = append(bad, semSpan{
			name: seg.name, sem: seg.semantics, first: seg.base, last: last,
			replay: last - max64(after, seg.base-1),
		})
	}
	return semanticsRefusal(bad, after)
}

// gateNewestSegment refuses on the active segment, after the walk.
//
// The newest segment cannot be gated from the directory, because its span has no
// upper bound there — its last sequence is only known once its records have been
// walked. Gating it optimistically ("the newest segment always contributes") would
// refuse in exactly the SnapshotAhead case, which is reachable after an ordinary
// power loss: a checkpoint does not sync the log first, so a snapshot can legitimately
// be ahead of the log's last record (docs/BOUNDED-RECOVERY.md §4.1). That is a false
// refusal on a benign crash, which is the noise the whole design exists to avoid. So
// it waits for walk.lastSeq, which the walk already computes, and both stages still
// run before RestoreEngine and before a single command is applied.
func gateNewestSegment(set *segmentSet, after, lastSeq int64, opts RecoverOptions) error {
	seg, ok := set.newest()
	if !ok || lastSeq <= after || opts.accepts(seg.semantics) {
		return nil
	}
	return semanticsRefusal([]semSpan{{
		name: seg.name, sem: seg.semantics, first: seg.base, last: lastSeq,
		replay: lastSeq - max64(after, seg.base-1),
	}}, after)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// semanticsRefusal renders the refusal, or nil when there is nothing to refuse.
//
// The message names both numbers, the file, the sequences, the count, and both routes
// forward with the safe one first. Every one of those is there because the alternative
// is an operator guessing during an incident: without the count they cannot tell a
// whole segment from one record, without the snapshot's position they cannot tell
// whether a checkpoint would clear it, and without the versions they cannot look up
// what changed.
func semanticsRefusal(bad []semSpan, after int64) error {
	if len(bad) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "this build matches at semantics %d; records this recovery would replay were written at %s.\n\n",
		matching.SemanticsVersion, versionPhrase(bad))
	for _, s := range bad {
		fmt.Fprintf(&b, "  segment  %s   %s\n", s.name, semanticsName(s.sem))
		fmt.Fprintf(&b, "           sequences %s..%s, of which %s would be replayed\n",
			commas(s.first), commas(s.last), commas(s.replay))
	}
	if after > 0 {
		fmt.Fprintf(&b, "           (the snapshot covers through %s)\n", commas(after))
	} else {
		b.WriteString("           (there is no snapshot, so the whole log would be replayed)\n")
	}
	fmt.Fprintf(&b, "\nReplaying them under this build produces a book the venue that wrote them never\n"+
		"had. What changed is in CHANGELOG.md; docs/SEMANTICS-VERSION.md §1.2 has the\n"+
		"registry of what each semantics version means.\n\n"+
		"Two ways forward, safest first:\n"+
		"  1. Start the PREVIOUS build once, let it checkpoint, stop it cleanly, then\n"+
		"     upgrade. Recovery then starts from a snapshot that build agreed with and\n"+
		"     has nothing left to replay.\n"+
		"  2. Accept the replay deliberately:  -wal-accept-semantics %s\n\n"+
		"See docs/RUNBOOKS.md \"Upgrading across a semantics change\".",
		acceptList(bad))
	return fmt.Errorf("%w: %s", ErrSemanticsMismatch, b.String())
}

// versionPhrase names the versions the refused records carry, in the sentence that
// opens the message.
func versionPhrase(bad []semSpan) string {
	vs := distinctSemantics(bad)
	names := make([]string, 0, len(vs))
	for _, v := range vs {
		names = append(names, semanticsName(v))
	}
	return strings.Join(names, " and ")
}

// semanticsName renders a declared version for a human. Zero is the one value that
// must never read as a number, because it is not one: it means the file predates the
// stamp and says nothing at all about which matcher wrote it.
func semanticsName(sem int) string {
	if sem == 0 {
		return "no declared semantics (a log written before the stamp existed)"
	}
	return fmt.Sprintf("semantics %d", sem)
}

// acceptList is the argument an operator would pass to accept exactly what was
// refused and nothing more.
func acceptList(bad []semSpan) string {
	vs := distinctSemantics(bad)
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, strconv.Itoa(v))
	}
	return strings.Join(out, ",")
}

func distinctSemantics(bad []semSpan) []int {
	seen := map[int]bool{}
	var out []int
	for _, s := range bad {
		if !seen[s.sem] {
			seen[s.sem] = true
			out = append(out, s.sem)
		}
	}
	sort.Ints(out)
	return out
}

// commas groups a sequence number for reading. An operator comparing 610422 against
// 6104220 at 3am should not have to count digits.
func commas(v int64) string {
	s := strconv.FormatInt(v, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// logSemantics is the distinct set of semantics the segments on disk declare,
// ascending. A set legitimately spans an upgrade — segment 400 can declare a
// different version from segment 1 — so this is a report of a fact rather than a
// condition, and it is exactly the fact recovery needed.
func logSemantics(set *segmentSet) []int {
	seen := map[int]bool{}
	var out []int
	for _, seg := range set.segs {
		if !seen[seg.semantics] {
			seen[seg.semantics] = true
			out = append(out, seg.semantics)
		}
	}
	sort.Ints(out)
	return out
}

// replaySemanticsDiffer reports whether any segment that contributes a record past
// after declares something other than this build's semantics. It is what
// RecoverReport.SemanticsAccepted is set from, and it is only ever consulted once the
// gate has already let the recovery through — so a true here means an override was
// used, which is the number to watch in the field.
func replaySemanticsDiffer(set *segmentSet, after, lastSeq int64) bool {
	for i, seg := range set.segs {
		last := lastSeq
		if i+1 < len(set.segs) {
			last = set.segs[i+1].base - 1
		}
		if last > after && seg.semantics != matching.SemanticsVersion {
			return true
		}
	}
	return false
}
