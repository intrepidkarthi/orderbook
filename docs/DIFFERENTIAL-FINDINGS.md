# Differential Findings — Deciding Three Defects Before Fixing Them

Status: **implemented** — the fix slice for the three engine defects
[`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §10.1 found and deliberately pinned
rather than repaired. Written before the code, as this repository does it. §3, §4 and
§5 each end in a decision; §8 is how a reviewer checks the decision was carried out;
§10 is how this can still be wrong; **§11 is what carrying it out measured, including
the three places this document was wrong about its own reachability** ·
Author: Karthikeyan NG · Last updated: 2026-08-17

> **The measurement that decided two of the three.** Four commands, `DefaultConfig`,
> nothing exotic switched on:
>
> 1. `u1` rests **sell 3 @ 100**.
> 2. `u1` rests **sell 5 @ 105**.
> 3. `u2` rests a **buy stop, trigger 100**, market for 2. Nothing has traded, so
>    `LastTradePrice` is 0 and the stop rests.
> 4. `u3` sends a **fill-or-kill buy 5 @ 100**. It prints 3, cannot fill, and every
>    print is reversed. It is **REJECTED with no fills of its own.**
>
> What the venue then did, measured: `LastTradePrice` is **100**, the stop
> **fired**, and trade `id=2` printed **2 lots @ 100 between `u1` and `u2`** — a real,
> settling execution that moved a resting order from 3 lots to 1. The complete event
> stream for command 4 is one event: `REJECTED`, naming `u3`.
>
> So a rejected order caused a trade between two other accounts, and **neither of them
> was told.** `u1`'s resting order silently lost 2 lots; `u2`'s stop silently became a
> filled position. Defect (a) fired the stop; defect (b) hid the print. Neither is
> reachable from the other's fix, and this slice is why they ship together.

Companion documents:
- [`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §9 and §10.1 — where these three were
  predicted, then found, then pinned. §1.3 is the sentence this document is the
  consequence of: "two implementations that agree may both be wrong, and they will
  both be wrong in exactly the case where they were written from the same wrong
  sentence." Two of these three are that case, measured.
- [`TRADE-BUST.md`](TRADE-BUST.md) §2 — "a bust does not rewind `LastTradePrice`."
  That reads as a precedent *against* §3's decision. §3.5 shows it is the precedent
  *for* it, once the rule behind it is stated rather than the outcome.
- [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §1 — "an exhaustive check over
  an incomplete input space reports completeness, and the report is load-bearing."
  `TestEventStreamReconstructsBook` is that shape exactly: an exact-output check over
  ~25 hand-written scenarios, none of which combines fill-or-kill with self-trade
  prevention. §4 is that lesson applied to an event stream instead of a journal.
- [`COMPATIBILITY.md`](COMPATIBILITY.md) §"The rule" — "Behaviour changes that fix a
  stated contract, with the changelog entry saying so" are not breaking; a venue that
  begins refusing input it used to accept is. §3.8, §4.8 and §5.6 apply that rule to
  each defect and reach two different answers.
- [`TESTING.md`](TESTING.md) §"The rule" — "A new test does not count until it has been
  run against code deliberately broken in the way it claims to detect." §9 is this
  slice's list, and it includes the two sabotages that break the *fix* rather than the
  engine.

---

## 1. Why this document exists

### 1.1 Three defects, pinned rather than fixed, and why that was right

Building the reference matcher found three engine defects
([`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §10.1). None was fixed in that slice.
Each is pinned by a hand-written test that asserts the **wrong** behaviour and carries
the sentence a fix has to come and delete:

| | Defect | Pinned by |
|---|---|---|
| **A** | A rejected fill-or-kill still moves `LastTradePrice` | `TestRejectedFOKStillMovesTheLastTradePrice` |
| **B** | A self-trade-prevented maker vanishes with no event | `TestSTPCancelledMakerVanishesWithNoEvent` |
| **C** | Pro-rata's self-skip rests a taker across the spread | `TestProRataSelfSkipCrossesTheBook` |

Pinning was right and this document is the reason: **each of the three has more than
one defensible answer, and shipping the wrong one is worse than leaving the defect
recorded.** A defect that is pinned, named and linked to a spec is a known liability
with a reproduction. A defect "fixed" in the wrong direction is a new rule that
nothing argues for, buried in a diff, which the next person will read as intentional.

### 1.2 Every fix here has three sides

`internal/refmatch` was written to reproduce the engine's position on **A** and **B**.
That is stated in [`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §2.2 as a measured
limit on the harness's independence, not as an accident: the model canonises the
engine's answer, so the differential comparison is silent about both **by
construction**.

The consequence is arithmetic. Fixing the engine alone makes the *model* wrong, and
`TestDifferentialTape` goes red across several profiles and seeds at once — measured
in the previous slice, not predicted:

| Engine-only fix | Fails | Class |
|---|---|---|
| restore `LastTradePrice` in `settleInto`'s FOK unwind | `fifo/seed=3`, `fifo/seed=8`, `prorata-shard7/seed=4`, `capped-shard3/seed=42` | `last-trade-price` |
| keep the `Canceled` events on a rejected order | `fifo/seed=3`, `fifo/seed=34`, `capped-shard3/seed=42` | `event-count` |

So every fix in this document is **three-sided** — the engine, the model, and the
pinning test that currently asserts the wrong answer — and the failure mode to guard
against is specific: *someone fixes the engine, sees a wall of divergences, does not
know why, and reaches for the cheapest-looking repair, which is to relax the
comparison.* That trades one real defect for a permanently weaker oracle.

Two corollaries, and they point in opposite directions on purpose:

- **A fix that changes only the engine is not finished.** The suite is red and the
  next person will "fix" the harness.
- **A fix that changes only the model is a cover-up.** The suite is green, the
  document says the defect is closed, and the venue still does it.

§9 requires both directions to be run and to fail.

### 1.3 What is decided here, and what is not

Decided: the semantics — what the venue should do, why, what it costs, and which
alternatives were considered and refused. Also decided: whether each one lands under
`Changed` or `Fixed` in [`CHANGELOG.md`](../CHANGELOG.md), which
[`COMPATIBILITY.md`](COMPATIBILITY.md) makes a rule rather than a taste.

Not decided here, deliberately: the diff. Line-level notes appear only where the
*place* is part of the argument — for instance that defect A must be repaired inside
`settleInto` and not inside `Match`, because a stop order settled by `cascadeStops`
calls `settleInto` again and needs its own pre-match reference.

## 2. The rule all three break

### 2.1 One sentence

> **After a command that is refused, the venue's observable state equals what it was
> before the command — or every difference is on the event stream.**

"Observable" is doing the work, and it is enumerable in this engine: the resting book
(L3 and the L2 aggregate), `LastTradePrice`, the trading state, the id counters, the
returned `MatchResult`, and the event stream. Defect **A** breaks the rule by leaving
an unannounced difference. Defect **B** breaks it by leaving several. Defect **C** is
a different rule (§5) and is only in this document because it arrived on the same
sweep.

### 2.2 Two remedies, and the test that picks between them

The rule has an "or" in it, so each difference gets one of two remedies: **revert it**,
or **announce it**. The test for which:

> Revert what the venue can restore without loss. Announce what it cannot.

`LastTradePrice` is a **derived datum**: one int64 whose only meaning is "the price of
the last print". Restoring it loses nothing, because the value it is being restored to
is still exactly right — a print with that price exists, or none does. So: revert (§3).

A self-trade-prevented maker is **not** restorable without loss. Putting it back costs
its queue position (`OrderBook.Add` appends to the tail of its level), and, worse,
restoring it is not composable with the other four things the walk does to the book
that are not trades. §4.4 measures two of them; one of them produces an order with a
**negative** `FilledQty` and nine displayed lots on an order whose stated quantity is
three. So: announce (§4).

That is not a compromise between two philosophies. It is one rule applied twice, and
saying so is what stops the next reader from concluding the two defects were fixed
inconsistently.

### 2.3 The engine already agrees, in three places

This is not a new rule being imposed on the code; it is a rule the code already
follows everywhere **except** inside the matching walk.

- `Engine.Cancel` emits the `Canceled` and calls `flushPending()` immediately
  (`engine.go:2035-2036`), so the event survives whatever the caller does next.
- `Engine.Reduce` does the same with `Replaced` (`engine.go:1864-1865`).
- `expireDue` cancels every expired order and calls `flushPending()`
  (`expiry.go:135-137`), so an unrelated order's expiry cannot be swallowed by the
  verdict of the order whose arrival happened to trigger the sweep.

`Engine.Replace`'s doc comment (`engine.go:1884-1890`) states the rule outright: if the
cancel succeeds and the replacement is then refused, "that is reported by the
`Canceled` and `Rejected` events of this same command, so it is immediately visible
rather than discovered later." That is true — because `Cancel` flushed. The events
recorded *inside* `match` are the only ones composed by `emitResult`, and
`emitResult` is the only place that drops (`engine.go:751-753`). **The rule is already
the house rule; one function does not obey it.**

## 3. Defect A — a rejected fill-or-kill moves the last trade price

### 3.1 What it does today, measured

Three commands (`TestRejectedFOKStillMovesTheLastTradePrice`): a maker rests 3 @ 100;
a fill-or-kill buy for 5 arrives; it prints 3, cannot fill, is reversed, returns
**REJECTED with zero trades and the maker fully restored** — and `LastTradePrice` is
**100**.

The mechanism is two functions that do not know about each other. `match` calls
`recordLast` on the way out (`engine.go:1670-1675`), setting the price from the last
print of the walk. `settleInto`'s fill-or-kill branch (`engine.go:972-982`) then calls
`reverseTrade` (`engine.go:1769-1797`) for every print and returns `dst[:start]`.
`reverseTrade` restores the makers' quantities and their place in the book. Nothing
restores the price.

Two further measurements, both new in this slice:

- **It fires stops.** The four commands in this document's opening block. `Match`
  calls `cascadeStops` *after* `settleInto` returns (`engine.go:699`), and
  `cascadeStops` reads `e.book.LastTradePrice()` as its trigger reference
  (`engine.go:1008`). A rejected order's phantom price triggered a resting stop, which
  really traded 2 lots between two other accounts.
- **It turns the collar on.** With `PriceBand` at 10% and nothing ever traded, the
  band is disabled (`outsideBand` returns false when the reference is 0,
  `engine.go:1411-1431`). After one rejected fill-or-kill the reference is 100, and an
  unrelated `buy 1 @ 150` from a fourth account is refused with `ErrPriceOutsideBand`.
  An order that never traded refused an order that never met it.

### 3.2 What `LastTradePrice` is for, and which reading each use wants

| Use | Site | Wants "the price of the last published print" | Wants "the price of the last attempted match" |
|---|---|---|---|
| Price-collar reference when no mark is set | `outsideBand`, `engine.go:1420` | yes — the collar is a band around where the market *is*, and a price nobody traded is not where the market is | no |
| Band-tick window for depth accounting | `engine.go:1530` | yes, same reason | no |
| Stop and stop-limit triggering | `cascadeStops`, `engine.go:1008`; `submitStopInto`, `engine.go:1055` | yes — a stop is a client instruction that says "when the market **trades** at X"; firing on a price nobody traded is a fill the client did not ask for | no |
| Trailing-stop ratchet | `ProcessTrailingStop`, `engine.go:1128` | yes, same reason | no |
| Market data: `Snapshot.LastTradePrice`, the feed, `cmd/obdash`, `cmd/obwasm` | `marketdata/feed.go:343`, `snapshot.go:125` | yes — this is the number a subscriber prints as "last" | no |
| Research reference price (`pkg/study`, `pkg/sim`, `pkg/backtest`) | e.g. `study/kyle.go:21` | yes — used as a mid fallback | no |
| Recovery | `snapshot.go:212-213` | either, as long as it is deterministic | either |

**Every use wants the same reading, and it is not the one the engine implements.**
There is no consumer of this value that wants "a price at which a match was attempted
and undone". The two most consequential — the collar and stop triggering — are the two
where the current reading actively causes a second wrong.

### 3.3 Decision A

> **A rejected fill-or-kill leaves `LastTradePrice` exactly as it found it.**
> `settleInto` captures the pre-match value before it calls `match`, and its
> fill-or-kill branch restores that value alongside `reverseTrade`. Capturing it in
> `settleInto` and not in `Match` is required, not stylistic: `cascadeStops` calls
> `settleInto` again for each triggered stop (`engine.go:1025`), and a nested
> fill-or-kill must restore the price *its own* walk started from, which is the one
> the triggering trade set.
>
> The unifying definition, which is what should go in the doc comment:
> **`LastTradePrice` is the price of the last trade this venue PUBLISHED.**

Three consequences fall out of that definition rather than being decided separately:

- An **IOC** that partially fills moves it and keeps it. Those fills stand, carry trade
  ids, reach the sink as `EventTrade`, and appear in the returned result. Published.
- A **rejected fill-or-kill** does not move it. Nothing was published: no event, no
  trade in the `MatchResult`, no maker left changed.
- A **bust** does not rewind it. The print *was* published. §3.5.

### 3.4 The alternatives, and why each is refused

**(i) Leave it — "the fills genuinely occurred before being unwound."** This is the
strongest counter-argument and it is wrong on its own terms. In what sense did they
occur? Not in the event stream (`emitResult` drops them, `engine.go:752`). Not in the
result (`dst[:start]`). Not in the book (`reverseTrade` restores every maker). Not in
the journal. The only traces they leave are the trade-id counter — deliberately, and
argued in §3.4(iv) — and this price, accidentally. A venue whose published "last trade
price" names a price that appears on no tape, in no drop copy and in no execution
report has not
recorded a fact; it has leaked an implementation strategy. The speculative
execute-then-reverse walk is *how this engine decides fillability*. No consumer
contracted for it.

**(ii) Decide fillability before printing anything.** The root repair: scan the
opposite side, sum what is eligible, and only execute if the whole quantity is
available — which is how a matching engine that never needs a reversal path works, and
it would close defect A and half of defect B at once. **Refused, and it is the closest
call in this document.** Three costs. First, it is a *second* implementation of the
crossing test, the self-match test and the STP branches, which can disagree with the
first — precisely the defect class this repository's oracle exists to catch, and one
where a differential harness cannot help because both sides would be asked to model
the same new rule. Second, it deletes a documented, correct behaviour that has an
oracle: `REFERENCE-MATCHER.md` §3.4's "trade ids have gaps, and the gaps are correct"
becomes false, and an operator reconciling a trade-id gap loses the explanation. Third,
STP `DECREMENT` makes "would it fill?" *depend on side effects* — it shrinks the taker
as it goes (`engine.go:1399-1407`), so a pre-scan has to simulate the walk it is trying
to avoid. Keeping the reversal and making it total is a smaller change with a smaller
blast radius. **If a future slice does take this route, this paragraph is the record of
what it must also do**: restate §3.4, keep a trade-id-gap test that fails when gaps
disappear silently, and re-run §9 here.

**(iii) Keep the phantom price but stop the stop cascade from reading it.** Fixes the
worst symptom and leaves the collar, the feed and every research reference wrong. It
also creates a second reference price with different rules, which is the kind of thing
that takes two years to remove. Refused.

**(iv) Rewind `tradeSeq` too, for symmetry.** Refused, and the asymmetry is the point.
A sequence number is a **name**; a counter that goes backwards can give one name to two
different prints, which is unrecoverable at every layer above the engine. A price is a
**datum**; restoring it makes it correct again. `REFERENCE-MATCHER.md` §3.4 already
argues the first half; this decision is the second half of the same sentence, and both
belong in the same doc comment so nobody "tidies" one into the other.

### 3.5 The bust precedent points this way, not the other way

[`TRADE-BUST.md`](TRADE-BUST.md) §2 and `bust.go:33-37` are explicit: a bust does not
rewind the book, does not un-fire the stops the print triggered, does not amend the
event that reported the trade, and **does not rewind `LastTradePrice`**. Read as an
outcome, that contradicts §3.3.

Read as a rule, it is §3.3. `Bust`'s own reasoning is "a bust arrives after the market
has moved, and each of those undos would be a second wrong rather than a correction of
the first." The market moved **because the print was published**: subscribers saw it,
stops fired on it, and the collar recentred on it. Unwinding it retroactively would
falsify a number other participants already acted on.

A reversed fill-or-kill print has none of those properties. It is reversed *inside the
same command*, before `emitResult` composes a single event, before any sink is called,
before the `MatchResult` is returned. **Nothing has acted on it, because nothing has
seen it.** The distinguishing rule is one word:

> A published print is permanent, whatever later happens to it. An unpublished one
> never existed.

Both documents should carry that sentence, which is why §8 lists a cross-reference
edit in `bust.go` as a deliverable rather than leaving two documents to be reconciled
by whoever hits them next.

### 3.6 What a real venue does

Two facts, and they agree.

**No production matching engine publishes a last-sale price for an unfilled
fill-or-kill,** because on real venues the FOK/AON evaluation happens *before*
execution: the engine determines that the full quantity is available and only then
prints. There is nothing to reverse, so the question never arises. The speculative walk
here is this engine's implementation choice (§3.4(ii)), and an implementation choice
must not be observable.

**"Last trade price" is defined by reported prints, not by matching attempts.** A
consolidated tape, a drop copy and an execution report all carry executions; an
unfilled fill-or-kill produces none of them, and there is no message on any real
order-entry protocol that says "an execution occurred and was then annulled within the
same message". A venue whose published last price cannot be found on its own tape has
published a number no participant can reconcile.

So the answer from the outside world is the same as the answer from the code: the
current behaviour is not a defensible venue rule that this engine happens to have
chosen. It is a leak.

### 3.7 The three sides of A

| Side | File | Change |
|---|---|---|
| Engine | `pkg/matching/engine.go` | `settleInto` captures `e.book.LastTradePrice()` before `e.match`; the `TIFFillOrKill` branch (`:972-982`) restores it after the `reverseTrade` loop. `recordLast`'s doc comment (`:1668-1675`) gains the definition from §3.3 |
| Model | `internal/refmatch` | `Model.settle`'s FOK branch (`commands.go:92-101`) restores `m.last`, captured before the walk. `execute` (`refmatch.go:583`) is unchanged — it is right that every print sets it; what was missing is the undo |
| Test | `pkg/matching/differential_findings_test.go` | `TestRejectedFOKStillMovesTheLastTradePrice` **inverts**: `LastTradePrice()` must be 0, and the failure message must now say that a non-zero value means the fix regressed. Its header comment loses the "THE SAME COMMIT MUST CHANGE THE MODEL" paragraph for A and gains a pointer here |
| Doc | `docs/REFERENCE-MATCHER.md` | §3.4's parenthetical ("a fourth candidate is not on this list because its status is genuinely open") is resolved and moved into the body as a settled rule; §9(a) and §10.1(a) are marked fixed with the date and this document |

New tests that must exist, because the pinned test only ever covered the value and not
its consequences:

- `TestRejectedFOKDoesNotFireAStop` — the four commands in this document's opening
  block. Asserts `PendingStopCount()` is unchanged, the result carries **zero** trades,
  and the resting maker still holds 3 lots.
- `TestRejectedFOKDoesNotMoveTheBand` — `PriceBand` 10%, a rejected FOK, then a
  `buy 1 @ 150` from an unrelated account which must be **accepted**.

### 3.8 `Fixed`, not `Changed`

By [`COMPATIBILITY.md`](COMPATIBILITY.md)'s rule this is a behaviour change that fixes
a stated contract, with the changelog entry saying so. Nothing exported moves —
`Engine.LastTradePrice`'s signature is untouched and `internal/apicheck`'s
`surface.txt` is unchanged. No integration can have depended on the old value
deliberately, because the old value names a print that does not exist anywhere the
integration can see it.

The entry must still name the two consequences, since both are visible from outside:
**stops that used to fire no longer fire, and a collar that used to be armed by a
rejected order is no longer armed by it.** A `Fixed` entry that hides a behaviour
change behind the word "fixed" is the thing `COMPATIBILITY.md` was written after.

## 4. Defect B — a self-trade-prevented maker vanishes with no event

### 4.1 It is four defects, not one

The pinned test covers one instance. Measured in this slice, the same line
(`engine.go:752`) produces at least four, and the last two are severe:

**(i) `STP CANCEL_OLDEST` (pinned).** Two commands. `u` rests sell 3 @ 100; `u` sends a
fill-or-kill buy 5 @ 100 under `CANCEL_OLDEST`. The maker is cancelled mid-walk
(`engine.go:1309-1313`), the fill-or-kill then fails and is rejected. The book goes
from one resting order to **zero**, and the only published event is `REJECTED`.
`CANCEL_BOTH` (`:1315-1320`) does the same.

**(ii) `STP DECREMENT`, not previously recorded.** Same two commands with
`STPDecrement`. `decrement` (`engine.go:1399-1407`) shrinks *both* sides by their
overlap with no trade to explain it, and here it takes the maker to zero, removes it,
and emits a `Canceled` that is then dropped. Measured after: the maker's `Quantity` is
**0**, it is gone from the book, the level aggregate is 0 — and the taker's own
`Quantity` has been mutated from 5 to **2** on an order that ends REJECTED. One event:
`REJECTED`. The partial-decrement variant is the same defect with `Replaced` dropped
instead of `Canceled`, which leaves a consumer's remaining-quantity accounting
permanently wrong for a maker that is still resting — the exact failure `emitReplaced`
exists to prevent (`engine.go:828-836`).

**(iii) A stop that the same command fires.** The opening block. The stop's
`Triggered`, its `Accepted` and the `Trade` of a **real, standing, settling** execution
are all in `e.pending` when `emitResult` drops the batch. This is the one that makes
"a rejection means nothing happened" (`engine.go:748-750`) false in the strongest
possible way: something happened, to two accounts that were not the rejected one.

**(iv) An OCO primary consumed by a failing fill-or-kill, not previously recorded.**
`u1` posts an OCO — a sell 3 @ 100 primary with a protective stop leg. `u2` sends a
fill-or-kill buy 5 @ 100. The walk fills the primary, `cancelOCOCounterpart` cancels
the stop leg, the fill-or-kill then fails and the primary is restored to the book.
Measured after: the book holds the primary again, `PendingStopCount()` is **0**, and
the stop leg's status is `CANCELLED`. **A client's protective stop was destroyed by a
stranger's rejected order, and the only event was `REJECTED` naming the stranger.**

A correction to an assumption worth writing down because it is the obvious next guess
and it is false: **the book-size cap is not a fifth instance.** A taker that leaves a
remainder must have exhausted at least one maker, so the resting count after it rests
is never higher than before the command, and `ErrOrderBookFull` is therefore
unreachable *after* a print in tier 1. `internal/refmatch`'s `settle` comment
("its prints still stand even though the events announcing them are dropped",
`commands.go:103-110`) describes a state the model can represent and the engine cannot
reach. The rule below is written to be right in that case anyway, because (iii) proves
"rejected with standing prints" *is* reachable by another route.

### 4.2 The two candidates the pinned test names

> "Either the maker must be restored like a traded one, or its cancellation must
> survive the rejection; both are real changes with consequences for replay, so
> neither is made here."

These are materially different claims about what happened. **B1 says the maker was
never removed. B2 says it was removed and stays removed.**

| | B1: restore the maker | B2: publish the events |
|---|---|---|
| Book after | maker back, at the **tail** of its price level — `OrderBook.Add` appends, and no path preserves rank | maker gone |
| Event stream | correctly silent: nothing to announce | `REJECTED` then `CANCELED`, in that order |
| L3 consumer | consistent | consistent |
| Replay | deterministic — commands are replayed, not events | deterministic, identically |
| STP's promise to the account | withdrawn after the fact | kept |
| Cost | a reversal path for **every** non-trade mutation the walk makes | one predicate in `emitResult`, one filter in the FOK branch |
| Generalises to (ii), (iii), (iv) | no — §4.4 | yes, unchanged |

### 4.3 Decision B

> **A rejection drops only the events describing state the engine actually undid.**
> Concretely: `settleInto`'s fill-or-kill branch, which is the only place that knows
> prints are being reversed, removes **its own reversed `EventTrade` entries** from
> `e.pending`. `emitResult` stops clearing the batch wholesale (`engine.go:751-753`)
> and publishes what remains after the `REJECTED`.
>
> An STP-cancelled maker stays cancelled, and its `Canceled` reaches the stream. A
> decremented maker stays decremented, and its `Replaced` or `Canceled` reaches the
> stream. A standing print reaches the stream as an `EventTrade`. A reversed print
> reaches nobody, which is unchanged and correct.

Composition order does not change: the submitted order's own event stays first, then
the pending batch, as `stampAndPublish` already documents (`engine.go:772-786`). A
consumer therefore sees `REJECTED(taker)` before `CANCELED(maker)` even though the
cancellation happened first. That is the same ordering the accepted path already has —
`ACCEPTED` precedes the trades that preceded it — and changing it here would make the
rejected path the only one with a different rule.

### 4.4 Why not B1 — measured, not argued

B1 is not wrong in principle. It is not **composable**, and the walk makes five kinds
of non-trade mutation that a "restore everything" rule would have to reverse: an STP
cancellation, an STP decrement (which mutates the *taker* too), an iceberg refill, an
OCO counterpart cancellation, and the guardrail's window counters. Two measurements
settle it.

**An iceberg exhausted by a failing fill-or-kill already corrupts.** `u1` posts an
iceberg: 9 total, 3 displayed. `u2` sends a fill-or-kill buy 12 @ 100. The walk
consumes each slice, refills from the reserve (`engine.go:1352-1362`) and re-adds the
**same order object**, then fails and reverses. Measured after:

```
slice quantity = 3   remaining = 9   FilledQty = -6   hidden reserve = 0
best ask aggregate = 9 lots on one order whose stated quantity is 3
events: [REJECTED]
```

`FilledQty` is **negative**. `checkInvariants` passes, because
`Filled + Remaining == Quantity` still holds (−6 + 9 = 3) and `Remaining ≥ 0` still
holds — a blind spot recorded in §7. The iceberg's entire reserve has been forced into
the open by an order that was rejected, which destroys the one property an iceberg
exists for. B1 would have to un-refill this; B2 makes no claim about it and does not
make it worse.

**An OCO's protective leg is already destroyed** — §4.1(iv). B1 would have to
re-register a cancelled stop in the stop book and un-cancel it.

Each of those is a new reversal path, in a branch that by construction almost never
runs, with no existing test and no oracle — because icebergs and OCO are **tier 2**
(`REFERENCE-MATCHER.md` §2.4) and the generated tape never draws them. B1 asks for
four new inverse operations to make one true sentence ("nothing happened"). B2 makes
the sentence honest instead ("this is what happened").

And B1 says something false about a risk control. `CANCEL_BOTH` means "cancel them
both". A venue that cancels both, fails to fill, and then quietly gives one of them
back has told the account something and then withdrawn it, with the withdrawal
depending on whether an *unrelated* liquidity condition was met. Under B2 the account
gets what its own STP mode named, every time, regardless of how the taker ended.

### 4.5 What B2 does to replay

Nothing, and this needs saying because the pinned test flags replay as a consequence.
Recovery replays **commands**, not events (`pkg/wal`, `RestoreAfter`). The book state
after the command is identical under B2 — the maker is gone either way, which is the
current behaviour. What changes is only what a *consumer* is told, and a consumer's
view is not an input to recovery. The one place it shows up is
`TestCrashAtEveryBoundary`, which compares a book digest and a trade tape; neither
moves.

### 4.6 Does the reconstruction claim become true? Does `wire.Executed` survive?

**`EventKind`'s doc comment** (`event.go:4-17`) claims Accepted/Trade/Canceled/Replaced
form a stream that reconstructs the L3 book, and says the claim is machine-checked by
`TestEventStreamReconstructsBook`. Today the claim is **false**, and the test is true —
over the ~25 hand-written scenarios it contains, none of which combines fill-or-kill
with self-trade prevention. That is `JOURNAL-COMPLETENESS.md` §1 in a second place: an
exhaustive check over an incomplete input space reporting completeness, with the report
load-bearing.

**After B2 the claim becomes true for the cases in §4.1(i), (ii) and (iii)**, and it
becomes true *in the way that matters* — the mirror is told about every book change.
It is **not** unconditionally true, and this document will not say it is: the iceberg
corruption in §4.4 leaves the engine's own book holding an order whose displayed size
no stream can explain, because the *engine* is wrong there, not the stream. §7 keeps
that open with a pinning test rather than letting the doc comment over-claim again.

So the doc comment must be edited to say what is actually proven, and the proof must
move from 25 hand-written scenarios to the generated tape. That is §8's deliverable 4:
**run `mirrorBook` on the engine's event stream after every command of every
differential tape and compare it to the engine's own book.** That assertion, on
generated input, is what would have caught defect B without anyone predicting it.

**`wire.Executed`'s `LeavesQty`** (`internal/wire/wire.go:617-619`) carries this
justification: "LeavesQty is carried because the event stream is proven to reconstruct
per-order remaining quantity (see TestEventStreamReconstructsBook); without that proof
this field would be a guess and had to be omitted."

Today that justification has a hole, and it is exactly the shape the field cares about:
a client holding a resting order that STP `DECREMENT` shrank inside a rejected
fill-or-kill has a `LeavesQty` that is permanently too large, and a client whose maker
was cancelled has one that should be zero. **Under B2 the justification survives, and
is stronger than it was** — the `Replaced` and `Canceled` that carry the correction now
reach the session layer, and the proof behind the sentence is a generated-tape
assertion rather than a scenario list. The sentence in `wire.go` should be updated to
cite the generated-path check, because the citation is the load-bearing part.

### 4.7 The three sides of B

| Side | File | Change |
|---|---|---|
| Engine | `pkg/matching/engine.go` | `settleInto` records `len(e.pending)` before `e.match`; the FOK branch compacts `e.pending[pendStart:]`, dropping `EventTrade` entries and keeping every other kind in order. `emitResult` (`:744-770`) loses the unconditional `e.pending = e.pending[:0]` on rejection, and its comment ("A rejection means nothing happened") is replaced by the rule in §4.3, which is a different and true sentence |
| Model | `internal/refmatch` | `Model.settle`'s FOK branch (`commands.go:92-101`) records `len(m.pending)` and drops the reversed `EvTrade` entries; `Model.compose` (`commands.go:126-146`) stops clearing `m.pending` on rejection. The comment at `:122-125` — the model's written copy of the engine's defect — is replaced by the rule |
| Test | `pkg/matching/differential_findings_test.go` | `TestSTPCancelledMakerVanishesWithNoEvent` **inverts**: `OrderCount()` is still 0 *and* a `Canceled` naming the maker must be in the batch. It keeps its existing assertions (status REJECTED, book empty) — this is a strengthening, not a replacement |
| Doc | `docs/REFERENCE-MATCHER.md`, `pkg/matching/event.go`, `internal/wire/wire.go` | §3.4's third rule keeps its first half (trade ids gap, event sequence numbers do not) and loses the implication that the whole batch is dropped; §9(b) and §10.1(b) marked fixed; `EventKind`'s doc comment cites the generated-path check |

New tests:

- `TestRejectedFOKAnnouncesTheDecrementedMaker` — §4.1(ii), asserting the `Canceled`
  (maker to zero) and, in a second scenario, the `Replaced` (maker shrunk in place)
  with the right `RemainingQty`.
- `TestRejectedCommandStillPublishesAStandingPrint` — §4.1(iii), asserting the
  `EventTrade` for the stop's real execution reaches the sink even though the command's
  verdict is REJECTED.
- Three scenarios added to `TestEventStreamReconstructsBook`'s table: FOK ×
  `CANCEL_OLDEST`, FOK × `CANCEL_BOTH`, FOK × `DECREMENT`. The scenario list is what
  the doc comment cites, so the combination it lacked is the combination it gains.

### 4.8 `Changed`, not `Fixed`

This one changes the **shape of a published stream**. A consumer that assumed a
rejected command's batch is exactly one `REJECTED` event — a reasonable assumption,
since it was true for every command this engine has ever processed — now sees more
events after it. Adapters at `cmd/obgw`, `pkg/marketdata` and anything built on
`EventSink` are affected.

By [`COMPATIBILITY.md`](COMPATIBILITY.md) §"Pre-1.0, honestly", that requires an entry
under `Changed` naming what breaks, in the same commit. Nothing exported moves, so
`surface.txt` is unchanged; the break is behavioural and the changelog is the only
place it can be announced. The entry must say the sentence a downstream author needs:
**a `REJECTED` batch may now carry `CANCELED`, `REPLACED`, `ACCEPTED`, `TRIGGERED` and
`TRADE` events after it, and a consumer must apply them.**

## 5. Defect C — pro-rata leaves the book crossed

### 5.1 What it does today, measured

Two commands (`TestProRataSelfSkipCrossesTheBook`): `u0` rests **sell 5 @ 99**, then
`u0` buys **5 @ 100**. `matchProRata` excludes the taker's own orders from the level's
eligible set (`engine.go:1592-1596`), finds `total == 0`, and **ends the walk**
(`:1600`). The remainder rests. The book is **bid 100 / ask 99**.

Three further measurements:

- **All five STP modes do it.** `ALLOW`, `CANCEL_NEWEST`, `CANCEL_OLDEST`,
  `CANCEL_BOTH` and `DECREMENT` all produce bid 100 / ask 99, 0 trades, 2 resting
  orders. `matchProRata` never calls `takerSTP` at all. So a venue configured
  `ALLOW` — an account that explicitly asked to be permitted to trade with itself —
  does not get it under pro-rata, and a venue configured `CANCEL_BOTH` gets neither
  order cancelled. **Pro-rata silently overrides the venue's self-trade-prevention
  configuration**, which is a bigger statement than "pro-rata crosses the book".
- **It declines an unrelated account's liquidity and crosses the book doing it.** `u0`
  rests sell 5 @ 99, `u1` rests sell 5 @ 100, `u0` buys 5 @ 100. Result: **zero
  trades**, three resting orders, book crossed at 100/99. `u1` was offering exactly
  what `u0` was bidding, at a price `u0` named, and no trade happened — because the
  walk ends at the touch rather than resolving it.

  Half of that is not pro-rata's fault and the distinction matters for §5.3: under
  price-time priority the same three orders also produce no trade, because the default
  `CANCEL_NEWEST` cancels the taker when it meets its own order at 99. What price-time
  priority does *not* do is leave the book crossed. Under §5.3's rule the mode decides
  both halves — `CANCEL_NEWEST` still declines and cancels the taker, and
  `CANCEL_OLDEST`, `DECREMENT` and `ALLOW` clear the 99 level and go on to trade with
  `u1`.
- **The crossed state persists and then implicates strangers.** On the pro-rata sweep
  at seed 25 the skipped order left before anything cleared the crossing, after which
  the touch was a crossing between two **unrelated** accounts
  (`REFERENCE-MATCHER.md` §10.1(c)).

A crossed continuous book is the invariant this engine advertises hardest. It is the
first assertion in `checkInvariants` and the first row of §1.1's table in
`REFERENCE-MATCHER.md`.

### 5.2 The options, and what each costs

| Option | Book uncrossed? | Cost |
|---|---|---|
| **C1 — do not skip; apply STP, as price-time priority does** | yes, in all five modes | roughly 25 lines in `matchProRata` and the same in the model; changes a verdict for takers that meet their own liquidity under pro-rata |
| C2 — skip, then reject the remainder | yes | a GTC limit is refused for a reason unrelated to it; a partially-filled order ends REJECTED, which no other path does; the client is told "no" to an order the venue could have accepted at a legal price |
| C3 — skip, then rest the remainder at a non-crossing price | yes | the venue silently reprices a client's order. Its price appears in market data, in the client's own reconciliation and in its execution report, and it is not the price the client sent. Sliding is a real venue mechanism for post-only entry; applying it to a taker's remainder is a lie about what was submitted |
| C4 — skip, and continue the walk past the level | **no** | by construction: the taker consumes deeper levels at **worse** prices while its own better-priced order sits at the touch, and if it still has a remainder it rests across the spread anyway. It reduces the frequency of the defect and prints trades that violate price priority to do it |
| C5 — skip, then cancel the remainder | yes | hard-codes `CANCEL_NEWEST` regardless of the mode the taker asked for. It is C1 with four of the five answers deleted |

### 5.3 Decision C

> **A pro-rata taker does not skip its own liquidity. Self-trade prevention applies at
> a pro-rata level exactly as it applies under price-time priority, and the allocation
> rule has no opinion about who owns what.**

The specification paragraph — this replaces the last sentence of
`REFERENCE-MATCHER.md` §2.3's pro-rata block, and the model implements *this
paragraph*, not the engine's loop:

> At each price level the taker touches, resting orders are partitioned by the
> self-match test into the taker's own and the rest. The taker is allocated pro-rata
> across **the rest**, exactly as specified above. If that exhausts the taker, the
> walk ends and self-trade prevention never fires — the taker never needed its own
> liquidity. If the taker still needs quantity at that level and the only orders left
> there are its own, self-trade prevention is applied to them in arrival order under
> **the taker's mode**: `CANCEL_NEWEST` cancels the taker and ends the walk;
> `CANCEL_OLDEST` removes the maker, emits its `Canceled`, and re-allocates whatever
> remains at the level; `CANCEL_BOTH` does both and ends the walk; `DECREMENT` shrinks
> both by their overlap, emits `Replaced` or `Canceled`, and continues; `ALLOW`
> includes the order in the allocation and trades with it. A level is never skipped,
> and a taker never rests at a price that crosses a resting order it could have been
> prevented against.

Two things about that paragraph are choices rather than consequences, so they are
stated rather than left to the code:

**Eligible liquidity trades first; STP fires when the taker's own order would be
needed.** This is the position-free analogue of what price-time priority does. Under
FIFO the taker trades with everything ahead of its own order and STP fires when the
walk *reaches* it; whether anything trades first depends on queue position. Pro-rata
has no queue position, so "reaches it" has to be defined, and the only definition that
does not invent an ordering is "the level's eligible liquidity is exhausted and the
taker still wants more". The cost, stated: a `CANCEL_NEWEST` taker under pro-rata may
print *more* than the same taker under FIFO would have, if its own order happens to
sit at the front of the FIFO queue. That is a difference between two allocation
policies, which is what an allocation policy is.

**The rule is smaller than it looks.** In every case where the taker's own liquidity is
not needed, the allocation is byte-for-byte what it is today: self orders are already
excluded from `total`, and `q := min(taker.RemainingQty, total)` is unchanged. The only
behaviour that moves is the branch that currently ends the walk (`engine.go:1600`) —
which is the branch that produces the crossed book.

### 5.4 Why not the alternatives

C2 and C3 both accept the premise that a taker's own resting order is an obstacle to be
worked around. It is not: it is a self-match, and this venue already has a configured,
per-order, five-way answer for a self-match. Inventing a sixth answer that applies only
under pro-rata means the venue's STP configuration means one thing under one allocation
policy and another thing under the other — and nothing in the configuration says so.

C4 is refused on a measurement rather than a principle: it does not uncross the book,
and it buys that failure by printing through a better price. C5 is C1 with the
configuration ignored.

The deepest reason for C1, though, is that it **deletes a rule instead of adding one**.
`REFERENCE-MATCHER.md` §2.3 already flags the skip as arbitrary — "nothing in market
microstructure forces it, and a different venue would round differently" — and that
document's §1.3 warns that a rule living only in two implementations has no oracle,
just two copies. The skip was exactly that. Removing it leaves the venue with one
self-match rule instead of two, and the one that remains is the one with five
hand-written tests, a model, and a per-mode coverage guard.

### 5.5 The sides of C — four, not three

| Side | File | Change |
|---|---|---|
| Engine | `pkg/matching/engine.go` | `matchProRata` (`:1567-1636`): the `total == 0` break becomes the STP branch, and the exhausted-eligible-liquidity case reaches it too. `matchProRata` begins calling `takerSTP`, `decrement` and `emitCancel`, which it never has |
| Model | `internal/refmatch/refmatch.go` | `Model.matchProRata` (`:668-717`) implements §5.3's paragraph; `m.stpDecisions` starts being incremented on the pro-rata profile, which strengthens `TestSelfTradePreventionModesAreAllReached` at no cost |
| Spec | `docs/REFERENCE-MATCHER.md` §2.3 | the prose paragraph is the *primary* artefact for C: the model implements the paragraph, so the paragraph is the thing under review. Editing the code without editing it re-creates the transcription that document's §2.2 warns about |
| Test | `pkg/matching/differential_test.go`, `differential_findings_test.go` | `TestProRataSelfSkipCrossesTheBook` **inverts** to assert the book is not crossed and the taker is cancelled. `crossingIsExpected`, `crossState` and `crossSeen` are **deleted**, restoring `checkInvariants` at full strength on the pro-rata profile |

The deletion in the last row is the one to read carefully against this slice's
constraint that no assertion may be weakened. It is the opposite: the pro-rata profile
currently runs a **narrowed** crossed-book assertion with a written exemption, and
after C1 it runs the same full-strength assertion as every other profile. The
narrowing was correct while the defect stood, and its written reason is what makes its
deletion checkable.

New tests: `TestProRata_STPModeDecides` — the five-mode table from §5.1's measurement,
inverted, asserting for each mode the outcome §5.3 names, and asserting for all five
that the book is not crossed; and
`TestProRata_ReachesUnrelatedLiquidityUnderCancelOldest` — the three-order case under
`CANCEL_OLDEST`, asserting the trade with `u1` that today does not happen. The second
is deliberately not written against the default mode: under `CANCEL_NEWEST` the taker
is cancelled and the trade correctly still does not happen, which §5.1's second bullet
records so that the test is not written to the wrong expectation. A third,
`TestProRata_MixedLevelTradesEligibleBeforeSTPFires`, holds the ordering choice §5.3
makes — a level carrying both the taker's own order and a stranger's, under
`CANCEL_NEWEST`, must print the stranger's lots before the taker is cancelled. It is
the only case that distinguishes §5.3's rule from applying STP first, which is why
sabotage row 10 is written against it.

### 5.6 `Changed`, not `Fixed`

A verdict changes under a supported configuration: with `ProRata` on, a taker that
meets its own resting liquidity used to rest and now (under the default
`CANCEL_NEWEST`) is cancelled. `COMPATIBILITY.md` counts "tightening what a function
accepts" as breaking, and this is its sibling — the same input now produces a different
outcome for a documented configuration. `Changed`, with the entry naming the
configuration and the five outcomes.

## 6. Rules that will look like bugs

Each of these is intended behaviour after this slice, and each will be reported as a
defect by someone reading the code or the stream. Every one is paired with the reading
that makes it look wrong, because a rule without that pairing gets "fixed" by the next
person.

| Rule | Why it will look like a bug |
|---|---|
| A rejected fill-or-kill still burns **trade ids**, leaving gaps, while `LastTradePrice` goes back | It looks inconsistent: one thing is rewound and another is not, in the same branch. The difference is that an id is a *name* — a counter that goes backwards can give one name to two prints — and a price is a *datum*. §3.4(iv) |
| A rejected command can publish `CANCELED`, `REPLACED`, `TRIGGERED`, `ACCEPTED` and `TRADE` events after its `REJECTED` | "The order was rejected, so why is the venue telling me things happened?" Because they did, to *other* orders. §4.1 |
| `REJECTED` is published **before** the `CANCELED` of a maker that was removed earlier in the same walk | The event order does not match the causal order. It matches every other command's composition, where `ACCEPTED` also precedes trades that preceded it (`engine.go:772-786`). Making the rejected path the exception would be the real bug |
| A reversed fill-or-kill print still reaches **no one**, even after §4 | It looks like §4's rule was applied inconsistently. It was applied exactly: the rule drops what was undone, and those prints were undone |
| An unfilled fill-or-kill leaves `LastTradePrice` at a value **older than the command** | On a busy venue this reads as a stale feed. It is the definition: the price of the last **published** print |
| Under pro-rata, a `CANCEL_NEWEST` taker can print more than the same taker under FIFO | It looks like STP is weaker under pro-rata. It is the same rule with no queue position to fire at; §5.3 states the choice |
| Under pro-rata, a level with only the taker's own liquidity is **consumed or cancelled**, never skipped | Someone will read "self orders are skipped" in an old comment and report the new behaviour as a regression. The old comment is what this slice deletes |
| `TestProRataSelfSkipCrossesTheBook` and its two neighbours **assert the opposite of their names** after this slice | An inverted pinning test reads as a test that was flipped to make a change pass. It is the mechanism that forced this document to exist: the tests carry the sentence a fix must come and change |

## 7. What this deliberately does not do

- **It does not fix the iceberg corruption in §4.4.** A fill-or-kill that exhausts an
  iceberg's reserve and then fails leaves an order with `FilledQty = -6`, nine
  displayed lots against a stated quantity of three, and an empty reserve. That is a
  defect in `reverseTrade`'s interaction with the refill path, not in the event stream,
  and B2 neither fixes nor worsens it. **It gets a pinning test in this slice**
  (`TestFailingFOKCorruptsAnIcebergsReserve`) and its own future slice, because a
  finding that is measured and left unpinned is how it gets found a third time.
- **It does not fix the OCO leg in §4.1(iv).** Same shape: a stranger's rejected
  fill-or-kill destroys a client's protective stop. Pinned by
  `TestFailingFOKCancelsAnOCOStopLeg`, deferred with the iceberg, and both belong with
  tier 2's exotics work where the model can finally hold an opinion about them.
- **It does not add `FilledQty ≥ 0` to `checkInvariants`** — though it records that the
  invariant suite passed on an order with `FilledQty = -6`, because
  `Filled + Remaining == Quantity` and `Remaining ≥ 0` both still held. Adding the
  assertion belongs with the fix it would catch, not with a slice that would then ship
  a red suite.
- **It does not rewind the self-output guardrail's window counters** for reversed
  prints. A rejected fill-or-kill's reversed prints count toward `MaxTrades` and
  `MaxNotional` (`engine.go:1693-1710`) and can trip the venue into `Halted`. That is
  arguably wrong by §2.1's rule — but the trip is published **immediately and
  directly** by `emitStateChange` (`engine.go:860-867`), not through `e.pending`, so by
  §2.2's test it is announced rather than hidden, and an already-published halt cannot
  be un-published. Left as-is, deliberately, and recorded here so it is not mistaken
  for an oversight.
- **It does not change how fill-or-kill decides fillability.** §3.4(ii) is the root
  repair and it is refused with reasons; this slice makes the reversal total instead of
  making the reversal unnecessary.
- **It does not touch the exported surface.** No signature, field, method or type
  moves. `internal/apicheck/testdata/surface.txt` must be **byte-identical** after this
  slice; if it is not, the change was not one of these three and needs its own
  argument.
- **It does not model icebergs, OCO, stops or the auction.** Tier 1 is unchanged
  (`REFERENCE-MATCHER.md` §2.3), and two of the four instances in §4.1 are outside it —
  which is why they are held by hand-written tests, exactly as §1.3 of that document
  requires for any rule the model cannot check.

## 8. Deliverables and acceptance criteria

| # | Deliverable | Done when |
|---|---|---|
| 1 | Defect A, three sides | `TestRejectedFOKStillMovesTheLastTradePrice` inverted and green; `TestRejectedFOKDoesNotFireAStop` and `TestRejectedFOKDoesNotMoveTheBand` exist and are green; `recordLast`'s doc comment carries §3.3's definition |
| 2 | Defect B, three sides | `TestSTPCancelledMakerVanishesWithNoEvent` inverted, keeping all four of its current assertions; `TestRejectedFOKAnnouncesTheDecrementedMaker` and `TestRejectedCommandStillPublishesAStandingPrint` green; three new scenarios in `TestEventStreamReconstructsBook` |
| 3 | Defect C, four sides | `TestProRataSelfSkipCrossesTheBook` inverted; `TestProRata_STPModeDecides` covers all five modes; `crossingIsExpected`, `crossState` and `crossSeen` deleted and `checkInvariants` unconditional in `runDiff`; `REFERENCE-MATCHER.md` §2.3's paragraph rewritten before the model is |
| 4 | **The stream check moves to the generated path** | `runDiff` (full mode) feeds the engine's event batch into a `mirrorBook` and compares the mirror against the engine's own book **after every command of every tape** — 2,240 commands rather than 25 scenarios. This is the assertion that would have caught defect B unpredicted, and it is the deliverable that stops the next one needing a prediction |
| 5 | The differential sweep is green **whole** | `TestDifferentialTape` passes on all three profiles and all 16 seeds with all three sides changed, and `go test -race ./...` passes across every package |
| 6 | The oracle is not weaker | All **21** mutations of `REFERENCE-MATCHER.md` §7.1/§10.2 are re-run and all 21 are still caught, each still shrinking to ≤ 4 commands. A fix that makes the harness miss a mutation is a regression in the thing that found these defects |
| 7 | Two findings pinned, not fixed | `TestFailingFOKCorruptsAnIcebergsReserve` and `TestFailingFOKCancelsAnOCOStopLeg` exist, assert today's wrong behaviour, and each carries the sentence a fix must come and delete |
| 8 | The record is updated where it is wrong | `REFERENCE-MATCHER.md` §3.4, §9(a), §9(b), §10.1(a)-(c) marked resolved with dates; `EventKind`'s doc comment cites the generated-path check; `internal/wire/wire.go:617-619`'s `LeavesQty` justification cites it too; `bust.go` carries §3.5's one-line rule |
| 9 | The changelog says which is which | One `Fixed` entry (A, naming the stop and collar consequences) and two `Changed` entries (B, naming the new event shape; C, naming the configuration and the five outcomes) |
| 10 | Surface frozen | `internal/apicheck/testdata/surface.txt` byte-identical; `gofmt -l` clean on every touched file |

### 8.1 The numbers to record when it is done

None is a pass criterion on its own; all are what §10 will be graded against.

- Runtime of `TestDifferentialTape` before and after deliverable 4. It is **0.26 s**
  today for 2,240 commands. A mirror rebuild per command is O(events), so the expected
  change is small; if it goes past 5 s the answer is a shorter tape, never a sampled
  one (`REFERENCE-MATCHER.md` §9).
- How many of the 16 committed tapes change their observation stream at all after A,
  after B, and after C. The prediction from the previous slice is 4 seeds for A and 3
  for B; C should touch the pro-rata profile only. **A number that comes back much
  larger means one of these decisions is wider than this document thinks.**
- STP decisions per mode across the sweep, before and after C, from
  `Model.STPDecisions()`. The pro-rata profile contributes zero today.
- Whether the pro-rata profile's tapes produce *more* trades after C, and *more*
  cancellations. Both should rise — the walk no longer abandons a level, so eligible
  liquidity behind a self order now trades, and `CANCEL_NEWEST` now cancels takers that
  used to rest. Record both, because only one of them is the improvement.

## 9. Sabotage runs required before this counts as done

§8 asks tests to pass. This section breaks the **fix** and requires the new tests to
fail. Per [`TESTING.md`](TESTING.md), nothing above counts until each row has been run.

| # | Sabotage | Must fail |
|---|---|---|
| 1 | Fix the engine for A, leave the model | `TestDifferentialTape` on `fifo/seed=3`, `fifo/seed=8`, `prorata-shard7/seed=4`, `capped-shard3/seed=42`, class `last-trade-price`. **Record the count**, since a different set means the tapes moved |
| 2 | Fix the model for A, leave the engine | `TestDifferentialTape` on the same seeds, **and** `TestRejectedFOKDoesNotMoveTheBand`. The cover-up direction must be red too, or the model is the only oracle for A |
| 3 | Fix the engine for B, leave the model | `TestDifferentialTape`, class `event-count`, on `fifo/seed=3`, `fifo/seed=34`, `capped-shard3/seed=42` |
| 4 | Fix the model for B, leave the engine | `TestDifferentialTape` and `TestSTPCancelledMakerVanishesWithNoEvent` |
| 5 | Restore `LastTradePrice` from the **post**-match value instead of the pre-match one | `TestRejectedFOKDoesNotMoveTheBand` and `TestRejectedFOKDoesNotFireAStop`. A no-op "restore" is the plausible-looking version of this fix |
| 6 | Restore `LastTradePrice` in `Match` instead of `settleInto` | A cascade-fired fill-or-kill stop must produce a wrong reference. If nothing fails, the tape does not reach a stop that is a fill-or-kill, and **that is a generator finding to record**, not a licence to move the code |
| 7 | Invert B's filter: drop the events that stand and keep the ones that were reversed | `TestDifferentialTape` (class `event-count` or `event-kind`) and deliverable 4's mirror check |
| 8 | Keep B's fix but drop `EventReplaced` from the surviving batch | `TestRejectedFOKAnnouncesTheDecrementedMaker`, and the mirror check on the generated path. This is the row that proves `Replaced` is load-bearing for `wire.Executed`'s `LeavesQty` |
| 9 | Delete deliverable 4's mirror check and re-run the **pre-fix** engine | Everything goes green. The point of the row is that it must, which is what establishes the mirror check is what catches defect B rather than being carried by a neighbour |
| 10 | Under C, apply STP **before** allocating the level's eligible liquidity | `TestProRata_MixedLevelTradesEligibleBeforeSTPFires`, and a `trade-count` divergence on the pro-rata sweep. The distinguishing case is a level holding **both** the taker's own order and a stranger's under `CANCEL_NEWEST`: allocate-first prints the stranger's lots and then cancels the taker, STP-first prints nothing. A one-sided level does not distinguish them, which is why the test is written on a mixed one |
| 11 | Under C, honour STP but keep `total == 0` ending the walk | `TestProRata_STPModeDecides` for `CANCEL_OLDEST`, plus the now-unconditional crossed-book assertion in `runDiff` |
| 12 | Implement B1 (restore the STP-cancelled maker) instead of B2 | Nothing need fail — the row requires a **measurement**: run §4.4's iceberg and OCO cases under B1 and record what B1 would additionally have to reverse. This is the row that documents why B2 was chosen, in numbers rather than in argument |
| 13 | Re-run all 21 mutations of `REFERENCE-MATCHER.md` §10.2 against the fixed engine | All 21 still caught. Any that goes green means this slice weakened the oracle while fixing the defects it found |

Rows 2, 4, 9 and 12 are the unusual ones. Two of them ask the *model* to be broken
instead of the engine, because §1.2's cover-up direction has no test today. Row 9 asks
the suite to pass against a broken engine, to prove which assertion is doing the work.
Row 12 asks for a number instead of a failure, because the alternative that was refused
should be refused with evidence.

## 10. How this can fail, stated in advance

So that whoever implements this is not graded on a curve.

- **The decisions may be right and the blast radius wrong.** §8.1 asks how many tapes
  change. If B's fix moves far more than three seeds, the events being published are
  not only the four instances in §4.1, and the extra ones need reading before the sweep
  is re-greened. The temptation at that moment will be to accept the new observations
  as "obviously the fix working". They must be read one at a time.
- **C may uncross the book and change more prints than expected.** Applying STP where a
  skip used to happen means levels that used to be abandoned are now consumed, which
  changes allocation *downstream* of the level for the rest of the tape. The pro-rata
  profile's 5 seeds are the whole evidence base for a new matching rule, which is thin.
  If C lands, the pro-rata profile should get more seeds in the same slice.
- **Deliverable 4 may be redundant, and that would be worth knowing.** The differential
  harness already compares the event list elementwise against the model. If the model
  is right, a mirror-versus-book check might add nothing — except that the model was
  *wrong here*, canonically, which is exactly the case the mirror does not depend on.
  Sabotage row 9 is the test of whether it earns its place; if it goes red with the
  pre-fix engine only because the model does too, the row's finding is that the mirror
  is decoration and it should be argued down rather than kept out of politeness.
- **§4.6's claim may be over-stated.** "The reconstruction claim becomes true" is
  asserted for the cases in §4.1 and explicitly denied for the iceberg. If a fifth
  instance exists that neither this document nor the mirror check reaches, the doc
  comment on `EventKind` will be over-claiming **again**, one slice after being
  corrected for it. The mitigation is that the claim now cites a generated-path
  assertion rather than a scenario list, so the next counter-example arrives as a
  failing test rather than as a reader's report.
- **The `Changed`/`Fixed` split may be wrong for A.** It is argued from "no consumer
  could have depended on the old value deliberately." Someone with a stop-triggering
  integration test built on this engine's current behaviour would disagree, and they
  would be describing a real break. The changelog entry naming both consequences is the
  hedge; if a user reports it, A is retro-classified in the release notes rather than
  argued with.
- **Two of the four instances in §4.1 live outside tier 1**, so the model cannot hold
  an opinion about them and only hand-written tests will. That is the standing limit of
  this harness (`REFERENCE-MATCHER.md` §2.4), and it is the reason §7 pins the two
  deferred findings instead of trusting the sweep to keep finding them.

## 11. What implementing it measured

Written after the code, as §10 said it would be. Three of the decisions above were
carried out unchanged; three of the *deliverables* were not reachable as written, and
each of those is a finding rather than a shortfall.

### 11.1 The numbers §8.1 asked for

| | before | after |
|---|---|---|
| `TestDifferentialTape`, 2,240 commands | 0.26 s | **0.27 s** |
| commands leaving the book crossed, `prorata-shard7` (700 commands) | **107** | **0** |
| prints, `prorata-shard7` | 77 | **82** |
| `CANCELED` events, `prorata-shard7` | 71 | **79** |
| prints, `fifo` / `capped-shard3` | 142 / 59 | **142 / 59** (unchanged) |
| `CANCELED` events, `fifo` / `capped-shard3` | 177 / 51 | **182 / 53** |
| STP decisions by mode across the whole sweep | `[47 9 5 18 4]` | **`[61 13 5 18 5]`** |

Both of §8.1's predictions for C hold: the pro-rata profile produces **more trades and
more cancellations**, and C touches the pro-rata profile only — the fifo and capped
print counts do not move at all, which is the blast-radius check §10's first bullet
asked for. The five extra `CANCELED` events on `fifo` and two on `capped-shard3` are
defect B's, and they are the only thing B changes on those profiles.

The STP-decision row is the one to read carefully. Pro-rata contributed **zero**
decisions before and contributes 19 now, but they are all `CANCEL_NEWEST` (+14),
`CANCEL_OLDEST` (+4) and `ALLOW` (+1): `CANCEL_BOTH` and `DECREMENT` are unchanged, so
the pro-rata profile still never reaches those two at a level. That is a **generator
finding**, not a fix defect — the sweep's evidence for two of the five new answers is
hand-written tests only, which is thin in exactly the way §10's second bullet warned.

Adding deliverable 4's mirror rebuild did not measurably slow the sweep, so §8.1's
"if it goes past 5 s the answer is a shorter tape" never arose.

### 11.2 Three deliverables that were not reachable as written

**(i) `TestRejectedCommandStillPublishesAStandingPrint` cannot exist, because §3
closes the case §4.1(iii) was measured on.** That measurement — a rejected command
whose stop cascade really printed 2 lots between two other accounts — was reachable
only because the rejected order's phantom `LastTradePrice` fired the stop. Fixing A
closes it, and `TestRejectedFOKDoesNotFireAStop` is what holds it closed. After A, a
`REJECTED` verdict cannot carry a standing print at all: the only rejection that does
not reverse its prints is the book-size cap on the resting branch, and a taker with a
remainder has emptied at least one maker, so the resting count after it rests is never
higher than before the command (§4.1's own note, generalised). The replacement is
`TestRejectedFOKAnnouncesAStandingCancellation`, which carries the same load on
§4.1(iv): a stranger's rejected fill-or-kill destroys a client's protective OCO leg,
the destruction stands, and it is now announced.

**(ii) `EventReplaced` can never appear in a rejected batch**, so §4.1(ii)'s
partial-decrement variant and §9's sabotage row 8 have no test to fail. The proof is
two lines: `decrement` shrinks both sides by `min(taker.RemainingQty,
maker.RemainingQty)`, so afterwards at least one side is zero; if the maker survives
then the taker is at zero, and a taker at zero is `IsFilled`, so the fill-or-kill
branch reports FILLED and never rejects. **Measured as well as argued**: keeping B's
fix and additionally dropping `EventReplaced` from the surviving batch leaves
`go test ./pkg/matching ./internal/refmatch` fully green. `Replaced` is therefore
load-bearing for `wire.Executed`'s `LeavesQty` on the ACCEPTED path, where
`TestRejectedFOKAnnouncesTheDecrementedMaker`'s second subtest asserts it and states
the unreachability so nobody writes the test that cannot fail.

**(iii) Sabotage row 6 had no test until this slice wrote one.** Row 6 predicted that
restoring `LastTradePrice` in `Match` instead of `settleInto` would fail something,
and said that if nothing failed it was a generator finding. Nothing failed: stops are
tier 2, so no differential seed ever fires one, and no hand-written scenario combined
a cascade-fired stop with a fill-or-kill. `TestNestedFOKRestoresItsOwnWalksReference`
is that scenario — a command that genuinely prints at 95 and fires a stop whose own
fill-or-kill then fails — and it fails with `Match`-level capture, which erases the
command's own published print. Row 6 now has teeth.

### 11.3 A fourth instance of the same family, found by deliverable 4

The generated-path mirror check immediately found a stream imperfection nobody
predicted, on `fifo/seed=5` command 133 and `capped-shard3/seed=7` command 118: an
order that STP `DECREMENT` empties inside its own command is announced `ACCEPTED`
**with `Quantity` 0** and never announced again, because a taker at zero remaining is
reported FILLED. A consumer keying on order id keeps a zero-lot entry forever.

It is not a phantom *resting* order — the size is zero — and it is not one of the
three decisions, so it is handled in the mirror rather than in the engine: an order
announced at zero size cannot rest, so the reconstruction does not add it, and the
rule is written where a reader will find it. Whether the engine should instead close
such an order out with a `CANCELED` is a real question and belongs with the tier-2
exotics slice; recording it here is what stops it being found a third time.

### 11.4 What was run

- Every new and inverted test was run against the **pre-fix** engine and model and
  fails there: the three inverted pins, the two consequence tests for A, the three
  event-stream tests for B, the three pro-rata tests for C, and the three new
  fill-or-kill × STP scenarios in `TestEventStreamReconstructsBook`.
- **Engine fixed, model left alone** (§9 rows 1 and 3): 8 of the 16 subtests of
  `TestDifferentialTape` fail — `fifo/seed=3`, `fifo/seed=34`, `capped-shard3/seed=7`
  and `capped-shard3/seed=42` with class `event-count`; `fifo/seed=8` and
  `prorata-shard7/seed=4` with class `last-trade-price`; `prorata-shard7/seed=1` and
  `prorata-shard7/seed=9` with class `verdict`. Every shrunk reproduction is 2 or 3
  commands. The predicted set was 4 seeds for A and 3 for B; `capped-shard3/seed=7` is
  the one addition, and the two `verdict` failures are C, which had no prediction.
- **Model fixed, engine left alone** (§9 rows 2 and 4, the cover-up direction): red
  too, on the same eight subtests.
- **Both sides pre-fix, harness as fixed** (§9 row 9's inverse): deliverable 4's
  mirror check alone fails 4 seeds, and the now-unconditional crossed-book assertion
  fails 3 more. The mirror earns its place — it catches defect B on generated input
  where the model comparison is silent by construction.
- **Sabotage 5** (restore from the post-match value): fails all three of A's tests.
  **Sabotage 6** (restore in `Match`): fails `TestNestedFOKRestoresItsOwnWalksReference`.
  **Sabotage 7** (invert B's filter): fails `TestSTPCancelledMakerVanishesWithNoEvent`
  and six differential subtests, four of them through the mirror's "trade against
  order N never announced". **Sabotage 8**: green, which is §11.2(ii).
  **Sabotage 10** (prevention before allocation): fails
  `TestProRata_MixedLevelTradesEligibleBeforeSTPFires` and four pro-rata subtests with
  class `verdict`. **Sabotage 11** (keep `total == 0` ending the walk): fails
  `TestProRataSelfSkipCrossesTheBook`, four of `TestProRata_STPModeDecides`'s five
  modes, `TestProRata_ReachesUnrelatedLiquidityUnderCancelOldest` and the differential
  sweep.
- `internal/apicheck`'s `surface.txt` is **byte-identical**: nothing exported moved.
- `go build ./...`, `go vet ./...` and `go test ./... -count=1` are green across all
  30 packages; `go test ./pkg/matching ./internal/refmatch ./internal/tape -race` is
  green.

### 11.5 What was NOT run, and it is a gap

**§8's deliverable 6 — re-running all 21 mutations of §7.1/§10.2 against the fixed
engine — was not carried out.** It is 21 separate engine edits and it was not done, so
this slice cannot claim the oracle is provably no weaker; it can only claim that
nothing in it was weakened by construction. Three things point the right way and none
of them is that measurement: the pro-rata profile's `crossingIsExpected` narrowing is
**deleted** rather than adjusted, so `checkInvariants` now runs at full strength on all
three profiles instead of two; a per-command event-stream reconstruction check was
**added** to the generated path; and every existing assertion in the two inverted
pinning tests was kept rather than replaced. Deliverable 6 remains open and should be
the first thing the next slice runs.

