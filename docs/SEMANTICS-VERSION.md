# Semantics Version — Making a Replay Refuse Rather Than Lie

Status: **built** — slice 3 of
[`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) M3, written before the code as this
repository does it. §12 records what building it changed, including the two places the
design below was wrong. ·
Author: Karthikeyan NG · Last updated: 2026-08-17

> **The one decision this document exists to get right is §5, and the second is §3.**
>
> §5 is the enforcement. A version somebody has to remember to bump is worse than no
> version at all, because it promises detection it does not provide — the first two
> people to touch `matching` will remember, the third will not, and from that day the
> stamp is a number that says "these two logs replay identically" while they do not.
> If this slice ships without a mechanical forcing function it should not ship.
>
> §3 is what recovery does on a mismatch, and it is load-bearing because both obvious
> answers are wrong. *Warn and continue* is today's behaviour with a log line on top:
> the venue still serves a book that never existed. *Always refuse* strands a venue
> whose log is fine, and a check that fires when nothing is wrong is a check operators
> learn to switch off — at which point it is worse than the warning, because now
> nobody is even reading the line. §3's answer is that the gate is on the records
> recovery is **about to replay**, not on the files it can see.
>
> Nothing below is measured. Every number in §8 is a target with a named fixture. A
> section recording what building it changed will be added afterwards, in the shape of
> [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §9 and
> [`LOG-ROTATION.md`](LOG-ROTATION.md) §12.

Companion documents:
- [`DIFFERENTIAL-FINDINGS.md`](DIFFERENTIAL-FINDINGS.md) — the three matching changes
  that opened this gap, and the reasoning that picked each. §5 — defect C, pro-rata —
  is the widest of the three and the reason this is urgent rather than tidy.
- [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §4 and §5.2 — the invariant, the check
  and the fallback this inherits, and the precedent for a check whose strictness moves
  with the snapshot boundary. §9.1 is the rule that `Open` must be the most permissive
  reader in the package, which §3.5 obeys.
- [`LOG-ROTATION.md`](LOG-ROTATION.md) §2 and §4.4 — the segment header this extends
  and the startup validation this joins. §2.4 already rejected three of the five
  placements §2 below would otherwise have to re-argue.
- [`COMPATIBILITY.md`](COMPATIBILITY.md) — the promise this lives inside, and
  `internal/apicheck`, which is the pattern §5 copies.
- [`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §2.2 — the model's known dependence on
  `engine.go`, which is why the differential harness cannot be the forcing function
  and §5.2 has to build a different one.
- [`RUNBOOKS.md`](RUNBOOKS.md) — gains one section, "Upgrading across a semantics
  change", which is the page an operator lands on at 3am from the refusal message.

---

## 1. Why this exists

Three changes landed tonight, all under `Unreleased` / `Changed`, and each alters what
matching does with the **same input**:

1. A rejected fill-or-kill no longer moves `LastTradePrice`.
2. A rejected command's event batch now carries the events describing state that was
   not rolled back.
3. Under `ProRata`, a taker that meets its own resting liquidity is no longer skipped;
   self-trade prevention decides, and with the default `CANCEL_NEWEST` a taker that
   used to rest is now cancelled.

The third is the widest. A taker that used to rest across the spread is now gone, so
**every command after it in the log replays against a book that differs from the one
the live venue had** — not one order out of place, a divergence that compounds for the
rest of the file.

Neither `wal.Entry` nor `matching.EngineSnapshot` carries any notion of which engine
wrote them. `wal.Recover` therefore cannot detect this and cannot refuse. It replays,
it starts, and the book is wrong in a way nothing downstream flags: every record
verifies its CRC, the segment set is contiguous, the snapshot is above the floor, the
digest is self-consistent, and the venue serves.

The changelog's current advice — take a checkpoint and archive the log it covers before
upgrading — is correct and is not a mechanism. It works exactly as well as the operator
who read it, on the night they read it, and it does nothing at all for the case that
matters most: a venue that **crashed** across the upgrade and is recovering a tail it
did not get to checkpoint.

This slice is the stamp that turns that advice into a refusal.

### 1.1 What this is not, said first

Two version numbers already exist in this repository and this must not be confused with
either. Both confusions have a failure mode worth naming in advance, because both are
what a reader skimming §2 will assume.

| | Answers | Changes when | Lives in |
|---|---|---|---|
| **Release version** (`v0.25.0`) | "which build is this" | every release | git tags, `CHANGELOG.md` |
| **Format version** (`OBWAL\x01`, `OBWAL\x02`, `OBSNAP\x01`, wire v4) | "can this reader parse these bytes" | the byte layout changes | file headers |
| **Semantics version** (this) | "will these bytes replay into the book that was actually served" | matching behaviour changes | segment headers, snapshots |

A release version as the stamp refuses logs that would replay identically, on every
upgrade, forever. That is the failure this document is most afraid of, because the
response to it is not "fix the check", it is `-wal-accept-semantics` in the unit file
permanently, and then the real mismatch three releases later is accepted silently by
a flag nobody remembers adding.

A format version as the stamp is silent in the other direction: all three changes above
are byte-identical on disk. The same `Entry`, the same framing, the same CRC. A format
version cannot see them and never will.

### 1.2 The registry

The number is small, dense, and has a table. The table is part of the contract: a
version with no row is a version nobody wrote down, which is the state this document
exists to leave behind.

| Semantics | Build | What it means |
|---:|---|---|
| **0** | every build up to and including v0.25.0 | **Unknown.** Not "1", not "compatible", not anything. See §4 |
| **1** | v0.25.0 + the differential-findings slice | The first stamped semantics. Includes the three changes in §1 — [`CHANGELOG.md`](../CHANGELOG.md) *Unreleased / Fixed*: "A rejected fill-or-kill no longer moves `LastTradePrice`"; *Unreleased / Changed*: "A `REJECTED` command's event batch may now carry further events" and "Under `ProRata`, a taker that meets its own resting liquidity is no longer skipped" — so it is *not* the semantics of the last released build |
| **2** | v0.25.0 + the pinned-defects slice | The two fixes in [`PINNED-DEFECTS.md`](PINNED-DEFECTS.md) — [`CHANGELOG.md`](../CHANGELOG.md) *Unreleased / Fixed*: "A failing fill-or-kill no longer corrupts an iceberg it consumed"; *Unreleased / Changed*: "A cascade-fired order the venue refuses now publishes a `CANCELED`". Only the first is replay-visible; the second ships under the same number because the two are one commit and the golden moves for both |
| **3** | this release onward | The admission fix in [`ICEBERG-ADMISSION.md`](ICEBERG-ADMISSION.md) — [`CHANGELOG.md`](../CHANGELOG.md) *Unreleased / Changed*: "The per-order size and notional caps measure the quantity the client submitted" and "Admission no longer runs again on an iceberg refill". `MinOrderQty`, `MaxOrderQty`, `MinOrderNotional`, `MaxOrderNotional` and the int64 notional overflow guard measured an iceberg's **displayed slice**, so a venue capped at 5 lots accepted 9 shown 3 and refused 90 shown 3; they now measure the client's total. A venue that sets none of the five, or accepts no icebergs, sees no change. Shipping under the same number, because they are one commit and the golden moves for all of them: an iceberg the venue REFUSES is no longer left in the engine's iceberg registry, where it made every later checkpoint unloadable ([`ICEBERG-ADMISSION.md`](ICEBERG-ADMISSION.md) §13.4) — *Unreleased / Fixed*: "A refused iceberg no longer makes the venue's own snapshot unloadable" |

**Row 2 nearly did not exist, and the reason is worth the line.** `internal/semcheck`
was **green** on both of those fixes with the corpus as it stood: the tier-2 script
reached icebergs and reached stops, and never crossed a fill-or-kill with either. Under
Rule 22 that is not "no bump needed", it is §5.5's boundary — *a behaviour the corpus
never reaches is a behaviour nobody can bump for* — so the corpus gained thirteen
commands first, and the number moved on the strength of the thirteen lines that
appeared. `Coverage.IcebergRestores` and `Coverage.CascadeTerminals` exist so it cannot
silently go back.

Six of those thirteen were added *after* the first version of the fix, and they are the
sharper illustration of the same rule. Review found that fix rewriting **queue order** at
a level — a restored iceberg re-entering ahead of a maker that had been resting in front
of it — with this fingerprint green, because the corpus's iceberg was alone at its price.
A rejected order that reorders a level moves no aggregate and no event; it moves the
maker id on the next print, and nothing in the corpus took that print.
[`PINNED-DEFECTS.md`](PINNED-DEFECTS.md) §13.6.

**Row 3 is the same rule a third time, one scenario over.** `conditional` was the only
scenario with icebergs and it configures no caps; `guarded` was the only scenario with
caps and it had no iceberg. So a change to *what the caps measure* left the golden
byte-identical, and outcome 4 refused the bump — measured, not predicted. `guarded`
gained eight commands first (seven cases and a cancel that clears the resting
privileged bid out of the band's way), and the number moved on the strength of the
eight lines that appeared. Two of the seven earn their place by **not** moving: a
one-lot iceberg is still dust, and one at exactly `MaxOrderQty` is still accepted — a
fix that measured anything other than the client's total would have moved them.
[`ICEBERG-ADMISSION.md`](ICEBERG-ADMISSION.md) §7.

**And a fourth time, one control over, found by review rather than by the gate.** The
extension above covers the two QUANTITY caps. `guarded` sets no notional caps, so
reverting only the notional half of the same five-check fix — both notional controls
*and* the `int64` overflow guard, the one no operator can switch off — left the golden
**byte-identical** again. Rule 22 would have permitted that regression under a number
saying nothing changed. The `notional` scenario closes it, appended last so it moves no
existing line, and it brings three rejection kinds the corpus had never reached:
`ORDER_EXCEEDS_MAX_NOTIONAL`, `ORDER_BELOW_MIN_NOTIONAL` and `NOTIONAL_OVERFLOW`, all
0→n. **The lesson is not "extend the corpus" but where to look:** all four times, the
gap was a scenario that turns a knob on and a scenario that reaches the behaviour, and
they were never the same scenario. [`ICEBERG-ADMISSION.md`](ICEBERG-ADMISSION.md) §13.9.

**This slice must ship in the same release as those three changes.** If it slips a
release, semantics 0 comes to mean two genuinely different matchers — builds before the
changes and builds after them but before the stamp — and the one thing §4 can say about
unknown ("it is not this build's") stops being the whole story. The rule in §4 still
holds either way; the registry just gets a footnote it should not need. Ship them
together.

Every future row needs three things and the acceptance criteria in §8 check for all
three: the number, the release, and a link to the changelog entries that justify it.

## 2. What the number means, and where it is written

### 2.1 The definition

**`matching.SemanticsVersion` identifies an equivalence class of builds: two builds
share a semantics version if and only if, for every command sequence and every engine
configuration, they produce the same trades, the same events, the same verdicts and the
same book.**

That is a statement about observable behaviour and not about code. A refactor that moves
a thousand lines and changes no outcome does not bump it. A three-character change that
makes one rejected order stop moving one price does.

*Rule 1 — the version changes if and only if the behaviour fingerprint in §5 changes.*
*Reason:* this is the definition restated in a form that can be applied without
judgement, which is the whole requirement. "Did matching behaviour change?" is a
question two engineers can answer differently at 6pm on a Friday. "Did
`internal/semcheck/testdata/semantics.txt` change?" is a question `git diff` answers.
The definition above is what the fingerprint is *for*; the fingerprint is what anybody
is actually held to.

*Rule 2 — it is an integer, it only ever increases, and it is set by hand.*
*Reason:* it has to be orderable, because the first thing an operator needs from a
refusal is the direction ("this log is older than this build" versus "somebody
downgraded"), and the second is a range to read in the changelog. §2.5 rejects the
derived-hash alternative that would remove the hand-setting, and the reason it is
rejected is not conservatism.

### 2.2 Where it is written: both, for different reasons

**In every log segment's header**, because a log is a *program* — recovery re-executes
it, and the question "under which rules were these commands executed the first time" is
a property of the records, not of the venue.

**In the snapshot**, because a snapshot is a *state*, and knowing which rules produced a
state is what lets an operator answer "is this checkpoint usable" without guessing.

They cost different things and they are used differently, and conflating them is how
this design goes wrong:

| | Written | Cost | Role |
|---|---|---|---|
| Segment header | once, at segment creation | 4 bytes per segment, 0 per record | **The gate.** §3 |
| Snapshot | every checkpoint | 1 JSON field, `omitempty` | **A report.** Never a gate |

*Rule 3 — the stamp is per SEGMENT, never per record and never per set.*
*Reason:* per record it would cost eight bytes on every one of 216 million records a
day to carry a number that is the same for all of them, and it would put a new field on
`wal.Entry`, which is frozen exported surface and is the JSON in every archived segment
for as long as the archive is kept. Per set it would be a lie the first time a venue
upgrades: a set legitimately spans upgrades, so segment 400 can be semantics 2 while
segment 1 is semantics 1, and that is not a corruption — it is precisely the fact
recovery needs. [`LOG-ROTATION.md`](LOG-ROTATION.md) §2.4 already rejected a
head-of-segment *record* for an independent reason (`boundary_test.go:105` asserts one
journal record per command) and the same veto applies here unchanged.

*Rule 4 — the snapshot's stamp is never a gate, in either direction.*
*Reason:* a snapshot is not replayed. Restoring a book that a semantics-1 venue actually
had, into a semantics-2 engine, gives you exactly what the changelog already tells
operators to do — "recovering from a snapshot written by the old build ... is a book
that build agreed with, rather than replaying its commands through this one". Gating on
it would refuse the recommended procedure. It would also give the design two gates with
two different rules, and an operator facing two refusals learns one override and applies
it to both.

### 2.3 The segment header, concretely

Today ([`LOG-ROTATION.md`](LOG-ROTATION.md) §2.2, `pkg/wal/segment.go:41`):

```
offset 0    "OBWAL\x02"      6 bytes
offset 6    [base:8]         big-endian int64
offset 14   [crc:4]          CRC-32C over the 8 base bytes
            [len:4][crc:4][payload]     record 1
```

With the stamp:

```
offset 0    "OBWAL\x03"      6 bytes
offset 6    [base:8]         big-endian int64
offset 14   [sem:4]          big-endian uint32: the semantics of the build that wrote
                             this segment's records
offset 18   [crc:4]          CRC-32C over the 12 bytes at offset 6
            [len:4][crc:4][payload]     record 1
```

`SegHeaderBytesV3 = 22`. Records are untouched: same framing, same CRC-32C over the
payload only, same `MaxRecordBytes`, byte-identical to what any earlier build would have
written at the same point in the stream. `segment.headerBytes()` already switches on
kind, so a fourth kind costs one case.

*Rule 5 — the magic is bumped rather than the header extended in place.*
*Reason:* a build that knows only `OBWAL\x02` would read four extra bytes at offset 18
as a record length prefix. A semantics of 1 is the four bytes `00 00 00 01`, a declared
record length of 1, which passes the `MaxRecordBytes` bound and fails its CRC — so the
old build refuses with `wal: corrupt record`, and an operator is sent to
[`RUNBOOKS.md`](RUNBOOKS.md) §"A corrupt log record" to check SMART data for what is a
downgrade. Bumping the magic instead sends the old build down the path
[`LOG-ROTATION.md`](LOG-ROTATION.md) §2.5 already built and already documented: six
bytes that are not `OBWAL\x01` are treated as a headerless v1 log, `"OBWA"` is read as a
length prefix of 1,329,747,777, and it refuses with `wal: corrupt record: record 1
declares 1329747777 bytes (limit 8388608)` — the exact message the runbook already
explains means a version mismatch and not a disk. One loud failure mode instead of two,
and the second one is already written down.

*Rule 6 — the CRC covers the base AND the semantics, as one twelve-byte field.*
*Reason:* same argument §2.2 of `LOG-ROTATION` makes for the base. An undetected bit
flip in the semantics field either invents a mismatch that refuses a good log, or — the
direction that matters — turns a 1 into whatever this build happens to be and lets a
mismatched log through the gate silently. It is the number the gate rests on, so it
checks itself, and it costs nothing to fold into the CRC that is already there.

*Rule 7 — the set marker at the stem stays exactly as it is: 18 bytes, `OBWAL\x02`,
base 0.*
*Reason:* the marker holds no records, so there is no semantics for it to declare, and
[`LOG-ROTATION.md`](LOG-ROTATION.md) §2.5's downgrade argument is calibrated to those
exact bytes and that exact 1,329,747,777. Changing a file whose entire job is to produce
one specific error message, in order to add a field nothing reads, is churn with a
runbook line attached.

*Rule 8 — `SegMagic` and `SegHeaderBytes` keep their names, their values and their
meaning; `SegMagicV3` and `SegHeaderBytesV3` are added beside them.*
*Reason:* both are exported and frozen ([`COMPATIBILITY.md`](COMPATIBILITY.md): renaming
anything exported is breaking). The names end up mildly awkward — `SegMagic` is the one
that is no longer written — and that is the cost of the promise. The doc comments say
which is written and which is only read.

### 2.4 The snapshot field

```go
// EngineSnapshot gains:

// Semantics is matching.SemanticsVersion of the build that produced this state.
// Zero means a snapshot written before the stamp existed.
//
// It is excluded from Digest for the same reason WALSeq is: it is provenance, not
// book state. See docs/SEMANTICS-VERSION.md §2.4.
Semantics int `json:"semantics,omitempty"`
```

*Rule 9 — `EngineSnapshot.Digest()` zeroes `Semantics`, exactly as it zeroes `WALSeq`
(`pkg/matching/snapshot.go:310`).*
*Reason, and there are two, the second of which is fatal:*

First, the digest answers "is this the same book". Two engines whose books are
identical must compare equal even when one of them is a build ahead, or the digest stops
being usable for the thing it exists for — `docs/REPLICATION.md`'s primary-versus-
follower comparison, `restoreMatchesLive`, and the recovery tests' equality assertion.
A follower a build ahead of its primary *will* diverge, on the first command that
touches a changed path; the digest's job is to report the divergence when it happens,
and a digest that reports "different" while the two books are byte-identical is a digest
people stop reading.

Second — and this is the one that decides it — the fingerprint in §5 contains snapshot
digests. If `Semantics` were inside the digest, bumping the version would change the
fingerprint by itself, so **every bump would satisfy its own evidence**. The enforcement
would be circular: bump the number, the fingerprint moves, regenerate, done, and nobody
ever has to change any behaviour to justify a bump or explain a behaviour change to
avoid one. §5's Rule 22 (a bump with no observable change is refused) is unenforceable
unless this rule holds.

### 2.5 The four alternatives, and why each was rejected

**A release version.** Free, already exists, already in every artifact. Rejected in §1.1:
it refuses logs that would replay identically, on every upgrade, and the response to a
check that cries wolf is a permanent override.

**A digest of the fingerprint itself, as the stamp.** Genuinely attractive, because it
removes the thing §5 has to work hardest to guarantee: nobody can forget to bump a
number that is derived. Rejected on two grounds, and the second is decisive.

It is not orderable, so a refusal cannot say which direction the mismatch runs and an
operator cannot look up a range in the changelog. `3f9a…` and `c214…` are two names with
no relation between them.

And it couples the on-disk gate to the **test corpus**. The fingerprint is computed over
a fixed set of tapes; §5 actively wants those tapes widened whenever a behaviour is
found to be unprotected, and §11 predicts they will be. Under a derived stamp, widening
the corpus changes the number, which invalidates every log on every disk, for a change
that altered no behaviour whatsoever. Improving the test would break the venue. That is
a design that punishes exactly the maintenance it depends on.

**A stamp per record.** Rejected in Rule 3: eight bytes × 216 million records a day to
carry a constant, plus a field on frozen exported surface that lives in every archived
record's JSON forever.

**A sidecar file mapping segment → semantics.** Rejected by
[`LOG-ROTATION.md`](LOG-ROTATION.md) §2.4's argument against a sidecar manifest,
unchanged: a second durable artifact that can disagree with the directory after a crash,
that an operator can copy a set without, and that lies in exactly the same way the
filename alone does.

## 3. What recovery does on a mismatch

This is the section the slice stands or falls on.

### 3.1 The rule

*Rule 10 — `Recover` refuses if and only if it is about to APPLY a record from a segment
whose declared semantics is not this build's. A mismatched segment whose records are all
covered by the snapshot is read, CRC-verified, skipped, reported, and never refused.*

*Reason:* the gate belongs on the replay set because replay is the only thing a
semantics difference can damage. A segment behind the snapshot boundary contributes
nothing to the recovered book — `RestoreAfter` drops it by sequence whatever it contains
— so refusing on it is refusing on a file that could be deleted with no effect. This is
not a new idea in this package: [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §5.2
already moved the "does this record decode" check with the snapshot boundary, for the
same reason and with the same consequence, and wrote out in full that it starts a venue
the old code refused to start.

The rule has a property that is worth stating as the design goal rather than as a
consequence: **a venue that followed the documented upgrade procedure starts with no
ceremony at all.** Checkpoint, archive, upgrade, restart — nothing left to apply from
the old segment, no refusal, no flag, no log line beyond the report. The refusal falls
on exactly two populations: a venue that crashed across the upgrade, and a venue
replaying an old log from an archive. Both are cases where the refusal is correct and
the alternative is a wrong book.

That property is what keeps the check credible. A check that fires on the happy path is
a check that gets switched off, and then it is not there on the day it matters.

### 3.2 Where the check runs, and why it is in two stages

*Rule 11 — sealed segments are gated BEFORE any record is read, from the directory and
the headers alone. The newest segment is gated after the walk, from its last complete
record's sequence.*

*Reason:* a sealed segment `Sᵢ` spans `[baseᵢ, baseᵢ₊₁ − 1]`, and both numbers come from
the 16-digit filenames without opening anything
([`LOG-ROTATION.md`](LOG-ROTATION.md) §2.1). So "does this segment contribute a record
past `snap.WALSeq`" is answerable in the same directory read that already does the gap,
overlap and floor checks, and a mismatch is refused in milliseconds instead of after
reading gigabytes. It joins §4.4's table of startup validations and gets the same
treatment.

The newest segment cannot be gated that way, because its span has no upper bound in the
directory — its last sequence is only known once its records have been walked. Gating it
optimistically ("the newest segment always contributes") would refuse in exactly the
`SnapshotAhead` case, which is reachable after an ordinary power loss
([`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §4.1: the checkpoint does not sync the log
first, so a snapshot can legitimately be ahead of the log's last record). That is a
false refusal on a benign crash, which is the noise §3.1 exists to avoid. So the newest
segment waits for `walk.lastSeq`, which the walk already computes. Both stages run before
`RestoreEngine` and before a single command is applied.

### 3.3 What an operator does at 3am, for each candidate answer

| Answer | 3am | What it costs when it is wrong |
|---|---|---|
| **Warn and continue** | Venue starts. The book is wrong. Nobody looks at the line, because the venue started | A wrong book, discovered by a client dispute days later, with no way to reconstruct what should have happened. This is today's behaviour plus a false sense of coverage |
| **Always refuse** | Venue will not start. The log is often fine. The operator's only route is to delete files or edit bytes during an incident | An outage manufactured by a check. Then the flag goes in the unit file permanently and the check is gone for good — the worst outcome of the three, because it looks like the safe one |
| **Refuse the replay set, with a named override** (this) | Venue will not start *only if* records from a different matcher would be replayed. The message names both versions, the segment, the sequence range and the record count, and prints two commands | An operator who overrides without reading gets today's behaviour, having been told. The residual risk is real and §11 says how it would show up |

### 3.4 The refusal, written out

Ergonomics at 3am is a design requirement, not polish. The message names both numbers,
the file, the sequences, the count, and both routes forward — the safe one first.

```
wal: matching semantics mismatch: this build matches at semantics 2; records this
recovery would replay were written at semantics 1.

  segment  BTC-USD.wal.0000000000610422   semantics 1
           sequences 610,422..741,003, of which 130,581 would be replayed
           (the snapshot covers through 610,421)

Replaying them under this build produces a book the venue that wrote them never
had. What changed between semantics 1 and 2 is in CHANGELOG.md; docs/SEMANTICS-
VERSION.md §1.2 has the registry.

Two ways forward, safest first:
  1. Start the PREVIOUS build once, let it checkpoint, stop it cleanly, then
     upgrade. Recovery then starts from a snapshot that build agreed with and
     has nothing left to replay.
  2. Accept the replay deliberately:  -wal-accept-semantics 1

See docs/RUNBOOKS.md "Upgrading across a semantics change".
```

The sentinel is `wal.ErrSemanticsMismatch`, distinct from `ErrCorrupt` and `ErrLogGap`
for the same reason those two are distinct from each other: corruption is bytes that
changed, a gap is files that are missing, and this is bytes that are intact and files
that are all present and a *meaning* that moved. Three conditions, three sentinels,
three runbook sections.

### 3.5 The override, and why it names a version instead of being a boolean

```go
// pkg/wal

// RecoverOptions are the deliberate deviations from a default recovery. The zero
// value is the default and is what Recover and RecoverWithReport use.
type RecoverOptions struct {
    // AcceptSemantics lists the semantics versions whose records this recovery
    // will replay in addition to this build's. Naming zero accepts records from
    // a log written before the stamp existed.
    AcceptSemantics []int
}

func RecoverWithOptions(config matching.Config, snapPath, walPath string, opts RecoverOptions) (*matching.Engine, RecoverReport, error)
```

`Recover` and `RecoverWithReport` keep their exact signatures and become this function
with the zero options; `cmd/obgw` grows `-wal-accept-semantics`, a comma-separated list.
All three additions are additive and non-breaking under
[`COMPATIBILITY.md`](COMPATIBILITY.md)'s rule.

*Rule 12 — the override names the versions it accepts. There is no boolean and no
wildcard.*
*Reason:* this is the single most important detail in §3, and it is about what happens
in six months rather than tonight. `-wal-semantics-mismatch-ok` gets added to a unit
file during one incident and stays there for the life of the deployment, and the next
mismatch — a different mismatch, for a different reason — is accepted silently by a flag
nobody remembers. `-wal-accept-semantics 1` stops working the moment the number moves
again, and it fails with a message naming the version it accepts and the version the log
actually carries. The decision has to be re-made because the artifact of the last
decision has gone stale, which is the same device `internal/apicheck` uses when it makes
you regenerate a golden and read the diff.

*Rule 13 — the override applies to the log gate and nothing else.* It does not relax
`ErrCorrupt`, `ErrLogGap`, the floor check, or any structural validation. An operator
reaching for it during an incident must not be able to acquire a second, unrelated
permission by accident.

### 3.6 `Open`, `ReadAll`, `Restore` and `RestoreAfter` never refuse

*Rule 14 — the semantics gate exists in `Recover` alone.*
*Reason:* [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §9.1 settled this. `cmd/obgw`
calls `Recover` and then `Open` on the same path (`server.go:370` and `:424`), so a
stricter `Open` means a venue that recovers its book successfully and then cannot open
the log it just recovered from — an outage manufactured by two readers of the same bytes
disagreeing. `ReadAll` is the diagnostic reader `RUNBOOKS.md` sends an operator to when
a client and the venue disagree about an order, and a diagnostic that refuses to show
you the file during an incident is not a diagnostic. `Restore` and `RestoreAfter` take
entries, not files; there is no header to consult by the time they run.

What `Open` does instead is §3.7, and it is more useful than refusing.

### 3.7 `Open` seals the old segment instead of appending behind the stamp

*Rule 15 — when the active segment's declared semantics is not this build's, `Open`
seals it and starts a new one before appending anything.*

*Reason:* without this the stamp is a lie in the one direction that matters. A venue
upgrades, restarts, and `Open` resumes the newest segment — which was created by the
previous build and declares the previous semantics — and every record this build writes
lands in it. The segment now claims semantics 1 and contains semantics-2 records. The
next crash recovery reads the header, believes it, and refuses a tail that is in fact
perfectly replayable; or, with the override in hand, replays semantics-1 records it
should have refused. Either way the header no longer describes its contents, which is
the only thing it was for.

It is also what makes the condition **self-healing**. The alternative — leave the segment
alone and let the stamp be conservative — means a venue that upgraded correctly hits the
refusal on every crash recovery until the segment fills, which at the 128 MiB default is
a long time to be one power loss away from an outage. With Rule 15 the set is correct
from the first restart after the upgrade, permanently, and the refusal only ever names
segments the old build really did write.

The cost is one rotation per upgrade: measured at 12.4 ms
([`LOG-ROTATION.md`](LOG-ROTATION.md) §12.5), one extra inode, one 22-byte header, once.
A venue flapping between two builds pays it on each restart, which is bounded and
visible in `RecoverReport` and in the rotation counter.

*Rule 16 — an active segment that declares a different semantics and holds NO records is
replaced in place by an atomic rename, not rotated.*
*Reason:* rotation starts the next segment at `last + 1`, and an empty segment's `last`
is `base − 1`, so the new segment would claim the same base as the old one and collide
with its own filename — `EEXIST` at exactly the moment a venue is trying to start. There
is nothing to preserve in a file with no records, so the fix is a fresh 22-byte header
written to a temp file and renamed over it, which is the same crash-atomic swap
`WriteSnapshot` already uses. It is a special case and it looks like one; it is here
because the general path is arithmetically impossible on that input.

*Rule 17 — a legacy stem (`OBWAL\x01` or headerless v1) is migrated to segment 1 by the
existing Rule 13 of `LOG-ROTATION` §7, and then sealed by Rule 15 above, in the same
`Open`.*
*Reason:* a legacy segment declares no semantics, so it is unknown, so Rule 15 applies
to it like any other mismatch. The v1 file's bytes are never touched, its framing never
changes, and this build's records go into a new checksummed segment beside it. This is
the `Writer` doc comment's existing sentence — *"Rotate to get checksums on an old
file"* — becoming true a second time, for a second reason. §7.2 says what it does to
`TestLegacyLogStillRecovers` and why the replacement assertion is stronger rather than
weaker.

## 4. Pre-stamp logs, which carry no version at all

An `OBWAL\x02` segment, an `OBWAL\x01` file and a headerless v1 file all declare nothing.
Three answers were available.

**Unknown means "compatible".** Detection is off for every log that exists on any disk
today, which is the entire installed base, and — worse — the assumption is affirmatively
false *right now*: the three changes in §1 mean a pre-stamp log genuinely does replay
into a different book under this build. Shipping a stamp whose first act is to assert a
falsehood, and to keep asserting it in every future release, is the "promises detection
it does not provide" failure this document opened by naming. Refused.

**Unknown means "refuse the file".** Strands every venue that has one, including venues
with nothing left to apply, which is the strawman §3 was written to avoid.

**Unknown means unknown, and is gated by the same rule as any other mismatch.** This is
the answer.

*Rule 18 — semantics 0 is a distinct value that is never equal to any version, including
itself.* An unstamped segment matches nothing; it is refused when its records would be
replayed and accepted when they would not; it is accepted by `-wal-accept-semantics 0`
and by nothing else.

*Reason it is the least bad:*

- **It is true.** It is the only one of the three that does not require asserting
  something about a file that cannot be known from the file.
- **Its cost is bounded and one-time.** It lands on the single upgrade that introduces
  the stamp, and only on venues with unreplayed records at that moment. After the first
  checkpoint under the new build, every segment in the replay set is stamped, and the
  condition cannot recur.
- **The remedy is the procedure the changelog already prescribes.** "Checkpoint and
  archive before upgrading" stops being advice and becomes the thing that makes the
  venue start. The refusal is the advice, enforced, at the only moment it can be.
- **The alternative is permanent.** A wrong assumption about unknown is baked into every
  future release and can never be revisited, because by then the number is load-bearing.
  A one-time operational cost is recoverable; a false premise in a durability check is
  not.

Two consequences are worth stating rather than discovering.

**`cmd/obgw` does not checkpoint on clean shutdown** (`server.go:586`: it drains, closes
the runners, syncs and closes the logs). So a clean stop still leaves up to one
checkpoint interval — 30 s by default — of records past `snap.WALSeq`, and those records
are in an unstamped segment. **The ordinary upgrade path therefore hits this refusal
once.** That is a real cost, it lands on everyone, and it is not hidden here: §6 records
that a final checkpoint on shutdown would remove it, and why that is not in this slice.
The release notes and `RUNBOOKS.md` carry the two-line procedure.

**An old build ignores the stamp completely, because it must.** A build that predates
this slice reads a snapshot's `semantics` field as an unknown JSON key and drops it. No
version stamp can protect against builds written before the stamp; the protection begins
at the release that introduces it and runs forward only. §6 lists this as a limit rather
than a gap, because there is no version of this design that does not have it.

## 5. The enforcement

A rule nobody is forced to follow is a rule the third person to touch `matching` breaks,
and this slice is then worse than not doing it — a number that says "these replay
identically" while they do not is more dangerous than no number, because recovery now
starts *confidently*.

So the number is not maintained by discipline. It is maintained by a test that fails.

### 5.1 What the pattern is copied from

`internal/apicheck` freezes the exported surface of `pkg/...` into
`testdata/surface.txt` and fails when it moves. It does not prevent a breaking change
and does not try; it makes one impossible to ship without a human reading a diff that
says `REMOVED or CHANGED`. Regeneration is `APICHECK_UPDATE=1` and the reading of the
diff is explicitly the point.

`internal/semcheck` does the same job for matching **behaviour** instead of matching
**signatures**, with one addition apicheck does not need: regeneration is gated on the
version constant having moved.

### 5.2 Why the differential harness cannot be the forcing function

The obvious idea is to let `TestDifferentialTape` do this — it already compares the
engine against `internal/refmatch` after every command, and a behaviour change turns it
red. It cannot, for a reason [`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §2.2 already
records and the changelog repeats under *Known limitation*: **the model is edited in the
same commit as the engine.** It has to be — three of tonight's fixes required exactly
that, and the pinned tests name the model edit a fix must make alongside the engine one.
A harness whose oracle is updated by the same person in the same change cannot detect
that the change happened.

The two ask different questions and both are needed:

| | Question | Oracle | Updated by |
|---|---|---|---|
| `TestDifferentialTape` | Is the engine **right**? | `internal/refmatch` | the same commit |
| `internal/semcheck` | Did the engine **change**? | the previous release's recorded behaviour | only with a version bump |

`EngineSnapshot.Digest()` is the repository's existing notion of "same state" and is a
component of the fingerprint, but it cannot be the whole of it: it sees only what the
snapshot carries. A change that rejects a command instead of accepting it and
immediately cancelling it leaves the same book, the same digest, and a completely
different event stream and verdict — which is defect B from
[`DIFFERENTIAL-FINDINGS.md`](DIFFERENTIAL-FINDINGS.md) §4, almost exactly. So the
fingerprint is observation-based, with the digest inside it.

### 5.3 The fingerprint

`internal/semcheck` drives a fixed corpus of tapes through `matching.Engine` — through
the **public** API only, with a deterministic clock — and renders one line per command:
the command, the verdict, the trades field by field, the published events in order with
their payloads, the state, the last trade price, best bid and best ask, both next-id
counters, and `EngineSnapshot.Digest()` after the command. The rendering is
human-readable, in the shape of `surface.txt`, because a diff that shows *which command's
outcome moved* is what turns "the fingerprint changed" into "the pro-rata walk now
cancels the taker", and that reading is the whole point.

The corpus is three things:

1. **The differential profiles** (`tape.Differential`, `tape.ProRata`) at their committed
   seeds — tier 1, pinned against accidental drift by
   `TestDifferentialTapeIsPinned` (`differential_guards_test.go:440`) so the *input*
   cannot move without somebody updating a golden of its own.
2. **The recovery profile** (`tape.Recovery`), which carries phase transitions and
   therefore the opening and closing uncrosses, and the full tier-1 order payload.
3. **A hand-written tier-2 script**, because generated tapes do not reach stops, OCO,
   icebergs, trailing stops, pegged orders, busts, mark prices, band breaches or expiry
   — and every one of those is decided behaviour that a change could move.

A closing block records aggregate counts: prints, rejections by reason, events by kind,
STP decisions by mode, auction uncrosses, triggers, refills, busts. A coverage
regression that leaves individual lines untouched still shows up in the diff.

*Rule 19 — the fingerprint is computed from the public API only.*
*Reason:* it must see what a consumer sees and nothing else. A fingerprint with access to
internals would move on refactors that no client could observe, which is the false-alarm
failure mode; and the residual — a change that is invisible to every consumer at the
command where it happens — is caught anyway at the later command where it becomes
visible, because the fingerprint runs over whole tapes rather than isolated commands.

### 5.4 The four outcomes, and the one that has teeth

*Rule 20 — a body diff with an unchanged version FAILS, and the failure does not offer
regeneration as the first option.* The message says: matching behaviour changed and
`SemanticsVersion` did not; either this change was not supposed to alter behaviour — in
which case find out why it did, and the diff above says where — or it was, and the
number has to move.

*Rule 21 — `SEMCHECK_UPDATE=1` refuses to write the golden unless
`matching.SemanticsVersion` is strictly greater than the version recorded in it.*
*Reason:* **this is the tooth.** Every other rule here is a warning; this is the one that
cannot be walked around. The path of least resistance for someone who has changed
behaviour and hit Rule 20 is to regenerate, and regeneration is precisely where the bump
is demanded. There is no sequence of commands that produces a green tree with changed
behaviour and an unchanged number.

*Rule 22 — a version greater than the golden's with an IDENTICAL body also fails.*
*Reason:* this looks backwards and it is the rule §1.1 exists to support. A bump that
changes nothing observable is a false alarm, false alarms are what teach operators to
put `-wal-accept-semantics` in the unit file, and a stamp that fires on every release is
exactly the useless version this document opened by rejecting. The route forward when
you believe you changed behaviour the corpus cannot see is to **extend the corpus until
it can** — and if no tape can be written in which the change is observable, then by §2.1's
definition it is not a semantics change and does not need a bump.

*Rule 23 — the constant must be wired, not merely declared.* A test writes a segment and
a snapshot with the real code paths and asserts both carry `matching.SemanticsVersion`.
A stamp that is a literal in `segHeader` diverges from the constant the first time the
constant moves, and every test in §5 would still pass.

### 5.5 The alphabet guard, and the part that is still human

Rule 22 makes the corpus load-bearing: a change on a path the corpus never reaches
cannot be bumped for. So the corpus needs a guard of its own, and the repository already
has the pattern — `entryKindCount` exists so `TestEveryEntryKindReplays` can enumerate
the block rather than trust a hand-written list, and
`TestRecoveryTapeSpeaksTheTierOneAlphabet` asserts an alphabet **by outcome** rather than
by draw.

*Rule 24 — the fingerprint run asserts, by outcome, that it reached every value of every
axis of behaviour that can be enumerated by the compiler, and every value of a declared
list for the axes that cannot.* Reached, not drawn — an order type that is generated and
never fills proves nothing.

The two halves are not equally strong and pretending otherwise is how a guard reports
coverage it does not have.

**Compiler-enumerable, therefore mechanical.** `EntryKind` is a `uint8` iota block with
the `entryKindCount` sentinel already sitting at the end of it for exactly this purpose,
so "every `EntryKind` was replayed" is a loop and a new kind fails the guard the moment
it is declared. `EventKind` is a `uint8` iota block **without** a count sentinel; this
slice adds one, in the same shape and with the same comment, and the guard skips
`EventBookDelta` by name because it is declared, deprecated and deliberately never
emitted (`event.go:65` and `:130`) — a named exception with a citation, not a
catch-all.

**Declared-list, therefore only as good as the list.** `types.OrderType`,
`types.TimeInForce` and `matching.SelfTradePrevention` are **string** types
(`order.go:22`, `:32`, `engine.go:32`), so there is no set the compiler can iterate and
no sentinel that can be added to one. The guard enumerates a list written in
`internal/semcheck`, and a new value declared in `pkg/types` is invisible to it until
somebody adds a line. That is a real hole, it is the same hole the wire codec and the
alphabet guards have, and it is stated here rather than left for a review to find.

**Not enumerable at all.** Behaviour that is not enum-shaped — how the collar computes
its reference, iceberg refill jitter, the trailing-stop ratchet, auction tie-breaking.
Those live in the hand-written tier-2 script, and nothing forces someone adding one to
add a scenario.

So the residual "somebody must remember" is reduced from *remember to bump a number* to
*remember to add a case to a list that is read in review*. That is a much smaller ask,
and unlike a number it is visible in a diff as a missing thing. It is not zero, and §11
says what it looks like when it fails.

### 5.6 What the number is worth even where §5.5's boundary bites

Stated plainly, because the temptation is to claim more. Where enforcement reaches, the
number is a guarantee: behaviour cannot change without it moving. Where enforcement does
not reach — a tier-2 path nobody added a scenario for — the number degrades to exactly
what the changelog gives you today, and the *log* is no worse off than it is now, because
a mismatch that goes unstamped is a mismatch that goes undetected, which is the status
quo. The stamp never makes a case worse than it is today; it makes a large and growing
class of cases refuse instead of lie.

## 6. What this deliberately does not do

- **It does not stamp the engine CONFIGURATION, and that is the biggest hole left open.**
  Two builds at the same semantics version with different `Config` produce different
  books from the same log — `ProRata`, `SelfTradePrevention`, `MaxOrders`, `PriceBand`,
  the collar, the shard index. `EngineSnapshot` carries no configuration at all, so a
  venue that flips `ProRata` between restarts replays its entire log under the other
  allocation policy and **nothing anywhere notices**, before or after this slice. That is
  arguably a live defect larger than the one this closes; it is a different design (a
  configuration fingerprint in the snapshot, and a decision about which fields are
  replay-relevant) and it is named here so it is not mistaken for something this covers.
- **It does not protect against builds that predate it.** An older build ignores the
  snapshot field and refuses a `\x03` segment as a corrupt v1 record. That is the best
  available and it is not detection. §4 says why every version stamp has this shape.
- **It does not say WHAT changed.** The number is an identity, not a description.
  §1.2's registry and the changelog are the description, and the refusal message links
  to both. A stamp that tried to encode which behaviours changed would be a schema
  nobody could extend without another format bump.
- **It does not version the snapshot schema, the record format, `wal.Entry`, or the
  wire protocol.** Those are format questions with format answers, and §1.1's table is
  the whole of what this document has to say about them.
- **It does not gate replication.** A follower on a build the primary is not on is not
  detected here: `examples/replication` ships records, not segments, and there is no
  handshake to carry a version. The digest will report the divergence once it happens,
  which is later than it should be. That belongs with [`REPLICATION.md`](REPLICATION.md).
- **It does not migrate, rewrite or re-stamp an existing log.** A semantics-1 segment
  stays semantics 1 forever, including in the archive. Rewriting a journal to make it
  agree with a build is the one operation this package must never learn.
- **It does not checkpoint on clean shutdown**, which is what would remove the one-time
  cost §4 lands on every venue. It is a change to `Server.Close`'s contract — a
  checkpoint can fail, it takes O(book), and a SIGTERM with a deadline would have to
  choose between finishing it and honouring the deadline — and that deserves its own
  decision rather than being smuggled in behind a version stamp.
- **It does not make the refusal per-symbol.** A multi-symbol venue is one process with
  one log per book; one book refusing fails `NewServer` and the whole gateway does not
  start. That is the right default (a venue serving three of its eight instruments is
  worse than one serving none) and the override is venue-wide rather than per instrument,
  which is coarser than it should be. Named, not fixed.
- **It does not detect a locally patched build.** A fork that changes `matchProRata` and
  leaves the constant alone produces logs that lie, and no amount of test-time
  enforcement in this repository can reach a build compiled elsewhere. §2.5's derived
  digest would have — and would have cost the two things §2.5 says it costs.
- **It does not make the number fine-grained.** A change that only affects pro-rata
  bumps the version for every FIFO venue too, and refuses their logs for a change that
  cannot have touched them. That coarseness is deliberate: a per-policy version is a bet
  that the mapping from configuration to affected behaviour is fully known, and the
  pro-rata finding is exactly a case where it was not. The cost is a false refusal that
  the changelog can explain and the override can clear.

## 7. Backward compatibility

Everything the current build recovers must still recover, and the tests that encode that
must keep every assertion they have.

### 7.1 What is unchanged, and passes as written

- **An `OBWAL\x02` segment set.** Read exactly as today, declaring semantics 0. Header
  parsing, base cross-check, contiguity, floor, torn tails: untouched.
- **An `OBWAL\x01` single file** and **a headerless v1 log.** Implicit base 1, semantics
  0, every `BOUNDED-RECOVERY.md` §3.2 rule about v1's decode-only integrity signal
  intact.
- **Snapshots with and without `SnapMagic`.** `ReadSnapshot` is unchanged; a snapshot
  without the magic is read without a checksum exactly as today and reports semantics 0.
- **`ReadAll`, `Restore`, `RestoreAfter`, `Recover` and `RecoverWithReport` signatures.**
  Additive only.
- **Every `pkg/wal` test whose log is written by this build's `Writer`**, which is nearly
  all of them: those segments are stamped with this build's version, the gate passes,
  and nothing changes.

### 7.2 What changes, and why each is stronger rather than weaker

*This list is exhaustive on purpose. "Do not weaken an assertion" is a constraint that
gets honoured by enumeration, not by intention.*

**Tests that hand-build a pre-stamp log and then recover a tail from it** must pass
`AcceptSemantics: []int{0}` at the call site. They are:
`TestLegacyLogStillRecovers` (`integrity_test.go:194`),
`TestV1LogRecoversIdenticallyBehindASnapshot` (`bounded_recovery_test.go:664`),
`TestV1UndecodableRecordBehindTheSnapshotStillStops` (`:703`),
`TestAFileThatDeclaresBaseOneAndCarriesSequence1001FallsBack` (`:488`),
`TestOnlyTheFirstRecordDisagreeingStillFallsBack` (`:529`), and
`TestASequenceThatIsNotItsOrdinalAfterTheBoundaryFallsBack` (`:562`).

Every one of those tests is about **framing or sequence arithmetic**, not about
semantics. Naming the option at the call site keeps all of their assertions, adds a
visible statement of what the test is deliberately not testing, and is the same
legitimate move [`LOG-ROTATION.md`](LOG-ROTATION.md) §7 made when it re-expressed
`Seq == ordinal` as `Seq == base + ordinal − 1`. The illegitimate alternative — making
unknown compatible so the tests do not have to change — is refused in §4 and is the
change this whole slice exists to prevent someone making quietly.

**`TestLegacyLogStillRecovers`'s append half changes shape and gets stronger.** It
currently asserts `!w.Checksummed()` after `Open`, with the comment "appending must keep
the file's own framing, not switch mid-file to a format no reader could then parse".
Under Rule 17 the v1 file is sealed and the append goes to a new `\x03` segment, so the
proxy stops matching the property. The property is preserved and improved — framing
still never changes mid-file, and now it cannot — so the assertion becomes, and must
become, strictly stronger:

- the v1 segment's bytes are **byte-identical** before and after the append (today's test
  cannot say this at all);
- the appended record is **not** in the v1 segment;
- `ReadAll` over the set still returns 5 entries and the fifth still carries `Seq == 5`
  (both unchanged, both pass as written, because `ReadAll` reads the set);
- `w.Checksummed()` is now true, and that is the `Writer` doc comment's *"Rotate to get
  checksums on an old file"* becoming true rather than a regression.

**Byte-surgery helpers** that construct `\x02` segment headers gain a semantics argument,
in the same way [`LOG-ROTATION.md`](LOG-ROTATION.md) §7 required them to gain a
"which segment" argument. Mechanical.

**`internal/apicheck/testdata/surface.txt` is regenerated deliberately**, and the diff
must contain these and nothing else:

```
ADDED:
  + matching.SemanticsVersion
  + matching.EngineSnapshot.Semantics
  + wal.SegMagicV3
  + wal.SegHeaderBytesV3
  + wal.ErrSemanticsMismatch
  + wal.RecoverOptions
  + wal.RecoverOptions.AcceptSemantics
  + wal.RecoverWithOptions
  + wal.RecoverReport.Semantics
  + wal.RecoverReport.SnapshotSemantics
  + wal.RecoverReport.LogSemantics
  + wal.RecoverReport.SemanticsAccepted
```

All additions, no removals, no interface methods — a minor release under
[`COMPATIBILITY.md`](COMPATIBILITY.md)'s rule. **A `REMOVED or CHANGED` line in that diff
means something in this slice went further than it was supposed to and is a stop.**

**`cmd/obgw` gains `-wal-accept-semantics`** and logs the semantics facts beside its
existing "recovered N resting orders" line. Flags on `cmd/...` are outside the
compatibility promise, and this one is additive anyway.

## 8. Deliverables and acceptance criteria

| # | Deliverable | Done when |
|---|---|---|
| 1 | The constant and the registry | `matching.SemanticsVersion == 1`; §1.2's table has a row for 0 and for 1, each naming the changelog entries it covers |
| 2 | The segment header | `TestASegmentDeclaresItsSemantics`: a freshly written set's newest segment is `OBWAL\x03`, 22 bytes of header, semantics 1, CRC over the 12 bytes; a flipped bit anywhere in those 12 is `ErrCorrupt` |
| 3 | The snapshot field | `TestASnapshotDeclaresItsSemantics`; and `TestTheDigestIsBlindToTheStamp`: two snapshots differing only in `Semantics` have equal digests |
| 4 | The gate refuses what it must | `TestARecordFromAnEarlierSemanticsIsRefused`: a two-segment set, the older stamped 0 or 1, a snapshot inside it, `Recover` returns `ErrSemanticsMismatch` naming both versions, the segment, the sequence range and the count — and returns **no engine** |
| 5 | The gate accepts what it must | `TestAMismatchedSegmentBehindTheSnapshotStarts`: the same set with the snapshot past the mismatched segment's last sequence recovers to the **same digest** the all-matching set produces, with `RecoverReport.LogSemantics` naming both versions |
| 6 | The newest segment is decided after the walk | `TestASnapshotAheadOfAMismatchedNewestSegmentStarts`: `snap.WALSeq` beyond the log's last record, newest segment stamped 0 — recovers, applies nothing, `SnapshotAhead == true`, no refusal |
| 7 | A mismatch in the middle is caught | `TestAMismatchedSegmentInTheMiddleOfTheReplaySetIsRefused`: segments stamped 1, 0, 1 with the snapshot below all three |
| 8 | Unknown is not this build's | `TestAnUnstampedSegmentWithRecordsToApplyIsRefused`, on all three pre-stamp shapes: `\x02`, `\x01`, headerless v1 |
| 9 | The override works and is narrow | `TestAnOverrideNamesTheVersionItAccepts`: `AcceptSemantics: []int{0}` starts the venue on the unstamped log and produces the same digest as the full replay; the same option against a segment stamped 1 still refuses; and the option does not relax `ErrCorrupt` or `ErrLogGap` |
| 10 | `Open` never refuses | `TestOpenAcceptsWhatRecoverRefuses`: the deliverable-4 fixture opens successfully and appends, per `BOUNDED-RECOVERY.md` §9.1 |
| 11 | Upgrading seals the old segment | `TestUpgradingSealsTheOldSegment`: `Open` against a set whose newest segment declares a different semantics creates a new segment before the first append; the old segment's bytes are unchanged; contiguity holds |
| 12 | The empty-segment case | `TestAnEmptyMismatchedSegmentIsReplacedNotRotated`: no `EEXIST`, the base is unchanged, the header is the new one, and a crash injected mid-replace leaves a valid set |
| 13 | Self-healing | `TestAfterAnUpgradeACrashRecoveryNoLongerRefuses`: upgrade, `Open`, append, kill, recover — starts clean with no override, and does so on every subsequent crash |
| 14 | The fingerprint exists and is stable | `internal/semcheck` renders the corpus; `TestTheFingerprintIsDeterministic` runs it twice in one process and requires identity (the `TestSameSeedSameObservations` device, against a clock or a map leaking in) |
| 15 | **A behaviour change without a bump fails** | `TestMatchingSemanticsAreFrozen` fails under sabotage 1 of §10, with a diff naming the command whose outcome moved |
| 16 | **Regeneration demands the bump** | `SEMCHECK_UPDATE=1` refuses to write while `SemanticsVersion` is unchanged, and says so |
| 17 | A bump nothing can see fails | `SEMCHECK_UPDATE=1` with a bumped constant and an identical body refuses, and names §5.4 Rule 22 |
| 18 | The stamp is wired | `TestTheStampIsWired`: the segment header and the snapshot both carry `matching.SemanticsVersion`, read back through the real write paths |
| 19 | The corpus reaches what it claims | `TestTheFingerprintReachesEveryDecidedBehaviour`: every `EntryKind` (via `entryKindCount`), every `EventKind` except `EventBookDelta` (via the new sentinel), every `types.OrderType`, `types.TimeInForce` and STP mode from §5.5's declared list, both allocation policies, and every rejection reason reachable on the tier-1 path — all asserted **by outcome** |
| 19a | The `EventKind` sentinel exists | `eventKindCount` is declared, unexported, last in the block, and carries the comment saying why — the `entryKindCount` treatment (`wal.go:115`) applied to the second enum that needed it |
| 20 | Backward compatibility | `go build ./...`, `go test ./... -race -count=1` green across every package; §7.1's list passes with its assertions intact; §7.2's list is exactly the set of tests that changed |
| 21 | Cost is what §2 claims | 4 bytes per segment and 0 per record, measured: `restart_cost_test.go`'s published `log-MiB` figure moves by at most 4 × (segments); `BenchmarkRecoverBehindACoveredChurnPrefix` median within 2% of the pre-change median; `internal/semcheck` runs in under 2 s |
| 22 | Prose | `CHANGELOG.md`'s "Stamping a version into both is the fix and is not done here" is replaced by what was done; `RUNBOOKS.md` gains "Upgrading across a semantics change"; `COMPATIBILITY.md` gains a line distinguishing this from the wire and release versions; `PRODUCTION-READINESS.md` and `LOG-ROTATION.md` §2.2 name the `\x03` header; `internal/apicheck/testdata/surface.txt` regenerated with the §7.2 diff and nothing else |

### 8.1 The numbers to record when it is done

Measured, not derived, and published in the section this document gains afterwards:

- Bytes added to a day's log at 2,500 messages/s: segments × 4, against 44 GiB.
- `Recover` median and allocation on `BenchmarkRecoverBehindACoveredChurnPrefix` at
  50k / 200k / 500k covered records, before and after.
- Time to refuse: how long `Recover` takes to return `ErrSemanticsMismatch` on a
  100-segment set whose second segment mismatches. §3.2's two-stage split is worth
  having only if this is milliseconds.
- `internal/semcheck` wall time and golden line count.
- The rotation cost paid at the upgrade `Open`, against `LOG-ROTATION.md` §12.5's
  12.4 ms.

## 9. Rules that will look like bugs

Every one of these has been read as a defect by someone during design. Each is the
behaviour the design chose.

| Looks like | Actually |
|---|---|
| A mismatched segment on disk and the venue started anyway | §3.1. The gate is on the replay set. That segment contributes nothing to the book; refusing on it is refusing on a file that could be deleted with no effect |
| `Digest()` ignores `Semantics`, so two engines a build apart can compare equal | §2.4. Including it would make every bump satisfy its own evidence — the enforcement would be circular — and would fire on a version skew that has not yet produced a divergence |
| Bumping the version with no behaviour change **fails the build** | §5.4 Rule 22. A version that changes when nothing changed is a version operators learn to override, and then the real mismatch is accepted by a flag nobody remembers adding |
| `Open` cheerfully opens a log `Recover` just refused | §3.6, and `BOUNDED-RECOVERY.md` §9.1. Two readers of the same bytes disagreeing is how a benign file becomes an outage |
| Upgrading rotates the log at startup with the segment nowhere near full | §3.7. Without it the active segment declares one semantics and holds records from two, and the header stops describing its own contents |
| An empty active segment is *replaced* rather than rotated | §3.7 Rule 16. Rotation would start the next segment at the same base and collide with its own filename |
| The override is a list of integers instead of a boolean | §3.5 Rule 12. A boolean survives the next bump and stops being a decision. A version goes stale and forces the decision to be made again |
| Unknown is not treated as version 1 | §4. It is not knowable from the file, and it is affirmatively false today — the three changes in §1 are exactly what a pre-stamp log did not have |
| `ReadAll` says nothing about semantics | §3.6. It is the diagnostic reader an operator reaches for during an incident, and a diagnostic that refuses to show you the file is not one |
| The differential harness is red and `semcheck` is green, or the reverse | §5.2. "Is the engine right" and "did the engine change" are different questions with different oracles, and only one of them has an oracle the same commit is allowed to edit |

## 10. Sabotage runs required before this counts as done

Each is a deliberate break, run to confirm the named deliverable actually fails. This
repository has had a test pass against its own sabotage before
([`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §9.2,
[`LOG-ROTATION.md`](LOG-ROTATION.md) §12.4), which is the only reason §8's claims are
worth anything.

1. **Revert change C** — make `matchProRata` skip the taker's own liquidity again.
   Deliverable 15 must fail, and the diff must name a specific command in the
   `prorata` corpus whose outcome moved. *This is the central sabotage: it re-creates the
   exact change that opened the gap.*
2. **Revert change C and bump the constant, without regenerating.** Deliverable 15 must
   still fail, and its message must now point at regeneration rather than at the change.
3. **Revert change C, bump, and regenerate.** The tree goes green — and deliverable 4
   must still pass, proving the fingerprint half and the on-disk half are independent and
   that neither was accidentally implemented in terms of the other.
4. **Bump the constant with no behaviour change and regenerate.** Deliverable 17 must
   refuse the write.
5. **Regenerate after a behaviour change without bumping.** Deliverable 16 must refuse
   the write. *If this one passes, the slice has no enforcement and nothing else in §8
   matters.*
6. **Write a literal `1` into `segHeader` instead of `matching.SemanticsVersion`.**
   Deliverable 18 must fail.
7. **Gate on the whole set instead of the replay set.** Deliverable 5 must fail. This
   guards the direction nobody tests for: over-refusal, which is what produces the
   permanent override.
8. **Gate on the newest segment only.** Deliverable 7 must fail.
9. **Make semantics 0 compare equal to this build's version.** Deliverable 8 must fail on
   all three pre-stamp shapes.
10. **Delete `Semantics` from `Digest()`'s normalisation.** Deliverable 3 must fail — and
    check the second consequence too: with the field in the digest, sabotage 4 stops
    being refusable, because the bump moves the fingerprint on its own.
11. **Delete the `Open`-time seal (Rule 15).** Deliverable 11 must fail, and deliverable
    13 must fail on the *second* crash, which is the one that shows the condition is
    permanent rather than transient.
12. **Make the override a boolean that accepts anything.** Deliverable 9's second half
    must fail: an override naming 0 must not accept a segment stamped 1.
13. **Drop the tier-2 script from the corpus.** Deliverable 19 must fail. Then, with it
    dropped, change a stop-trigger comparison and confirm deliverable 15 **passes** —
    which is §5.5's boundary demonstrated rather than asserted, and the number that
    belongs in the write-up.
14. **Skip the CRC over the semantics field (Rule 6).** Deliverable 2 must fail on a
    flipped bit at offset 14.

## 11. How this can fail, stated in advance

So §8's write-up is not graded on a curve.

- **The one-time cost of §4 could be worse than predicted.** `cmd/obgw` does not
  checkpoint on shutdown, so every venue meets the refusal on the upgrade that introduces
  the stamp. If that turns out to be painful in practice rather than in theory, the fix
  is the shutdown checkpoint §6 defers, and it should be taken as a follow-up rather than
  by weakening §4 — the weakening would be permanent and the pain is not.
- **The fingerprint could be too sensitive.** A refactor that changes id allocation order
  or event ordering with no consequence any client can act on would still move the golden
  and demand a bump, and a bump nobody can explain is precisely the false alarm Rule 22
  exists to refuse. The two rules pull against each other on purpose. If false bumps
  start happening, the answer is to narrow what the observation records — not to loosen
  Rule 21, which is the only tooth in the design.
- **The tier-2 script could rot.** Nothing forces someone adding a new conditional-order
  behaviour to add a scenario, and §5.5 says so. It would show up as a semantics change
  that shipped without a bump and was found later by a differential finding — the same
  way the three changes in §1 were found. That is a real regression to the status quo for
  that path, and the only defence is that the list is short, read in review, and named
  in this document as the thing to check.
- **Operators could learn to pass the override anyway.** Rule 12 makes it go stale; it
  does not make it impossible for a configuration-management template to carry a growing
  list of accepted versions. If `RecoverReport.SemanticsAccepted` is routinely true in
  the field, the number has become a formality and the diagnosis is that §3's refusal is
  firing on cases §3.1 predicted it would not.
- **The coarseness in §6's last bullet could bite immediately.** The very first bump
  after this one is likely to be a pro-rata or an STP change, which will refuse every
  FIFO venue's log for a change that cannot affect it. That is the design working as
  specified and it will not feel like it. If it happens twice, the argument for a
  per-policy version deserves re-opening — with the pro-rata finding in hand as the
  reason it was refused the first time.
- **Rule 15's rotation could interact badly with retention.** Every upgrade adds a
  segment, and `MinSegments` counts sealed segments; a venue that restarts frequently
  accumulates small segments that retention's floor then declines to delete. Bounded and
  visible, but it is the kind of thing that is discovered on a disk-space alert rather
  than in a test, so §8's deliverable 21 should measure a set after ten simulated
  upgrades.

## 12. What building it changed

Written after the code, in the shape of [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §9
and [`LOG-ROTATION.md`](LOG-ROTATION.md) §12. Two things above turned out to be wrong
and one turned out to be incomplete; they are here rather than quietly fixed in place.

### 12.1 §7.2's list of changed tests was not exhaustive, and could not have been

The section says "This list is exhaustive on purpose", and it named six tests plus the
append half of `TestLegacyLogStillRecovers`. Building it needed more, for one reason the
design missed: **a fresh log had to start declaring a semantics too.**

`Open` on an empty path used to create a six-byte `OBWAL\x01` file at the stem. Under
§4's rule that an unstamped segment is refused when its records would be replayed, a
brand-new venue that crashed before its first checkpoint would have refused to start —
and §7.1's claim that "every `pkg/wal` test whose log is written by this build's Writer
... those segments are stamped" is only true if the fresh path stamps. So a fresh log now
opens with the full 22-byte header at the stem: still one file, still migrated to segment
1 at the first rotation, still `Checksummed()`, four bytes wider than `OBWAL\x02` would
have been and sixteen wider than the magic it replaces.

That moved every test that assumed records begin at `len(Magic)`. They were changed
mechanically, through one new helper — `recordsBegin`, which reads whichever of the three
header shapes a file carries — and the assertions they carry are unchanged. Two format
assertions were rewritten rather than moved and both got stronger:
`TestNewLogCarriesHeaderAndChecksums` now asserts the declared base and the declared
semantics as well as the magic, and `TestOpenWritesTheMagicIntoAnExistingEmptyFile`
asserts the header the empty file is repaired with.

One further test changed that §7.2 did not name:
`TestRotatingAV1LogKeepsTheWholeSetReadable`. Its `!w.Checksummed()` assertion is the
same proxy §7.2 already retired in `TestLegacyLogStillRecovers`, and it stops matching
for the same reason — Rule 17 seals the v1 file at `Open`. It got the same replacement:
the v1 segment's bytes are asserted **byte-identical** across the append, which the proxy
could not say, and the set still reads end to end. Its `Recover` assertion now needs
`AcceptSemantics: []int{0}`, and the test additionally asserts that WITHOUT the override
recovery refuses — an assertion added, not removed.

`handBuiltSegment` gained the semantics argument §7.2 predicted, as
`handBuiltSegmentAt(path, base, sem, seqs)` with the old signature kept as a wrapper
passing `matching.SemanticsVersion`. Every existing call site is untouched.

The `apicheck` diff is exactly §7.2's twelve additions and nothing else. No `REMOVED or
CHANGED` line.

### 12.2 The corpus was not reaching a boundary it claimed to cover

Sabotage 13 is the one that earned its place. With the tier-2 script present and a
stop-trigger comparison relaxed from `>=` to `>`, **the freeze stayed green** — a live
behaviour change, on a path the corpus reached, that moved no line.

The reason is worth stating because it generalises: the script *exercised* stops and
never *decided* one. Every stop in it was strictly through its trigger, and a comparison
is only decidable at equality. §5.5's boundary is about paths the corpus does not reach;
this was a path it reached and did not pin, which is a third category the section did not
name and now the run-book for writing scenarios has: **for every comparison, put a case
exactly on it.**

Two commands were added — a sell stop and a buy stop whose triggers equal a last traded
price the script pins deliberately, so an edit above cannot move them off the boundary —
and the sabotage is caught. It is the number that belongs in this write-up: the corpus
found one real hole in itself, and it found it by being attacked rather than by being
read.

### 12.3 The residual hole in the enforcement, named

`SEMCHECK_UPDATE=1` cannot refuse what it cannot compare against. **Deleting
`testdata/semantics.txt` and regenerating produces a golden at an unchanged version with
changed behaviour**, and no rule in §5 stops it. That path is the only supported way to
change the RENDERER without changing behaviour, so it cannot simply be closed; what it
gets instead is a log line at regeneration time saying the version rule was not enforced,
and the fact that a recreated golden is a whole-file diff in review. `internal/apicheck`
has the same shape of hole and does not have even the version rule, which is the
comparison to keep in mind rather than a defence.

### 12.4 The sabotage results

Fourteen runs, in a scratch copy, each reverted before the next. Every one behaved as
§10 required except where noted.

| # | Sabotage | Required | Result |
|---|---|---|---|
| 1 | Revert change C (pro-rata skips own liquidity) | deliverable 15 fails, naming a command | **Fails.** Diff names `prorata-shard7/0013`: `CANCELLED` → `NEW`, taker resting at `bid=100:7` |
| 2 | Revert C, bump, do not regenerate | fails, pointing at regeneration | **Fails**, message now says "moved from 1 to 2, which is the right half" |
| 3 | Revert C, bump, regenerate | tree green, deliverable 4 still passes | **Green** in `semcheck` and `pkg/wal`. (`TestDifferentialTape` is red, correctly: it answers a different question) |
| 4 | Bump with no behaviour change, regenerate | deliverable 17 refuses | **Refused**, and the plain run fails too |
| 5 | Regenerate after a behaviour change without bumping | deliverable 16 refuses | **Refused**, golden not written |
| 6 | Literal in place of the constant | deliverable 18 fails | **Fails** — after the test was strengthened to cover the rotation path as well as `Open`'s; the first version of it passed the sabotage |
| 7 | Gate the whole set, not the replay set | deliverable 5 fails | **Fails**, refusing on a segment with "0 would be replayed" |
| 8 | Gate the newest segment only | deliverable 7 fails | **Fails** |
| 9 | Unknown compares equal to this build | deliverable 8 fails | **Fails on all three pre-stamp shapes** |
| 10 | Delete `Semantics` from `Digest()` | deliverable 3 fails; sabotage 4 stops being refusable | **Both.** The no-change bump regenerates silently, which is the circularity §2.4 predicted |
| 11 | Delete the `Open`-time seal | deliverables 11 and 13 fail | **Fails**, and deliverable 13 fails on the FIRST crash as well as later ones |
| 12 | Boolean override that accepts anything | deliverable 9's second half fails | **Fails** |
| 13 | Drop the tier-2 script | deliverable 19 fails; then a stop-trigger change passes | **15 coverage failures**; and the stop change is invisible with the script dropped and caught with it present — see §12.2 |
| 14 | Skip the CRC over the semantics field | deliverable 2 fails on a flipped bit at offset 14 | **Fails at offsets 14 through 17** |

### 12.5 The numbers §8.1 asked for

Measured on the same machine as [`BENCHMARKS.md`](BENCHMARKS.md) (Apple M-series,
darwin/arm64, Go 1.23), each figure the median of three runs.

| | Before | After |
|---|---|---|
| `BenchmarkRecoverBehindACoveredChurnPrefix` `covered1000` | 3.60 ms, 0.3460 log-MiB | 3.01 ms, 0.3460 log-MiB |
| `covered50000` | 4.51 ms, 8.928 log-MiB | 4.42 ms, 8.928 log-MiB |
| `covered200000` | 13.49 ms, 35.35 log-MiB | 12.96 ms, 35.35 log-MiB |
| `covered500000` | 31.40 ms, 88.42 log-MiB | 33.06 ms, 88.42 log-MiB |
| allocations, all sizes | 2,366,960 B / 9,618 | 2,366,992 B / 9,619 |

**Bytes added to the log: none that the published figure can see.** The `log-MiB` metric
is identical to four significant figures at every size, which is the arithmetic working
out rather than a coincidence: four bytes per segment against a segment holding tens of
thousands of records. A day at 2,500 messages/s is about 350 segments, so the stamp costs
**1,400 bytes against 44 GiB** — one part in 33 million. The one extra allocation is the
`[]int` of distinct log semantics in `RecoverReport`.

Recovery time is inside run-to-run noise in both directions; the largest size is 5%
slower and the smallest 16% faster on the same code, which is what the spread of three
runs looks like at this scale rather than a signal.

**Time to refuse: 1.77 ms** on a 100-segment, 20,000-record set whose SECOND segment
mismatches, against **55.1 ms** to read the same set through when everything matches.
§3.2's two-stage split is what buys that — the sealed-segment gate answers from the
directory and the headers, so a refusal costs 3% of a full read rather than 100% of one.
The split was worth having.

**`internal/semcheck`: 0.30 s wall, 1,164 lines, 256 KB golden.** Well inside §8's
two-second bound, which matters because a check people wait for is a check people skip.

**The rotation an upgrade pays at `Open`** is one `materialiseSegment` — the same 12.4 ms
[`LOG-ROTATION.md`](LOG-ROTATION.md) §12.5 measures — once per upgrade, and zero after
that, because §3.7 makes the condition self-healing rather than recurring.
