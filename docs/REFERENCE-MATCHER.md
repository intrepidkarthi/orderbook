# Reference Matcher — An Oracle That Does Not Share Code With What It Checks

Status: **built** — milestone M9 in
[`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md), specified before the code as this
repository does it. §10 records what building it found, including three engine
defects and five places this document turned out to be wrong about its own design ·
Author: Karthikeyan NG · Last updated: 2026-08-17

> **This paragraph describes the position this document was written from, in
> 2026-08-17's morning. It is kept as written; §10 is what happened.**
>
> **M9 is labelled "partial" and is near zero on its central ask.** The tests that
> look model-based are one of two things: invariant-only on generated input
> (`checkInvariants`, `pkg/matching/fuzz_test.go:13-27`, driven by `FuzzEngine`,
> `FuzzExoticOrders` and `TestSoak`), or exact-output on hand-written input
> (`TestEventStreamReconstructsBook`, ~25 scenarios). **There is no reference
> matcher in this repository**, and no generated tape is ever compared against an
> expected answer.
>
> An invariant suite cannot catch a matcher that is self-consistently wrong. A book
> that allocates LIFO within a price level never crosses, conserves quantity, and
> keeps every remaining quantity non-negative. It passes every assertion
> `checkInvariants` makes, on every tape, forever. The property that fails is
> "the second order at 100 filled before the first", and nothing in this repository
> currently has an opinion about that on generated input.

Companion documents:
- [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §1 — "an exhaustive check
  over an incomplete input space reports completeness, and the report is
  load-bearing." §4.2 and §4.4 of this document are that lesson applied twice: once
  to the command alphabet, and once to a second alphabet that document did not have
  to think about — the engine's **configuration**.
- [`TESTING.md`](TESTING.md) §"The rule" — "A new test does not count until it has
  been run against code deliberately broken in the way it claims to detect." §7.1
  names eighteen such breakages by file and expression, and §8 is the run.
- [`TRADE-BUST.md`](TRADE-BUST.md) §4.5 and §7 — the pattern of shipping a first
  pass with a written-down deferral list rather than a half-built everything, and
  the record of a digest test that passed against the exact sabotage it existed to
  catch.
- [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) and [`LOG-ROTATION.md`](LOG-ROTATION.md)
  — the house shape for a spec with measurable acceptance criteria and a sabotage
  table.

---

## 1. Why this exists

### 1.1 The gap, stated precisely

Three things are true at once, and only the third is a problem.

1. The **invariant** coverage on generated input is good. `checkInvariants` runs
   after every operation of `FuzzEngine` and `FuzzExoticOrders`, and asserts the
   book is never crossed, that `FilledQty + RemainingQty == Quantity`, and that no
   remaining quantity goes negative.
2. The **exact-output** coverage on hand-written input is good.
   `TestEventStreamReconstructsBook` rebuilds an L3 book from the event stream
   alone across roughly twenty-five scenarios, including all five STP modes, FOK
   both ways, post-only, iceberg refill, OCO both legs and a stop cascade.
3. **The two never meet.** No generated tape is compared against an expected
   answer, and no hand-written scenario has more than a few dozen commands in it.

The class of defect that lives exactly in that hole is a matcher that is wrong the
same way every time. Every property in (1) is closed under that kind of error:

| Wrong rule | Book crossed? | Quantity conserved? | Remaining ≥ 0? | Caught today |
|---|---|---|---|---|
| LIFO within a price level | no | yes | yes | no |
| Trade prints at the taker's price | no | yes | yes | no |
| `Reduce` re-queues instead of shrinking in place | no | yes | yes | no |
| Level aggregate not decremented on a partial fill | no | yes | yes | no |
| Pro-rata remainder to the largest instead of the earliest | no | yes | yes | no |

Four of those five are one-token edits. The fourth is a one-line deletion in
`pkg/matching/engine.go:1367` and it corrupts every L2 market-data feed derived
from the book while leaving the L3 book perfect.

### 1.2 Why now, and why this slice can lean on replay

M9 was deferred one round, deliberately. A differential harness that asserts
"replay equals live" over a generated tape is only as good as the replay path, and
until commit `0958c5a` the replay path **provably dropped auctions**: `cmdSetPhase`
had no case in `logCommand`, so a venue that ran an opening uncross and crashed
recovered into the wrong phase with a crossed book
([`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §2, §3). Building an oracle
on top of that would have produced a harness that agreed enthusiastically with a
known-wrong answer.

That is fixed and committed. `SetPhase` is journalled, `RestoreAfter` re-runs the
uncross, `buildTape` (`pkg/wal/runner_recovery_test.go:70`) now runs a real session
with four phase transitions, and `TestEveryMutatingCommandReachesTheLog` fails if a
mutating command is added without a log record. **Replay is now a trustworthy
oracle**, which is why §3.5 is allowed to use it and why §2.3 is allowed to defer
the auction to a later tier: the auction is not un-oracled, it has a *different*
oracle, checked at all 24 boundaries of a two-auction session.

### 1.3 What a differential test can and cannot prove

Stated up front so nothing later reads as a stronger claim than it is.

A differential test proves **disagreement**. Two implementations that agree may
both be wrong, and they will both be wrong in exactly the case where they were
written from the same wrong sentence. That is why every matching rule this document
relies on is written out in prose here, in §3.4 and §7.1, rather than left implicit
in two bodies of code: the prose is the thing under review, and the model is an
implementation of the prose.

So the suite has three legs and this is the third, not a replacement for either
other:

- **hand-written scenarios** prove the agreed answer is the *right* answer;
- **invariants on generated tapes** prove nothing catastrophic happens on inputs
  nobody thought about;
- **the model on generated tapes** proves the optimised engine still computes what
  a slow obvious implementation computes.

## 2. What the model is, and what it deliberately is not

### 2.1 Shape

`internal/refmatch` is a limit order book written to be **read**, not run. Its
representation is:

```go
// illustrative; the point is the shape, not the field list
type book struct {
    bids []resting // sorted: price descending, then arrival ascending
    asks []resting // sorted: price ascending,  then arrival ascending
}
```

Two sorted slices, scanned linearly, re-sorted on insert. Cancel is a linear
search. Depth is a fold. There is no index, no pool, no map from id to node, and no
free list.

**This is the point, not a weakness, and it is worth saying because it will look
like one.** The production book keeps a `map[int64]*node`, a pooled node
allocator, and a separate sorted price vector per side
(`pkg/orderbook/orderbook.go:92-116`), and its level aggregate is maintained
incrementally through `push`/`unlink` with a `contributed` field that is
deliberately *not* `order.RemainingQty` (`:70-88`). Every one of those is a place
where the engine can disagree with itself. The model has no aggregate to maintain —
it computes depth by summing the orders that are actually there — which is exactly
why §3.2 assertion 4 can compare the engine's maintained aggregate against the
model's computed one and learn something.

The model must be O(n) per command and must never be optimised. If a future
profile shows the differential harness is slow, the answer is shorter tapes or
fewer seeds, never a faster model. A model with an index has the engine's bug
class.

### 2.2 The independence rule, and how it is enforced mechanically

**`internal/refmatch` and `internal/tape` may import the Go standard library and
each other, and nothing else in this module.** No `pkg/types`, no `pkg/orderbook`,
no `pkg/matching`, no `pkg/auction`.

`pkg/types` is the tempting exception and it is refused. `types.Order.Fill` is
where `FilledQty + RemainingQty == Quantity` is maintained; a model that calls it
inherits whatever that arithmetic does, and the invariant the harness would then
"check" is one both sides get from the same seven lines. The model declares its own
plain-int64 order and trade structs and does its own arithmetic. Side, order type,
TIF and STP mode are the model's own small enums, not the production string
constants.

The translation between the two vocabularies lives in **exactly one file** — the
adapter in `pkg/matching/differential_test.go` — which is allowed to know both
sides and is allowed to know nothing else. Concentrating the coupling in one
reviewable place is the whole design; spreading it thinly is how independence is
lost without anyone deciding to lose it.

Enforced, not promised: **`TestReferenceMatcherImportsNothing`** parses the import
blocks of every file in `internal/refmatch` and `internal/tape` with `go/parser`
and fails on any path that is neither standard-library nor the other of the two.
A prose rule about imports is a rule that survives until the first afternoon
someone needs `types.Side`.

#### The half of independence that is not enforced, and is not fully achieved

There are two kinds of independence here and only one of them has a test.

**Mechanical independence** — no shared code, no shared types, no shared arithmetic
— holds, and the `go/parser` guard has teeth (adding a `_ "pkg/types"` import to
`refmatch.go` fails it with the right message).

**Derivation independence** — the model written from the *specification* rather than
from the engine's source — is weaker than this document originally implied, and the
evidence is in the files. Two comment lines are byte-identical across the two
implementations (`refmatch.go:570` / `engine.go:1288` and `refmatch.go:579` /
`engine.go:1297`, both "A limit buy/sell only crosses asks/bids at or below/above its
price"); `refmatch.go:622` and `engine.go:1339` are both "// fall through and trade";
`commands.go:86` and `engine.go:966` differ only by a contraction. The five STP
branches appear in the same order with the same early-return/continue shape, and
`settle` applies the state gate, post-only, the walk, market, IOC, FOK and GTC in
`settleInto`'s exact order. `matchProRata` is close to a line-for-line parallel of
`proRataAllocate`.

**What that costs, precisely.** Any rule the engine author got wrong and the model
author reproduced is invisible: the harness reports agreement. This is not
hypothetical here — it is the *documented* state of two of the three findings in §10.
The model deliberately canonises the engine's position on the FOK last-trade-price
and on events dropped from a rejected order, which is why both are pinned by
hand-written tests instead. §1.3 says a differential test proves disagreement and
that two implementations written from the same wrong sentence are both wrong; this
section is the measurement of how close to that failure mode this particular pair
sits.

**What is not affected.** Mutation testing still works, because a mutation changes
one side only: 21 deliberate engine mutations were caught, and an independently
written set of 19 more caught every one aimed inside tier 1. A shared *wrong
sentence* is the exposure; a shared *right* sentence costs nothing.

**Not closed by rewording the comments.** Deleting the duplicated lines would remove
the evidence and none of the dependence. Closing it properly means a second model
written from §2.3 and §3.4 by someone who has not read `engine.go` — worth doing, out
of scope here, and dishonest to imply otherwise.

### 2.3 Tier 1 — what the first version models

The **continuous session**, in full:

| Area | In tier 1 |
|---|---|
| Order types | limit, market |
| Time in force | GTC, IOC, FOK |
| Flags | post-only, `Privileged`, `TradeGroupID`, per-order `STPMode` |
| Commands | submit, cancel, reduce, replace, cancel-all-for-user |
| Controls | halt, resume, cancel-only (and the transitions between them) |
| STP | all five modes: cancel-newest, cancel-oldest, cancel-both, decrement, allow |
| Allocation | price-time (FIFO) **and** pro-rata, as two configurations |
| Caps | `MaxOrders` (the book-size cap), because it changes a verdict |

Two of those need their inclusion defended.

**All five STP modes, in the first version.** They are pure matching-loop
semantics — five branches in the model, twenty lines — and they are the single most
likely place in this engine to be self-consistently wrong. `STPDecrement` shrinks a
resting maker in place with no trade to explain it, emits `EventReplaced`, and
calls `book.UpdateOrderQuantity` to keep the level aggregate in step
(`pkg/matching/engine.go:1396-1407`); `STPCancelOldest` removes a maker mid-walk
and continues; `STPCancelBoth` does both and stops. Every one of those touches book
state through a path the hand-written scenarios exercise once each and a generated
tape exercises in combination.

**Pro-rata, with a caveat stated rather than hidden.** `proRataAllocate`
(`pkg/matching/engine.go:1641-1668`) distributes the integer remainder **greedily
in slice order**, which is arrival order within the level. That is an *arbitrary*
rule: nothing in market microstructure forces it, and a different venue would round
differently. A model that reimplements an arbitrary rule from the code is not
independent — it is a transcription. So the rule is written out here, as
specification text, and the model implements *this paragraph*:

> Pro-rata allocation of quantity `q` across eligible resting orders `o1..on` at a
> price level, in arrival order: each order is provisionally allocated
> `min(q * remaining_i / total, remaining_i)` using truncating integer division.
> The shortfall `q - sum(provisional)` is then distributed one order at a time in
> **arrival order**, each taking as much of the shortfall as its unallocated
> remainder allows, until the shortfall is zero.

**Self-trade prevention at a pro-rata level.** A second paragraph, added when the
first one's last sentence turned out to be a defect (§10.1(c),
[`DIFFERENTIAL-FINDINGS.md`](DIFFERENTIAL-FINDINGS.md) §5). It used to read
"orders belonging to the taker are excluded from `total` and receive nothing; a
level with no eligible liquidity ends the walk rather than skipping to the next
level", and that skip left the taker's remainder resting **across the spread** —
bid 100 / ask 99 on a continuous book, in all five STP modes, because `matchProRata`
never consulted the mode at all. A venue configured `ALLOW` did not get the
self-trade it had asked for and one configured `CANCEL_BOTH` cancelled neither
order: pro-rata was silently overriding the venue's STP configuration. The
replacement deletes a rule rather than adding one — self-trade prevention applies at
a pro-rata level exactly as it applies under price-time priority, and the allocation
rule has no opinion about who owns what:

> At each price level the taker touches, resting orders are partitioned by the
> self-match test into the taker's own and the rest — **unless the taker's mode is
> `ALLOW`**, in which case there is no self-match to prevent and the taker's own
> orders take part in the allocation exactly as any other order does, which is what
> `ALLOW` already means under price-time priority. The taker is allocated pro-rata
> across the rest, exactly as specified above. If that exhausts the taker, the walk
> ends and self-trade prevention never fires — the taker never needed its own
> liquidity. If the taker still needs quantity at that level and the only orders
> left there are its own, self-trade prevention is applied to them **in arrival
> order** under the taker's mode: `CANCEL_NEWEST` cancels the taker and ends the
> walk; `CANCEL_OLDEST` removes the maker, emits its `Canceled`, and re-allocates
> whatever remains at the level; `CANCEL_BOTH` does both and ends the walk;
> `DECREMENT` shrinks both by their overlap, emits `Replaced` or `Canceled`, and
> continues; and the level is then re-read, because both an allocation and a
> prevention decision change what is resting there. A level is never skipped, and a
> taker never rests at a price that crosses a resting order it could have been
> prevented against.

Two things in that paragraph are choices rather than consequences, so they are
stated here rather than left to the code.

**Eligible liquidity trades first; prevention fires when the taker's own order would
be needed.** This is the position-free analogue of what price-time priority does.
Under FIFO the taker trades with everything ahead of its own order and prevention
fires when the walk *reaches* it; whether anything trades first depends on queue
position. Pro-rata has no queue position, so "reaches it" has to be defined, and the
only definition that does not invent an ordering is "the level's eligible liquidity
is exhausted and the taker still wants more". The cost, stated: a `CANCEL_NEWEST`
taker under pro-rata may print *more* than the same taker under FIFO would have, if
its own order happens to sit at the front of the FIFO queue. That is a difference
between two allocation policies, which is what an allocation policy is.
`TestProRata_MixedLevelTradesEligibleBeforeSTPFires` is the only case that
distinguishes this rule from applying prevention first, so it is written on a level
holding both the taker's own order and a stranger's.

**`ALLOW` is not demoted.** Under `ALLOW` the taker's own order is in the allocation
from the start rather than being reached after the strangers, because `ALLOW` says
the self-match test does not fire — and under FIFO it trades from its own place in
the queue rather than from behind everyone else's. A mode string the engine does not
recognise falls through to the same behaviour, in both allocation policies.

If either paragraph is wrong, both implementations are wrong together and the
harness is silent. That is the honest limit of a differential test (§1.3), and the
mitigation is that the paragraphs are short, in a document, and reviewable — which
`proRataAllocate` plus a caller is not.

### 2.4 What tier 1 defers, and why that split

The split is not "hard things later". It is: **tier 1 is everything that changes
what the matching walk does; a later tier is everything that sits on top of a
matching walk it does not change.**

| Deferred | Tier | Why |
|---|---|---|
| DAY, GTD | 2 | These are functions of a *clock*, not of the book. Modelling them means modelling `expiryQueue` and the lazy-expiry rule ("a deadline is checked whenever a command arrives", `pkg/matching/engine.go:713`), which is a design about *when*, not *what*. It is the cheapest tier-2 item and should be next. |
| Stops, OCO, iceberg, pegged, trailing | 2 | Each is a trigger, pairing or refill rule that *composes with* the matching core and does not alter it: a triggered stop becomes an ordinary taker. The model's value is highest at the core, and these already have hand-written event-conformance coverage plus `FuzzExoticOrders` for invariants. |
| Pre-open / closing auction, and the uncross | 2 | Modelling `Uncross` means modelling `pkg/auction`'s volume-maximising clearing-price choice and its tie-breaks — a *second algorithm*, where a wrong model produces a wrong oracle rather than a caught bug. And it is not un-oracled: since `0958c5a` the auction is compared at every crash boundary of a two-auction session against its own replay. Different oracle, not no oracle. |
| Trade bust | 2 | A bust deliberately does not rewind the book ([`TRADE-BUST.md`](TRADE-BUST.md) §2), so its differential content is a registry comparison, not a matching comparison. Real, cheap, low value here. |
| Price band, mark price, guardrail, min-resting-time, dust floors, per-account caps, client-id dedup, band-breach pause, iceberg jitter | never modelled | **Admission controls, not matching.** They decide whether an order reaches the walk, and they are configuration. §2.5 is how they are kept from silently going unchecked. |
| Multi-symbol / shard routing | out | `ShardIndex` is modelled as an id-composition rule (§3.2) because it changes customer-visible ids. Routing between books is [`MULTI-SYMBOL.md`](MULTI-SYMBOL.md)'s. |

Tier 2 is a follow-on slice with its own spec section, not a promise buried in a
table. What matters is that the boundary is *enumerated* rather than left to
whoever reads the code next — which is §2.5's job.

### 2.5 The second alphabet: configuration

[`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §1 diagnosed an exhaustive
test running over an incomplete **command** alphabet. This slice has the same
exposure along a second axis that document never had to consider.

`matching.Config` has twenty-six fields (`pkg/matching/engine.go:183-322`). The
differential harness runs the engine in a stated configuration; every knob the
model does not implement must be at its **disabling** value, or the harness is
comparing a model that does not know about a control against an engine that is
applying it. Today that would be a comment. Tomorrow someone adds
`MinOrderNotional` to `DefaultConfig`, the harness starts seeing rejections the
model cannot predict, and the natural fix — "ignore rejections whose reason the
model does not know" — quietly turns the oracle off.

So `Config` gets the `cmdKindCount` treatment, by reflection rather than by a
sentinel:

```go
// illustrative shape; the point is that every field appears
var configClassification = map[string]configRole{
    "SelfTradePrevention": modelled("all five modes are tier-1 matching semantics"),
    "ProRata":             modelled("the second allocation rule; see REFERENCE-MATCHER.md §2.3"),
    "MaxOrders":           modelled("a book-size cap changes a verdict, so the model applies it"),
    "PriceBand":           mustBeZero("an admission control on the decimal path; tier-1 tapes run with no collar"),
    "MinRestingTime":      mustBeZero("reads the wall clock, which the model does not have; see §3.3(a)"),
    "Clock":               harnessOwned("the harness injects a deterministic step clock"),
    // ...
}
```

and three tests over it:

1. **`TestEveryConfigFieldIsClassified`** walks `reflect.TypeOf(Config{})` and
   fails on any field missing from the table. Adding a config field without
   deciding whether the model knows about it becomes a test failure at the moment
   the field is written.
2. **`TestHarnessConfigMatchesItsClassification`** takes the exact `Config` the
   differential harness uses and asserts every `mustBeZero` field is at its zero
   value. This is the one with teeth: it fails if someone "improves" the harness by
   turning on a control the model cannot predict.
3. **`TestModelledConfigFieldsAreExercised`** asserts that every field classified
   `modelled` is set to a non-default value by at least one harness profile.
   Classifying `ProRata` as modelled and never running a pro-rata profile is the
   config-axis form of a zero generator weight, and it reports as coverage.

The `mustBeZero` reasons are strings for the same reason `readOnly`'s are
(`pkg/matching/command_alphabet_test.go:54`): a future author turning a knob on has
to write down why the model can now cope, and a reviewer gets a sentence to
disagree with instead of a flipped flag.

## 3. Equivalence

**This is the load-bearing section.** Every permitted difference is a hole in the
oracle, so each one below is justified and each one names what still constrains it.

### 3.1 The observation

The harness drives one tape through both sides and, **after every command**,
produces from each an `Observation` — a plain comparable value, no pointers, no
`time.Time`:

```go
type Observation struct {
    Verdict  Verdict       // final status of the submitted order + mapped reason
    Trades   []Trade       // this command's prints, in the order they printed
    Book     []Resting     // full L3, both sides, price-then-rank, complete depth
    Levels   []Level       // L2 aggregate: price, total quantity, order count
    Events   []Event       // this command's events, in order: kind, subject, user, reason
    State    State         // open / halted / cancel-only
    LastTradePrice int64
    NextOrderID    int64   // the id the next submit will be assigned
    NextTradeID    int64   // the id the next print will be assigned
}
```

Both sides produce it. `reflect.DeepEqual` decides. **The comparison is over the
whole struct, not a field list**, which matters: a field-list comparison is a place
where a future field is added and silently not compared, and the whole shape of
this document is that silence is the enemy. A field that is legitimately not
comparable does not get skipped in the comparator — it does not go in the struct
(and §3.3 says where it went instead).

### 3.2 What must match exactly

1. **The verdict.** The submitted order's terminal status, and when it is a
   rejection, the *reason* as a mapped enum. Error strings are not a contract;
   `types.ErrPostOnlyWouldCross` versus `types.ErrFOKCannotFill` is. The adapter
   holds the sentinel-to-enum map and **`TestEveryTierOneRejectionIsMapped`**
   asserts totality: every `types.Err*` reachable on the tier-1 path appears in it,
   so a new rejection reason cannot arrive as "unknown, therefore equal".

2. **The trades of that command: count, order, and every field.** Price, quantity,
   taker side, taker order id, maker order id, buyer user, seller user, trade id,
   sequence number, and the `Auction` flag. *Order within the command matters* —
   it is the price-time walk, and a set comparison would call a reversed walk equal.

3. **The resting book as a ranked L3 list.** For each side, levels in price order
   (bids descending, asks ascending) and, within a level, orders in arrival order.
   Per order: id, user, original quantity, filled, remaining, side, price, post-only
   flag.

   **Rank, not membership.** This is the assertion that catches a `Reduce` which
   re-queues and a level that pushes at the head. Membership alone is satisfied by
   both.

   > **Correction.** This sentence originally also credited the ranked comparison
   > with catching "an iceberg refill that lands in the wrong place". It cannot, and
   > the claim was checked by mutation rather than by argument. Icebergs are tier 2
   > (`commandTier`, `cmdIceberg`), so the generator never emits one and the ranked
   > comparison never sees a refill. Re-adding a refilled slice at the **head** of
   > its price level — built with only the book's existing exported API, so it does
   > not disturb the API-surface freeze — passed `go test ./...` across all 23
   > packages, with the hidden order silently taking queue priority it never queued
   > for. No other test in the repository covered it either, though `engine.go` states
   > the rule in a comment at the refill site.
   >
   > The property is a fairness rule with money attached, so it is now held by a
   > hand-written test, `TestIceberg_RefillGoesToTheBackOfTheQueue`, which fails on
   > that mutation. Modelling icebergs is still tier 2. This is recorded rather than
   > quietly edited because a sentence that makes an untested property read as
   > covered is [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §1 repeated
   > rather than applied — in the document written to apply it.

4. **The L2 aggregate, asymmetrically.** The engine's side comes from its
   *maintained* aggregate (`orderbook.Snapshot` / `GetBidLevels`, which read
   `PriceLevel.TotalQty` and `count`). The model's side is **summed from its L3**.

   The asymmetry is the entire value of this assertion and would be destroyed by
   "simplifying" it to compare two sums. The engine maintains `TotalQty`
   incrementally through `push`, `unlink`, `UpdateOrderQuantity` and
   `RestoreOrderQuantity`, using a per-node `contributed` value that is
   intentionally not the order's live `RemainingQty` (`pkg/orderbook/orderbook.go:70-88`).
   Comparing that against an independent sum is the only thing in this repository
   that would catch a level aggregate drifting from the orders in it **from an
   engine command tape** — the roadmap lists this as missing for exactly that
   reason, and today it is pinned only by hand-written churn inside
   `pkg/orderbook/alloc_test.go`.

5. **Order ids, exactly, including the shard field.** The model reproduces
   `orderSeq` and composes ids the same way (`ComposeID(shardIndex, seq)`,
   `pkg/matching/id.go`). At least one harness profile runs a **non-zero
   `ShardIndex`**, because a shard index of zero composes to the sequence itself and
   would let a broken composition pass.

   Ids could have been declared "engine-assigned, allowed to differ", and that was
   rejected. An order id is a customer-visible name that appears in execution
   reports, drop copies, cancels and busts. Allowing it to differ would also forfeit
   comparing the book *by id* at all, forcing a positional comparison — and a
   positional comparison is satisfied by an engine that hands two clients each
   other's ids.

6. **Trade ids and sequence numbers, exactly**, with the gaps (§3.4).

7. **The event stream of that command, as an ordered list**: kind, subject order id,
   user, and reason class. Sequence *numbers* are §3.3(b).

8. **Engine state, last trade price, and the two next-id counters.** The counters
   are in the observation rather than inferred, so a divergence in id allocation is
   caught at the command that causes it rather than at the next submit.

### 3.3 What is allowed to differ, and what still constrains it

Three things. Not four.

**(a) Wall-clock timestamps — `CreatedAt`, `UpdatedAt`, `Trade.CreatedAt`.** The
model has no clock. No timestamp appears in `Observation`.

*What still constrains it:* every engine code path that reads the clock **for a
decision** is behind a `Config` knob that §2.5's `mustBeZero` classification forces
off in the harness — `MinRestingTime` (`engine.go:2024`, `:1850`), `BandBreachPause`
and its auto-resume (`:872`), `SessionClose` for DAY, and GTD's deadline. With those
off, no tier-1 verdict, trade or book position depends on what the clock said, and
that is a checkable claim rather than a hope: it is what
`TestHarnessConfigMatchesItsClassification` asserts. Separately, the injected clock
is a deterministic step function, so the replay and snapshot comparisons of §3.5 —
which run through `Digest()`, itself a timestamp-normaliser
(`pkg/matching/snapshot.go:351-358`) — stay exact.

*What is genuinely lost:* if the engine stamped a wrong timestamp on an order, this
harness would not notice. Tier 2's DAY/GTD work is precisely the work of making
time observable, and that is where it gets an oracle.

**(b) `Event.Seq` values.** The model produces an ordered event list; it does not
assign sequence numbers.

*What still constrains it:* two properties that are asserted, and which together
determine the value uniquely. First, the ordered list of `(kind, subject, user,
reason)` is compared elementwise against the model's, so an event dropped, added or
reordered fails. Second, **`TestEventSequenceIsDenseAndMonotonic`** asserts the
engine's own stream over the whole tape starts at 1, increases by exactly one per
event, and never repeats. A list that matches and a numbering that is dense from 1
leave exactly one possible value per event. Making the model own the counter would
have added a second computation of a number both sides derive from the same list.

**(c) Internal representation and pointer identity.** Obviously. The model has
slices where the engine has a map, a pool and a price vector.

*What still constrains it:* everything that recovery must reproduce is in
`EngineSnapshot`, and §3.5 compares the restored engine's **visible book** per
command against the model, then drives a restored engine forward through the rest of
the tape.

> **Correction — this paragraph used to end with a false sentence.** It read:
> "Internal state that is not in the snapshot and not in the observation is, by
> definition, state no consumer and no restart can see." That is wrong, and the
> counter-example is the single most widely consumed number this venue publishes.
>
> `PriceLevel.TotalQty` is **not** in `EngineSnapshot` — the snapshot carries orders,
> and the level aggregate is rebuilt by `book.Add` on the way in. So the aggregate is
> in the observation for the *live* engine and absent from the snapshot, which put it
> in precisely the gap the sentence claimed was empty. Adding one line to
> `LoadSnapshot` that double-counts each restored order into its level makes every
> restored engine serve **double** the true L2 depth at every price:
> `go test ./... -count=1` exited **0 across all 23 packages**. Live best bid 7,
> restored best bid 14, `restored.Digest() == snap.Digest()` **true** — which is
> exactly why the old check could not see it. The original `restoreMatchesLive` was a
> snapshot → restore → snapshot digest round-trip, so it could only ever see state
> the snapshot itself carries.
>
> Both halves are fixed in §3.5. The lesson is the same one this document keeps
> re-learning: an equivalence argued from a definition ("by definition, state no
> consumer can see") is not an assertion, and the argument was wrong.

**And one thing that is deliberately not on this list.** `Trade.Symbol` is constant
across a tier-1 tape and `Trade.Auction` is always false (there is no uncross in
tier 1). Neither is excluded from the comparison — both are **asserted constant**.
A field excluded because it happens to be constant today is a field that silently
stops being checked on the day tier 2 makes it vary.

### 3.4 Three rules that look like bugs

Each of these is correct behaviour that a reader will report as a defect, and each
is a rule the model must implement deliberately.

**Order ids have gaps, and the gaps are correct.** `Match` calls `e.nextID(order)`
as its *first* statement (`pkg/matching/engine.go:672-674`), before any admission
check, so an order rejected for being outside the band, for post-only crossing, or
for arriving into a halted venue **still consumes a sequence number**. A model that
increments only on acceptance diverges permanently at the first rejection. This is
also the rule that makes mutation 10 in §7.1 catchable: moving `nextID` after the
checks is a plausible-looking tidy-up that changes every id a client will ever see.

**Trade ids have gaps too, and for a sharper reason.** `executeTrade` increments
`tradeSeq` and composes the trade id (`:1682`, `:1722`). A fill-or-kill order that
cannot fully fill then has every one of its trades reversed by `reverseTrade`
(`:1769-1797`) — which restores the makers' quantities and their place in the book
and **does not decrement `tradeSeq`**. So a rejected FOK burns trade ids that no
trade will ever carry. That is the right choice — a counter that goes backwards is
a counter that can name two different prints with one id — but it means the model
must burn them too, and it means an operator counting trade ids and finding a gap
has found a rejected FOK, not a lost print.

**Trade ids gap while event sequence numbers do not.** The same rejected FOK
produces *no* trade events at all: `settleInto`'s fill-or-kill branch removes the
events describing the prints it just reversed, and `stampAndPublish` numbers only
the composed batch (`:776-784`). One counter has holes, the other does not, in the
same command. Anyone reconciling the two streams needs that sentence.

What that no longer implies, since
[`DIFFERENTIAL-FINDINGS.md`](DIFFERENTIAL-FINDINGS.md) §4: **the rest of the batch
is not dropped.** A rejection drops only the events describing state the engine
actually undid, so a rejected command can publish `CANCELED`, `REPLACED`,
`ACCEPTED` and `TRIGGERED` events after its `REJECTED` — they describe what happened
to *other* orders, and none of it is being reversed. `emitResult` used to clear
`e.pending` wholesale, which is what made an STP-cancelled maker vanish silently.

**A rejected FOK leaves the last trade price where it found it.** This used to be a
fourth rule on this list, marked genuinely open, and §9(a) predicted it as a defect
before anything was run. It reproduced, and it is fixed:
[`DIFFERENTIAL-FINDINGS.md`](DIFFERENTIAL-FINDINGS.md) §3 settles that
`LastTradePrice` means the price of the last trade this venue **published**, so
`settleInto` captures the pre-match value and its fill-or-kill branch restores it
alongside `reverseTrade`. The model implements the same rule. The asymmetry with
`tradeSeq` two paragraphs up is the decision rather than an oversight: an id is a
**name** and a counter that goes backwards can give one name to two prints; a price
is a **datum** and restoring it makes it correct again.

### 3.5 Snapshot and replay equivalence

The model is not the oracle for these; the engine is its own oracle, and the
harness's contribution is the *tape*.

After every command of the differential tape, the harness asserts:

- **Snapshot-restore equals uninterrupted, in two parts.** Both were needed; the
  first shipped alone and was not enough.

  *(a) State, after every command.* `e.TakeSnapshot()`, restore into a fresh engine,
  and compare the restored engine's **visible book** — ranked L3, the L2 aggregate,
  the trading state, the last trade price and both id counters — against the model.
  Not just `Digest()` against `Digest()`: that is a snapshot → restore → snapshot
  round-trip, structurally blind to everything `LoadSnapshot` *rebuilds*, and the
  mutation in §3.3(c) walks straight through it with all 23 packages green. The level
  comparison is now **doubly** asymmetric: the live engine's aggregate is maintained
  incrementally, the restored engine's is rebuilt from scratch by a different code
  path, and the model's is summed independently from its L3.

  *(b) Behaviour, from a fork point.* State equality is necessary and not sufficient
  — a restored engine can look right and then *behave* wrong, because restoring
  rebuilds the structures the next command reads. So
  `TestSnapshotRestoreEqualsUninterruptedExecution` runs the tape to a fork, restores,
  and drives the **remainder** of the tape through the restored engine alone,
  comparing the whole observation after every command. Three forks per tape, 48
  subtests. Until this existed, nothing in the repository restored a snapshot and
  then kept trading against it, and the roadmap carried the row as ❌ while its own
  summary claimed it closed.

- **Replay equals live.** The same tape driven through a `Runner` with a WAL,
  replayed from every prefix, comparing the book digest and the trade tape. This is
  `TestCrashAtEveryBoundary`'s existing property (`pkg/wal/boundary_test.go`) — the
  change is not the assertion, it is the tape.

  > **Correction.** This bullet claimed the tape "now speaks the tier-1 alphabet
  > instead of 'limit orders, cancels and four phase transitions'". It did not.
  > `tape.Recovery` shipped with `Exotic: false`, so on the 400-command sweep all 287
  > submits were plain GTC limits: **zero** market orders, IOC, FOK, post-only,
  > per-order STP, trade groups or privileged orders. The profile was a superset of
  > `Differential` on the command-*kind* axis and a strict subset on the order-*payload*
  > axis, and its own comment asserted the opposite.
  >
  > So the replay oracle never crossed a crash boundary carrying a rejected FOK's
  > reversed prints or an STP-cancelled maker — the two paths §9(a) and §9(b) named
  > **in advance** as defect-bearing, and the two this slice then confirmed as live
  > defects. Every assertion in the file passed throughout, because they count prints,
  > auction prints and depth, none of which move if every submit is a plain limit.
  >
  > Fixed rather than reworded: `Exotic` is on, `walOrder` carries the whole tier-1
  > payload onto the journal, and `TestRecoveryTapeSpeaksTheTierOneAlphabet` asserts
  > the payload axis **by outcome** (a rejection reason a consumer was actually told)
  > wherever an outcome is observable, and by draw only where it is not. The one
  > tier-1 rejection the tape still does not reach, `ErrMarketOrderNoLiquidity`, is
  > enumerated there with its reason rather than dropped, and market orders are
  > asserted through the outcome they *do* reach.
  >
  > Widening the alphabet cost depth, which is a real trade and is recorded as
  > numbers: at the differential draw rate the sweep fell from 117 continuous prints
  > to 52 and from 45 peak resting depth to 25, because market orders, IOC and FOK
  > never rest and the three cancelling STP modes remove liquidity that already has.
  > `Profile.ExoticDamp` divides the exotic draw *rate* — a density knob, never an
  > on/off one, because an on/off one is what caused this — and at damp 4 the sweep
  > holds 110 prints, 17 auction prints and peak depth 32, clearing every existing
  > floor with no number argued down, while still drawing every exotic path.

Two practical constraints on that second bullet. The sweep is O(n²) and its
runtime is already recorded (~0.90 s for 400 commands after the phase extension,
[`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §8.5), so the wider alphabet
goes in at the same command count and the runtime is re-measured (§7.3) rather
than assumed. And the differential harness itself drives `Engine` directly, not
`Runner`: the queue's ordering semantics are tested elsewhere, and a goroutine
between the tape and the book buys nothing but flakiness.

## 4. The tape

### 4.1 One generator, one alphabet

`pkg/wal/runner_recovery_test.go:70` already has a tape generator, and this slice
**supersedes it rather than forking it**. `buildTape`, `tapeCmd`, `tapePhases`,
`lcg` and `applyTapeCmd` move to `internal/tape`; `pkg/wal` becomes a consumer.

That is a deliberate refactor of a test file that four other tests depend on, and
the reason is the one this repository has already paid for once. Two generators
means two alphabets, means one of them is extended and the other is not, means the
sweep that claims to be the stronger test is exercising less than the one it is
supposed to dominate. `applyTapeCmd`'s own comment already says this
(`pkg/wal/runner_recovery_test.go:97-100`); this makes it structural.

The generator is `tape.Gen(profile, seed, n) []tape.Cmd`. A **profile** names which
kinds may be drawn and with what weights:

| Profile | Alphabet | Consumer |
|---|---|---|
| `Differential` | tier-1 kinds only (§2.3) | `pkg/matching` differential harness |
| `DifferentialProRata` | the same, engine configured pro-rata | the second allocation configuration |
| `Recovery` | every journalled kind the drivers can issue, including phases | `pkg/wal` boundary sweeps |

`Recovery` is deliberately a **superset** of `Differential`: the replay oracle can
check kinds the model does not model, and refusing to sweep them because the model
is behind would be the exact trade this document exists to argue against.

### 4.2 Keeping the alphabet complete as the engine grows

`pkg/matching/command_alphabet_test.go`'s classification already forces every
`cmdKind` to be declared journalled or read-only. It gains a **second axis**: every
kind is additionally `modelled`, `deferredToTier(n, reason)`, or `notACommand`.

Three guards follow:

1. **`TestEveryCommandKindHasATier`** — enumerates `0 … cmdKindCount-1` and fails
   on any kind without a tier. Adding `cmdSomethingNew` is a test failure at the
   moment the constant is written, at the one point where the author still knows
   whether the model needs to learn it.
2. **`TestEveryModelledCommandIsGenerated`** — every kind classified `modelled` has
   a `tape.Kind`, and a reference draw at the harness's default seed and length
   produces **at least one** of it. A generator weight of zero is the config-free
   form of an incomplete alphabet, and it reports as coverage exactly the way
   `buildTape` reported completeness while never emitting a phase transition.
3. **`TestRecoveryProfileCoversEveryJournalledKind`** — reports, as a failing test
   with a written exemption list, which journalled `EntryKind`s never appear at any
   crash boundary. [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §8.7 records
   that thirteen of them do not; this turns that paragraph into a number that has to
   be argued down rather than a sentence that ages.

Guard 3 will fail on the day it is written. That is intended: it starts life with
an explicit exemption list naming all thirteen, and every tier-2 slice deletes
entries from it. A list that must be edited is a list someone reads.

### 4.3 Deletion-closed tapes, which is what makes shrinking possible

A shrinker that deletes commands only works if **every subsequence of a valid tape
is a valid tape**. That is a property of the tape *format*, decided here, not a
problem for the shrinker to solve later.

The failure mode it avoids: `buildTape` today names a cancel's target as
`cancelIx`, an index into the list of ids the run has *accumulated so far*
(`pkg/wal/runner_recovery_test.go:83-86`). Delete an earlier submit and every later
cancel silently retargets — the reduced tape is a *different* tape, so the shrinker
loses the failure and reports that the bug went away.

The rule instead: **a command that names an earlier order names it by the tape
position of the submit that created it.** Deleting that submit leaves the cancel
naming a position that produced no order, and the driver issues it as a cancel of a
known-absent id — a well-formed command with a predictable rejection that *both*
sides model. So deletion never rewrites the meaning of what remains, and the
"cancel something that isn't there" case gets swept for free rather than needing
its own generator branch.

The same rule applies to reduce, replace and cancel-all. Cancel-all names a user
string, which is drawn from a fixed small pool, so it is deletion-stable already.

### 4.4 The shrinker

Delta debugging by deletion, in `internal/tape`:

1. Run the tape. Record the **divergence class** of the first mismatch — an enum
   (`trade-count`, `trade-price`, `trade-order`, `book-rank`, `book-quantity`,
   `level-aggregate`, `order-id`, `trade-id`, `verdict`, `event-kind`,
   `event-order`, `state`), taken from which field of the `Observation` first
   differs.
2. Repeatedly try removing a contiguous chunk, starting at half the tape and
   halving down to one command. Keep a removal if the reduced tape still diverges
   **with the same class**.
3. Stop when no single-command removal helps, or when the budget is spent.

The predicate is "same class", not "still fails" and not "fails identically at the
same index". "Still fails" lets the shrinker wander onto a different bug and report
a minimal tape for something that was not the failure; "identically at the same
index" refuses almost every reduction, because deleting a command shifts every
later index. If the class ever changes during a shrink, the failure message says so
and prints both tapes — a shrink that changed the bug is information, not an error.

The budget is a command count and an iteration cap, not a wall clock: a
time-budgeted shrink produces a different minimal tape on a loaded machine, and a
failure message that varies with machine load is a failure message people learn to
distrust.

**The output is the deliverable.** The message prints the shrunk tape as a
compilable `[]tape.Cmd{...}` Go literal, ready to paste into a regression test
verbatim, alongside the seed, the profile, the divergence class, the command index,
and a field-by-field diff of the two observations. A differential failure that
prints two thousand commands is a failure nobody will debug; a differential failure
that prints nine commands and the line to paste them into is one somebody fixes
before lunch.

### 4.5 Determinism and reproduction

- **The generator is a pure function of `(profile, seed, n)`.** It uses the existing
  `lcg` (`pkg/wal/runner_recovery_test.go:31-41`), moved with everything else, and
  never `math/rand` — the reason is already written in that file's comment and holds
  here: `math/rand`'s stream is not guaranteed stable across Go releases, and a
  harness whose input silently changes under a toolchain upgrade is a harness whose
  green run means nothing.
- **The LCG stream is pinned by a golden vector**, not the tape. Pinning generated
  tapes would make every intentional alphabet extension a golden-file churn, which
  trains people to regenerate goldens without reading them. Pinning the primitive
  catches an accidental change to the generator's arithmetic and lets the alphabet
  grow freely.
- **The engine side is deterministic by construction**: an injected step clock, a
  fixed `ShardIndex` per profile, no `Runner`, no goroutines, no maps iterated for
  output.
- **`TestSameSeedSameObservations`** runs one tape twice in one process and
  requires identical observation streams. It is the guard that catches a map
  iteration or a live clock leaking into either side, and it is cheap enough to run
  always.
- **Every failure message names its profile and seed**, and every tape in the
  committed sweep is addressable as a subtest:
  `go test ./pkg/matching -run 'TestDifferentialTape/fifo/seed=3'`. For
  `FuzzDifferential`, whose input is the fuzzer's bytes rather than a seed, the
  message prints the **decoded tape literal** — a corpus file is not a reproduction
  anyone can read.

  > **Correction.** This bullet used to promise `-tape.seed` / `-tape.n` /
  > `-tape.profile` flags. No such flag is registered anywhere in the repository, and
  > following the instruction gets you `flag provided but not defined`. The subtest
  > name and the printed literal are the reproduction path, and they are better than
  > flags would have been: the literal reproduces the *shrunk* tape, which is the one
  > worth reading. Recorded here rather than silently deleted, because §10.5 exists to
  > list the places this document was wrong about its own design and this one was
  > nearly dropped instead of written down.

## 5. How this composes with the invariant fuzzers

They keep their jobs and this does not overlap them.

- `FuzzEngine` and `FuzzExoticOrders` take **arbitrary bytes** and check that
  nothing catastrophic happens. They reach inputs the model has no opinion about —
  malformed constructions, exotic types outside tier 1, quantities the tape
  generator would never draw. Cheap, wide, no oracle. They are not modified and
  not narrowed.
- The differential harness takes a **structured tape** over a declared alphabet and
  checks agreement with a model. Narrow, deep, an oracle.

One concrete piece of cooperation rather than duplication: **the differential
harness calls `checkInvariants` on the engine side too**, before comparing. An
invariant violation is a better failure message than a divergence — "the book
crossed after command 412" is diagnosable, "resting order 7's rank is 3 and should
be 2" from the same underlying cause is not. Invariants first, then equivalence.

**And the harness gets the CI job the milestone says is missing.** Today CI is
`go test -race ./...` (`.github/workflows/ci.yml:31`), which runs seed corpora and
nothing more. This slice adds `FuzzDifferential` with a committed seed corpus under
`pkg/matching/testdata/fuzz/FuzzDifferential/`, and a bounded `-fuzz` run in the
existing nightly soak workflow. The committed in-CI run stays a fixed small sweep of
seeds — a test people will actually run rather than a nightly job they will not.

> **Two corrections to this paragraph, both about the nightly campaign being weaker
> than described.**
>
> *It ran a weaker oracle.* `FuzzDifferential` called `runDiff` with `full=false`, so
> the 30-minute campaign ran the observation comparison **only** — no
> `checkInvariants`, no snapshot-restore comparison, no duplicate-order-id check and
> no trade-quantity-balance check. The last two are the checks two recorded mutations
> are credited to *by name*. Demonstrated: dropping `LastTradePrice` in
> `LoadSnapshot` failed all 16 subtests of the committed sweep and left a full corpus
> run green. The unbounded half was strictly weaker than the fixed one, which is the
> opposite of what this section led a reader to expect. It now runs `full=true`; the
> shrinker still runs with it off, because it only needs the divergence class.
>
> *The corpus does not hold what was claimed.* "Every failure the campaign finds is
> committed to the corpus in its shrunk form" was false. Go writes the fuzz **input**
> — a `(uint64 seed, uint16 n)` pair — and the shrunk `[]tape.Cmd` literal lives only
> in the printed message. The resulting regression test replays the full tape, not
> the two-to-four-command reproduction. The workflow comment now says so, and says to
> copy the literal out of the log before it rotates.

## 6. What this deliberately does not do

- **It does not become a second production engine.** `internal/refmatch` is
  test-only, outside the compatibility promise
  ([`COMPATIBILITY.md`](COMPATIBILITY.md)), and no `cmd/` or `pkg/` binary imports
  it. Nothing in it is a candidate for "we could just use the simple one".
- **It does not get optimised, ever.** See §2.1. If the harness is slow, the tapes
  get shorter.
- **It does not model time** (tier 1), the auction, the exotics, or any admission
  control. §2.4 and §2.5 are how those absences stay visible.
- **It does not replace hand-written scenarios.** Agreement is not correctness
  (§1.3). `TestEventStreamReconstructsBook`'s twenty-five scenarios stay, and every
  bug the campaign finds becomes one more.
- **It does not fix `TestSoak`'s sampling.** `TestSoak` checks invariants every
  50,000 ops of 500,000 (`pkg/matching/fuzz_test.go:178`), so a violation that
  self-heals inside a window is invisible — a real gap, correctly listed under M9,
  and a different budget: a 500,000-op run with a per-op model comparison is a
  nightly job, and M13's soak is where a nightly job belongs. Named here so it is
  not mistaken for closed.
- **It does not shrink values, only commands.** A shrinker that also reduces prices
  and quantities produces smaller reproductions and is a second design. Deletion
  first; if the shrunk tapes are still hard to read, that is the evidence for doing
  the second one.
- **It does not compare against any other venue's engine**, published test vector,
  or third-party implementation. That would be a stronger oracle and it is
  M10's cross-language track, not this.
- **It does not assert performance.** The model is slower than the engine by
  design and by orders of magnitude; no number from this harness belongs in
  [`BENCHMARKS.md`](BENCHMARKS.md).

## 7. Deliverables and acceptance criteria

| # | Deliverable | Done when |
|---|---|---|
| 1 | `internal/refmatch` — the tier-1 model | `TestReferenceMatcherImportsNothing` passes; the package builds with no import outside stdlib and `internal/tape` |
| 2 | `internal/tape` — generator, profiles, shrinker | `buildTape`/`tapeCmd`/`tapePhases`/`lcg` deleted from `pkg/wal`; the four existing recovery/boundary tests pass unchanged in assertion, now driven from `tape.Gen` |
| 3 | The adapter and `Observation` comparison | `pkg/matching/differential_test.go` drives a tape through both sides and compares whole `Observation` values; `TestEveryTierOneRejectionIsMapped` passes |
| 4 | `TestDifferentialTape` | A fixed sweep of seeds × two profiles (FIFO and pro-rata, one with non-zero `ShardIndex`) passes, and completes in under 5 s |
| 5 | The command-alphabet tier guard | The three tests of §4.2 pass; `TestEveryCommandKindHasATier` fails when a `cmdKind` is added without one |
| 6 | The config guard | The three tests of §2.5 pass; `TestEveryConfigFieldIsClassified` fails when a `Config` field is added without one |
| 7 | Per-command snapshot and replay equivalence | The **restored engine's visible book** compared against the model after every command of a generated tape (a digest-after-restore comparison is not enough — see §3.3(c)); a restored engine driven forward through the rest of the tape; and the boundary sweeps running the tier-1 alphabet **payload**, asserted by outcome |
| 8 | The shrinker | Every mutation in §7.1 shrinks to **≤ 12 commands** within budget, and each shrunk tape is recorded in §10 |
| 9 | `FuzzDifferential` + corpus + nightly `-fuzz` | The fuzz target exists, a seed corpus is committed, and the nightly workflow runs it with a time bound |
| 10 | The four missing per-command assertions | "no duplicate order id", "aggregate depth equals resting orders", "trade quantities balance", and "snapshot restore equals uninterrupted, per command" are asserted **on the generated path**, closing M9's four `❌` rows. The fourth took two assertions and two attempts; the first attempt satisfied the words and not the property, and §10.5 records why |

### 7.1 The mutations it must catch

**This is the criterion that decides whether the harness is worth anything.** Per
[`TESTING.md`](TESTING.md), none of the deliverables above counts until the harness
has been run against each engine mutation below and **failed**. Each names the file
and the edit, so the runs are reproducible and so a reviewer can check the list is
not padded with mutations any test would catch.

| # | Mutation | Primary witness |
|---|---|---|
| 1 | `PriceLevel.push` inserts at the head instead of the tail (`orderbook.go:55-68`) — LIFO within a level | book rank; trade maker order |
| 2 | Limit-buy crossing test `taker.Price < maker.Price` → `<=` (`engine.go:1289`) | trade count |
| 3 | `executeTrade(taker, maker, taker.Price, …)` — print at the taker's price (`engine.go:1344`) | trade price |
| 4 | `qty := min(taker.RemainingQty, maker.RemainingQty)` → `maker.RemainingQty` (`engine.go:1343`) | trade quantity; negative remaining |
| 5 | Drop `e.book.UpdateOrderQuantity(maker.ID, qty)` on a partial fill (`engine.go:1367`) | ~~**level aggregate only** — nothing else sees it~~ → the whole-book `checkInvariants` (`level SELL 98 publishes 9 lots and the 1 orders resting there hold 4`), which the harness runs before it compares anything. See the note under this table |
| 6 | `Reduce` removes and re-adds instead of shrinking in place (`engine.go:1821`) | **book rank only** |
| 7 | `takerSTP` ignores the per-order `STPMode` and always returns the engine default (`engine.go:1389-1397`) | trades; events |
| 8 | Drop `e.book.UpdateOrderQuantity(maker.ID, d)` in `decrement` (`engine.go:1405`) | level aggregate |
| 9 | `reverseTrade` does not restore the maker's quantity (`engine.go:1778-1779`, `:1794`) | ~~book quantity~~ → the whole-book `checkInvariants` (`a resting order (41): rests with 0 remaining`), for the same reason as 5 |
| 10 | Move `e.nextID(order)` below the admission checks (`engine.go:674`) | order id |
| 11 | `reverseTrade` decrements `tradeSeq` (id reuse after a rejected FOK) | trade id |
| 12 | `OrderBook.Remove` deletes from the index but not the level list (`orderbook.go:235`) | book membership; level aggregate |
| 13 | `wouldCross` uses `>` instead of `>=` for a buy (`engine.go:1553`) — post-only accepted when it exactly touches | verdict; book |
| 14 | A market order's remainder rests instead of cancelling (`engine.go:950-960`) | book |
| 15 | `StateCancelOnly` accepts a new order (`engine.go:902-904`) | verdict |
| 16 | `proRataAllocate` distributes the remainder in reverse arrival order (`engine.go:1653-1666`) | trade order and quantity |
| 17 | `Replace` preserves queue position instead of forfeiting it (`engine.go:1891`) | book rank |
| 18 | `emitCancel(maker)` dropped for an STP-cancelled maker (`engine.go:1312`, `:1318`, `:1326`) | event stream |

Mutations 5, 6 and 18 are the ones that justify the three most expensive
assertions in §3.2 — the asymmetric level comparison, ranked rather than set book
comparison, and the ordered event list. If any of those three mutations is caught
by something cheaper, the corresponding assertion is over-engineered and should be
argued down.

**That rule has fired for mutation 5, and the answer is "not yet".** The strengthened
whole-book `checkInvariants` ([`PINNED-DEFECTS.md`](PINNED-DEFECTS.md) §5) runs inside
`TestDifferentialTape` after every command — deliberately, because "the book crossed
after command 412" is a better failure message than a rank divergence with the same
cause — and it now catches mutations 5 and 9 before any comparison against the model
runs. Both are still caught, so nothing is broken; the *evidence* moved.

The assertion is not removed here, and the reason is not sentiment: the two checks
compare different things. The invariant compares the engine's level aggregate against
the engine's **own** order list; the differential assertion compares it against an
**independent implementation**. What the rule establishes is that mutation 5 no longer
*justifies* the assertion — not that the assertion is unjustified. So this table loses
mutation 5 as the level comparison's evidence and gains an open item: **find a mutation
the asymmetric level comparison catches and the whole-book invariant does not, or argue
the assertion down in the slice that fails to.** A search when this was recorded did not
find one. The obvious candidate — a level dropped from the sorted price vector while its
order still rests, which `OrderBook.Orders()` cannot see and the invariant is therefore
blind to — turns out to be caught by `book-membership` and by the event mirror, not by
the level comparison.

### 7.2 The mutations it must NOT flag

A harness that fails on a legitimate change is worse than no harness, because it
gets disabled. M11 is *about* replacing the book's data structures — "choosing
between a radix tree and a dense grid with only hand-written scenarios as an oracle
is how a wrong book ships" ([`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md)) —
so this harness is the thing that has to stay green across exactly those changes.

Three no-op mutations must leave the whole suite green:

1. Change the pooling and capacity hints in `orderbook.Config` (`orderbook.go:152-159`).
2. Replace a price level's doubly-linked list with a slice-backed queue, preserving
   FIFO semantics.
3. Replace the sorted price vector with a different ordered container, preserving
   the price ordering.

If any of those turns the harness red, the harness is asserting an implementation
detail and the offending assertion is wrong, not the change.

### 7.3 The numbers

Recorded when the work is done; none of these is a pass criterion on its own.

- Runtime of `TestDifferentialTape` in the committed sweep (target: under 5 s, so it
  runs in `go test ./...` without anyone thinking about it).
- Runtime of `TestCrashAtEveryBoundary` **before and after** the alphabet widens.
  It was ~0.34 s, then ~0.90 s after phase transitions were added, and it is O(n²).
  If widening the alphabet pushes it past a few seconds, §7 of
  [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) already gave the answer: a
  second dedicated tape, never a shorter main one.
- Shrunk tape length per mutation (target: ≤ 12 commands; §7.1 has eighteen data
  points).
- How many of the eighteen mutations a **single** seed catches, versus the full
  sweep. This is the number that says whether the sweep is load-bearing or
  decorative — see sabotage 8.
- Lines of code in `internal/refmatch`. If it passes a few hundred, "obviously
  correct by inspection" has stopped being true and the tier boundary is in the
  wrong place.

## 8. Sabotage runs required before this counts as done

§7.1 breaks the **engine** and requires the harness to fail. This section breaks the
**harness** and requires its own guards to fail. Both are necessary: a harness whose
guards are decoration is a harness that will be silently narrowed within two
slices, which is the entire subject of
[`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md).

| # | Sabotage | Must fail |
|---|---|---|
| 1 | Make `internal/refmatch` import `pkg/types` and use `types.Order.Fill` | `TestReferenceMatcherImportsNothing` |
| 2 | Delete the L2-aggregate comparison from `Observation` | Mutation 5 must stop being caught — run it, confirm the harness goes green against a broken engine, restore |
| 3 | Compare the resting book as a *set* instead of a ranked list | Mutations 1, 6 and 17 must stop being caught |
| 4 | Set the generator weight for `reduce` to zero | `TestEveryModelledCommandIsGenerated` |
| 5 | Add a `cmdKind` before the sentinel and give it no tier | `TestEveryCommandKindHasATier` |
| 6 | Add a `Config` field and classify it nowhere | `TestEveryConfigFieldIsClassified` |
| 7 | Set `PriceBand` to a non-zero value in the harness config | `TestHarnessConfigMatchesItsClassification` |
| 8 | Run the eighteen mutations of §7.1 against **one seed** instead of the sweep | Record how many are still caught. This row does not require a failure; it requires a **number**, and that number is the argument for the sweep's size |
| 9 | Replace the injected step clock with `time.Now` | `TestSameSeedSameObservations` |
| 10 | Weaken the shrink predicate from "same divergence class" to "still diverges" | Re-run mutations 5 and 18 and record whether the shrinker drifts onto a different failure. If it does not, the class enum is over-engineered and should be simplified |
| 11 | Map an unmapped rejection reason to a catch-all "other" enum | `TestEveryTierOneRejectionIsMapped` |
| 12 | Keep the whole slice but revert the `pkg/wal` alphabet widening (§4.1) | Nothing fails — **and that is the point**, exactly as row 8 of `JOURNAL-COMPLETENESS.md` §6 intended. It demonstrates the boundary sweeps stay green over a narrowed alphabet, which is why the alphabet is now a shared artefact with a guard on it rather than a local `buildTape` |

Rows 2, 3, 8 and 12 are the unusual ones and they are the important ones. Every
other row asks a test to fail. Those four ask the suite to *pass* against something
broken, to establish that a specific assertion is what is doing the work rather than
being carried by a neighbour.

## 9. How this can fail, stated in advance

So that §10 is not graded on a curve.

- **The first thing this finds may be a defect in the engine, not a mismatch in the
  model, and the honest response is to say so.** Two specific predictions, both
  derived by reading the code rather than by running anything:

  **(a) A rejected FOK appears to move the last trade price.** `match` calls
  `recordLast` on return, setting `LastTradePrice` from the final print
  (`engine.go:1670-1675`); `settleInto`'s FOK branch then reverses every one of
  those trades and returns `dst[:start]` (`:972-982`). Nothing puts the last trade
  price back. Since `LastTradePrice` is the fallback reference for the price collar
  when no mark is set (`outsideBand`, `:1411-1431`), a rejected order can move the
  band. The model has to take a position; if the model's position is "a rejected
  order changes nothing" and the engine disagrees, the finding is the engine's.

  **Reproduced, then fixed 2026-08-17** — and the model's position was the wrong
  one too, which is §1.3 measured rather than warned about. See
  [`DIFFERENTIAL-FINDINGS.md`](DIFFERENTIAL-FINDINGS.md) §3 for the decision and
  §10.1(a) below for what it cost.

  **(b) An STP-cancelled maker can vanish from the book with no event announcing
  it.** `emitCancel(maker)` appends to `e.pending` (`:798-804`), and `emitResult`
  clears `e.pending` when the status is `Rejected` (`:752`). A fill-or-kill taker
  under `STPCancelOldest` or `STPCancelBoth` cancels a maker mid-walk, fails to
  fill, and is rejected — and `reverseTrade` restores only makers it *traded* with,
  not ones STP removed. So the book loses an order and the event stream never says
  so, and a consumer built on the stream keeps a phantom resting order forever. Both
  halves are covered individually today (FOK both ways, all five STP modes) and
  their **combination** is exactly what a generated tape reaches and a hand-written
  scenario list does not. If this reproduces, it is a live correctness defect and it
  gets its own fix, not a model workaround.

  **Reproduced, then fixed 2026-08-17**, and it got its own fix rather than a model
  workaround: the cancellation stands and the event survives the rejection. See
  [`DIFFERENTIAL-FINDINGS.md`](DIFFERENTIAL-FINDINGS.md) §4. The prediction
  understated it — the same dropped batch also swallowed an STP `DECREMENT`'s
  `Canceled` and an OCO stop leg's, and the reconstruction claim on `EventKind` had
  to be narrowed and re-proved on the generated path.

- **Tier 1 may turn out not to be a clean cut.** The tier boundary assumes exotics
  compose with the matching core without changing it. `cascadeStops` runs *inside*
  `Match` (`engine.go:699`) and its trades land in the same buffer, so a tape that
  never creates a stop never reaches it — fine — but if any tier-1 path can produce
  a trigger, the boundary is wrong and either the tape must exclude more or the
  model must learn stops sooner than planned.

- **The `Observation` may be expensive enough to change what is tested.** A full L3
  book copy after every command is O(book) per command and O(n·book) per tape, which
  is the point, and it is also the thing that tempts someone to sample every k
  commands. Sampling is how `TestSoak` ended up checking invariants every 50,000
  ops. If the harness is too slow, shorten the tape and keep every command checked;
  never keep the tape and check every tenth command.

- **`reflect.DeepEqual` over the whole `Observation` may prove too strict in an
  unhelpful way** — for example if a slice is `nil` on one side and empty on the
  other. That is a real nuisance and the fix is normalisation in the *constructor*
  of the observation, applied identically to both sides and stated in one place, not
  a comparator with exceptions in it. The moment the comparator grows a special
  case, §3.3's list of three permitted differences has silently become four.

- **The `pkg/wal` refactor may be more disruptive than budgeted.** Four tests depend
  on `buildTape` and two of them sweep every boundary of their tape. Moving the
  generator changes the *tape*, so the boundary sweeps sweep different commands, and
  a test that was green may go red for a reason that has nothing to do with this
  slice. That would be good news found awkwardly, and it must be reported as a
  finding rather than fixed by pinning the old tape.

- **The model may quietly become a transcription.** §2.3's pro-rata paragraph is the
  visible instance; there will be others, wherever the engine's behaviour is a
  choice rather than a consequence. The countermeasure is that any such rule must be
  written into this document as prose *before* the model implements it. A rule that
  appears only in the model and only in the engine has no oracle at all — it has two
  copies.

## 10. What building it found

Written after the code, as §9 said it would be.

### 10.1 Three engine defects, two of them predicted

> **All three fixed 2026-08-17.** Each was pinned here rather than repaired, because
> each had more than one defensible answer;
> [`DIFFERENTIAL-FINDINGS.md`](DIFFERENTIAL-FINDINGS.md) is the document that picked
> between them and §11 of that document records what the fix measured. Each fix was
> three-sided — engine, model, and the pinning test inverted — and (c) had a fourth
> side, §2.3's prose. The three pinning tests keep their names and now assert the
> opposite of them, which is the pin being redeemed rather than a test flipped to
> make a change pass.

**(a) A rejected fill-or-kill moves the last trade price.** §9 predicted it from
reading `recordLast` (`engine.go:1670-1675`) against `settleInto`'s FOK branch
(`:972-982`), and it reproduces in three commands. A maker rests 3 lots at 100; an
FOK buy for 5 arrives; it prints 3, cannot fill, has that print reversed, returns
**REJECTED with zero trades and the maker fully restored** — and `LastTradePrice` is
100. Since `LastTradePrice` is the collar's reference when no mark is set
(`outsideBand`, `:1411-1431`), an order that never traded can move the band that
admits the next one. Pinned by
`TestRejectedFOKStillMovesTheLastTradePrice`.

**(b) A self-trade-prevented maker vanishes with no event.** Also predicted, and the
more serious of the two. An FOK taker under `STPCancelOldest` removes a resting maker
mid-walk, fails to fill, and is rejected; `emitResult` clears `e.pending` on a
rejection (`:752`) so the maker's `EventCanceled` never reaches a consumer, and
`reverseTrade` restores only makers it *traded* with. Two commands. **The book goes
from one resting order to zero and the only event published is `REJECTED`.** A
consumer rebuilding L3 from the stream keeps a phantom order forever, which
contradicts the reconstruction claim in `EventKind`'s doc comment —
`TestEventStreamReconstructsBook` proves that claim for every scenario it contains,
and this is the combination it does not contain (FOK *and* STP in one order), which
is exactly what a generated tape reaches. Pinned by
`TestSTPCancelledMakerVanishesWithNoEvent`.

**(c) Pro-rata leaves the book crossed. Not predicted.** The harness found it on its
first pro-rata seed, and it is a two-command reproduction: one account rests a sell at
99 and then buys at 100. Under price-time priority self-trade prevention cancels the
taker; under pro-rata a taker's own liquidity is **skipped** rather than
STP-cancelled (`matchProRata`, `:1589-1601`), the walk ends with nothing eligible at
the touch, and the remainder **rests across the spread**. Every L1 and L2 consumer
then sees a negative spread on a continuous book. Worse, the crossed state persists:
on the pro-rata sweep at seed 25 the skipped order left before anything cleared the
crossing, after which the touch was a crossing between two **unrelated** accounts —
which reads as an outright matching failure rather than an STP artefact. Pinned by
`TestProRataSelfSkipCrossesTheBook`.

None was fixed here. Each is a matching-semantics or event-stream change with
consequences past the matcher, and this repository writes the spec before the code.
Each test carried the sentence a fix had to come and change, and
[`DIFFERENTIAL-FINDINGS.md`](DIFFERENTIAL-FINDINGS.md) is that spec. What the fix
slice then measured, beyond what is written above:

- **(a) also fires stops.** With a stop resting at trigger 100 and nothing ever
  traded, the rejected fill-or-kill's phantom price fired it and really printed 2
  lots at 100 between two accounts that had not sent the rejected order. Neither was
  told, because (b) dropped the batch. The two defects had to ship together.
- **(b) is four defects.** The pinned `CANCEL_OLDEST` instance, plus `CANCEL_BOTH`,
  plus `DECREMENT` (which also mutates the *taker* of an order that ends REJECTED),
  plus an OCO stop leg destroyed by a stranger's rejected order.
- **(c) overrides the venue's STP configuration.** All five modes left bid 100 /
  ask 99, because `matchProRata` never called `takerSTP` at all — so `ALLOW` did not
  get the self-trade it asked for and `CANCEL_BOTH` cancelled neither order. On the
  committed sweep the pro-rata profile was in a crossed state after **107** of its
  700 commands; it is now 0.

Two consequences for this document. §1.3's warning is exactly what (a) and (b) are:
the model was written to reproduce the engine's position on both, so the differential
comparison is silent about them **by construction**, and only the hand-written
statement of the rule is an oracle for it. And (c) is the case for the harness in one
line — it was found by `checkInvariants` running on the engine side of a generated
tape, which is the cooperation §5 argued for and not the model comparison at all.

### 10.2 §7.1: every mutation, and what caught it

Eighteen mutations from §7.1, plus three added while building (19-21) for the
assertions §7's deliverable 10 asks for. **All twenty-one are caught by the committed
sweep.** "Model only" is the same sweep with the invariant and snapshot-restore checks
turned off, which isolates what the `Observation` comparison alone does.

| # | Mutation | Sweep | Model only | First class | Shrunk |
|---|---|---|---|---|---|
| 1 | `push` inserts at the head (LIFO in a level) | caught | caught | `book-rank` | 2 |
| 2 | limit-buy crossing test `<` → `<=` | caught | caught | `verdict` | 2 |
| 3 | print at the taker's price | caught | caught | `trade-price` | 2 |
| 4 | fill quantity is the maker's, not the minimum | caught | caught | `verdict` | 2 |
| 5 | level aggregate not decremented on a partial fill | caught | caught | `level-aggregate` | 2 |
| 6 | `Reduce` removes and re-adds | caught | caught | `book-rank` | 3 |
| 7 | `takerSTP` ignores the per-order mode | caught | caught | `verdict` | 2 |
| 8 | `decrement` does not shrink the aggregate | caught | caught | `level-aggregate` | 2 |
| 9 | `reverseTrade` does not restore the maker | caught | caught | `book-quantity` | 2 |
| 10 | `nextID` moved below the admission checks | caught | caught | `order-id` | 2 |
| 11 | `reverseTrade` rewinds `tradeSeq` | caught | caught | `trade-id` | 2 |
| 12 | `Remove` skips the level list | caught | caught | *(livelock; see below)* | — |
| 13 | `wouldCross` uses `>` for a buy | caught | caught | `verdict` | 2 |
| 14 | a market remainder rests | caught | caught | `verdict` | 1 |
| 15 | cancel-only accepts a new order | caught | caught | `verdict` | 2 |
| 16 | pro-rata remainder in reverse arrival order | caught | caught | `trade-count` | 4 |
| 17 | `Replace` resizes in place, keeping priority | caught | caught | `verdict` | 2 |
| 18 | `emitCancel` dropped for an STP-cancelled maker | caught | caught | `event-count` | 2 |
| 19 | `nextID` reuses a sequence number | caught | caught | *(duplicate order id)* | — |
| 20 | `executeTrade` over-fills the maker | caught | caught | `book-quantity` | 2 |
| 21 | `executeTrade` publishes the print twice | caught | caught | *(quantities balance)* | — |

Every shrunk tape is **1 to 4 commands**, against §7's target of ≤ 12, and each is
printed as a pasteable `[]tape.Cmd` literal. Two examples verbatim:

```go
// mutation 5, the level aggregate — nothing but the L2 comparison sees this
[]tape.Cmd{
	{Pos: 91, Kind: tape.Submit, User: "u3", Sell: true, Price: 98, Qty: 7, STP: 1},
	{Pos: 103, Kind: tape.Submit, User: "u1", Price: 98, Qty: 3, STP: 3},
}

// mutation 6, a reduce that re-queues — nothing but the RANKED book comparison sees this
[]tape.Cmd{
	{Pos: 87, Kind: tape.Submit, User: "u1", Price: 99, Qty: 6, STP: 1},
	{Pos: 114, Kind: tape.Submit, User: "u3", Price: 99, Qty: 5, TradeGroup: 1},
	{Pos: 118, Kind: tape.Reduce, User: "u1", Target: 87, NewQty: 4},
}
```

Three entries need their footnote rather than their row.

**Mutation 12 is caught as a livelock, not a divergence.** Leaving a removed order in
its level list makes the match loop peek the same zero-quantity maker forever, so the
run is killed by the test timeout at 60 s with no message worth reading. The harness
does detect it, and any test that ran the engine would. Naming it honestly: this is a
property of the mutation, not evidence about the oracle.

**Mutation 17 does not isolate what §7.1 claims it does.** The realisation used here —
`Replace` resizing in place when the price is unchanged, which is the plausible-looking
optimisation someone would actually write — changes the reported status as well as the
queue position, so it is caught by `verdict` before rank is consulted. §7.1 names
mutation 17 as one of the three that justify the ranked book comparison; on this
realisation it does not, and §10.3 shows mutation 6 does.

**Mutations 1 and 6 confirm the rank assertion, but only 6 isolates it.** With the book
compared as a ranked list, mutation 1's first divergence *is* `book-rank`. With it
compared as a set (sabotage row 3), mutation 1 is still caught — by `verdict`, because
over a long tape a LIFO level changes which maker trades and that propagates. Mutation 6
is the clean one: as a set, it goes green.

### 10.3 §8: the sabotage runs

| # | Sabotage | Required | Result |
|---|---|---|---|
| 1 | `refmatch` imports `pkg/types` | `TestReferenceMatcherImportsNothing` fails | **fails** |
| 2 | delete the L2-aggregate comparison | mutation 5 stops being caught | **mutations 5 and 8 both go green** |
| 3 | compare the book as a set | mutations 1, 6, 17 stop being caught | **6 goes green; 1 and 17 are still caught, by `verdict`** |
| 4 | zero the generator weight for reduce | `TestEveryModelledCommandIsGenerated` fails | **fails** |
| 5 | add a `cmdKind` with no tier | `TestEveryCommandKindHasATier` fails | **fails** |
| 6 | add a `Config` field, classify it nowhere | `TestEveryConfigFieldIsClassified` fails | **fails** |
| 7 | set a non-zero `PriceBand` in the harness | `TestHarnessConfigMatchesItsClassification` fails | **fails** |
| 8 | run the mutations against ONE seed | a number | **8 of 18** (see below) |
| 9 | replace the step clock with `time.Now` | `TestSameSeedSameObservations` fails | **it does NOT — see below** |
| 10 | weaken the shrink predicate | record whether the shrinker drifts | **no drift on 1, 5 or 18** |
| 11 | map an unmapped reason to a catch-all | `TestEveryTierOneRejectionIsMapped` fails | **fails** |
| 12 | revert the `pkg/wal` alphabet widening | nothing fails | **nothing fails** |

**Row 2 is the important one.** Dropping the L2 aggregate from the `Observation` makes
mutations 5 *and* 8 pass against a broken engine. Nothing else in this repository
sees a level total drifting from the orders in it on a command tape. The asymmetry —
the engine's *maintained* `TotalQty` and `count` against the model's *summed* L3 — is
doing exactly the work §3.2 claimed, and simplifying it to two sums would delete it.

**Row 8: one seed of one profile catches 8 of the 18.** The ten it misses are 2, 6, 7,
8, 9, 11, 13, 16, 17 and 18 — which is to say self-trade prevention, the fill-or-kill
unwind, the reduce path, post-only at the touch and the whole pro-rata profile. The
sweep is load-bearing, and the number is what says so.

**Row 9 named the wrong guard, and the design is fine.** Replacing the injected clock
with `time.Now` leaves `TestSameSeedSameObservations` green. That is §3.3(a) working:
no timestamp is in the `Observation`, and with every clock-reading knob at its
disabling value no tier-1 verdict depends on what the clock said, so a live clock
*cannot* make an observation stream differ. The property still deserves a guard, so
this slice adds `TestHarnessClockIsDeterministic`, which asserts it directly — two
engines built from the same profile see the same instants — and which does fail on the
sabotage.

**Row 10 says the class enum is not earning its keep in the shrinker.** With the
predicate weakened from "same divergence class" to "still diverges", mutations 1, 5 and
18 all shrink to the *same* minimal tape with the *same* class, and the shrinker never
reported a drift. By §8's own criterion that is an argument for simplifying it. It is
kept, for a different reason than the one §4.4 gave: the class is the most useful line
in the failure message, and `TestShrinkKeepsTheClass` shows the predicate does matter
on a tape carrying two independent failures. What is *not* demonstrated is that a real
engine mutation ever produces one, and that is written down here rather than assumed.

**Row 12 behaved exactly as intended.** Narrowing `tape.Recovery` back to submits,
cancels and the four phase transitions leaves every `pkg/wal` test green. The boundary
sweeps have no opinion about how wide the alphabet is — which is precisely why the
alphabet is now a shared artefact with `TestEveryModelledCommandIsGenerated` on it,
rather than a `buildTape` local to one test file.

**§7.2, the changes that must NOT be flagged.** Removing the node and level pools, and
replacing the sorted price vector with a differently-ordered container, both leave the
whole differential suite green. Removing the pools does turn `TestZeroAllocHotPath` and
`TestCancelReplaceIsAllocationFree` red — which is those tests doing their job, not the
harness asserting an implementation detail. The third no-op (replacing a level's
doubly-linked list with a slice-backed queue) was **not run**: it is a rewrite of
`PriceLevel` and its call sites rather than a mutation, and it belongs to the slice
that actually does it. That is a gap in this section, not a pass.

### 10.4 §7.3: the numbers

- **`TestDifferentialTape`: 0.26 s** over three runs, for 16 tapes across 3
  profiles and **2,240 commands compared** — each with a full L3 copy, an L2 fold, an
  event batch, and a snapshot-restore that is now itself a full state comparison
  rather than a digest. Target was under 5 s, so §9's worry that the observation would
  be expensive enough to tempt someone into sampling every k commands does not arise
  at this tape length. It would at 500,000 ops, which is why the soak stays M13's
  problem and not this harness's. Strengthening the restore comparison cost nothing
  measurable: the same 0.26 s.
- **`TestSnapshotRestoreEqualsUninterruptedExecution`: 0.03-0.04 s** for 48 subtests
  (3 forks × 16 tapes), each driving the tail of a tape through a restored engine.
- **`TestCrashAtEveryBoundary`: 0.77 s → 0.29 s** when the alphabet widened, back to
  **0.70 s** once the generator was fixed to keep the auction phases pure (below), and
  **0.70-0.76 s** now that the tape carries the tier-1 order payload as well. It got
  *cheaper*, not dearer, at the first step because the wider alphabet keeps less
  resting inventory — which is the finding in the next bullet, not a saving.
- **The widened alphabet made the boundary sweep wider and thinner at the same time.**
  On the same 400-command tape: continuous prints **202 → 117**, peak resting depth
  **72 → 45**, auction prints **11 → 16**, and five command kinds (reduce, replace,
  cancel-all, halt, cancel-only) swept at every boundary that the old tape could not
  reach at all. `TestCrashAtEveryBoundary` now asserts floors on all three numbers
  rather than "> 0", so the next alphabet change has to argue a number down.
- **And the next alphabet change had to.** Adding the order *payload* (§3.5) at the
  differential draw rate took the same sweep to **52 prints and 25 peak depth**, which
  the floors caught. `ExoticDamp` at 4 lands it at **110 prints, 17 auction prints,
  peak depth 32** — every floor cleared, no number argued down, and every exotic path
  still drawn. The floors did the job they were added for on the very next change.
- **Shrunk tape length: 1 to 4 commands** across twenty-one mutations, target ≤ 12.
- **One seed catches 8 of 18 mutations; the sweep catches 18 of 18.**
- **`internal/refmatch` is 1,049 lines across two files** — 243 comment lines, 91
  blank, **715 of code**. §7.3 said that passing a few hundred lines is where
  "obviously correct by inspection" stops being true, so this is over the line it
  drew and the honest answer is that the line was drawn against the wrong quantity.
  Most of those 715 are *declarations*: the model's own enums, the `Resting`, `Trade`,
  `Level`, `Event` and `Observation` structs, and the accessors the harness folds into
  an observation — all of which exist because the model may not borrow `pkg/types`.
  The part a reviewer actually has to believe is the matching walk plus the command
  surface, and that is ~230 lines. The tier boundary held; the metric did not, and a
  future §7.3 should count the walk rather than the package.
- A 30-minute-equivalent local `-fuzz` run (30 s, 70,278 executions, 295 new
  interesting inputs) found **no divergence**.

### 10.5 Where this document was wrong about its own design

- **§4.1 said the recovery alphabet should simply be widened. Widening it silently
  turned the auction off.** With halts, cancel-onlys and cancel-alls drawable
  everywhere, commands landing *inside* a pre-open or closing-auction phase refused or
  removed the very orders the uncross was going to clear, and the 400-command sweep
  went from 11 auction prints to **zero** while every assertion in it still passed —
  because the assertion guarding that (`auctionPrints() != 0`) is satisfied by one
  stray print. The generator now draws submits and nothing else inside an accumulating
  phase (`tape.accumulating`), and the sweep asserts floors instead of non-zero. This
  is §1's lesson arriving by the opposite route: not an alphabet too narrow to reach a
  command, but one wide enough to stop reaching a *phase*.
- **§8 row 9 assigned the wrong guard** (see 10.3).
- **§7.1's mutation 17 does not isolate the ranked comparison** on any realisation
  reachable without book surgery (see 10.2).
- **§4.4's shrink predicate is not yet justified by a real mutation** (see 10.3, row 10).
- **The generator's first version reported coverage it did not have, twice.** Drawing a
  reduce command at an 8% weight produced **zero successful reduces in sixteen tapes**,
  because the account was drawn independently of the target and a venue answers "not
  yours" and "does not exist" identically. The book-size cap was likewise never
  reached. Both were invisible in a kind-count and both are now asserted by outcome
  (`TestDifferentialSweepReachesEveryOutcome`), which is the level the §4.2 guards
  should have been written at in the first place.

#### Found by adversarial review of the first implementation

Six more, all confirmed by running the case rather than by argument. Three were holes
in the **oracle**, which is worse than a missing feature because a hole looks like
coverage.

- **§3.3(c)'s equivalence argument was false, and it hid a real class of restore bug.**
  "State that is not in the snapshot and not in the observation is state no consumer
  and no restart can see" — falsified by `PriceLevel.TotalQty`. A `LoadSnapshot` that
  double-counts every restored order into its level ships a venue serving double the
  true L2 depth, and passed `go test ./...` across all 23 packages. `restoreMatchesLive`
  was a digest round-trip and could not see it. Fixed in §3.5(a), and §3.5(b) adds the
  fork-and-continue test that makes "equals uninterrupted execution" literally true.
- **§3.5's replay bullet claimed an alphabet the recovery tape did not speak.**
  `tape.Recovery` set `Exotic: false`; all 287 submits on the 400-command sweep were
  plain GTC limits. The replay oracle never carried a rejected FOK or an STP-cancelled
  maker across a crash boundary — the two paths §9 named in advance. Fixed, with an
  outcome guard.
- **§3's ranked-L3 sentence credited itself with catching an iceberg refill.** Icebergs
  are tier 2 and never generated; the mutation passed all 23 packages. Corrected, and
  the property now has a hand-written test.
- **§2.2 overstated independence.** Mechanical independence holds and is enforced;
  derivation independence does not, and the duplicated comment lines are the evidence.
  Written up in §2.2 rather than tidied away.
- **§5 described a nightly campaign stronger than the one that ran.**
  `FuzzDifferential` used `full=false`, making 30 minutes of fuzzing a weaker oracle
  than a 0.25-second test; and the corpus holds fuzz inputs, not shrunk tapes. Both
  corrected in §5.
- **§4.5 promised three command-line flags that do not exist.** Corrected in §4.5. It
  was nearly dropped silently, which is why it is listed here.

Two more, smaller, fixed in code without a spec claim behind them: the compared "event
stream" carried no trade payload, so corrupting the price and quantity on every
published fill left `pkg/matching` entirely green (caught only by gateway and
marketdata tests reading it downstream); and `TestSameSeedSameObservations` collected
two streams of *engine* observations, so nondeterminism in the model would have
surfaced as an unexplained intermittent divergence with no test pointing at it.

