# Trade Bust — Annulling a Print Without Rewriting History

Status: **specified** — no code yet · Author: Karthikeyan NG · Last updated: 2026-08-10

Companion documents:
- [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) — says there is no trade-bust
  path and that it "interacts badly with an append-only event stream, so design it
  early." This is that design.
- [`THREAT-MODEL.md`](THREAT-MODEL.md) — claims the WAL spine gives "clean trade-bust /
  replay", and cites the LME nickel busts against it. That claim is the one this spec
  exists to test.
- [`PROTOCOL.md`](PROTOCOL.md) — the frozen wire, which turns out not to be able to
  name a trade at all.

---

## 1. Why this exists

Two documents in this repository disagree about whether trade bust exists.

[PRODUCTION-READINESS.md](PRODUCTION-READINESS.md) §3, under "The financial stack —
absent by design": *"No trade bust or correction: once a trade is published there is
no way to amend it."*

[THREAT-MODEL.md](THREAT-MODEL.md) §5, listing what the engine already provides:
*"Monotonic sequence + WAL + snapshot — replayable audit spine; clean trade-bust /
replay,"* mapped to the LME nickel episode, where roughly $3.9B of trades were busted
in a single morning.

One of those is wrong. The threat model's row is the familiar shape: a capability
asserted because the *ingredients* are present, by a document that never tried to
cook. This project has found four such seams by building consumers for them, and the
fifth — the log-tail hook that handed subscribers a pointer into live state — was
found by `examples/replication` within a day of the code existing.

So the work is not "add bust". It is: **write down what a bust means on an
append-only stream, then build the one consumer that would notice if the sentence
were wrong.**

## 2. What a bust is, and the four things it is not

A bust annuls the **economics** of a print that has already been published. It is a
clearing-level statement — "this trade will not settle" — arriving after the matching
engine has moved on.

Everything below follows from one observation: **the book at bust time is not the book
at trade time**, and no amount of care makes it so.

1. **A bust does not restore the orders to the book.** Both sides have since traded
   elsewhere, cancelled, been filled, or gone home. Re-resting them would inject
   liquidity at a stale queue position against a market that has moved — a second
   wrong, not an undo. Nasdaq's ITCH "Broken Trade" message says a trade is broken and
   says nothing else; the book is untouched.
2. **A bust does not unwind what the print caused.** Stops that fired off the busted
   price stay fired. Trailing stops that ratcheted stay ratcheted. A band-breach pause
   it tripped stays tripped, and the orders rejected during that pause stay rejected.
   Cascading the annulment is unbounded — every consequence has consequences — and
   every venue that has busted a trade has drawn this line in the same place.
3. **A bust does not rewind the last trade price.** `LastTradePrice` is the reference
   the price band and pegged orders evaluate against *now*. Rewinding it would move
   the band under live orders and re-price every peg, which is a book change caused by
   a clearing decision. If your venue wants a corrected reference price, set it
   explicitly with `SetMarkPrice`; that is a separate decision and it should look like
   one.
4. **A bust does not delete or amend the event that reported the trade.** The stream
   is append-only and the digest over it is what recovery and replication are checked
   against. A bust is a **new event that refers backwards**, which is the only shape
   that keeps a replayed tape byte-identical to the live one.

What a bust *is*, then: an appended annotation, addressed to a trade by id, that every
downstream consumer of the tape must apply to its own view of what settled.

## 3. What the code has, verified against the code

Checked at the time of writing, not quoted from other documents.

1. **Trades already have durable identity.** `types.Trade.ID` is assigned from the
   engine's `tradeSeq` (`pkg/matching/engine.go:1630`), monotonic per engine, and it
   survives restart: `EngineSnapshot.TradeSeq` (`pkg/matching/snapshot.go:40`) is
   restored and the digest covers it. A bust has something to name.
2. **The command log is write-ahead and replayed.** `matching.CommandLog`
   (`pkg/matching/engine_loop.go:87`), `wal.Writer` with per-record CRC-32C, and
   `wal.Recover` = snapshot + tail.
3. **The event stream is append-only and sequenced.** `Event.Seq`, gap-free, and
   machine-checked to reconstruct the L3 book across 22 scenarios.
4. **The engine has a precedent for an operator-issued state change.** `Halt` /
   `Resume` / `SetCancelOnly` emit a transition, not a call — a second halt of an
   already-halted venue publishes nothing (`pkg/matching/engine.go:1357`). A bust
   should inherit that rule.

And two gaps this spec found, both of which have to close before a bust means
anything.

5. **Control commands are not durable — and an operator halt is already being lost.**
   `Runner.logCommand` ends with `default: return // control commands carry no book
   state; the snapshot covers them` (`pkg/matching/engine_loop.go:309-310`). The
   snapshot covers them *as of the snapshot*. A halt, resume, cancel-only or mark-price
   change issued after the last checkpoint is not in the log, so recovery does not
   replay it — and a venue an operator deliberately halted comes back **Open**.

   This is not a projection; it reproduces in twenty lines against the shipped code.
   It is also exactly the same reasoning error as the WAL durability comment fixed in
   v0.20.0: a guarantee stated against the wrong reference point. A bust registry
   parked on this seam would be lost the same way, silently, and only after a crash.

6. **Neither edge of the protocol can name a trade.** `wire.Executed`
   (`internal/wire/wire.go:575`) carries ClOrdID, price, quantity, leaves and
   aggressor — no trade id. `wire.MDTrade` (`internal/wire/wire.go:200`) carries
   price, quantity and aggressor — no trade id. `marketdata.Update` for `UpdateTrade`
   is the same. So today a venue cannot tell a client *which* of its fills was busted,
   because it never told the client the fill had a name. The threat model's "clean
   trade-bust" claim fails here first, before any of the interesting semantics.

## 4. The design

### 4.1 The engine

```go
// BustRecord is why a print was annulled, retained so a late joiner can be told.
type BustRecord struct {
    TradeID int64
    Reason  string
    At      time.Time
}

// Bust annuls a published trade. It does not change the book: see TRADE-BUST.md §2.
// Busting an unknown or already-busted trade is refused, not silently accepted.
func (e *Engine) Bust(tradeID int64, reason string) error
```

- **Validation is identity-only.** The engine knows `tradeSeq`, so `tradeID <= 0` or
  `tradeID > tradeSeq` is `ErrUnknownTrade`. It does **not** retain the trades
  themselves — a hot path that kept every print would be a memory leak with a venue's
  uptime — so "was trade 5 for 300 lots at 99" is a question for whoever kept the
  tape, and the engine will not pretend to answer it. Said plainly here because the
  alternative is a caller assuming the engine validated more than it did.
- **Already busted is `ErrAlreadyBusted`**, not a silent no-op. This departs from the
  halt rule in §3.4 deliberately: a duplicate halt is an operator being redundant, a
  duplicate bust is usually an operator busting the wrong id, and a venue annulling
  trades should not swallow that.
- **`busted map[int64]BustRecord` joins `EngineSnapshot` and therefore the digest.**
  Two engines that have applied the same commands are equal only if they agree on what
  settled. A bust registry outside the digest would let a primary and its follower
  disagree about $3.9B and still compare identical.

### 4.2 The event

`EventBusted` is appended to the same stream, referring backwards by trade id. It is a
new `EventKind` at the end of the iota block — never inserted, for the reason
`EventBookDelta` is still declared and never emitted.

The reconstruction claim is unaffected and must be shown to be: `EventBusted` changes
no book state, so `TestEventStreamReconstructsBook` must keep passing with busts in
the tape, and the L3 mirror must ignore the event rather than fail on an unknown kind.

### 4.3 Durability — the seam that has to be built first

`CommandLog` gains the control commands it should always have had:

```go
AppendBust(tradeID int64, reason string) (int64, error)
AppendHalt() (int64, error)
AppendResume() (int64, error)
AppendCancelOnly() (int64, error)
AppendSetMark(price int64) (int64, error)
```

with matching `wal.EntryKind`s appended to the end of that iota block. Replay applies
them exactly as live, and a busted trade is busted again on recovery because the bust
is a logged command like any other.

Fixing halt/resume/mark at the same time is not scope creep — it is the same defect,
and shipping a durable bust alongside a non-durable halt would leave the engine with
two classes of control command that look identical and behave differently under crash.

### 4.4 The consumer

`marketdata.Feed` publishes `UpdateBust`, and `Update` gains a `TradeID` — set on
`UpdateTrade` as well, since a bust addressed to a trade the feed never named is
undeliverable. Both are additive struct fields on an in-process type: no wire change,
no golden-hex churn.

This is what makes the exercise worth doing. The feed is a real subscriber with a
sequenced ring, a snapshot path and a resume path, and it will either take the bust
cleanly or show us which of §2's four rules is wrong.

### 4.5 What stays out

- **The wire protocol.** Carrying trade identity to real clients means a `Version` bump
  on `Executed` and `MDTrade` plus two new message types, against a wire frozen by
  golden hex. That is its own change with its own compatibility story, and doing it in
  the same pass as the semantics would mean debugging both at once. Gap §3.6 stays
  open, and stays *stated* — the core is honest, and no external client can be told
  about a bust until this lands.
- **Rebooking, corrections and price adjustments.** Real venues bust *or* adjust to a
  reference price. Adjustment is a different animal — it changes the economics rather
  than voiding them — and it needs the settlement layer this project deliberately does
  not have.
- **A bust window.** CME's is eight minutes; venues differ. `Bust` refuses unknown
  ids and nothing else, because a time policy belongs to the operator, and a wrong one
  compiled into the engine is worse than none.

## 5. Deliverables

| # | Deliverable | Done when |
|---|---|---|
| 1 | Control commands become durable | A halt, resume, cancel-only and mark-price change issued after the last checkpoint survive `wal.Recover`; the regression test fails against today's code |
| 2 | `Engine.Bust` + `EventBusted` + registry in snapshot and digest | Bust refused for unknown/duplicate ids; digest differs between a busted and an unbusted engine; `TestEventStreamReconstructsBook` still passes with busts in the tape |
| 3 | Bust survives crash and replication | Recovered engine agrees on the bust registry; the replication drills' follower digest-matches a primary that busted mid-stream |
| 4 | A consumer that would notice | `marketdata.Feed` publishes `UpdateBust` with a trade id, and a subscriber resuming from a sequence before the bust still learns about it |

## 6. How this can fail, stated in advance

So that the write-up in §7 is not graded on a curve:

- **The book-untouched rule could be wrong.** If any consumer needs the busted trade's
  orders back, §2.1 is a design error and this ships as a much larger change.
- **The digest could be the wrong home for the registry.** If busts accumulate without
  bound, the snapshot grows forever and the digest slows with venue age. A retention
  rule may prove necessary, and "unbounded" would be the honest finding.
- **The halt fix could break replication.** New `EntryKind`s in the log mean a
  follower on an older build sees records it cannot classify. The drills should be run
  against a mixed pair before this is called done.

## 7. What building it found

To be written after, not before — and it goes here whether or not it is flattering.
