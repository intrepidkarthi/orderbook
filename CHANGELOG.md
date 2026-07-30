# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (pre-1.0: minor
versions may include breaking changes).

## [Unreleased]

### Fixed

- **Published benchmark figures that did not reproduce.** Re-measured every number
  in [docs/BENCHMARKS.md](docs/BENCHMARKS.md) and the README on the stated hardware
  (median of 5 runs, idle machine, `go1.23.5`), and corrected what disagreed. The
  engine did not change; the documentation was wrong, in both directions.

  - `OrderBook_Cancel` was published at 253 ns. Five runs gave 265–301 ns, median
    273 — the old figure is outside the measured range. Now 273.
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
