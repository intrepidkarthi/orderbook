# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (pre-1.0: minor
versions may include breaking changes).

## [Unreleased]

### Added

- **`marketdata.Feed`** — the venue's public market-data stream: one sequenced
  broadcast of book deltas, trade prints and venue-state changes, with
  snapshot-plus-delta recovery.

  The guarantee, stated so it can be falsified: for one incarnation the sequence is
  dense and gap-free from 1, and **`Snapshot(at Seq S)` plus every update after `S`
  equals the engine's book**. A subscriber can start anywhere and be exactly right.
  Asserted from many different starting points across a 2,000-command random tape,
  including from sequence 0.

  What makes it true is that `Snapshot` captures the book *and* its sequence under one
  lock. Reading them separately is precisely the bug the order-entry side shipped in
  v0.12.0, where a report claimed consistency with a sequence the client had not
  reached and a change got applied twice.

  It is deliberately **not** `pkg/orderentry`. Order entry is private and per-account,
  and a client that falls behind is owed the messages it missed — an execution report
  it never sees is a position it does not know it has. Market data is public and
  identical for everyone, so a subscriber that falls too far behind is simply told the
  current state. One stream instead of a registry, and a fresh snapshot instead of a
  refusal.

  Retention is bounded, because an unbounded ring turns one stalled subscriber into a
  venue-wide memory leak, and an evicted cursor is refused explicitly rather than
  served a truncated slice that looks complete.

### Fixed

- **A manual halt told nobody.** `Engine.Halt`, `Resume` and `SetCancelOnly` set the
  state and emitted nothing. Only the *automatic* transitions — a guardrail trip, a
  band-breach pause — reached the event stream, so the one halt a venue most needs to
  broadcast, an operator deliberately stopping trading, reached no consumer, no
  market-data feed and no client.

  All three now emit, and only on an actual transition: halting an already-halted
  venue produces nothing, because an event that describes no change is worse than
  none. Cancel-only gets its own kind (`EventCancelOnly`) rather than being reported
  as a halt — a subscriber told the venue is halted when it is still accepting
  cancels would draw the wrong conclusion about whether it can get out of a position.

  Found by writing a market-data test that asserted a halt appears in the feed.

## [0.14.0] - 2026-07-30

A depth bug that had been wrong in public, and the measurements that found it.

**The bug.** A price level's aggregate quantity was not reduced when a resting order
was fully consumed, so L2 depth was over-reported after every complete fill — three
sells of 5 swept by a 12-lot buy left the level reporting 13 lots with a single 3-lot
order at it. `Snapshot` is the read model for `pkg/signals`, the research studies, the
WASM demo and any L2 feed, so all of them saw inflated depth. It was present at least
as far back as v0.12.0.

**How it was found, which is the part worth repeating.** Not by testing the book. By
building an incremental L2 feed above the engine and asserting the derived levels
equalled the engine's own `Snapshot` after every command of a random tape. Two views
of the same state, forced to agree. Every existing test checked the orders *or* the
level; none checked that the two matched, which is exactly the gap a bug like this
lives in.

**What it cost.** Order-flow imbalance is computed from depth, so the published study
moved: mean contemporaneous R² was 0.1685 and is 0.2357, with the predictive gap going
from ~540× to ~577×. The conclusion is *strengthened* — OFI explains more of the
same-interval move than reported and still essentially none of the next one. Kyle's λ
keeps λ, R² and trade counts identical and moved only its depth column. The delta/CVD
study is untouched. `cmd/ofistudy` also carried its verdict as literal text and printed
figures its own table contradicted; it now computes them.

**Two claims corrected by measuring them.** Publishing one tail-latency scenario
understated the tail by ~30×, and the mass cancel — never measured — blocks the
matching goroutine for ~872 µs per 5,000 orders, which makes the kill switch a
venue-wide pause. And recovery was documented as "bounded to O(recent)": true of the
replay, false of the restart, since loading a 100,000-order snapshot is ~174 ms
against ~21 ms for a 10,000-record tail. Restart is O(book) + O(tail).

### Fixed

- **A price level's aggregate quantity was wrong after every full fill, and nothing
  checked it.** The matching engine fills a maker — dropping `RemainingQty` to zero —
  and only then removes it from the book, and `PriceLevel.unlink` subtracted
  `RemainingQty`. So it subtracted nothing, and the maker's entire original size
  stayed in the level total forever. Three sell orders of 5 at one price, swept by a
  12-lot buy, left the level reporting **13** lots with a single 3-lot order resting
  at it.

  `Snapshot` is the read model for `pkg/signals`, the research studies, the WASM demo
  and any L2 feed, so **all of them saw over-reported depth**. Present at least as far
  back as v0.12.0 — confirmed by running the same check against that tag.

  Fixed by making the total the book's own property: each node records what it
  contributed (`node.contributed`), and `unlink` subtracts that. The level total was
  previously a discipline every removal site had to remember, and forgetting it at two
  sites is precisely what this bug was.

  No test caught it because every existing assertion checked the orders *or* the
  level, never that the two agreed. There is now an invariant — level total equals the
  sum of the `RemainingQty` resting at it — asserted after a full fill, after a
  removal with no prior update, and after every step of a 2,000-operation churn. It
  was verified to fail against the old code, after a first attempt at it passed for
  the wrong reason: with a single order at the level, the emptied level is deleted
  outright and takes the error with it.

- **The research numbers were re-measured, and two of the three studies moved.**
  Order-flow imbalance is computed from level depth, so it was affected: mean
  contemporaneous R² was published as 0.1685 and is **0.2357**, with the gap to the
  predictive figure going from ~540× to ~577×. The conclusion is *strengthened* —
  OFI explains more of the same-interval move than reported and still essentially
  none of the next one. Kyle's λ study keeps λ, R² and trade counts byte-identical
  (λ is estimated from trades, not depth) and only its depth column and λ·depth
  product moved. The delta/CVD study is unchanged: it reads trades and aggressor
  inference, and never touches depth.

  `cmd/ofistudy` carried its verdict as literal text — "~17% … a ~540x gap" — and so
  printed numbers its own table contradicted the moment the measurement changed. It
  now computes them from its results, and prints the slope columns the write-up
  needs so the table can be regenerated from the program rather than assembled by
  hand.

### Added

- **Tail latency for six more scenarios, and the mass cancel measured for the first
  time.** One scenario was published — a cancel-heavy mix — and it was the friendliest
  of the six. Its p99.9 is 958 ns; an aggressive sweep's is 6,875 ns with a p99.99 of
  31 µs, so **publishing only that one understated the tail by roughly 30×**. Each
  scenario now states its preload, reports out to p99.99, and covers the operations
  that were never measured: a thin-book walk, all five self-trade-prevention modes
  (all cheap), and the bulk cancel.

  The mass cancel is the one worth knowing: pulling one account's 5,000 resting orders
  takes **~872 µs at p50 and ~1.26 ms at p99**, on the matching goroutine, so nobody
  else's orders are processed for that whole time. The kill switch is a venue-wide
  pause proportional to the account's book. It scales, so a 100,000-order account is
  on the order of 18 ms.

  Methodology borrowed from `joaquinbejar/OrderBook-rs`, whose HDR bench suite does
  this properly: name the scenario, state the preload, report the upper quantiles.

- **Recovery time, which was claimed and never measured.** A 100,000-order book
  restarts in **~174 ms** from a checkpoint; a 10,000-record tail on top adds ~21 ms,
  at ~2.1 µs per record.

  That corrects a claim rather than confirming one. The package doc said recovery is
  "bounded to O(recent)", which is true of the *replay* and not of the restart:
  loading the snapshot is O(book) and dominates by an order of magnitude. Restart is
  O(book) + O(tail). The doc now says so.

  It also locates the cost: **reading the log is ~90% of replay** — 2.1 µs of 2.3 µs
  per record, at ~15 allocations per record, because records are JSON. A binary record
  format would not help write throughput, which is fsync-dominated, but it would cut
  restart time. That is a better-founded reason to consider one than the encode
  benchmark that first suggested it.

- **`marketdata.L2Feed`** — an incremental depth feed derived from the event stream:
  aggregated level changes, coalesced per command, with absolute quantities so a
  subscriber that misses one recovers on the next rather than staying wrong.

  This is where `matching.EventBookDelta` was heading, and the engine was the wrong
  place for it. L2 is a pure function of L3, the event stream is already tested to
  reconstruct the L3 book exactly, and computing deltas on the matching goroutine
  would add work to a path whose whole design goal is to allocate nothing and return.
  The constant stays declared but unemitted, with that reasoning attached, because
  removing a value from the middle of an iota block silently renumbers every kind
  after it.

  The test that matters compares the derived levels against the engine's own
  `Snapshot` after every command of a 3,000-command random tape — the two views
  cannot drift without it failing. It is also what found the depth bug above.

- **`EventTriggered` is now emitted**, on both paths where a conditional order fires:
  the cascade after a trade, and the immediate fire when a stop arrives already
  through its trigger. It precedes the `Accepted`, because a consumer seeing only the
  `Accepted` cannot tell a fired stop from an order a client just submitted, and "a
  stop fired" is the event a risk system actually wants.

## [0.13.0] - 2026-07-30

Completes the client's order lifecycle, makes the log trustworthy, and doubles
cancel throughput.

**The protocol.** A client can now express every order type the engine implements
and every operation it supports: six order types (stop, stop-limit, OCO, iceberg,
pegged, trailing joining limit and market), atomic cancel/replace, mass cancel, and
cancel-on-disconnect. Before this release the wire carried two of six order types and
had no replace, so a reprice meant two messages with the client naked in between.
Fourteen new message types, and the protocol stays at **version 2** — every golden
vector from a released version is byte-identical, which is the third dividend from
the type byte v0.11.0 spent a version freeze on.

**The log.** It was not recording five commands that mutate the book — every
conditional order type — so a stop entered after the last checkpoint did not survive
a restart. And it had no checksums at all: a record altered on disk but still valid
JSON was replayed as truth, silently. Both fixed, with the torn-tail and
media-corruption cases now treated differently on purpose.

**The speed.** Cancel went from ~47 ns to ~22 ns and the match path lost 27%, from
profiling this engine against a C++ one and two other Go books. Reading the wall
clock was 46% of the match path; a stop cascade ran over an empty stop book after
every match; and the order index was a Go map where a purpose-built one is 12×
faster. On the same machine and the same 200,000-order book, cancel is now ~3×
faster than liquibook's C++ book and at parity with `geseq/orderbook`.

**A note on how most of this was found.** Six of the ten fixes below came from
building the next feature and asking what it would be sitting on, not from testing
the feature itself: `QueryEnd.Seq` asserting a boundary the client had not reached,
two message types assigned the same byte, five commands missing from the log, an
accessor that raced. The comparison work in particular paid for itself twice — the
WAL checksum gap came from reading a Rust order book's dependency list.

### Performance

- **Cancel is ~2× faster: the order index is no longer a Go map.** A
  `map[int64]*node` was the largest single cost in a cancel. It is now a
  purpose-built table using Fibonacci (multiplicative) hashing over power-of-two
  buckets, with chained entries recycled through a free list.

  Go's map is general-purpose: it hashes arbitrary key types through a runtime
  call, keeps tophash bytes for probing, and handles concurrent-access detection
  and incremental growth. None of that is needed for a dense, monotonically
  increasing `int64` assigned by the book itself, in a structure already serialised
  by the book's own mutex. Measured in isolation, get + delete + put costs ~45 ns
  through Go's map and ~3.7 ns through this.

  Book-level cancel, measured by alternating baseline and patched builds in one
  session: **~47.7 ns → ~23.3 ns**. All ten patched measurements came in below all
  ten baseline ones.

- **A cancel no longer hashes an account string.** The per-account admission
  counter was a `map[string]int`, hashed on every add *and* every cancel. Accounts
  are now interned to a dense `int32` on first sight and the id is cached on the
  book node, so a cancel reads it off the node it already holds. An add still hashes
  once, which is unavoidable — the order arrives carrying a string.

  Prompted by measuring against `geseq/orderbook` (15–21 ns cancel) and
  `joaquinbejar/OrderBook-rs` (41 ns at a 1 M book). Two independent libraries
  beating this one at the same operation was the signal; both did it with a
  purpose-built index rather than the language's map.

### Added

- **Atomic cancel/replace.** `ReplaceOrder` (`Z`) cancels a resting order and enters
  another in one command. Without it a reprice is two messages and the client is naked
  between them: if the connection dies in the gap it cannot tell whether it holds zero
  orders or one, and another participant can take the price meanwhile.

  Priority is forfeited by design — an order that could reprice in place would let a
  participant reserve a place in line — so `Reduce` remains the way to shrink at the
  same price. There is no new outbound message: a successful replace is the `Canceled`
  for the old id followed by the `Accepted` for the new one.

  The failure semantics are asymmetric and deliberate. If the original cannot be
  cancelled, **the replacement is not entered** and the refusal names the original id;
  a client replacing an order it no longer holds did not ask to open a new position.
  If the cancel succeeds and the replacement is then refused, the client holds neither
  and is told so within the same command — reported rather than discovered, which is
  what the two-message sequence could not offer. Restoring the original instead would
  hand it back at the tail of the queue without saying so, and a silent loss of
  priority is worse than a reported refusal.

  It is subject to the minimum resting time, like cancel and reduce: it withdraws
  displayed size, and a verb that escaped the floor would leave the anti-spoofing
  control guarding two routes out of three.

- `Engine.Replace`, `Runner.TryReplaceAsync`, `wal.KindReplace` and
  `CommandLog.AppendReplace`. The log records a replace as **one** record rather than a
  cancel plus a submit, so replay cannot apply half of it.

- **Conditional orders on the wire: stop, stop-limit, OCO, iceberg, pegged and
  trailing.** The engine has supported all of them since v0.5.0 and the protocol
  could express none: a client could place a limit or a market order and nothing
  else, so four of the six order types the engine implements were reachable only by
  an embedder calling it in-process. Same shape of gap as `Reduce` before v0.12.0 —
  real, tested capability with no way for a client to ask for it.

  Five messages (`EnterStop` `S`, `EnterOCO` `O`, `EnterIceberg` `I`, `EnterPegged`
  `P`, `EnterTrailing` `W`), each carrying the same 56-byte base-order block as
  `Enter`'s body plus its own parameters, rather than one message with a union of
  fields — three meaningless fields per message is what the v0.11.0 audit spent its
  time removing.

  Design points worth stating: an OCO's stop leg inherits symbol, side, quantity and
  TIF from the primary, so legs of differing size (which would leave a residual
  position behind whichever fired) are not expressible; a pegged order must send
  price 0 and is refused otherwise, rather than having a price it supplied silently
  overwritten; and an iceberg carries no jitter field, because reload-size jitter is
  set from the engine's own configuration and a client value would be decoded and
  discarded — the exact `Symbol` bug from v0.10.0.

- `Runner.TryEnqueueStop` / `TryEnqueueOCO` / `TryEnqueueIceberg` /
  `TryEnqueuePegged` / `TryEnqueueTrailing`, and `Runner.TrailingStopCount`. The
  conditional types had synchronous entry points only, which a network ingress must
  not use: those hand back the engine-owned order.

- **Mass cancel on the wire.** `MassCancel` / `MassCancelAck` (`F` / `G`).
  `Engine.CancelAllForUser` has existed since v0.9.0 with no way for a client to
  invoke it, which is the difference between a venue you can test against and one you
  would quote on. Each removed order still produces its own `Canceled`; the ack
  follows them and carries the count, so a completed sweep of zero is distinguishable
  from a connection that died mid-sweep.

- **Cancel-on-disconnect.** `CancelOnDisconnect` / `CODAck` (`B` / `V`), a message
  rather than a `LoginRequest` field because adding a field there would move every
  byte after it and invalidate a committed vector. Acknowledged explicitly: a client
  must never be guessing about a control that decides whether its book survives.

  A **venue** shutdown deliberately does not fire it. A graceful shutdown drops every
  connection at once, and sweeping there would journal a cancel for every order of
  every enabled session, permanently destroying books that are meant to come back
  after the restart. There is a test for exactly that.

- `matching.Runner.TryCancelAllAsync`, the ingress-shaped path for the sweep: the
  enqueue happens on the caller's goroutine so it cannot overtake an order submitted
  just before it, while the wait moves off that goroutine.

Four more message types, still **no version bump** — every pre-existing golden
vector is byte-identical. `MassCancel` shares `Query`'s exact layout and
`MassCancelAck` shares `QueryEnd`'s, separated by nothing but the type byte, which is
the third time that has paid for itself.

### Fixed

- **Two message types were each assigned twice.** Adding the conditional-order
  messages gave `'O'` to both `EnterOCO` and `OpenOrder`, and `'P'` to both
  `EnterPegged` and `Replaced`. Nothing failed, because the payload widths differed and
  `checkHeader` tests width first — which means those messages were being told apart by
  *length*, the exact v1 dispatch the type byte was introduced to replace. A collision
  between two messages of equal width would have silently decoded one as the other.

  `EnterOCO` is now `'N'` and `EnterPegged` `'Y'`, and
  `TestMessageTypesAreDistinct` asserts every `Msg*` constant is unique across both
  directions so this cannot come back. Two golden vectors changed as a result — both
  added earlier in this same unreleased cycle, so no released client has seen them.

- **The conditional order types were never written to the log.** `cmdStop`,
  `cmdOCO`, `cmdIceberg`, `cmdPegged` and `cmdTrailing` all mutate the book and none
  of them reached `CommandLog`, so a stop or iceberg entered after the last
  checkpoint did not survive a restart. Snapshots captured them (v0.9.0 fixed that);
  the log tail did not.

  Invisible while those commands were reachable only in-process, and about to become
  client-facing. `CommandLog` gains five methods, the log five entry kinds, and
  replay reconstructs each wrapper from its recorded parameters. The `countingLog`
  test double implements the whole interface deliberately, so adding a mutating
  command without logging it fails to compile — which is how this was caught.

- **`Engine.TrailingStopCount` was unsafe to call from another goroutine**, and a
  `Runner` accessor briefly exposed it as if it were not. The other read accessors
  delegate to the book and stop book, which carry their own locks; trailing stops
  live in a plain map owned by the matching goroutine. `Runner.TrailingStopCount`
  now goes through the command queue, and the engine method documents its
  single-writer requirement. The race detector caught it.

- **`QueryEnd.Seq` asserted a boundary the client had not reached.** Draining the
  publisher only moves events into the account's *stream*; the connection receives
  them from a separate polling goroutine, so a report written straight to the
  outbound queue could overtake stream messages it claimed to come after. A client
  applying them in arrival order would apply the same execution twice — the exact
  failure the drain was introduced to prevent.

  Found while building the mass-cancel ack, whose test asserted the ordering
  directly and failed. Both paths now wait until the connection has actually queued
  everything through the reported sequence.

- **The write-ahead log had no checksums, so silent corruption was replayed as
  truth.** `ReadAll` stopped at a record that failed to *parse*, which meant a
  record altered on disk but still valid JSON — a flipped digit inside a price —
  was handed to the engine and booked. The recovered venue was wrong about what
  had happened and nothing anywhere said so.

  Records now carry a CRC-32C, and the file carries a magic header so the framing
  is identifiable rather than guessed. The two failure modes are deliberately
  separated: a **torn tail** (a short final record from a crash mid-write) still
  stops cleanly, because that command was never acknowledged, while a **complete
  record whose checksum disagrees** is refused with `ErrCorrupt`. Stopping quietly
  on the second would be indistinguishable from a clean end of log while discarding
  every acknowledged command after it. A venue that cannot trust its log should
  refuse to start, not serve a book that does not match it.

- **A corrupt length prefix could ask recovery for a 4 GiB allocation.** The
  4-byte record length was read off disk and passed straight to `make([]byte, n)`,
  so one flipped bit requested up to 4 GiB at the moment a venue can least afford
  it. Bounded by `MaxRecordBytes` (8 MiB, far beyond any real record), and a zero
  length is refused rather than looping.

  Logs written before this change have no header and are still read — without
  verification, since there is nothing to verify against — and appending to one
  keeps its original framing rather than switching format mid-file. Rotate to get
  checksums on an existing log; `Writer.Checksummed` reports which framing is in
  use.

  Found by reading `joaquinbejar/OrderBook-rs`, which checksums its journal.

- **Published benchmark figures that did not reproduce.** Re-measured every number
  in [docs/BENCHMARKS.md](docs/BENCHMARKS.md) and the README on the stated hardware
  (median of 5 runs, idle machine, `go1.23.5`), and corrected what disagreed. The
  engine did not change; the documentation was wrong, in both directions.

  - `OrderBook_Cancel` was published at 253 ns. Five runs gave 265–301 ns, median
    273 — the old figure is outside the measured range. Corrected, and then
    corrected again for a bigger reason: see the next item.
  - `Runner.Process` was published at **4 allocs/op**; it is **3**. Checked against
    the v0.11.0 tag to confirm this was a recording error rather than a change.
  - "Group commit costs roughly an order of magnitude" — it costs **~30×**
    (18,260 ns against 613 ns). "Syncing every command costs three further orders
    of magnitude" — it costs **~210×**, not ~1000×.
  - "`Checkpoint` is on the order of a millisecond" — it is **~0.3 ms** over a
    5,000-order book.
  - Tail latency p999 was published at 292 ns; five of six runs give **250 ns**.
    The "p999 within ~3.5× of the median" claim becomes ~3×.
  - Several figures were *conservative* rather than wrong (`BestBid`, `MatchInto`,
    `Process`) and have been brought to their measured medians too, so the table is
    internally consistent rather than a mix of vintages.

- **The cancel benchmark's book size was an undisclosed parameter.**
  `OrderBook_Cancel` inserts `b.N` orders and cancels all of them, so `b.N` is also
  the book depth — and Go picks `b.N` by wall-clock. The published figure was
  therefore a **ten-million-order book**, a depth no real symbol reaches, and it
  understated the engine by 4×: cancel is ~65 ns at 200,000 resting orders and
  ~273 ns at 10 M. Both are now published, with the depth stated, plus the scaling
  curve between them. `OrderBook_Add` has the same property (92 ns at 200 K, 206 ns
  at 10 M); `CancelReplace` and `LevelChurn` do not, because their working sets are
  fixed, which is why those two barely move.

  Stated rather than quietly corrected, because it generalises: for any book-level
  benchmark the book size is part of the result, and a figure quoted without it is
  not comparable to anything.

- **"0 allocs/op" was being reported as stronger than it is.** Go computes that
  column by integer division, so anything under 1.0 prints as `0` — which is how
  `OrderBook_Cancel` could publish `0 allocs/op` and `41 B/op` on the same line
  without the contradiction being visible.

  Measured directly against `runtime.MemStats`, cancel allocates **0.0002**
  objects/op and market-maker churn **0.009**, so the claim holds in substance —
  but `Add` into a growing book allocates **1.05**, which is what "pooled" means
  rather than "allocation-free". All three are now asserted by tests in
  `pkg/orderbook/alloc_test.go`, including a floor on `Add`, so the claim can fail
  instead of being a rounded-down column.

- **A scope note that had gone stale.** Both the README and BENCHMARKS said the
  figures exclude "any session or order-entry protocol — none of which exist in
  this repository". Those layers have existed since v0.10.0 (`internal/wire`,
  `pkg/orderentry`, `cmd/obgw`). They are still unmeasured, which was the real
  point, and it is now stated that way.

- `README.md` linked no protocol documentation, and described the release history
  as "v0.1.0 → v0.8.0".

### Changed

- **Breaking (embedders):** `matching.CommandLog` gained six methods —
  `AppendReduce`, `AppendCancelAll`, `AppendReplace`, `AppendStop`, `AppendOCO`,
  `AppendIceberg`, `AppendPegged` and `AppendTrailing`. `pkg/wal.Writer` implements
  all of them; a custom log will not compile until it does, which is the intended
  outcome — silently continuing to drop those commands is the bug being fixed.
- **Breaking (embedders):** `Engine.Reduce` and `Runner.Reduce` now fail with
  `ErrCancelTooSoon` inside a configured `MinRestingTime`, as does the new
  `Engine.Replace`. Venues that leave the floor at zero — the default — see no change.
- `Engine.TrailingStopCount` is documented as single-writer only; use
  `Runner.TrailingStopCount` from any other goroutine.

## [0.12.0] - 2026-07-30

Completes the client's side of the order lifecycle, and repairs three things found
underneath it.

v0.11.0 spent a version freeze on a message-type byte. This release is what that
bought: four new message types, no bump, every pre-existing golden vector
byte-identical. `Query` / `OpenOrder` / `QueryEnd` give a client a way back to a
correct picture in-protocol, and `Reduce` lets it shrink an order without going to
the back of the queue — a capability the engine has had since v0.10.0 and no client
could ask for.

The three repairs are the more interesting half, and all three were found by asking
what the new message would be sitting on rather than whether it worked. The command
log was not recording two commands that mutate the book. The anti-spoofing floor
guarded `Cancel` and not `Reduce`, which would have handed the Coscia pattern to
every authenticated client the moment the wire carried a reduce. And recovery
rebuilt the book without the index over it, so a restart left recovered orders
unreachable and — quietly, with nothing logged anywhere — stopped reporting their
fills to the makers who owned them.

### Fixed

- **Two mutating commands were never written to the log.** `Reduce` and
  `CancelAllForUser` both change the book, and `CommandLog` recorded only submits
  and cancels — so recovery restored a reduced order at its original size, and
  handed a pulled account its whole book back. Neither failed loudly; the
  recovered venue was simply wrong about what was resting.

  `KindReduce` and `KindCancelAll` now exist in the log, and the interface
  documents the rule the omission broke: a mutating command missing from
  `CommandLog` is not "not yet logged", it is a book the log cannot reproduce.
  `CancelAllForUser` logs the *intent* rather than the ids it removed, which is
  what makes replay correct — the log is written before the sweep, so the same
  point in the command stream holds the same book and removes the same set.

- **Recovery restored the book and nothing else, so recovered orders were
  unreachable — and their fills went unreported.** The session layer's `ClOrdID` →
  order-id index started empty against a non-empty venue. A client could see its
  recovered orders in a `Query` reply and be told `2` (unknown order) for every
  `Cancel` or `Reduce` naming one, which are contradictory answers from the same
  venue about the same order.

  The severe consequence was quieter. With no record of the order, `publishTrade`
  found nothing to attribute a fill to and dropped the execution report entirely: a
  maker whose resting order filled while the venue was down would never have been
  told, and its position would have been wrong with no way to notice. That is
  precisely the failure the stream-outliving-the-connection design exists to
  prevent, reintroduced through recovery.

  `Registry.Adopt` and `Engine.RestingOrders` now rebuild the index on start.
  Adoption seeds from `RemainingQty`, not `Quantity` — a recovered order can be
  partly filled and `fill()` decrements from that number, so the original size would
  over-report `LeavesQty` by exactly what had already traded. It delivers nothing to
  any stream: those orders were acknowledged in a previous incarnation, and
  replaying them into a fresh sequence space would be inventing history.

- **`Reduce` bypassed the minimum resting time.** `Cancel` enforces it; `Reduce`
  did not. The control targets the Coscia pattern — post size, pull it before it
  can fill — and a reduce from 1000 lots to 1 withdraws 999 of them, so the whole
  pattern was available behind a different verb. That was nearly harmless while
  only an embedder could call `Reduce`; shipping it on the wire would have handed
  it to every authenticated client. Now refused with `ErrCancelTooSoon`, with the
  same exemptions `Cancel` has (replay, and privileged liquidation orders).

  **Behaviour change for embedders:** `Engine.Reduce` and `Runner.Reduce` now fail
  with `ErrCancelTooSoon` inside a configured `MinRestingTime`. Venues that leave
  the floor at zero — the default — see no change.

### Added

- **Reduce over the wire.** `Engine.Reduce` and the outbound `Replaced` that
  reports it both shipped in v0.10.0, but no inbound message could ever
  ask for one: a client's only route to a smaller order was cancel-then-new, which
  sends it to the back of its price level. That is the exact cost `Engine.Reduce`
  exists to avoid, and the capability was unreachable from the only place a client
  can speak.

  `Quantity` is the new **total**, not a delta. A delta cannot be made safe
  against a concurrent fill: the two sides would be subtracting from different
  numbers, and the result would depend on which the venue believed.

  Unlike a cancel, a refused reduce is reported. It fails for reasons the client
  caused and can correct — asking to grow, or to shrink below what is already
  filled — and silence is indistinguishable from a reduce still in flight. Zero is
  refused rather than treated as a cancel, because one message with two meanings
  is how a client ends up cancelling an order it meant to trim.

- `matching.Runner.TryReduceAsync`, which is the shape a network ingress needs and
  neither existing path had: the enqueue happens on the caller's goroutine so a
  reduce cannot overtake the order it names, while only the wait for the outcome
  moves off it so the matcher can never stall a connection's ingress. Only the
  error crosses the channel — the applied order is engine-owned.

- `ReasonInvalidQuantity` (16). Distinct from `Malformed` (14): malformed means the
  venue would not look at the message, this means it looked at a real order and the
  size asked for is not one it can take.

- `ReasonTooSoon` (17), the one refusal in the vocabulary a client should simply
  retry.

- `Engine.RestingOrders`, returning deep copies of every active resting order
  across all accounts. `OpenOrdersFor` could not serve recovery: it is scoped to one
  account, and a recovering venue does not know the accounts until it has read the
  book.

- `orderentry.Registry.Adopt`, which rebuilds the client-order-id index over a
  recovered book.

- **In-band reconciliation.** A `Query` message returns one `OpenOrder` per live
  order followed by a `QueryEnd`. Resume can legitimately fail — an evicted
  cursor or a restarted venue — and a client refused at login previously had no
  in-protocol way back to a correct picture; "reconcile out of band" is telling
  someone to build a second integration.

  The report is read from the book on the matching goroutine, and the publisher
  is drained before it is written, so every event up to that instant has already
  reached the client. `QueryEnd.Seq` names that point: everything after it is a
  change to apply on top. Reading the book without draining first would let an
  execution from before the read arrive after the report, and the client would
  apply it twice.

  `QueryEnd.Count` exists so a truncated report cannot look like a complete one —
  otherwise "you have nothing open" and "the connection died mid-report" are
  indistinguishable.

- `Engine.OpenOrdersFor` / `Runner.OpenOrdersFor`, returning deep copies read on
  the matching goroutine.

### Changed

- **Breaking (embedders):** `matching.CommandLog` gained `AppendReduce` and
  `AppendCancelAll`. `pkg/wal.Writer` implements both; a custom log will not
  compile until it does, which is the intended outcome — silently continuing to
  drop those commands is the bug being fixed.

### On the version

Four new message types this cycle, and **no version bump** — which is what the
type byte introduced in v0.11.0 bought. Every pre-existing golden vector is
byte-identical, and a test pins the eight original payload widths against values
derived by hand.

`Reduce` is the sharpest demonstration: it encodes to the same 30 bytes as
`Replaced`, with the same field at the same offset, and the two vectors differ in
exactly one byte — the type. Under v1's length-based dispatch they could not have
coexisted at all.

The protocol stays at **version 2**.

## [0.11.0] - 2026-07-29

A hostile re-read of v0.10.0, and the repairs. Everything below was found by
reviewing the new code the way the original critique reviewed the old — looking
for fields that exist but are never checked, constants declared and never used,
and claims the code does not back.

### Fixed

- **The protocol had no message type.** Eight `Msg*` constants were declared and
  never used; the server told Enter from Cancel by payload length. Any future
  message sharing a length with an existing one would have been silently misread
  as it, and a dead `switch payload[0]` block carried a comment describing a type
  byte that did not exist. Every payload now leads with an explicit type, both
  ends verify it, and the protocol is at **version 2** — which is what spending a
  version freeze is supposed to look like.
- **`Symbol` was decoded and thrown away.** The gateway built every order with
  its own configured instrument and never looked at the one the client sent, so
  an order naming any symbol was booked here. Now refused.
- **The reference server had no durability.** The library ships a write-ahead
  log, a checkpoint and a documented seam, and `cmd/obgw` wired up none of it —
  the one artifact showing people how to use this demonstrated running without
  it. Now `-wal`, `-snapshot` and `-checkpoint`, with group commit, recovery on
  start, and a startup warning when durability is off.
- **`NewServer` recovered from disk and discarded the result**, building its
  runner from a bare config. Added `matching.NewRunnerFor`, which takes an
  already-recovered engine, because `NewRunner` silently starting empty is the
  trap that caused it.
- **No timeouts anywhere.** No read deadline, no idle timeout, and
  `PacketServerHeartbt` was declared and never sent. Connect and say nothing and
  a goroutine, a buffer and a stream lived forever. Now a 10s unauthenticated
  login deadline, a 30s authenticated idle timeout refreshed by any packet, and a
  5s server heartbeat so a client can tell a quiet venue from a dead one.
- **`Publisher.Close` deadlocked when `Pump` had never run**, waiting forever on
  a goroutine that did not exist. A publisher built and closed without serving —
  an aborted startup — hung the process.
- **A typed-nil `CommandLog` segfaulted the matcher.** Assigning a nil
  `*wal.Writer` to the interface field yields a non-nil interface holding a nil
  pointer, so the `!= nil` guard passed and the first command dereferenced nil.
  Fixed at the call site and documented on the field, since the API invites it.

### Added

- `matching.NewRunnerFor` and `Engine.SetEventSink`. The latter exists because
  recovery must replay with no sink attached — otherwise restarting republishes a
  lifetime of historical executions at whoever connects next.
- Tests for every item above, plus one asserting the reason-code vocabulary
  defined in `internal/wire` and `pkg/orderentry` still agrees. It was duplicated
  across two packages with nothing checking it.

### Changed

- `docs/PROTOCOL.md` documents v2, the timeout and heartbeat regime, and the
  durability flags. It also states plainly that the golden vectors were generated
  by running the encoder: they prove the layout has not changed *accidentally*,
  which is a real job, but they do not prove it is correct. A ratchet, not a
  specification.

## [0.10.0] - 2026-07-29

Adds the network edge. v0.9.0 fixed what was broken; this release adds what was
absent — a client-facing order-entry protocol, a session layer, per-account
message sequencing with gap-free resume, and a reference gateway that speaks it
over TCP.

With this, *production-grade embeddable matching core with a demonstrated network
seam* is a claim the tests support. Both qualifiers are load-bearing:
production-grade describes the core, embeddable concedes the venue is not here.

### Added

- **`cmd/obgw`** — a reference TCP order-entry gateway. Authentication defaults to
  deny, so an unconfigured venue rejects everyone rather than admitting them.
  One goroutine per connection, admission control before enqueue, drain on
  SIGTERM.
- **`internal/wire`** — SoupBinTCP 3.00 framing carrying fixed-width big-endian
  payloads, no new dependencies. Frozen by 12 byte-exact golden vectors: a field
  that moves silently reinterprets every message a deployed client sends, so
  changing a layout means bumping `Version`, not editing a vector. Documented in
  [docs/PROTOCOL.md](docs/PROTOCOL.md).
- **`pkg/orderentry`** — per-account outbound streams that outlive connections.
  A `Session` is a socket; a `Stream` is an account's sequence. That separation
  is what makes resume possible: a maker whose resting order fills while its
  connection is down still has the execution waiting when it returns.
- **Gap-free resume**, scoped to a venue incarnation. A restart mints a new
  incarnation id, so a stale cursor is refused rather than served different
  content under numbers the client believes it already has. A cursor older than
  the retention ring is refused explicitly; the client is never told it is up to
  date when it is not.
- **`Engine.Reduce` / `Runner.Reduce`** — in-place down-size retaining queue
  position. The one order-entry operation a gateway provably cannot build
  outside the writer goroutine: cancel-then-new sends the order to the back of
  its level, which for a market maker managing size is a material loss. Size
  increases and price changes are rejected rather than silently reinterpreted,
  because an order that could grow in place would let a participant reserve a
  spot in the queue.
- **`matching.MultiSink`** and **`Runner.TryEnqueue` / `TryEnqueueCancel`**.
  Fire-and-forget submission never hands the engine-owned order back to a
  connection goroutine, which removes the whole read-after-submit race class.
- **`Gateway.Allow`** splits the admission decision from forwarding, so the gate
  can sit in front of `TryEnqueue`.

### Security

- The wire carries **no account and no engine order id**. Orders are named only
  by the client's own `ClOrdID`, scoped to the authenticated session. The engine
  cancels by `(orderID, userID)` and self-trade prevention lets one account
  observe another's resting orders, so a wire carrying either field would let a
  client name an order it does not own — there is no field in which to express
  it. Two accounts using the identical `ClOrdID` cannot reach each other's
  orders, and a test asserts it.
- `STPMode` and the privileged flag are absent from the wire: the first is venue
  policy, the second a liquidation capability that must never be client-settable.
- A reduce or cancel against another account's order is refused
  indistinguishably from a missing order, so a probe cannot learn that someone
  else's order exists.

### Fixed

- `Server.Close` deadlocked against its own connection handlers. Closing a
  listener stops new accepts and does nothing to established sockets, so the
  drain waited forever on handlers parked in a read. Found by the integration
  test on its first run, which is the reason the reference server exists.

### Changed

- README now claims *production-grade embeddable matching core with a
  demonstrated network seam*, and states plainly what is still yours: TLS,
  credential storage, multi-symbol routing, clearing, and any HA topology. The
  library ships the seams for primary-backup and deliberately not the consensus.
- **Production-readiness remains a property of a deployment, not of a library.**
  That sentence stays in the README permanently.

## [0.9.0] - 2026-07-29

A correctness release. A production-readiness audit of the recovery, event and
concurrency paths found that several controls the documentation marked as
shipped did not actually hold, and one execution path lost fills outright. Each
item below was reproduced with a failing test before it was fixed.

The headline is uncomfortable and worth stating plainly: the parts most likely
to hurt an embedder were not the missing pieces the README already disclosed,
but the pieces it claimed were finished.

### Fixed

- **Lost executions on OCO entry.** `ProcessOCO` called `submitStopInto` and
  discarded all three return values. A stop leg triggering on entry still
  settled through the book — filling and removing real makers and moving the
  last trade price — while reaching neither the event stream nor any
  `MatchResult`. The counterparty's fill was gone permanently. The same discard
  meant the stop leg was never announced, so its later fills referenced an order
  id no consumer had seen.
- **The event stream did not reconstruct the book**, despite `event.go` saying
  it did. Eight distinct defects: market and IOC remainders announced as
  `ACCEPTED` and terminated with no delete; iceberg refills re-adding a slice
  under the same id silently, so the owner of a live iceberg went dark after its
  first slice; the surviving OCO leg removed silently; STP `CANCEL_OLDEST` and
  `CANCEL_BOTH` removing the maker with no delete; STP `DECREMENT` shrinking
  both sides with no trade and no event at all; cascade-fired stops settling
  without ever being announced; and a rejected FOK publishing trades that
  `reverseTrade` had already undone. Emission is now composed in causal order
  per command, with sequence numbers assigned at publish time — numbering at
  record time produced a stream whose `Seq` ran backwards.
- **Snapshot restore was lossy in five ways.** Trailing stops vanished outright
  (they live only in the engine's map, never in either book); icebergs came back
  as a bare displayed slice with the reserve gone; OCO pairings were lost,
  leaving two independent orders either of which could fill; `markPrice` reset
  to zero, and both manipulation clamps skip when the current mark is zero, so
  the first post-recovery update was unconstrained; and the self-output
  guardrail's window reset, handing back a full budget immediately after a
  restart.
- **The client-order-id duplicate guard was empty after recovery**, on both the
  snapshot and the replay path. It stopped enforcing precisely when a client is
  most likely to resend — after the venue restarts — which is the FIX PossDup
  case it exists to cover.
- **Snapshot and log could not be joined.** `EngineSnapshot` carried no log
  position, and `INTEGRATION.md` told callers to replay entries after `Seq` —
  the engine's *order* sequence, unrelated to log positions. Following the
  documented recipe replayed an arbitrary slice of the log.
- **Replay bypassed the deterministic admission checks**, including the int64
  notional overflow guard. Because the log is write-ahead it records commands as
  submitted, not accepted, so an order the live engine rejected rested on the
  recovered book.
- **`WriteSnapshot` was atomic but not durable** — no fsync of either the file
  or the parent directory, so a crash could leave a correctly-named empty
  snapshot that `Recover` would load.
- **`Runner.Close` panicked in-flight producers** by closing the shared command
  queue from the consumer side. Correct shutdown required proving every producer
  had stopped, which a server with a goroutine per connection cannot do.
- **`pkg/gateway` data-raced** at the topology its own package doc recommended.

### Added

- `wal.Checkpoint`, `wal.Recover` and `wal.RestoreAfter` — the snapshot/log join
  expressed once, in code, instead of as prose for callers to reimplement.
- `Runner.Checkpoint` and `RunnerConfig.Log`: a write-ahead seam that logs each
  mutating command before applying it, tracking the sequence of the last command
  *applied* rather than appended.
- `Engine.CancelAllForUser` / `Runner.CancelAllForUser` — the operator kill
  switch. It pulls resting orders, pending stops and trailing stops, ignores
  `MinRestingTime`, and announces every removal.
- `matching.ErrShuttingDown` and an idempotent, fenced `Runner.Close`.
- `EventReplaced` now carries in-place size changes that keep queue position.
- Crash-recovery property test over a 2,000-command generated tape, replacing a
  determinism gate that ran on six hand-written orders.
- `TestEventStreamReconstructsBook`: 22 scenarios asserting the event stream
  rebuilds the engine's own L3 book, order-for-order and lot-for-lot.
- Benchmarks for the durable path (`Runner` + `EventSink` + WAL), which had no
  published number while the README advertised the bare engine's.

### Changed

- **Docs corrected where they overclaimed.** `THREAT-MODEL.md` row 16 no longer
  marks the taker speed bump as an enforcing control — it is an observation hook
  that reports and does not delay. The README no longer implies the
  zero-allocation figure applies to the concurrent API: `Match` into a caller
  buffer allocates nothing, `Runner.Process` allocates 4/op, and durability adds
  roughly an order of magnitude.

## [0.8.0] - 2026-07-26

Completes the microstructure research agenda. All four roadmap items are now
implemented, measured, and written up in [docs/research/](docs/research) — and
most of the popular claims they test do not survive contact with ground truth.

### Added

- **Order-flow study** (research-roadmap.md §4), completing the research agenda:
  `signals.CVD`, `signals.TickRuleSide` / `signals.LeeReadySide` (aggressor
  inference, there to be measured against ground truth rather than used in place
  of it), `signals.AbsorptionConfig`, `signals.Divergence`, and
  `signals.WilsonInterval`; `study.RunInference`, `study.RunDivergence` (with a
  price-only control arm), `study.RunAbsorption`, `study.RunSqueezeDemo`, and
  `study.PoolSignals`; `cmd/flowstudy` runs them all. Write-up:
  [order-flow.md](docs/research/order-flow.md) — a 94.5%-accurate tick rule
  builds a CVD wrong by 169% (sometimes with the opposite sign); CVD divergence
  beats its base rate but loses to a price-only control, so the CVD half adds
  nothing; absorption predicts nothing, though the mechanism is demonstrably
  real.

## [0.7.0] - 2026-07-26

The research release: the microstructure agenda's first two items measured,
written up, and checked in — plus an honest account of what data the harness
runs on.

### Added

- **Kyle's λ price-impact study** (research-roadmap.md §2): `signals.SignedFlow`
  and `signals.EstimateLambda` (fits `ΔP = λ·y`, reporting λ with R² and N);
  `study.RunKyleLambda` (the λ that emerges from a real book, with none
  configured), `study.RunKyleDepth` (λ ∝ 1/depth sweep), `study.RunExecution`
  (block vs sliced execution, scored on implementation shortfall plus realized
  and permanent impact), and `study.RunLambdaCalibration` (estimator validation);
  `cmd/lambdastudy` runs them all.
- **`docs/research/`**, the results directory the methodology has always
  required, with two write-ups.
  [kyle-lambda.md](docs/research/kyle-lambda.md): λ ∝ 1/depth holds, slicing is
  ~8% cheaper per lot and completes where a block leaves ~8% unfilled, and
  permanent impact is unchanged either way — slicing buys back the temporary
  component, not the permanent one. [ofi.md](docs/research/ofi.md): the §1
  result written up properly over ten seeds, including that the predictive slope
  is negative in nine of them.
- **`docs/research-roadmap.md` §0 "Data and scope"**: what the engine emits
  (per-order L3/MBO event streams), what simulator ground truth adds, what is
  missing (real capture is L2-only), and why "L4" is a vendor label rather than
  an exchange tier.

### Fixed

- **Overstated OFI figure in the docs.** `LEARN.md` claimed a contemporaneous
  R² ≈ 0.33 and `cmd/ofistudy` called it "~a third" of the same-interval move.
  The measured mean across ten seeds is **0.1685** (range 0.0704–0.2397); the
  three seeds the CLI printed were the joint highest of the first ten. Both
  numbers corrected and the CLI widened to ten seeds. The verdict is unchanged —
  predictive R² was always ~0.0003.

## [0.6.0] - 2026-07-23

The market-integrity release: a research-grounded threat model
([docs/THREAT-MODEL.md](docs/THREAT-MODEL.md)) and the defenses it prioritized,
plus durable persistence.

### Added

- **Durable WAL** (`pkg/wal`): append-only, length-prefixed command log written
  write-ahead, torn-tail-safe `Restore` into a fresh engine, and atomic snapshot
  write/read — the LMAX/Binance journal-plus-snapshot recovery model.
- **Threat model** (`docs/THREAT-MODEL.md`): a 27-attack catalogue, top-ten deep
  dives, a what-we-defend inventory, and a prioritized roadmap — every entry tied
  to a real enforcement action or incident.
- **In-core pre-trade risk & anti-manipulation controls** (`matching.Config`,
  opt-in, cold-path, `Privileged`-exempt, bypassed on deterministic replay):
  `MaxOrderQty` / `MaxOrderNotional` (fat-finger) and `MinOrderQty` /
  `MinOrderNotional` (dust) caps with an int64 notional-overflow guard;
  `MaxOrdersPerAccount`; `MinRestingTime` (anti-spoofing); `DedupClientOrderIDs`
  (idempotency); `MaxMarkStep` **and** `MinMarkDepth` / `MarkDepthBand` (anti
  oracle-pump — single jump and patient drag); `MaxForceTradeQty` (chunked
  liquidation); `BandBreachPause` (timed halt + auto-resume); `IcebergPeakJitter`
  (anti-sniffing). `HALTED` / `RESUMED` events on guardrail trips and pauses.
- **Surveillance detectors** (`pkg/surveillance`, alert-only): `OTRDetector`
  (order-to-trade ratio), `CloseMarkingDetector`, `RampingDetector`,
  `PingingDetector`, and `CrossBookMonitor` (cross-product correlation), alongside
  the existing spoof and rate detectors.
- **Call-auction session** (`pkg/auction`): `AuctionSession` for the open, close,
  and halt recovery, with a replay-safe `RandomizedClose` that defeats marking the
  close.
- **Edge gateway** (`pkg/gateway`): an enforcing token-bucket `RateGate` (rejects
  over-quota orders; cancels never gated) and an asymmetric taker speed bump, with
  `examples/gateway` demonstrating them plus a CAT-style audit export.
- `OrderBook.OrdersByUser` and `OrderBook.DepthWithin`; `Engine.SetReplaying`.
- Docs: `docs/INTEGRATION.md` "Market integrity & pre-trade risk" section; every
  new knob in `docs/CONFIG.md`; refreshed `docs/SPEC.md` package layout and
  market-integrity section; README highlights and docs table.

### Changed

- **BREAKING:** `Engine.SetMarkPrice(price int64)` now returns `error` (it rejects
  a mark update that violates `MaxMarkStep` / `MinMarkDepth`, or a negative price).
  `Runner.SetMarkPrice` is unchanged (async, fire-and-forget).
- `EngineSnapshot` gained `PausedUntil` so a mid-pause snapshot restores exactly.

## [0.5.0] - 2026-07-23

Phase C — real-world features. Self-trade prevention with taker-decides,
`DECREMENT` mode, cross-account `TradeGroupID`, and a `Privileged` exemption; a
mark/index-driven price band (`SetMarkPrice`) plus a `ForceTrade` liquidation/ADL
primitive; a per-symbol `Shards` router; an event-stream adapter example
(`examples/eventfeed`); and a uniform-price batch-auction mode (`auction.BatchAuction`).

## [0.4.0] - 2026-07-22

Determinism & integration seam. **Phase A:** an injectable `Clock` (byte-identical
replay), replay-equivalence and zero-allocation CI gates, feature-flagged exotic
order types (`DisabledClasses`), degraded states (`Open` / `CancelOnly` / `Halted`),
and a self-output `Guardrail`. **Phase B:** a monotonic `Event.Seq` + typed
`EventSink` event stream, `TakeSnapshot` / `RestoreEngine`, and bounded
backpressure (`TrySubmit` → `ErrQueueFull`).

## [0.3.0] - 2026-07-22

Production-grade low-µs core (P0–P6). O(1) cancel via intrusive linked lists, a
zero-allocation `Match` path (pooled nodes/levels + caller trade buffer), and a
single-writer `Runner` (MPSC command queue, lock-free hot path). Tail-latency,
fuzz, soak, and WAL-replay-recovery suites.

## [0.2.0] - 2026-07-22

**BREAKING:** integer-exact pricing. Prices are `int64` ticks and quantities
`int64` lots; a per-symbol `Instrument` converts decimals only at the boundary.
Engine-assigned monotonic `int64` ids replace UUIDs.

## [0.1.0] - 2026-07-21

Initial release: a decimal-first CLOB and matching engine with the full order
surface (limit, market, stop/stop-limit, iceberg, post-only, pegged, OCO,
trailing), GTC/IOC/FOK, self-trade prevention, a price-band circuit breaker, FIFO
and pro-rata allocation, L1/L2/L3 market data, a surveillance starter kit, and a
market-microstructure research harness with a WebAssembly demo.

[Unreleased]: https://github.com/intrepidkarthi/orderbook/compare/v0.8.0...HEAD
[0.8.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/intrepidkarthi/orderbook/releases/tag/v0.1.0
