package wal

import (
	"errors"
	"fmt"
	"strings"
)

// The iceberg-reserve gate: what recovery does when a record it is about to replay
// cannot state the order it records.
//
// docs/ICEBERG-DURABILITY.md §4 is the reasoning. A KindIceberg record written before
// this build carries Order.Quantity SHRUNK to the display size and no total anywhere,
// so its hidden reserve is not merely missing from the recovered book — it was never
// on the disk. The record is intact, verified, contiguous, correctly framed, and
// INSUFFICIENT, which is a fourth condition alongside corruption, a gap and a
// semantics mismatch.
//
// Recovering it silently is today's behaviour and it is not a bounded error. The
// measured tape: an iceberg of 9 shown 3, a buy of 9, a sell of 5. A venue that lost
// the reserve prints two trades instead of three, PRINTS FIVE LOTS AT 100 THAT THE
// VENUE NEVER PRINTED, and ends holding the opposite side of the book. The divergence
// is not confined to the client's order, so "start with a warning and cancel the
// affected order" is not an available remediation.
//
// So the gate is on the records recovery is ABOUT TO REPLAY, exactly as the semantics
// gate is (semantics.go), and it inherits that design's property: a venue whose
// snapshot covers those records starts with no ceremony at all. It is per RECORD
// rather than per segment because the fact is per record — which is the mirror of the
// argument that made the semantics stamp per segment.

// ErrIcebergReserveUnknown reports that recovery would have replayed a KindIceberg
// record that cannot state its total size, rebuilding a client's iceberg as an
// ordinary order of its display size with the reserve silently gone.
//
// A fourth sentinel beside ErrCorrupt, ErrLogGap and ErrSemanticsMismatch, for the
// same reason those three are distinct from each other. Corruption is bytes that
// changed; a gap is files that are missing; a semantics mismatch is a meaning that
// moved; this is a record that never carried what it needed, written by a build that
// did not know it had to. Four conditions, four sentinels, four runbook sections.
var ErrIcebergReserveUnknown = errors.New("wal: iceberg reserve unknown")

// statesItsTotal reports whether a KindIceberg record says how big the order was.
//
// Presence of TotalQty is the whole discriminator, and it works because a valid
// iceberg's total is always positive so omitempty never drops it. The alternative
// designs could not detect their own absence: a `reserve` field is omitempty and a
// genuine zero reserve is legal, so a pre-fix record with a LOST reserve and a
// post-fix record with NO reserve would be byte-identical. Putting the total in
// Order.Quantity alone has the same ambiguity — `quantity 3, display 3` is what a
// pre-fix nine-lot iceberg wrote and what a post-fix three-lot iceberg writes.
//
// Rule 5: a record carrying BOTH, where they disagree, is treated as carrying
// neither. This package cannot write one, so such a record was hand-edited, forged,
// or written by something else claiming to be this format; a reader that picked one
// of the two numbers would be guessing which of two contradictory statements about
// the same quantity to believe. Treating it as unstated routes it into the gate,
// which is the fail-safe direction. The RECONSTRUCTION is unchanged either way
// (restoreEntry's Rule 4 expression), so an operator who accepts the loss gets one
// documented answer rather than new arithmetic invented for a forged record.
func statesItsTotal(e Entry) bool {
	if e.TotalQty <= 0 {
		return false
	}
	if e.Order != nil && e.Order.Quantity != e.TotalQty {
		return false
	}
	return true
}

// gateIcebergReserves refuses if and only if recovery is about to APPLY a KindIceberg
// record that cannot state its total.
//
// lossy is the records already selected by the caller's replay-set filter — the same
// single pass that computes RecoverReport.Applied — so this costs one predicate on a
// slice that is being iterated anyway, it happens before a single command is applied,
// and it needs no second read of anything.
//
// The count is over the REPLAY SET and never over the file. A record behind the
// snapshot boundary in a checksummed log is never decoded at all (walkSegment's parse
// predicate), so counting it would mean undoing the covered-prefix skip, which is the
// entire saving docs/BOUNDED-RECOVERY.md exists for. Two consequences, both
// deliberate: a report can read IcebergsWithoutReserve: 0 for a log holding a hundred
// of them behind its snapshot — correct, since none of them affects the book — and
// the count is framing-independent, where a count over the file would report
// different numbers for the same commands depending on whether they were written to a
// v1 segment (which parses every record) or a headered one (which does not).
func gateIcebergReserves(lossy []Entry, after int64, opts RecoverOptions) (accepted bool, err error) {
	if len(lossy) == 0 {
		return false, nil
	}
	if opts.AcceptIcebergsWithoutReserve == len(lossy) {
		return true, nil
	}
	return false, icebergReserveRefusal(lossy, opts.AcceptIcebergsWithoutReserve, after)
}

// icebergReserveRefusal renders the refusal.
//
// Ergonomics is a design requirement here rather than polish, and this refusal can be
// considerably more useful than the semantics one because the damage is localised to
// specific orders and those orders are named in the records being refused. So it
// names them, and it says what replaying them would actually do.
func icebergReserveRefusal(lossy []Entry, given int, after int64) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s this recovery would replay %s written before the journal recorded an\n"+
		"iceberg's total size, so their hidden reserve cannot be reconstructed.\n\n",
		len(lossy), plural(len(lossy), "record", "records"), plural(len(lossy), "was", "were"))
	for _, e := range lossy {
		fmt.Fprintf(&b, "  sequence %-12s order %-20s displays %d, total unknown\n",
			commas(e.Seq), orderName(e), e.DisplayQty)
	}
	if after > 0 {
		fmt.Fprintf(&b, "  (the snapshot covers through %s; these are past it)\n", commas(after))
	} else {
		b.WriteString("  (there is no snapshot, so the whole log would be replayed)\n")
	}
	if given > 0 {
		fmt.Fprintf(&b, "\n-wal-accept-iceberg-loss %d does not match: this recovery would replay %d such\n"+
			"%s. The count must be exact, so that accepting a loss is a decision about a log\n"+
			"somebody has read rather than a flag that outlives it.\n",
			given, len(lossy), plural(len(lossy), "record", "records"))
	}
	b.WriteString("\nReplaying them rebuilds each as an ordinary order of its DISPLAY size. The\n" +
		"missing quantity is not merely hidden — it is gone, and every command after a\n" +
		"trade against one of these replays against a different book.\n\n" +
		"Three ways forward, safest first:\n")
	fmt.Fprintf(&b, "  1. Recover from a snapshot that covers sequence %s or later. A snapshot\n"+
		"     carries the reserve, so these records are then behind the boundary and this\n"+
		"     refusal does not apply.\n", commas(lastSeqOf(lossy)))
	fmt.Fprintf(&b, "  2. Accept the loss deliberately, then CANCEL the %s above and tell\n"+
		"     %s owners to re-enter:  -wal-accept-iceberg-loss %d\n",
		plural(len(lossy), "order", "orders"), plural(len(lossy), "its", "their"), len(lossy))
	b.WriteString("  3. If you capture inbound order entry, the total is in the client's original\n" +
		"     EnterIceberg message; the journal is the only place it was lost.\n\n" +
		"Starting the PREVIOUS build does not help: it wrote these records and reads them\n" +
		"the same way.\n\n" +
		"See docs/RUNBOOKS.md \"An iceberg whose reserve was never journalled\".")
	return fmt.Errorf("%w: %s", ErrIcebergReserveUnknown, b.String())
}

// orderName identifies the order a refused record owns, in the words an operator will
// use to tell its client. The client order id is the client's own name for it, so it
// leads when it is there; without one the venue's user id is all the record has.
func orderName(e Entry) string {
	if e.Order == nil {
		return "(no order in the record)"
	}
	if e.Order.ClientOrderID != "" {
		return e.Order.UserID + "/" + e.Order.ClientOrderID
	}
	return e.Order.UserID
}

func lastSeqOf(lossy []Entry) int64 {
	var last int64
	for _, e := range lossy {
		if e.Seq > last {
			last = e.Seq
		}
	}
	return last
}

// plural picks a word for n. Five one-line helpers were the alternative, and a
// refusal message is not the place to be clever about grammar.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
