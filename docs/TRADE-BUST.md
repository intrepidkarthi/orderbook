# Trade Bust — Annulling a Print Without Rewriting History

Status: **implemented** — `pkg/matching/bust.go`, wire v3, drill D7 in CI; written as
a spec before any code existed, and §7 records what building it found ·
Author: Karthikeyan NG · Last updated: 2026-08-10

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
   machine-checked to reconstruct the L3 book across 23 scenarios.
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

6. **Neither edge of the protocol can name a trade.** ⚠️ **Historical — read the
   indented paragraph below before quoting this item.** A status pass in 2026-08 cited
   this bullet as the current state of the wire and reported M8 as blocked on it. It is
   a problem statement from before the work, and the paragraph that closes it is eight
   lines further down. `wire.Executed`
   (`internal/wire/wire.go:575`) carries ClOrdID, price, quantity, leaves and
   aggressor — no trade id. `wire.MDTrade` (`internal/wire/wire.go:200`) carries
   price, quantity and aggressor — no trade id. `marketdata.Update` for `UpdateTrade`
   is the same. So today a venue cannot tell a client *which* of its fills was busted,
   because it never told the client the fill had a name. The threat model's "clean
   trade-bust" claim fails here first, before any of the interesting semantics.

   *Closed after this spec was written:* wire **v3** gives both payloads a `TradeID`
   and adds `Busted` (`U`) / `MDBust` (`u`); the wire is now at **v4**, and
   `cmd/obgw/bust_e2e_test.go:21` busts a trade across both edges over real sockets
   with two counterparties and a market-data subscriber all agreeing on the id. §5
   rows 4 and 5 are marked done. This paragraph is left as it was because §3 records
   what was true when the design was made, and the gap is why the design has §4.5 —
   but a reader after the state of the code should check `internal/wire/wire.go`, not
   this section. A spec's §"why this exists" is an artefact of its own date.

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

- **The wire protocol — deferred, then done.** Carrying trade identity to real clients
  meant a `Version` bump on `Executed` and `MDTrade` plus two new message types,
  against a wire frozen by golden hex. It was deliberately excluded from the first
  pass so the semantics and the compatibility story were not debugged at once, and it
  landed immediately after as wire **v3**: `TradeID` on both payloads, `Busted` (`U`)
  on order entry, `MDBust` (`u`) on market data. Every other payload is byte-identical
  to v2 apart from the version field. See §7 for what routing a bust to a client
  turned out to require.
- **Rebooking, corrections and price adjustments.** Real venues bust *or* adjust to a
  reference price. Adjustment is a different animal — it changes the economics rather
  than voiding them — and it needs the settlement layer this project deliberately does
  not have.
- **A bust window.** CME's is eight minutes; venues differ. `Bust` refuses unknown
  ids and nothing else, because a time policy belongs to the operator, and a wrong one
  compiled into the engine is worse than none.

## 5. Deliverables

| # | Deliverable | Done when | Status |
|---|---|---|---|
| 1 | Control commands become durable | A halt, resume, cancel-only and mark-price change issued after the last checkpoint survive `wal.Recover`; the regression test fails against today's code | **done** — `TestControlCommandsSurviveRecovery`, `TestControlCommandsReplayInOrder` |
| 2 | `Engine.Bust` + `EventBusted` + registry in snapshot and digest | Bust refused for unknown/duplicate ids; digest differs between a busted and an unbusted engine; `TestEventStreamReconstructsBook` still passes with busts in the tape | **done** — `pkg/matching/bust_test.go`, and a 23rd conformance scenario |
| 3 | Bust survives crash and replication | Recovered engine agrees on the bust registry; the replication drills' follower digest-matches a primary that busted mid-stream | **done** — `pkg/wal/bust_recovery_test.go`, drill D7 |
| 4 | A consumer that would notice | `marketdata.Feed` publishes `UpdateBust` with a trade id, and a subscriber resuming from a sequence before the bust still learns about it | **done** — `pkg/marketdata/bust_test.go` |
| 5 | The wire can name a trade | `Executed` and `MDTrade` carry a trade id and each edge has a bust message | **done** — wire v3; `cmd/obgw/bust_e2e_test.go` busts across both edges over real sockets |

Every test above was run against deliberately broken code before it counted: the
registry removed from the snapshot, the wall-clock normalisation removed from the
digest, replayed busts dropped in `RestoreAfter`, and the trade id stripped from
the feed's bust update. The digest test passed against one of those sabotages on
its first draft; §7 has that story.

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

Written after. Deliverables 1–4 are done; §5's table is the checklist they were
graded against.

**The threat model's claim was wrong twice over, not once.** It said the WAL spine
gave "clean trade-bust / replay". The obvious failure is that nobody had built it.
The instructive one is that the mechanism it named — replay the log without the
busted trade — is the wrong mechanism, and building it would have produced a venue
whose recovered tape did not match the tape its subscribers received. The row now
names `Engine.Bust` and says what the old one described.

**The halt gap was already live, and had been for four releases.** §3.5 predicted
it from reading `logCommand`; it reproduced on the first try. An operator halt
issued after the last checkpoint was in no log, so a restart brought the venue back
Open. That is not a trade-bust bug — it was shipped, in the dark, in every release
since control commands existed, and the only reason it surfaced here is that a bust
needed the same seam and the seam turned out not to exist. Fixed in the same arc
(`TestControlCommandsSurviveRecovery`), because shipping a durable bust next to a
non-durable halt would leave two classes of control command that look identical and
behave differently under crash.

**The first version of the digest test passed against sabotage.** It busted on one
engine, not on another, and compared digests — which differ anyway, because
emitting `EventBusted` moves `EventSeq`. So it would have reported success with the
registry left out of the snapshot entirely. Caught by running it against that exact
sabotage rather than by reading it. The test now compares two snapshots that differ
in nothing but the registry. Worth recording because the sabotage pass is the only
reason this section is not claiming a property the suite never checked.

**§2's four rules survived contact.** No consumer wanted the orders back, and the
one that could have — `marketdata.Feed`, whose entire contract is that a
subscriber's book equals the engine's — needed the book left alone to keep that
contract. §6's first stated failure mode did not happen.

**§6's second one is real and open.** The registry does grow without bound. It is
in the snapshot, so a venue that busts routinely grows its snapshot and slows its
digest forever. Nothing here needs it yet and no retention rule is invented on
speculation, but "unbounded" is the honest status and it is now stated in the
`EngineSnapshot.Busted` doc comment as well as here.

**§6's third did not materialise.** Mixed-build replication was the worry: new
`EntryKind`s mean an older follower sees records it cannot classify. It does not
crash — `RestoreAfter`'s type switch ignores unknown kinds — but that is the
failure it should have: an old follower silently ignoring busts diverges from its
primary and, because the registry is in the digest, the divergence is *detectable*
rather than silent. Drill D7 is exactly that scenario with the roles reversed, and
it fails as it should when the bust is dropped. A version negotiation between
primary and follower is still not there and is still yours.

**The wire landed, and routing the bust was the hard part — not the encoding.** Wire
v3 gives both payloads a `TradeID` and adds `Busted` / `MDBust`; the golden vectors
confirm every other payload changed only in its version byte. The market-data edge was
a two-line translation. Order entry was not, and the reason is the same one that
shapes §4.1: **by the time a bust arrives, both orders have usually left the book.**
`orderentry.Registry` forgets an order the moment it is filled or cancelled, so
looking the trade up in the live-order map finds nothing and tells nobody — the
implementation that "obviously works" delivers a bust to zero clients in the common
case. The Registry now keeps a bounded memory of recent prints
(`SetFillMemory`, default 65,536 ≈ 26s of tape at the SOAK.md rate) purely so a bust
can be routed, and a bust older than that memory increments `UnroutableBusts` rather
than disappearing — because "we could not tell the client" is an operational fact
somebody has to act on, and it is the direct analogue of §6's unbounded-registry
worry landing somewhere else than predicted.

`TestBustRoutesAfterTheOrdersAreGone` is the test that would have caught the naive
version, and `cmd/obgw/bust_e2e_test.go` is the end-to-end proof: two counterparties
and a market-data subscriber, real sockets, all three agreeing on the trade id.
