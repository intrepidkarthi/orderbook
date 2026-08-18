# Journal Completeness — Making the Fourth Escape a Failing Test

Status: **implemented** — slice 1 of the re-cut "Immediate next slice" in
[`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md), written before the code as this
repository does it. §8 records what building it found, including the two places this
document was wrong about its own guard (§8.2, §8.3) and the prediction in §7 that did
not come true (§8.1) ·
Author: Karthikeyan NG · Last updated: 2026-08-17

> **This is a live correctness defect, not a milestone item.** `Runner.SetPhase` is a
> frozen public mutating command that runs an opening or closing auction, prints trades,
> and changes state the snapshot carries — and it is absent from `logCommand`'s switch
> (`pkg/matching/engine_loop.go:335`). A venue that runs an auction and crashes
> before its next checkpoint recovers into the wrong phase with an un-uncrossed book.
> §3 shows the damage does not stop at the phase field.
>
> **It is the third escape of exactly this kind.** `Reduce` was applied but not
> recorded, so a restart brought an order back at its original size. An operator's
> `Halt` was applied but not recorded, so a restart brought a deliberately halted venue
> back Open — found by [`TRADE-BUST.md`](TRADE-BUST.md) §3.5, after four releases in the
> dark. `SetPhase` is the third, and the `CommandLog` doc comment that narrates the
> first two sits eleven lines above the branch the third falls into. Fixing one command
> is half the work. **The other half is making the fourth a failing test rather than a
> post-mortem**, and that is what §4.4 is for.

Companion documents:
- [`TRADE-BUST.md`](TRADE-BUST.md) §3.5 and §7 — where the second escape was found, and
  the argument for fixing a whole class of control command at once rather than the one
  that happened to be in the way. This document is that argument applied a second time,
  which is itself evidence the argument was not fully acted on.
- [`COMPATIBILITY.md`](COMPATIBILITY.md) §"The rule" — "Adding a method to an exported
  interface … is a break: every implementer outside this repository stops compiling.
  `CommandLog` gained five methods in v0.21.0 and this is the case that taught the
  lesson." This work needs a sixth. §4.2 is about paying that price deliberately
  instead of avoiding it and getting a silent hole in exchange.
- [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) and [`LOG-ROTATION.md`](LOG-ROTATION.md)
  — the two slices that made the log operationally bounded. They bounded what recovery
  *reads*. This one is about what recovery *has to read and does not have*.

---

## 1. Why this exists

The strongest property this repository asserts is that a replayed tape reproduces the
live venue exactly. It is not asserted loosely: `TestCrashAtEveryBoundary`
(`pkg/wal/boundary_test.go:54`) crashes at **all 401 boundaries** of a 400-command tape
and compares both a book digest and the trade tape at every one of them, and
`:152` repeats the whole exercise across the snapshot-plus-tail join that production
recovery actually uses.

That test is correct. Its **tape** is not complete:

```go
// buildTape, pkg/wal/boundary_test.go — limit orders and cancels, nothing else
```

No phase transition is ever on it. So the most exhaustive property in the project is
proven over a command alphabet that happens to exclude the one command that escapes the
journal. That is the whole story of this defect: not a missing test, a missing letter in
the alphabet the test runs over.

This is worth stating precisely because it is the failure mode a reader would otherwise
draw the wrong lesson from. The lesson is **not** "write more tests". `boundary_test.go`
is a better test than most systems have. The lesson is that an exhaustive check over an
incomplete input space reports completeness, and the report is load-bearing — it is why
nobody looked at `logCommand`'s `default` branch for three releases.

## 2. What escapes today, verified against the code

Checked at the time of writing by reading the code, not by quoting another document.

1. **`cmdSetPhase` has no case in `logCommand`.** The switch
   (`pkg/matching/engine_loop.go:335`) has fifteen cases, from `cmdSubmit` through
   `cmdBust`, and `cmdSetPhase` is not among them. It reaches:

   ```go
   default:
       // Genuinely read-only commands: queries, the checkpoint, and ExpireDue …
       return
   ```

   The comment is false as written, which is why it now carries a `KNOWN DEFECT` note
   naming this document.

2. **It is genuinely mutating, by all three of this project's own tests for that.**
   It changes engine state that the snapshot carries (`EngineSnapshot.State`,
   `pkg/matching/snapshot.go:51`, restored at `:257`). It emits events
   (`emitStateChange`, and the auction's trade prints). And it produces trades —
   `Engine.SetPhase` returns `[]types.Trade` from `Uncross`
   (`pkg/matching/phase.go:62-90`).

3. **It is public, frozen, and reachable.** Both `matching.(*Engine).SetPhase` and
   `matching.(*Runner).SetPhase` are in `internal/apicheck/testdata/surface.txt`
   (lines 174 and 216), so they are covered by the compatibility promise and cannot be
   quietly withdrawn.

4. **It is not reachable from `cmd/obgw` today.** There is no wire message for a phase
   change and no server call site — grep for `SetPhase` outside `pkg/matching` returns
   only tests. **This bounds the blast radius and does not remove it.** No shipped
   reference gateway loses an auction; a library embedder running sessions through
   `Runner.SetPhase` — which is exactly what the API is for, and what
   `pkg/matching/phase.go:14` tells them to do ("The venue calls SetPhase, because the
   trading calendar is a venue's business") — does.

5. **`wal.EntryKind` has no phase kind and `RestoreAfter` has no phase case.**
   The iota block ends at `KindBust` (`pkg/wal/wal.go:82-101`); `RestoreAfter`'s type
   switch handles fifteen kinds and ignores anything else. *(Corrected after the fact:
   this said "fourteen" before the code was written and the count missed `KindBust` —
   see §8.6. Line references in this section have drifted too; `logCommand`'s switch
   was at `:374`, not `:335`, by the time the fix landed.)*

## 3. What a missing phase record actually costs

The obvious cost — "the venue comes back in the wrong phase" — is the smallest one, and
if it were the only one this would be a lower-priority fix. It is not. Each item below
was verified by reading the path it names.

**3.1 A missing `SetPhase(StatePreOpen)` makes the replayed venue trade through the
pre-open.** `settleInto` (`pkg/matching/engine.go:850-867`) branches on `e.state`: in
`StatePreOpen` or `StateClosingAuction` an order is *rested without matching* and a
market order is *refused*. Replay that arrives in `StateOpen` instead matches those same
orders continuously. So the recovered tape contains **trades that never happened**, and
because trade ids come from a monotonic `tradeSeq`, every subsequent trade id is shifted
by the number of phantom prints. A consumer that stored trade ids before the crash finds
them naming different trades after it.

**3.2 A missing `SetPhase(StateOpen)` skips the auction.** The crossed book that
pre-open legitimately accumulated stays crossed — a bid resting above an ask — which
`pkg/matching/phase.go:53-61` names as the thing a venue must never open onto. The
auction's prints are absent from the recovered tape while the live subscribers already
received them.

**3.3 The divergence then propagates into admission decisions.** `outsideBand`
(`pkg/matching/engine.go:1356-1375`) evaluates the price collar against
`e.markPrice`, falling back to `book.LastTradePrice()` when no mark is set. A skipped
uncross leaves `LastTradePrice` at its pre-auction value, so the collar sits around a
stale reference for **every order after the auction**. An order the live venue accepted
can be rejected on replay, and vice versa. This is the part that makes the defect
qualitatively worse than a wrong enum: the two engines do not merely disagree about a
field, they start making different decisions about customer orders.

**3.4 The digest catches it, which is the one piece of good news.** `EngineSnapshot`
carries `State`, and `Digest()` covers it, so a follower that missed a phase transition
**diverges detectably** rather than silently — the same property
[`TRADE-BUST.md`](TRADE-BUST.md) §7 relied on for busts. Detectable is not the same as
detected: nothing compares those digests in production (see
[`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) M7), so today the guarantee is that
a drill would notice, not that an operator would.

## 4. The design

### 4.1 The log record

```go
// KindSetPhase records a Runner.SetPhase / Engine.SetPhase transition.
KindSetPhase // a SetPhase(phase)
```

appended to the **end** of the `EntryKind` iota block (`pkg/wal/wal.go:82-101`), never
inserted — for the same reason `EventBookDelta` is still declared and never emitted.
An inserted constant renumbers every kind after it, and a log written yesterday would
decode into different commands today. That is unrecoverable rather than merely wrong.

`Entry` gains one field:

```go
// Phase is the target trading phase of a KindSetPhase, as its name
// ("OPEN", "PRE_OPEN", …) rather than its ordinal.
Phase string `json:"phase,omitempty"`
```

**The phase is written as a name, not as a number, and this is deliberate against the
grain of the codebase.** `EngineState` is a `uint8` iota and `EngineSnapshot.State` is
already JSON-encoded as its ordinal, so encoding the log's phase as a number would be
*consistent*. Three reasons it is still wrong:

- **Lifetimes differ by orders of magnitude.** A snapshot is rewritten every checkpoint,
  so the oldest ordinal on disk is minutes old. A log segment is retained for as long as
  retention says and may be archived for years ([`LOG-ROTATION.md`](LOG-ROTATION.md)
  §5). Reordering the `EngineState` block is a change nobody would think of as a
  format change, and it would silently reinterpret every archived phase record.
- **The log is read by humans and by tooling.** `"phase":3` in a record an operator is
  reading during an incident is a trap laid for whoever reads it next. This is not a new
  argument here — it is exactly why `MarkPrice` got its own field instead of borrowing
  `CancelID` (`pkg/wal/wal.go:131-135`), and the reasoning transfers unchanged.
- **A name fails loudly on an unknown value; an ordinal fails quietly.** A future
  `StateAuctionFreeze` written by a newer build reaches an older reader as `6`, which
  decodes as a valid-looking `EngineState` nobody defined. As `"AUCTION_FREEZE"` it is
  an unparseable string and can be refused.

The cost is a parser, since `EngineState` has `String()` and no inverse. It gets one —
`matching.ParseEngineState(string) (EngineState, error)` — which is an *addition* to the
frozen surface and therefore a minor-release change, not a break. **A round-trip test
over every declared `EngineState` is mandatory** (§5, deliverable 5): two mappings that
can drift are worse than one that is ugly, and the only thing stopping drift is a test
that enumerates the block.

`json.Marshal` on `Entry` is unchanged in shape, and `omitempty` means every existing
record encodes byte-identically. Old logs replay exactly as before.

### 4.2 The interface, and the compatibility price

```go
AppendSetPhase(phase EngineState) (int64, error)
```

joins `matching.CommandLog`, and `wal.(*Writer)` gains the matching method.

**This is a breaking change and it is being made on purpose.**
[`COMPATIBILITY.md`](COMPATIBILITY.md) names this exact case — "`CommandLog` gained five
methods in v0.21.0 and this is the case that taught the lesson" — so the price is
already written down: a minor version bump, a `Changed` heading in the changelog naming
what breaks, and an updated `surface.txt` in the same commit. It will be paid rather
than avoided.

The alternative was considered and rejected, and the reasoning matters more than the
conclusion. A **narrow optional interface** —

```go
type PhaseLog interface{ AppendSetPhase(EngineState) (int64, error) }  // rejected
```

— type-asserted inside `logCommand` would break nobody. It would also mean that a
`CommandLog` which does not implement it **silently drops phase records**, which is the
precise failure this document exists to eliminate, reintroduced as the mechanism of its
own fix. An optional durability interface is a contradiction: durability that an
implementer can decline by omission is not a guarantee, it is a default. If the
compatibility promise and the durability promise conflict, the durability promise wins,
and the compatibility promise's job is to make the collision *visible* — which is
exactly what it does here.

**One shape change is deliberately not made.** The root cause of three escapes is that
`CommandLog` is a method-per-command interface: a new mutating command needs a new
method (breaking, therefore discouraged) *and* a new switch case (silent if forgotten).
The design makes the correct thing expensive and the wrong thing free. A single
`Append(Command) (int64, error)` taking a tagged record — which is what `wal.Entry` and
`Writer.append` already are internally — would make a new command a new *value* and
break no implementer. That is very probably the right long-term shape, it is a
fifteen-method redesign, and doing it in this slice would mean debugging the phase
semantics and a whole-interface migration at the same time. §4.5 keeps it out for the
same reason [`TRADE-BUST.md`](TRADE-BUST.md) §4.5 kept the wire out of the first bust
pass. It is recorded here so the next person does not have to rediscover the diagnosis.

### 4.3 Replay

`RestoreAfter` gains:

```go
case KindSetPhase:
    if p, err := matching.ParseEngineState(e.Phase); err == nil {
        eng.SetPhase(p)
    }
```

**Replay re-runs the auction, and it must.** This looks alarming — a recovery path that
executes trades — so the reason is stated here rather than left to be inferred: the
uncross is a pure function of the book, and the book at that point in the tape is
reproduced by the records before it. Restoring the phase *field* without re-running the
uncross would leave the crossed book unresolved and the auction prints missing, which is
the very divergence being fixed. Re-running it is what makes the replayed tape equal the
live one.

Four properties make that safe, each verified rather than assumed:

1. **The uncross is deterministic.** `auction.Uncross` maximises executed volume with
   ties broken deterministically, and allocation is price-time priority within the
   clearing price — `pkg/matching/phase.go:95-101` states this is so "two runs of the
   same book produce the same result — which matters, because this runs during replay
   too." The claim predates this document; it was simply never reachable on the replay
   path, because the command that triggers it never got there.
2. **Trade ids agree.** They come from `tradeSeq`, restored from the snapshot before the
   tail runs — the same ordering argument `KindBust`'s replay comment already makes
   (`pkg/wal/wal.go:1560-1566`), and for the same reason.
3. **Replay mode does not change the outcome.** `SetReplaying(true)` suppresses exactly
   two wall-clock-dependent controls: the minimum resting time
   (`pkg/matching/engine.go:1795`, `:1969`) and the band-breach pause (`:879`). Neither
   is on the uncross path — `bestEligible` (`pkg/matching/phase.go:171-183`) consults
   price only, and the band-breach pause is on the *submit* path. This was checked
   specifically, because "recovery matches differently from live" is the failure this
   whole area exists to prevent, and an unverified "probably fine" is how it would ship.
4. **Refusals replay as refusals.** `SetPhase` early-returns when `e.state == phase`
   (`pkg/matching/phase.go:63`). The state is reproduced by the prefix, so replay
   reaches the same verdict — the identical argument `KindSetMark` already documents.

**Timestamps will differ, and that is not a divergence.** The uncross stamps `e.now`
from the injected clock, which reads a different instant during recovery. `Digest()`
already normalises order wall-clock for exactly this reason, and `tapeSink`
(`pkg/wal/boundary_test.go:16-31`) already excludes it from the trade tape with the note
"Trade ids and the aggressor are part of the contract; wall-clock is not." Anyone
comparing raw `time.Time` values across a recovery will see a difference and should not
report it as a bug.

**An older reader ignores `KindSetPhase`, and that is the correct failure.** A follower
on a build without this kind falls through `RestoreAfter`'s type switch, misses the
transition, and diverges from its primary — but because `State` is in the snapshot and
the digest covers it, the divergence is **detectable**. This is the same outcome
[`TRADE-BUST.md`](TRADE-BUST.md) §7 recorded for `KindBust` and reached the same way:
not by design, but by the digest already covering the right thing. Version negotiation
between primary and follower is still absent and is still out of scope.

### 4.4 The exhaustiveness guard — the half that outlives this bug

Fixing `SetPhase` closes one hole. This closes the shape that produces them.

**A sentinel at the end of the command block** (`pkg/matching/queue.go:9-30`):

```go
cmdIndicative
cmdCheckpoint
cmdKindCount // sentinel: keep last
```

**A classification table in the test package**, mapping every `cmdKind` to one of two
things — the `wal.EntryKind` it must be journalled as, or a *named reason* it is
genuinely read-only:

```go
// illustrative shape; the point is that every kind appears
cmdSubmit:     journalled(wal.KindSubmit),
cmdSetPhase:   journalled(wal.KindSetPhase),
cmdCheckpoint: readOnly("takes a snapshot; changes nothing recovery must reproduce"),
cmdExpireDue:  readOnly("derives cancels from a clock replay cannot rewind"),
```

and three tests over it:

1. **`TestEveryCommandKindIsClassified`** iterates `0 … cmdKindCount-1` and fails on any
   kind missing from the table. Adding a command kind without deciding whether it is
   durable becomes a compile-and-test failure at the moment the constant is added,
   rather than a discovery after a crash three releases later.
2. **`TestEveryMutatingCommandReachesTheLog`** drives a `Runner` with a recording
   `CommandLog`, issues each command classified as journalled, and asserts **exactly
   one** record of the expected `EntryKind`. This is the test with teeth: it is the one
   that would have caught `Reduce`, `Halt` and `SetPhase`, and it fails against today's
   code on the third.
3. **`TestReadOnlyCommandsWriteNothing`** issues each read-only command and asserts the
   log is untouched — so the table cannot be gamed by classifying an awkward command as
   read-only to make test 2 pass.

   > **This claim is false, and building it proved so (§8.2).** That test only catches
   > a kind classified read-only which *still* reaches a `CommandLog` method — the one
   > combination a gaming author would never write. Classifying `cmdSetPhase` read-only
   > and dropping its `logCommand` case passes it. A fourth test does the job the
   > paragraph above promises: **`TestReadOnlyCommandsChangeNoRecoverableState`**
   > drives every read-only kind through a `Runner` and requires
   > `EngineSnapshot.Digest()` to be unchanged, so the written reason is checked
   > against the engine rather than taken on trust.

   And a fifth, in `pkg/wal`, because the chain does not end at the log (§8.3):
   **`TestEveryEntryKindReplays`** enumerates `entryKindCount` and requires every
   `wal.EntryKind` to have an arm in `restoreEntry`. Journalled is not the same as
   replayed, and a kind with a `Writer` method and no replay arm is a durable record
   that recovery silently discards.

   > A sixth joined them later, and it exists because the fifth turned out to answer a
   > narrower question than it looks like it answers: *recognised* is not *applied*.
   > **`TestEveryWrapperRecordRebuildsItsOrder`** enumerates the same sentinel and
   > requires every kind that carries a wrapper order to round-trip through a
   > recovery **from the log alone**, compared against the live engine by trade tape
   > and snapshot digest — because `KindIceberg` had an arm in `restoreEntry`, passed
   > every guard above, and still rebuilt a nine-lot iceberg as a three-lot order.
   > [`ICEBERG-DURABILITY.md`](ICEBERG-DURABILITY.md) §6 and §12.2.

The `readOnly` reasons are strings rather than a bare boolean deliberately. A future
author reclassifying a command has to write down why, and a reviewer gets a sentence to
disagree with instead of a flipped flag.

**And the alphabet gets fixed.** `buildTape` in `pkg/wal/boundary_test.go` gains phase
transitions — a pre-open that accumulates a crossed book, an open that uncrosses it, and
a closing auction — so `TestCrashAtEveryBoundary` and its snapshot variant exercise
auctions at every boundary. Without this, §1's diagnosis stands uncorrected: the
exhaustive test would still be exhaustive over the wrong alphabet, and the next escape
would hide in the same place.

### 4.5 What this deliberately does not do

- **It does not collapse `CommandLog` into a single tagged-record method.** Diagnosed
  in §4.2, deferred on purpose. Fifteen methods to one is a migration for every
  implementer and deserves its own spec and its own version bump.
- **It does not make `Engine.SetPhase` durable.** `Engine` is the undurable
  single-writer core; `Runner` is the durable seam. An embedder calling `Engine.SetPhase`
  directly bypasses the log exactly as `Engine.Halt` and `Engine.Cancel` already do, and
  making the Engine self-journalling would give it a file handle and a failure mode it
  is specifically designed not to have. The guard in §4.4 covers the `Runner` seam,
  because that is where durability is promised.
- **It does not add a wire message for a phase change.** `cmd/obgw` still cannot be told
  to open or close a session by a client or an operator over the protocol, and that
  stays true. Scheduling a trading calendar is the venue's business
  (`pkg/matching/phase.go:14`); this slice makes the transition *durable when it
  happens*, not remotely triggerable.
- **It does not add phase transitions to `cmd/obsoak`'s load.** The soak's narrow order
  mix is a real gap ([`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) M13) and a
  separate one.
- **It does not introduce primary/follower version negotiation.** An older follower
  ignoring `KindSetPhase` diverges detectably (§4.3). Making that a refusal instead of a
  detection is M4 work.
- **It does not audit `pkg/orderbook` or the `Engine` for other unlogged mutations.**
  The guard covers commands that pass through the `Runner` queue. A mutation reachable
  only by calling `Engine` directly is outside it by the bullet above.

## 5. Deliverables and acceptance criteria

| # | Deliverable | Done when |
|---|---|---|
| 1 | `KindSetPhase` + `Entry.Phase` + `Writer.AppendSetPhase` | An `Entry` round-trips through `append`/`ReadAll` with its phase intact; every pre-existing record encodes byte-identically (no golden change) |
| 2 | `CommandLog.AppendSetPhase`, `logCommand` case, `syncingLog` delegation | `TestEveryMutatingCommandReachesTheLog` passes for `cmdSetPhase`; `surface.txt` updated and `CHANGELOG.md` carries a `Changed` entry naming the interface break |
| 3 | `RestoreAfter` case | A venue that runs pre-open → open, crashes before its next checkpoint, and recovers, comes back **Open with an uncrossed book and the auction prints on its tape** |
| 4 | The crash test that fails against today's code | `TestCrashAcrossAnAuction`: pre-open, a crossed book, `SetPhase(StateOpen)`, crash at every boundary — book digest **and** trade tape identical to the uninterrupted run. Must fail before the fix and pass after |
| 5 | `ParseEngineState` + round-trip | `TestEngineStateNamesRoundTrip` covers every declared `EngineState`; an unknown name returns an error rather than `StateOpen` |
| 6 | The exhaustiveness guard | The three tests of §4.4 pass, and `TestEveryCommandKindIsClassified` fails when a new `cmdKind` is added without a table entry |
| 7 | The alphabet fix | `buildTape` includes phase transitions; `TestCrashAtEveryBoundary` and `TestCrashAtEveryBoundaryWithSnapshot` still pass at every boundary, now with auctions in the tape |
| 8 | The replicated case | Drill **D10**: a primary that runs an auction mid-stream, a follower that digest-matches it, and a promotion that preserves the phase |

**The measurable criterion, stated as one sentence so it can be checked rather than
argued about:** after this work, a tape containing any sequence of phase transitions
replays to a byte-identical book digest and an identical trade tape at **every** command
boundary — the property `boundary_test.go` already asserts, over an alphabet that now
includes the auction.

Two supporting numbers worth recording while the work is done, neither of them a pass
criterion: the added bytes per `KindSetPhase` record (it should be the smallest record
the log writes apart from `KindHalt`), and whether extending `buildTape` measurably
slows `TestCrashAtEveryBoundary`, which is O(n²) and was explicitly sized to stay "a
test people will actually run rather than a nightly job they will not."

## 6. Sabotage runs required before this counts as done

No deliverable above counts until the test that proves it has been run against code
deliberately broken in the matching way, and **failed**. This project has one recorded
case of a digest test passing against the exact sabotage it existed to catch
([`TRADE-BUST.md`](TRADE-BUST.md) §7), which is the only reason that section is not
claiming a property the suite never checked.

| # | Sabotage | Must fail |
|---|---|---|
| 1 | Delete `case cmdSetPhase` from `logCommand` | `TestCrashAcrossAnAuction`, `TestEveryMutatingCommandReachesTheLog` |
| 2 | Delete `case KindSetPhase` from `RestoreAfter` | `TestCrashAcrossAnAuction` |
| 3 | Journal the command but always write `StateOpen` regardless of the target phase | `TestCrashAcrossAnAuction` — this is the one that catches a test which only checks *that* a record exists |
| 4 | Make `ParseEngineState` return `StateOpen` for an unknown name instead of erroring | `TestEngineStateNamesRoundTrip` |
| 5 | Remove one entry from the classification table | `TestEveryCommandKindIsClassified` |
| 6 | Add a new `cmdKind` before the sentinel and classify it nowhere | `TestEveryCommandKindIsClassified` |
| 7 | Classify `cmdSetPhase` as `readOnly` to make test 2 pass | ~~`TestReadOnlyCommandsWriteNothing`~~ → **`TestReadOnlyCommandsChangeNoRecoverableState`**. The named test *passes* against this sabotage; see §8.2 |
| 8 | Apply the whole fix, then revert **only** the `buildTape` extension | Nothing fails — **and that is the point.** This run proves the alphabet fix is load-bearing rather than decorative: with it reverted, the suite goes green over code that was broken for three releases. Run it, record that it passes, and keep the extension. *(Outcome: it now FAILS, because the extension came with two `t.Fatal` guards that defend it. The demonstration was run against `HEAD` instead, where it holds exactly — see §8.5)* |
| 9 | Drop `KindSetPhase` on the replication wire | Drill D10 |
| 10 | Restore the phase field on replay *without* re-running the uncross | `TestCrashAcrossAnAuction` — the crossed book and the missing prints must both be caught, not just the phase |

Sabotage 8 is the unusual one and it is the most important. Every other row asks a test
to fail. That row asks the suite to *pass* against broken code, to demonstrate that the
gap §1 diagnosed was real and that closing it is what the other nine rows depend on.

## 7. How this can fail, stated in advance

So that §8 is not graded on a curve.

- **The uncross may not be as deterministic as `phase.go:91-100` claims.** That comment
  asserts replay-safety for a path that has never actually run during replay, because
  the command that triggers it never reached the log. §4.3 checked the two suppressed
  controls and found neither on the uncross path, but "checked the paths I thought of"
  is how the first three escapes happened. If `TestCrashAcrossAnAuction` produces a
  different clearing price or a different allocation on replay, the honest finding is
  that the auction has a hidden dependency and this slice grows a great deal.
- **The self-trade-prevention branch inside `Uncross` is the likeliest place for that to
  be true.** It cancels the *newer* of two self-crossing orders by comparing ids and
  stamps `victim.UpdatedAt = e.now` (`pkg/matching/phase.go:131-146`). The comparison is
  deterministic; the timestamp is not, and it lands on an order that may still be
  resting and therefore *in the snapshot*. `Digest()` normalises order wall-clock, so
  this should be invisible — but "should be invisible because a normaliser covers it" is
  a claim about the normaliser, and it needs the sabotage in row 3 to be trusted.
- **`ParseEngineState` may be the wrong home for the name mapping.** Putting it in
  `pkg/matching` adds an exported symbol to a frozen surface to serve a need that
  originates in `pkg/wal`. If a reviewer prefers the mapping in `wal` to keep the
  matching surface small, the counter-argument is that `EngineState` is a `matching`
  type and a parser for it living elsewhere is exactly the drift the round-trip test
  exists to prevent. Worth arguing before it ships, not after.
- **The interface break may cost more than predicted.** Nobody outside this repository
  is known to implement `CommandLog`, which makes the break cheap — but that is an
  assumption about a population this project cannot see, and
  [`COMPATIBILITY.md`](COMPATIBILITY.md) exists precisely because the last such
  assumption was wrong six times in two days.
- **Extending `buildTape` may make the boundary tests slow enough that someone reduces
  `n`.** That would trade the alphabet fix for the exhaustiveness, which is a bad trade
  made quietly. If it happens, the right answer is a second smaller tape dedicated to
  phase transitions, not a shorter main one.

## 8. What building it found

Written after the code, in the manner of [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md)
§9 and [`LOG-ROTATION.md`](LOG-ROTATION.md) §12. Everything below was run, not
reasoned about.

### 8.1 The prediction this document was most worried about was wrong

§7 leads with "the uncross may not be as deterministic as `phase.go` claims", and
names the self-trade-prevention branch as the likeliest place for that to be true,
because it stamps a wall-clock `UpdatedAt` on a victim order that may still be
resting and therefore in the snapshot. `auctionTape` reaches that branch on purpose —
the participant `"self"` crosses itself inside the pre-open — and
`TestCrashAcrossAnAuction` compares a book digest **and** a trade tape at all 24
boundaries of a two-auction session.

It passed on the first run and has passed every run since. `Digest()`'s
wall-clock normalisation does cover it, and sabotage row 3 is what makes that
statement worth anything: with the writer changed to record `StateOpen` regardless of
the target phase, the same test fails at boundary 1. The normaliser hides the
timestamp and does not hide the phase.

**The slice did not grow.** §7 predicted that if this went the other way "this slice
grows a great deal"; it did not, and the honest reading is that
`phase.go`'s determinism comment was correct for three releases while being
unreachable on the path it described.

### 8.2 §4.4's read-only guard did not do what §4.4 says it does

This is the significant finding, and it is a hole in the *guard*, not in the fix.

§4.4 claims `TestReadOnlyCommandsWriteNothing` "stops the table being gamed by
classifying an awkward command as read-only to make test 2 pass". **It does not.** It
only detects a kind that is classified read-only *and* still reaches a `CommandLog`
method — the one combination an author gaming the table would never produce. Run
sabotage row 7 exactly as specified — classify `cmdSetPhase` as
`readOnly("just moves an enum; carries no book state")` and drop its `logCommand`
case — and `TestReadOnlyCommandsWriteNothing` **passes**. So does the whole of the
rest of the shape guard.

That is the precise shape of all three escapes. `Reduce`, `Halt` and `SetPhase` were
each a mutating command that reached no log and looked to its author like it carried
no book state. A guard whose only defence against that is a prose reason field is a
guard that would have missed all three.

So the reason is now checked against the engine.
`TestReadOnlyCommandsChangeNoRecoverableState` drives every read-only kind through a
`Runner` and requires `EngineSnapshot.Digest()` to be byte-identical across it. The
digest covers the book, the trading state, the mark and the sequence counters — it is
the mechanical statement of "changes nothing recovery must reproduce", so a command
that moves it is journalled or it is a defect, and no sentence can settle the
question instead. All five kinds classified read-only today pass it, which is the
first evidence any of those five reasons has ever had.

Verified by running: a new `cmdSabotageMutator` whose dispatch arm calls
`SetCancelOnly`, journalled nowhere, classified `readOnly("looked harmless to the
author who added it")`, passes the entire `pkg/matching` suite as §4.4 specifies it
and fails the digest check.

**Row 7 of §6 names the wrong test.** It should read
`TestReadOnlyCommandsChangeNoRecoverableState`.

### 8.3 The guard chain is longer than §4.4 knew

§4.4 guards `cmdKind → CommandLog method`. The actual chain a command travels is:

```
cmdKind  →  CommandLog method  →  wal.EntryKind  →  restoreEntry arm
```

**Journalled is not the same as replayed**, and only the first link was enumerated.
A command wired end to end — a `cmdKind`, a table entry, a `CommandLog` method, a
`logCommand` case, an `EntryKind`, a `Writer` method — but with no arm in
`RestoreAfter` writes a durable record that recovery silently discards, and the guard
reports the alphabet complete. Unlike `cmdKind` there was no `EntryKind` sentinel and
no test asserting an arm exists.

Closed the same way: `entryKindCount`, and `TestEveryEntryKindReplays`, which
enumerates the block and requires `restoreEntry` to recognise each kind. That is what
`restoreEntry`'s boolean return is for — an unrecognised kind must still be *skipped*
(a newer build's record reaching an older reader has to be ignored, not guessed at),
so "skipped" and "has no arm" are the same behaviour and needed telling apart.
Sabotage row 2 now fails twice: once as a divergence, once as
`EntryKind 16 has no arm in restoreEntry`.

### 8.4 The guard failed by crashing instead of reporting

As first written, two of the three tests indexed `commandClassification` without the
comma-ok form. An unclassified kind therefore yielded a zero-value classification
whose `sample` was nil, and the next line dereferenced it: `SIGSEGV`, the test binary
dead, and the rest of `pkg/matching` never run. The message
`TestEveryCommandKindIsClassified` exists to print — the one sentence telling an
author what to do — was buried under a stack trace from a different test.

Confirmed by running both ways: with a table entry removed, the comma-ok form reports
`cmdKind 13 is not in commandClassification: decide whether it is journalled … or
read-only`, and reverting to the bare index on the same tree panics. A guard that
takes the package down with it costs more than it finds.

### 8.5 Two of §5's and §6's own predictions did not survive contact

**Sabotage row 8 no longer passes, and that is an improvement.** Row 8 asks for the
whole fix with only the `buildTape` extension reverted, and predicts nothing fails.
Two `t.Fatal` guards were added with the extension — the sweep must run an auction,
and the snapshot sweep's auction must fall in the *tail* — so reverting it now fails
outright with `the tape ran no auction, so this sweep is back to the alphabet
docs/JOURNAL-COMPLETENESS.md §1 diagnosed`. The extension defends itself, which is
better than being load-bearing on trust.

The demonstration row 8 actually wanted still exists, and it was run against `HEAD`
instead: with neither the durability fix nor the tape extension,
`TestCrashAtEveryBoundary`, `TestCrashAtEveryBoundaryWithSnapshot` and
`TestCrashRecoveryMatchesUninterrupted` **all pass** over code that loses an entire
auction on restart. 401 boundaries, a book digest and a trade tape compared at every
one, green. §1's diagnosis reproduced exactly.

**The record is not the small one §5 predicted.** §5 guessed `KindSetPhase` would be
"the smallest record the log writes apart from `KindHalt`". Measured:

| record | bytes |
|---|---:|
| `KindHalt` / `KindResume` / `KindCancelOnly` | 33 |
| `KindSetPhase` (`"OPEN"`) | 48 |
| `KindSetPhase` (`"PRE_OPEN"`) | 52 |
| `KindSetMark` | 52 |
| `KindSetPhase` (`"CLOSING_AUCTION"`) | 59 |
| `KindCancel` | 65 |

A phase record is 15 to 26 bytes larger than a halt, and the longest phase name costs
more than a mark price. **That is the price of §4.1's argument, and it is the right
trade** — phase transitions are rare, and a segment an operator reads during an
incident says `"CLOSING_AUCTION"` rather than `5` — but §4.1 argued the case on
lifetime and legibility without ever pricing it, and the number belongs next to the
argument.

The other number §5 asked for: extending `buildTape` takes
`TestCrashAtEveryBoundary` from ~0.34 s to ~0.90 s (2.7x, three runs each). Still
comfortably "a test people will actually run". §7 warned someone would react to this
by shrinking `n`; the guard in 8.5 above means shrinking the *alphabet* instead is now
a failure, and if `n` ever has to move, §7's answer — a second dedicated tape, not a
shorter main one — still stands.

### 8.6 Where the code disagreed with this document

- **§2 item 5 says `RestoreAfter`'s type switch "handles fourteen kinds".** It handled
  **fifteen** at `HEAD` — the count missed `KindBust`. §2 item 1's "fifteen cases" for
  `logCommand` was right.
- **Line references drifted before the code was written.** §2 cites
  `engine_loop.go:335` for `logCommand`'s switch; it was at `:374` by the time the fix
  landed, and `RestoreAfter` at `:1495` rather than the cited range. Line numbers in a
  spec age faster than the spec does.
- **Deliverable 5's one test is two tests.** `TestEngineStateNamesRoundTrip` covers the
  round trip over every declared state; refusing an unknown name is a separate
  property with a separate failure mode, and it is
  `TestParseEngineStateRefusesUnknownNames` that catches sabotage row 4. A third,
  `TestEngineStateSentinelIsNotAState`, pins the enumeration itself, because a loop
  written `s <= engineStateCount` would assert the sentinel is a valid phase and pass.
- **§4.3's "an older reader ignores `KindSetPhase`" is now a tested behaviour**, not
  an inference: `TestUnknownEntryKindIsSkippedNotApplied`.
- **Drill D10's phase assertion is not the one that works.** Sabotage row 9 — the
  phase dropped on the replication wire — leaves the follower in `StateOpen`, which is
  both the zero value and the phase the drill expects, so `snap.State` matches and the
  assertion stays silent. Only the digest comparison fails. The lesson is D7's,
  restated: assert the digest, not the field, because the field can be right for the
  wrong reason.
- **§4.1's placement worry was not worth the space.** §7 wondered whether
  `ParseEngineState` belongs in `pkg/wal` rather than on the frozen `matching`
  surface. It stayed in `matching`, on the document's own counter-argument, and
  nothing in the build pushed back.

### 8.7 What this slice still does not do

Everything in §4.5 stands. Two things §4.5 did not name:

- **The guard covers the `Runner` queue, and `Runner.SetReplaying` is not on it.** It
  is a public, frozen, mutating `Runner` method that writes `e.replaying` directly,
  bypassing the queue and therefore `logCommand`, so it is structurally invisible to a
  guard that enumerates `cmdKind`. It is legitimately unjournalled — recovery sets the
  flag itself — but the guard's claim is "every command kind", not "every public
  mutating method", and the difference deserved recording rather than discovering.
- **The swept alphabet is wider, not complete.** `buildTape` now speaks submit, cancel
  and setphase. Thirteen journalled kinds — stop, OCO, iceberg, pegged, trailing,
  halt, resume, cancel-only, set-mark, bust, cancel-all, reduce, replace — still never
  appear at any crash boundary, and neither do market orders, IOC/FOK, post-only or
  GTD. Each has a hand-written recovery test, so this is not a live defect; it is the
  same sentence as §1 with a smaller number, and the next escape can still hide behind
  it.
