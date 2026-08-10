# Multi-Symbol — One Venue, Many Books

Status: **implemented** — all six deliverables; `pkg/matching/id.go`, `manifest.go`,
durable shards, wire v4, `examples/multisymbol`, drill D8. Written as a spec before
any code existed, and §7 records what building it found ·
Author: Karthikeyan NG · Last updated: 2026-08-10

Companion documents:
- [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) — where multi-symbol is
  "partial", and where the sentence this spec exists to correct lives.
- [`EXCHANGE-ARCHITECTURE.md`](EXCHANGE-ARCHITECTURE.md) — why shard-by-instrument
  is the shape, and why this project does not bundle the layers above it.
- [`PROTOCOL.md`](PROTOCOL.md) — the frozen wire, which serves one instrument.
- [`REPLICATION.md`](REPLICATION.md) — per-shard recovery is per-shard replication;
  the drills generalise or they do not, and §6 says which.

---

## 1. Why this exists

[PRODUCTION-READINESS.md](PRODUCTION-READINESS.md) §3 says:

> `matching.Shards` routes by symbol across independent engines. But order ids and
> event sequences are per-engine, so there is no venue-wide identifier space […] A
> multi-symbol venue is a routing layer you write.

The first two sentences are true. The last one is more comfortable than the truth,
and checking the code rather than the sentence is what this section is for.

`Shards` (`pkg/matching/shard.go`) creates each shard with
`NewRunner(RunnerConfig{Engine: …, QueueSize: …})`. There is no `Log` field in
`ShardsConfig` and no way to supply one. **A sharded venue therefore has no
command log at all** — no write-ahead journal, no snapshot path, no crash
recovery, no replication, because every one of those hangs off `CommandLog`. What
ships today is a router for a venue that never restarts.

That is not "a routing layer you write". It is most of the venue, and the gap
deserves a spec rather than a clause.

## 2. The decision everything else follows from

**Is there a single order of events across symbols?**

**No, and not by accident.** A venue-wide sequence requires every shard to agree on
a common counter, which requires a serialisation point that every command passes
through — which is the exact bottleneck sharding exists to remove. Buying a total
order costs the linear scaling that is the only reason to have more than one
engine.

So: **each symbol is its own timeline.** Within a symbol, everything this project
already proves holds — deterministic apply, a gap-free event sequence, exact
recovery, a book that reconstructs from its own stream. Across symbols there is no
ordering guarantee whatsoever, and none is implied by anything the venue publishes.

What that costs, stated plainly rather than discovered:

- **No cross-symbol atomicity.** No spread orders, no basket or contingent orders,
  no "fill both legs or neither". A venue that needs those needs a layer above with
  its own protocol, and that layer is not this repository.
- **No venue-wide "as of" instant.** You cannot snapshot the whole venue at one
  point in the command order, because there is no such point. Per-symbol snapshots
  taken at unrelated moments are what exists, and §4.4 argues that is sufficient.
- **A client watching two symbols sees two sequence spaces** and must not infer
  from "BTC event 500 arrived before ETH event 500" that anything happened first.

This is the same shape of answer as [EXCHANGE-ARCHITECTURE.md](EXCHANGE-ARCHITECTURE.md)
gives for consensus: the honest primitive, and an explicit refusal to bundle the
opinionated layer above it.

## 3. What exists today, verified against the code

Checked at the time of writing, not quoted from other documents.

1. **Routing works.** `Shards.Runner(symbol)` lazily creates one `Runner` per
   symbol, shared-nothing, safe for concurrent producers
   (`pkg/matching/shard.go`). This part needs nothing.
2. **Ids collide across symbols.** `Engine.nextID` assigns from a per-engine
   `orderSeq` starting at zero (`pkg/matching/engine.go:552`), so every symbol has
   an order 1, a trade 1 and an event 1. An `int64` order id does not name an order
   at a venue with two books.
3. **A shard cannot be given a command log.** `ShardsConfig` carries `NewConfig`
   and `QueueSize` and nothing else; `RunnerConfig.Log` is unreachable through the
   router. Durability, recovery and replication are all unavailable to a sharded
   venue. This is the finding that turned a clause into a spec.
4. **The order-entry registry is symbol-blind.** `byClOrd` is
   `account -> clOrdID -> engine order id` (`pkg/orderentry/stream.go:211`). Two
   symbols, one account, one repeated `ClOrdID`, and the second resolution silently
   wins.
5. **Both edges serve one instrument.** The gateway rejects any order whose symbol
   is not its own (`cmd/obgw/server.go:707`), holds a single `marketdata.Feed`
   (`server.go:164`), and `wire.MDSubscribe` has no symbol field — a subscriber
   cannot say what it wants to watch.
6. **What already generalises for free.** `types.Order` and `types.Trade` carry
   `Symbol`; `EngineSnapshot` carries `Symbol` and so self-identifies; the WAL
   format is per-engine and needs no change; `EngineSnapshot.Digest`,
   `EventSink` and `marketdata.Feed` are all per-engine concepts that simply get
   instantiated N times.

## 4. The design

### 4.1 Identifiers — partition the space, do not centralise it

Give each symbol a **stable shard index**, and compose it into the id:

```
id = (shardIndex << 48) | seq      // seq is the existing per-engine counter
```

An `int64` order id must stay positive — negative ids would break every comparison
and every `omitempty` in the log — so the budget is 63 bits, not 64: **15 bits of
index (32,768 symbols) and 48 bits of sequence (2.8×10¹⁴ orders per symbol).**
Neither is a limit a venue here will reach, and getting this wrong by one bit is
the sort of arithmetic that should be written down rather than assumed.

Why this rather than a global atomic counter: a shared counter makes a shard's ids
depend on its interleaving with every other shard, so **replaying one shard's log
alone would not reproduce its own ids.** That breaks recovery, which is the
property this project is most confident about. A partitioned id is globally unique
*and* per-shard deterministic, which is the only combination that keeps both.

Why this rather than "the identifier is the pair `(symbol, id)`": a composite key
is honest but pushes two fields through every audit trail, drop copy and regulatory
report that wants to name one order. A single `int64` that is unique venue-wide is
what those consumers actually need, and it costs one durable artifact:

- **The venue manifest.** `symbol -> shardIndex`, durable, CRC-checked like the
  snapshot, written before a symbol first trades. **If a symbol comes back with a
  different index, every id it ever issued is now ambiguous** — so the manifest is
  not a convenience, it is the thing that makes the property survive a restart, and
  it must be treated with the same suspicion as the snapshot (which had no
  integrity check for four releases, for exactly the "it can't go wrong" reason).
- `Config.ShardIndex` feeds `nextID`. Replay reproduces composite ids because the
  index is config, not state.
- `EngineSnapshot.Seq` keeps storing the **raw** per-shard counter, so the digest
  is unaffected and a snapshot stays comparable across the change.

A pleasant consequence: `shardIndex = id >> 48` routes a command by order id
without a lookup.

### 4.2 Sequence spaces — per symbol, and said out loud

`Event.Seq` and the market-data feed sequence stay per symbol. They are not
partitioned, because unlike an order id they are never quoted outside the context of
their own stream, and inflating them would only invite the belief that they can be
compared across symbols. The protocol documentation must say that comparison is
meaningless.

**Trade ids are the exception, and are partitioned.** This paragraph originally
grouped `TradeSeq` with the two above and that was wrong: `Busted` and `MDBust` name
a print by id *alone* on both wire edges, so a trade id is quoted outside its stream
by construction, and an unpartitioned one would let an operator annul an ambiguous
print at a two-book venue. `Engine.Bust` splits the id and refuses another symbol's
trade. `EngineSnapshot.TradeSeq` keeps the raw counter, like `Seq`.

| | Partitioned? | Because |
|---|---|---|
| Order id | **yes** | named by cancel, audit, drop copy |
| Trade id | **yes** | named by `Busted` / `MDBust`, alone |
| `Event.Seq` | no | a cursor into one engine's own stream |
| Feed sequence | no | a cursor into one feed |

### 4.3 Durability — one log per shard

`ShardsConfig` gains `NewLog func(symbol string) (CommandLog, error)`, and the
router wires the result into each `RunnerConfig.Log`.

One log per shard, not one per venue: a shared log serialises every append behind
one lock, which is the §2 bottleneck arriving through the back door. Per-shard logs
also mean recovery, replay admission and the replication drills are the *existing*
single-symbol code paths, run N times — which is the same argument
[REPLICATION.md](REPLICATION.md) made for making the follower a Runner in replay
mode: a bug in the multi-symbol path is then a bug in a path that already has
tests.

### 4.4 Recovery — per shard, and why no venue-wide cut is needed

Each shard recovers from its own snapshot plus its own log tail, exactly as today.
Shards will recover to different points in wall-clock time. **That is correct, not a
compromise**, and the reason is §2: with no cross-symbol commands there is no
invariant spanning two books that a skewed recovery could violate. Every ordering
guarantee this venue makes is within one symbol, and each symbol keeps its own.

Venue startup reads the manifest, then recovers each named shard. A symbol in the
manifest with no snapshot and no log is a new symbol; a snapshot whose `Symbol` does
not match the manifest entry that pointed at it is a refusal, not a repair.

### 4.5 The edges

**Order entry survives without a wire change**, on one stated condition:
`ClOrdID` must be unique per account across the venue, not merely per symbol. That
is already standard FIX practice (uniqueness per firm per day) and it means
`wire.Cancel`, `Reduce` and `ReplaceOrder` — all of which name an order by
`ClOrdID` alone — keep working untouched. `wire.Enter` already carries `Symbol`.
`orderentry.Registry` keys `byClOrd` by account and client id as it does now; what
changes is that the engine order id it resolves to is globally unique, so the
gateway can route the resulting command by `id >> 48`.

The venue must **enforce** that uniqueness rather than assume it, since a duplicate
across symbols silently retargets a cancel — the `MaxOrders`-class defect, one
layer up.

**Market data needs one field.** `wire.MDSubscribe` gains `Symbol`, and a
subscription is then for exactly one symbol: every message on that connection
belongs to it, so `MDDelta`, `MDTrade`, `MDBust` and the rest are unchanged. One
connection per symbol is also how a subscriber wants to be shaped, since it can
drop a feed it stopped caring about without disturbing the others.

That is a `Version` bump to **v4** on the market-data side. It is one field on one
message, against a wire that has just been through v3, and doing it in the same
change as the semantics is the mistake [TRADE-BUST.md](TRADE-BUST.md) §4.5
deliberately avoided — so it lands as its own deliverable, after the core.

### 4.6 What stays out

- **Cross-symbol orders of any kind**, per §2.
- **A venue-wide event stream.** Anyone who wants one can merge N per-symbol
  streams and choose their own tie-break; the venue will not pretend its choice is
  authoritative.
- **Dynamic symbol lifecycle.** Listing, delisting and symbol renaming are venue
  operations with their own approvals; the manifest is append-mostly and a
  delisted symbol keeps its index forever, because reusing one is how ids stop
  being unique.
- **Per-symbol policy configuration.** `NewConfig(symbol)` already lets an embedder vary tick
  size, bands and risk limits per instrument. Nothing further is needed.

## 5. Deliverables

| # | Deliverable | Done when |
|---|---|---|
| 1 | Partitioned ids | `Config.ShardIndex` composes ids; two shards issue disjoint id ranges; replay of one shard alone reproduces its ids exactly |
| 2 | The venue manifest | Durable, CRC-checked, refused on mismatch; a symbol's index is stable across restart, and a test proves ids stay unique when it is not |
| 3 | Durable shards | `ShardsConfig.NewLog`; a sharded venue crash-recovers every book, asserted against uninterrupted controls per symbol |
| 4 | Symbol-safe order entry | Venue-wide `ClOrdID` uniqueness enforced and tested, including the duplicate-across-symbols case that would retarget a cancel |
| 5 | Market data per symbol | Wire v4 `MDSubscribe.Symbol`; two subscribers on two symbols each reconstruct their own book, proven over sockets |
| 6 | The drills generalise | Replication D1–D7 run against a two-symbol venue |

Status: **all six done.** See §7 for what changed along the way, including two
places the spec was wrong.

## 6. How this can fail, stated in advance

- **The manifest is a new single point of failure.** It is small, rarely written and
  CRC-checked, which is exactly what was said about the snapshot before it turned
  out to have no integrity check at all. If it is lost, every id the venue ever
  issued becomes ambiguous — worse than losing a snapshot, which only costs a
  replay. Deliverable #2 should be paranoid.
- **The 15/48 split may be wrong.** A venue with few symbols and enormous flow would
  rather have 8/55. If the split needs to be configurable, ids stop being
  self-describing and `id >> 48` routing dies with it — that would be a finding
  worth writing down, not working around.
- **Venue-wide `ClOrdID` uniqueness may be too strong for real clients.** If it is,
  §4.5 collapses and `Cancel`/`Reduce`/`ReplaceOrder` all need a symbol field —
  a much larger wire change than v4's one field. This is the assumption most likely
  to be wrong, and it should be tested against a real client integration before
  deliverable #4 is called done.
- **Per-shard recovery skew may surprise an operator** even though §4.4 argues it is
  harmless. "The venue is up" becomes a per-symbol question, and the dashboard,
  health endpoint and runbooks all currently assume one answer.

## 7. What building it found

Written after. Deliverables 1–4 are done; 5 and 6 are not, and §5's table says so.

**The spec was wrong about trade ids, and the error was mine.** §4.2 originally said
`Event.Seq`, `TradeSeq` and the feed sequence all stay per symbol, on the grounds
that a stream cursor is never quoted outside its own stream. That is true of the two
sequences and false of a trade id, and trade bust is why: `Busted` and `MDBust` name
a print by id **alone** on both wire edges, so at a two-book venue an unpartitioned
trade id would have let an operator annul an ambiguous print. Trade ids are
partitioned like order ids, `Engine.Bust` now splits the id before validating, and
busting another symbol's trade is `ErrUnknownTrade` rather than a coin flip. Written
down here because the spec's own §4.2 said otherwise for a day.

**Shard 0 composing to the identity was worth more than expected.** `ComposeID(0,
seq) == seq`, so every existing single-symbol deployment keeps the ids, snapshots,
logs and golden vectors it already had, and this whole feature is invisible to them.
That was a design goal; what was not obvious in advance is that it also made the
change testable against the existing suite without regenerating anything.

**Refusing is better than serving, and it is a breaking change.** A second symbol
with no manifest is now refused rather than silently given shard index 0 and
colliding ids. Two existing tests had to gain a manifest. That is the right trade —
duplicate ids have no symptom except two orders nobody can tell apart, discovered
much later — but it does break any embedder running `Shards` multi-symbol today,
which is exactly what they were unknowingly doing.

**A manifest test passed against its own sabotage.** The first version of the
bad-CRC case corrupted the file by flipping its first `0` byte. That byte lives
inside the magic's `\u0001` escape, so with the CRC check disabled the test still
went green — by detecting a bad magic, not the thing it was named for. It now edits
a symbol's index through the JSON and leaves the checksum alone. Second time this
pattern has been caught by running a test against the exact sabotage it claims to
detect (the first was the bust digest, [TRADE-BUST.md](TRADE-BUST.md) §7), and the
lesson generalises: a test that can only fail one way is not proof it fails that way.

**§4.5's uniqueness rule was already half-enforced, by a mechanism I had not
credited.** The engine's `DedupClientOrderIDs` ring already makes a client id
single-use, deterministically, on the matching goroutine — that *is* FIX's per-day
uniqueness, and it has been there for releases. What it cannot do is span symbols,
because the ring belongs to one engine. So the venue-wide check added here is not
the enforcement §4.5 imagined; it is the cross-symbol complement of an existing
per-symbol one, and it is admission control rather than a guarantee because the
naming index it reads is cleared asynchronously by the pump.

That distinction matters and the spec did not draw it: the authoritative check is
the engine's and it is deterministic; the venue-wide check is advisory and racy at
the edges. A genuinely venue-wide *deterministic* rule would need the dedup on the
matching goroutine across shards — which is a cross-shard serialisation point, the
thing §2 spends the whole document refusing. **That tension is unresolved**, and it
is the honest status rather than a to-do: for now the cross-symbol case is caught at
admission and the per-symbol case is caught properly.

**`cmd/obgw` now serves many books, and converting it found one real bug.**
`buildOrder` validated the incoming symbol and then stamped the *configured* one
onto the order — so at a two-book venue every order silently landed on the first
book. It was caught by the first test written against the converted gateway, which
is the argument for writing that test rather than trusting a refactor that
compiled.

The structure is one `symbolBook` per instrument (matching goroutine, command log,
market-data feed, rate gate) and a venue-wide account layer above them: one
`Registry`, one publisher, one stream per account. A client holds one session and
sees one ordered conversation whatever mix it trades, which is what makes a client
id enough to name an order without also naming a symbol.

**Routing a cancel needed the session to remember, not the registry to be asked.**
`wire.Cancel` names an order by `ClOrdID` alone (§4.5), so the gateway has to pick
a book without being told. Resolving the engine order id up front to read its shard
field is the obvious move and it is wrong: the naming index is written by the
*matching* goroutine, so a cancel arriving while its own Enter is still queued
resolves to nothing and is refused for an order that is about to exist — the
orphaned-order defect [SOAK.md](SOAK.md) measured at 12,843 orders in thirty
seconds. The session already knows the answer, because it read the Enter and the
Enter carried the symbol. So `session.entered` records it on the read loop before
the command is enqueued, with the registry as the fallback for orders from earlier
connections. `TestCancelRoutesWhileItsOwnEnterIsStillQueued` is the test, and
deleting the session record is the sabotage that makes only that test fail.

**Two things the gateway cannot do venue-wide, stated rather than discovered.** A
mass cancel and a `Query` fan across every book and aggregate, because an account's
orders are its orders. The price gauges do not aggregate and cannot — a last trade
price averaged over two instruments is not a number — so at a multi-book venue they
report the first book. The right answer is one series per symbol, which needs label
support `pkg/observability` does not have. Readiness does take the worst book,
since a venue with one wedged matcher is not ready however healthy the others are.

**The market-data edge landed as wire v4 and a new reference before the gateway
caught up.** `MDSubscribe` gains `Symbol`; a subscription selects one instrument and
every message after it belongs to that instrument, so no other market-data payload
changed and the golden vectors show it. `cmd/obgw` validates the field and refuses
anything but the one symbol it serves — a real improvement even at one instrument,
since a subscriber cannot detect being served the wrong book for itself.

The first consumer was `examples/multisymbol`: a two-book venue with per-shard
feeds and logs, serving both over sockets. Keeping it separate from the gateway for
one release was deliberate — converting `cmd/obgw` in the same pass as the protocol
would have meant debugging both at once, the argument
[TRADE-BUST.md](TRADE-BUST.md) §4.5 made for deferring the wire, applied one level
up. The gateway followed immediately after, and the example remains the smaller
thing to read first.

**Drill D8 was cheap, and §2 is why.** With no order across symbols there is nothing
to synchronise, so the multi-symbol drill is two independent primary/follower pairs
and a check that their ids stay disjoint. It comes with a negative control —
`TestDrillD8Refuses_AFollowerOnTheWrongShard` — because a drill that cannot fail is
decoration: a follower carrying the wrong shard index rebuilds the same orders under
different numbers, and the digest catches it.

**One test artifact worth recording**, because it looks like a bug and is not. A
venue's live engines have an event sink attached; an engine replayed from its log
with no sink emits nothing, so its `EventSeq` stays at zero and its digest differs
from the venue it faithfully reproduced. That is a difference in what was
*published*, not in the book. Real recovery attaches the sink after replay for
exactly that reason (`cmd/obgw` does); a test comparing against a live engine has to
match the arms.
