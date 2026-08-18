# Iceberg Durability — A Record That Cannot State The Order It Records

Status: **implemented** — specified first, as this repository does it, and §12 is what
building it measured. §1 is an
audit that had to run before anything could be decided, and its result is what bounds
the rest: **one order type has this defect, and the mutation that causes it leaks into
two consumers besides the journal.** §3, §4 and §5 each end in a decision; §9 is how a
reviewer checks it was carried out; §10 is the list of sabotages that must be red before
it counts; §11 is how it can still be wrong. Every number below was measured against
`main` with a probe built outside the repository and discarded; §12 records what
building it changed, in the shape of
[`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §9, including the three rows this document
got wrong ·
Author: Karthikeyan NG · Last updated: 2026-08-18

> **The measurement that decides this.** A client posts an iceberg: 9 lots, 3
> displayed. A stranger buys 9. Then someone sells 5. Nothing is rejected, no
> fill-or-kill is involved, no snapshot exists — the venue crashed and is recovering
> from its journal alone, which is the only reason recovery exists.
>
> ```
> live venue          3 prints, 9 lots     ends holding  SELL 5 @100
> log-only recovery   2 prints, 8 lots     ends holding  BUY  9 @100, 1 left
> ```
>
> The reserve is not merely missing. The recovered venue loses the two refilled slices
> the venue really printed (six lots), **prints five lots at 100 that the venue never
> printed**, and ends holding the opposite side of the book. One record that could not
> state its own order, and every command after it replays against a different venue.

Companion documents:
- [`PINNED-DEFECTS.md`](PINNED-DEFECTS.md) §13.7 — where this was measured and
  deliberately left, and §9's bullet declining it: *"a journal-format question in
  another package, with its own compatibility argument to make."* This document is that
  argument. `pkg/wal/iceberg_reserve_pin_test.go` carries the sentence this slice comes
  to delete.
- [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §4 and §5.2 — the covered-prefix skip,
  which decides what §4.5 below is *able* to count, and the precedent for a check whose
  strictness travels with the snapshot boundary.
- [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §2.1, §3 and §4 — the definition §5
  applies, the gate §4 copies, and the "unknown means unknown" argument §4 reruns for a
  record instead of a segment.
- [`LOG-ROTATION.md`](LOG-ROTATION.md) §2 and §5 — the formats this must not disturb and
  the archive lifetime that decides how self-describing a record has to be.
- [`COMPATIBILITY.md`](COMPATIBILITY.md) §"The rule" — adding a struct field is additive
  and lands in a minor release; `internal/apicheck` still makes a human read the diff.
- [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §8 — `entryKindCount` and the
  guard that enumerates it. §6 below builds the third guard in that family.
- [`TESTING.md`](TESTING.md) §"The rule" — a test does not count until it has been run
  against code broken the way it claims to detect. §10 is this slice's list.

---

## 1. The audit: is the iceberg the only one?

This ran first, before any of §3–§6 was decided, because "fix the iceberg" and "the
journal cannot record a wrapper order" are different slices and only the audit says
which one this is.

### 1.1 What was checked, and how

Two questions per order type, and the second is the one that matters:

1. **Does the constructor mutate the `*types.Order` it is handed?** Deep-copy the order,
   construct the wrapper, compare. A constructor that mutates is a constructor whose
   effect has already happened by the time anything can be journalled, because every
   caller — `cmd/obgw/server.go:1547`, `internal/semcheck`, every test — constructs the
   wrapper and *then* hands it to `Runner`, which logs it in `logCommand`
   (`engine_loop.go:370`) before applying it. Write-ahead ordering cannot help: the
   damage precedes the write-ahead.
2. **Does a log-only recovery rebuild the same venue?** Append the wrapper, process it
   live, `Close`, `wal.Recover(cfg, "", path)`, and compare
   `EngineSnapshot.Digest()` — which carries the reserve, the refill count and the
   trailing ratchet — against the live engine's.

Question 2 is the real test. Question 1 only explains a failure; a wrapper could round
trip badly without any mutation (by deriving state at construction that the record does
not carry), and a wrapper could mutate harmlessly (if the record carries enough to undo
it). So both were run, and neither was inferred from reading the code.

### 1.2 Results

| Order type | Constructor mutates the order | Round trip through a log-only recovery |
|---|---|---|
| Stop (market) | **no** | **exact** — resting, and again after the stop fires mid-tape |
| Stop-limit | **no** | **exact** |
| OCO | **no**, on either leg | **exact** — both legs are logged (`StopOrder` + `StopPrice`) |
| Pegged | **no** | **exact** |
| Trailing stop | **no** | **exact** — including a ratchet that had already moved |
| **Iceberg** | **YES** — `quantity 9 → 3`, `remaining 9 → 3` | **DIVERGES** — `Hidden 6` live, `Hidden 0` recovered |

The record each one produces, so the round trip is written out rather than asserted:

```
KindStop      order{qty 9}                       + stop_price 90
KindOCO       order{qty 9} + stop_order{qty 9}   + stop_price 90
KindPegged    order{qty 4, price 1}              + peg_ref BID + peg_offset 1
KindTrailing  order{qty 4}                       + trail 5
KindIceberg   order{qty 3}                       + display_qty 3     <- the client sent 9
```

The two round trips that carry the most weight, because in both the wrapper's state had
already **moved** before the log was closed — a resting wrapper proves much less:

```
stop, fired mid-tape        live: triggered=true, 2 prints, 0 stops left in the snapshot
                       recovered: identical, digest 568f7add5a96 == 568f7add5a96

trailing, ratcheted 3x      live: {Extreme:100 StopPrice:95 Initialized:true}
                       recovered: {Extreme:100 StopPrice:95 Initialized:true}
```

Neither `triggered` nor the ratchet is in any record. Both come back exactly, because
the log carries the trades that produced them and replay re-derives them from the same
tape. That is the property the iceberg's reserve does not have: **nothing in the tape
re-derives a quantity that was never displayed.**

Four of the five wrappers are *a base order plus one or two scalars*, exactly as
`Entry`'s comment claims (`wal.go:151-154`), and the scalars are all in the record. The
iceberg is the one wrapper that is **a base order plus a scalar plus a piece of the
order itself**, and that third thing is the one the record does not have.

Three properties that look like the same defect and are not, checked individually
because each would have been a second finding:

- **`IcebergOrder.JitterBps` is not logged, and does not need to be.** `ProcessIceberg`
  sets it from `Config.IcebergPeakJitter` on every entry, live and replayed alike
  (`engine.go:1356`). It is configuration, not command state. A venue that changes the
  config between runs replays differently — which is
  [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §6's named and much larger hole, not
  this one.
- **`IcebergOrder.refills` is not logged, and does not need to be.** It is derived by
  replaying the trades that caused the refills. The snapshot carries it because a
  snapshot has no trades to replay from; a log does.
- **`TrailingStop`'s ratchet is not logged, and does not need to be**, for the same
  reason, and this one was the most likely second finding: `TrailingState` exists
  precisely because the extreme cannot be reconstructed without the price history. A log
  *is* the price history. Measured with a ratchet that had already moved three times:
  exact.

**So the answer is one: the iceberg.** The audit is not a formality that confirmed a
guess — it is what makes the rest of this document a record-format change to one kind
rather than a redesign of how wrappers are journalled.

### 1.3 What the audit found instead, and it is not in the journal

The constructor mutation has three consumers, and this slice fixes one of them. The
other two were measured while establishing that the constructor is the cause:

- **The engine's ingress size caps see the display slice.** With
  `Config.MaxOrderQty = 5`: a plain sell of 9 is `REJECTED — order quantity exceeds the
  configured maximum`; **the same nine lots posted as an iceberg shown 3 is accepted**,
  with 6 in reserve. The cap is evaded by exactly the hidden quantity.
- **And they misjudge it in the other direction.** With `Config.MinOrderQty = 5`, an
  iceberg of **90** shown 3 is `REJECTED — order quantity is below the configured
  minimum`, for being 18× under a floor it is 18× over.
- **The replication follower rebuilds icebergs from the same records**
  (`examples/replication/follower.go:106` calls `RestoreAfter` on each shipped entry),
  so a follower's book has been diverging from its primary on the first iceberg, from
  the first record, with `Digest()` reporting it and **nothing exercising it**: there is
  no iceberg anywhere in `examples/`. This one the fix repairs for free, because the
  follower consumes the record this slice repairs.

The first two are ingress policy in `pkg/matching` and are **out of scope** (§8), for
the same reason [`PINNED-DEFECTS.md`](PINNED-DEFECTS.md) §9 gave for declining this one:
"should an iceberg's cap be its total or its slice" is a venue-policy argument with its
own answer to justify, and real venues differ. They are **pinned** here rather than
merely mentioned, because a measured finding left unpinned is how it gets found a third
time.

### 1.4 The scope decision, stated rather than left to omission

This slice fixes the **iceberg record only**. It does not generalise the fix to the
other four wrappers, because the audit proved there is nothing to generalise, and a
"wrapper state" mechanism built for one member is a mechanism whose second member will
not fit it. What it *does* generalise is the **detection** (§6): the round trip above
becomes a standing, enumerated test, so the sixth wrapper is checked by machine on the
day its constant is written.

## 2. The defect, once

`types.NewIcebergOrder` (`pkg/types/iceberg.go:34-41`) takes an order whose `Quantity`
is the total, computes `hidden = Quantity - displayQty`, **shrinks `Quantity` and
`RemainingQty` to the display size**, and puts the remainder on the wrapper, which is
not an `Order` and is not what `Entry` carries.

`Writer.AppendIceberg` (`wal.go:1120`) then logs `Entry{Kind: KindIceberg, Order:
ib.Order, DisplayQty: ib.DisplayQty}`. `ib.Order.Quantity` is already 3. `ib.Hidden` is
never written at all. `restoreEntry` rebuilds with
`types.NewIcebergOrder(e.Order.Fresh(), e.DisplayQty)` → `hidden = 3 - 3 = 0`.

The doc comment above `AppendIceberg` says *"Order.Quantity is the total and DisplayQty
the slice"*. It has never been true. It is the precondition the record needs, written as
though it already held — which is why nobody reading either half found it: the writer
reads correct, the reader reads correct, and the sentence between them is the bug.

A recovery **with** a snapshot is unaffected: `EngineSnapshot` carries `Hidden`,
`Refills` and `JitterBps` per iceberg (`matching.IcebergEntry`). So the defect is scoped
to the log-only path — which is the path a venue takes when its snapshot is missing,
refused as corrupt, or older than the retention floor. It is scoped to the incidents
recovery exists for.

**What it costs is not one order.** Measured, on the tape in this document's opening: a
recovered venue that has lost a reserve prints trades the venue never printed, because
the taker that should have been filled by the reserve rests instead and is hit by the
next order. The divergence compounds for the rest of the file, in exactly the shape
[`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §1 describes for its third change. This is
why §4 refuses rather than warns: "the client's iceberg came back small" is a bounded
error a venue could announce and remediate. "Every command after it replayed against a
different book" is not.

## 3. Decision 1 — what the record carries

### 3.1 The two candidates, and a third that nearly won

**(a) The record states the total, in the order.** `AppendIceberg` logs a copy of the
order whose `Quantity` and `RemainingQty` are the total; the reconstruction is unchanged
and already correct. Zero new fields.

**(b) The record states the reserve, in a new field.** `Entry.Reserve int64`, and the
restore sets `ib.Hidden` after construction (`Hidden` is exported, so this needs no new
API in `pkg/types`).

**(a) is the fix and (b) cannot detect its own absence.** Both reconstruct correctly
going forward, so the choice is decided entirely by the *old* records, and that is §4's
question arriving early: a build must be able to tell a record that could not state its
reserve from one that states a reserve of zero. Under (b), `reserve` is `omitempty` and
a genuine zero reserve is legal — an iceberg whose display equals its total is
unusual and permitted — so a post-fix record with no reserve and a pre-fix record with a
lost reserve are **byte-identical**. The detection §4 needs would not exist.

Under (a) the ambiguity is the same shape: `quantity 3, display 3` is what a pre-fix
9-lot iceberg wrote *and* what a post-fix 3-lot iceberg writes. So (a) alone does not
carry the detection either.

**The third candidate, which nearly won: `Reserve *int64`.** A nil pointer is omitted, a
pointer to zero is written as `"reserve":0`, so a post-fix record *always* carries the
key and a pre-fix record *never* does — exact detection with no redundancy. It was
rejected on two grounds. It leaves the record still saying `quantity 3` for an order the
client sent as 9, which is the statement that caused this defect and the statement an
operator reads at 3am. And it puts a pointer field on the struct every record in the log
decodes into — 216 million of them a day at 2,500 messages/s — for a value that is a
plain `int64` in every other field of that struct.

### 3.2 The rules

*Rule 1 — `AppendIceberg` records the order as the CLIENT SENT IT: `Quantity` and
`RemainingQty` are `ib.TotalRemaining()`, `FilledQty` is 0.*
*Reason:* a write-ahead log records commands, and the command was "iceberg, 9 lots, show
3". What the log has been recording is a post-constructor artifact of how this
implementation represents that command. Fixing the record to say what was asked for is
the repair; anything else is carrying the artifact and annotating it.
`TotalRemaining()` (`iceberg.go:85`) is `Hidden + Order.RemainingQty`, which at submit
time — the only time the `Runner` logs one — is exactly the client's total, and which
for a hypothetical re-log of a partly worked iceberg is the right thing to rebuild
rather than the original total. No new method, no new exported API in `pkg/types`.

*Rule 2 — it logs a COPY. The live order is not touched.*
*Reason:* the engine is about to match against that exact pointer. A writer that
"restores" the quantity on the live order would hand the book a 9-lot order showing 9,
which is the defect this document exists to remove, inverted and worse. This is the
one line of the change with a way to be catastrophically wrong, so it is a rule rather
than an implementation detail.

*Rule 3 — the record also carries `Entry.TotalQty`, whose only job is to be ABSENT in
records written before this change.*

```go
// TotalQty is a KindIceberg's total size — the quantity the client submitted, of
// which DisplayQty is shown at a time. It is deliberately the same number as
// Order.Quantity, and it is not redundant: a record written before this field
// existed carries Order.Quantity SHRUNK to the display size, and no reader can
// tell that from a record whose total genuinely equals its display. Presence is
// the discriminator, and a valid iceberg's total is always positive, so omitempty
// never drops it. See docs/ICEBERG-DURABILITY.md §3.
TotalQty int64 `json:"total_qty,omitempty"` // KindIceberg
```

*Reason:* it buys §4, and §4 is the whole difference between this slice and one that
silently starts producing better records. Without it a pre-fix log is undetectable and
the only available answers are "recover silently" and "refuse every iceberg record ever
written", both of which §4.1 refuses.

*Rule 4 — the reconstruction is: use `TotalQty` when it is there; otherwise use
`Order.Quantity`, which is what the code does today.*

```go
total := e.TotalQty
if total == 0 {
    total = e.Order.Quantity // pre-fix record, or a hand-built one
}
o := e.Order.Fresh()
o.Quantity, o.RemainingQty = total, total
if ib, err := types.NewIcebergOrder(o, e.DisplayQty); err == nil {
    eng.ProcessIceberg(ib)
}
```

*Reason:* it makes the reader's change one line, it defines the behaviour on the
override path of §4, and it keeps every hand-built record working. That last one is
concrete, not hypothetical: `entryKindSamples` in `entry_kind_test.go:48` writes
`{Order: order(BUY, 100, 10), DisplayQty: 2}` — a record with the total in `Quantity`
and no `TotalQty` at all, written that way because that is what any reader assumes the
field means. Under Rule 4 it still restores to a reserve of 8. **No existing assertion
is weakened to achieve that**; the fallback is today's expression, unchanged.

*Rule 5 — a record carrying both, where they disagree, is treated as a record carrying
neither.*
*Reason:* this package cannot write one, so such a record was hand-edited, forged, or
written by something else claiming to be this format. A reader that picked one of the
two numbers would be guessing which of two contradictory statements about the same
quantity to believe. Treating it as unstated routes it into §4's gate, which refuses it
unless an operator says otherwise — the fail-safe direction, and no new arithmetic.

### 3.3 What it costs in format terms

- **Every existing record is byte-identical.** `omitempty` on a zero `int64` emits
  nothing, so no record of any other kind grows by a byte, no golden moves, and no
  archived segment is reinterpreted. This is the same argument `Entry.Phase` made
  (`wal.go:190`) and it is checked the same way.
- **A `KindIceberg` record grows by 14 bytes**: measured, 311 → 325 for the record in
  §2. Icebergs are a small fraction of any venue's flow, and the comparison that matters
  is [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) Rule 3's rejected per-record stamp —
  eight bytes on *every* record to carry a constant. This is fourteen bytes on one kind
  to carry that record's own missing datum.
- **No framing change.** Not `Magic`, not `SegMagic`, not `SegMagicV3`, not
  `SegHeaderBytes`, not `MaxRecordBytes`, not the CRC, not the snapshot. Every log the
  current build reads — v1 headerless, `OBWAL\x01`, `OBWAL\x02` and `OBWAL\x03` sets,
  snapshots with and without their magic — parses exactly as before, record for record
  and error for error. §4 changes what recovery *does* with one kind of record; nothing
  changes what any reader can *parse*.
- **A downgrade improves, in the case where a downgrade is possible at all.** An older
  build reading a post-fix record computes `hidden = 9 - 3 = 6` and gets it right — the
  field it does not know about is dropped by `encoding/json` and the number it does know
  about is now correct. That only reaches an old build for a **never-rotated single-file
  log**, which is what `Open` still creates on an empty path (`OBWAL\x01`,
  [`LOG-ROTATION.md`](LOG-ROTATION.md) §7); a segment set written by this build is
  `OBWAL\x03`, which an old build refuses at the header. It is a real property in a
  bounded case, and candidate (b) would have lost it, so it is recorded with its bound
  rather than claimed flatly.
- **Exported surface.** `Entry.TotalQty`, `RecoverOptions.AcceptIcebergsWithoutReserve`,
  two `RecoverReport` fields and `ErrIcebergReserveUnknown` are all additions. Additive
  under [`COMPATIBILITY.md`](COMPATIBILITY.md), minor release, and `surface.txt` is
  regenerated in the same commit with a human reading the diff.

### 3.4 What a person reading a segment at 3am sees

Before (a nine-lot iceberg, shown three):

```json
{"seq":41,"kind":7,"order":{"quantity":3,"remaining_qty":3,...},"display_qty":3}
```

After:

```json
{"seq":41,"kind":7,"order":{"quantity":9,"remaining_qty":9,...},"display_qty":3,"total_qty":9}
```

The first record is not merely incomplete, it is **wrong in a way that reads as right**:
it describes an order the client never sent, consistently, with no field out of place. An
operator reconciling a client's dispute against the journal would confirm the venue's
wrong number from the venue's own record. That is the strongest argument for putting the
total where the total goes, and it is not a durability argument at all.

## 4. Decision 2 — what a pre-fix log means

A record written before this change genuinely cannot reconstruct its reserve. The
information was never on the disk. So this is not a decision about how to recover it; it
is a decision about what to do with a record that is intact, verified, contiguous,
correctly framed, and **insufficient**.

### 4.1 The three answers, at 3am

| Answer | What the operator gets | What it costs when it is wrong |
|---|---|---|
| **Recover silently** | The venue starts. It prints trades that never happened and holds a book nobody had. Nothing downstream detects it — every CRC passes, the set is contiguous, the digest is self-consistent | A wrong book discovered days later by a client dispute, with the journal itself confirming the wrong number (§3.4). This is today's behaviour |
| **Refuse any pre-fix iceberg record** | The venue will not start whenever such a record exists anywhere in a retained log, including logs with nothing left to apply | An outage manufactured by a check, on files that could be deleted with no effect. Then the override goes in the unit file permanently and the check is gone for good |
| **Refuse the REPLAY SET, report the rest, with a named override** (this) | The venue will not start *only if* a record that cannot state its reserve would be replayed. The message names the records, the orders that own them, and the routes forward | An operator who overrides without reading gets today's behaviour, having been told, with a count in `RecoverReport` and a metric |

The third is [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §3.1's rule applied one
level down — from a segment's declared semantics to a record's declared total — and it
inherits that section's design goal unchanged: **a venue that had a snapshot covering
those records starts with no ceremony at all.**

### 4.2 The rule

*Rule 6 — `Recover` refuses if and only if it is about to APPLY a `KindIceberg` record
that does not state its total. A record the snapshot already covers is read,
CRC-verified, skipped and never refused.*
*Reason:* a covered record contributes nothing to the recovered book —
`RestoreAfter` drops it by sequence whatever it contains — so refusing on it is refusing
on a file that could be deleted with no effect. Same move, same reason, as
[`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §5.2 and
[`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §3.1.

*Rule 7 — the gate is on the record, not on the file, and therefore does not care about
the framing.*
*Reason:* the same defect is in a v1 headerless log, an `OBWAL\x01` file and an
`OBWAL\x03` segment set, because it is a property of the JSON payload, which all three
carry identically. A header-level check would have to be invented three times and would
still miss the single-file case. It also means the check needs no new format version,
which §5 argues at length is the correct instrument here.

*Rule 8 — it runs after the walk and before `RestoreEngine`, over `walk.entries`
filtered by `Seq > after` — the same pass that already computes `RecoverReport.Applied`
(`wal.go:2071`).*
*Reason:* it costs one predicate on a slice that is already being iterated, it happens
before a single command is applied, and it needs no second read of anything. Unlike the
semantics gate it cannot be answered from the directory, because it is per record; that
asymmetry is the mirror of Rule 3 in `SEMANTICS-VERSION` (the semantics stamp is per
segment *because* it is the same for every record in it — and this fact is per record
because it is not).

*Rule 9 — `ReadAll`, `Open`, `Restore` and `RestoreAfter` never refuse.*
*Reason:* [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §9.1 settled it. `cmd/obgw` calls
`Recover` and then `Open` on the same path, so a stricter `Open` turns a benign file into
an outage by having two readers of the same bytes disagree; `ReadAll` is the diagnostic
[`RUNBOOKS.md`](RUNBOOKS.md) sends an operator to, and a diagnostic that refuses to show
you the file during an incident is not a diagnostic. `RestoreAfter` takes entries, not
files — and it is what `examples/replication/follower.go` applies, where refusing would
stop a follower rather than inform anyone.

### 4.3 The override

```go
// RecoverOptions gains:

// AcceptIcebergsWithoutReserve is the number of KindIceberg records this recovery
// may replay that cannot state their hidden reserve — records written before the
// total was journalled. It must equal exactly the number found: naming 12 when the
// log holds 11 or 13 refuses and says so.
//
// It is a COUNT rather than a boolean for the reason docs/SEMANTICS-VERSION.md's
// Rule 12 gives: a boolean goes into a unit file during one incident and stays for
// the life of the deployment, so the next occurrence — which this build cannot
// produce, and which therefore means a foreign writer or a downgrade — is accepted
// silently by a flag nobody remembers. A count goes stale the moment the log
// changes, and it makes the operator state a quantity they have read.
//
// It relaxes this gate and nothing else.
AcceptIcebergsWithoutReserve int
```

`cmd/obgw` grows `-wal-accept-iceberg-loss N`. `Recover` and `RecoverWithReport` keep
their signatures and pass the zero value, which accepts none.

*Rule 10 — the override is a count and there is no wildcard.* A sequence list was the
first design and is rejected on ergonomics: an archive replay with 2,000 such records
would need 2,000 numbers pasted at 3am, and an operator who cannot use the safe form
uses the unsafe one. The count keeps the property that matters — it goes stale, so the
decision has to be made again — while fitting on one line. What it gives up is stated
plainly: it asserts *how many*, not *which*, so a different set of the same size would
also pass. The class of damage is identical across the set, which is why that is
acceptable here and would not be for `ErrLogGap`.

*Rule 11 — the override applies to this gate only.* Not `ErrCorrupt`, not `ErrLogGap`,
not the floor check, not the semantics gate. An operator reaching for one permission
during an incident must not acquire a second.

### 4.4 The refusal, written out

Ergonomics here is a design requirement, and this refusal can be considerably more
useful than the semantics one, because the damage is localised to specific orders and
those orders are named in the records being refused.

```
wal: iceberg reserve unknown: 2 records this recovery would replay were written
before the journal recorded an iceberg's total size, so their hidden reserve
cannot be reconstructed.

  sequence 610,433   order u17/ACME-8841   displays 3, total unknown
  sequence 610,502   order u04/BLU-113     displays 50, total unknown

Replaying them rebuilds each as an ordinary order of its DISPLAY size. The
missing quantity is not merely hidden — it is gone, and every command after a
trade against one of these replays against a different book.

Three ways forward, safest first:
  1. Recover from a snapshot that covers sequence 610,502 or later. A snapshot
     carries the reserve, so these records are then behind the boundary and this
     refusal does not apply.
  2. Accept the loss deliberately, then CANCEL the two orders above and tell
     their owners to re-enter:  -wal-accept-iceberg-loss 2
  3. If you capture inbound order entry, the total is in the client's original
     EnterIceberg message; the journal is the only place it was lost.

Starting the PREVIOUS build does not help: it wrote these records and reads them
the same way.

See docs/RUNBOOKS.md "An iceberg whose reserve was never journalled".
```

The last line before the pointer is there because it is the one habit the semantics
runbook teaches that is **wrong here**: "put the old build back" is route 1 for a
semantics mismatch and is useless for this, since the old build has the same defect. A
message that omitted it would send an operator down a path that costs an hour and
changes nothing.

### 4.5 What is not counted, and why that is not a gap

*Rule 12 — the count is over the RECORDS THAT WOULD BE REPLAYED, never over the file.*

A `KindIceberg` record behind the snapshot boundary in a checksummed log is **never
decoded** — `walkSegment`'s parse predicate (`wal.go:1485`) parses record 1, everything
from the boundary onward, and every record of a v1 segment, and nothing else. So a
covered lossy record cannot be counted without undoing the covered-prefix skip, which is
the entire saving [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) exists for: 1.66 s → 64 ms
on a 500,000-record prefix.

That trade is not reopened here. Two consequences, both stated rather than discovered:

- A venue's report will say `IcebergsWithoutReserve: 0` for a log that contains a
  hundred of them behind its snapshot. That is correct — none of them affects the book —
  and the field's doc comment says so in those words.
- Defining the count over the replay set also makes it **framing-independent**: a v1
  segment parses every record and a headered one does not, so a count over the file
  would report different numbers for the same commands depending on which format they
  were written in. `ReadAll` remains the reader that sees everything, and
  [`RUNBOOKS.md`](RUNBOOKS.md) already sends operators there for exactly this kind of
  question.

### 4.6 The report and the metric

```go
// RecoverReport gains:

// IcebergsWithoutReserve counts KindIceberg records IN THE REPLAY SET that could
// not state their hidden reserve. Records the snapshot covers are not counted,
// because they are not decoded (docs/BOUNDED-RECOVERY.md §5.2) and do not affect
// the recovered book. Non-zero means either the recovery refused, or an override
// accepted the loss and the orders named in the log line need cancelling.
IcebergsWithoutReserve int
// IcebergReserveLossAccepted reports that AcceptIcebergsWithoutReserve let those
// records through. This is the field to alert on: it should be true exactly once,
// on one upgrade, and never again.
IcebergReserveLossAccepted bool
```

`cmd/obgw` logs the condition next to its existing recovery line and exports
`obgw_recovery_iceberg_reserve_unknown_total{symbol}`, with a threshold row in
[`RUNBOOKS.md`](RUNBOOKS.md): normal 0, any non-zero is an action, because this build
cannot produce such a record and one appearing after the upgrade means a foreign writer,
a downgrade, or a hand-edited log.

### 4.7 Why this is bounded and one-time, and where it is not

The condition can only arise from records written before the fix. After one checkpoint
under the new build every record in the replay set carries its total, and the gate can
never fire again on that venue. That is the same argument
[`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §4 makes, with one difference worth
naming: **an archive is forever.** A log kept for years for dispute reconstruction keeps
its pre-fix iceberg records permanently, so anyone replaying one meets this gate every
time. That population is deliberate — they are the people who most need to be told that
what they are reconstructing is not what the venue served — and route 2 of the message is
written for them.

## 5. Decision 3 — is this a matching-semantics change?

**No, and `matching.SemanticsVersion` must not move.** This is the crux the reviewer's
question names, so it is argued rather than asserted, including the argument against.

### 5.1 The case that it is one

[`COMPATIBILITY.md`](COMPATIBILITY.md)'s table says the semantics version answers *"will
these bytes replay into the book that was actually served"*. This change alters the
answer for a specific set of bytes: a pre-fix `KindIceberg` record replays into one book
under the old reader and — with its total present — a different one under the new. That
is precisely the property the stamp exists to protect, and a reading of that table alone
says bump it.

### 5.2 Why it is not one

**The definition is about builds and commands, not about readers and bytes.**
[`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §2.1: *two builds share a semantics
version if and only if, for every command sequence and every engine configuration, they
produce the same trades, the same events, the same verdicts and the same book.* Feed the
pre-fix engine and the post-fix engine the same command sequence — including
`ProcessIceberg(iceberg{total 9, display 3})` — and every trade, event, verdict and book
is identical. `pkg/matching` is not touched by this slice. The two builds are in the same
equivalence class, by the definition, exactly.

**What changed is the TRANSLATION between bytes and commands, not the engine that
consumes them.** The old writer encoded the command "iceberg 9 shown 3" as a record that
decodes to the command "iceberg 3 shown 3". The engine was never wrong; it was correctly
executing a command the journal had corrupted. Calling that a matching change would mean
the number no longer identifies a matcher, and the next person reading the registry
would find a row where nothing about matching moved.

**The enforcement refuses the bump, and this is decisive rather than convenient.**
`internal/semcheck` drives `matching.Engine`'s public API only — it does not import
`pkg/wal`, deliberately, and `semantics_alphabet_test.go` depends on that direction
holding. So this change leaves `testdata/semantics.txt` byte-identical, and Rule 22
(implemented in `semantics_test.go` as outcome 4) **fails a version bump with an
identical body**. The prescribed route out — *extend the corpus until it can see the
change* — is not available, because no engine tape can observe the contents of a WAL
record. A version that cannot be justified by the mechanism that maintains it is a
version that would have to be smuggled past its own guard.

**And the cost of bumping is exactly the failure mode that document is most afraid
of.** A bump to 3 refuses *every* segment declaring semantics 2 whose records would be
replayed — which is every segment on every disk written by the current release,
regardless of whether it holds a single iceberg — on every venue, on one upgrade, for a
condition that is present in a handful of records or in none. That is a check firing
on the happy path, which is how `-wal-accept-semantics` ends up in the unit file
permanently, and then the *real* mismatch three releases later is accepted silently. The
per-record gate in §4 refuses exactly the venues that have the problem and exactly the
records that have it.

### 5.3 So what does move

Nothing versioned, and that is the answer rather than an omission:

- **`SemanticsVersion`**: unchanged at 2. `internal/semcheck` must be green **without
  regeneration**, and that is deliverable 11 — it is the proof that this slice did not
  touch matching, in the same way `PINNED-DEFECTS.md` used a moved golden as proof that
  its slice did.
- **The wire version**: unchanged. `EnterIceberg` already carries the total plus the
  display size (`internal/wire/codec.go:547`). The client's message was never lossy; the
  journal was.
- **`Magic`, `SegMagicV3`, `SnapMagic`**: unchanged, per §3.3.
- **A new `EntryKind`** was considered and rejected. It would give old readers a clean
  forward-compatibility skip — they would drop the record entirely rather than rebuild it
  small — and dropping is *worse*: an iceberg that never rests loses every trade against
  it, where one rebuilt at display size at least reproduces its first slice. It would
  also burn a kind number and give `restoreEntry` two arms for one command forever.
- **The release version** carries it, with a changelog entry under `Fixed` naming the
  record change, the refusal, and the flag.

## 6. Decision 4 — making the next one impossible to find by accident

This defect was found by a reviewer reading two files, not by a test, and the reviewer's
question — *is the iceberg the only one?* — took a probe outside the repository to
answer. The next wrapper type should not need either.

Three guards, each catching the defect at a different depth. §1's audit is the first
one, promoted from a probe to a test.

*Rule 13 — every `EntryKind` is classified, and every kind that carries a wrapper has a
round-trip row.*
`TestEveryWrapperRecordRebuildsItsOrder` enumerates `KindSubmit..entryKindCount` — the
sentinel that already exists for `TestEveryEntryKindReplays` — and requires each kind to
be either a **wrapper** with a row, or **plain** with a one-line citation. A wrapper row
supplies a builder, an `Append*` call, and a probe tape; the test appends, processes on a
live engine, `Close`s, recovers **from the log alone**, runs the probe on both engines,
and requires the trades and the snapshot digests to agree. A new `EntryKind` with no
classification fails at the moment its constant is written, which is the same device
`entryKindCount` was introduced for.
*Reason:* this is the assertion that would have caught this defect in 2024, and it makes
no assumption about *why* a record might be insufficient. It catches constructor
mutation, derived state, an unlogged scalar, and whatever the sixth wrapper does that
none of the five do.

*Rule 14 — a constructor in `pkg/types` must not mutate the order it is handed, and the
one that does is a named exception with a citation.*
`TestNoOrderWrapperConstructorMutatesItsOrder` deep-copies, constructs, compares — for
stop, stop-limit, both OCO legs, pegged and trailing — and holds `NewIcebergOrder` as
the single exception, whose failure message is the rule: *if you are adding a
constructor that mutates, the record for it must carry enough to undo the mutation, and
Rule 13's row is where you prove it.*
*Reason:* Rule 13 catches the symptom on a scenario somebody wrote. This catches the
cause on the day it is introduced, before anyone has to think of the scenario. It is a
tripwire, not a proof, and it says so.

*Rule 15 — every exported field of every wrapper type is classified as logged,
re-derived at replay, or snapshot-only, with a citation for the last two.*
`TestEveryWrapperFieldIsAccounted` reflects over `StopOrder`, `OCOOrder`,
`IcebergOrder`, `PeggedOrder` and `TrailingStop` and requires each exported field to
appear in a map: `logged` (with the `Entry` field it lands in), `derived` (with the
reason, e.g. `JitterBps` ← `Config.IcebergPeakJitter` at `ProcessIceberg`), or
`snapshotOnly`. A new field fails until somebody writes which it is.
*Reason:* the field-level version of the audit in §1.1. It is mechanical where the
compiler can enumerate (the fields) and human where it cannot (the classification),
which is the same honest split [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §5.5 makes
about its alphabet guard, and it is stated with the same limit rather than sold as
airtight.

**The residual, named.** Rule 13's oracle is the snapshot digest plus a probe tape, so
derived state that is invisible to both is state nothing checks. Unexported wrapper
state that never influences a trade is the only thing in that gap today, and by
definition it cannot make a recovered venue behave differently — but "by definition" is
an argument, not a test, and it is written here so the next reviewer can attack it.

## 7. Rules that will look like bugs

| Rule | Why it will look like a bug |
|---|---|
| A `KindIceberg` record now says `quantity 9` where the engine's own order says `quantity 3` | The record and the live object disagree on the same field. That is the point: the record states the *command*, the object is the *implementation of it*. `Entry.Order` for a `KindSubmit` has always been the command as sent |
| The record carries the same number twice (`quantity` and `total_qty`) | It reads as a redundancy nobody removed. One of them is data and one is a witness whose only job is to be missing in old records (§3.1); Rule 5 says what happens if they ever disagree |
| A venue that recovers cleanly today refuses to start after the upgrade | Only if it is about to replay an iceberg record written before the upgrade. It is the one behaviour change with an operational cost, it is bounded to one restart per venue, and §4.1 is why it beats the alternative |
| A pre-fix iceberg whose display genuinely equalled its total is refused too | A false refusal, on a record that would have recovered correctly. It is indistinguishable from a lost reserve by construction (§3.1), it is accepted by the same override, and refusing an intact record beats starting a wrong venue |
| `IcebergsWithoutReserve` is 0 on a log full of them | The count is over the replay set, and covered records are never decoded (§4.5). A count that read the whole file would undo `BOUNDED-RECOVERY`'s 26× |
| "Start the previous build" is missing from this refusal and present in the semantics one | The two look like the same class of problem. The old build wrote these records and reads them identically, so the advice would waste an hour during an incident |
| `SemanticsVersion` does not move for a change that makes the same log replay into a different book | §5 is the argument. The short form: the engine is unchanged, the *record* was wrong, and bumping would fire on every upgrading venue to catch a condition almost none of them have |
| `TestLogOnlyRecoveryLosesAnIcebergsReserve` asserts the opposite of its name after this slice | An inverted pinning test reads as a test flipped to make a change pass. It is the mechanism that made this document exist (`differential_findings_test.go:12-17`), and the name is kept deliberately |

## 8. What this deliberately does not do

- **It does not repair a venue that has already recovered wrong.** An iceberg that came
  back at display size and has since traded is not reconstructable from anything the
  venue holds, and a snapshot taken after such a recovery faithfully carries the wrong
  reserve. This is the same shape as [`RUNBOOKS.md`](RUNBOOKS.md)'s "what the upgrade
  does NOT repair, at semantics 2": a state is not a program. The runbook section gains
  the same paragraph, with the remedy — cancel and re-enter, and tell the client.
- **It does not fix the ingress caps §1.3 measured.** `Config.MaxOrderQty` still sees the
  display slice, so nine lots refused as a plain order are accepted as an iceberg shown
  three; and `Config.MinOrderQty` still refuses a 90-lot iceberg shown 3. Both are
  ingress policy in `pkg/matching` with a venue-policy question attached ("is an
  iceberg's cap its total or its slice?"), both would change what the engine accepts —
  which *is* a semantics change, with a corpus extension and a version bump behind it —
  and neither belongs in a journal-format slice. **They are pinned**:
  `TestIcebergEvadesTheMaxOrderSizeCap` and `TestIcebergIsRefusedForAMinimumItExceeds`
  assert today's behaviour with the sentence a fix must come and delete.
- **It does not model icebergs in `internal/refmatch`.** They stay tier 2
  ([`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §2.4), so the differential harness
  cannot reach any of this and a green sweep is evidence of nothing about it — stated
  here because "the differential sweep is green" is exactly the sentence someone will
  offer as proof.
- **It does not extend `internal/semcheck`'s corpus**, and it must not: the corpus is
  the evidence base for version bumps, and widening it in a slice that deliberately does
  not bump would make the next bump's diff unreadable.
- **It does not change the covered-prefix skip**, in either direction. §4.5 is the
  decision to leave it alone, taken knowingly.
- **It does not add a repair tool.** A venue that captures inbound order entry can
  recover a lost total from the client's original `EnterIceberg` message; turning that
  into a journal-rewriting utility is a tool that edits a durable record, which needs its
  own document and probably its own answer of "no".
- **It does not touch the framing, the segment header, the snapshot format, `ReadAll`'s
  contract, or any signature.** Every existing assertion in `pkg/wal` stands; the two
  pinning tests are inverted, keeping their names, and every assertion in them is kept.

## 9. Deliverables and acceptance criteria

| # | Deliverable | Done when |
|---|---|---|
| 1 | The audit is a test, not a probe | `TestEveryWrapperRecordRebuildsItsOrder` exists, enumerates `entryKindCount` and classifies all 16 kinds. Run it **before** the writer change: the four sibling rows pass and the iceberg row fails. That ordering is the deliverable — a guard first seen green proves nothing about what it detects |
| 2 | The record states the total | `AppendIceberg` logs a copy with `Quantity == RemainingQty == ib.TotalRemaining()` and `FilledQty == 0`, plus `TotalQty`; the live order is byte-identical after the call (Rule 2) |
| 3 | The reader uses it | `restoreEntry`'s `KindIceberg` arm implements Rule 4; `entryKindSamples`' hand-built record still restores to a reserve of 8, unmodified |
| 4 | The pin is inverted | `TestLogOnlyRecoveryLosesAnIcebergsReserve` **inverted, keeping its name**: the record reads `qty 9 display 3 total 9`, the recovered iceberg holds `Hidden 6`, and the buy of 9 prints 9. Every existing assertion kept |
| 5 | The other half still holds | `TestSnapshotRecoveryKeepsAnIcebergsReserve` passes unmodified |
| 6 | The compounding case is asserted, not just the reserve | A new test for §2's tape: live and log-only recovery agree on trade count (3), printed quantity (9) and final book (`SELL 5 @100`). On today's build it must fail on **both** halves independently — the two prints that go missing and the five-lot print that is invented — because a fix that repaired only the first would leave the venue printing trades nobody made |
| 7 | A pre-fix log refuses | `TestPreFixIcebergRecordInTheReplaySetRefuses`: a hand-built `KindIceberg` record with no `TotalQty` past the boundary yields `ErrIcebergReserveUnknown`, and the message names the sequence and the order |
| 8 | …and is not refused when it is covered | `TestPreFixIcebergRecordBehindTheSnapshotStartsTheVenue`: same record, snapshot ahead of it, venue starts on the same digest the snapshot alone gives, `IcebergsWithoutReserve == 0` |
| 9 | The override works and is exact | `TestAcceptIcebergLossRequiresTheExactCount`: N accepts, N−1 and N+1 refuse and say the real number; `IcebergReserveLossAccepted == true`; `ErrCorrupt` and `ErrLogGap` are unaffected by it |
| 10 | Every format still recovers | v1 headerless, `OBWAL\x01`, `OBWAL\x02` and `OBWAL\x03` sets each with a post-fix iceberg record recover to the live digest; the same four with a pre-fix record refuse identically (Rule 7). `go test ./pkg/wal/... ./cmd/obgw/... ./examples/...` green, full run and `-race` |
| 11 | Matching did not move | `internal/semcheck` green **without regeneration**; `matching.SemanticsVersion` still 2; `TestZeroAllocHotPath` unmodified |
| 12 | The surface change is deliberate | `surface.txt` regenerated in the same commit, diff shows five additions and no `REMOVED or CHANGED` |
| 13 | Reintroduction is guarded | Rules 14 and 15 exist as tests; each fails against a newly-added mutating constructor and a newly-added wrapper field respectively |
| 14 | The two adjacent findings are pinned | `TestIcebergEvadesTheMaxOrderSizeCap` and `TestIcebergIsRefusedForAMinimumItExceeds` assert today's behaviour and carry the sentence a fix must delete |
| 15 | The record is updated where it is wrong | `AppendIceberg`'s doc comment (which claimed this behaviour for four releases); `PINNED-DEFECTS.md` §13.7 marked fixed with a date and a pointer here; `RUNBOOKS.md` gains "An iceberg whose reserve was never journalled"; `CHANGELOG.md` names the record change, the refusal and the flag |
| 16 | Nothing else moved | `go build ./...`, `go vet ./...`, `go test ./... -count=1` green; `gofmt -l` clean on every touched file |

### 9.1 The numbers to record when it is done

None is a pass criterion on its own; they are what §11 is graded against.

- **Bytes added to a `KindIceberg` record.** Expected 14 (311 → 325 measured). Bytes
  added to every other kind: expected 0, checked by a byte comparison of a written log
  before and after, not by inspection.
- **`BenchmarkRecoverBehindACoveredChurnPrefix`, before and after.** Expected: unchanged
  inside noise. The gate is one predicate per retained record; if this moves, it is
  reading records it should not be.
- **Tests that fail with the writer fixed and the pin not yet inverted.** Expected:
  exactly 1 — the pin. Anything else is a behaviour change nobody argued for.
- **Tests that fail with the reader's Rule 4 fallback removed.** Expected: at least
  `TestEveryEntryKindReplays`, via the hand-built sample. Record the full list: it is the
  measure of how much of the suite writes records by hand.
- **`IcebergsWithoutReserve` on a 500,000-record log holding 100 pre-fix iceberg records
  behind the snapshot.** Expected 0, and the reason is §4.5 rather than a bug.

## 10. Sabotage runs required before this counts as done

Per [`TESTING.md`](TESTING.md), nothing above counts until every row has been run and
its result recorded — including rows whose honest result is "nothing failed".

| # | Sabotage | Must fail | Status |
|---|---|---|---|
| 1 | Log the total but forget `RemainingQty` (`Quantity` only) | Deliverable 6. `Fresh()` recomputes `RemainingQty` from `Quantity`, so the *reserve* is right and this may catch nothing — record it either way, because that answers whether Rule 1's second field is load-bearing | **run — caught by exactly one assertion, and not the one predicted.** No recovery test moved: the reserve is correct either way, exactly as the row suspected. The only failure is the inverted pin's *record* assertion (`remaining=3`, want 9). So Rule 1's second field is load-bearing **for what a human reads**, not for what a replay rebuilds, and one assertion is the whole of its protection |
| 2 | Set the total on the LIVE order instead of a copy (drop Rule 2) | Any test that matches against a fresh iceberg: the book shows 9 where it should show 3. If only one test catches it, that test is the whole of Rule 2's protection and should say so | **run — four tests, two packages.** `TestALostReserveInventsTradesAndFlipsTheBook`, `TestLogOnlyRecoveryLosesAnIcebergsReserve`, `TestEveryWrapperRecordRebuildsItsOrder`, and `TestDrillD11_AnIcebergReplicatesWithItsReserve` in `examples/replication`. Not resting on one test |
| 3 | Write `TotalQty` and leave `Order.Quantity` at the display size | Deliverable 4's assertion on the record's `quantity`, and nothing else — which measures exactly how much of §3.4's "what a human reads" is actually asserted | **run — five tests, and the extra four are Rule 5's doing.** The record now contradicts itself (`quantity 3, total_qty 9`), which §3.2 Rule 5 classifies as stating NEITHER, so every recovery refuses. §3.4's sentence is asserted once directly and enforced four more times as a side effect. This is the mutation §11 asked for: **Rule 5 is not over-engineering** |
| 4 | Drop `TotalQty`, keeping only the total in `Order.Quantity` | Deliverable 7: with no witness there is no discriminator, so a pre-fix record is indistinguishable and the gate cannot fire | **run — five tests, failing the other way round.** Deliverable 7's own test still passes (it hand-builds its record). What breaks is every *correct* record: with no witness `statesItsTotal` is unanswerable, so the gate refuses icebergs this build itself wrote. The discriminator's absence shows up as **over**-refusal, not under-refusal — worth recording, because the row predicted the opposite |
| 5 | Remove Rule 4's fallback (`total = e.Order.Quantity` when `TotalQty` is absent) | `TestEveryEntryKindReplays`, via the hand-built sample. This is the row that proves the fallback is not decoration | **run — and `TestEveryEntryKindReplays` stayed GREEN, which is a finding about that guard.** Its boolean means "restoreEntry RECOGNISED the kind", not "applied it": with the total reading zero, `NewIcebergOrder` refuses the display size and the record is dropped on the floor inside an arm that still returns true. `TestAHandBuiltIcebergRecordStillMeansTheTotal` was added to assert deliverable 3 directly, and with it the row fails on three tests |
| 6 | Move the gate from the replay set to the whole file | Deliverable 8: a covered pre-fix record must not refuse. It will also make the gate silently framing-dependent (v1 parses everything, headered does not), which no assertion catches — add one if it does not | **run — caught NOTHING, and the row was right to ask.** `walkSegment` retains only records past the boundary, so the two filters agree on every ordinary recovery. They part company in one place: after a `FellBack`, `walk.entries` holds the covered prefix. `TestACoveredLossyIcebergIsNotCountedEvenWhenTheWalkFallsBack` was added for it; the sabotage then fails |
| 7 | Make the gate run inside `ReadAll` as well | Deliverable 10 and `RUNBOOKS`' diagnostic path: a reader that refuses to show an operator the file during an incident | **run — caught by `TestDiagnosticReadersNeverRefuseALossyIceberg`**, the test written for Rule 9, and by nothing else. `cmd/obgw` stayed green, which is the point: it calls `Recover` and then `Open` on the same path, so the damage would have surfaced as two readers of the same bytes disagreeing |
| 8 | Make the override a boolean | Deliverable 9's N−1 / N+1 assertions | **run — caught by `TestAcceptIcebergLossRequiresTheExactCount`** |
| 9 | Bump `matching.SemanticsVersion` to 3 | `internal/semcheck` outcome 4 — a bump with an identical body. This row is §5's argument, executed | **run — refused by its own guard**, on two tests: *"SemanticsVersion is 3 and testdata/semantics.txt records 2, but the engine's behaviour on the whole corpus is unchanged"* and the golden's version line. §5.2's mechanical argument holds |
| 10 | Add a sixth wrapper type with a mutating constructor and no `Entry` field | Rules 13 and 14, both. If only one fires, the other is weaker than §6 claims | **run — both fire**, plus `TestEveryEntryKindReplays`. Rule 14 only fires because it was built to **enumerate constructors from the package's own source** (`go/ast`) rather than from the hand-written list §6 described; with the list, a sixth constructor would have been invisible to it and this row would have failed |
| 11 | Add an exported field to `IcebergOrder` and log nothing for it | Rule 15 only. If Rule 13 also fires, the field guard is redundant and should be argued down | **run — Rule 15 only**, exactly as predicted. Rule 13 cannot see a field that influences no trade, which is §6's named residual demonstrated rather than asserted. The field guard is not redundant |
| 12 | Apply the whole fix and revert only the inverted pin | Nothing new — the pin fails, which is the point. Record that `pkg/matching` and `internal/semcheck` stay green, because that is the evidence for deliverable 11 | **run — the pin fails with its own message** (*"IF THIS NOW READS qty=9 THE DEFECT HAS BEEN FIXED"*), `TestSnapshotRecoveryKeepsAnIcebergsReserve` still passes, and `pkg/matching` and `internal/semcheck` are **green**. Deliverable 11's evidence |

Rows 1, 3, 6, 11 and 12 ask for a **measurement** rather than a failure. A sabotage
nothing catches is the most useful row in the table: it names which assertion is doing
the work, and twice above the honest answer may be "none, yet".

## 11. How this can fail, stated in advance

So whoever implements this is not graded on a curve.

- **The refusal may fire more often than §4.7 predicts.** `cmd/obgw` does not checkpoint
  on clean shutdown (`Server.Close`, `server.go:708`: it drains, closes the runners, then
  syncs and closes the logs), so an ordinary stop leaves up to one checkpoint
  interval of unreplayed records — the same fact that made
  [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §4 concede that the ordinary upgrade
  path hits its refusal once. Here it needs an iceberg in that window as well, so it
  should be much rarer. If it is not, the diagnosis is the same and so is the fix: a
  final checkpoint on shutdown, which is not in this slice.
- **The count-based override may prove unusable in the archive case.** If replaying old
  logs for research turns out to hit this constantly, operators will want a wildcard, and
  the pressure to add one will arrive as a reasonable request. The answer, if it comes,
  should be a *separate* documented mode for archive replay — not a flag on the venue's
  recovery path — because the two populations want opposite defaults.
- **Rule 5 may be over-engineering.** A record where `TotalQty` and `Order.Quantity`
  disagree cannot be produced by this package, so the rule may never execute outside its
  own test. It is cheap, it fails safe, and if a reviewer finds a mutation it catches
  that nothing else does, that mutation belongs in §10.
- **§5's argument may not survive the next reader.** The distinction between "the matcher
  changed" and "the record was wrong" is real but it is a *judgement about which layer
  moved*, and the layer boundary is not enforced by anything. If a future change makes
  `pkg/wal` and `pkg/matching` share more state, the argument weakens and the honest
  response is to re-argue it rather than to cite this section.
- **The three guards in §6 may not survive contact with a real sixth wrapper.** They are
  designed against five examples that are all "a base order plus scalars". A wrapper that
  is genuinely two orders with shared state — the OCO is the closest thing today and it
  is still just two orders — could satisfy all three guards and still be lossy. That is
  the same class of gap this document was written to close, one level up, and naming it
  is the only defence available in advance.

## 12. What building it changed

Written after the fact, in the shape [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §9
uses: the numbers §9.1 asked for, the three places the design was wrong, and the two
places the guards had to be built stronger than this document specified.

### 12.1 The numbers §9.1 asked for

| Measurement | Predicted | Measured |
|---|---|---|
| Bytes added to a `KindIceberg` record | 14 | **14** (311 → 325), asserted by `TestAPostFixIcebergRecordCostsFourteenBytesAndNothingElse` rather than inspected |
| Bytes added to every other kind | 0 | **0.** Checked over the whole of `entryKindSamples`: no other kind emits `total_qty`, and a zero-valued `Entry` does not either |
| `BenchmarkRecoverBehindACoveredChurnPrefix`, before and after | unchanged inside noise | **unchanged, and the allocation counter is the real evidence.** 500,000-record covered prefix: 31.8–33.9 ms with the gate against 37.4–45.3 ms without it, which says only that this machine's noise (14.3 ms to 45.3 ms across runs of the same configuration) swamps the effect. `2,388,432 B/op` and `9,619 allocs/op` are identical to the byte in both, which is the statement that matters: the gate reads no record it should not |
| Tests failing with the writer fixed and the pin not yet inverted | exactly 1 | **exactly 1** — `TestLogOnlyRecoveryLosesAnIcebergsReserve` — plus `internal/apicheck`, which is deliverable 12 arriving on schedule rather than a behaviour change |
| Tests failing with Rule 4's fallback removed | at least `TestEveryEntryKindReplays` | **3, and `TestEveryEntryKindReplays` is NOT among them.** See §12.2 |
| `IcebergsWithoutReserve` on a 500,000-record log with 100 pre-fix iceberg records behind the snapshot | 0 | **0**, on a 145.3 MiB log, recovering in **64 ms** with 499,000 skipped and 1,000 applied — the same 64 ms [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) records for that prefix. Moving the same file's snapshot below those records refuses and names 50 of them |

### 12.2 Three places this document was wrong

- **Sabotage row 5 named the wrong guard.** It expected removing Rule 4's fallback to
  fail `TestEveryEntryKindReplays` through the hand-built sample. It does not, and the
  reason is a property of that guard worth writing down: its boolean means *restoreEntry
  RECOGNISED the kind*, not *applied it*. With the total reading zero,
  `types.NewIcebergOrder` refuses the display size and the record is dropped on the
  floor inside an arm that still returns `true`. Deliverable 3's claim — that the
  hand-built sample still restores to a reserve of 8 — was therefore asserted by
  nothing, and `TestAHandBuiltIcebergRecordStillMeansTheTotal` was added to assert it
  directly. **No existing assertion was weakened**; one was added that names what it
  holds.
- **Sabotage row 4 predicted the wrong direction.** With `TotalQty` dropped, the gate
  does not silently fail to fire — it fires on records this build itself wrote, because
  `statesItsTotal` keys on the witness and without it the question is unanswerable. The
  discriminator's absence surfaces as over-refusal, which is the fail-safe direction and
  is caught by five tests. Deliverable 7's own test still passes, since it hand-builds
  its record.
- **Sabotage row 6 caught nothing, as it suspected, and the reason is sharper than the
  row guessed.** The gate over the replay set and the gate over the whole walk agree on
  every ordinary recovery, because `walkSegment` RETAINS only records past the boundary
  — the sequence filter in `RecoverWithOptions` is redundant with it. They part company
  in exactly one place: after a `FellBack`, `walk.entries` holds the covered prefix too.
  `TestACoveredLossyIcebergIsNotCountedEvenWhenTheWalkFallsBack` is the assertion that
  row asked for.

### 12.3 Two guards built stronger than §6 specified

- **Rule 14 enumerates constructors from the package's own source.** As written, §6
  described a hand-listed set of constructors — and sabotage row 10 (a sixth wrapper with
  a mutating constructor) would then have been invisible to it, leaving §6's claim that
  "Rules 13 and 14 both fire" false. It now parses `pkg/types` with `go/ast` and requires
  every exported `New*` whose first parameter is a `*Order` to have a row. Reading source
  in a test is unusual; it is the only mechanical option, since Go reflection enumerates a
  struct's fields but not a package's functions, and a hand-maintained list is precisely
  the artifact that let this constructor go unexamined for four releases.
- **Rule 13's probe had to trade to see anything.** The first version compared snapshot
  digests and swept the book from both sides with one user, and the sweep never reached
  the iceberg: the sell rested at 1 and became the buy's own counterparty, and self-trade
  prevention cancelled what was left. A probe that does not print measures the book's
  shape rather than its contents, and the reserve is invisible in the shape by
  construction. The buy goes first and the two sides are different users, and the row now
  fails on the trades as well as on the digest.

### 12.4 What is still true and uncomfortable

- **The refusal lists every record.** Measured on a log holding 50 lossy records in the
  replay set: fifty lines. An archive replay with two thousand would produce a
  two-thousand-line error. The list is not capped, because route 2 of the message tells
  an operator to cancel *the orders above* and a truncated list makes that instruction
  wrong. §11's second bullet already names archive replay as the population this design
  serves worst, and this is the shape that discomfort actually takes.
- **`examples/replication` gained the drill it never had.** The follower was repaired for
  free by the record change, as §1.3 predicted, and
  `TestDrillD11_AnIcebergReplicatesWithItsReserve` is what stops that being an assertion.
  It fails on the pre-fix writer and on sabotage row 2, which is what makes it evidence.
