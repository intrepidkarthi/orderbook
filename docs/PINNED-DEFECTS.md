# Pinned Defects — One Rejection That Fails to Undo, One That Fails to Announce

Status: **implemented** — the spec was written before the code, as this repository does
it, and §13 is what implementing it measured. §3, §4 and §5 each end in a decision; §10
is how a reviewer checks it was carried out; §11 is the list of sabotages that had to be
red before it counted, now with its results; §12 is how this can still be wrong. Every
number in §1–§12 was measured on a scratch prototype of the design in §3.3, §4.3 and
§5.3, built to check the design and discarded before this was written — so they are
*checked expectations*; §13 has the same measurements taken again from the shipped tree,
including the four places this document turned out to be wrong (§13.2, §13.5, §13.6,
§13.7) and the two the code turned out to be wrong about it (§13.6, §13.8) ·
Author: Karthikeyan NG · Last updated: 2026-08-18

> **The measurement that decides the first one.** A client posts an iceberg: 30 lots
> total, 5 displayed, 25 in reserve. A stranger sends a fill-or-kill for 100 lots. It
> cannot fill, every print is reversed, and it is **REJECTED**.
>
> What the venue then holds, measured — and this is the same order, worked off by
> three ordinary 5-lot buys afterwards, with and without that one rejected command:
>
> ```
> without the rejected fill-or-kill: displayed 5, 5, 5   reserve 20, 15, 10
> with    the rejected fill-or-kill: displayed 25, 20, 15  reserve 0, 0, 0
> ```
>
> The reserve is gone. Not spent — **displayed**. The client's remaining size is
> standing in the open where the client never put it, `FilledQty` is **−20**, and the
> order goes on trading from there for the rest of its life. An iceberg exists to hide
> size. One rejected order from an unrelated account leaks all of it, permanently, and
> nothing on the stream or in the invariant suite says a word.
>
> **The measurement that decides the second one.** A stop fires inside a cascade, its
> order is a fill-or-kill that cannot fill, and the venue refuses it. The stream says
> `TRIGGERED`, then `ACCEPTED`, and then nothing, ever. A consumer rebuilding the book
> holds `3@200:50` forever; the engine holds nothing. The venue's own `pkg/marketdata`
> L2 feed publishes **46 lots at 200** that its book does not have.

Companion documents:
- [`DIFFERENTIAL-FINDINGS.md`](DIFFERENTIAL-FINDINGS.md) §4.4 and §7 — where both of
  these were measured, argued about, and deliberately left. §7's first and third
  bullets are the two sentences this slice comes to delete. §2.1 is the rule both
  defects break and this document does not restate.
- [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §5.5 — "a behaviour the corpus never
  reaches is a behaviour nobody can bump for." §6 below is that sentence turning out to
  be true of both of these, measured, and what it costs to close.
- [`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §2.4 — icebergs, stops and OCO are
  **tier 2**. The model does not have them, this slice does not give it them, and
  therefore neither defect is reachable by the differential harness. §9 says so as a
  decision rather than leaving it as an omission.
- [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §1 — "an exhaustive check over
  an incomplete input space reports completeness, and the report is load-bearing." §5
  and §6 are that lesson in two more places: an invariant suite that checks the one
  order that cannot be the victim, and a fingerprint whose corpus never reaches either
  path.
- [`COMPATIBILITY.md`](COMPATIBILITY.md) §"The rule" — §3.8 and §4.6 apply it to each
  fix and reach two different answers, for the same reason §3.8 and §4.8 of
  `DIFFERENTIAL-FINDINGS.md` did.
- [`TESTING.md`](TESTING.md) §"The rule" — a test does not count until it has been run
  against code broken the way it claims to detect. §11 is this slice's list.

---

## 1. Why this document exists

### 1.1 Two pins, and the sentence each carries

Both defects are live on `main`, both were found by adversarial review of an earlier
slice rather than by a failing test, and both were pinned instead of repaired:

| Pin | What it asserts today |
|---|---|
| `TestFailingFOKCorruptsAnIcebergsReserve` | `base.FilledQty` is **negative**, and the best ask shows **more lots than the order's stated quantity** |
| `TestCascadeFiredStopRejectedLeavesAPhantom` | the mirror's book and the engine's book have **different lengths**, and the difference is the rejected order |

Each carries the sentence a fix has to come and delete — "if this is no longer
negative the defect has been FIXED", "that is the fix this test was waiting for" — and
`pkg/matching/event.go:29-41` names both as exclusions from the claim that the event
stream reconstructs the L3 book. This slice is that fix. A pin keeps its name after
inversion (`differential_findings_test.go:12-17`): renaming it hides the promise being
kept.

### 1.2 The family, and the member already fixed

Both belong to one family: **a rejection path that fails to undo, or fails to announce,
what it did on the way.** The self-trade-prevention member of that family was fixed in
the previous slice — a maker removed mid-walk by `CANCEL_OLDEST` inside a fill-or-kill
that then failed, gone from the book with nothing on the stream to say so
(`DIFFERENTIAL-FINDINGS.md` §4). These two are what it left, and they are the two
halves of the family that slice did not have:

- Defect 1 is a failure to **undo**, in the engine's own state. The stream is not
  wrong; it faithfully reports a book that is.
- Defect 2 is a failure to **announce**, in the stream only. The engine's state is
  right; every consumer's copy of it is wrong.

They ship together because they are one review's findings about one branch, and
because the invariant in §5 is the answer to "what would have caught either of them
without a reviewer" — which is a question about the family, not about either defect.

### 1.3 What is decided here, and what is not

Decided: who owns the rewind when a refill and a reversal disagree (§3); how a cascade
reports an order it refused (§4); which invariant closes the blind spot that let defect
1 pass a green suite (§5); what the fingerprint corpus has to gain before either fix
can be justified under Rule 22 (§6).

Not decided, and named in §9 so it is not mistaken for oversight: the OCO leg a
rejected stranger still destroys, the fill counters a rejected taker still carries, the
depth `pkg/marketdata` still publishes for a resting stop, and whether
`internal/refmatch` should model tier 2. That last one is the reason defect 1 can only
ever be held by a hand-written test in this slice, and §9 states it rather than leaving
a reader to infer it from a green sweep.

## 2. The rule both break

`DIFFERENTIAL-FINDINGS.md` §2.1 states it once and this document does not restate it:

> **After a command that is refused, the venue's observable state equals what it was
> before the command — or every difference is on the event stream.**

§2.2 gives the test that picks the remedy: **revert what the venue can restore without
loss; announce what it cannot.** Applied here, the two defects fall on opposite sides,
which is why one is a state fix and the other is a stream fix:

- An iceberg's fill state **is** restorable without loss. It is five numbers on one
  order that no other participant has acted on, because the prints that moved them
  reached nobody. So: revert (§3).
- A cascade-fired order that has already been announced `TRIGGERED` and `ACCEPTED`
  **cannot** have those announcements withdrawn. So: announce the ending (§4).

## 3. Defect 1 — a failing fill-or-kill corrupts an iceberg's reserve

### 3.1 What it does today, measured

`u1` posts an iceberg, 9 total and 3 displayed. `u2` sends a fill-or-kill buy 12 @ 100.
Two commands, `DefaultConfig`, nothing exotic switched on
(`TestFailingFOKCorruptsAnIcebergsReserve`):

| | before | after |
|---|---|---|
| slice `Quantity` | 3 | 3 |
| `FilledQty` | 0 | **−6** |
| `RemainingQty` | 3 | **9** |
| hidden reserve | 6 | **0** |
| refill count | 0 | 2 |
| status | NEW | PARTIALLY_FILLED |
| best ask | 100:3 | **100:9** |
| `pkg/marketdata` L2 | 100:3 | 100:9 |
| events published | — | `[REJECTED, ACCEPTED#1/PARTIALLY_FILLED/9, ACCEPTED#1/PARTIALLY_FILLED/9]` |

Four separate things are wrong, and they are worth separating because a fix that
repairs three of them looks finished:

1. **`FilledQty` is negative.** An order has un-filled past zero.
2. **The reserve is in the open.** `RemainingQty` (9) exceeds the order's own
   `Quantity` (3): the level publishes a slice three times the size the client
   authorised showing. This is the one that matters, because it is the entire purpose
   of the order type.
3. **It persists and compounds.** The order goes on trading from the corrupted state:
   a follow-up buy of 4 lots takes 4 from it and leaves `FilledQty = −2`,
   `RemainingQty = 5`, reserve still 0. It never refills again — the walk deleted it
   from `e.icebergOrders` when the reserve ran dry, and nothing put it back.
4. **The stream repeats the corruption rather than contradicting it.** The two
   `ACCEPTED` events are the two refills, and both carry `RemainingQty 9`, because an
   `Event` holds a *pointer* and the batch is published after the reversal has already
   mutated the object. A mirror built from the stream lands on 9 lots, the engine holds
   9 lots, and the two agree — on a number neither should have. This is what
   `event.go:29-32` means by "the engine is wrong there rather than the stream".

A second reachable shape, which matters because it is the one that also moves the
**level aggregate** independently of the order: take one lot of the displayed slice
first, so the saved state is a *partially consumed* slice, and let the walk be stopped
by self-trade prevention rather than by an empty book (`u1` iceberg 9/3, `u3` buys 1,
`u2` rests sell 5 behind it, `u2` sends fill-or-kill buy 20 under `CANCEL_NEWEST`).
Measured after: `FilledQty −2`, `RemainingQty 5`, reserve 3, refills 1, level publishes
10. Pre-command it was `FilledQty 1`, `RemainingQty 2`, reserve 6, refills 0, level 7.

`checkInvariants` passes on every one of these. −6 + 9 = 3, and 9 ≥ 0. §5.

### 3.2 The mechanism, and the question it poses

The walk consumes the visible slice, and on a fully consumed maker it refills
(`engine.go:1386-1397`, and the copy in `matchProRata` at `:1681-1691`):

```go
if maker.IsFilled() {
    _, _ = e.book.Remove(maker.ID)
    if ib, ok := e.icebergOrders[maker.ID]; ok {
        if ib.Refill() { _ = e.book.Add(ib.Order); e.emitAdd(ib.Order) } else { delete(...) }
    }
}
```

`Refill` (`pkg/types/iceberg.go:48-63`) sets `Quantity`, `RemainingQty` and `FilledQty`
to a **fresh slice** on the *same* `*types.Order`, and the same object is re-added to
the book under the same id. Three prints against three slices are three prints against
one object, and the object's counters describe only the last of them.

Then the fill-or-kill fails and `reverseTrade` (`engine.go:1906-1937`) runs once per
print: `maker.RemainingQty += tr.Quantity; maker.FilledQty -= tr.Quantity`. It is
arithmetically exact and it is being applied to an object whose counters the refill
already rewound. Three prints of 3 lots add 9 to a slice of 3.

So: **two paths disagree about who owns the rewind.** `reverseTrade` believes the
maker still carries the fills the print made. The refill path silently makes that
false. The question this section exists to answer is which of the two changes.

### 3.3 Decision 1

> **The refill path owns the rewind. `reverseTrade` does not learn about icebergs.**
>
> The walk **saves an iceberg's whole state the first time it is about to trade against
> it**, and a failed fill-or-kill **restores it whole** — order quantities, fill
> counters, status, hidden reserve, refill counter and registry entry — instead of
> inverting anything. The prints against a restored iceberg are **not** passed to
> `reverseTrade`: the restore has already accounted for them. The `ACCEPTED` events
> announcing the undone refills are dropped with the reversed trades.

The argument is one sentence and its consequences: `reverseTrade` has a precondition,
the refill path is the only code in the engine that breaks it, and the code that breaks
a precondition is the code that pays to restore it.

- **The precondition is real and is now written down.** `reverseTrade`'s own comment
  already says it is kept general "for reuse by any future reversal path". A special
  case for icebergs inside it is a special case every future caller inherits — the bust
  path, a replication rewind, an auction unwind — and each of them would have to know
  that one order type can un-fill itself mid-walk.
- **The refill path is the only violator, and after this fix there are none.** Nothing
  else in the engine recycles an order object's identity. Once the refill path restores
  what it clobbered, `reverseTrade`'s arithmetic is correct for every maker it still
  sees, without knowing why.
- **Save-and-restore, not an inverse.** An "un-refill" is not well defined on its own:
  under `IcebergPeakJitter` the slice size is derived from `(Order.ID, refills)`, so
  undoing one means restoring the counter *and* the size *and* the status *and* the
  registry entry it may have deleted. That is the save. Writing an inverse that needs
  the save is writing the save twice.

Measured on the prototype: with jitter at ±20% on a 30/5 iceberg, the state after
`[iceberg, failed fill-or-kill, buy, buy, buy]` is **identical in every field** to the
state after `[iceberg, buy, buy, buy]`, refill counter and reserve included. The
rejected command leaves no trace at all, which is the property §2 asks for.

### 3.4 What the order must hold afterwards

For `TestFailingFOKCorruptsAnIcebergsReserve`'s reproduction, after the rejection:

```
Quantity 3   FilledQty 0   RemainingQty 3   Status NEW
hidden reserve 6   refills 0   still registered in e.icebergOrders
best ask 100:3     OrderCount 1
events published: [REJECTED]        (exactly one)
LastTradePrice 0                    (unchanged; DIFFERENTIAL-FINDINGS §3 already)
```

Stated generally, and this is the acceptance rule rather than the example:

> After a rejected fill-or-kill, every iceberg it touched holds **exactly** what it
> held before the command — displayed slice, fill counters, status, reserve and refill
> counter — and its level publishes exactly what it published, **except for queue
> position**, which the restored slice forfeits to the tail of its level exactly as
> `reverseTrade`'s own re-add already does for an ordinary maker.

"Exactly as `reverseTrade`'s own re-add already does" is a stronger constraint than it
looks, and the code shipped for review did not meet it: `reverseTrade` re-adds each
maker **at its own print**, so where the restore happens in the reversal is queue
position. §13.6 is that measurement and the correction.

And the reserve must still *work*: a buy that consumes the restored slice afterwards
must refill from the reserve. That assertion is what catches a restore that repairs the
numbers and leaves the order deleted from `e.icebergOrders`, which is the plausible
half-fix.

### 3.5 The alternatives, and why each is refused

**(i) Teach `reverseTrade` about icebergs.** Refused in §3.3: it is the shared
reversal primitive, and the special case propagates to every future caller. It is also
not sufficient — reversing prints correctly still leaves the reserve empty and the
registry entry deleted, because neither is a function of the prints.

**(ii) Let the refill stand and reverse only the last slice's print.** This is the
`DIFFERENTIAL-FINDINGS.md` §4 rule ("a rejection drops only what was undone; everything
else stands and is announced") applied literally. It is refused because it is the one
option that **destroys client quantity**: with the refills standing, the order holds
one 3-lot slice and an empty reserve where a 9-lot order used to be, and six lots that
nobody traded have simply ceased to exist. The §4 rule is about mutations that a rule
the client chose authorised — its own self-trade-prevention mode cancelling its own
maker. No rule authorises deleting six lots of a third party's order because a stranger
sent an order the venue refused.

**(iii) Give every refilled slice its own order id.** It makes the aliasing impossible
by construction and it is a much larger change: slice ids are customer-visible on every
wire edge, in `wire.Executed`, in drop copies and in every consumer's order map, and
`emitAdd`'s whole design (`engine.go:792-802`) is that a reload re-announces the *same*
id so a consumer's later fills resolve. That is a venue-behaviour decision that deserves
its own document, not a side effect of a bug fix.

**(iv) Cancel the iceberg on a failed reversal.** Refuses to solve the problem and
punishes the victim: the client loses the order because someone else's command failed.

**(v) Do nothing until `internal/refmatch` models icebergs.** That is the status quo,
and the status quo is a live defect that leaks a client's hidden size. §9 keeps the
model at tier 1 and accepts that a hand-written test is the only oracle here; it does
not accept waiting.

### 3.6 Two things the design relies on, proved rather than assumed

**(a) The save can be taken at the first print, because a walk never both trades with
and self-trade-prevents the same maker.** `isSelfMatch` is a function of
`(taker, maker)` user id and trade group, constant for the length of a walk. If it is
true, the pair never trades and the STP branch runs; if it is false, the STP branch
never runs for that pair. So the mutation `decrement` makes to a maker — the one
non-trade mutation that must survive a rejection under §4's rule — can never land on an
iceberg this walk also traded with, and restoring that iceberg cannot undo an
authorised STP decrement. The two are disjoint by construction.

**(b) A failing fill-or-kill has fully consumed every maker it printed against.** The
walk takes `min(taker.RemainingQty, maker.RemainingQty)`; if the maker survives a
print, the taker is at zero, and a taker at zero is filled, which is fill-or-kill
*success*. Under pro-rata the same holds one level at a time: a round that allocates
`q < total` implies `q == taker.RemainingQty`, which again is success. This is why the
restore never has to reason about a half-consumed slice *that the walk itself created*
— though it must handle a half-consumed slice it **inherited**, which is §3.1's second
shape and the reason the restore removes the resting slice from the book before putting
the saved one back rather than mutating it in place.

### 3.7 Where the save lives, and what it costs

A slice on the engine, `icebergSaves []icebergSave`, truncated to zero at the top of
each fill-or-kill walk in `match` and `matchProRata` and read only by `settleInto`'s
fill-or-kill failure branch, which runs immediately after that walk with nothing
between them that could start another. That is the whole lifetime rule, and it is
stated in one line rather than distributed over `settleInto`'s seven return paths.

Cost on the hot path: for a non-fill-or-kill taker, **nothing** — the walk does not
enter the branch. For a fill-or-kill taker on a venue with no icebergs resting, one
`len(e.icebergOrders) > 0` test per print. For a fill-or-kill against an iceberg, one
map lookup and a scan of a slice that is one entry per distinct iceberg the walk
touched. `TestZeroAllocHotPath` passes unmodified on the prototype, and it is a
deliverable that it does.

### 3.8 `Fixed`, not `Changed`

Nobody depended on this deliberately. There is no reading of a negative `FilledQty`, a
displayed size larger than the order's quantity, or a reserve that empties itself on a
stranger's rejection under which a consumer built something correct. By
[`COMPATIBILITY.md`](COMPATIBILITY.md) §"The rule" that is a `Fixed` entry naming the
behaviour, not a `Changed` one naming a break.

It is nonetheless **replay-visible**, which is a different axis and is §7.

## 4. Defect 2 — a cascade-fired stop's rejection reaches nobody

### 4.1 What it does today, measured

Mirror `[3@200:50, 4@120:4]` against engine `[4@120:4]`, from six commands
(`TestCascadeFiredStopRejectedLeavesAPhantom`). The complete batch for the triggering
command:

```
ACCEPTED#6/FILLED/0   TRADE#6/95x1   TRIGGERED#3/REJECTED/46   ACCEPTED#3/REJECTED/46
```

`cascadeStops` announces the stop's order `TRIGGERED` and then `ACCEPTED` before
settling it (`engine.go:1051-1061`), `settleInto` rejects it, and the rejection reaches
nobody: `emitTerminalIfDone` fires only on `OrderStatusCancelled`
(`engine.go:844-851`), and a cascade-fired order never reaches `emitResult` because
`cascadeStops` discards both the status and the reason. The stream has said an order
entered the book and will never say it left.

Note the `ACCEPTED#3/REJECTED/46`: the event carries a pointer, so by publication time
the order's own status already reads REJECTED. A consumer *could* switch on it. Every
consumer in this repository keys on the kind, as the documented contract tells them to,
and so:

- the mirror in `event_conformance_test.go` holds `3@200:50` forever;
- `pkg/marketdata`'s L2 feed publishes **46 lots at 200** on a book that has none;
- `pkg/orderentry`'s session registry keeps the order live for the client.

The on-arrival stop path does **not** have this defect, and the asymmetry is the
diagnosis: `submitStopInto` emits `TRIGGERED` and then returns its status to
`ProcessStop`, which calls `emitResult` — so a stop that fires on arrival and is
refused publishes `REJECTED` and never publishes an `ACCEPTED` at all. The cascade path
emits its own `ACCEPTED` precisely *because* there is no `emitResult` behind it. That
half was implemented. The terminal half was not.

### 4.2 The three candidates

| | What it does | What it does to ordering |
|---|---|---|
| **C1** Route cascade-fired orders through `emitResult` | Publishes `ACCEPTED`/`REJECTED` for the order and flushes `e.pending` | Wrong twice: `emitAdd` has already published an `ACCEPTED`, so a filled cascade order would be announced twice; and `emitResult` composes a **batch**, so a nested cascade would publish mid-command and break the one-batch-per-command composition every consumer sees |
| **C2** Delay the `ACCEPTED` until after `settleInto` | Publishes `ACCEPTED` or `REJECTED`, never both | Puts the stop's own trades **before** the event that introduces the order. `emitAdd`'s comment (`engine.go:792-796`) records exactly this failure: consumers "had to discard every later fill of a still-live order as referencing something unknown" |
| **C3** Widen `emitTerminalIfDone` beyond `Cancelled` | Appends one terminal event after the order settles | Nothing moves. The new event lands where the existing `Cancelled` close-out already lands, in the batch, in causal order |

### 4.3 Decision 2

> **C3. `emitTerminalIfDone` fires for every cascade-fired order that ends without
> resting — `Cancelled` or `Rejected` — and publishes an `EventCanceled` carrying the
> reason `settleInto` returned.**

`EventCanceled` rather than `EventRejected`, and the reason is a contract question
rather than a taste question:

- Its documented meaning already covers this: "order removed: cancelled, **or
  terminated without resting**" (`event.go:62`), and `emitTerminalIfDone`'s own comment
  says it "closes out an order that settled without resting". The case is the one the
  function was written for; only the status test was too narrow.
- **Every existing consumer already treats `CANCELED` as a delete**, and none treats
  `REJECTED` as one — correctly, since on the submit path a `REJECTED` order never
  entered anything. Choosing `EventRejected` would mean a new rule for every consumer
  ("a `REJECTED` may delete an order you hold"), shipped in a patch whose entire
  purpose is that consumers are already wrong about this order.
- The information is not lost: the event carries the `Order` (whose `Status` is
  `REJECTED`) and the `Reason` (`FOK_CANNOT_FILL`), which is exactly what
  `emitCancelReason` already exists to convey — "so a consumer can tell an expiry from
  a cancel the client issued". `pkg/orderentry` turns it into a client-facing
  `KindCanceled` with a reason code, which is what a venue tells a client whose
  contingent order could not be filled.

### 4.4 What a consumer sees, in order

```
TRIGGERED#3   the stop fired
ACCEPTED#3    it became a live order
(its prints, if any — all reversed and dropped if it was a failed fill-or-kill)
CANCELED#3/FOK_CANNOT_FILL/REJECTED/9    it is gone
```

Three properties this ordering has to keep, each of which is a sabotage row in §11:
the `CANCELED` comes **after** the `ACCEPTED` (before it, a mirror deletes and then
re-adds, and the phantom returns); it is in the **same batch** as the command that
caused it (a consumer must never see a live order across a batch boundary that the
venue already refused); and it is published **whether or not the order printed**, since
a rejected fill-or-kill's prints are dropped and would otherwise be the only trace.

### 4.5 The reason field, and the one existing outcome it widens

Passing `settleInto`'s reason through widens one behaviour beyond the defect: a
cascade-fired **market** order that finds no liquidity already ends `Cancelled` and
already publishes a `CANCELED`, and that event will now carry
`ErrMarketOrderNoLiquidity` where it previously carried `nil`. Measured, and stated
here so it is not discovered as a surprise. It is strictly more information on a field
that is already optional and already carries a reason on the expiry path, and the
alternative — carrying the reason only when the status is `Rejected` — makes the field
mean two different things depending on a status a consumer would have to check first.

### 4.6 `Changed`, not `Fixed`

This changes the **shape of a published stream**, which is the same test
`DIFFERENTIAL-FINDINGS.md` §4.8 applied to defect B. A consumer that assumed a
cascade-fired order produces exactly `TRIGGERED` + `ACCEPTED` now sees a third event,
and a consumer counting `CANCELED` events sees more of them. By
[`COMPATIBILITY.md`](COMPATIBILITY.md) that requires a `Changed` entry naming the new
shape in the same commit, even though every consumer that had the old assumption was
already wrong.

## 5. The invariant blind spot

### 5.1 What the suite checks today

`checkInvariants` (`pkg/matching/fuzz_test.go:13-27`) asserts three things: the book is
not crossed; and, **for the one order passed to it**, `filled + remaining == quantity`
and `remaining >= 0`.

The obvious addition is `FilledQty >= 0`. It is the wrong lesson on its own, and the
proof is in the pinning test itself: `TestFailingFOKCorruptsAnIcebergsReserve` calls
`checkInvariants(t, e, nil)`. The corrupted order is a **maker**, so the per-order half
of the suite runs on nothing at all. A defect in a *reversal* is by construction a
defect in a maker; the suite checks the taker; the taker is the one order that cannot
be the victim. `FuzzExoticOrders`, the only place in the repository that hammers
icebergs, also calls it with `nil` — so on the entire exotic surface, the invariant
suite asserts exactly one property, and it is not about quantity.

### 5.2 What `filled + remaining == quantity` does not constrain

Enumerated, because "add the missing one" is only a good answer if you know what is
missing:

1. **The sign of either term.** −6 + 9 = 3 satisfies it. So does 9 + (−6) = 3.
2. **Any order but the one just submitted.** Every resting maker is unchecked.
3. **The relationship between an order and the level that publishes it.** The book
   maintains its aggregate incrementally through a `contributed` field that is
   deliberately *not* `order.RemainingQty` (`REFERENCE-MATCHER.md` §2.1) — a place the
   engine can disagree with itself, and §3.1's second shape is a live example: with
   the restore's `book.Remove` omitted, the order reads 2 lots and its level publishes
   3, with every other assertion in the suite green.
4. **Whether a resting order is in a state that permits resting.** A zero-remaining or
   cancelled order still in the book satisfies conservation perfectly.

### 5.3 Decision 3

> **`checkInvariants` becomes a whole-book check, not a one-order check**, and asserts
> four things instead of two:
>
> - per order — including **every resting order**, not just the submitted one —
>   `filled + remaining == quantity`, `remaining >= 0`, **and `filled >= 0`**;
> - a resting order has `remaining > 0` and an active status;
> - each level's maintained `TotalQty` equals the sum of the `RemainingQty` of the
>   orders actually resting there, on both sides;
> - the book is not crossed (unchanged).

That is one rule — *every order the engine still owns conserves quantity, in both
directions, and the book's aggregate agrees with the orders in it* — rather than a
patch for one defect. Items 1 and 3 of §5.2 are each caught by a different one of these
assertions, measured: the sign check catches §3.1's first shape (`a resting order (1):
negative filled quantity: -6`), the aggregate check catches §3.1's second under the
plausible half-fix, and neither catches the other's case.

### 5.4 An invariant with no reachable input is decoration

`FuzzExoticOrders` draws stops, icebergs, trailing stops and market takers. It draws no
fill-or-kill, so it cannot reach a reversal at all, and the strengthened invariant would
have sat there green forever. **The invariant and the input are one deliverable**: the
fuzz target's alphabet gains a fill-or-kill taker, a fifth case in its four-case switch.

Measured on the prototype, against the **unfixed** engine: strengthened invariant plus
the new alphabet symbol, `go test -fuzz=FuzzExoticOrders` fails in **0.13 seconds**,
from the existing seed corpus, on a minimised 12-byte input — `a resting order (7):
negative filled quantity: -1`. Nobody had to predict the defect. That input is
committed as a regression seed, and against the fixed engine the same target survives
**293,942 executions** in 45 s.

### 5.5 What it costs

`checkInvariants` becomes O(resting orders + levels) per call. Measured across
`TestDifferentialTape`'s 2,240 commands, where it runs after every one: **0.58–0.66 s
before, 0.52–0.70 s after** — inside the run-to-run noise. `TestSoak` calls it four
times on a book of up to 200,000 orders and does not move. If a future profile shows it
dominating a fuzz run, the answer is a shorter run, not a weaker assertion.

## 6. The fingerprint does not fire, and that is the finding

### 6.1 Measured: both fixes are invisible to `internal/semcheck`

The premise this slice started from was that `internal/semcheck` freezes matching
behaviour and would therefore fire on any change to it. **It does not.** With both
fixes applied and the corpus untouched, the prototype ran `go test ./...` across all
packages and `internal/semcheck` was **green** — all six of its tests, including
`TestMatchingSemanticsAreFrozen`. Only the two pinning tests failed, which is the
intended result.

The reason is the corpus, and it is `SEMANTICS-VERSION.md` §5.5's boundary arriving
exactly where that document said it would. The hand-written tier-2 script reaches
icebergs (`ICEBERG=1`, `refills 2`) and stops (`STOP=4`, `triggers 5`), but:

- no fill-or-kill in it ever meets an iceberg — the iceberg is worked off by ordinary
  GTC buys and then cancelled;
- no stop it fires ever fails — every trigger in the corpus fills.

So neither defect is in the fingerprint, and under Rule 22 — *the version changes if
and only if the golden body changes* — **neither fix could be bumped for.** A fix that
cannot move the golden is a fix the next person can silently revert.

### 6.2 The corpus gains seven commands, appended

Appended to the end of `tierTwo()`, **never inserted**: an insertion mid-script shifts
every later line's order id, event id and digest, and turns a reviewable diff into a
thousand-line rewrite that nobody reads. Appended, the diff is legible.

```go
// A fill-or-kill that exhausts an iceberg's reserve and then fails.
ice2 := s.add(Cmd{Kind: Iceberg, User: "i2", Sell: true, Price: 102, Qty: 9, DisplayQty: 3})
s.add(Cmd{Kind: Submit, User: "t11", Price: 102, Qty: 20, TIF: types.TIFFillOrKill,
    Note: "consumes every slice, cannot fill, and is reversed"})
s.add(Cmd{Kind: Submit, User: "t12", Price: 102, Qty: 3, Note: "the restored reserve still refills"})
s.add(Cmd{Kind: Cancel, User: "i2", Target: ice2})

// A stop fired by a CASCADE whose own order is then refused.
s.add(Cmd{Kind: Stop, User: "s5", Price: 200, Qty: 50, TIF: types.TIFFillOrKill, StopPrice: 105,
    Note: "its own order cannot fill"})
s.add(Cmd{Kind: Submit, User: "k1", Sell: true, Price: 105, Qty: 1})
s.add(Cmd{Kind: Submit, User: "k2", Price: 105, Qty: 1, Note: "prints at 105, firing a stop that is refused"})
```

The third command is not padding: it is the assertion that the **restored reserve still
works**, and it is the line that moves if a restore repairs the numbers and leaves the
iceberg deregistered.

### 6.3 The diff the regeneration must show

With the corpus extended, the fixes applied and `matching.SemanticsVersion` raised from
1 to **2**, `SEMCHECK_UPDATE=1 go test ./internal/semcheck/` must produce a diff with
**exactly three parts**:

1. the header, `semantics 1` → `semantics 2`;
2. **seven appended lines** in the `conditional` section, `conditional/0082` to
   `conditional/0088`;
3. the trailing coverage block.

Every other line of all six scenarios — `fifo`, `prorata-shard7`,
`capped-decrement-shard3`, `auction`, `guarded` and the first 82 lines of
`conditional` — must be **byte-identical**. Measured on the prototype: 29 lines in the
`diff` output, and not one of them outside those three parts. Anything else means a fix is
wider than this document thinks, and §12's first bullet is what to do about it.

What the two fixes do to those seven lines, measured by rendering the same extended
corpus against the unfixed engine and against the fixed one:

```
-  0083 ... E[REJECTED#60/FOK_CANNOT_FILL/REJECTED/9 ACCEPTED#59/PARTIALLY_FILLED/-6
-                                                    ACCEPTED#59/PARTIALLY_FILLED/-6] ... ask=102:9
+  0083 ... E[REJECTED#60/FOK_CANNOT_FILL/REJECTED/9] ...                                 ask=102:3

-  0084 ... E[ACCEPTED#61/FILLED/3 TRADE#61/102x3] ...                          ask=102:6
+  0084 ... E[ACCEPTED#61/FILLED/3 TRADE#61/102x3 ACCEPTED#59/NEW/0] ...        ask=102:3

-  0085 ... E[CANCELED#59/CANCELLED/-3]
+  0085 ... E[CANCELED#59/CANCELLED/0]

-  0088 ... TRIGGERED#62/REJECTED/9 ACCEPTED#62/REJECTED/9]
+  0088 ... TRIGGERED#62/REJECTED/9 ACCEPTED#62/REJECTED/9 CANCELED#62/FOK_CANNOT_FILL/REJECTED/9]
```

The golden prints the corruption in plain text — `PARTIALLY_FILLED/-6`, twice, and a
cancellation carrying `-3` — which is the argument for the corpus extension in one
line: this is what the fingerprint has been unable to see.

`SEMCHECK_UPDATE=1` refuses to regenerate twice at the same version, so the corpus
extension, both fixes and the bump are **one regeneration**, not three.

### 6.4 Two coverage counters, so this cannot happen again

`TestTheFingerprintReachesEveryDecidedBehaviour` is the guard that makes the corpus
load-bearing, and it enumerates the behaviours the corpus must reach. It gains two
rows, and `Coverage` gains the two counters behind them:

- **`IcebergRestores`** — a command whose verdict was REJECTED after which an iceberg's
  `Refills` is **lower** than it was before. `semcheck.go` already snapshots icebergs
  per command and tracks a per-order maximum refill count; a rewind is exactly the
  thing that maximum is currently hiding.
- **`CascadeTerminals`** — a `TRIGGERED` and an `ACCEPTED` for an order id followed, in
  the same batch, by a `CANCELED` for it.

Both are zero on today's corpus, so adding them turns the guard red until the corpus
reaches both paths. That is the mechanical forcing function: the next person who
deletes those commands fails a test that says why, rather than quietly returning the
fingerprint to its current blindness.

## 7. Replay, the wire, and the changelog

### 7.1 Is either fix replay-visible?

**Defect 1: yes, in the strong sense.** Recovery replays *commands*
(`DIFFERENTIAL-FINDINGS.md` §4.5), so what matters is whether the same commands produce
a different book. They do: any journal containing a fill-or-kill that failed after
consuming at least one full slice of a resting iceberg replays to a **different, and
correct, book** on the new build. That is precisely the condition
`matching.SemanticsVersion` exists to detect, and `wal.Recover` will refuse a
pre-upgrade segment whose records it is about to apply. RUNBOOKS' "Upgrading across a
semantics change" is the procedure, unchanged.

The fix is **not retroactive**, and this needs saying in the runbook rather than
discovered: `EngineSnapshot` carries each iceberg's `Hidden` and `Refills`
(`snapshot.go:162-169`), so an order already corrupted on a running venue survives a
snapshot round trip unrepaired. A snapshot is a state, not a program. The remedy for an
already-corrupted order is to cancel and re-enter it, and the corruption is
recognisable: `FilledQty < 0`, or a displayed size larger than the order's `Quantity`.

**Defect 2: no.** The engine's state is identical before and after — the phantom exists
only in consumers. Nothing recovery reads changes. It still needs the version bump,
because the bump is mechanical on the golden body (§6) and because the two fixes ship
in one commit.

### 7.2 Is either wire-visible?

Both, and differently:

- **Defect 1** changes the published L2 aggregate (`pkg/marketdata`: 100:9 → 100:3) and
  the L3 stream (two spurious `ACCEPTED` events disappear). Every one of those changes
  is a consumer being told the truth instead of a falsehood, and no consumer could have
  built anything on the falsehood.
- **Defect 2** adds an event to a stream that a consumer must apply, plus a reason on
  an existing one (§4.5). `pkg/orderentry` will deliver a `KindCanceled` with a reason
  code to the client whose stop was refused — a message that client has never received
  before and should have.

Nothing exported moves. `internal/apicheck/testdata/surface.txt` must be
**byte-identical** after this slice, and it was on the prototype.

### 7.3 What the changelog must say

Three entries, in the same commit:

1. **`Fixed`** — defect 1, naming what an operator can recognise: a fill-or-kill that
   fails after consuming an iceberg's slices no longer leaves the order with a negative
   `FilledQty`, its reserve displayed, and its refill registration dropped. Must state
   that the fix is not retroactive and how to recognise an already-corrupted order
   (§7.1).
2. **`Changed`** — defect 2, with the sentence a downstream author needs: **a
   cascade-fired stop or trailing stop whose order the venue refuses now publishes a
   `CANCELED` for that order after its `ACCEPTED`, and a consumer must apply it**; plus
   the note that a cascade-fired order's `CANCELED` now carries a reason.
3. **`Changed`** — `matching.SemanticsVersion` 1 → 2, with its row added to
   `SEMANTICS-VERSION.md` §1.2 naming the two entries above, per the registry rule that
   every value names the changelog entries it covers.

## 8. Rules that will look like bugs

| Rule | Why it will look like a bug |
|---|---|
| A restored iceberg re-enters at the **back** of its price level *as that level stands at the moment of its own first reversed print* | The client did nothing wrong and can still lose queue position to a stranger's rejected order — behind a maker a self-trade-prevention decrement left resting mid-level, for instance. It is the behaviour `reverseTrade` already has for every ordinary maker it puts back (`OrderBook.Add` appends to the tail), and making the iceberg the one order type that keeps priority through a reversal would be the inconsistency. **The words in italics were not in this row as written, and the code shipped without them was wrong**: it restored every iceberg before the reversal loop, which put it back ahead of makers that had been resting in FRONT of it. §13.6 |
| A restored iceberg keeps its **`UpdatedAt`** from the command that was rejected | It reads as a modification that never happened. Rewinding a timestamp is not restoring a datum, it is inventing one; `reverseTrade` does not rewind it either |
| A rejected fill-or-kill still leaves the **taker's** own `FilledQty` positive | Half the family is fixed and half is not, in one branch. §9 pins it, with a measurement: it is a much wider consumer-visible change than either fix here and it is not this slice's |
| An order is announced `ACCEPTED` and later `CANCELED` **with `Order.Status == REJECTED`** | The kind and the status disagree. The kind is what consumers switch on and means "removed"; the status is why. §4.3 |
| The `CANCELED` that closes a refused cascade order arrives **after** the `ACCEPTED`, not instead of it | The event order does not match the verdict order. It matches causal order, which is the documented contract for a batch, and the `ACCEPTED` was already published before the verdict existed |
| A cascade-fired market order that finds no liquidity now carries a **reason** on an event that used to carry none | It looks like an unrelated change smuggled in. It is: §4.5 names it, measures it, and explains why the alternative is worse |
| `checkInvariants` now walks the **whole book** on every call | It looks like a fuzz target got slower for a defect that is already fixed. The defect it catches is always on an order the old check never looked at, and the cost is inside the noise (§5.5) |
| `TestFailingFOKCorruptsAnIcebergsReserve` and `TestCascadeFiredStopRejectedLeavesAPhantom` **assert the opposite of their names** after this slice | An inverted pinning test reads as a test flipped to make a change pass. It is the mechanism that forced this document to exist (`differential_findings_test.go:12-17`) |

## 9. What this deliberately does not do

- **It does not extend `internal/refmatch` to tier 2.** Icebergs and OCO stay
  unmodelled (`REFERENCE-MATCHER.md` §2.4), so the differential harness **cannot reach
  defect 1 at all** and a green sweep after this slice is evidence of nothing about it.
  The only oracles for defect 1 are the hand-written test, the strengthened invariant
  with its new fuzz alphabet (§5), and the fingerprint's thirteen new lines (§6, §13.6). That is
  stated here rather than left implicit, because "the differential sweep is green" is
  exactly the sentence someone will offer as proof.
- **It does not fix the OCO leg** (`DIFFERENTIAL-FINDINGS.md` §4.1(iv),
  `TestFailingFOKCancelsAnOCOStopLeg`). A stranger's rejected fill-or-kill still
  destroys a client's protective stop, and the destruction is still only *announced*.
  It stays pinned. Note that §4.4 of that document counted **four** inverse operations
  a "restore everything" rule would need in order to refuse B1; this slice supplies one
  of them, so the count is now three, and that is honest bookkeeping rather than a
  reopening — the iceberg is restored because the alternative destroys quantity nobody
  authorised, which is not true of an STP cancellation the taker's own mode asked for.
- **It does not rewind the rejected taker's own fill counters.** Measured: a rejected
  fill-or-kill buy for 12 that printed 9 and had all 9 reversed keeps
  `FilledQty = 9, RemainingQty = 3`, and that number is published on its `REJECTED`
  event — it is the `/9` on `conditional/0044` of the fingerprint today. It is the same
  family and it is not this slice: it would change the payload of **every** rejected
  fill-or-kill that printed, on a path the corpus reaches 40 times, which is a
  consumer-visible change with its own argument to make. **It gets a pinning test in
  this slice** — `TestRejectedFOKKeepsItsOwnFillCounters` — with
  the sentence a fix must come and delete, because a measured finding left unpinned is
  how it gets found a third time.
- **It does not fix `pkg/marketdata`'s pending-stop phantom.** Measured while writing
  §4.1: `L2Feed` adds an order to its depth on any `ACCEPTED`, including a stop resting
  at `PENDING_TRIGGER`, so a 50-lot stop at 200 appears in the public depth feed from
  the moment it is submitted — the mirror in `event_conformance_test.go` has the guard
  for exactly this (`e.Order.Status == types.OrderStatusPendingTrigger`) and the feed
  does not. Defect 2's fix will make the residue of it disappear in this one path,
  which is precisely why it is written down now: the general defect survives, in
  another package, with a stronger case for its own slice.
- **It does not fix `pkg/wal`'s log-only iceberg recovery.** Added after the fact, from
  adversarial review: `AppendIceberg` logs an order whose `Quantity` `NewIcebergOrder`
  has already shrunk to the display size, so a recovery with no snapshot rebuilds every
  iceberg with an **empty reserve**. Pre-existing, reproduced identically on `main`,
  and with no fill-or-kill involved — but it bounds §7.1's replay claim to the
  snapshot path, so §13.7 measures it, `CHANGELOG.md` carries the bound, and
  `pkg/wal/iceberg_reserve_pin_test.go` pins it with the sentence a fix must delete.
- **It does not change how fill-or-kill decides fillability**, and it does not revisit
  `DIFFERENTIAL-FINDINGS.md` §3.4(ii)'s refusal to make the reversal unnecessary.
- **It does not touch the exported surface.** `internal/apicheck/testdata/surface.txt`
  byte-identical, or the change was not one of these two.
- **It does not renumber or re-order the existing fingerprint corpus** (§6.2), and it
  does not relax any assertion anywhere. Every existing assertion in both pinning tests
  is kept when they invert; the strengthening adds, it does not replace.

## 10. Deliverables and acceptance criteria

| # | Deliverable | Done when |
|---|---|---|
| 1 | Defect 1, engine | `settleInto`'s fill-or-kill branch restores every iceberg the walk touched ~~before reversing prints~~ **at the iceberg's first reversed print** (§13.6 corrects this row), skips `reverseTrade` for their prints, and drops their refill `ACCEPTED`s; `match` and `matchProRata` both record the saves; `reverseTrade` is **unchanged** |
| 2 | Defect 1, test | `TestFailingFOKCorruptsAnIcebergsReserve` **inverted, keeping its name**, asserting every field of §3.4 *and the exact event batch* `[REJECTED]`, *and* that a follow-up buy of 3 consumes the restored slice and refills the reserve |
| 3 | Defect 1, second shape | A new test for §3.1's second reproduction — partially consumed slice, walk stopped by `CANCEL_NEWEST`, refilled slice still resting — asserting the order **and** the level aggregate. This is the case the plausible half-fix gets wrong |
| 4 | Defect 2, engine | `emitTerminalIfDone` fires on `Cancelled` and `Rejected`, carries the reason, and `cascadeStops` passes `settleInto`'s reason for both stops and trailing stops |
| 5 | Defect 2, test | `TestCascadeFiredStopRejectedLeavesAPhantom` **inverted, keeping its name**: mirror equals engine exactly, plus an assertion on the batch — a `CANCELED` naming the stop's order, carrying `ErrFOKCannotFill`, positioned after the `ACCEPTED` |
| 6 | Defect 2, scenario coverage | A cascade-fired-and-refused stop added to `TestEventStreamReconstructsBook`'s table, so the list the `EventKind` comment cites gains the combination it lacked |
| 7 | The invariant | `checkInvariants` implements all four assertions of §5.3; `FuzzExoticOrders` gains a fill-or-kill symbol; the 12-byte regression input from §5.4 is committed as a seed |
| 8 | The fingerprint | Corpus extended per §6.2; `matching.SemanticsVersion` = 2; `SEMANTICS-VERSION.md` §1.2 gains row 2; the two coverage counters and their guard rows exist and are non-zero |
| 9 | The diff is exactly §6.3 | Every line of `fifo`, `prorata-shard7`, `capped-decrement-shard3`, `auction`, `guarded` and `conditional/0000`–`conditional/0081` byte-identical. **Checked by reading, not by regenerating twice** |
| 10 | The third finding is pinned, not fixed | `TestRejectedFOKKeepsItsOwnFillCounters` exists, asserts today's behaviour (a rejected fill-or-kill that printed 9 keeps `FilledQty 9`, `RemainingQty 3`, and publishes it on its `REJECTED`), and carries the sentence a fix must come and delete (§9) |
| 11 | The record is updated where it is wrong | `DIFFERENTIAL-FINDINGS.md` §7's first and third bullets **deleted**, §4.4 marked fixed with a date and a pointer here; `event.go`'s two exclusion paragraphs (`:29-41`) **deleted** and the claim restated to keep only the zero-quantity reader condition; `reverseTrade`'s comment gains its precondition; `REFERENCE-MATCHER.md` §2.4 unchanged and §9's first bullet is why |
| 12 | Nothing else moved | `go build ./...`, `go vet ./...`, `go test ./... -count=1` green across every package; `go test ./pkg/matching ./internal/refmatch ./internal/tape -race` green; `surface.txt` byte-identical; `TestZeroAllocHotPath` passes unmodified; `gofmt -l` clean on every touched file |
| 13 | The changelog says which is which | §7.3's three entries |

### 10.1 The numbers to record when it is done

None is a pass criterion on its own; they are what §12 is graded against, and each has
a prototype expectation to be checked against.

- **Lines of the golden that move.** Expected: 29, in three parts (§6.3). A larger
  number means a fix is wider than §3.3 or §4.3 says.
- **Tests that fail with the engine fixed and the pins not yet inverted.** Expected:
  exactly 2, both pins. Anything else is a behaviour change nobody argued for.
- **Tests that fail with the invariant strengthened and the engine *not* fixed.**
  Expected: exactly 1 — `TestFailingFOKCorruptsAnIcebergsReserve`, at
  `checkInvariants`, with `negative filled quantity: -6`. If the number is larger, the
  new invariant is finding something this document did not measure, and that is a
  finding to read one at a time before greening it.
- **Time for `FuzzExoticOrders` to find defect 1** with the new alphabet against the
  unfixed engine. Expected: **0.13 s**, minimising to a 12-byte input.
- **`TestDifferentialTape` runtime**, before and after. Expected: unchanged inside
  noise (0.58–0.66 s → 0.52–0.70 s measured).
- **Executions survived** by the strengthened `FuzzExoticOrders` against the fixed
  engine. Expected: ≥ 290,000 in 45 s.
- **`refills`, `triggers`, `ICEBERG`, `STOP` and `CANCELED` counts** in the golden's
  coverage block, before and after. Expected: 2→3, 5→6, 1→2, 4→5, 130→132.

## 11. Sabotage runs required before this counts as done

§10 asks tests to pass. This asks the **fix** to be broken and the new tests to fail.
Per [`TESTING.md`](TESTING.md), nothing above counts until every row has been run and
its result recorded — including the rows whose honest result is "nothing failed".

| # | Sabotage | Must fail | Status |
|---|---|---|---|
| 1 | Restore the iceberg but do **not** skip `reverseTrade` for its prints | The inverted pin (`FilledQty` −9, remaining 12) and the strengthened invariant |**failed as required.** `the iceberg's FilledQty is -9, want 0`, and the second-shape test at `3/-1/4`. The invariant fires on the same state when it is reached before the pin's own assertion: `a resting order (1): negative filled quantity: -9` |
| 2 | Skip `reverseTrade` for iceberg makers but do **not** restore | The inverted pin: the reserve stays empty and the slice stays wrong |**failed as required.** `FilledQty is 3, want 0` — the last slice is left fully consumed and the reserve empty; second shape at `3/0/3` |
| 3 | Take the save at **refill** time instead of at the first print | The inverted pin, and the invariant's "an order with nothing left cannot rest" — the restored order is the fully consumed slice |**failed as required.** The pin at `FilledQty is 3`; the restored order is the fully consumed slice and the invariant says so: `a resting order (1): rests with 0 remaining` |
| 4 | Keep the refill `ACCEPTED`s in the rejected batch | The inverted pin's exact-batch assertion, **and** `conditional/0083` of the golden. **Run on the prototype: `pkg/matching` is otherwise green** — the mirror is blind to it, because the retained `ACCEPTED` carries the *restored* quantity and therefore agrees with the engine. This row is the argument for both the exact-batch assertion and the corpus extension | **reproduced exactly.** The pin's exact-batch assertion (`published [REJECTED ACCEPTED ACCEPTED]`) and `conditional/0083`; nothing else in `pkg/matching` fails |
| 5 | Omit the `book.Remove` before the restored slice is re-added | Deliverable 3's test, on the **level aggregate** only: the order reads 2 and its level publishes 3. **Run on the prototype: no other test in the tree fails**, which is what makes deliverable 3 load-bearing rather than decorative | **reproduced exactly.** `the ask level publishes 8 lots, want 7`, and **nothing else in the whole tree** fails |
| 6 | Restore the iceberg but leave it out of `e.icebergOrders` | The inverted pin's follow-up assertion (the restored reserve must still refill), and `conditional/0084` |**failed as required.** `order 1 is no longer registered as an iceberg`, and `conditional/0084` loses its `ACCEPTED#59/NEW/0` and ends `ask=106:5` — the reserve never reloads |
| 7 | Emit `EventRejected` instead of `EventCanceled` for the refused cascade order | The inverted phantom test — the mirror does not delete on `REJECTED`, which is the whole reason §4.3 chose the other kind |**failed as required.** The inverted phantom test (`mirror [3@200:50 4@120:4]` against `engine [4@120:4]`) **and** the new `TestEventStreamReconstructsBook` scenario, which is what makes deliverable 6 load-bearing |
| 8 | Emit the `CANCELED` **before** the `ACCEPTED` | The inverted phantom test: the mirror deletes an order it has not yet added, then adds it, and the phantom returns |**failed as required.** Same two tests, same phantom: the mirror deletes an order it has not yet added and then adds it |
| 9 | Widen `emitTerminalIfDone` to `Rejected` but drop the `Reason` | `conditional/0088` of the golden only. Record it: if nothing else fails, the reason is carried on the strength of the fingerprint alone |**failed as required, and by more than predicted.** `conditional/0088` is the only golden line that moves, and the inverted phantom test also fails (`the CANCELED carries reason <nil>`) because deliverable 5 asks it to assert the reason. So the reason is *not* carried on the fingerprint alone |
| 10 | Apply both fixes and **revert the corpus extension** | Nothing. `internal/semcheck` goes green and the version bump becomes unjustifiable under Rule 22. The row exists to demonstrate §6.1 rather than to catch anything | **reproduced exactly.** With both fixes applied and the corpus untouched, `go test ./...` was green across every package including `internal/semcheck`. §6.1 is correct |
| 11 | Strengthen `checkInvariants` and leave `FuzzExoticOrders`' alphabet alone | Nothing, in a 60-second fuzz run against the **unfixed** engine. The row proves the alphabet is the load-bearing half of §5.4 |**nothing failed, as predicted.** 546,346 executions in 60 s against the unfixed engine, green. The alphabet is the load-bearing half |
| 12 | Strengthen `checkInvariants`, keep the new alphabet, revert both engine fixes | `FuzzExoticOrders` in **0.13 s**, and `TestFailingFOKCorruptsAnIcebergsReserve` at `checkInvariants` | **reproduced, faster than predicted.** `-fuzz` found it unprompted in **0.51 s** (2.5 s wall) minimising to a 19-byte input, `a resting order (9): negative filled quantity: -1`; both committed seeds fail; and the pin fails at `FilledQty is -6` |
| 13 | Add only `FilledQty >= 0`, to the submitted order only — the obvious fix §5 refuses | Nothing at all. The corrupted order is a maker and the pinning test passes `nil`. This is the row that justifies the whole of §5 |**nothing failed, as predicted.** 1,733,793 executions in 60 s against the unfixed engine, green, and no invariant fires anywhere in `pkg/matching`. The only red is the hand-written tests this slice adds |
| 14 | Re-run the 21 mutations of `REFERENCE-MATCHER.md` §10.2 against the fixed engine | All 21 still caught. This is `DIFFERENTIAL-FINDINGS.md` §8's deliverable 6, **left unrun by that slice** (§11.5) and inherited here |**run, and all 21 still caught** — the deliverable `DIFFERENTIAL-FINDINGS.md` §8 left open. See §13.4 |

Rows 4, 5, 10, 11 and 13 are the unusual ones: four of them ask for a **measurement**
rather than a failure, and the measurement is the point. A sabotage that nothing
catches is the most useful row in the table, because it names exactly which assertion
is doing the work — and in rows 4, 5 and 13 the answer is "one that this slice is
adding".

## 12. How this can fail, stated in advance

So that whoever implements this is not graded on a curve.

- **The blast radius may be wider than §6.3.** The prototype says 29 lines in three
  parts. If the regenerated golden moves a line in `fifo`, `prorata-shard7`,
  `capped-decrement-shard3`, `auction` or `guarded`, one of these fixes reaches
  something this document does not know about, and the temptation at that moment will
  be to accept it as "obviously the fix working". Read it. Those profiles draw no
  icebergs and fire no stops, so a moved line there is a fix touching the ordinary
  path.
- **The save-at-first-print rule rests on §3.6, and §3.6 is an argument.** Both halves
  are proofs about the current walk, and both would silently stop holding if a future
  change let self-trade prevention and a trade land on the same maker, or let a walk
  end with a maker half-consumed and the taker unfilled. Neither is checked
  mechanically. The mitigation is that the strengthened invariant catches the
  *consequence* on any input that reaches it, which is why §5 is in this slice rather
  than the next one.
- **`EventCanceled` may be the wrong kind, and the cost of being wrong is asymmetric.**
  If a real integration wants an execution report that says "rejected" for a refused
  cascade order, §4.3's choice makes them read `Order.Status` off a `CANCELED`. That is
  a worse client API than a `REJECTED` event and a better wire contract than one that
  requires every consumer to change. If someone reports it, the fix is additive — emit
  both, `REJECTED` for the report and `CANCELED` for the book — and it is a change to
  make on evidence rather than in advance.
- **The invariant may be too strong somewhere this document has not run.** It was run
  against the whole of `pkg/matching` on the prototype and found exactly one violation,
  which is defect 1. `pkg/auction`, the recovery paths and the replication drills call
  it less directly. A second violation found during implementation is a **finding**, to
  be read and written up here, not a reason to weaken the assertion.
- **§9's third bullet may not survive review.** Leaving a rejected taker holding
  `FilledQty = 9` while fixing the maker side is a defensible scope line and an
  indefensible-looking one. It is pinned rather than argued, which means the next slice
  inherits a test that fails the moment somebody fixes it, and that is the correct
  outcome either way.
- **This document's own premise was wrong once already.** It was written on the
  assumption that `internal/semcheck` would fire on both fixes. It does not (§6.1), and
  the only reason that was caught before implementation is that the design was
  prototyped and the suite was run. Assume the same about the rest of it.

## 13. What implementing it measured

Written after the code, against the shipped tree rather than the prototype, in the
order §10.1 and §11 ask for it. Two of this document's own claims turned out to be
wrong and both are named here rather than quietly corrected: §11 row 9 and §5.4.

### 13.1 The numbers §10.1 asked for

| Measurement | Expected | Actual |
|---|---|---|
| Lines of the golden that move | 29, in three parts | **47 lines of `diff` output, in exactly the three parts.** The header, **thirteen** appended `conditional/0082`–`0094` lines (§6.2's seven plus §13.6's six), and the coverage block. Checked mechanically, not by eye: `fifo` (361 lines), `prorata-shard7` (241), `capped-decrement-shard3` (241), `auction` (201), `guarded` (12) and `conditional/0000`–`0081` are **byte-identical**, and `conditional` first differs at its 83rd line. The count differs from 29 because the coverage block gained two counters and because §13.6 added six commands this document did not originally ask for |
| Tests failing with the engine fixed and the pins not inverted | exactly 2, both pins | **exactly 2, both pins**, across `go test ./...` — nothing else in any package |
| Tests failing with the invariant strengthened and the engine not fixed | exactly 1, at `checkInvariants`, `negative filled quantity: -6` | **exactly that**, verbatim: `the iceberg's FilledQty is -6`, and on the invariant path `a resting order (1): negative filled quantity: -6` |
| Time for `FuzzExoticOrders` to find defect 1 unprompted | 0.13 s, 12-byte input | **0.51 s of fuzzing (2.5 s wall), minimised to 19 bytes** — `a resting order (9): negative filled quantity: -1`. The 9 and the −1 are not the same run as §5.4's but the shape is |
| `TestDifferentialTape` runtime, before and after | 0.58–0.66 s → 0.52–0.70 s | **0.464–0.771 s before, 0.458–0.465 s after** (three runs each; the 0.771 is a cold first run). Inside the noise, as predicted |
| Executions survived by the fixed engine | ≥ 290,000 in 45 s | **444,496 in 46 s** |
| `refills`, `triggers`, `ICEBERG`, `STOP`, `CANCELED` | 2→3, 5→6, 1→2, 4→5, 130→132 | **2→3, 5→6, 1→**3**, 4→5, 130→**134**.** Three exactly; `ICEBERG` and `CANCELED` are one and two higher than predicted because of §13.6's six extra commands, which this document did not know about when it wrote the row |

`internal/apicheck/testdata/surface.txt` is byte-identical. `TestZeroAllocHotPath`
passes unmodified. `gofmt -l pkg cmd internal` reports only `cmd/obwasm/main.go`,
which is unmodified by this slice and was already unformatted on `main`.

### 13.2 §5.4 was wrong about the alphabet, and the correction is the whole symbol

§5.4 says the fuzz target's alphabet needs a fill-or-kill symbol. It does, and that
alone is not enough: **every other draw in `FuzzExoticOrders` uses the account `"x"`,
so an `"x"` fill-or-kill meets `"x"`'s own iceberg, is stopped by self-trade prevention
and never prints.** Measured with the symbol added under `"x"`: **1,315,252 executions
in 90 s against the unfixed engine, green.** The defect is a *stranger's* rejected
order leaking a client's reserve, so the stranger has to exist; with the symbol drawn
as `"y"` the same target finds it in half a second. The comment on that case says so,
because it reads like an arbitrary string and is not.

Both seeds are committed: the hand-built six-byte one (an iceberg buy of 10 shown 5 at
102, and a stranger's fill-or-kill sell of 12 at 101), which is legible, and the
nineteen-byte input the fuzzer itself minimised to, which is the artefact.

### 13.3 §6.1 was right, and it is the finding that shaped the slice

The task this slice was given asserted that `internal/semcheck` **would** fire on both
fixes. It does not, and §6.1 predicted that. Measured on the shipped tree: with both
engine fixes applied and the corpus untouched, `go test ./...` was **green in every
package including `internal/semcheck`**, and only the two pinning tests failed. The
corpus was extended first and the version moved on the strength of the golden lines that
appeared — which is the order Rule 22 requires and the opposite of the order the task
described.

### 13.4 §11 row 14: all 21 mutations, run

`DIFFERENTIAL-FINDINGS.md` §8's deliverable 6, left unrun by that slice (§11.5) and
inherited here. Each mutation was applied to the shipped tree and `TestDifferentialTape`
run against it. **All 21 caught.**

| # | Mutation | Result |
|---|---|---|
| 1 | `push` inserts at the head | caught, `book-rank` |
| 2 | limit-buy crossing test `<` → `<=` | caught, `verdict` |
| 3 | print at the taker's price | caught, `trade-price` |
| 4 | fill quantity is the maker's, not the minimum | caught, `verdict` |
| 5 | level aggregate not decremented on a partial fill | caught |
| 6 | `Reduce` removes and re-adds | caught, `book-rank` |
| 7 | `takerSTP` ignores the per-order mode | caught, `trade-count` |
| 8 | `decrement` does not shrink the aggregate | caught |
| 9 | `reverseTrade` does not restore the maker | caught |
| 10 | a refused order burns no order id | caught, `order-id` |
| 11 | `reverseTrade` rewinds `tradeSeq` | caught, `trade-id` |
| 12 | `Remove` skips the level list | caught (as the livelock §10.2 documents) |
| 13 | `wouldCross` uses `>` for a buy | caught, `verdict` |
| 14 | a market remainder rests | caught, `verdict` |
| 15 | cancel-only accepts a new order | caught, `verdict` |
| 16 | pro-rata remainder in reverse arrival order | caught, `trade-count` |
| 17 | `Replace` resizes in place, keeping priority | caught |
| 18 | `emitCancel` dropped for an STP-cancelled maker | caught |
| 19 | `nextID` reuses a sequence number | caught, `order-id` |
| 20 | `executeTrade` over-fills the maker | caught |
| 21 | `executeTrade` publishes the print twice | caught |

**Three realisations had to be rewritten before the mutation was reachable, and that is
worth as much as the row it produced.** Mutation 10 applied to `checkOrderCaps` is
unreachable — none of the three differential profiles sets a size or notional cap — and
had to be moved to the post-only refusal, which the tapes do draw. Mutation 14 written
as "rest if the order has a price" is a no-op, because a market order's price is zero.
Mutation 17 restricted to an unchanged price goes **green**, and only the unconditional
in-place resize is caught — which is §10.2's own footnote about that mutation arriving
a second time. A mutation whose realisation is unreachable reports "the oracle is
strong" and means "the input never got there", which is exactly the failure
`JOURNAL-COMPLETENESS.md` §1 names.

### 13.5 Two deliberate departures from this document

**The two coverage counters do not mean what §6.4 says.** §6.4 defines
`IcebergRestores` as "a command whose verdict was REJECTED after which an iceberg's
`Refills` is **lower** than it was before". That is unreachable against a correct fix:
the restore returns the counter to exactly its pre-command value, which is also the
per-order maximum `semcheck.go` tracks, so the comparison is equality and never "lower"
— against the *unfixed* engine it is higher. The counter as shipped counts a
fill-or-kill **refused while priced into a resting iceberg that came back with its
reserve and refill counter unchanged**, and its doc comment states plainly that it
cannot assert the restore happened, only that the corpus still reaches the setup. The
golden line is the correctness assertion; this is the guard that keeps the command in
the corpus. `CascadeTerminals` is as specified. Both are non-zero (1 each) and both are
guard rows in `TestTheFingerprintReachesEveryDecidedBehaviour`.

**`internal/refmatch` is unchanged**, per §9's first bullet. Icebergs and OCO stay tier
1-absent, the differential sweep cannot reach defect 1 at all, and a green sweep after
this slice is evidence of nothing about it. The oracles for defect 1 are the two
hand-written tests, the strengthened invariant with its new fuzz symbol, and the
fingerprint's thirteen new lines (§6.2 plus §13.6) — and nothing else.

### 13.6 The restore jumped the queue, and the fingerprint could not see it

**This is the one place the code disagreed with this document, and the document was
right.** Adversarial review of the shipped tree found it; it is recorded here at
length because the shape of it is more useful than the one-line fix.

**What the code did.** `settleInto`'s fill-or-kill failure branch called
`restoreSavedIcebergs()` — all saves, all at once — *before* the loop that reverses the
walk's prints. `reverseTrade` re-adds each fully consumed maker **at its own print**,
which is what puts a level back in the order it was in. Restoring every iceberg up front
therefore put it back **ahead of every other maker the same failed order removed**,
including makers that had been resting in FRONT of it.

**Measured.** Level `@100` in arrival order: `u3` sells 5 (id 1), then an iceberg `u1`
sells 9 shown 3 (id 2). A stranger's fill-or-kill buy of 20 at 100 takes `5 + 3 + 3 + 3
= 14` and is **REJECTED**. Three trees, same four commands:

```
                             level @100 after the rejection   next 1-lot buy prints against
main, before either fix      [1 rem=5] [2 rem=9]              maker 1   (order right, reserve LEAKED)
restore-before-the-loop      [2 rem=3] [1 rem=5]              maker 2   (reserve right, ORDER WRONG)
restore-at-the-first-print   [1 rem=5] [2 rem=3]              maker 1   (both right)
```

The middle row is the regression: a client who sent nothing takes time priority from a
client who was there first, paid for by a third party's refused order — and `main`'s own
behaviour, wrong as it was about the reserve, had the ordering right. So this was never
a trade-off between the two; it was a fix that gave away something it did not have to.

**Why nothing caught it.** The id SET at the level is right and only the ORDER is
wrong, so it moves no fill counter, no level aggregate, no event batch and no verdict.
`checkInvariants` passes. Both hand-written iceberg tests pass, because in both of them
the iceberg is **alone at its price** — the same shape of blind spot as §6.1's, one
level down. And `internal/semcheck` was green, for exactly the same reason: the corpus's
`i2` is alone at 102.

**The fix**, in one place: the restore moved *inside* the reversal loop and happens at
the iceberg's **first** print, with its later prints skipped as before
(`icebergSave.restored` is what makes it once-only). That reproduces `reverseTrade`'s
own ordering by construction rather than by coincidence, and it makes §3.4's acceptance
rule and §8's first row literally true instead of approximately true.

**What now holds it**, because a fix with no oracle is the thing this document exists to
refuse:

| Oracle | What it asserts | Result on the restore-before-the-loop tree |
|---|---|---|
| `TestFailingFOKRestoresAnIcebergAtItsOwnPlaceInTheQueue` | the level's id order across the rejection, **and** that the reserve is still hidden (8 lots published, not 14), **and** which maker the next lot prints against | `the level came back [2 1], want [1 2]` |
| `TestRejectedFOKPreservesEveryLevelsQueueOrder` | over 60 committed tapes × 40 commands: after every REFUSED fill-or-kill, **every** level holds the same ids in the same order as before the command | `seed 15, command 20: … rewrote level BUY 96 from [4 15 2] to [4 2 15]` |
| `internal/semcheck`, six more corpus commands | a maker resting **in front of** an iceberg at one price, both swept by a failing fill-or-kill, then a one-lot buy whose maker id is printed on the golden line | `m65` → `m66`, plus three moved digests |

The generated one is narrow on purpose and each exclusion is a case where a refusal
legitimately moves a level: distinct accounts throughout (a self-trade-prevention
DECREMENT leaves an untouched maker resting mid-level while the reversal re-adds behind
it), no OCO (whose leg a rejected fill-or-kill does cancel by design, §9), and FIFO
allocation (pro-rata prints within a level in allocation order, not arrival order).

**The corpus is now thirteen commands, not §6.2's seven**, and the golden diff is
`conditional/0082`–`0094` plus the coverage block — still appended, never inserted,
still nothing else moved. `icebergRestores` is 2. This is §6.1's lesson arriving a
second time inside the slice that wrote it down: the corpus reached a failing
fill-or-kill against an iceberg and still could not see *where the iceberg came back*,
because one command's worth of setup was missing.

### 13.7 §7.1's replay claim is true only of the snapshot path

Review measured this and it is **pre-existing, not introduced here** — but it bounds a
sentence this slice wrote, so it is this slice's to correct.

`pkg/wal`'s `AppendIceberg` logs `ib.Order`, and `types.NewIcebergOrder` has *already*
shrunk `Order.Quantity` to the display size by the time anything can log it
(`iceberg.go:34-41`). So the journal records `qty=3 display=3` for a 9-lot iceberg shown
3, and a replay that rebuilds it with `NewIcebergOrder(entry.Order, entry.DisplayQty)`
computes `hidden = 3 - 3 = 0`. **A log-only recovery reconstructs every iceberg with an
empty reserve.**

Measured, identical on `main` and on this tree, with no fill-or-kill involved:

```
live engine          iceberg sell 9 @100 shown 3 → Hidden 6; a later buy of 9 prints 9
wal.Recover(cfg, "", walPath)   → Hidden 0; the same buy prints 3
wal.Recover(cfg, snapPath, walPath) → unaffected; the snapshot carries Hidden and Refills
```

§7.1 says "any journal containing a fill-or-kill that failed after consuming at least
one full slice of a resting iceberg replays to a different, and correct, book on the new
build." That is true **when a snapshot supplies the reserve**. On a log-only replay the
reserve never exists, so the walk cannot consume a second slice and the fixed path is
not reached at all. `CHANGELOG.md` carries the bound; §7.1 above does not, and this
paragraph is the correction rather than an edit to it, per this document's rule about
where corrections live.

It was **pinned, not fixed**, in `pkg/wal/iceberg_reserve_pin_test.go` — the first test
in the repository to cover iceberg WAL recovery at all — carrying the sentence a fix
must come and delete. It was not fixed here for the reason §9 gives for everything else
it declines: it is a journal-format question in another package (what a `KindIceberg`
entry's `Quantity` field *means*, and what a build that meets an old log should do about
it), with its own compatibility argument to make, and folding it into a matching fix
would be exactly the "change wearing another change's clothes" this repository's
semantics gate exists to prevent.

**FIXED — 2026-08-18, in [`ICEBERG-DURABILITY.md`](ICEBERG-DURABILITY.md).** That
compatibility argument is now written and carried out. `AppendIceberg` records a COPY of
the order at the client's total plus `Entry.TotalQty`, a witness whose only job is to be
absent in older records; `restoreEntry` uses the total when it is there and
`Order.Quantity` when it is not; and a record that cannot state its total is **refused**
by `Recover` when it would be replayed (`ErrIcebergReserveUnknown`,
`-wal-accept-iceberg-loss N`) rather than rebuilt small. The pin above is inverted and
keeps its name. What that document's audit added to this one's ledger: the same
constructor mutation also lets an iceberg **evade `Config.MaxOrderQty`** and be
**refused by `Config.MinOrderQty` for a floor it exceeds 18×**, both now pinned in
`pkg/matching/engine_iceberg_test.go`, and it had been diverging every replication
follower from its primary on the first iceberg with nothing in `examples/` exercising it
(now `TestDrillD11_AnIcebergReplicatesWithItsReserve`).

The bound `CHANGELOG.md` carries is therefore now historical: on a log-only replay under
**this** build the reserve does exist, so §7.1's claim holds on both paths — for logs
written by this build. A log written before it cannot state the reserve at all, which is
why recovery refuses instead of quietly making the old sentence true again.

### 13.8 `REFERENCE-MATCHER.md` §7.1's argue-down rule fired, and the answer is "not yet"

§5's strengthened `checkInvariants` runs inside `TestDifferentialTape` — the harness
calls it after every command, deliberately, so that "the book crossed after command 412"
beats a rank divergence with the same cause. The consequence, measured on the shipped
tree by re-running the mutations:

| Mutation | `REFERENCE-MATCHER.md` §7.1 says | Now fails on |
|---|---|---|
| 5 — level aggregate not decremented on a partial fill | "**level aggregate only** — nothing else sees it" | `level SELL 98 publishes 9 lots and the 1 orders resting there hold 4` — the new invariant |
| 9 — `reverseTrade` does not restore the maker's quantity | "book quantity" | `a resting order (41): rests with 0 remaining` — the new invariant |

Both are still caught, so nothing is broken. But §7.1 carries a rule — *"if any of
those three mutations is caught by something cheaper, the corresponding assertion is
over-engineered and should be argued down"* — and mutation 5 is the mutation that
justified the asymmetric level comparison. The rule has fired.

The answer recorded there and here is **not to remove the assertion in this slice**, and
the reason is not sentiment: the two checks compare different things. The invariant
compares the engine's level aggregate against the engine's **own** order list; the
differential assertion compares it against an **independent implementation**. What §7.1's
rule establishes is that mutation 5 no longer *justifies* the assertion — not that the
assertion is unjustified. So §7.1 loses mutation 5 as its evidence and gains an open
item: find a mutation the asymmetric level comparison catches and the whole-book
invariant does not, or argue the assertion down in the slice that fails to. A search
here did not find one — a level dropped from the price vector while its order still
rests is invisible to the invariant, as predicted (`OrderBook.Orders()` walks the price
vectors), but it is caught by `book-membership` and by the mirror, not by the level
comparison.
